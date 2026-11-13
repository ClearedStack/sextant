// Package errcode holds the three public error codes Sextant may report and
// the closed list of reasons that may accompany them.
//
// Reasons are compile-time constants on purpose: an error built from this
// package cannot carry a machine name, an address, a path or a credential,
// because it cannot carry any text that was not written here.
package errcode

import "errors"

// Code is one of the three public error codes.
type Code string

const (
	// Timeout means something outside Sextant took too long.
	Timeout Code = "E_TIMEOUT"
	// Parse means the data was broken.
	Parse Code = "E_PARSE"
	// NeedsSubtool means the data was valid and unfamiliar.
	NeedsSubtool Code = "E_NEEDS_SUBTOOL"
)

// Reason says, in a fixed word, why a code was raised.
type Reason string

const (
	// E_NEEDS_SUBTOOL reasons.
	NoMatchingSubTool     Reason = "no_matching_sub_tool"
	AmbiguousSubTool      Reason = "more_than_one_sub_tool_matches"
	UnfamiliarFormControl Reason = "unfamiliar_form_control"
	AmbiguousCoreJob      Reason = "more_than_one_core_job_rule_matches"
	AmbiguousLicense      Reason = "more_than_one_license_rule_matches"
	AmbiguousPriceModel   Reason = "more_than_one_price_model_rule_matches"
	AmbiguousLogin        Reason = "more_than_one_login_rule_matches"
	AmbiguousOffline      Reason = "more_than_one_offline_rule_matches"
	PageLargerThanBudget  Reason = "page_larger_than_memory_budget_allows"

	// E_PARSE reasons.
	EmptyPage          Reason = "empty_page"
	InvalidText        Reason = "text_is_not_valid_utf8"
	TruncatedMarkup    Reason = "markup_ends_inside_a_construct"
	BrokenFormNesting  Reason = "form_tags_do_not_pair_up"
	BrokenResourceLink Reason = "resource_address_does_not_parse"
	RecordFailedSchema Reason = "record_failed_schema_validation"

	// E_TIMEOUT reasons. Nothing in the first milestone can raise these.
	DeadlineExceeded Reason = "deadline_exceeded"
)

// Error is the only error type that crosses Sextant's public surface.
type Error struct {
	Code   Code
	Reason Reason
}

func (e *Error) Error() string { return string(e.Code) + ": " + string(e.Reason) }

// New builds an Error.
func New(c Code, r Reason) *Error { return &Error{Code: c, Reason: r} }

// From returns the coded error inside err, if there is one.
func From(err error) (*Error, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}
