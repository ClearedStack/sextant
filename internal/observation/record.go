// Package observation defines the structured observation record: the only
// thing Sextant lets out about a page. Every value in a record comes from a
// closed vocabulary defined here, is a normalised public-rule citation, a
// host name, a date or a count. No field can hold free text from the page,
// which is how "copying nothing" is enforced by construction.
package observation

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"time"
)

// Tier is the confidence tier of one fact.
type Tier string

const (
	// Seen: Sextant observed the fact directly in the public page's structure.
	Seen Tier = "Seen"
	// Stated: the page claims it in words; Sextant did not verify the claim.
	Stated Tier = "Stated"
	// Unknown: no rule found the fact. The value is empty.
	Unknown Tier = "Unknown"
)

// MaxAddressBytes is the longest address Muster and Logbook accept.
const MaxAddressBytes = 2000

// Closed vocabularies. A sub-tool's rule file may only produce these values.
var (
	CoreJobs = set(
		"date_calculation",
		"amount_calculation",
		"rate_calculation",
		"eligibility_check",
		"record_search",
		"compliance_matrix",
		"control_mapping",
		"score_calculation",
		"config_validation",
	)
	// InputKinds and OutputKinds describe the shape of a field, never its label.
	InputKinds        = set("text", "number", "date", "choice", "file")
	OutputKinds       = set("computed_value", "structured_data", "compliance_report", "document_export")
	LoginRequirements = set("required", "not_required")
	Licenses          = set("public_domain", "copyright_asserted", "open_license")
	PriceModels       = set("free", "paid")
	OfflineBehaviors  = set("works_offline", "server_round_trip")
)

// Fact is a single value with its tier. Value is nil exactly when the tier is
// Unknown.
type Fact struct {
	Value *string `json:"value"`
	Tier  Tier    `json:"tier"`
}

// Known builds a Fact with a value.
func Known(value string, tier Tier) Fact { return Fact{Value: &value, Tier: tier} }

// NotKnown builds the Unknown Fact.
func NotKnown() Fact { return Fact{Tier: Unknown} }

// InputField is one thing the tool asks for.
type InputField struct {
	Kind     string `json:"kind"`
	Required bool   `json:"required"`
}

// OutputField is one thing the tool gives back.
type OutputField struct {
	Kind string `json:"kind"`
}

// Inputs lists input fields in page order.
type Inputs struct {
	Fields []InputField `json:"fields"`
	Tier   Tier         `json:"tier"`
}

// Outputs lists output fields in page order.
type Outputs struct {
	Fields []OutputField `json:"fields"`
	Tier   Tier          `json:"tier"`
}

// Tracking is the tracking profile visible in the page's static markup:
// the hosts, other than the page's own, that the markup loads resources from.
type Tracking struct {
	ExternalHosts []string `json:"external_hosts"`
	Tier          Tier     `json:"tier"`
}

// CitedRules lists normalised public-rule citations, sorted.
type CitedRules struct {
	Citations []string `json:"citations"`
	Tier      Tier     `json:"tier"`
}

// Record is the structured observation record. Field order here is the field
// order of the printed JSON.
type Record struct {
	ItemID             string     `json:"item_id"`
	SourceURL          string     `json:"source_url"`
	SubTool            string     `json:"sub_tool"`
	ObservedOn         string     `json:"observed_on"`
	PublicationDate    *Fact      `json:"publication_date,omitempty"`
	CoreJob            Fact       `json:"core_job"`
	Inputs             Inputs     `json:"inputs"`
	Outputs            Outputs    `json:"outputs"`
	LoginRequirement   Fact       `json:"login_requirement"`
	Tracking           Tracking   `json:"tracking"`
	LicenseOrCopyright Fact       `json:"license_or_copyright"`
	PriceModel         Fact       `json:"price_model"`
	CitedPublicRules   CitedRules `json:"cited_public_rules"`
	OfflineBehavior    Fact       `json:"offline_behavior"`
}

var (
	itemIDPattern   = regexp.MustCompile(`^PA-([0-9]{4}|[1-9][0-9]{4,8})$`)
	subToolPattern  = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	hostPattern     = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$`)
	citationPattern = regexp.MustCompile(`^[A-Z]+ [A-Za-z0-9][A-Za-z0-9 .-]*$`)
)

// ValidItemID reports whether s matches Muster and Logbook's item ID pattern.
func ValidItemID(s string) bool { return itemIDPattern.MatchString(s) }

// ValidDate reports whether s is a calendar date written YYYY-MM-DD.
func ValidDate(s string) bool {
	t, err := time.Parse("2006-01-02", s)
	return err == nil && t.Format("2006-01-02") == s
}

func hasWhiteSpace(s string) bool {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ' ', '\t', '\n', '\r', '\v', '\f':
			return true
		}
	}
	return false
}

// Validate checks a record against the schema. It returns nil or a plain
// description of the first problem; the description names fields, never page
// content.
func (r *Record) Validate() error {
	if !ValidItemID(r.ItemID) {
		return errors.New("item_id is not a valid PA identifier")
	}
	if len(r.SourceURL) == 0 || len(r.SourceURL) > MaxAddressBytes || hasWhiteSpace(r.SourceURL) {
		return errors.New("source_url exceeds address bounds or contains white space")
	}
	u, err := url.Parse(r.SourceURL)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
		return errors.New("source_url is not a plain https address")
	}
	if !subToolPattern.MatchString(r.SubTool) {
		return errors.New("sub_tool is not a slug")
	}
	if !ValidDate(r.ObservedOn) {
		return errors.New("observed_on is not YYYY-MM-DD")
	}
	if r.PublicationDate != nil {
		if err := checkDateFact("publication_date", *r.PublicationDate); err != nil {
			return err
		}
	}
	facts := []struct {
		name  string
		fact  Fact
		vocab map[string]bool
	}{
		{"core_job", r.CoreJob, CoreJobs},
		{"login_requirement", r.LoginRequirement, LoginRequirements},
		{"license_or_copyright", r.LicenseOrCopyright, Licenses},
		{"price_model", r.PriceModel, PriceModels},
		{"offline_behavior", r.OfflineBehavior, OfflineBehaviors},
	}
	for _, f := range facts {
		if err := checkFact(f.name, f.fact, f.vocab); err != nil {
			return err
		}
	}
	if err := checkFieldList("inputs", len(r.Inputs.Fields), r.Inputs.Fields == nil, r.Inputs.Tier); err != nil {
		return err
	}
	for _, in := range r.Inputs.Fields {
		if !InputKinds[in.Kind] {
			return errors.New("inputs holds a kind outside the vocabulary")
		}
	}
	if err := checkFieldList("outputs", len(r.Outputs.Fields), r.Outputs.Fields == nil, r.Outputs.Tier); err != nil {
		return err
	}
	for _, out := range r.Outputs.Fields {
		if !OutputKinds[out.Kind] {
			return errors.New("outputs holds a kind outside the vocabulary")
		}
	}
	// Tracking is the one list that may be empty and Seen: "the static markup
	// loads nothing from another host" is itself something that was seen.
	if r.Tracking.ExternalHosts == nil {
		return errors.New("tracking.external_hosts is missing")
	}
	switch r.Tracking.Tier {
	case Seen:
	case Unknown:
		if len(r.Tracking.ExternalHosts) != 0 {
			return errors.New("tracking is Unknown but lists hosts")
		}
	default:
		return errors.New("tracking has a tier it cannot have")
	}
	if !sortedUnique(r.Tracking.ExternalHosts) {
		return errors.New("tracking.external_hosts is not sorted and unique")
	}
	for _, h := range r.Tracking.ExternalHosts {
		if !hostPattern.MatchString(h) {
			return errors.New("tracking.external_hosts holds something that is not a host name")
		}
	}
	if err := checkSeenList("cited_public_rules", len(r.CitedPublicRules.Citations), r.CitedPublicRules.Citations == nil, r.CitedPublicRules.Tier); err != nil {
		return err
	}
	if !sortedUnique(r.CitedPublicRules.Citations) {
		return errors.New("cited_public_rules.citations is not sorted and unique")
	}
	for _, c := range r.CitedPublicRules.Citations {
		if len(c) > 64 || !citationPattern.MatchString(c) {
			return errors.New("cited_public_rules holds something that is not a normalised citation")
		}
	}
	return nil
}

func checkFact(name string, f Fact, vocab map[string]bool) error {
	switch f.Tier {
	case Unknown:
		if f.Value != nil {
			return fmt.Errorf("%s is Unknown but has a value", name)
		}
	case Seen, Stated:
		if f.Value == nil || !vocab[*f.Value] {
			return fmt.Errorf("%s holds a value outside the vocabulary", name)
		}
	default:
		return fmt.Errorf("%s has a tier that does not exist", name)
	}
	return nil
}

func checkDateFact(name string, f Fact) error {
	switch f.Tier {
	case Unknown:
		if f.Value != nil {
			return fmt.Errorf("%s is Unknown but has a value", name)
		}
	case Seen, Stated:
		if f.Value == nil || !ValidDate(*f.Value) {
			return fmt.Errorf("%s is not YYYY-MM-DD", name)
		}
	default:
		return fmt.Errorf("%s has a tier that does not exist", name)
	}
	return nil
}

// checkFieldList enforces: a field list is never null; it is empty exactly when
// Unknown; a non-empty field list is Seen (live controls) or Stated (feature table).
func checkFieldList(name string, n int, isNil bool, tier Tier) error {
	if isNil {
		return fmt.Errorf("%s list is missing", name)
	}
	switch {
	case tier == Unknown && n == 0, (tier == Seen || tier == Stated) && n > 0:
		return nil
	}
	return fmt.Errorf("%s tier does not agree with its list", name)
}

// checkSeenList enforces: a list is never null; it is empty exactly when
// Unknown; a non-empty list is Seen.
func checkSeenList(name string, n int, isNil bool, tier Tier) error {
	if isNil {
		return fmt.Errorf("%s list is missing", name)
	}
	switch {
	case tier == Unknown && n == 0, tier == Seen && n > 0:
		return nil
	}
	return fmt.Errorf("%s tier does not agree with its list", name)
}

func sortedUnique(s []string) bool {
	return sort.SliceIsSorted(s, func(i, j int) bool { return s[i] < s[j] }) && !hasDuplicate(s)
}

func hasDuplicate(sorted []string) bool {
	for i := 1; i < len(sorted); i++ {
		if sorted[i] == sorted[i-1] {
			return true
		}
	}
	return false
}

func set(values ...string) map[string]bool {
	m := make(map[string]bool, len(values))
	for _, v := range values {
		m[v] = true
	}
	return m
}
