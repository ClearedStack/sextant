package browserjob

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

func TestEveryRuleValueHasASavedExample(t *testing.T) {
	r := loaded(t)
	seen := map[string]bool{}
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
		for name, f := range map[string]observation.Fact{
			"core_job": rec.CoreJob,
			"login":    rec.LoginRequirement,
			"license":  rec.LicenseOrCopyright,
			"price":    rec.PriceModel,
			"offline":  rec.OfflineBehavior,
		} {
			if f.Value != nil {
				seen[name+"="+*f.Value] = true
			}
		}
		for _, in := range rec.Inputs.Fields {
			seen["kind="+in.Kind] = true
		}
	}
	lists := map[string][]wordRule{
		"core_job": r.coreJob,
		"login":    r.login,
		"license":  r.license,
		"price":    r.price,
	}
	for name, list := range lists {
		for _, rule := range list {
			if !seen[name+"="+rule.value] {
				t.Errorf("no fixture expects %s = %s", name, rule.value)
			}
		}
	}
	for _, kind := range r.kinds {
		if !seen["kind="+kind] {
			t.Errorf("no fixture expects input kind %s", kind)
		}
	}
	for _, off := range []string{"works_offline", "server_round_trip"} {
		if !seen["offline="+off] {
			t.Errorf("no fixture expects offline_behavior = %s", off)
		}
	}
}

func TestNeedsBrowserJob(t *testing.T) {
	shell := []byte(`<!DOCTYPE html><html><head><title>App</title></head><body><div id="app"></div><script src="/bundle.js"></script></body></html>`)
	if !NeedsBrowserJob(shell) {
		t.Fatal("expected SPA shell to need browser job")
	}
	withForm := []byte(`<!DOCTYPE html><html><body><form><input name="x"></form><script src="/bundle.js"></script></body></html>`)
	if NeedsBrowserJob(withForm) {
		t.Fatal("static form page must not trigger browser job")
	}
	withTable := []byte(`<!DOCTYPE html><html><body><table><tr><td>a</td></tr></table><script src="/bundle.js"></script></body></html>`)
	if NeedsBrowserJob(withTable) {
		t.Fatal("feature table page must not trigger browser job")
	}
}

func TestRuleFileCannotLeaveTheVocabulary(t *testing.T) {
	good := string(matchYAML)
	edits := map[string][2]string{
		"free-text job":     {"- value: config_validation", "- value: checks sbom files"},
		"bad input kind":    {"- kind: number", "- kind: slider"},
		"bad output kind":   {"kind: computed_value", "kind: dom_node"},
		"wrong tool":        {"tool: on-demand-browser-job", "tool: gov-portal"},
		"lower-case method": {"    - POST", "    - post"},
		"path no slash":     {"    - /api/", "    - api/"},
		"unknown key":       {"citations:", "guess: yes\ncitations:"},
	}
	for name, e := range edits {
		if !strings.Contains(good, e[0]) {
			t.Fatalf("%s: text %q not in match.yaml", name, e[0])
		}
		if _, err := parseRules([]byte(strings.Replace(good, e[0], e[1], 1))); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
