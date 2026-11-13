// Command sextant observes one saved public page and prints the structured
// observation record.
//
//	sextant observe -item PA-0001 -url https://calc.example-agency.gov/ \
//	    -page saved.html -observed-on 2026-01-15
//
// It reads the saved page from a file ("-" for standard input). It opens no
// network connection, reads no clock and writes no file.
//
// Exit status: 0 a record was printed; 2 the command was used wrongly;
// 10 E_NEEDS_SUBTOOL; 11 E_PARSE; 12 E_TIMEOUT. For 10 to 12 a small JSON
// failure object is printed in place of the record. Nothing is ever printed
// that is partly a record.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"runtime/debug"

	"github.com/clearedstack/sextant/internal/errcode"
	"github.com/clearedstack/sextant/internal/observation"
	"github.com/clearedstack/sextant/internal/subtool"
	"github.com/clearedstack/sextant/tools"
)

const (
	exitOK           = 0
	exitUsage        = 2
	exitNeedsSubtool = 10
	exitParse        = 11
	exitTimeout      = 12
)

// memoryLimit is the soft ceiling given to the Go runtime. It sits under the
// 30 MiB budget in SPEC.md so the collector works harder before the budget
// is reached, not after.
const memoryLimit = 20 << 20

func main() {
	debug.SetMemoryLimit(memoryLimit)
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// failure is what is printed when an item cannot be observed. "outcome" is
// the call Sextant would make to Muster for the item: needs_subtool or fail.
type failure struct {
	ItemID  string       `json:"item_id"`
	Outcome string       `json:"outcome"`
	Error   failureError `json:"error"`
}

type failureError struct {
	Code   errcode.Code   `json:"code"`
	Reason errcode.Reason `json:"reason"`
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	usage := func(msg string) int {
		fmt.Fprintln(stderr, "sextant: "+msg)
		fmt.Fprintln(stderr, "usage: sextant observe -item PA-XXXX -url <https address> -page <file or -> -observed-on YYYY-MM-DD")
		return exitUsage
	}
	if len(args) == 0 || args[0] != "observe" {
		return usage("the only command is \"observe\"")
	}
	fs := flag.NewFlagSet("observe", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	item := fs.String("item", "", "")
	address := fs.String("url", "", "")
	pagePath := fs.String("page", "", "")
	observedOn := fs.String("observed-on", "", "")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
		return usage("unrecognised arguments")
	}
	if !observation.ValidItemID(*item) {
		return usage("-item must look like PA-0001")
	}
	if !observation.ValidDate(*observedOn) {
		return usage("-observed-on must be a date written YYYY-MM-DD")
	}
	u, err := url.Parse(*address)
	if err != nil || !u.IsAbs() || u.Host == "" || u.Opaque != "" {
		return usage("-url must be a full address")
	}
	if u.User != nil {
		return usage("-url must not contain a user name or password")
	}
	u.Fragment, u.RawFragment = "", ""
	if *pagePath == "" {
		return usage("-page is required")
	}

	in := stdin
	if *pagePath != "-" {
		f, err := os.Open(*pagePath)
		if err != nil {
			return usage("the -page file cannot be opened")
		}
		defer f.Close()
		in = f
	}
	// Read one byte past the limit so an oversized page is noticed without
	// ever being held in full.
	page, err := io.ReadAll(io.LimitReader(in, subtool.MaxPageBytes+1))
	if err != nil {
		return usage("the -page file cannot be read")
	}

	registry, err := tools.Load()
	if err != nil {
		fmt.Fprintln(stderr, "sextant: the built-in rule files do not load: "+err.Error())
		return exitUsage
	}
	record, err := registry.Observe(subtool.Target{ItemID: *item, URL: u, Page: page, ObservedOn: *observedOn})
	if err != nil {
		coded, _ := errcode.From(err) // Observe returns only coded errors
		f := failure{ItemID: *item, Outcome: "fail", Error: failureError{Code: coded.Code, Reason: coded.Reason}}
		status := exitParse
		switch coded.Code {
		case errcode.NeedsSubtool:
			f.Outcome, status = "needs_subtool", exitNeedsSubtool
		case errcode.Timeout:
			status = exitTimeout
		}
		return print(stdout, f, status)
	}
	return print(stdout, record, exitOK)
}

// print writes v as indented JSON. Field order is fixed by the struct
// definitions, so the bytes are the same on every machine.
func print(w io.Writer, v any, status int) int {
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return exitUsage
	}
	if _, err := w.Write(append(out, '\n')); err != nil {
		return exitUsage
	}
	return status
}
