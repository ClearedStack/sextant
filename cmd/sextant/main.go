// Command sextant either observes one saved public page ("sextant observe") or
// runs the single-item Muster queue worker ("sextant work").
//
//	sextant observe -item PA-0001 -url https://calc.example-agency.gov/ \
//	    -page saved.html -observed-on 2026-01-15
//
//	sextant work -muster-url http://muster.clearedstack.svc.cluster.local:8080
//
// Exit status: 0 success; 2 invalid arguments or configuration;
// 10 E_NEEDS_SUBTOOL; 11 E_PARSE; 12 E_TIMEOUT.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ClearedStack/sextant/internal/errcode"
	"github.com/ClearedStack/sextant/internal/fetch"
	"github.com/ClearedStack/sextant/internal/muster"
	"github.com/ClearedStack/sextant/internal/observation"
	"github.com/ClearedStack/sextant/internal/spool"
	"github.com/ClearedStack/sextant/internal/subtool"
	"github.com/ClearedStack/sextant/internal/worker"
	"github.com/ClearedStack/sextant/tools"
)

const (
	exitOK           = 0
	exitUsage        = 2
	exitNeedsSubtool = 10
	exitParse        = 11
	exitTimeout      = 12
)

// memoryLimit is the soft ceiling given to the Go runtime when GOMEMLIMIT is
// unset. It sits 10 MiB under the 30 MiB pod budget in SPEC.md so the
// collector works harder well before the kernel RSS limit is approached.
const memoryLimit = 20 << 20

func main() {
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(memoryLimit)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	os.Exit(runContext(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// failure is what is printed when an item cannot be observed in CLI mode.
// "outcome" is the call Sextant would make to Muster: needs_subtool or fail.
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
	return runContext(context.Background(), args, stdin, stdout, stderr)
}

func runContext(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	usage := func(msg string) int {
		fmt.Fprintln(stderr, "sextant: "+msg)
		fmt.Fprintln(stderr, "usage:")
		fmt.Fprintln(stderr, "  sextant observe -item PA-XXXX -url <https address> -page <file or -> -observed-on YYYY-MM-DD")
		fmt.Fprintln(stderr, "  sextant work [-muster-url <url>] [-token-file <path>] [-spool <dir>] [-drain]")
		return exitUsage
	}
	if len(args) == 0 {
		return usage("a command is required (\"observe\" or \"work\")")
	}
	switch args[0] {
	case "observe":
		return runObserve(args[1:], stdin, stdout, stderr, usage)
	case "work":
		return runWork(ctx, args[1:], stderr, usage)
	default:
		return usage("unknown command (expected \"observe\" or \"work\")")
	}
}

func runObserve(args []string, stdin io.Reader, stdout, stderr io.Writer, usage func(string) int) int {
	fs := flag.NewFlagSet("observe", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	item := fs.String("item", "", "")
	address := fs.String("url", "", "")
	pagePath := fs.String("page", "", "")
	observedOn := fs.String("observed-on", "", "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
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
	defer clear(page)

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
	status := print(stdout, record, exitOK)
	if os.Getenv("SEXTANT_REPORT_PEAK") == "1" {
		writeStoppedPeak(stderr, "observe_stopped")
	}
	return status
}

func runWork(ctx context.Context, args []string, stderr io.Writer, usage func(string) int) int {
	if cv := strings.TrimSpace(os.Getenv("CONTRACT_VERSION")); cv != "" && cv != muster.ContractVersion {
		return usage("CONTRACT_VERSION must be v1")
	}

	pollDefault, ok := envDuration("SEXTANT_POLL_EVERY", 2*time.Second)
	if !ok {
		return usage("SEXTANT_POLL_EVERY is not a valid duration")
	}
	fetchDefault, ok := envDuration("SEXTANT_FETCH_TIMEOUT", 20*time.Second)
	if !ok {
		return usage("SEXTANT_FETCH_TIMEOUT is not a valid duration")
	}
	hbDefault, ok := envDuration("SEXTANT_HEARTBEAT_EVERY", 20*time.Second)
	if !ok {
		return usage("SEXTANT_HEARTBEAT_EVERY is not a valid duration")
	}

	fs := flag.NewFlagSet("work", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	musterURL := fs.String("muster-url", strings.TrimSpace(os.Getenv("MUSTER_URL")), "")
	tokenFile := fs.String("token-file", envOr("SEXTANT_TOKEN_FILE", muster.DefaultTokenFile), "")
	spoolDir := fs.String("spool", envOr("SEXTANT_SPOOL_DIR", spool.DefaultDir), "")
	pollEvery := fs.Duration("poll-every", pollDefault, "")
	fetchTimeout := fs.Duration("fetch-timeout", fetchDefault, "")
	heartbeatEvery := fs.Duration("heartbeat-every", hbDefault, "")
	observedOn := fs.String("observed-on", strings.TrimSpace(os.Getenv("SEXTANT_OBSERVED_ON")), "")
	drain := fs.Bool("drain", false, "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return usage("unrecognised arguments")
	}

	u, err := url.Parse(*musterURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return usage("-muster-url must be an http or https address without credentials")
	}
	if strings.TrimSpace(*tokenFile) == "" {
		return usage("-token-file is required")
	}
	if strings.TrimSpace(*spoolDir) == "" {
		return usage("-spool is required")
	}
	if *pollEvery <= 0 || *fetchTimeout <= 0 || *heartbeatEvery <= 0 {
		return usage("durations must be positive")
	}
	if *observedOn != "" && !observation.ValidDate(*observedOn) {
		return usage("-observed-on must be a date written YYYY-MM-DD")
	}

	registry, err := tools.Load()
	if err != nil {
		fmt.Fprintln(stderr, "sextant: the built-in rule files do not load")
		return exitUsage
	}
	sp, err := spool.Open(*spoolDir)
	if err != nil {
		return usage("-spool directory cannot be opened")
	}
	client, err := muster.NewHTTP(muster.HTTPConfig{
		BaseURL:   *musterURL,
		TokenFile: *tokenFile,
	})
	if err != nil {
		return usage("cannot initialize Muster client")
	}
	fetcher := fetch.New(fetch.Config{
		Timeout: *fetchTimeout,
	})
	w, err := worker.New(worker.Config{
		Muster:            client,
		Fetcher:           fetcher,
		Registry:          registry,
		Spool:             sp,
		Log:               stderr,
		PollInterval:      *pollEvery,
		HeartbeatInterval: *heartbeatEvery,
		ObservedOn:        *observedOn,
		Drain:             *drain,
	})
	if err != nil {
		return usage("cannot initialize worker")
	}
	if err := w.Run(ctx); err != nil {
		return exitTimeout
	}
	writeStoppedPeak(stderr, "worker_stopped")
	return exitOK
}

func writeStoppedPeak(w io.Writer, event string) {
	ev := struct {
		Event   string `json:"event"`
		PeakKiB int    `json:"peak_kib,omitempty"`
	}{
		Event:   event,
		PeakKiB: peakMemoryKiB(),
	}
	if raw, err := json.Marshal(ev); err == nil {
		_, _ = w.Write(append(raw, '\n'))
	}
}

// peakMemoryKiB returns the process high-water RSS in KiB from /proc/self/status
// on Linux (reset across execve, unlike wait4 Maxrss), or 0 where unavailable.
func peakMemoryKiB() int {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if rest, ok := strings.CutPrefix(line, "VmHWM:"); ok {
			fields := strings.Fields(rest)
			if len(fields) == 0 {
				return 0
			}
			n, err := strconv.Atoi(fields[0])
			if err != nil {
				return 0
			}
			return n
		}
	}
	return 0
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) (time.Duration, bool) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback, true
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, false
	}
	return d, true
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
