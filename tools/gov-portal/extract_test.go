package govportal

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ClearedStack/sextant/internal/observation"
)

func loaded(t *testing.T) *rules {
	t.Helper()
	r, err := parseRules(matchYAML)
	if err != nil {
		t.Fatalf("match.yaml does not load: %v", err)
	}
	return r
}

// TestEveryRuleValueHasASavedExample checks that each value a word list can
// produce, each input kind and each citation pattern shows up in the expected
// output of at least one fixture. The exact-output check itself is the
// fixture test in cmd/sextant. This is coverage per value, not per phrase.
func TestEveryRuleValueHasASavedExample(t *testing.T) {
	r := loaded(t)
	seen := map[string]bool{}
	var pages []string
	dirs, _ := filepath.Glob("fixtures/observed-*")
	if len(dirs) == 0 {
		t.Fatal("no observed-* fixtures")
	}
	for _, dir := range dirs {
		raw, err := os.ReadFile(filepath.Join(dir, "expected.json"))
		if err != nil {
			t.Fatal(err)
		}
		var rec observation.Record
		if err := json.Unmarshal(raw, &rec); err != nil {
			t.Fatalf("%s: %v", dir, err)
		}
		if err := rec.Validate(); err != nil {
			t.Errorf("%s: expected.json is not a valid record: %v", dir, err)
		}
		for name, f := range map[string]observation.Fact{"core_job": rec.CoreJob, "login": rec.LoginRequirement,
			"license": rec.LicenseOrCopyright, "price": rec.PriceModel, "offline": rec.OfflineBehavior} {
			if f.Value != nil {
				seen[name+"="+*f.Value] = true
			}
		}
		for _, in := range rec.Inputs.Fields {
			seen["kind="+in.Kind] = true
		}
		pages = append(pages, strings.Join(rec.CitedPublicRules.Citations, "\n"))
	}
	lists := map[string][]wordRule{"core_job": r.coreJob, "login": r.login, "license": r.license,
		"price": r.price, "offline": r.offline}
	for name, list := range lists {
		for _, rule := range list {
			if !seen[name+"="+rule.value] {
				t.Errorf("no fixture expects %s = %s", name, rule.value)
			}
		}
	}
	for _, kind := range r.kinds {
		if !seen["kind="+kind] {
			t.Errorf("no fixture expects an input of kind %s", kind)
		}
	}
	cited := strings.Join(pages, "\n")
	for _, c := range r.citations {
		prefix := strings.TrimSuffix(c.write, "$1")
		found := false
		for _, line := range strings.Split(cited, "\n") {
			rest, ok := strings.CutPrefix(line, prefix)
			// "FAR " is also a prefix of "FAR Part 15"; tell the two apart.
			if ok && rest != "" && !strings.Contains(rest, " ") {
				found = true
			}
		}
		if !found {
			t.Errorf("no fixture expects a citation written as %q", c.write)
		}
	}
}

func TestCitations(t *testing.T) {
	r := loaded(t)
	text := "Under FAR 52.204-21, FAR clause 15.404-1, FAR Part 9, DFARS 252.204-7012, DFARS Clause 252.225-7001, " +
		"NIST SP 800-171 and NIST Special Publication 800-53A. Not citations: the FAR, DFARS 52.204-21, " +
		"AFAR 52.204-21, FAR52.204-21, NIST SP 900-1, FAR 1234.5."
	got := strings.Join(r.cite(text).Citations, " | ")
	want := "DFARS 252.204-7012 | DFARS 252.225-7001 | FAR 15.404-1 | FAR 52.204-21 | FAR Part 9 | NIST SP 800-171 | NIST SP 800-53A"
	if got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

// TestRuleFileCannotLeaveTheVocabulary shows that editing match.yaml cannot
// make the extractor emit a value the record schema does not define.
func TestRuleFileCannotLeaveTheVocabulary(t *testing.T) {
	good := string(matchYAML)
	edits := map[string][2]string{
		"free-text job":     {"- value: date_calculation", "- value: works out due dates"},
		"new input kind":    {"- kind: number", "- kind: money"},
		"wrong tool":        {"tool: gov-portal", "tool: saas-marketing"},
		"upper-case phrase": {"- due date calculator", "- Due Date Calculator"},
		"control twice":     {"        - input:tel", "        - input:hidden"},
		"suffix no dot":     {"    - .mil", "    - mil"},
		"bad pattern":       {`pattern: '\bFAR\s+Part\s+(\d{1,2})\b'`, `pattern: '\bFAR\s+Part\s+(\d{1,2}'`},
		"pattern no group":  {`pattern: '\bFAR\s+Part\s+(\d{1,2})\b'`, `pattern: '\bFAR\s+Part\s+\d{1,2}\b'`},
		"unknown key":       {"citations:", "fallback: guess\ncitations:"},
	}
	for name, e := range edits {
		if !strings.Contains(good, e[0]) {
			t.Fatalf("%s: the text to edit is not in match.yaml", name)
		}
		if _, err := parseRules([]byte(strings.Replace(good, e[0], e[1], 1))); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
