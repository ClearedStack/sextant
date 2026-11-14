// Package muster implements Sextant's client for Muster's contract v1 and an
// in-memory stand-in hub used by unit and contract-replay tests.
package muster

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ClearedStack/sextant/internal/errcode"
)

const (
	// ContractVersion is the shared worker contract version Sextant speaks.
	ContractVersion = "v1"
	// MaxRequestBytes is Muster's 64 KiB body limit for non-Waybill calls.
	MaxRequestBytes = 64 << 10
	// maxResponseBytes bounds responses read from Muster.
	maxResponseBytes = 512 << 10
	// DefaultTokenFile is the projected Kubernetes ServiceAccount token path.
	DefaultTokenFile = "/var/run/secrets/kubernetes.io/serviceaccount/token"
)

var (
	// ErrBadResponse means Muster answered with HTTP status or JSON that
	// violates contract/v1/CONTRACT.md.
	ErrBadResponse = errors.New("muster: response violates contract v1")
	// ErrRequestTooLarge means the outbound JSON body exceeds 64 KiB.
	ErrRequestTooLarge = errors.New("muster: request body exceeds 64 KiB limit")
)

// Found holds the intake metadata recorded for an item.
type Found struct {
	Source  string `json:"source"`
	Title   string `json:"title"`
	Address string `json:"address"`
}

// Proposal holds an edit proposal payload on proposal leases.
type Proposal struct {
	Target     string `json:"target"`
	Repository string `json:"repository"`
	File       string `json:"file"`
	EditKind   string `json:"edit_kind"`
	Change     string `json:"change"`
	Reason     string `json:"reason"`
}

// Incident holds a Bellbuoy incident payload on incident leases.
type Incident struct {
	ID     string `json:"id"`
	Worker string `json:"worker"`
	Code   string `json:"code"`
	State  string `json:"state"`
}

// Step is one leased queue step returned by POST /v1/lease.
type Step struct {
	Lease    string                     `json:"lease"`
	Kind     string                     `json:"kind"`
	ID       string                     `json:"id"`
	State    string                     `json:"state"`
	Lane     string                     `json:"lane"`
	Attempt  int                        `json:"attempt"`
	Expires  string                     `json:"expires"`
	Found    *Found                     `json:"found,omitempty"`
	Results  map[string]json.RawMessage `json:"results,omitempty"`
	Proposal *Proposal                  `json:"proposal,omitempty"`
	Incident *Incident                  `json:"incident,omitempty"`
}

// HeartbeatReply is the response to POST /v1/heartbeat.
type HeartbeatReply struct {
	Lease   string `json:"lease"`
	Expires string `json:"expires"`
}

// CompleteReply is the response to POST /v1/complete.
type CompleteReply struct {
	ID    string `json:"id"`
	State string `json:"state"`
	Next  string `json:"next,omitempty"`
}

// FailReply is the response to POST /v1/fail.
type FailReply struct {
	ID    string `json:"id"`
	State string `json:"state"`
	Next  string `json:"next,omitempty"`
	Held  bool   `json:"held,omitempty"`
}

// PauseReply is the response to POST /v1/needs_subtool.
type PauseReply struct {
	ID    string `json:"id"`
	State string `json:"state"`
}

// Refusal is Muster's structured error envelope.
type Refusal struct {
	Status int          `json:"-"`
	Code   errcode.Code `json:"error"`
	Cause  string       `json:"cause"`
	Reason string       `json:"reason"`
}

func (r *Refusal) Error() string {
	return fmt.Sprintf("muster refused (%d %s %s)", r.Status, r.Code, r.Cause)
}

// Retryable reports whether the refusal is a transient hub condition that
// should remain in the spool and be retried.
func (r *Refusal) Retryable() bool {
	return r.Status == http.StatusTooManyRequests ||
		r.Status == http.StatusServiceUnavailable ||
		r.Status == http.StatusBadGateway ||
		r.Status == http.StatusGatewayTimeout ||
		r.Code == errcode.Timeout
}

// LeaseNotHeld reports whether the lease expired or belongs to another worker.
func (r *Refusal) LeaseNotHeld() bool {
	return r.Status == http.StatusConflict && r.Cause == "lease_not_held"
}

// MoveNotAllowed reports whether Muster's state tables forbid the transition.
func (r *Refusal) MoveNotAllowed() bool {
	return r.Status == http.StatusConflict && r.Cause == "move_not_allowed"
}

// Client is the interface Sextant's worker uses to talk to Muster.
type Client interface {
	Lease(ctx context.Context) (*Step, error)
	Heartbeat(ctx context.Context, lease string) (*HeartbeatReply, error)
	Complete(ctx context.Context, lease, state string, result any) (*CompleteReply, error)
	Fail(ctx context.Context, lease string, code errcode.Code) (*FailReply, error)
	NeedsSubtool(ctx context.Context, lease string) (*PauseReply, error)
}

// HTTPConfig configures an HTTP Client for Muster.
type HTTPConfig struct {
	BaseURL     string
	TokenFile   string
	StaticToken string // used in tests when TokenFile is empty
	Timeout     time.Duration
	HTTPClient  *http.Client
}

// HTTP talks to Muster over cluster-private HTTP using contract v1.
type HTTP struct {
	baseURL     string
	tokenFile   string
	staticToken string
	client      *http.Client
}

// NewHTTP builds a contract v1 HTTP client for Muster.
func NewHTTP(cfg HTTPConfig) (*HTTP, error) {
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		return nil, errors.New("muster: base URL is required")
	}
	hc := cfg.HTTPClient
	if hc == nil {
		timeout := cfg.Timeout
		if timeout <= 0 {
			timeout = 15 * time.Second
		}
		hc = &http.Client{Timeout: timeout}
	}
	return &HTTP{
		baseURL:     base,
		tokenFile:   cfg.TokenFile,
		staticToken: cfg.StaticToken,
		client:      hc,
	}, nil
}

type leaseReq struct {
	Contract string `json:"contract"`
}

type heartbeatReq struct {
	Contract string `json:"contract"`
	Lease    string `json:"lease"`
}

type completeReq struct {
	Contract string `json:"contract"`
	Lease    string `json:"lease"`
	State    string `json:"state"`
	Result   any    `json:"result,omitempty"`
}

type failReq struct {
	Contract string       `json:"contract"`
	Lease    string       `json:"lease"`
	Code     errcode.Code `json:"code"`
}

type pauseReq struct {
	Contract string `json:"contract"`
	Lease    string `json:"lease"`
}

// Lease requests the next step for this worker from POST /v1/lease.
// When Muster answers 204 No Content, Lease returns (nil, nil).
func (c *HTTP) Lease(ctx context.Context) (*Step, error) {
	status, raw, err := c.post(ctx, "/v1/lease", leaseReq{Contract: ContractVersion})
	if err != nil {
		return nil, err
	}
	if status == http.StatusNoContent {
		if len(bytes.TrimSpace(raw)) != 0 {
			return nil, ErrBadResponse
		}
		return nil, nil
	}
	if status != http.StatusOK {
		return nil, parseRefusal(status, raw)
	}
	var step Step
	if err := decodeStrict(raw, &step); err != nil {
		return nil, ErrBadResponse
	}
	if step.Lease == "" || step.ID == "" || step.State == "" || step.Lane == "" || step.Attempt < 1 || step.Expires == "" {
		return nil, ErrBadResponse
	}
	switch step.Kind {
	case "item", "proposal", "incident":
	default:
		return nil, ErrBadResponse
	}
	if _, err := time.Parse(time.RFC3339, step.Expires); err != nil {
		return nil, ErrBadResponse
	}
	return &step, nil
}

// Heartbeat extends an active lease via POST /v1/heartbeat.
func (c *HTTP) Heartbeat(ctx context.Context, lease string) (*HeartbeatReply, error) {
	if strings.TrimSpace(lease) == "" {
		return nil, errors.New("muster: lease is required")
	}
	status, raw, err := c.post(ctx, "/v1/heartbeat", heartbeatReq{
		Contract: ContractVersion,
		Lease:    lease,
	})
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, parseRefusal(status, raw)
	}
	var rep HeartbeatReply
	if err := decodeStrict(raw, &rep); err != nil {
		return nil, ErrBadResponse
	}
	if rep.Lease != lease || rep.Expires == "" {
		return nil, ErrBadResponse
	}
	if _, err := time.Parse(time.RFC3339, rep.Expires); err != nil {
		return nil, ErrBadResponse
	}
	return &rep, nil
}

// Complete reports a completed step via POST /v1/complete.
func (c *HTTP) Complete(ctx context.Context, lease, state string, result any) (*CompleteReply, error) {
	if strings.TrimSpace(lease) == "" || strings.TrimSpace(state) == "" {
		return nil, errors.New("muster: lease and state are required")
	}
	status, raw, err := c.post(ctx, "/v1/complete", completeReq{
		Contract: ContractVersion,
		Lease:    lease,
		State:    state,
		Result:   result,
	})
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, parseRefusal(status, raw)
	}
	var rep CompleteReply
	if err := decodeStrict(raw, &rep); err != nil {
		return nil, ErrBadResponse
	}
	if rep.ID == "" || rep.State != state {
		return nil, ErrBadResponse
	}
	return &rep, nil
}

// Fail reports E_TIMEOUT or E_PARSE via POST /v1/fail.
func (c *HTTP) Fail(ctx context.Context, lease string, code errcode.Code) (*FailReply, error) {
	if strings.TrimSpace(lease) == "" {
		return nil, errors.New("muster: lease is required")
	}
	if code != errcode.Timeout && code != errcode.Parse {
		return nil, errors.New("muster: /v1/fail only accepts E_TIMEOUT or E_PARSE")
	}
	status, raw, err := c.post(ctx, "/v1/fail", failReq{
		Contract: ContractVersion,
		Lease:    lease,
		Code:     code,
	})
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, parseRefusal(status, raw)
	}
	var rep FailReply
	if err := decodeStrict(raw, &rep); err != nil {
		return nil, ErrBadResponse
	}
	if rep.ID == "" || rep.State == "" {
		return nil, ErrBadResponse
	}
	// CONTRACT.md: /v1/fail returns id, state, and either next or held (never both, never neither).
	if (rep.Held && rep.Next != "") || (!rep.Held && rep.Next == "") {
		return nil, ErrBadResponse
	}
	return &rep, nil
}

// NeedsSubtool pauses an item with no matching rule via POST /v1/needs_subtool.
func (c *HTTP) NeedsSubtool(ctx context.Context, lease string) (*PauseReply, error) {
	if strings.TrimSpace(lease) == "" {
		return nil, errors.New("muster: lease is required")
	}
	status, raw, err := c.post(ctx, "/v1/needs_subtool", pauseReq{
		Contract: ContractVersion,
		Lease:    lease,
	})
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, parseRefusal(status, raw)
	}
	var rep PauseReply
	if err := decodeStrict(raw, &rep); err != nil {
		return nil, ErrBadResponse
	}
	if rep.ID == "" || rep.State != "needs_subtool" {
		return nil, ErrBadResponse
	}
	return &rep, nil
}

// Healthz checks GET /healthz without sending a bearer token.
func (c *HTTP) Healthz(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return classifyTransportErr(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return classifyTransportErr(err)
	}
	if resp.StatusCode != http.StatusOK {
		return parseRefusal(resp.StatusCode, raw)
	}
	var body struct {
		Status string `json:"status"`
	}
	if err := decodeStrict(raw, &body); err != nil || body.Status != "ok" {
		return ErrBadResponse
	}
	return nil
}

func (c *HTTP) post(ctx context.Context, path string, body any) (int, []byte, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return 0, nil, err
	}
	if len(payload) > MaxRequestBytes {
		return 0, nil, ErrRequestTooLarge
	}
	token, err := c.readToken()
	if err != nil {
		return 0, nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(payload))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return 0, nil, classifyTransportErr(err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return 0, nil, classifyTransportErr(err)
	}
	if len(raw) > maxResponseBytes {
		return 0, nil, ErrBadResponse
	}
	return resp.StatusCode, raw, nil
}

func (c *HTTP) readToken() (string, error) {
	if c.tokenFile != "" {
		b, err := os.ReadFile(c.tokenFile)
		if err != nil {
			return "", errors.New("muster: cannot read ServiceAccount token file")
		}
		return strings.TrimSpace(string(b)), nil
	}
	return strings.TrimSpace(c.staticToken), nil
}

func parseRefusal(status int, raw []byte) error {
	var wire struct {
		Error  *string `json:"error"`
		Cause  *string `json:"cause"`
		Reason *string `json:"reason"`
	}
	if err := decodeStrict(raw, &wire); err != nil || wire.Error == nil || wire.Cause == nil || wire.Reason == nil {
		if status == http.StatusTooManyRequests || status >= 500 {
			return errcode.New(errcode.Timeout, errcode.UpstreamUnavailable)
		}
		return ErrBadResponse
	}
	code := errcode.Code(*wire.Error)
	switch code {
	case errcode.Timeout, errcode.Parse, errcode.NeedsSubtool:
	default:
		return ErrBadResponse
	}
	if *wire.Cause == "" || *wire.Reason == "" {
		return ErrBadResponse
	}
	return &Refusal{
		Status: status,
		Code:   code,
		Cause:  *wire.Cause,
		Reason: *wire.Reason,
	}
}

func decodeStrict(raw []byte, into any) error {
	if !utf8.Valid(raw) || !json.Valid(raw) {
		return ErrBadResponse
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return ErrBadResponse
	}
	if hasDuplicateKeys(trimmed) {
		return ErrBadResponse
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return ErrBadResponse
	}
	return nil
}

func hasDuplicateKeys(raw []byte) bool {
	dec := json.NewDecoder(bytes.NewReader(raw))
	ok := checkValueNoDup(dec)
	return !ok
}

func checkValueNoDup(dec *json.Decoder) bool {
	tok, err := dec.Token()
	if err != nil {
		return false
	}
	delim, isDelim := tok.(json.Delim)
	if !isDelim {
		return true
	}
	switch delim {
	case '[':
		for dec.More() {
			if !checkValueNoDup(dec) {
				return false
			}
		}
		_, err := dec.Token()
		return err == nil
	case '{':
		seen := map[string]bool{}
		for dec.More() {
			kTok, err := dec.Token()
			if err != nil {
				return false
			}
			k, ok := kTok.(string)
			if !ok || seen[k] {
				return false
			}
			seen[k] = true
			if !checkValueNoDup(dec) {
				return false
			}
		}
		_, err := dec.Token()
		return err == nil
	}
	return false
}

func classifyTransportErr(err error) error {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return errcode.New(errcode.Timeout, errcode.DeadlineExceeded)
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return errcode.New(errcode.Timeout, errcode.DeadlineExceeded)
	}
	return errcode.New(errcode.Timeout, errcode.UpstreamUnavailable)
}
