package errcode

import (
	"fmt"
	"regexp"
	"testing"
)

func TestFingerprintAndFrom(t *testing.T) {
	hex12 := regexp.MustCompile(`^[0-9a-f]{12}$`)
	e1 := New(NeedsSubtool, NoMatchingSubTool)
	e2 := New(NeedsSubtool, NoMatchingSubTool)
	e3 := New(Parse, EmptyPage)

	if e1.Fingerprint() != e2.Fingerprint() {
		t.Fatalf("fingerprint is not deterministic: %q != %q", e1.Fingerprint(), e2.Fingerprint())
	}
	if !hex12.MatchString(e1.Fingerprint()) {
		t.Fatalf("fingerprint %q is not 12 lowercase hex chars", e1.Fingerprint())
	}
	if e1.Fingerprint() == e3.Fingerprint() {
		t.Fatalf("distinct errors collided: %q", e1.Fingerprint())
	}

	wrapped := fmt.Errorf("outer: %w", e1)
	got, ok := From(wrapped)
	if !ok || got.Code != NeedsSubtool || got.Reason != NoMatchingSubTool {
		t.Fatalf("From(wrapped) = %v, %v", got, ok)
	}
}
