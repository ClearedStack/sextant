package derive_test

import (
	"bytes"
	"encoding/json"
	"flag"
	"net/url"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/ClearedStack/sextant/internal/subtool"
	"github.com/ClearedStack/sextant/tools"
)

var update = flag.Bool("update", false, "rewrite the golden results.sextant")

// profile is testdata/pricing-profile-facts.json: the facts: section of
// Assay's pricing profile, copied unchanged (see its "source" field).
type profile struct {
	Facts map[string]struct {
		Type   string   `json:"type"`
		Values []string `json:"values"`
		Min    *int     `json:"min"`
		Max    *int     `json:"max"`
	} `json:"facts"`
}

func loadProfile(t *testing.T) profile {
	t.Helper()
	raw, err := os.ReadFile("testdata/pricing-profile-facts.json")
	if err != nil {
		t.Fatal(err)
	}
	var p profile
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	if len(p.Facts) != 22 {
		t.Fatalf("the profile fixture holds %d facts, want 22", len(p.Facts))
	}
	return p
}

// wrapRateResult runs the invented wrap-rate page through the real registry
// and the derivation, as the worker does.
func wrapRateResult(t *testing.T) []byte {
	t.Helper()
	page, err := os.ReadFile("testdata/wrap-rate/page.html")
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse("https://rates.example-tools.org/wrap")
	if err != nil {
		t.Fatal(err)
	}
	reg, err := tools.Load()
	if err != nil {
		t.Fatal(err)
	}
	rec, err := reg.Observe(subtool.Target{ItemID: "PA-0300", URL: u, Page: page, ObservedOn: "2026-01-15"})
	if err != nil {
		t.Fatalf("observe: %v", err)
	}
	res, err := derivedDefault(rec)
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	out, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(out, '\n')
}

func TestGoldenWrapRatePage(t *testing.T) {
	got := wrapRateResult(t)
	const golden = "testdata/wrap-rate/results.sextant.json"
	if *update {
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("output differs from %s\n--- got\n%s--- want\n%s", golden, got, want)
	}
}

// TestGoldenHoldsExactlyTheProfileFacts checks every fact of Assay's pricing
// profile is present, with a value of the right type, inside the declared
// enum or range, and nothing else is.
func TestGoldenHoldsExactlyTheProfileFacts(t *testing.T) {
	p := loadProfile(t)
	var res struct {
		Category string `json:"category"`
		Facts    map[string]struct {
			Tier  string          `json:"tier"`
			Value json.RawMessage `json:"value"`
		} `json:"facts"`
	}
	if err := json.Unmarshal(wrapRateResult(t), &res); err != nil {
		t.Fatal(err)
	}
	if res.Category != "pricing" {
		t.Errorf("category = %q", res.Category)
	}
	var have, want []string
	for n := range res.Facts {
		have = append(have, n)
	}
	for n := range p.Facts {
		want = append(want, n)
	}
	sort.Strings(have)
	sort.Strings(want)
	if strings.Join(have, ",") != strings.Join(want, ",") {
		t.Fatalf("facts differ\n have %v\n want %v", have, want)
	}
	for name, f := range res.Facts {
		if f.Tier != "Seen" && f.Tier != "Stated" && f.Tier != "Designed" {
			t.Errorf("%s: tier %q", name, f.Tier)
		}
		s := p.Facts[name]
		switch s.Type {
		case "bool":
			var b bool
			if json.Unmarshal(f.Value, &b) != nil || string(f.Value) == "null" {
				t.Errorf("%s: %s is not a bool", name, f.Value)
			}
		case "int":
			var n int
			if json.Unmarshal(f.Value, &n) != nil || n < *s.Min || n > *s.Max {
				t.Errorf("%s: %s is not an int in range", name, f.Value)
			}
		case "enum":
			var v string
			if json.Unmarshal(f.Value, &v) != nil || !contains(s.Values, v) {
				t.Errorf("%s: %s is not in the enum", name, f.Value)
			}
		case "list":
			var l []string
			if json.Unmarshal(f.Value, &l) != nil || len(l) == 0 {
				t.Errorf("%s: %s is not a list", name, f.Value)
			}
			for _, v := range l {
				if !contains(s.Values, v) {
					t.Errorf("%s: %q is not allowed", name, v)
				}
			}
		default:
			t.Errorf("%s: unknown type %q", name, s.Type)
		}
	}
}

func TestGoldenCopiesNothingFromThePage(t *testing.T) {
	out := string(wrapRateResult(t))
	for _, leak := range []string{"zzcanary", "Wrap rate", "signup", "rates.example-tools.org", "MIT"} {
		if strings.Contains(out, leak) {
			t.Errorf("result contains page or address text %q", leak)
		}
	}
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}
