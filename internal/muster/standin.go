package muster

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/bits"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

var (
	allWorkers = map[string]bool{
		"trawl": true, "sextant": true, "assay": true, "countersign": true,
		"waybill": true, "moldloft": true, "shakedown": true, "chartroom": true,
		"tidegauge": true, "bellbuoy": true,
	}
	allComponents = map[string]string{ // component -> repository
		"logbook":     "logbook",
		"muster":      "muster",
		"trawl":       "trawl",
		"sextant":     "sextant",
		"assay":       "assay",
		"countersign": "countersign",
		"waybill":     "waybill",
		"moldloft":    "moldloft",
		"shakedown":   "shakedown",
		"chartroom":   "chartroom",
		"tidegauge":   "tidegauge",
		"bellbuoy":    "bellbuoy",
		"keelworks":   "keelworks",
		"lazarette":   "lazarette",
		"shipkeeper":  "shipkeeper",
		"plainforge":  "plainforge",
		"conn":        "clearedstack.com",
	}
	knownSources      = map[string]bool{"sam-gov": true, "github-issues": true, "web-sweep": true, "site-form": true}
	knownHealthStates = map[string]bool{"healthy": true, "idle": true, "degraded": true, "paused": true}
	knownErrorCodes   = map[string]bool{"E_TIMEOUT": true, "E_PARSE": true, "E_NEEDS_SUBTOOL": true}
	pauseableStates   = map[string]bool{"recorded": true, "observed": true, "approved": true}
	digestPattern     = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	powNoncePattern   = regexp.MustCompile(`^[0-9a-f]{1,32}$`)
	itemPattern       = regexp.MustCompile(`^PA-([0-9]{4}|[1-9][0-9]{4,8})$`)
)

type ownershipEntry struct {
	Repository string
	File       string
	Kind       string
}

var defaultOwnershipMap = map[string]ownershipEntry{
	"catalog.sample-calculator.description": {
		Repository: "clearedstack.com",
		File:       "src/data/sample-catalog.json",
		Kind:       "DATA",
	},
}

type nextRule struct {
	nextWorker string
	forceLane  string
	delay      time.Duration
}

func lookupItemMove(worker, fromState, toState string) (nextRule, bool) {
	type key struct{ w, from, to string }
	table := map[key]nextRule{
		{"trawl", "discovered", "recorded"}:         {nextWorker: "sextant"},
		{"trawl", "discovered", "rejected"}:         {},
		{"sextant", "recorded", "observed"}:         {nextWorker: "assay"},
		{"assay", "observed", "scored"}:             {nextWorker: "countersign"},
		{"assay", "observed", "screened_out"}:       {nextWorker: "countersign"},
		{"countersign", "scored", "approved"}:       {nextWorker: "waybill"},
		{"countersign", "scored", "parked"}:         {},
		{"countersign", "scored", "linked"}:         {nextWorker: "chartroom"},
		{"countersign", "scored", "merged"}:         {nextWorker: "chartroom"},
		{"countersign", "scored", "rejected"}:       {},
		{"countersign", "screened_out", "observed"}: {nextWorker: "assay"},
		{"waybill", "approved", "approved"}:         {nextWorker: "moldloft"},
		{"moldloft", "approved", "building"}:        {nextWorker: "shakedown"},
		{"shakedown", "building", "building"}:       {nextWorker: "chartroom"},
		{"shakedown", "building", "parked"}:         {},
		{"chartroom", "building", "live"}:           {nextWorker: "tidegauge", forceLane: "backfill", delay: 168 * time.Hour},
		{"chartroom", "linked", "linked"}:           {},
		{"chartroom", "merged", "merged"}:           {},
		{"chartroom", "retired", "retired"}:         {},
		{"tidegauge", "live", "live"}:               {nextWorker: "tidegauge", forceLane: "backfill", delay: 168 * time.Hour},
		{"tidegauge", "live", "retired"}:            {nextWorker: "chartroom", forceLane: "urgent"},
		{"tidegauge", "live", "building"}:           {nextWorker: "waybill", forceLane: "urgent"},
		{"waybill", "building", "building"}:         {nextWorker: "moldloft"},
		{"moldloft", "building", "building"}:        {nextWorker: "shakedown"},
	}
	r, ok := table[key{worker, fromState, toState}]
	return r, ok
}

func defaultWorkerForState(state string) string {
	switch state {
	case "discovered":
		return "trawl"
	case "recorded":
		return "sextant"
	case "observed":
		return "assay"
	case "scored", "screened_out":
		return "countersign"
	case "approved":
		return "waybill"
	case "building":
		return "shakedown"
	case "linked", "merged", "retired":
		return "chartroom"
	case "live":
		return "tidegauge"
	default:
		return ""
	}
}

type storedItem struct {
	id         string
	num        int
	state      string
	found      *Found
	results    map[string]json.RawMessage
	pausedFrom string
	pausedBy   string
	pausedSeq  int
}

type storedProposal struct {
	id      string
	num     int
	status  string
	payload Proposal
	results map[string]json.RawMessage
}

type queueStep struct {
	arrival     int
	kind        string // "item", "proposal", "incident"
	subjectID   string
	worker      string
	lane        string
	readyAt     time.Time
	attempt     int
	lease       string
	leasedBy    string
	leasedUntil time.Time
	held        bool
	incident    *Incident
}

type finishedOutcome struct {
	lease  string
	worker string
	status int
	body   []byte
}

type componentStatus struct {
	state  string
	code   string
	digest string
}

// SeedItem seeds an item into a StandIn before calls run.
type SeedItem struct {
	ID      string                     `json:"id"`
	State   string                     `json:"state"`
	Lane    string                     `json:"lane"`
	Next    string                     `json:"next"`
	Found   *Found                     `json:"found,omitempty"`
	Results map[string]json.RawMessage `json:"results,omitempty"`
}

// StandIn is an in-memory HTTP implementation of Muster's contract v1.
type StandIn struct {
	mu             sync.Mutex
	now            time.Time
	leaseDuration  time.Duration
	powBits        int
	dailyLimit     int
	saltSecret     []byte
	condition      string
	nextArrival    int
	nextPauseSeq   int
	maxItemNum     int
	maxProposalNum int
	items          map[string]*storedItem
	addrToItem     map[string]string
	proposals      map[string]*storedProposal
	steps          []*queueStep
	outcomes       map[string]finishedOutcome // lease -> outcome
	health         map[string]componentStatus
	rateCounts     map[string]int
}

// NewStandIn creates a StandIn starting at 2026-01-05T09:00:00Z with contract defaults.
func NewStandIn() *StandIn {
	return &StandIn{
		now:           time.Date(2026, 1, 5, 9, 0, 0, 0, time.UTC),
		leaseDuration: 60 * time.Second,
		powBits:       20,
		dailyLimit:    5,
		saltSecret:    []byte("standin-cluster-secret"),
		items:         map[string]*storedItem{},
		addrToItem:    map[string]string{},
		proposals:     map[string]*storedProposal{},
		outcomes:      map[string]finishedOutcome{},
		health:        map[string]componentStatus{},
		rateCounts:    map[string]int{},
	}
}

// Advance moves the stand-in clock forward by d.
func (s *StandIn) Advance(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now = s.now.Add(d)
}

// SetCondition configures a simulated server/store condition from contract examples.
func (s *StandIn) SetCondition(cond string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.condition = cond
	if cond == "the queue says sextant is next for an item that is live" {
		s.items["PA-0001"] = &storedItem{
			id:      "PA-0001",
			num:     1,
			state:   "live",
			results: map[string]json.RawMessage{},
		}
		if s.maxItemNum < 1 {
			s.maxItemNum = 1
		}
		s.enqueue("item", "PA-0001", "sextant", "normal", s.now, nil)
	}
}

// Seed adds pre-existing items to the stand-in.
func (s *StandIn) Seed(seeds []SeedItem) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, seed := range seeds {
		num := parseIDNum(seed.ID)
		if num > s.maxItemNum {
			s.maxItemNum = num
		}
		res := map[string]json.RawMessage{}
		if len(seed.Results) > 0 {
			res = cloneResults(seed.Results)
		}
		it := &storedItem{
			id:      seed.ID,
			num:     num,
			state:   seed.State,
			found:   seed.Found,
			results: res,
		}
		s.items[seed.ID] = it
		if seed.Found != nil && seed.Found.Address != "" {
			s.addrToItem[seed.Found.Address] = seed.ID
		}
		next := seed.Next
		if next == "" {
			next = defaultWorkerForState(seed.State)
		}
		if next != "" {
			lane := seed.Lane
			if lane == "" {
				lane = "normal"
			}
			s.enqueue("item", seed.ID, next, lane, s.now, nil)
		}
	}
}

func parseIDNum(id string) int {
	if idx := strings.IndexByte(id, '-'); idx >= 0 {
		n, _ := strconv.Atoi(id[idx+1:])
		return n
	}
	return 0
}

func formatID(prefix string, n int) string {
	if n <= 9999 {
		return fmt.Sprintf("%s-%04d", prefix, n)
	}
	return fmt.Sprintf("%s-%d", prefix, n)
}

func (s *StandIn) enqueue(kind, subjectID, worker, lane string, readyAt time.Time, inc *Incident) {
	s.nextArrival++
	s.steps = append(s.steps, &queueStep{
		arrival:   s.nextArrival,
		kind:      kind,
		subjectID: subjectID,
		worker:    worker,
		lane:      lane,
		readyAt:   readyAt,
		incident:  inc,
	})
}

func (s *StandIn) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" {
		if r.Method != http.MethodGet {
			writeRefusal(w, http.StatusMethodNotAllowed, "E_PARSE", "wrong_method", "this call does not accept that method")
			return
		}
		writeRawJSON(w, http.StatusOK, []byte(`{"status":"ok"}`))
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.condition == "every place for a call in flight is taken" {
		writeRefusal(w, http.StatusServiceUnavailable, "E_TIMEOUT", "busy", "too busy; try again")
		return
	}

	knownMethods := map[string]string{
		"/v1/lease":         http.MethodPost,
		"/v1/heartbeat":     http.MethodPost,
		"/v1/complete":      http.MethodPost,
		"/v1/fail":          http.MethodPost,
		"/v1/needs_subtool": http.MethodPost,
		"/v1/items":         http.MethodPost,
		"/v1/release":       http.MethodPost,
		"/v1/health":        http.MethodPost,
		"/v1/edits":         http.MethodPost,
		"/v1/feed":          http.MethodGet,
	}
	wantMethod, exists := knownMethods[r.URL.Path]
	if !exists {
		writeRefusal(w, http.StatusNotFound, "E_PARSE", "no_such_call", "no such call")
		return
	}
	if r.Method != wantMethod {
		writeRefusal(w, http.StatusMethodNotAllowed, "E_PARSE", "wrong_method", "this call does not accept that method")
		return
	}

	caller, ok := extractCaller(r.Header.Get("Authorization"))
	if !ok {
		writeRefusal(w, http.StatusUnauthorized, "E_PARSE", "no_credential", "no valid credential was presented")
		return
	}
	if s.condition == "the cluster does not answer the identity check in time" {
		writeRefusal(w, http.StatusServiceUnavailable, "E_TIMEOUT", "busy", "the identity check took too long; try again")
		return
	}
	if !callerAllowed(r.URL.Path, caller) {
		writeRefusal(w, http.StatusForbidden, "E_PARSE", "not_your_call", "this caller may not make this call")
		return
	}

	switch s.condition {
	case "logbook takes too long":
		writeRefusal(w, http.StatusServiceUnavailable, "E_TIMEOUT", "store_slow", "the record store took too long")
		return
	case "logbook refuses a field":
		writeRefusal(w, http.StatusBadRequest, "E_PARSE", "bad_field", "the record store refused a field")
		return
	case "logbook has no rule for a value":
		writeRefusal(w, http.StatusUnprocessableEntity, "E_NEEDS_SUBTOOL", "unknown_value", "the record store has no rule for a value")
		return
	case "logbook fails in a way nothing provides for":
		writeRefusal(w, http.StatusInternalServerError, "E_PARSE", "unexpected", "an unexpected condition; the request was refused")
		return
	}

	if r.URL.Path == "/v1/feed" {
		s.handleFeed(w)
		return
	}

	ct := strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0])
	if !strings.EqualFold(ct, "application/json") {
		writeRefusal(w, http.StatusUnsupportedMediaType, "E_PARSE", "wrong_type", "the content type must be application/json")
		return
	}

	limit := int64(MaxRequestBytes)
	if r.URL.Path == "/v1/edits" {
		limit = 4096
	} else if r.URL.Path == "/v1/complete" && caller == "waybill" {
		limit = 256 << 10
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		writeRefusal(w, http.StatusBadRequest, "E_PARSE", "not_json", "the request is not one json value")
		return
	}
	if int64(len(raw)) > limit {
		writeRefusal(w, http.StatusRequestEntityTooLarge, "E_PARSE", "too_large", "the request is too large")
		return
	}

	fields, ok := parseJSONObject(raw)
	if !ok {
		writeRefusal(w, http.StatusBadRequest, "E_PARSE", "not_json", "the request is not one json value")
		return
	}

	switch r.URL.Path {
	case "/v1/lease":
		s.handleLease(w, caller, fields)
	case "/v1/heartbeat":
		s.handleHeartbeat(w, caller, fields)
	case "/v1/complete":
		s.handleComplete(w, caller, fields)
	case "/v1/fail":
		s.handleFail(w, caller, fields)
	case "/v1/needs_subtool":
		s.handleNeedsSubtool(w, caller, fields)
	case "/v1/release":
		s.handleRelease(w, fields)
	case "/v1/items":
		s.handleItems(w, caller, fields)
	case "/v1/health":
		s.handleHealth(w, fields)
	case "/v1/edits":
		s.handleEdits(w, fields)
	}
}

func extractCaller(authHeader string) (string, bool) {
	rest, ok := strings.CutPrefix(authHeader, "Bearer ")
	if !ok {
		return "", false
	}
	caller := strings.TrimSpace(rest)
	if allWorkers[caller] || caller == "conn" || caller == "shipkeeper" {
		return caller, true
	}
	return "", false
}

func callerAllowed(path, caller string) bool {
	switch path {
	case "/v1/lease", "/v1/heartbeat", "/v1/complete", "/v1/fail", "/v1/needs_subtool":
		return allWorkers[caller]
	case "/v1/items":
		return caller == "trawl" || caller == "conn"
	case "/v1/release":
		return caller == "countersign"
	case "/v1/health":
		return caller == "shipkeeper"
	case "/v1/edits", "/v1/feed":
		return caller == "conn"
	}
	return false
}

func parseJSONObject(raw []byte) (map[string]json.RawMessage, bool) {
	if !utf8.Valid(raw) || !json.Valid(raw) {
		return nil, false
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' || hasDuplicateKeys(trimmed) {
		return nil, false
	}
	var obj map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	if err := dec.Decode(&obj); err != nil {
		return nil, false
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, false
	}
	return obj, true
}

func checkAllowedFields(w http.ResponseWriter, fields map[string]json.RawMessage, allowed ...string) bool {
	set := make(map[string]bool, len(allowed))
	for _, a := range allowed {
		set[a] = true
	}
	for k := range fields {
		if !set[k] {
			writeRefusal(w, http.StatusBadRequest, "E_PARSE", "unknown_field", "the request has a field this call does not define")
			return false
		}
	}
	cRaw, ok := fields["contract"]
	if !ok {
		writeRefusal(w, http.StatusBadRequest, "E_PARSE", "missing_field", "a required field is missing")
		return false
	}
	var c string
	if err := json.Unmarshal(cRaw, &c); err != nil {
		writeRefusal(w, http.StatusBadRequest, "E_PARSE", "bad_field", "a field does not have the form this call requires")
		return false
	}
	if c == "" {
		writeRefusal(w, http.StatusBadRequest, "E_PARSE", "missing_field", "a required field is missing")
		return false
	}
	if c != ContractVersion {
		writeRefusal(w, http.StatusUnprocessableEntity, "E_NEEDS_SUBTOOL", "unknown_contract", "this contract version is not known")
		return false
	}
	return true
}

func reqString(w http.ResponseWriter, fields map[string]json.RawMessage, name string, allowEmpty bool) (string, bool) {
	raw, ok := fields[name]
	if !ok {
		writeRefusal(w, http.StatusBadRequest, "E_PARSE", "missing_field", "a required field is missing")
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		writeRefusal(w, http.StatusBadRequest, "E_PARSE", "bad_field", "a field does not have the form this call requires")
		return "", false
	}
	if !allowEmpty && s == "" {
		writeRefusal(w, http.StatusBadRequest, "E_PARSE", "missing_field", "a required field is missing")
		return "", false
	}
	return s, true
}

func (s *StandIn) handleLease(w http.ResponseWriter, worker string, fields map[string]json.RawMessage) {
	if !checkAllowedFields(w, fields, "contract") {
		return
	}
	for _, lane := range []string{"urgent", "normal", "backfill"} {
		for i := 0; i < len(s.steps); i++ {
			st := s.steps[i]
			if st.worker != worker || st.lane != lane || st.held || st.readyAt.After(s.now) {
				continue
			}
			if st.lease != "" && st.leasedUntil.After(s.now) {
				continue
			}
			if st.kind == "item" {
				it := s.items[st.subjectID]
				if it == nil || defaultWorkerForState(it.state) == "" || !workerCanHoldItem(worker, it.state) {
					st.held = true
					st.lease = ""
					st.leasedUntil = time.Time{}
					state := ""
					if it != nil {
						state = it.state
					}
					s.replaceIncident(st.subjectID, worker, "E_PARSE", state)
					writeRefusal(w, http.StatusInternalServerError, "E_PARSE", "bad_record", "a stored record was not consistent and has been set aside")
					return
				}
			}
			st.attempt++
			prefix := st.subjectID
			if st.kind == "incident" {
				prefix = "incident:" + st.subjectID
			}
			st.lease = fmt.Sprintf("%s.%d.%d", prefix, st.arrival, st.attempt)
			st.leasedBy = worker
			st.leasedUntil = s.now.Add(s.leaseDuration)

			step := Step{
				Lease:   st.lease,
				Kind:    st.kind,
				ID:      st.subjectID,
				Lane:    st.lane,
				Attempt: st.attempt,
				Expires: st.leasedUntil.UTC().Format(time.RFC3339),
			}
			switch st.kind {
			case "item":
				it := s.items[st.subjectID]
				step.State = it.state
				step.Found = it.found
				if len(it.results) > 0 {
					step.Results = cloneResults(it.results)
				}
			case "proposal":
				pr := s.proposals[st.subjectID]
				step.State = pr.status
				if len(pr.results) > 0 {
					step.Results = cloneResults(pr.results)
				}
				cp := pr.payload
				step.Proposal = &cp
			case "incident":
				cp := *st.incident
				step.State = cp.State
				step.Incident = &cp
			}
			out, _ := json.Marshal(step)
			writeRawJSON(w, http.StatusOK, out)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func workerCanHoldItem(worker, state string) bool {
	switch worker {
	case "trawl":
		return state == "discovered"
	case "sextant":
		return state == "recorded"
	case "assay":
		return state == "observed"
	case "countersign":
		return state == "scored" || state == "screened_out"
	case "waybill", "moldloft":
		return state == "approved" || state == "building"
	case "shakedown":
		return state == "building"
	case "chartroom":
		return state == "building" || state == "linked" || state == "merged" || state == "retired"
	case "tidegauge":
		return state == "live"
	}
	return false
}

func (s *StandIn) findActiveStep(lease, worker string) (*queueStep, int) {
	for i, st := range s.steps {
		if st.lease == lease && st.leasedBy == worker && !st.held {
			return st, i
		}
	}
	return nil, -1
}

func (s *StandIn) handleHeartbeat(w http.ResponseWriter, worker string, fields map[string]json.RawMessage) {
	if !checkAllowedFields(w, fields, "contract", "lease") {
		return
	}
	lease, ok := reqString(w, fields, "lease", false)
	if !ok {
		return
	}
	st, _ := s.findActiveStep(lease, worker)
	if st == nil || !st.leasedUntil.After(s.now) {
		writeRefusal(w, http.StatusConflict, "E_PARSE", "lease_not_held", "the lease is not held")
		return
	}
	st.leasedUntil = s.now.Add(s.leaseDuration)
	out, _ := json.Marshal(HeartbeatReply{
		Lease:   lease,
		Expires: st.leasedUntil.UTC().Format(time.RFC3339),
	})
	writeRawJSON(w, http.StatusOK, out)
}

func (s *StandIn) handleComplete(w http.ResponseWriter, worker string, fields map[string]json.RawMessage) {
	if !checkAllowedFields(w, fields, "contract", "lease", "state", "result") {
		return
	}
	lease, ok := reqString(w, fields, "lease", false)
	if !ok {
		return
	}
	toState, ok := reqString(w, fields, "state", false)
	if !ok {
		return
	}
	var resultObj json.RawMessage
	if rRaw, hasResult := fields["result"]; hasResult {
		if _, isObj := parseJSONObject(rRaw); !isObj {
			writeRefusal(w, http.StatusBadRequest, "E_PARSE", "bad_field", "a field does not have the form this call requires")
			return
		}
		resultObj = append(json.RawMessage(nil), bytes.TrimSpace(rRaw)...)
	}

	if prev, done := s.outcomes[lease]; done && prev.worker == worker {
		writeRawJSON(w, prev.status, prev.body)
		return
	}

	st, idx := s.findActiveStep(lease, worker)
	if st == nil {
		writeRefusal(w, http.StatusConflict, "E_PARSE", "lease_not_held", "the lease is not held")
		return
	}

	switch st.kind {
	case "item":
		it := s.items[st.subjectID]
		rule, allowed := lookupItemMove(worker, it.state, toState)
		if !allowed {
			writeRefusal(w, http.StatusConflict, "E_PARSE", "move_not_allowed", "that move is not allowed")
			return
		}
		it.state = toState
		if resultObj != nil {
			it.results[worker] = resultObj
		}
		lane := st.lane
		if rule.forceLane != "" {
			lane = rule.forceLane
		}
		s.steps = append(s.steps[:idx], s.steps[idx+1:]...)
		if rule.nextWorker != "" {
			s.enqueue("item", it.id, rule.nextWorker, lane, s.now.Add(rule.delay), nil)
		}
		rep, _ := json.Marshal(CompleteReply{ID: it.id, State: toState, Next: rule.nextWorker})
		s.outcomes[lease] = finishedOutcome{lease: lease, worker: worker, status: http.StatusOK, body: rep}
		writeRawJSON(w, http.StatusOK, rep)

	case "proposal":
		pr := s.proposals[st.subjectID]
		nextW, nextLane, allowed := lookupProposalMove(worker, pr.status, toState)
		if !allowed {
			writeRefusal(w, http.StatusConflict, "E_PARSE", "move_not_allowed", "that move is not allowed")
			return
		}
		if resultObj != nil {
			pr.results[worker] = resultObj
		}
		pr.status = toState
		s.steps = append(s.steps[:idx], s.steps[idx+1:]...)
		if nextW != "" {
			s.enqueue("proposal", pr.id, nextW, nextLane, s.now, nil)
		}
		rep, _ := json.Marshal(CompleteReply{ID: pr.id, State: toState, Next: nextW})
		s.outcomes[lease] = finishedOutcome{lease: lease, worker: worker, status: http.StatusOK, body: rep}
		writeRawJSON(w, http.StatusOK, rep)

	case "incident":
		if worker != "bellbuoy" || toState != st.incident.State || resultObj != nil {
			writeRefusal(w, http.StatusConflict, "E_PARSE", "move_not_allowed", "that move is not allowed")
			return
		}
		s.steps = append(s.steps[:idx], s.steps[idx+1:]...)
		rep, _ := json.Marshal(CompleteReply{ID: st.subjectID, State: toState})
		s.outcomes[lease] = finishedOutcome{lease: lease, worker: worker, status: http.StatusOK, body: rep}
		writeRawJSON(w, http.StatusOK, rep)
	}
}

func lookupProposalMove(worker, from, to string) (string, string, bool) {
	switch {
	case worker == "countersign" && from == "pending_approval" && to == "approved":
		return "bellbuoy", "urgent", true
	case worker == "countersign" && from == "pending_approval" && to == "declined":
		return "", "", true
	case worker == "bellbuoy" && from == "approved" && to == "building":
		return "", "", true
	}
	return "", "", false
}

func (s *StandIn) handleFail(w http.ResponseWriter, worker string, fields map[string]json.RawMessage) {
	if !checkAllowedFields(w, fields, "contract", "lease", "code") {
		return
	}
	lease, ok := reqString(w, fields, "lease", false)
	if !ok {
		return
	}
	code, ok := reqString(w, fields, "code", false)
	if !ok {
		return
	}
	if code == "E_NEEDS_SUBTOOL" {
		writeRefusal(w, http.StatusBadRequest, "E_PARSE", "bad_field", "report a missing rule with the needs_subtool call")
		return
	}
	if code != "E_TIMEOUT" && code != "E_PARSE" {
		writeRefusal(w, http.StatusBadRequest, "E_PARSE", "bad_field", "a field does not have the form this call requires")
		return
	}
	if prev, done := s.outcomes[lease]; done && prev.worker == worker {
		writeRawJSON(w, prev.status, prev.body)
		return
	}
	st, _ := s.findActiveStep(lease, worker)
	if st == nil {
		writeRefusal(w, http.StatusConflict, "E_PARSE", "lease_not_held", "the lease is not held")
		return
	}
	state := s.stepState(st)
	st.lease = ""
	st.leasedBy = ""
	st.leasedUntil = time.Time{}

	var rep []byte
	if code == "E_TIMEOUT" {
		rep, _ = json.Marshal(FailReply{ID: st.subjectID, State: state, Next: st.worker})
	} else {
		st.held = true
		if st.kind == "item" {
			s.replaceIncident(st.subjectID, worker, "E_PARSE", state)
		}
		rep, _ = json.Marshal(FailReply{ID: st.subjectID, State: state, Held: true})
	}
	s.outcomes[lease] = finishedOutcome{lease: lease, worker: worker, status: http.StatusOK, body: rep}
	writeRawJSON(w, http.StatusOK, rep)
}

func (s *StandIn) handleNeedsSubtool(w http.ResponseWriter, worker string, fields map[string]json.RawMessage) {
	if !checkAllowedFields(w, fields, "contract", "lease") {
		return
	}
	lease, ok := reqString(w, fields, "lease", false)
	if !ok {
		return
	}
	if prev, done := s.outcomes[lease]; done && prev.worker == worker {
		writeRawJSON(w, prev.status, prev.body)
		return
	}
	st, idx := s.findActiveStep(lease, worker)
	if st == nil {
		writeRefusal(w, http.StatusConflict, "E_PARSE", "lease_not_held", "the lease is not held")
		return
	}
	if st.kind != "item" {
		writeRefusal(w, http.StatusConflict, "E_PARSE", "move_not_allowed", "that move is not allowed")
		return
	}
	it := s.items[st.subjectID]
	if !pauseableStates[it.state] {
		writeRefusal(w, http.StatusConflict, "E_PARSE", "move_not_allowed", "that move is not allowed")
		return
	}
	prevState := it.state
	it.pausedFrom = prevState
	it.pausedBy = worker
	s.nextPauseSeq++
	it.pausedSeq = s.nextPauseSeq
	it.state = "needs_subtool"
	s.steps = append(s.steps[:idx], s.steps[idx+1:]...)
	s.replaceIncident(it.id, worker, "E_NEEDS_SUBTOOL", prevState)

	rep, _ := json.Marshal(PauseReply{ID: it.id, State: "needs_subtool"})
	s.outcomes[lease] = finishedOutcome{lease: lease, worker: worker, status: http.StatusOK, body: rep}
	writeRawJSON(w, http.StatusOK, rep)
}

func (s *StandIn) replaceIncident(itemID, worker, code, state string) {
	filtered := s.steps[:0]
	for _, st := range s.steps {
		if st.kind == "incident" && st.subjectID == itemID {
			continue
		}
		filtered = append(filtered, st)
	}
	s.steps = filtered
	s.enqueue("incident", itemID, "bellbuoy", "normal", s.now, &Incident{
		ID:     itemID,
		Worker: worker,
		Code:   code,
		State:  state,
	})
}

func (s *StandIn) stepState(st *queueStep) string {
	switch st.kind {
	case "item":
		return s.items[st.subjectID].state
	case "proposal":
		return s.proposals[st.subjectID].status
	case "incident":
		return st.incident.State
	}
	return ""
}

func (s *StandIn) handleRelease(w http.ResponseWriter, fields map[string]json.RawMessage) {
	if !checkAllowedFields(w, fields, "contract", "id") {
		return
	}
	id, ok := reqString(w, fields, "id", false)
	if !ok {
		return
	}
	if !itemPattern.MatchString(id) {
		writeRefusal(w, http.StatusBadRequest, "E_PARSE", "bad_field", "a field does not have the form this call requires")
		return
	}
	for i, st := range s.steps {
		if st.kind == "item" && st.subjectID == id && st.held {
			worker := st.worker
			state := s.items[id].state
			s.steps = append(s.steps[:i], s.steps[i+1:]...)
			s.enqueue("item", id, worker, "urgent", s.now, nil)
			out, _ := json.Marshal(CompleteReply{ID: id, State: state, Next: worker})
			writeRawJSON(w, http.StatusOK, out)
			return
		}
	}
	writeRefusal(w, http.StatusConflict, "E_PARSE", "move_not_allowed", "that move is not allowed")
}

func (s *StandIn) handleItems(w http.ResponseWriter, caller string, fields map[string]json.RawMessage) {
	if !checkAllowedFields(w, fields, "contract", "source", "title", "address", "result", "pow", "visitor") {
		return
	}
	source, ok := reqString(w, fields, "source", false)
	if !ok {
		return
	}
	title, ok := reqString(w, fields, "title", false)
	if !ok {
		return
	}
	address, ok := reqString(w, fields, "address", false)
	if !ok {
		return
	}
	if !knownSources[source] {
		writeRefusal(w, http.StatusUnprocessableEntity, "E_NEEDS_SUBTOOL", "unknown_value", "this source is not known")
		return
	}
	if len(title) > 200 || strings.ContainsAny(title, "\r\n") || len(address) > 2000 || strings.ContainsAny(address, " \t\r\n") {
		writeRefusal(w, http.StatusBadRequest, "E_PARSE", "bad_field", "a field does not have the form this call requires")
		return
	}

	var resultObj json.RawMessage
	if rRaw, hasRes := fields["result"]; hasRes {
		if _, isObj := parseJSONObject(rRaw); !isObj {
			writeRefusal(w, http.StatusBadRequest, "E_PARSE", "bad_field", "a field does not have the form this call requires")
			return
		}
		resultObj = append(json.RawMessage(nil), bytes.TrimSpace(rRaw)...)
	}

	_, hasPow := fields["pow"]
	_, hasVisitor := fields["visitor"]
	if source == "site-form" {
		powTok, ok := reqString(w, fields, "pow", false)
		if !ok {
			return
		}
		visitor, ok := reqString(w, fields, "visitor", false)
		if !ok {
			return
		}
		if !s.verifyPoW(powTok, source, title, address) {
			writeRefusal(w, http.StatusBadRequest, "E_PARSE", "bad_field", "the proof of work is not valid")
			return
		}
		if !s.allowRate(visitor) {
			writeRefusal(w, http.StatusTooManyRequests, "E_TIMEOUT", "rate_limited", "too many submissions from this visitor today; try again tomorrow")
			return
		}
	} else if hasPow || hasVisitor {
		writeRefusal(w, http.StatusBadRequest, "E_PARSE", "bad_field", "a field does not have the form this call requires")
		return
	}

	cleanTitle := strings.TrimSpace(stripMarkup(title))
	if cleanTitle == "" {
		writeRefusal(w, http.StatusBadRequest, "E_PARSE", "bad_field", "nothing is left of the text once markup is removed")
		return
	}

	if existingID, dup := s.addrToItem[address]; dup {
		it := s.items[existingID]
		out, _ := json.Marshal(struct {
			ID        string `json:"id"`
			State     string `json:"state"`
			Duplicate bool   `json:"duplicate"`
		}{ID: it.id, State: it.state, Duplicate: true})
		writeRawJSON(w, http.StatusOK, out)
		return
	}

	s.maxItemNum++
	id := formatID("PA", s.maxItemNum)
	it := &storedItem{
		id:      id,
		num:     s.maxItemNum,
		state:   "discovered",
		found:   &Found{Source: source, Title: cleanTitle, Address: address},
		results: map[string]json.RawMessage{},
	}
	if resultObj != nil {
		it.results[caller] = resultObj
	}
	s.items[id] = it
	s.addrToItem[address] = id
	lane := "normal"
	if source == "web-sweep" {
		lane = "backfill"
	}
	s.enqueue("item", id, "trawl", lane, s.now, nil)
	out, _ := json.Marshal(CompleteReply{ID: id, State: "discovered", Next: "trawl"})
	writeRawJSON(w, http.StatusCreated, out)
}

func (s *StandIn) handleHealth(w http.ResponseWriter, fields map[string]json.RawMessage) {
	if !checkAllowedFields(w, fields, "contract", "reports") {
		return
	}
	rawReports, ok := fields["reports"]
	if !ok {
		writeRefusal(w, http.StatusBadRequest, "E_PARSE", "missing_field", "a required field is missing")
		return
	}
	var rawList []json.RawMessage
	if err := json.Unmarshal(rawReports, &rawList); err != nil {
		writeRefusal(w, http.StatusBadRequest, "E_PARSE", "bad_field", "a field does not have the form this call requires")
		return
	}
	if len(rawList) == 0 {
		writeRefusal(w, http.StatusBadRequest, "E_PARSE", "missing_field", "a required field is missing")
		return
	}
	if len(rawList) > 64 {
		writeRefusal(w, http.StatusBadRequest, "E_PARSE", "bad_field", "a field does not have the form this call requires")
		return
	}

	type parsedReport struct {
		component string
		state     string
		code      string
		digest    string
	}
	var batch []parsedReport
	seen := map[string]bool{}

	for _, entry := range rawList {
		m, isObj := parseJSONObject(entry)
		if !isObj {
			writeRefusal(w, http.StatusBadRequest, "E_PARSE", "bad_field", "a field does not have the form this call requires")
			return
		}
		for k := range m {
			if k != "component" && k != "state" && k != "code" && k != "digest" {
				writeRefusal(w, http.StatusBadRequest, "E_PARSE", "unknown_field", "the request has a field this call does not define")
				return
			}
		}
		comp, ok := reqString(w, m, "component", false)
		if !ok {
			return
		}
		state, ok := reqString(w, m, "state", false)
		if !ok {
			return
		}
		if _, exists := allComponents[comp]; !exists {
			writeRefusal(w, http.StatusBadRequest, "E_PARSE", "bad_field", "a field does not have the form this call requires")
			return
		}
		if seen[comp] {
			writeRefusal(w, http.StatusConflict, "E_PARSE", "duplicate", "the same component is reported twice in one batch")
			return
		}
		seen[comp] = true
		if !knownHealthStates[state] {
			writeRefusal(w, http.StatusUnprocessableEntity, "E_NEEDS_SUBTOOL", "unknown_value", "this health state is not known")
			return
		}
		var code, digest string
		if cRaw, hasCode := m["code"]; hasCode {
			if err := json.Unmarshal(cRaw, &code); err != nil || !knownErrorCodes[code] {
				writeRefusal(w, http.StatusBadRequest, "E_PARSE", "bad_field", "a field does not have the form this call requires")
				return
			}
		}
		if dRaw, hasDig := m["digest"]; hasDig {
			if err := json.Unmarshal(dRaw, &digest); err != nil || !digestPattern.MatchString(digest) {
				writeRefusal(w, http.StatusBadRequest, "E_PARSE", "bad_field", "a field does not have the form this call requires")
				return
			}
		}
		batch = append(batch, parsedReport{component: comp, state: state, code: code, digest: digest})
	}

	replayed := 0
	live := 0
	for _, r := range batch {
		prev := s.health[r.component]
		changed := r.digest != "" && prev.digest != "" && r.digest != prev.digest
		newDigest := prev.digest
		if r.digest != "" {
			newDigest = r.digest
		}
		s.health[r.component] = componentStatus{state: r.state, code: r.code, digest: newDigest}

		if changed {
			if allWorkers[r.component] {
				var paused []*storedItem
				for _, it := range s.items {
					if it.state == "needs_subtool" && it.pausedBy == r.component {
						paused = append(paused, it)
					}
				}
				sort.Slice(paused, func(i, j int) bool { return paused[i].pausedSeq < paused[j].pausedSeq })
				for _, it := range paused {
					it.state = it.pausedFrom
					it.pausedFrom = ""
					it.pausedBy = ""
					s.enqueue("item", it.id, r.component, "urgent", s.now, nil)
					replayed++
				}
			}
			repo := allComponents[r.component]
			for _, pr := range s.proposals {
				if pr.status == "building" && pr.payload.Repository == repo {
					pr.status = "live"
					live++
				}
			}
		}
	}

	out, _ := json.Marshal(struct {
		Received int `json:"received"`
		Replayed int `json:"replayed"`
		Live     int `json:"live"`
	}{Received: len(batch), Replayed: replayed, Live: live})
	writeRawJSON(w, http.StatusOK, out)
}

func (s *StandIn) handleEdits(w http.ResponseWriter, fields map[string]json.RawMessage) {
	if !checkAllowedFields(w, fields, "contract", "target", "change", "reason", "pow", "honeypot", "source") {
		return
	}
	target, ok := reqString(w, fields, "target", false)
	if !ok {
		return
	}
	change, ok := reqString(w, fields, "change", false)
	if !ok {
		return
	}
	reason, ok := reqString(w, fields, "reason", false)
	if !ok {
		return
	}
	powTok, ok := reqString(w, fields, "pow", false)
	if !ok {
		return
	}
	honeypot, ok := reqString(w, fields, "honeypot", true)
	if !ok {
		return
	}
	visitor, ok := reqString(w, fields, "source", false)
	if !ok {
		return
	}

	if !s.verifyPoW(powTok, target, change, reason) {
		writeRefusal(w, http.StatusBadRequest, "E_PARSE", "bad_field", "the proof of work is not valid")
		return
	}
	if honeypot != "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if !s.allowRate(visitor) {
		writeRefusal(w, http.StatusTooManyRequests, "E_TIMEOUT", "rate_limited", "too many submissions from this visitor today; try again tomorrow")
		return
	}
	own, ok := defaultOwnershipMap[target]
	if !ok {
		writeRefusal(w, http.StatusBadRequest, "E_PARSE", "bad_field", "the target is not in the ownership map")
		return
	}
	cleanChange := strings.TrimSpace(stripMarkup(change))
	cleanReason := strings.TrimSpace(stripMarkup(reason))
	if cleanChange == "" {
		writeRefusal(w, http.StatusBadRequest, "E_PARSE", "bad_field", "nothing is left of the text once markup is removed")
		return
	}

	s.maxProposalNum++
	id := formatID("EP", s.maxProposalNum)
	pr := &storedProposal{
		id:     id,
		num:    s.maxProposalNum,
		status: "pending_approval",
		payload: Proposal{
			Target:     target,
			Repository: own.Repository,
			File:       own.File,
			EditKind:   own.Kind,
			Change:     cleanChange,
			Reason:     cleanReason,
		},
		results: map[string]json.RawMessage{},
	}
	s.proposals[id] = pr
	s.enqueue("proposal", id, "countersign", "normal", s.now, nil)

	out, _ := json.Marshal(struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}{ID: id, Status: "pending_approval"})
	writeRawJSON(w, http.StatusAccepted, out)
}

func (s *StandIn) handleFeed(w http.ResponseWriter) {
	var urgent, normal, backfill, paused int
	for _, st := range s.steps {
		if st.held || st.readyAt.After(s.now) {
			continue
		}
		if st.lease != "" && st.leasedUntil.After(s.now) {
			continue
		}
		switch st.lane {
		case "urgent":
			urgent++
		case "normal":
			normal++
		case "backfill":
			backfill++
		}
	}
	for _, it := range s.items {
		if it.state == "needs_subtool" {
			paused++
		}
	}

	at := s.now.UTC().Format(time.RFC3339)
	var b strings.Builder
	eventID := 1
	depthsJSON, _ := json.Marshal(struct {
		At           string `json:"at"`
		Urgent       int    `json:"urgent"`
		Normal       int    `json:"normal"`
		Backfill     int    `json:"backfill"`
		NeedsSubtool int    `json:"needs_subtool"`
	}{At: at, Urgent: urgent, Normal: normal, Backfill: backfill, NeedsSubtool: paused})
	fmt.Fprintf(&b, "id: %d\nevent: depths\ndata: %s\n\n", eventID, depthsJSON)

	var comps []string
	for c := range s.health {
		comps = append(comps, c)
	}
	sort.Strings(comps)
	for _, c := range comps {
		eventID++
		h := s.health[c]
		hJSON, _ := json.Marshal(struct {
			At        string `json:"at"`
			Component string `json:"component"`
			State     string `json:"state"`
			Code      string `json:"code,omitempty"`
		}{At: at, Component: c, State: h.state, Code: h.code})
		fmt.Fprintf(&b, "id: %d\nevent: health\ndata: %s\n\n", eventID, hJSON)
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(b.String()))
}

func (s *StandIn) verifyPoW(token, a, b, c string) bool {
	day, nonce, ok := strings.Cut(token, ".")
	if !ok || !powNoncePattern.MatchString(nonce) {
		return false
	}
	today := s.now.UTC().Format("2006-01-02")
	yesterday := s.now.UTC().AddDate(0, 0, -1).Format("2006-01-02")
	if day != today && day != yesterday {
		return false
	}
	msg := strings.Join([]string{"muster-pow-v1", day, a, b, c, nonce}, "\x00")
	sum := sha256.Sum256([]byte(msg))
	return leadingZeroBits(sum[:]) >= s.powBits
}

func leadingZeroBits(b []byte) int {
	total := 0
	for _, x := range b {
		if x == 0 {
			total += 8
			continue
		}
		total += bits.LeadingZeros8(x)
		break
	}
	return total
}

func (s *StandIn) allowRate(visitor string) bool {
	day := s.now.UTC().Format("2006-01-02")
	macSalt := hmac.New(sha256.New, s.saltSecret)
	macSalt.Write([]byte(day))
	daySalt := macSalt.Sum(nil)

	macKey := hmac.New(sha256.New, daySalt)
	macKey.Write([]byte(visitor))
	key := day + ":" + hex.EncodeToString(macKey.Sum(nil))

	s.rateCounts[key]++
	return s.rateCounts[key] <= s.dailyLimit
}

var (
	commentRE = regexp.MustCompile(`(?is)<!--.*?-->`)
	scriptRE  = regexp.MustCompile(`(?is)<script\b[^>]*>.*?</script>`)
	styleRE   = regexp.MustCompile(`(?is)<style\b[^>]*>.*?</style>`)
	tagRE     = regexp.MustCompile(`<[^>]*>`)
)

func stripMarkup(in string) string {
	s := commentRE.ReplaceAllString(in, "")
	s = scriptRE.ReplaceAllString(s, "")
	s = styleRE.ReplaceAllString(s, "")
	s = tagRE.ReplaceAllString(s, "")
	return s
}

func cloneResults(in map[string]json.RawMessage) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(in))
	for k, v := range in {
		out[k] = append(json.RawMessage(nil), v...)
	}
	return out
}

func writeRefusal(w http.ResponseWriter, status int, code, cause, reason string) {
	out, _ := json.Marshal(struct {
		Error  string `json:"error"`
		Cause  string `json:"cause"`
		Reason string `json:"reason"`
	}{Error: code, Cause: cause, Reason: reason})
	writeRawJSON(w, status, out)
}

func writeRawJSON(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
