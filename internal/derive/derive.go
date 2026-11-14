// Package derive turns the observation record, which says what a public page
// shows, into what Assay scores: a category and the facts of that category's
// profile. It is deterministic, reads no clock and copies nothing from the
// page: every value comes from a table in this package or is a closed word
// or a number computed from closed words.
//
// Three groups of facts are derived:
//
//   - rule facts, from the citation table, keyed by the normalised citations
//     Sextant extracts (tier Stated);
//   - build facts, from the template table, keyed by the observed core job.
//     They describe the tool ClearedStack would build, not the page
//     (tier Designed);
//   - observed facts, from the page itself (tier Seen, or as observed).
//
// Whatever cannot be derived pauses the item with E_NEEDS_SUBTOOL. Nothing
// is defaulted or guessed.
package derive

import (
	"encoding/json"

	"github.com/ClearedStack/sextant/internal/errcode"
	"github.com/ClearedStack/sextant/internal/observation"
)

// Tiers of a fact. Designed is the value the template guarantees for the
// tool to be built; Shakedown proves it later.
const (
	Seen     = "Seen"
	Stated   = "Stated"
	Designed = "Designed"
)

// Pause reasons. They live here because errcode is not this package's to
// change; each is a fixed word, as errcode.Reason requires.
const (
	NoCitationRow          errcode.Reason = "no_citation_row_matches"
	CitationRowsDisagree   errcode.Reason = "citation_rows_disagree"
	CategoryWithoutTables  errcode.Reason = "category_has_no_fact_tables"
	CoreJobNotShown        errcode.Reason = "core_job_not_shown"
	NoTemplateForCoreJob   errcode.Reason = "no_template_for_core_job"
	PriceModelNotShown     errcode.Reason = "price_model_not_shown"
	AlternativeUndecidable errcode.Reason = "best_alternative_not_decidable"
)

// Fact is one envelope as Assay reads it: exactly a tier and a value.
type Fact struct {
	Tier  string          `json:"tier"`
	Value json.RawMessage `json:"value"`
}

// Result is what Sextant writes under results.sextant, and nothing else.
type Result struct {
	Category   string          `json:"category"`
	SubTool    string          `json:"sub_tool"`
	ObservedOn string          `json:"observed_on"`
	Facts      map[string]Fact `json:"facts"`
}

// CitationRow is what one public rule says about the tools built on it.
type CitationRow struct {
	Category        string
	SourceKind      string
	SourcePublic    bool
	SourcePrecision string
	WorkedExamples  int
	UpdatesPerYear  int
	RateTables      int
	RoadmapPhaseFit int
}

// TemplateRow is what a Moldloft template guarantees about the tool it
// generates.
type TemplateRow struct {
	ClientRuntime  string
	NeedsBackend   bool
	NeedsRemoteAPI bool
	WorksOffline   bool
	NeedsAccount   bool
	EmitsTracking  bool
	ConnectSrcSelf bool
	OutputKinds    []string
	LargestInputKB int
	EstSourceLines int
}

// Tables are the two reviewed tables the derivation reads.
type Tables struct {
	Citations map[string]CitationRow // normalised citation -> row
	Templates map[string]TemplateRow // observed core_job -> template row
}

// Derive builds the result for one observation, or pauses the item.
func (t Tables) Derive(rec *observation.Record) (*Result, error) {
	if rec == nil || rec.Validate() != nil {
		return nil, errcode.New(errcode.Parse, errcode.RecordFailedSchema)
	}
	row, err := t.citationRow(rec.CitedPublicRules.Citations)
	if err != nil {
		return nil, err
	}
	if rec.CoreJob.Value == nil {
		return nil, errcode.New(errcode.NeedsSubtool, CoreJobNotShown)
	}
	tmpl, ok := t.Templates[*rec.CoreJob.Value]
	if !ok {
		return nil, errcode.New(errcode.NeedsSubtool, NoTemplateForCoreJob)
	}
	alt, err := bestAlternative(rec)
	if err != nil {
		return nil, err
	}

	f := map[string]Fact{}
	put := func(name, tier string, v any) {
		raw, _ := json.Marshal(v) // v is a string, bool, int or []string: cannot fail
		f[name] = Fact{Tier: tier, Value: raw}
	}
	// Rule facts.
	put("source_kind", Stated, row.SourceKind)
	put("source_public", Stated, row.SourcePublic)
	put("source_precision", Stated, row.SourcePrecision)
	put("worked_examples", Stated, row.WorkedExamples)
	put("reference_updates_per_year", Stated, row.UpdatesPerYear)
	put("rate_tables", Stated, row.RateTables)
	put("roadmap_phase_fit", Stated, row.RoadmapPhaseFit)
	// Build facts.
	put("client_runtime", Designed, tmpl.ClientRuntime)
	put("needs_backend", Designed, tmpl.NeedsBackend)
	put("needs_remote_api", Designed, tmpl.NeedsRemoteAPI)
	put("works_offline", Designed, tmpl.WorksOffline)
	put("needs_account", Designed, tmpl.NeedsAccount)
	put("emits_tracking", Designed, tmpl.EmitsTracking)
	put("connect_src_self", Designed, tmpl.ConnectSrcSelf)
	put("task_count", Designed, 1)
	put("output_kinds", Designed, tmpl.OutputKinds)
	put("largest_input_kb", Designed, tmpl.LargestInputKB)
	put("est_source_lines", Designed, tmpl.EstSourceLines)
	// Observed facts. One observation is one signal: Sextant cannot count
	// sightings and does not invent more.
	put("core_job", string(rec.CoreJob.Tier), *rec.CoreJob.Value)
	put("best_alternative", Seen, alt)
	put("need_signals", Seen, 1)
	put("community_demand", Seen, 1)

	res := &Result{Category: row.Category, SubTool: rec.SubTool, ObservedOn: rec.ObservedOn, Facts: f}
	if res.Validate() != nil {
		// A table row holds something outside the closed vocabulary.
		return nil, errcode.New(errcode.Parse, errcode.RecordFailedSchema)
	}
	return res, nil
}

// citationRow finds the one row the item's citations point to. Citations
// with no row are ignored; no match at all, or matches that disagree on any
// value, pause the item.
func (t Tables) citationRow(citations []string) (CitationRow, error) {
	var first CitationRow
	matched := false
	for _, c := range citations {
		row, ok := t.Citations[c]
		switch {
		case !ok:
		case !matched:
			first, matched = row, true
		case row != first:
			return CitationRow{}, errcode.New(errcode.NeedsSubtool, CitationRowsDisagree)
		}
	}
	if !matched {
		return CitationRow{}, errcode.New(errcode.NeedsSubtool, NoCitationRow)
	}
	if _, ok := factSpecs[first.Category]; !ok {
		return CitationRow{}, errcode.New(errcode.NeedsSubtool, CategoryWithoutTables)
	}
	return first, nil
}

// bestAlternative reads the page's price model, login requirement and
// tracking: a paid tool leaves a paid-only gap; a free one that needs a login
// or loads from other hosts is only a partial answer; a free one with
// neither is complete. If the page does not show enough to say, it pauses.
func bestAlternative(rec *observation.Record) (string, error) {
	if rec.PriceModel.Value == nil {
		return "", errcode.New(errcode.NeedsSubtool, PriceModelNotShown)
	}
	if *rec.PriceModel.Value == "paid" {
		return "paid_only", nil
	}
	if len(rec.Tracking.ExternalHosts) > 0 || (rec.LoginRequirement.Value != nil && *rec.LoginRequirement.Value == "required") {
		return "partial", nil
	}
	if rec.LoginRequirement.Value != nil && rec.Tracking.Tier == observation.Seen {
		return "complete", nil
	}
	return "", errcode.New(errcode.NeedsSubtool, AlternativeUndecidable)
}
