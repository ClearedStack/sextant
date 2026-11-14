package agencypdf

import (
	_ "embed"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/ClearedStack/sextant/internal/observation"
	"github.com/ClearedStack/sextant/internal/rulefile"
)

//go:embed match.yaml
var matchYAML []byte

// Name is this sub-tool's registry name.
const Name = "agency-pdf"

type wordRule struct {
	value   string
	phrases []string
}

type citationRule struct {
	pattern *regexp.Regexp
	write   string
}

type rules struct {
	schemes      map[string]bool
	hostSuffixes []string
	pdfPrefixes  []string

	ignoredButtonFlag  int
	readonlyFlag       int
	requiredFlag       int
	readonlyOutputKind string
	fieldKinds         map[string]string

	dateInfoKeys []string
	datePatterns []*regexp.Regexp

	coreJob, login, license, price, offline []wordRule
	citations                               []citationRule
}

func parseRules(src []byte) (*rules, error) {
	root, err := rulefile.Parse(src)
	if err != nil {
		return nil, err
	}
	top, err := rulefile.Map(root, "match.yaml", "tool", "match", "acroform", "publication_dates",
		"core_job", "login_requirement", "license_or_copyright", "price_model", "offline_behavior", "citations")
	if err != nil {
		return nil, err
	}
	if name, _ := rulefile.String(top["tool"], "tool"); name != Name {
		return nil, fmt.Errorf("tool: expected %q", Name)
	}
	r := &rules{fieldKinds: map[string]string{}}

	m, err := rulefile.Map(top["match"], "match", "schemes", "host_suffixes", "pdf_prefixes")
	if err != nil {
		return nil, err
	}
	if r.schemes, err = stringSet(m["schemes"], "match.schemes"); err != nil {
		return nil, err
	}
	if r.hostSuffixes, err = rulefile.Strings(m["host_suffixes"], "match.host_suffixes"); err != nil {
		return nil, err
	}
	for _, s := range r.hostSuffixes {
		if len(s) < 2 || s[0] != '.' {
			return nil, fmt.Errorf("match.host_suffixes: %q must start with a dot", s)
		}
	}
	if r.pdfPrefixes, err = rulefile.Strings(m["pdf_prefixes"], "match.pdf_prefixes"); err != nil {
		return nil, err
	}
	for _, p := range r.pdfPrefixes {
		if !strings.HasPrefix(p, "%PDF-") {
			return nil, fmt.Errorf("match.pdf_prefixes: %q must start with %%PDF-", p)
		}
	}

	af, err := rulefile.Map(top["acroform"], "acroform",
		"ignored_button_flag", "readonly_flag", "required_flag", "readonly_output_kind", "field_kinds")
	if err != nil {
		return nil, err
	}
	if r.ignoredButtonFlag, err = posInt(af["ignored_button_flag"], "acroform.ignored_button_flag"); err != nil {
		return nil, err
	}
	if r.readonlyFlag, err = posInt(af["readonly_flag"], "acroform.readonly_flag"); err != nil {
		return nil, err
	}
	if r.requiredFlag, err = posInt(af["required_flag"], "acroform.required_flag"); err != nil {
		return nil, err
	}
	if r.readonlyOutputKind, err = rulefile.String(af["readonly_output_kind"], "acroform.readonly_output_kind"); err != nil {
		return nil, err
	}
	if !observation.OutputKinds[r.readonlyOutputKind] {
		return nil, fmt.Errorf("acroform.readonly_output_kind: %q is outside OutputKinds", r.readonlyOutputKind)
	}
	fkList, err := rulefile.List(af["field_kinds"], "acroform.field_kinds")
	if err != nil {
		return nil, err
	}
	for _, item := range fkList {
		fm, err := rulefile.Map(item, "acroform.field_kinds", "ft", "kind")
		if err != nil {
			return nil, err
		}
		ft, _ := rulefile.String(fm["ft"], "acroform.field_kinds.ft")
		kind, _ := rulefile.String(fm["kind"], "acroform.field_kinds.kind")
		if ft != strings.ToLower(ft) || r.fieldKinds[ft] != "" {
			return nil, fmt.Errorf("acroform.field_kinds: %q must be lower-case and unique", ft)
		}
		if !observation.InputKinds[kind] {
			return nil, fmt.Errorf("acroform.field_kinds: %q is not an input kind", kind)
		}
		r.fieldKinds[ft] = kind
	}

	pd, err := rulefile.Map(top["publication_dates"], "publication_dates", "info_keys", "text_patterns")
	if err != nil {
		return nil, err
	}
	if r.dateInfoKeys, err = rulefile.Strings(pd["info_keys"], "publication_dates.info_keys"); err != nil {
		return nil, err
	}
	tp, err := rulefile.Strings(pd["text_patterns"], "publication_dates.text_patterns")
	if err != nil {
		return nil, err
	}
	for _, pat := range tp {
		re, err := regexp.Compile(pat)
		if err != nil || re.NumSubexp() != 1 {
			return nil, fmt.Errorf("publication_dates.text_patterns: %q needs one capture group", pat)
		}
		r.datePatterns = append(r.datePatterns, re)
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

func posInt(n rulefile.Node, where string) (int, error) {
	s, err := rulefile.String(n, where)
	if err != nil {
		return 0, err
	}
	v, err := strconv.Atoi(s)
	if err != nil || v <= 0 {
		return 0, fmt.Errorf("%s: expected positive integer", where)
	}
	return v, nil
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
