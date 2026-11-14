package derive_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/ClearedStack/sextant/internal/derive"
	"github.com/ClearedStack/sextant/internal/errcode"
	"github.com/ClearedStack/sextant/internal/observation"
)

// record returns a valid observation of a free, login-free calculator that
// cites one FAR Part 31 provision. Tests change one thing at a time.
func record() *observation.Record {
	return &observation.Record{
		ItemID:             "PA-0300",
		SourceURL:          "https://rates.example-tools.org/wrap",
		SubTool:            "saas-marketing",
		ObservedOn:         "2026-01-15",
		CoreJob:            observation.Known("rate_calculation", observation.Stated),
		Inputs:             observation.Inputs{Fields: []observation.InputField{{Kind: "number", Required: true}}, Tier: observation.Stated},
		Outputs:            observation.Outputs{Fields: []observation.OutputField{{Kind: "computed_value"}}, Tier: observation.Stated},
		LoginRequirement:   observation.Known("not_required", observation.Stated),
		Tracking:           observation.Tracking{ExternalHosts: []string{}, Tier: observation.Seen},
		LicenseOrCopyright: observation.Known("open_license", observation.Stated),
		PriceModel:         observation.Known("free", observation.Stated),
		CitedPublicRules:   observation.CitedRules{Citations: []string{"FAR 31.203"}, Tier: observation.Seen},
		OfflineBehavior:    observation.Known("works_offline", observation.Stated),
	}
}

func factJSON(t *testing.T, r *derive.Result, name string) (tier string, value string) {
	t.Helper()
	f, ok := r.Facts[name]
	if !ok {
		t.Fatalf("fact %s is missing", name)
	}
	return f.Tier, string(f.Value)
}

func wantFact(t *testing.T, r *derive.Result, name, tier, value string) {
	t.Helper()
	gotTier, gotValue := factJSON(t, r, name)
	if gotTier != tier || gotValue != value {
		t.Errorf("%s = %s %s, want %s %s", name, gotTier, gotValue, tier, value)
	}
}

func derived(t *testing.T, rec *observation.Record) *derive.Result {
	t.Helper()
	res, err := derive.Default().Derive(rec)
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	if err := res.Validate(); err != nil {
		t.Fatalf("result does not validate: %v", err)
	}
	return res
}

func wantPause(t *testing.T, tables derive.Tables, rec *observation.Record, reason errcode.Reason) {
	t.Helper()
	res, err := tables.Derive(rec)
	var coded *errcode.Error
	if !errors.As(err, &coded) || coded.Code != errcode.NeedsSubtool || coded.Reason != reason {
		t.Fatalf("got (%v, %v), want E_NEEDS_SUBTOOL %s", res, err, reason)
	}
	if res != nil {
		t.Fatalf("a paused item must carry no result, got %+v", res)
	}
}

func TestRuleFactsComeFromTheCitationTable(t *testing.T) {
	res := derived(t, record())
	if res.Category != "pricing" || res.SubTool != "saas-marketing" || res.ObservedOn != "2026-01-15" {
		t.Errorf("header = %q %q %q", res.Category, res.SubTool, res.ObservedOn)
	}
	wantFact(t, res, "source_kind", "Stated", `"far"`)
	wantFact(t, res, "source_public", "Stated", `true`)
	wantFact(t, res, "source_precision", "Stated", `"defined_procedure"`)
	wantFact(t, res, "worked_examples", "Stated", `0`)
	wantFact(t, res, "rate_tables", "Stated", `0`)
	for _, name := range []string{"reference_updates_per_year", "roadmap_phase_fit"} {
		if tier, _ := factJSON(t, res, name); tier != "Stated" {
			t.Errorf("%s tier = %s, want Stated", name, tier)
		}
	}
}

func TestEveryDefaultCitationRowYieldsAValidResult(t *testing.T) {
	tables := derive.Default()
	if len(tables.Citations) == 0 {
		t.Fatal("the default citation table is empty")
	}
	for cite := range tables.Citations {
		rec := record()
		rec.CitedPublicRules.Citations = []string{cite}
		if res, err := tables.Derive(rec); err != nil {
			t.Errorf("%s: %v", cite, err)
		} else if err := res.Validate(); err != nil {
			t.Errorf("%s: %v", cite, err)
		}
	}
}

// TestBuildFactsComeFromTheTemplateTable covers all five calculator jobs.
// Sextant's vocabulary can only observe rate_calculation and
// amount_calculation today, so each row is also tried as the row for an
// observed rate_calculation.
func TestBuildFactsComeFromTheTemplateTable(t *testing.T) {
	for _, job := range []string{"rate_calculation", "amount_calculation", "cost_allocation", "price_escalation", "interest_calculation"} {
		t.Run(job, func(t *testing.T) {
			tables := derive.Default()
			row, ok := tables.Templates[job]
			if !ok {
				t.Fatalf("no template row for %s", job)
			}
			tables.Templates["rate_calculation"] = row
			res, err := tables.Derive(record())
			if err != nil {
				t.Fatal(err)
			}
			checkBuild(t, res)
		})
	}
	for _, job := range []string{"rate_calculation", "amount_calculation"} {
		rec := record()
		rec.CoreJob = observation.Known(job, observation.Stated)
		wantFact(t, derived(t, rec), "core_job", "Stated", `"`+job+`"`)
	}
}

func checkBuild(t *testing.T, res *derive.Result) {
	t.Helper()
	wantFact(t, res, "client_runtime", "Designed", `"browser_js"`)
	wantFact(t, res, "needs_backend", "Designed", `false`)
	wantFact(t, res, "needs_remote_api", "Designed", `false`)
	wantFact(t, res, "works_offline", "Designed", `true`)
	wantFact(t, res, "needs_account", "Designed", `false`)
	wantFact(t, res, "emits_tracking", "Designed", `false`)
	wantFact(t, res, "connect_src_self", "Designed", `true`)
	wantFact(t, res, "task_count", "Designed", `1`)
	wantFact(t, res, "output_kinds", "Designed", `["citation_lookup","computed_number"]`)
	for _, name := range []string{"largest_input_kb", "est_source_lines"} {
		if tier, _ := factJSON(t, res, name); tier != "Designed" {
			t.Errorf("%s tier = %s, want Designed", name, tier)
		}
	}
}

func TestBuildFactsDescribeTheToolNotThePage(t *testing.T) {
	rec := record()
	rec.OfflineBehavior = observation.Known("server_round_trip", observation.Stated)
	rec.Tracking = observation.Tracking{ExternalHosts: []string{"t.example.net"}, Tier: observation.Seen}
	res := derived(t, rec)
	wantFact(t, res, "works_offline", "Designed", `true`)
	wantFact(t, res, "emits_tracking", "Designed", `false`)
}

func TestObservedFacts(t *testing.T) {
	tests := []struct {
		name   string
		edit   func(*observation.Record)
		wantBA string
	}{
		{"paid", func(r *observation.Record) { r.PriceModel = observation.Known("paid", observation.Stated) }, "paid_only"},
		{"free with login", func(r *observation.Record) {
			r.LoginRequirement = observation.Known("required", observation.Stated)
		}, "partial"},
		{"free with tracking", func(r *observation.Record) {
			r.Tracking = observation.Tracking{ExternalHosts: []string{"t.example.net"}, Tier: observation.Seen}
		}, "partial"},
		{"free, tracking unknown, login required", func(r *observation.Record) {
			r.LoginRequirement = observation.Known("required", observation.Stated)
			r.Tracking = observation.Tracking{ExternalHosts: []string{}, Tier: observation.Unknown}
		}, "partial"},
		{"free, no login, no tracking", func(r *observation.Record) {}, "complete"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := record()
			tc.edit(rec)
			res := derived(t, rec)
			wantFact(t, res, "best_alternative", "Seen", `"`+tc.wantBA+`"`)
			wantFact(t, res, "need_signals", "Seen", `1`)
			wantFact(t, res, "community_demand", "Seen", `1`)
			wantFact(t, res, "core_job", "Stated", `"rate_calculation"`)
		})
	}
}

func TestCoreJobKeepsItsObservedTier(t *testing.T) {
	rec := record()
	rec.CoreJob = observation.Known("rate_calculation", observation.Seen)
	wantFact(t, derived(t, rec), "core_job", "Seen", `"rate_calculation"`)
}

func TestPauses(t *testing.T) {
	two := derive.Default()
	two.Citations["FAR 31.201-2"] = derive.CitationRow{Category: "proposal", SourceKind: "far", SourcePublic: true, SourcePrecision: "general_principle"}
	disagree := derive.Default()
	row := disagree.Citations["FAR 31.203"]
	row.RateTables = 3
	disagree.Citations["FAR 31.201-2"] = row
	other := derive.Default()
	other.Citations["FAR 31.203"] = derive.CitationRow{Category: "proposal", SourceKind: "far", SourcePublic: true, SourcePrecision: "general_principle"}

	tests := []struct {
		name   string
		tables derive.Tables
		edit   func(*observation.Record)
		reason errcode.Reason
	}{
		{"no citations", derive.Default(), func(r *observation.Record) {
			r.CitedPublicRules = observation.CitedRules{Citations: []string{}, Tier: observation.Unknown}
		}, derive.NoCitationRow},
		{"citation with no row", derive.Default(), func(r *observation.Record) {
			r.CitedPublicRules.Citations = []string{"FAR 52.212-1"}
		}, derive.NoCitationRow},
		{"rows of different categories", two, func(r *observation.Record) {
			r.CitedPublicRules.Citations = []string{"FAR 31.201-2", "FAR 31.203"}
		}, derive.CitationRowsDisagree},
		{"rows of one category with different values", disagree, func(r *observation.Record) {
			r.CitedPublicRules.Citations = []string{"FAR 31.201-2", "FAR 31.203"}
		}, derive.CitationRowsDisagree},
		{"category with no tables", other, func(r *observation.Record) {}, derive.CategoryWithoutTables},
		{"core job unknown", derive.Default(), func(r *observation.Record) { r.CoreJob = observation.NotKnown() }, derive.CoreJobNotShown},
		{"core job with no template", derive.Default(), func(r *observation.Record) {
			r.CoreJob = observation.Known("eligibility_check", observation.Stated)
		}, derive.NoTemplateForCoreJob},
		{"core job outside the category", derive.Default(), func(r *observation.Record) {
			r.CoreJob = observation.Known("control_mapping", observation.Stated)
		}, derive.NoTemplateForCoreJob},
		{"price model not shown", derive.Default(), func(r *observation.Record) { r.PriceModel = observation.NotKnown() }, derive.PriceModelNotShown},
		{"login not shown", derive.Default(), func(r *observation.Record) { r.LoginRequirement = observation.NotKnown() }, derive.AlternativeUndecidable},
		{"tracking not shown", derive.Default(), func(r *observation.Record) {
			r.Tracking = observation.Tracking{ExternalHosts: []string{}, Tier: observation.Unknown}
		}, derive.AlternativeUndecidable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := record()
			tc.edit(rec)
			wantPause(t, tc.tables, rec, tc.reason)
		})
	}
}

func TestSameCitationTwiceInTwoRowsOfEqualValuesIsNotAmbiguous(t *testing.T) {
	tables := derive.Default()
	tables.Citations["FAR 31.201-2"] = tables.Citations["FAR 31.203"]
	rec := record()
	rec.CitedPublicRules.Citations = []string{"FAR 31.201-2", "FAR 31.203"}
	if _, err := tables.Derive(rec); err != nil {
		t.Fatal(err)
	}
}

func TestBrokenRecordIsAParseError(t *testing.T) {
	rec := record()
	rec.CoreJob = observation.Known("copied vendor phrase", observation.Seen)
	_, err := derive.Default().Derive(rec)
	if coded, ok := errcode.From(err); !ok || coded.Code != errcode.Parse {
		t.Fatalf("got %v, want E_PARSE", err)
	}
	if _, err := derive.Default().Derive(nil); err == nil {
		t.Fatal("nil record must fail")
	}
}

func TestADamagedTableCannotLetAFreeTextValueOut(t *testing.T) {
	tables := derive.Default()
	row := tables.Citations["FAR 31.203"]
	row.SourceKind = "whatever the page said"
	tables.Citations["FAR 31.203"] = row
	if _, err := tables.Derive(record()); err == nil {
		t.Fatal("a value outside the closed vocabulary must not be emitted")
	}
}

func TestSameInputSameBytes(t *testing.T) {
	a, _ := json.Marshal(derived(t, record()))
	b, _ := json.Marshal(derived(t, record()))
	if string(a) != string(b) {
		t.Fatal("two runs differ")
	}
}

func TestValidateRefusesWhatAssayWouldRefuse(t *testing.T) {
	good := func() *derive.Result { return derived(t, record()) }
	tests := []struct {
		name string
		edit func(*derive.Result)
	}{
		{"wrong category", func(r *derive.Result) { r.Category = "market" }},
		{"missing fact", func(r *derive.Result) { delete(r.Facts, "rate_tables") }},
		{"extra fact", func(r *derive.Result) { r.Facts["note"] = r.Facts["rate_tables"] }},
		{"unknown tier", func(r *derive.Result) { f := r.Facts["rate_tables"]; f.Tier = "Guessed"; r.Facts["rate_tables"] = f }},
		{"null value", func(r *derive.Result) {
			f := r.Facts["rate_tables"]
			f.Value = json.RawMessage("null")
			r.Facts["rate_tables"] = f
		}},
		{"int out of range", func(r *derive.Result) {
			f := r.Facts["roadmap_phase_fit"]
			f.Value = json.RawMessage("4")
			r.Facts["roadmap_phase_fit"] = f
		}},
		{"fraction", func(r *derive.Result) {
			f := r.Facts["rate_tables"]
			f.Value = json.RawMessage("1.5")
			r.Facts["rate_tables"] = f
		}},
		{"enum outside vocabulary", func(r *derive.Result) {
			f := r.Facts["core_job"]
			f.Value = json.RawMessage(`"free text"`)
			r.Facts["core_job"] = f
		}},
		{"bool as string", func(r *derive.Result) {
			f := r.Facts["needs_backend"]
			f.Value = json.RawMessage(`"false"`)
			r.Facts["needs_backend"] = f
		}},
		{"list element outside vocabulary", func(r *derive.Result) {
			f := r.Facts["output_kinds"]
			f.Value = json.RawMessage(`["x"]`)
			r.Facts["output_kinds"] = f
		}},
		{"bad sub_tool", func(r *derive.Result) { r.SubTool = "Sub Tool" }},
		{"bad date", func(r *derive.Result) { r.ObservedOn = "2026-02-30" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := good()
			tc.edit(r)
			if err := r.Validate(); err == nil {
				t.Fatal("Validate accepted it")
			}
		})
	}
}

func TestResultShapeIsExactlyFourKeys(t *testing.T) {
	raw, err := json.Marshal(derived(t, record()))
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for k := range top {
		keys = append(keys, k)
	}
	if len(keys) != 4 || strings.Join(sorted(keys), ",") != "category,facts,observed_on,sub_tool" {
		t.Fatalf("keys = %v", keys)
	}
	var env map[string]map[string]json.RawMessage
	if err := json.Unmarshal(top["facts"], &env); err != nil {
		t.Fatal(err)
	}
	for name, e := range env {
		if len(e) != 2 || e["tier"] == nil || e["value"] == nil {
			t.Errorf("%s envelope has keys other than tier and value", name)
		}
	}
	var back derive.Result
	if err := json.Unmarshal(raw, &back); err != nil || back.Validate() != nil {
		t.Fatalf("round trip: %v", err)
	}
	if !reflect.DeepEqual(derived(t, record()).Facts, back.Facts) {
		t.Fatal("round trip changed the facts")
	}
}

func sorted(s []string) []string {
	for i := range s {
		for j := i + 1; j < len(s); j++ {
			if s[j] < s[i] {
				s[i], s[j] = s[j], s[i]
			}
		}
	}
	return s
}
