// Package govportal is the gov-portal sub-tool: federal .gov and .mil portals
// and calculators. match.yaml holds the matching rule and every word list;
// this file holds the logic; fixtures/ holds saved pages and the exact output
// expected for each.
//
// The extractor reads the page it is given and nothing else. It submits no
// form, follows no link and runs no script.
package govportal

import (
	"net/url"
	"sort"
	"strings"
	"unicode"

	"github.com/clearedstack/sextant/internal/errcode"
	"github.com/clearedstack/sextant/internal/markup"
	"github.com/clearedstack/sextant/internal/observation"
	"github.com/clearedstack/sextant/internal/subtool"
)

// Tool is the gov-portal sub-tool.
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
	key      string // "input:<type>" or the tag name
	name     string
	required bool
}

type form struct {
	role     string
	action   string
	controls []control
}

// page is what one pass over the tokens collects.
type page struct {
	forms     []form
	outputs   int      // <output> elements outside ignored forms
	headline  string   // <title> and <h1> text, lower-cased
	text      string   // <title> and all visible text, white space collapsed
	lower     string   // text, lower-cased
	resources []string // addresses the static markup loads
}

// Observe applies the matching rule and, if it holds, extracts the record.
func (t *Tool) Observe(target subtool.Target) (*observation.Record, error) {
	r := t.rules
	host := hostOf(target.URL)
	if !r.schemes[target.URL.Scheme] || !hasSuffix(host, r.hostSuffixes) || !markup.LooksLikeHTML(target.Page) {
		return nil, subtool.ErrNoMatch
	}
	pg, err := r.read(target.Page)
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
		return nil, subtool.ErrNoMatch
	}
	tool := toolForms[0]

	fields, gated, err := r.fields(tool)
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
	if rec.CoreJob, err = classify(r.coreJob, pg.headline, errcode.AmbiguousCoreJob); err != nil {
		return nil, err
	}
	if rec.LicenseOrCopyright, err = classify(r.license, pg.lower, errcode.AmbiguousLicense); err != nil {
		return nil, err
	}
	if rec.PriceModel, err = classify(r.price, pg.lower, errcode.AmbiguousPriceModel); err != nil {
		return nil, err
	}

	// A credential field on the one public form is a login gate. Observation
	// stops at the public surface: what the tool asks for and gives back
	// behind the gate is not recorded.
	if gated {
		rec.LoginRequirement = observation.Known("required", observation.Seen)
	} else {
		if rec.LoginRequirement, err = classify(r.login, pg.lower, errcode.AmbiguousLogin); err != nil {
			return nil, err
		}
		if len(fields) > 0 {
			rec.Inputs = observation.Inputs{Fields: fields, Tier: observation.Seen}
		}
		for i := 0; i < pg.outputs; i++ {
			rec.Outputs.Fields = append(rec.Outputs.Fields, observation.OutputField{Kind: "computed_value"})
			rec.Outputs.Tier = observation.Seen
		}
	}

	// A form that declares a server address to submit to is Seen to need a
	// round trip. Without one, only what the page says is left.
	if !gated && submitsToServer(tool.action) {
		rec.OfflineBehavior = observation.Known("server_round_trip", observation.Seen)
	} else if rec.OfflineBehavior, err = classify(r.offline, pg.lower, errcode.AmbiguousOffline); err != nil {
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

// read walks the page's tokens once.
func (r *rules) read(raw []byte) (*page, error) {
	pg := &page{}
	var headline, text spaced
	open := -1 // index of the form being read, or -1
	h1 := 0
	last := "" // name of the most recent start tag
	err := markup.Walk(raw, func(tok markup.Token) error {
		switch tok.Kind {
		case markup.Start:
			last = tok.Name
			switch tok.Name {
			case "form":
				if open >= 0 {
					return errcode.New(errcode.Parse, errcode.BrokenFormNesting)
				}
				pg.forms = append(pg.forms, form{
					role:   strings.ToLower(strings.TrimSpace(tok.Attrs["role"])),
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
			case "output":
				if open < 0 || !r.ignoreFormRoles[pg.forms[open].role] {
					pg.outputs++
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
				return nil // script, style and textarea bodies are not page text
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

// fields turns the tool form's controls into input fields, in page order.
func (r *rules) fields(f form) (fields []observation.InputField, gated bool, err error) {
	group := map[string]int{} // "control\x00name" -> index into fields
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

// classify applies one word list to text that is already lower-case with
// its white space collapsed.
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

// cite finds every public-rule citation in text.
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

// externalHosts lists the hosts, other than the page's own, that the static
// markup loads scripts, images, frames or stylesheets from.
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
			continue // data: and the like load nothing from a host
		}
		if h := hostOf(u); h != "" && h != own && !seen[h] {
			seen[h] = true
			hosts = append(hosts, h)
		}
	}
	sort.Strings(hosts)
	return observation.Tracking{ExternalHosts: hosts, Tier: observation.Seen}, nil
}

func submitsToServer(action string) bool {
	a := strings.ToLower(action)
	return a != "" && !strings.HasPrefix(a, "#") && !strings.HasPrefix(a, "javascript:")
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

// spaced gathers pieces of text, turning every run of white space, and every
// join between pieces, into one space.
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

// normalise lower-cases text and collapses its white space. strings.ToLower
// uses fixed Unicode tables, not the machine's locale, so the result is the
// same on every machine.
func normalise(text string) string {
	var s spaced
	s.add(text)
	return strings.ToLower(s.String())
}
