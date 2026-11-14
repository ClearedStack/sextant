package derive

// Default returns the reviewed tables. Each call builds fresh maps so no
// caller can change another's.
//
// Rule: add a row only with a value you can justify from the public text,
// and say where it comes from. A citation or core job with no row pauses.
func Default() Tables {
	return Tables{
		Citations: map[string]CitationRow{
			// FAR Part 31, "Contract Cost Principles and Procedures", on
			// acquisition.gov. Common to every row: source_kind far and
			// source_public true (the FAR is a published regulation, 48 CFR
			// Chapter 1); worked_examples 0 (Part 31 prints no numerical
			// worked example); rate_tables 0 (it prints no rate table: rates
			// are each contractor's own, negotiated or audited);
			// reference_updates_per_year 1 is a cautious ceiling, not a
			// count (FAR text changes only through Federal Acquisition
			// Circulars; how often Part 31 itself changes was not checked,
			// so verify it against the FAC history); roadmap_phase_fit 3
			// is a ClearedStack planning input, not from the regulation:
			// pricing is the category phase 1 is built around.
			//
			// Part 31 as a whole: states cost principles (applicability,
			// allowability, allocability) and gives no single formula.
			"FAR Part 31": farPart31(sourcePrecisionGeneralPrinciple),
			// FAR 31.201-2: the factors for determining allowability
			// (reasonableness, allocability, CAS or GAAP, contract terms,
			// other limitations). A list of tests, not a computation.
			"FAR 31.201-2": farPart31(sourcePrecisionGeneralPrinciple),
			// FAR 31.203: indirect costs. Defines how to accumulate them into
			// pools and choose an allocation base (31.203(b) to (d)), which
			// is a defined procedure; it prints no formula for a rate.
			"FAR 31.203": farPart31(sourcePrecisionDefinedProcedure),
		},
		Templates: map[string]TemplateRow{
			// The calculator template (Moldloft SPEC.md, sub-tools table:
			// "Numeric rates or dates in; step-by-step math and cited totals
			// out"). Moldloft SPEC.md "Generated tool security" guarantees
			// connect-src 'self', no tracking and offline execution; a
			// generated tool is static files, so browser JS, no backend, no
			// remote API and no account. output_kinds are the math
			// (computed_number) and the cited totals (citation_lookup).
			// largest_input_kb 1: typed numbers and dates, no file input.
			// est_source_lines 400 is an estimate, not measured: replace it
			// with the size Moldloft's golden calculator reports.
			"rate_calculation":     calculator(),
			"amount_calculation":   calculator(),
			"cost_allocation":      calculator(),
			"price_escalation":     calculator(),
			"interest_calculation": calculator(),
		},
	}
}

const (
	sourcePrecisionGeneralPrinciple = "general_principle"
	sourcePrecisionDefinedProcedure = "defined_procedure"
)

// farPart31 is the row every Part 31 provision shares, apart from how
// precisely it states the rule.
func farPart31(precision string) CitationRow {
	return CitationRow{
		Category:        Pricing,
		SourceKind:      "far",
		SourcePublic:    true,
		SourcePrecision: precision,
		WorkedExamples:  0,
		UpdatesPerYear:  1,
		RateTables:      0,
		RoadmapPhaseFit: 3,
	}
}

func calculator() TemplateRow {
	return TemplateRow{
		ClientRuntime:  "browser_js",
		WorksOffline:   true,
		ConnectSrcSelf: true,
		OutputKinds:    []string{"citation_lookup", "computed_number"},
		LargestInputKB: 1,
		EstSourceLines: 400,
	}
}
