package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/clearedstack/sextant/internal/subtool"
)

var update = flag.Bool("update", false, "rewrite every fixture's expected.json from the current output")

type fixture struct {
	dir        string
	ItemID     string `json:"item_id"`
	URL        string `json:"url"`
	Page       string `json:"page"`
	ObservedOn string `json:"observed_on"`
	Exit       int    `json:"exit"`
}

func (f fixture) args() []string {
	return []string{"observe", "-item", f.ItemID, "-url", f.URL,
		"-page", filepath.Join(f.dir, f.Page), "-observed-on", f.ObservedOn}
}

// fixtures loads every saved example of every sub-tool.
func fixtures(t *testing.T) []fixture {
	t.Helper()
	dirs, err := filepath.Glob("../../tools/*/fixtures/*")
	if err != nil || len(dirs) == 0 {
		t.Fatalf("no fixtures found: %v", err)
	}
	var out []fixture
	for _, dir := range dirs {
		raw, err := os.ReadFile(filepath.Join(dir, "case.json"))
		if err != nil {
			t.Fatalf("%s: %v", dir, err)
		}
		f := fixture{dir: dir}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&f); err != nil {
			t.Fatalf("%s/case.json: %v", dir, err)
		}
		out = append(out, f)
	}
	return out
}

func observe(t *testing.T, args []string, stdin string) (stdout, stderr string, status int) {
	t.Helper()
	var o, e bytes.Buffer
	status = run(args, strings.NewReader(stdin), &o, &e)
	return o.String(), e.String(), status
}

// TestFixtures is the saved-example test: for each saved page the command
// must print exactly the saved output and leave with the saved exit status.
func TestFixtures(t *testing.T) {
	for _, f := range fixtures(t) {
		t.Run(filepath.Base(f.dir), func(t *testing.T) {
			got, stderr, status := observe(t, f.args(), "")
			if stderr != "" {
				t.Errorf("unexpected text on standard error: %q", stderr)
			}
			if status != f.Exit {
				t.Errorf("exit status = %d, want %d", status, f.Exit)
			}
			golden := filepath.Join(f.dir, "expected.json")
			if *update {
				if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatal(err)
			}
			if got != string(want) {
				t.Errorf("output differs from expected.json\n--- got\n%s--- want\n%s", got, want)
			}
		})
	}
}

// TestSameInputSameOutput runs every fixture repeatedly and from standard
// input, and expects the same bytes each time.
func TestSameInputSameOutput(t *testing.T) {
	for _, f := range fixtures(t) {
		first, _, firstStatus := observe(t, f.args(), "")
		for i := 0; i < 5; i++ {
			again, _, status := observe(t, f.args(), "")
			if again != first || status != firstStatus {
				t.Fatalf("%s: run %d differed from the first run", f.dir, i+2)
			}
		}
		page, err := os.ReadFile(filepath.Join(f.dir, f.Page))
		if err != nil {
			t.Fatal(err)
		}
		args := f.args()
		args[6] = "-"
		piped, _, status := observe(t, args, string(page))
		if piped != first || status != firstStatus {
			t.Fatalf("%s: reading the page from standard input changed the result", f.dir)
		}
	}
}

// TestNothingFromThePageLeaks checks the clean-room wall. The fixture pages
// carry the marker "zzcanary" in prose, comments, attribute values, styles,
// scripts and form field names. None of it, and no markup, may reach output.
func TestNothingFromThePageLeaks(t *testing.T) {
	marked := 0
	for _, f := range fixtures(t) {
		page, err := os.ReadFile(filepath.Join(f.dir, f.Page))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(bytes.ToLower(page), []byte("zzcanary")) {
			marked++
		}
		out, _, _ := observe(t, f.args(), "")
		lower := strings.ToLower(out)
		for _, banned := range []string{"zzcanary", "<", ">", `<`, `>`} {
			if strings.Contains(lower, banned) {
				t.Errorf("%s: output contains %q", f.dir, banned)
			}
		}
	}
	if marked < 5 {
		t.Fatalf("only %d fixtures carry the marker; the test would prove little", marked)
	}
}

// TestFailuresAreSanitized checks the shape of every failure: one of the
// three codes, a reason that is a fixed lower-case word, and nothing else.
func TestFailuresAreSanitized(t *testing.T) {
	shape := regexp.MustCompile(`^\{\n  "item_id": "PA-[0-9A-Z]{4}",\n  "outcome": "(needs_subtool|fail)",\n  "error": \{\n    "code": "(E_NEEDS_SUBTOOL|E_PARSE|E_TIMEOUT)",\n    "reason": "[a-z0-9_]+"\n  \}\n\}\n$`)
	failures := 0
	for _, f := range fixtures(t) {
		out, _, status := observe(t, f.args(), "")
		if status == exitOK {
			continue
		}
		failures++
		if !shape.MatchString(out) {
			t.Errorf("%s: failure output has the wrong shape:\n%s", f.dir, out)
		}
		wantOutcome := map[int]string{exitNeedsSubtool: `"needs_subtool"`, exitParse: `"fail"`}[status]
		wantCode := map[int]string{exitNeedsSubtool: `"E_NEEDS_SUBTOOL"`, exitParse: `"E_PARSE"`}[status]
		if wantOutcome == "" || !strings.Contains(out, wantOutcome) || !strings.Contains(out, wantCode) {
			t.Errorf("%s: exit status %d does not agree with the output:\n%s", f.dir, status, out)
		}
	}
	if failures == 0 {
		t.Fatal("no failing fixtures were found")
	}
}

func TestOversizedPageIsPaused(t *testing.T) {
	page := "<!DOCTYPE html><html><body><form><input type=\"date\"></form>" +
		strings.Repeat("<p>made-up filler</p>", subtool.MaxPageBytes/20+1) + "</body></html>"
	out, _, status := observe(t, []string{"observe", "-item", "PA-0301",
		"-url", "https://calc.example-agency.gov/big", "-page", "-", "-observed-on", "2026-01-15"}, page)
	if status != exitNeedsSubtool || !strings.Contains(out, `"page_larger_than_memory_budget_allows"`) {
		t.Fatalf("status %d, output:\n%s", status, out)
	}
}

func TestUsageErrors(t *testing.T) {
	good := []string{"observe", "-item", "PA-0001", "-url", "https://calc.example-agency.gov/",
		"-page", "-", "-observed-on", "2026-01-15"}
	with := func(i int, v string) []string {
		a := append([]string(nil), good...)
		a[i] = v
		return a
	}
	cases := map[string][]string{
		"no command":        {},
		"unknown command":   {"serve"},
		"bad item":          with(2, "PA-1"),
		"relative url":      with(4, "/due-date"),
		"credential in url": with(4, "https://someone:hunter2-made-up@calc.example-agency.gov/"),
		"missing page":      with(6, filepath.Join(t.TempDir(), "absent-zzpath.html")),
		"bad date":          with(8, "2026-02-30"),
		"today is no date":  with(8, "today"),
		"extra argument":    append(append([]string(nil), good...), "extra"),
		"unknown flag":      append(append([]string(nil), good...), "-fetch"),
	}
	for name, args := range cases {
		out, stderr, status := observe(t, args, "<!DOCTYPE html><html></html>")
		if status != exitUsage || out != "" || stderr == "" {
			t.Errorf("%s: status %d, stdout %q, stderr %q", name, status, out, stderr)
		}
		for _, banned := range []string{"hunter2", "someone", "zzpath"} {
			if strings.Contains(stderr, banned) {
				t.Errorf("%s: the message repeats %q", name, banned)
			}
		}
	}
}
