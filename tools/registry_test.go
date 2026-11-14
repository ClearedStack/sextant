package tools

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/ClearedStack/sextant/internal/errcode"
	"github.com/ClearedStack/sextant/internal/observation"
	"github.com/ClearedStack/sextant/internal/subtool"
)

// TestRegistryAgreesWithTheRepository checks every registered sub-tool is
// named in component.json and has its three parts on disk.
func TestRegistryAgreesWithTheRepository(t *testing.T) {
	reg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("../component.json")
	if err != nil {
		t.Fatal(err)
	}
	var component struct {
		SubTools []string `json:"subTools"`
	}
	if err := json.Unmarshal(raw, &component); err != nil {
		t.Fatal(err)
	}
	named := map[string]bool{}
	for _, s := range component.SubTools {
		named[s] = true
	}
	if len(reg.Names()) != len(component.SubTools) {
		t.Fatalf("registered %d sub-tools (%v), component.json names %d (%v)",
			len(reg.Names()), reg.Names(), len(component.SubTools), component.SubTools)
	}
	for _, name := range reg.Names() {
		if !named[name] {
			t.Errorf("%s is not a sub-tool named in component.json", name)
		}
		for _, part := range []string{"match.yaml", "extract.go"} {
			if _, err := os.Stat(filepath.Join(name, part)); err != nil {
				t.Errorf("%s has no %s", name, part)
			}
		}
		if cases, _ := filepath.Glob(filepath.Join(name, "fixtures", "*", "expected.json")); len(cases) == 0 {
			t.Errorf("%s has no fixtures", name)
		}
	}
}

func TestRegistryFileIsChecked(t *testing.T) {
	bad := map[string]string{
		"unbuilt tool": "tools:\n  - name: unknown-subtool\n    folder: tools/unknown-subtool\n",
		"repeated":     "tools:\n  - name: gov-portal\n    folder: tools/gov-portal\n  - name: gov-portal\n    folder: tools/gov-portal\n",
		"wrong folder": "tools:\n  - name: gov-portal\n    folder: elsewhere\n",
		"extra key":    "tools:\n  - name: gov-portal\n    folder: tools/gov-portal\n    fallback: yes\n",
		"no tools":     "version: one\n",
	}
	for name, src := range bad {
		if _, err := load([]byte(src)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

type fake struct {
	rec *observation.Record
	err error
}

func (fake) Name() string                                          { return "fake" }
func (f fake) Observe(subtool.Target) (*observation.Record, error) { return f.rec, f.err }

// TestDispatchNeverChooses checks the registry's only three outcomes: one
// match is used, no match pauses the item, two matches pause the item.
func TestDispatchNeverChooses(t *testing.T) {
	u, _ := url.Parse("https://calc.example-agency.gov/")
	target := subtool.Target{ItemID: "PA-0001", URL: u, Page: []byte("<html></html>"), ObservedOn: "2026-01-15"}
	hit := fake{rec: &observation.Record{ItemID: "PA-0001"}}
	miss := fake{err: subtool.ErrNoMatch}
	broken := fake{err: errcode.New(errcode.Parse, errcode.TruncatedMarkup)}
	cases := []struct {
		name   string
		tools  []subtool.Tool
		reason errcode.Reason
	}{
		{"one match", []subtool.Tool{miss, hit, miss}, ""},
		{"no match", []subtool.Tool{miss, miss}, errcode.NoMatchingSubTool},
		{"no tools", nil, errcode.NoMatchingSubTool},
		{"two matches", []subtool.Tool{hit, hit}, errcode.AmbiguousSubTool},
		{"match and a broken match", []subtool.Tool{hit, broken}, errcode.AmbiguousSubTool},
		{"broken match", []subtool.Tool{miss, broken}, errcode.TruncatedMarkup},
	}
	for _, c := range cases {
		rec, err := (&Registry{tools: c.tools}).Observe(target)
		if c.reason == "" {
			if err != nil || rec == nil {
				t.Errorf("%s: got %v", c.name, err)
			}
			continue
		}
		e, ok := errcode.From(err)
		if !ok || e.Reason != c.reason || rec != nil {
			t.Errorf("%s: got record %v, error %v", c.name, rec, err)
		}
	}
}
