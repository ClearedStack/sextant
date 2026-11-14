package agencypdf

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
		if rec.PublicationDate == nil {
			t.Errorf("%s: agency-pdf record missing publication_date", dir)
		} else if rec.PublicationDate.Value != nil {
			seen["pub_tier="+string(rec.PublicationDate.Tier)] = true
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
		"offline":  r.offline,
	}
	for name, list := range lists {
		for _, rule := range list {
			if !seen[name+"="+rule.value] {
				t.Errorf("no fixture expects %s = %s", name, rule.value)
			}
		}
	}
	for _, kind := range r.fieldKinds {
		if !seen["kind="+kind] {
			t.Errorf("no fixture expects AcroForm input kind %s", kind)
		}
	}
	for _, tier := range []string{"Seen", "Stated"} {
		if !seen["pub_tier="+tier] {
			t.Errorf("no fixture expects publication_date tier %s", tier)
		}
	}
}

func TestRuleFileCannotLeaveTheVocabulary(t *testing.T) {
	good := string(matchYAML)
	edits := map[string][2]string{
		"free-text job":     {"- value: amount_calculation", "- value: reads procurement pdfs"},
		"bad input kind":    {"kind: text", "kind: signature"},
		"bad output kind":   {"readonly_output_kind: computed_value", "readonly_output_kind: raw_pdf"},
		"wrong tool":        {"tool: agency-pdf", "tool: gov-portal"},
		"bad pdf prefix":    {"- '%PDF-1.'", "- 'PDF-1.'"},
		"upper-case phrase": {"- price evaluation worksheet", "- Price Evaluation Worksheet"},
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
