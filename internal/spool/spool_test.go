package spool

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ClearedStack/sextant/internal/errcode"
	"github.com/ClearedStack/sextant/internal/observation"
)

func sampleValidRecord() *observation.Record {
	return &observation.Record{
		ItemID:             "PA-0001",
		SourceURL:          "https://calc.example.gov/tool",
		SubTool:            "gov-portal",
		ObservedOn:         "2026-01-05",
		CoreJob:            observation.Known("date_calculation", observation.Seen),
		Inputs:             observation.Inputs{Fields: []observation.InputField{{Kind: "date", Required: true}}, Tier: observation.Seen},
		Outputs:            observation.Outputs{Fields: []observation.OutputField{{Kind: "computed_value"}}, Tier: observation.Seen},
		LoginRequirement:   observation.Known("not_required", observation.Seen),
		Tracking:           observation.Tracking{ExternalHosts: []string{}, Tier: observation.Seen},
		LicenseOrCopyright: observation.Known("public_domain", observation.Seen),
		PriceModel:         observation.Known("free", observation.Seen),
		CitedPublicRules:   observation.CitedRules{Citations: []string{}, Tier: observation.Unknown},
		OfflineBehavior:    observation.Known("server_round_trip", observation.Seen),
	}
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
		Record:  sampleValidRecord(),
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
	badRec.CoreJob = observation.Known("copied vendor marketing phrase", observation.Seen)

	cases := []Entry{
		{Lease: "", ItemID: "PA-0001", Outcome: OutcomeNeedsSubtool, Code: errcode.NeedsSubtool, Reason: errcode.NoMatchingSubTool},
		{Lease: "PA-0001.1.1", ItemID: "PA-0001", Outcome: OutcomeComplete, State: "scored", Record: sampleValidRecord()},
		{Lease: "PA-0001.1.1", ItemID: "PA-0001", Outcome: OutcomeComplete, State: "observed", Record: badRec},
		{Lease: "PA-0001.1.1", ItemID: "PA-0001", Outcome: OutcomeFail, Code: errcode.NeedsSubtool, Reason: errcode.NoMatchingSubTool},
		{Lease: "PA-0001.1.1", ItemID: "PA-0001", Outcome: OutcomeNeedsSubtool, State: "observed", Code: errcode.NeedsSubtool, Reason: errcode.NoMatchingSubTool},
	}
	for i, tc := range cases {
		if _, err := s.Stage(tc); err == nil {
			t.Fatalf("case %d: expected Stage to reject invalid entry %+v", i, tc)
		}
	}
}
