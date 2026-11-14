// Package browserjob is the on-demand-browser-job sub-tool: it inspects
// rendered DOM snapshots and client-side network logs produced by an ephemeral
// headless browser Job for JavaScript single-page applications.
//
// All raw rendered markup and network URLs remain sealed here; only closed
// observation facts leave.
package browserjob

import (
	"bytes"
	"encoding/json"
	"io"
	"net/url"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/ClearedStack/sextant/internal/errcode"
	"github.com/ClearedStack/sextant/internal/markup"
	"github.com/ClearedStack/sextant/internal/observation"
	"github.com/ClearedStack/sextant/internal/subtool"
)

// CaptureFormat is the format identifier written by an ephemeral browser Job.
const CaptureFormat = "sextant-browser-capture/v1"

// Capture is the structured capture produced by an ephemeral browser Job.
type Capture struct {
	Format          string           `json:"format"`
	RenderedHTML    string           `json:"rendered_html"`
	NetworkRequests []NetworkRequest `json:"network_requests"`
}

// NetworkRequest is one client-side HTTP request observed while rendering the
// single-page application and applying synthetic local inputs.
type NetworkRequest struct {
	Method string `json:"method"`
	URL    string `json:"url"`
}

// NeedsBrowserJob reports whether raw HTML looks like an unrendered single-page
// application shell (HTML with an external script bundle, zero forms, and zero
// feature tables) that requires an ephemeral browser Job before extraction.
func NeedsBrowserJob(raw []byte) bool {
	if !markup.LooksLikeHTML(raw) {
		return false
	}
	hasScriptSrc := false
	hasFormOrTable := false
	err := markup.Walk(raw, func(tok markup.Token) error {
		if tok.Kind != markup.Start {
			return nil
		}
		switch tok.Name {
		case "form", "table":
			hasFormOrTable = true
		case "script":
			if strings.TrimSpace(tok.Attrs["src"]) != "" {
				hasScriptSrc = true
			}
		}
		return nil
	})
	return err == nil && hasScriptSrc && !hasFormOrTable
}

// Tool is the on-demand-browser-job sub-tool.
type Tool struct{ rules *rules }

// New loads the embedded match.yaml.
func New() (subtool.Tool, error) {
	r, err := parseRules(matchYAML)
	if err != nil {
		return nil, err
	}
	return &Tool{rules: r}, nil
}

// Name is the registry name.
func (*Tool) Name() string { return Name }

type control struct {
	key      string
	name     string
	required bool
}

type form struct {
	role     string
	action   string
	controls []control
}

type domPage struct {
	forms     []form
	outputs   int
	headline  string
	text      string
	lower     string
	resources []string
}

// Observe matches sextant-browser-capture/v1 payloads on HTTPS targets and
// extracts Seen and Stated facts from the rendered DOM and network log.
func (t *Tool) Observe(target subtool.Target) (*observation.Record, error) {
	r := t.rules
	if !r.schemes[target.URL.Scheme] || !looksLikeJSON(target.Page) {
		return nil, subtool.ErrNoMatch
	}
	capDoc, matched, err := r.decodeCapture(target.Page)
	if err != nil {
		return nil, err
	}
	if !matched {
		return nil, subtool.ErrNoMatch
	}
	if !markup.LooksLikeHTML([]byte(capDoc.RenderedHTML)) {
		return nil, errcode.New(errcode.Parse, errcode.BrokenBrowserCapture)
	}

	pg, err := r.readDOM([]byte(capDoc.RenderedHTML))
	if err != nil {
		return nil, err
	}
	var toolForms []form
	for _, f := range pg.forms {
		if !r.ignoreFormRoles[f.role] {
			toolForms = append(toolForms, f)
		}
	}
	if len(toolForms) != 1 {
		return nil, errcode.New(errcode.NeedsSubtool, errcode.NoMatchingSubTool)
	}

	tool := toolForms[0]
	fields, gated, err := r.fields(tool)
	if err != nil {
		return nil, err
	}

	netHosts, netRoundTrip, err := r.inspectNetwork(target.URL, pg.resources, capDoc.NetworkRequests)
	if err != nil {
		return nil, err
	}

	rec := &observation.Record{
		ItemID:     target.ItemID,
		SourceURL:  target.SourceURL(),
		SubTool:    Name,
		ObservedOn: target.ObservedOn,
		Inputs:     observation.Inputs{Fields: []observation.InputField{}, Tier: observation.Unknown},
		Outputs:    observation.Outputs{Fields: []observation.OutputField{}, Tier: observation.Unknown},
		Tracking:   observation.Tracking{ExternalHosts: netHosts, Tier: observation.Seen},
	}

	if rec.CoreJob, err = classify(r.coreJob, pg.headline, errcode.AmbiguousCoreJob); err != nil {
		return nil, err
	}
	if rec.LicenseOrCopyright, err = classify(r.license, pg.lower, errcode.AmbiguousLicense); err != nil {
		return nil, err
	}
	if rec.PriceModel, err = classify(r.price, pg.lower, errcode.AmbiguousPriceModel); err != nil {
		return nil, err
	}

	if gated {
		rec.LoginRequirement = observation.Known("required", observation.Seen)
		rec.OfflineBehavior = observation.NotKnown()
		if netRoundTrip {
			rec.OfflineBehavior = observation.Known("server_round_trip", observation.Seen)
		}
	} else {
		if rec.LoginRequirement, err = classify(r.login, pg.lower, errcode.AmbiguousLogin); err != nil {
			return nil, err
		}
		if len(fields) > 0 {
			rec.Inputs = observation.Inputs{Fields: fields, Tier: observation.Seen}
		}
		for i := 0; i < pg.outputs; i++ {
			rec.Outputs.Fields = append(rec.Outputs.Fields, observation.OutputField{Kind: r.outputKind})
			rec.Outputs.Tier = observation.Seen
		}
		if submitsToServer(tool.action) || netRoundTrip {
			rec.OfflineBehavior = observation.Known("server_round_trip", observation.Seen)
		} else {
			rec.OfflineBehavior = observation.Known("works_offline", observation.Seen)
		}
	}

	rec.CitedPublicRules = r.cite(pg.text)

	if err := rec.Validate(); err != nil {
		return nil, errcode.New(errcode.Parse, errcode.RecordFailedSchema)
	}
	return rec, nil
}

func looksLikeJSON(raw []byte) bool {
	p := bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))
	p = bytes.TrimLeft(p, " \t\r\n")
	return len(p) > 0 && p[0] == '{'
}

func (r *rules) decodeCapture(raw []byte) (*Capture, bool, error) {
	if !utf8.Valid(raw) {
		return nil, false, errcode.New(errcode.Parse, errcode.InvalidText)
	}
	var probe struct {
		Format *string `json:"format"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, false, errcode.New(errcode.Parse, errcode.BrokenBrowserCapture)
	}
	if probe.Format == nil || *probe.Format != r.captureFormat {
		return nil, false, nil
	}

	var capDoc Capture
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&capDoc); err != nil {
		return nil, true, errcode.New(errcode.Parse, errcode.BrokenBrowserCapture)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, true, errcode.New(errcode.Parse, errcode.BrokenBrowserCapture)
	}
	if capDoc.NetworkRequests == nil {
		return nil, true, errcode.New(errcode.Parse, errcode.BrokenBrowserCapture)
	}
	return &capDoc, true, nil
}

func (r *rules) readDOM(raw []byte) (*domPage, error) {
	pg := &domPage{}
	var headline, text spaced
	open := -1
	h1 := 0
	last := ""

	err := markup.Walk(raw, func(tok markup.Token) error {
		switch tok.Kind {
		case markup.Start:
			last = tok.Name
			role := strings.ToLower(strings.TrimSpace(tok.Attrs["role"]))
			if r.outputRoles[role] && (open < 0 || !r.ignoreFormRoles[pg.forms[open].role]) {
				pg.outputs++
			} else if r.outputTags[tok.Name] && (open < 0 || !r.ignoreFormRoles[pg.forms[open].role]) {
				pg.outputs++
			}
			switch tok.Name {
			case "form":
				if open >= 0 {
					return errcode.New(errcode.Parse, errcode.BrokenFormNesting)
				}
				pg.forms = append(pg.forms, form{
					role:   role,
					action: strings.TrimSpace(tok.Attrs["action"]),
				})
				open = len(pg.forms) - 1
			case "input", "select", "textarea", "button":
				if open < 0 {
					break
				}
				key := tok.Name
				if tok.Name == "input" {
					typ := strings.ToLower(strings.TrimSpace(tok.Attrs["type"]))
					if typ == "" {
						typ = "text"
					}
					key = "input:" + typ
				}
				pg.forms[open].controls = append(pg.forms[open].controls,
					control{key: key, name: tok.Attrs["name"], required: tok.Has("required")})
			case "h1":
				h1++
			case "script", "img", "iframe":
				if src := strings.TrimSpace(tok.Attrs["src"]); src != "" {
					pg.resources = append(pg.resources, src)
				}
			case "link":
				href := strings.TrimSpace(tok.Attrs["href"])
				if href != "" && hasWord(tok.Attrs["rel"], "stylesheet") {
					pg.resources = append(pg.resources, href)
				}
			}
		case markup.End:
			switch tok.Name {
			case "form":
				if open < 0 {
					return errcode.New(errcode.Parse, errcode.BrokenFormNesting)
				}
				open = -1
			case "h1":
				if h1 > 0 {
					h1--
				}
			}
		case markup.Text:
			if tok.Raw {
				if last == "title" {
					headline.add(tok.Text)
					text.add(tok.Text)
				}
				return nil
			}
			text.add(tok.Text)
			if h1 > 0 {
				headline.add(tok.Text)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if open >= 0 {
		return nil, errcode.New(errcode.Parse, errcode.BrokenFormNesting)
	}
	pg.headline = strings.ToLower(headline.String())
	pg.text = text.String()
	pg.lower = strings.ToLower(pg.text)
	return pg, nil
}

func (r *rules) fields(f form) (fields []observation.InputField, gated bool, err error) {
	group := map[string]int{}
	for _, c := range f.controls {
		switch {
		case r.ignored[c.key]:
		case r.credential[c.key]:
			gated = true
		default:
			kind, ok := r.kinds[c.key]
			if !ok {
				return nil, false, errcode.New(errcode.NeedsSubtool, errcode.UnfamiliarFormControl)
			}
			if r.grouped[c.key] && c.name != "" {
				id := c.key + "\x00" + c.name
				if i, seen := group[id]; seen {
					fields[i].Required = fields[i].Required || c.required
					continue
				}
				group[id] = len(fields)
			}
			fields = append(fields, observation.InputField{Kind: kind, Required: c.required})
		}
	}
	return fields, gated, nil
}

func (r *rules) inspectNetwork(pageURL *url.URL, domRefs []string, reqs []NetworkRequest) ([]string, bool, error) {
	own := hostOf(pageURL)
	seen := map[string]bool{}
	hosts := []string{}
	roundTrip := false

	addRef := func(raw string) (*url.URL, error) {
		u, err := pageURL.Parse(raw)
		if err != nil {
			return nil, errcode.New(errcode.Parse, errcode.BrokenResourceLink)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return u, nil
		}
		h := hostOf(u)
		if h != "" && h != own && !seen[h] {
			seen[h] = true
			hosts = append(hosts, h)
		}
		return u, nil
	}

	for _, ref := range domRefs {
		if _, err := addRef(ref); err != nil {
			return nil, false, err
		}
	}
	for _, req := range reqs {
		meth := strings.ToUpper(strings.TrimSpace(req.Method))
		if meth == "" || strings.TrimSpace(req.URL) == "" {
			return nil, false, errcode.New(errcode.Parse, errcode.BrokenBrowserCapture)
		}
		u, err := addRef(req.URL)
		if err != nil {
			return nil, false, err
		}
		if r.roundTripMethods[meth] {
			roundTrip = true
			continue
		}
		lowerPath := strings.ToLower(u.Path)
		for _, pref := range r.roundTripPathPrefixes {
			if strings.HasPrefix(lowerPath, pref) {
				roundTrip = true
				break
			}
		}
	}

	sort.Strings(hosts)
	return hosts, roundTrip, nil
}

func classify(list []wordRule, lower string, ambiguous errcode.Reason) (observation.Fact, error) {
	found := ""
	for _, rule := range list {
		for _, phrase := range rule.phrases {
			if strings.Contains(lower, phrase) {
				if found != "" && found != rule.value {
					return observation.Fact{}, errcode.New(errcode.NeedsSubtool, ambiguous)
				}
				found = rule.value
				break
			}
		}
	}
	if found == "" {
		return observation.NotKnown(), nil
	}
	return observation.Known(found, observation.Stated), nil
}

func (r *rules) cite(text string) observation.CitedRules {
	seen := map[string]bool{}
	out := []string{}
	for _, c := range r.citations {
		for _, m := range c.pattern.FindAllStringSubmatchIndex(text, -1) {
			s := string(c.pattern.ExpandString(nil, c.write, text, m))
			if !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	if len(out) == 0 {
		return observation.CitedRules{Citations: out, Tier: observation.Unknown}
	}
	sort.Strings(out)
	return observation.CitedRules{Citations: out, Tier: observation.Seen}
}

func submitsToServer(action string) bool {
	a := strings.ToLower(action)
	return a != "" && !strings.HasPrefix(a, "#") && !strings.HasPrefix(a, "javascript:")
}

func hostOf(u *url.URL) string {
	return strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
}

func hasWord(list, word string) bool {
	for _, w := range strings.Fields(strings.ToLower(list)) {
		if w == word {
			return true
		}
	}
	return false
}

type spaced struct {
	b   strings.Builder
	gap bool
}

func (s *spaced) add(piece string) {
	for _, c := range piece {
		if unicode.IsSpace(c) {
			s.gap = true
			continue
		}
		if s.gap && s.b.Len() > 0 {
			s.b.WriteByte(' ')
		}
		s.gap = false
		s.b.WriteRune(c)
	}
	s.gap = true
}

func (s *spaced) String() string { return s.b.String() }

func normalise(text string) string {
	var s spaced
	s.add(text)
	return strings.ToLower(s.String())
}
