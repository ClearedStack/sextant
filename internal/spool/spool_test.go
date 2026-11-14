package spool

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ClearedStack/sextant/internal/derive"
	"github.com/ClearedStack/sextant/internal/errcode"
	"github.com/ClearedStack/sextant/internal/observation"
)

func sampleValidRecord() *derive.Result {
	rec := &observation.Record{
		ItemID:             "PA-0001",
		SourceURL:          "https://calc.example.gov/tool",
		SubTool:            "gov-portal",
		ObservedOn:         "2026-01-05",
		CoreJob:            observation.Known("rate_calculation", observation.Seen),
		Inputs:             observation.Inputs{Fields: []observation.InputField{{Kind: "number", Required: true}}, Tier: observation.Seen},
		Outputs:            observation.Outputs{Fields: []observation.OutputField{{Kind: "computed_value"}}, Tier: observation.Seen},
		LoginRequirement:   observation.Known("not_required", observation.Seen),
		Tracking:           observation.Tracking{ExternalHosts: []string{}, Tier: observation.Seen},
		LicenseOrCopyright: observation.Known("public_domain", observation.Seen),
		PriceModel:         observation.Known("free", observation.Seen),
		CitedPublicRules:   observation.CitedRules{Citations: []string{"FAR 31.203"}, Tier: observation.Seen},
		OfflineBehavior:    observation.Known("works_offline", observation.Seen),
	}
	res, err := derive.Default().Derive(rec)
	if err != nil {
		panic(err)
	}
	return res
}

func TestSpoolStageRestartAndQuarantine(t *testing.T) {
	dir := t.TempDir()

	// Pre-seed a stale temp file as if an earlier pod died mid-write before rename.
	pendingDir := filepath.Join(dir, "pending")
	if err := os.MkdirAll(pendingDir, 0o700); err != nil {
		t.Fatal(err)
	}
	staleTmp := filepath.Join(pendingDir, ".tmp-interrupted")
	if err := os.WriteFile(staleTmp, []byte("{partial"), 0o600); err != nil {
		t.Fatal(err)
	}

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := os.Stat(staleTmp); !os.IsNotExist(err) {
		t.Fatalf("expected stale temp file to be removed on Open, got err=%v", err)
	}

	e1, err := s.Stage(Entry{
		Lease:   "PA-0001.1.1",
		ItemID:  "PA-0001",
		Outcome: OutcomeComplete,
		State:   "observed",
		Result:  sampleValidRecord(),
	})
	if err != nil {
		t.Fatalf("Stage complete: %v", err)
	}

	e2, err := s.Stage(Entry{
		Lease:   "PA-0002.2.1",
		ItemID:  "PA-0002",
		Outcome: OutcomeNeedsSubtool,
		Code:    errcode.NeedsSubtool,
		Reason:  errcode.NoMatchingSubTool,
	})
	if err != nil {
		t.Fatalf("Stage needs_subtool: %v", err)
	}
	if e2.Fingerprint == "" {
		t.Fatalf("expected Fingerprint to be populated")
	}

	// Re-open spool to verify persistence and monotonic sequence numbering.
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("re-Open: %v", err)
	}
	e3, err := s2.Stage(Entry{
		Lease:   "PA-0003.3.1",
		ItemID:  "PA-0003",
		Outcome: OutcomeFail,
		Code:    errcode.Parse,
		Reason:  errcode.CorruptedPDF,
	})
	if err != nil {
		t.Fatalf("Stage fail: %v", err)
	}
	if e3.Sequence <= e2.Sequence || e2.Sequence <= e1.Sequence {
		t.Fatalf("sequences not strictly increasing across restart: %d, %d, %d", e1.Sequence, e2.Sequence, e3.Sequence)
	}

	// Drop a corrupt JSON file into pending/ and ensure Pending() quarantines it.
	corruptPath := filepath.Join(pendingDir, "0000000000000099-bad.json")
	if err := os.WriteFile(corruptPath, []byte(`{"broken":true}`), 0o600); err != nil {
		t.Fatal(err)
	}

	pending, err := s2.Pending()
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if len(pending) != 3 {
		t.Fatalf("expected 3 valid pending entries, got %d", len(pending))
	}
	if pending[0].ItemID != "PA-0001" || pending[1].ItemID != "PA-0002" || pending[2].ItemID != "PA-0003" {
		t.Fatalf("unexpected FIFO order: %s, %s, %s", pending[0].ItemID, pending[1].ItemID, pending[2].ItemID)
	}
	if _, err := os.Stat(filepath.Join(dir, "quarantine", "0000000000000099-bad.json")); err != nil {
		t.Fatalf("expected corrupt file in quarantine: %v", err)
	}

	// Acknowledge first entry, quarantine second, keep third.
	if err := s2.Remove(pending[0]); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if err := s2.Quarantine(pending[1]); err != nil {
		t.Fatalf("Quarantine: %v", err)
	}

	remaining, err := s2.Pending()
	if err != nil {
		t.Fatalf("Pending after remove/quarantine: %v", err)
	}
	if len(remaining) != 1 || remaining[0].ItemID != "PA-0003" {
		t.Fatalf("expected only PA-0003 remaining, got %+v", remaining)
	}
}

func TestSpoolRejectsUnsanitizedOrInvalidEntries(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	badRec := sampleValidRecord()
	badRec.Facts["core_job"] = derive.Fact{Tier: derive.Seen, Value: []byte(`"copied vendor marketing phrase"`)}

	cases := []Entry{
		{Lease: "", ItemID: "PA-0001", Outcome: OutcomeNeedsSubtool, Code: errcode.NeedsSubtool, Reason: errcode.NoMatchingSubTool},
		{Lease: "PA-0001.1.1", ItemID: "PA-0001", Outcome: OutcomeComplete, State: "scored", Result: sampleValidRecord()},
		{Lease: "PA-0001.1.1", ItemID: "PA-0001", Outcome: OutcomeComplete, State: "observed", Result: badRec},
		{Lease: "PA-0001.1.1", ItemID: "PA-0001", Outcome: OutcomeFail, Code: errcode.NeedsSubtool, Reason: errcode.NoMatchingSubTool},
		{Lease: "PA-0001.1.1", ItemID: "PA-0001", Outcome: OutcomeNeedsSubtool, State: "observed", Code: errcode.NeedsSubtool, Reason: errcode.NoMatchingSubTool},
	}
	for i, tc := range cases {
		if _, err := s.Stage(tc); err == nil {
			t.Fatalf("case %d: expected Stage to reject invalid entry %+v", i, tc)
		}
	}
}

func logged(t *testing.T, s *Spool) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	s.SetLog(&buf)
	return &buf
}

func writePending(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, "pending", name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPendingLogsOneFixedLineForEachQuarantinedEntry(t *testing.T) {
	good, err := json.Marshal(Entry{Sequence: 5, Lease: "PA-0001.1.1", ItemID: "PA-0001", Outcome: OutcomeComplete, State: "observed", Result: sampleValidRecord()})
	if err != nil {
		t.Fatal(err)
	}
	// An entry the previous build wrote: a "record" where this build wants a "result".
	legacy := `{"sequence":1,"lease":"PA-0001.1.1","item_id":"PA-0001","outcome":"complete","state":"observed","record":{"item_id":"PA-0001","zzcanary":"x"}}`
	invalid := strings.Replace(string(good), `"observed"`, `"scored"`, 1)
	tests := []struct{ name, body, line string }{
		{"legacy", legacy, `{"event":"spool_quarantined","reason":"legacy_record_format"}`},
		{"undecodable", `{not json zzcanary`, `{"event":"spool_quarantined","reason":"undecodable"}`},
		{"invalid", invalid, `{"event":"spool_quarantined","reason":"failed_validation"}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			s, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			buf := logged(t, s)
			writePending(t, dir, "0000000000000001-zzcanary.json", tc.body)
			if got, err := s.Pending(); err != nil || len(got) != 0 {
				t.Fatalf("Pending = %v, %v", got, err)
			}
			if buf.String() != tc.line+"\n" {
				t.Fatalf("log = %q, want %q", buf.String(), tc.line+"\n")
			}
			if _, err := os.Stat(filepath.Join(dir, "quarantine", "0000000000000001-zzcanary.json")); err != nil {
				t.Fatalf("not in quarantine: %v", err)
			}
		})
	}
}

func TestFailedQuarantineIsReportedOnceAndDoesNotBlockOrSpin(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	buf := logged(t, s)
	bad := writePending(t, dir, "0000000000000001-bad.json", `{broken`)
	if _, err := s.Stage(Entry{Lease: "PA-0002.1.1", ItemID: "PA-0002", Outcome: OutcomeNeedsSubtool, Code: errcode.NeedsSubtool, Reason: errcode.NoMatchingSubTool}); err != nil {
		t.Fatal(err)
	}
	// The move fails: there is no quarantine directory to move into.
	if err := os.RemoveAll(filepath.Join(dir, "quarantine")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		got, err := s.Pending()
		if err != nil || len(got) != 1 || got[0].ItemID != "PA-0002" {
			t.Fatalf("pass %d: the good entry must still be delivered: %v, %v", i, got, err)
		}
	}
	if want := `{"event":"spool_quarantine_failed","reason":"move_failed"}` + "\n"; buf.String() != want {
		t.Fatalf("log = %q, want exactly one line %q", buf.String(), want)
	}
	if _, err := os.Stat(bad); err != nil {
		t.Fatalf("the evidence must stay where it is: %v", err)
	}
}

func TestUnreadablePendingFileIsReportedOnce(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	buf := logged(t, s)
	// A dangling link is listed by ReadDir but cannot be read.
	if err := os.Symlink(filepath.Join(dir, "nowhere"), filepath.Join(dir, "pending", "0000000000000001-zzcanary.json")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if got, err := s.Pending(); err != nil || len(got) != 0 {
			t.Fatalf("Pending = %v, %v", got, err)
		}
	}
	if want := `{"event":"spool_read_failed","reason":"read_failed"}` + "\n"; buf.String() != want {
		t.Fatalf("log = %q, want %q", buf.String(), want)
	}
}

func TestQuarantineNeverOverwritesEarlierEvidence(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	logged(t, s)
	name := "0000000000000001-x.json"
	// Two corrupt files with the same name arrive one after the other.
	for _, body := range []string{`{first`, `{second`, `{third`} {
		writePending(t, dir, name, body)
		if _, err := s.Pending(); err != nil {
			t.Fatal(err)
		}
	}
	// And one through Quarantine, which the worker uses for refused entries.
	e, err := s.Stage(Entry{Lease: "PA-0003.1.1", ItemID: "PA-0003", Outcome: OutcomeNeedsSubtool, Code: errcode.NeedsSubtool, Reason: errcode.NoMatchingSubTool})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		raw, err := os.ReadFile(e.path)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Quarantine(e); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(e.path, raw, 0o600); err != nil { // the same file name again
			t.Fatal(err)
		}
	}
	files, err := os.ReadDir(filepath.Join(dir, "quarantine"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 5 { // three corrupt files, two quarantined entries (the third copy is still pending)
		t.Fatalf("quarantine holds %d files, want 5", len(files))
	}
	seen := map[string]bool{}
	for _, f := range files {
		raw, _ := os.ReadFile(filepath.Join(dir, "quarantine", f.Name()))
		seen[string(raw)] = true
	}
	for _, want := range []string{"{first", "{second", "{third"} {
		if !seen[want] {
			t.Errorf("evidence %q was overwritten", want)
		}
	}
}
