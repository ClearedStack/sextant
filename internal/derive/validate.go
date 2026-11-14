package derive

import (
	"encoding/json"
	"errors"
	"regexp"
	"slices"

	"github.com/ClearedStack/sextant/internal/observation"
)

// Pricing is the one category with tables so far.
const Pricing = "pricing"

// spec is the shape of one declared fact: a bool, an int in a range, one of
// a closed list of words, or a list of such words.
type spec struct {
	kind     string // "bool", "int", "enum" or "list"
	values   []string
	min, max int
}

func enum(v ...string) spec { return spec{kind: "enum", values: v} }
func list(v ...string) spec { return spec{kind: "list", values: v} }
func num(min, max int) spec { return spec{kind: "int", min: min, max: max} }
func flag() spec            { return spec{kind: "bool"} }
func (s spec) ok(raw json.RawMessage) bool {
	if string(raw) == "null" {
		return false
	}
	switch s.kind {
	case "bool":
		var b bool
		return json.Unmarshal(raw, &b) == nil
	case "int":
		var n int
		return json.Unmarshal(raw, &n) == nil && n >= s.min && n <= s.max
	case "enum":
		var v string
		return json.Unmarshal(raw, &v) == nil && slices.Contains(s.values, v)
	case "list":
		var l []string
		if json.Unmarshal(raw, &l) != nil || len(l) == 0 {
			return false
		}
		for _, v := range l {
			if !slices.Contains(s.values, v) {
				return false
			}
		}
		return true
	}
	return false
}

// factSpecs holds, per category, the facts its Assay profile declares and the
// values it allows. The pricing entry is copied from the facts: section of
// Assay's tools/rubric-profiles/pricing/profile.yaml (version 3);
// testdata/pricing-profile-facts.json is the same section, and a test checks
// the output against that file, not against this map.
var factSpecs = map[string]map[string]spec{
	Pricing: {
		"source_kind":                enum("statute", "far", "dfars", "nist_sp", "agency_publication", "vendor_document", "paywalled_standard", "unpublished"),
		"source_public":              flag(),
		"source_precision":           enum("exact_formula", "lookup_table", "defined_procedure", "general_principle"),
		"client_runtime":             enum("browser_js", "browser_wasm", "desktop_native", "server_side"),
		"needs_backend":              flag(),
		"needs_remote_api":           flag(),
		"largest_input_kb":           num(0, 10000000),
		"works_offline":              flag(),
		"needs_account":              flag(),
		"emits_tracking":             flag(),
		"connect_src_self":           flag(),
		"task_count":                 num(1, 100),
		"core_job":                   enum("rate_calculation", "amount_calculation", "cost_allocation", "price_escalation", "price_table_lookup", "interest_calculation", "date_calculation", "eligibility_check", "record_search", "multi_workflow", "portal_suite", "open_ended_advisory"),
		"output_kinds":               list("computed_number", "structured_data", "citation_lookup", "legal_advice", "compliance_determination"),
		"need_signals":               num(0, 100000),
		"best_alternative":           enum("none", "paid_only", "outdated", "partial", "complete"),
		"est_source_lines":           num(1, 1000000),
		"worked_examples":            num(0, 10000),
		"reference_updates_per_year": num(0, 1000),
		"rate_tables":                num(0, 1000),
		"roadmap_phase_fit":          num(0, 3),
		"community_demand":           num(0, 100000),
	},
}

var subToolPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// Validate checks a result the way Assay will: the category has a profile,
// the sub-tool is a slug, the date is a date, and the facts are exactly the
// profile's, each with a known tier and a value of the declared shape. It is
// the clean-room check: no value that is not a closed word or a number gets
// through.
func (r *Result) Validate() error {
	specs, ok := factSpecs[r.Category]
	if !ok {
		return errors.New("category has no fact tables")
	}
	if len(r.SubTool) > 64 || !subToolPattern.MatchString(r.SubTool) {
		return errors.New("sub_tool is not a slug")
	}
	if !observation.ValidDate(r.ObservedOn) {
		return errors.New("observed_on is not YYYY-MM-DD")
	}
	if len(r.Facts) != len(specs) {
		return errors.New("facts are not exactly the profile's")
	}
	for name, s := range specs {
		f, ok := r.Facts[name]
		if !ok {
			return errors.New("a declared fact is missing")
		}
		if f.Tier != Seen && f.Tier != Stated && f.Tier != Designed {
			return errors.New("a fact has a tier it cannot have")
		}
		if !s.ok(f.Value) {
			return errors.New("a fact holds a value outside its declared shape")
		}
	}
	return nil
}
