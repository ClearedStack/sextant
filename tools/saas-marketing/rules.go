package saasmarketing

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
const Name = "saas-marketing"

type wordRule struct {
	value   string
	phrases []string
}

type citationRule struct {
	pattern *regexp.Regexp
	write   string
}

type rules struct {
	schemes             map[string]bool
	excludeHostSuffixes []string

	headerTags      map[string]bool
	inputLabels     map[string]bool
	requiredMarkers []string
	outputLabels    map[string]bool
	statedInputs    []wordRule
	statedOutputs   []wordRule

	credentialControls map[string]bool
	authActionPrefixes []string

	coreJob, login, license, price, offline []wordRule
	citations                               []citationRule
}

func parseRules(src []byte) (*rules, error) {
	root, err := rulefile.Parse(src)
	if err != nil {
		return nil, err
	}
	top, err := rulefile.Map(root, "match.yaml", "tool", "match", "feature_table", "login_gates",
		"core_job", "login_requirement", "license_or_copyright", "price_model", "offline_behavior", "citations")
	if err != nil {
		return nil, err
	}
	if name, _ := rulefile.String(top["tool"], "tool"); name != Name {
		return nil, fmt.Errorf("tool: expected %q", Name)
	}
	r := &rules{}

	m, err := rulefile.Map(top["match"], "match", "schemes", "exclude_host_suffixes")
	if err != nil {
		return nil, err
	}
	if r.schemes, err = stringSet(m["schemes"], "match.schemes"); err != nil {
		return nil, err
	}
	if r.excludeHostSuffixes, err = rulefile.Strings(m["exclude_host_suffixes"], "match.exclude_host_suffixes"); err != nil {
		return nil, err
	}
	for _, s := range r.excludeHostSuffixes {
		if len(s) < 2 || s[0] != '.' {
			return nil, fmt.Errorf("match.exclude_host_suffixes: %q must start with a dot", s)
		}
	}

	ft, err := rulefile.Map(top["feature_table"], "feature_table",
		"header_tags", "input_labels", "required_markers", "output_labels", "stated_inputs", "stated_outputs")
	if err != nil {
		return nil, err
	}
	if r.headerTags, err = stringSet(ft["header_tags"], "feature_table.header_tags"); err != nil {
		return nil, err
	}
	if r.inputLabels, err = phraseSet(ft["input_labels"], "feature_table.input_labels"); err != nil {
		return nil, err
	}
	if r.requiredMarkers, err = phraseList(ft["required_markers"], "feature_table.required_markers"); err != nil {
		return nil, err
	}
	if r.outputLabels, err = phraseSet(ft["output_labels"], "feature_table.output_labels"); err != nil {
		return nil, err
	}
	if r.statedInputs, err = kindRules(ft["stated_inputs"], "feature_table.stated_inputs", observation.InputKinds); err != nil {
		return nil, err
	}
	if r.statedOutputs, err = kindRules(ft["stated_outputs"], "feature_table.stated_outputs", observation.OutputKinds); err != nil {
		return nil, err
	}

	lg, err := rulefile.Map(top["login_gates"], "login_gates", "credential_controls", "auth_action_prefixes")
	if err != nil {
		return nil, err
	}
	if r.credentialControls, err = stringSet(lg["credential_controls"], "login_gates.credential_controls"); err != nil {
		return nil, err
	}
	if r.authActionPrefixes, err = rulefile.Strings(lg["auth_action_prefixes"], "login_gates.auth_action_prefixes"); err != nil {
		return nil, err
	}
	for _, p := range r.authActionPrefixes {
		if !strings.HasPrefix(p, "/") || p != strings.ToLower(p) {
			return nil, fmt.Errorf("login_gates.auth_action_prefixes: %q must be a lower-case path prefix", p)
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
		{"offline_behavior", observation.OfflineBehaviors, &r.offline},
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

func kindRules(n rulefile.Node, where string, vocab map[string]bool) ([]wordRule, error) {
	items, err := rulefile.List(n, where)
	if err != nil {
		return nil, err
	}
	var out []wordRule
	seen := map[string]bool{}
	for _, item := range items {
		m, err := rulefile.Map(item, where, "kind", "phrases")
		if err != nil {
			return nil, err
		}
		kind, _ := rulefile.String(m["kind"], where)
		if !vocab[kind] || seen[kind] {
			return nil, fmt.Errorf("%s: kind %q is not allowed here or is repeated", where, kind)
		}
		seen[kind] = true
		phrases, err := phraseList(m["phrases"], where)
		if err != nil {
			return nil, err
		}
		out = append(out, wordRule{value: kind, phrases: phrases})
	}
	return out, nil
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
		phrases, err := phraseList(m["phrases"], where)
		if err != nil {
			return nil, err
		}
		out = append(out, wordRule{value: value, phrases: phrases})
	}
	return out, nil
}

func phraseList(n rulefile.Node, where string) ([]string, error) {
	phrases, err := rulefile.Strings(n, where)
	if err != nil {
		return nil, err
	}
	for _, p := range phrases {
		if p != normalise(p) {
			return nil, fmt.Errorf("%s: phrase %q must be lower-case with single spaces", where, p)
		}
	}
	return phrases, nil
}

func phraseSet(n rulefile.Node, where string) (map[string]bool, error) {
	list, err := phraseList(n, where)
	if err != nil {
		return nil, err
	}
	set := make(map[string]bool, len(list))
	for _, s := range list {
		set[s] = true
	}
	return set, nil
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
