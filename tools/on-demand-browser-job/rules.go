package browserjob

import (
	_ "embed"
	"fmt"
	"regexp"
	"strings"

	"github.com/ClearedStack/sextant/internal/observation"
	"github.com/ClearedStack/sextant/internal/rulefile"
)

//go:embed match.yaml
var matchYAML []byte

// Name is this sub-tool's registry name.
const Name = "on-demand-browser-job"

type wordRule struct {
	value   string
	phrases []string
}

type citationRule struct {
	pattern *regexp.Regexp
	write   string
}

type rules struct {
	schemes         map[string]bool
	captureFormat   string
	ignoreFormRoles map[string]bool

	ignored    map[string]bool
	credential map[string]bool
	grouped    map[string]bool
	kinds      map[string]string

	outputTags  map[string]bool
	outputRoles map[string]bool
	outputKind  string

	roundTripMethods      map[string]bool
	roundTripPathPrefixes []string

	coreJob, login, license, price []wordRule
	citations                      []citationRule
}

func parseRules(src []byte) (*rules, error) {
	root, err := rulefile.Parse(src)
	if err != nil {
		return nil, err
	}
	top, err := rulefile.Map(root, "match.yaml", "tool", "match", "controls", "outputs", "network",
		"core_job", "login_requirement", "license_or_copyright", "price_model", "citations")
	if err != nil {
		return nil, err
	}
	if name, _ := rulefile.String(top["tool"], "tool"); name != Name {
		return nil, fmt.Errorf("tool: expected %q", Name)
	}
	r := &rules{kinds: map[string]string{}}

	m, err := rulefile.Map(top["match"], "match", "schemes", "capture_format", "ignore_form_roles")
	if err != nil {
		return nil, err
	}
	if r.schemes, err = stringSet(m["schemes"], "match.schemes"); err != nil {
		return nil, err
	}
	if r.captureFormat, err = rulefile.String(m["capture_format"], "match.capture_format"); err != nil {
		return nil, err
	}
	if r.ignoreFormRoles, err = stringSet(m["ignore_form_roles"], "match.ignore_form_roles"); err != nil {
		return nil, err
	}

	c, err := rulefile.Map(top["controls"], "controls", "ignored", "credential", "grouped_by_name", "kinds")
	if err != nil {
		return nil, err
	}
	if r.ignored, err = stringSet(c["ignored"], "controls.ignored"); err != nil {
		return nil, err
	}
	if r.credential, err = stringSet(c["credential"], "controls.credential"); err != nil {
		return nil, err
	}
	if r.grouped, err = stringSet(c["grouped_by_name"], "controls.grouped_by_name"); err != nil {
		return nil, err
	}
	kinds, err := rulefile.List(c["kinds"], "controls.kinds")
	if err != nil {
		return nil, err
	}
	for _, k := range kinds {
		km, err := rulefile.Map(k, "controls.kinds", "kind", "controls")
		if err != nil {
			return nil, err
		}
		kind, _ := rulefile.String(km["kind"], "controls.kinds.kind")
		if !observation.InputKinds[kind] {
			return nil, fmt.Errorf("controls.kinds: %q is not an input kind", kind)
		}
		controls, err := rulefile.Strings(km["controls"], "controls.kinds.controls")
		if err != nil {
			return nil, err
		}
		for _, ctl := range controls {
			if _, dup := r.kinds[ctl]; dup || r.ignored[ctl] || r.credential[ctl] {
				return nil, fmt.Errorf("controls: %q is listed twice", ctl)
			}
			r.kinds[ctl] = kind
		}
	}
	for ctl := range r.credential {
		if r.ignored[ctl] {
			return nil, fmt.Errorf("controls: %q is listed twice", ctl)
		}
	}
	for ctl := range r.grouped {
		if _, ok := r.kinds[ctl]; !ok {
			return nil, fmt.Errorf("controls.grouped_by_name: %q has no kind", ctl)
		}
	}

	outMap, err := rulefile.Map(top["outputs"], "outputs", "tags", "roles", "kind")
	if err != nil {
		return nil, err
	}
	if r.outputTags, err = stringSet(outMap["tags"], "outputs.tags"); err != nil {
		return nil, err
	}
	if r.outputRoles, err = stringSet(outMap["roles"], "outputs.roles"); err != nil {
		return nil, err
	}
	if r.outputKind, err = rulefile.String(outMap["kind"], "outputs.kind"); err != nil {
		return nil, err
	}
	if !observation.OutputKinds[r.outputKind] {
		return nil, fmt.Errorf("outputs.kind: %q is outside OutputKinds", r.outputKind)
	}

	netMap, err := rulefile.Map(top["network"], "network", "round_trip_methods", "round_trip_path_prefixes")
	if err != nil {
		return nil, err
	}
	if r.roundTripMethods, err = stringSet(netMap["round_trip_methods"], "network.round_trip_methods"); err != nil {
		return nil, err
	}
	for meth := range r.roundTripMethods {
		if meth != strings.ToUpper(meth) {
			return nil, fmt.Errorf("network.round_trip_methods: %q must be upper-case", meth)
		}
	}
	if r.roundTripPathPrefixes, err = rulefile.Strings(netMap["round_trip_path_prefixes"], "network.round_trip_path_prefixes"); err != nil {
		return nil, err
	}
	for _, p := range r.roundTripPathPrefixes {
		if !strings.HasPrefix(p, "/") || p != strings.ToLower(p) {
			return nil, fmt.Errorf("network.round_trip_path_prefixes: %q must be a lower-case path prefix", p)
		}
	}

	lists := []struct {
		key   string
		vocab map[string]bool
		into  *[]wordRule
	}{
		{"core_job", observation.CoreJobs, &r.coreJob},
		{"login_requirement", observation.LoginRequirements, &r.login},
		{"license_or_copyright", observation.Licenses, &r.license},
		{"price_model", observation.PriceModels, &r.price},
	}
	for _, l := range lists {
		if *l.into, err = wordRules(top[l.key], l.key, l.vocab); err != nil {
			return nil, err
		}
	}

	cites, err := rulefile.List(top["citations"], "citations")
	if err != nil {
		return nil, err
	}
	for _, n := range cites {
		cm, err := rulefile.Map(n, "citations", "family", "pattern", "write")
		if err != nil {
			return nil, err
		}
		pattern, _ := rulefile.String(cm["pattern"], "citations.pattern")
		write, _ := rulefile.String(cm["write"], "citations.write")
		if _, err := rulefile.String(cm["family"], "citations.family"); err != nil {
			return nil, err
		}
		re, err := regexp.Compile(pattern)
		if err != nil || re.NumSubexp() != 1 || write == "" {
			return nil, fmt.Errorf("citations: a pattern needs exactly one group and a \"write\"")
		}
		r.citations = append(r.citations, citationRule{pattern: re, write: write})
	}
	return r, nil
}

func wordRules(n rulefile.Node, where string, vocab map[string]bool) ([]wordRule, error) {
	items, err := rulefile.List(n, where)
	if err != nil {
		return nil, err
	}
	var out []wordRule
	seen := map[string]bool{}
	for _, item := range items {
		m, err := rulefile.Map(item, where, "value", "phrases")
		if err != nil {
			return nil, err
		}
		value, _ := rulefile.String(m["value"], where)
		if !vocab[value] || seen[value] {
			return nil, fmt.Errorf("%s: value %q is not allowed here or is repeated", where, value)
		}
		seen[value] = true
		phrases, err := rulefile.Strings(m["phrases"], where)
		if err != nil {
			return nil, err
		}
		for _, p := range phrases {
			if p != normalise(p) {
				return nil, fmt.Errorf("%s: phrase %q must be lower-case with single spaces", where, p)
			}
		}
		out = append(out, wordRule{value: value, phrases: phrases})
	}
	return out, nil
}

func stringSet(n rulefile.Node, where string) (map[string]bool, error) {
	list, err := rulefile.Strings(n, where)
	if err != nil {
		return nil, err
	}
	set := make(map[string]bool, len(list))
	for _, s := range list {
		set[s] = true
	}
	return set, nil
}
