// Package saasmarketing is the saas-marketing sub-tool: public vendor landing
// pages and feature tables on commercial (non-.gov, non-.mil) HTTPS hosts.
//
// It inspects public markup passively, never logs in or submits forms, and
// records stated inputs and outputs from the page's feature table using only
// closed vocabularies.
package saasmarketing

import (
	"net/url"
	"sort"
	"strings"
	"unicode"

	"github.com/ClearedStack/sextant/internal/errcode"
	"github.com/ClearedStack/sextant/internal/markup"
	"github.com/ClearedStack/sextant/internal/observation"
	"github.com/ClearedStack/sextant/internal/subtool"
)

// Tool is the saas-marketing sub-tool.
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

type tableCell struct {
	header bool
	text   string
}

type featureTable struct {
	rows [][]tableCell
}

type page struct {
	tables    []featureTable
	loginGate bool
	headline  string
	text      string
	lower     string
	resources []string
}

// Observe matches public non-government HTTPS HTML pages carrying one feature
// table and extracts the structured observation record.
func (t *Tool) Observe(target subtool.Target) (*observation.Record, error) {
	r := t.rules
	host := hostOf(target.URL)
	if host == "" || !r.schemes[target.URL.Scheme] || hasSuffix(host, r.excludeHostSuffixes) || !markup.LooksLikeHTML(target.Page) {
		return nil, subtool.ErrNoMatch
	}
	pg, err := r.read(target.Page)
	if err != nil {
		return nil, err
	}
	if len(pg.tables) != 1 {
		return nil, subtool.ErrNoMatch
	}

	inputs, outputs, err := r.extractFeatureRows(pg.tables[0])
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
	}
	if len(inputs) > 0 {
		rec.Inputs = observation.Inputs{Fields: inputs, Tier: observation.Stated}
	}
	if len(outputs) > 0 {
		rec.Outputs = observation.Outputs{Fields: outputs, Tier: observation.Stated}
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

	if pg.loginGate {
		rec.LoginRequirement = observation.Known("required", observation.Seen)
	} else if rec.LoginRequirement, err = classify(r.login, pg.lower, errcode.AmbiguousLogin); err != nil {
		return nil, err
	}

	if rec.OfflineBehavior, err = classify(r.offline, pg.lower, errcode.AmbiguousOffline); err != nil {
		return nil, err
	}
	if rec.Tracking, err = externalHosts(target.URL, pg.resources); err != nil {
		return nil, err
	}
	rec.CitedPublicRules = r.cite(pg.text)

	if err := rec.Validate(); err != nil {
		return nil, errcode.New(errcode.Parse, errcode.RecordFailedSchema)
	}
	return rec, nil
}

func (r *rules) read(raw []byte) (*page, error) {
	pg := &page{}
	var headline, text, cellText spaced
	inTable := false
	inRow := false
	inCell := ""
	formDepth := 0
	h1 := 0
	last := ""

	err := markup.Walk(raw, func(tok markup.Token) error {
		switch tok.Kind {
		case markup.Start:
			last = tok.Name
			switch tok.Name {
			case "table":
				if inTable {
					return errcode.New(errcode.Parse, errcode.BrokenTableNesting)
				}
				inTable = true
				pg.tables = append(pg.tables, featureTable{})
			case "tr":
				if !inTable || inRow {
					return errcode.New(errcode.Parse, errcode.BrokenTableNesting)
				}
				inRow = true
				idx := len(pg.tables) - 1
				pg.tables[idx].rows = append(pg.tables[idx].rows, nil)
			case "td", "th":
				if !inRow || inCell != "" {
					return errcode.New(errcode.Parse, errcode.BrokenTableNesting)
				}
				inCell = tok.Name
				cellText = spaced{}
			case "form":
				if formDepth > 0 {
					return errcode.New(errcode.Parse, errcode.BrokenFormNesting)
				}
				formDepth = 1
				if r.isAuthAction(tok.Attrs["action"]) {
					pg.loginGate = true
				}
			case "input":
				if formDepth > 0 {
					typ := strings.ToLower(strings.TrimSpace(tok.Attrs["type"]))
					if typ == "" {
						typ = "text"
					}
					if r.credentialControls["input:"+typ] {
						pg.loginGate = true
					}
				}
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
			case "td", "th":
				if inCell != tok.Name {
					return errcode.New(errcode.Parse, errcode.BrokenTableNesting)
				}
				tIdx := len(pg.tables) - 1
				rIdx := len(pg.tables[tIdx].rows) - 1
				pg.tables[tIdx].rows[rIdx] = append(pg.tables[tIdx].rows[rIdx], tableCell{
					header: r.headerTags[tok.Name],
					text:   normalise(cellText.String()),
				})
				inCell = ""
			case "tr":
				if !inRow || inCell != "" {
					return errcode.New(errcode.Parse, errcode.BrokenTableNesting)
				}
				inRow = false
			case "table":
				if !inTable || inRow || inCell != "" {
					return errcode.New(errcode.Parse, errcode.BrokenTableNesting)
				}
				inTable = false
			case "form":
				if formDepth == 0 {
					return errcode.New(errcode.Parse, errcode.BrokenFormNesting)
				}
				formDepth = 0
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
			if inCell != "" {
				cellText.add(tok.Text)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if inTable || inRow || inCell != "" {
		return nil, errcode.New(errcode.Parse, errcode.BrokenTableNesting)
	}
	if formDepth > 0 {
		return nil, errcode.New(errcode.Parse, errcode.BrokenFormNesting)
	}
	pg.headline = strings.ToLower(headline.String())
	pg.text = text.String()
	pg.lower = strings.ToLower(pg.text)
	return pg, nil
}

func (r *rules) isAuthAction(action string) bool {
	a := strings.ToLower(strings.TrimSpace(action))
	if a == "" {
		return false
	}
	if u, err := url.Parse(a); err == nil && u.Path != "" {
		a = strings.ToLower(u.Path)
	}
	for _, prefix := range r.authActionPrefixes {
		if strings.HasPrefix(a, prefix) {
			return true
		}
	}
	return false
}

func (r *rules) extractFeatureRows(tbl featureTable) ([]observation.InputField, []observation.OutputField, error) {
	var inputs []observation.InputField
	var outputs []observation.OutputField
	for _, row := range tbl.rows {
		if len(row) != 2 || (row[0].header && row[1].header) {
			continue
		}
		label, detail := row[0].text, row[1].text
		switch {
		case r.inputLabels[label]:
			kind, err := matchSingleKind(r.statedInputs, detail)
			if err != nil {
				return nil, nil, err
			}
			req := strings.Contains(label, "required")
			if !req {
				for _, m := range r.requiredMarkers {
					if strings.Contains(detail, m) {
						req = true
						break
					}
				}
			}
			inputs = append(inputs, observation.InputField{Kind: kind, Required: req})
		case r.outputLabels[label]:
			kind, err := matchSingleKind(r.statedOutputs, detail)
			if err != nil {
				return nil, nil, err
			}
			outputs = append(outputs, observation.OutputField{Kind: kind})
		}
	}
	return inputs, outputs, nil
}

func matchSingleKind(list []wordRule, lower string) (string, error) {
	found := ""
	for _, rule := range list {
		for _, phrase := range rule.phrases {
			if strings.Contains(lower, phrase) {
				if found != "" && found != rule.value {
					return "", errcode.New(errcode.NeedsSubtool, errcode.UnfamiliarFeatureRow)
				}
				found = rule.value
				break
			}
		}
	}
	if found == "" {
		return "", errcode.New(errcode.NeedsSubtool, errcode.UnfamiliarFeatureRow)
	}
	return found, nil
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

func externalHosts(pageURL *url.URL, resources []string) (observation.Tracking, error) {
	own := hostOf(pageURL)
	seen := map[string]bool{}
	hosts := []string{}
	for _, ref := range resources {
		u, err := pageURL.Parse(ref)
		if err != nil {
			return observation.Tracking{}, errcode.New(errcode.Parse, errcode.BrokenResourceLink)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			continue
		}
		if h := hostOf(u); h != "" && h != own && !seen[h] {
			seen[h] = true
			hosts = append(hosts, h)
		}
	}
	sort.Strings(hosts)
	return observation.Tracking{ExternalHosts: hosts, Tier: observation.Seen}, nil
}

func hostOf(u *url.URL) string {
	return strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
}

func hasSuffix(host string, suffixes []string) bool {
	for _, s := range suffixes {
		if strings.HasSuffix(host, s) && len(host) > len(s) {
			return true
		}
	}
	return false
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
