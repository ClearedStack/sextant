package worker

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ClearedStack/sextant/internal/derive"
	"github.com/ClearedStack/sextant/internal/errcode"
	"github.com/ClearedStack/sextant/internal/fetch"
	"github.com/ClearedStack/sextant/internal/muster"
	"github.com/ClearedStack/sextant/internal/spool"
	"github.com/ClearedStack/sextant/tools"
	browserjob "github.com/ClearedStack/sextant/tools/on-demand-browser-job"
)

type stubBrowserRunner struct {
	capture *browserjob.Capture
}

func (s *stubBrowserRunner) Render(_ context.Context, _ *url.URL) (*browserjob.Capture, error) {
	return s.capture, nil
}

func mustReadFixture(t *testing.T, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "tools", rel))
	if err != nil {
		t.Fatalf("read fixture %s: %v", rel, err)
	}
	return b
}

func newTestFetcher(t *testing.T, targetSrv *httptest.Server, browser fetch.BrowserRunner, timeout time.Duration) *fetch.Fetcher {
	t.Helper()
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	addr := targetSrv.Listener.Addr().String()
	rt := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, addr)
		},
	}
	t.Cleanup(rt.CloseIdleConnections)
	return fetch.New(fetch.Config{
		Timeout:   timeout,
		Transport: rt,
		Browser:   browser,
	})
}

// testTables cites the saved pages' own citations, so that the saved pages
// reach "observed". The default tables hold FAR Part 31 only.
func testTables() derive.Tables {
	tables := derive.Default()
	row := tables.Citations["FAR 31.203"]
	for _, c := range []string{"FAR Part 15", "FAR 15.404-1", "FAR Part 16"} {
		tables.Citations[c] = row
	}
	tables.Templates["date_calculation"] = tables.Templates["rate_calculation"]
	return tables
}

func postMusterJSON(t *testing.T, baseURL, token, path string, body any) (int, []byte) {
	t.Helper()
	rawBody, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, baseURL+path, bytes.NewReader(rawBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, out
}

func TestWorkerEndToEndAllFourSubToolsAndSanitizedLogs(t *testing.T) {
	govHTML := mustReadFixture(t, "gov-portal/fixtures/observed-calculator/page.html")
	// Append a canary phrase in an HTML comment to prove neither logs nor
	// completed observation records copy vendor or agency text.
	govHTMLWithCanary := append(append([]byte(nil), govHTML...), []byte("\n<!-- zzcanary-secret-phrase -->\n")...)
	saasHTML := mustReadFixture(t, "saas-marketing/fixtures/observed-free-rate-tool/page.html")
	pdfBytes := mustReadFixture(t, "agency-pdf/fixtures/observed-flate-worksheet/page.pdf")
	spaCaptureRaw := mustReadFixture(t, "on-demand-browser-job/fixtures/observed-public-rate-spa/capture.json")

	var spaCap browserjob.Capture
	if err := json.Unmarshal(spaCaptureRaw, &spaCap); err != nil {
		t.Fatalf("unmarshal spa capture: %v", err)
	}

	targetSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		switch r.Host {
		case "calc.example.gov":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(govHTMLWithCanary)
		case "vendor.example.com":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(saasHTML)
		case "forms.example.gov":
			w.Header().Set("Content-Type", "application/pdf")
			_, _ = w.Write(pdfBytes)
		case "spa.example.com":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(`<!doctype html><html><body><div id="root"></div><script type="module" src="/assets/index.js"></script></body></html>`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer targetSrv.Close()

	hub := muster.NewStandIn()
	hub.Seed([]muster.SeedItem{
		{
			ID:    "PA-0001",
			State: "recorded",
			Found: &muster.Found{Source: "sam-gov", Title: "Gov Calc", Address: "https://calc.example.gov/zzcanary"},
		},
		{
			// 5-digit Logbook ID + post-restart empty Found, relying on results.trawl.address fallback.
			ID:    "PA-10000",
			State: "recorded",
			Found: &muster.Found{Source: "", Title: "Vendor Matrix", Address: ""},
			Results: map[string]json.RawMessage{
				"trawl": json.RawMessage(`{"address":"https://vendor.example.com/pricing"}`),
			},
		},
		{
			ID:    "PA-0002",
			State: "recorded",
			Found: &muster.Found{Source: "web-sweep", Title: "Agency Worksheet", Address: "https://forms.example.gov/worksheet.pdf"},
		},
		{
			ID:    "PA-0003",
			State: "recorded",
			Found: &muster.Found{Source: "github-issues", Title: "Offline SBOM SPA", Address: "https://spa.example.com/app"},
		},
	})
	hubSrv := httptest.NewServer(hub)
	defer hubSrv.Close()

	tokenPath := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenPath, []byte("sextant\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	musterClient, err := muster.NewHTTP(muster.HTTPConfig{
		BaseURL:   hubSrv.URL,
		TokenFile: tokenPath,
	})
	if err != nil {
		t.Fatal(err)
	}

	reg, err := tools.Load()
	if err != nil {
		t.Fatal(err)
	}
	sp, err := spool.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	var logBuf bytes.Buffer
	w, err := New(Config{
		Muster:            musterClient,
		Fetcher:           newTestFetcher(t, targetSrv, &stubBrowserRunner{capture: &spaCap}, 5*time.Second),
		Registry:          reg,
		Tables:            testTables(),
		Spool:             sp,
		Log:               &logBuf,
		PollInterval:      10 * time.Millisecond,
		HeartbeatInterval: 50 * time.Millisecond,
		Drain:             true,
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := w.Run(context.Background()); err != nil {
		t.Fatalf("Worker.Run: %v", err)
	}

	// Verify logs never contain URLs, hostnames, bearer tokens, or zzcanary.
	logs := logBuf.String()
	for _, forbidden := range []string{"zzcanary", "calc.example.gov", "vendor.example.com", "https://", "Bearer"} {
		if strings.Contains(logs, forbidden) {
			t.Fatalf("worker log leaked %q:\n%s", forbidden, logs)
		}
	}

	// Verify Assay can lease all 4 items in state "observed" with valid Sextant records.
	assayClient, err := muster.NewHTTP(muster.HTTPConfig{
		BaseURL:     hubSrv.URL,
		StaticToken: "assay",
	})
	if err != nil {
		t.Fatal(err)
	}
	wantSubTools := map[string]string{
		"PA-0001":  "gov-portal",
		"PA-10000": "saas-marketing",
		"PA-0002":  "agency-pdf",
		"PA-0003":  "on-demand-browser-job",
	}
	seenItems := map[string]bool{}
	for i := 0; i < 4; i++ {
		step, err := assayClient.Lease(context.Background())
		if err != nil {
			t.Fatalf("assay lease %d: %v", i, err)
		}
		if step == nil {
			t.Fatalf("expected 4 items for assay, got nil on #%d", i)
		}
		if step.State != "observed" {
			t.Fatalf("expected state observed, got %q", step.State)
		}
		rawRec, ok := step.Results["sextant"]
		if !ok {
			t.Fatalf("missing results.sextant on %s", step.ID)
		}
		if strings.Contains(string(rawRec), "zzcanary-secret-phrase") {
			t.Fatalf("results.sextant copied page text on %s", step.ID)
		}
		var top map[string]json.RawMessage
		if err := json.Unmarshal(rawRec, &top); err != nil || len(top) != 4 {
			t.Fatalf("results.sextant on %s must hold exactly category, sub_tool, observed_on and facts: %v", step.ID, err)
		}
		var rec derive.Result
		if err := json.Unmarshal(rawRec, &rec); err != nil {
			t.Fatalf("unmarshal results.sextant on %s: %v", step.ID, err)
		}
		if err := rec.Validate(); err != nil {
			t.Fatalf("Validate results.sextant on %s: %v", step.ID, err)
		}
		if rec.SubTool != wantSubTools[step.ID] {
			t.Fatalf("%s: got sub_tool %q, want %q", step.ID, rec.SubTool, wantSubTools[step.ID])
		}
		seenItems[step.ID] = true
	}
	if len(seenItems) != 4 {
		t.Fatalf("expected 4 distinct items for assay, got %v", seenItems)
	}
}

func TestWorkerPauseReplayTimeoutFailReleaseAndSpoolRecovery(t *testing.T) {
	var serveValid atomic.Bool
	var serveTimeout atomic.Bool
	var slowFetch atomic.Bool
	govHTML := mustReadFixture(t, "gov-portal/fixtures/observed-calculator/page.html")

	targetSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		if slowFetch.Load() {
			time.Sleep(60 * time.Millisecond)
		}
		switch r.URL.Path {
		case "/unfamiliar":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			if serveValid.Load() {
				_, _ = w.Write(govHTML)
				return
			}
			// Page with no tool form pauses with E_NEEDS_SUBTOOL.
			_, _ = w.Write([]byte(`<!doctype html><html><body><h1>Static agency notice</h1></body></html>`))
		case "/flaky":
			if serveTimeout.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(govHTML)
		case "/corrupt":
			if serveValid.Load() {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				_, _ = w.Write(govHTML)
				return
			}
			// Invalid UTF-8 inside HTML triggers E_PARSE.
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(append([]byte("<!doctype html><html><body>"), 0xff, 0xfe, 0xfd))
		default:
			http.NotFound(w, r)
		}
	}))
	defer targetSrv.Close()

	hub := muster.NewStandIn()
	hub.Seed([]muster.SeedItem{
		{
			ID:    "PA-0010",
			State: "recorded",
			Found: &muster.Found{Source: "sam-gov", Title: "Unfamiliar", Address: "https://calc.example.gov/unfamiliar"},
		},
		{
			ID:    "PA-0012",
			State: "recorded",
			Found: &muster.Found{Source: "sam-gov", Title: "Corrupt", Address: "https://calc.example.gov/corrupt"},
		},
		{
			ID:    "PA-0011",
			State: "recorded",
			Found: &muster.Found{Source: "sam-gov", Title: "Flaky", Address: "https://calc.example.gov/flaky"},
		},
	})
	hubSrv := httptest.NewServer(hub)
	defer hubSrv.Close()

	musterClient, err := muster.NewHTTP(muster.HTTPConfig{
		BaseURL:     hubSrv.URL,
		StaticToken: "sextant",
	})
	if err != nil {
		t.Fatal(err)
	}
	reg, err := tools.Load()
	if err != nil {
		t.Fatal(err)
	}
	spoolDir := t.TempDir()
	sp, err := spool.Open(spoolDir)
	if err != nil {
		t.Fatal(err)
	}

	serveTimeout.Store(true)
	slowFetch.Store(true)

	w, err := New(Config{
		Muster:            musterClient,
		Fetcher:           newTestFetcher(t, targetSrv, nil, 5*time.Second),
		Registry:          reg,
		Tables:            testTables(),
		Spool:             sp,
		PollInterval:      10 * time.Millisecond,
		HeartbeatInterval: 15 * time.Millisecond,
		Drain:             true,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Step 1: PA-0010 pauses via /v1/needs_subtool (while heartbeating during slowFetch).
	worked, err := w.StepOnce(context.Background())
	if err != nil || !worked {
		t.Fatalf("step 1 (PA-0010 pause): worked=%v err=%v", worked, err)
	}

	// Step 2: PA-0012 hits invalid UTF-8 -> /v1/fail E_PARSE -> held + Bellbuoy incident!
	worked, err = w.StepOnce(context.Background())
	if err != nil || !worked {
		t.Fatalf("step 2 (PA-0012 parse fail): worked=%v err=%v", worked, err)
	}

	// Step 3: PA-0011 hits 503 -> /v1/fail E_TIMEOUT -> requeued for sextant!
	worked, err = w.StepOnce(context.Background())
	if err != nil || !worked {
		t.Fatalf("step 3 (PA-0011 timeout): worked=%v err=%v", worked, err)
	}

	// Now upstream recovers for PA-0011, and simulate Muster going 503 right when
	// Sextant tries to flush PA-0011's completion!
	serveTimeout.Store(false)
	slowFetch.Store(false)

	var blockComplete atomic.Bool
	blockComplete.Store(true)
	flakyHubSrv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/v1/complete" && blockComplete.Load() {
			rw.Header().Set("Content-Type", "application/json")
			rw.WriteHeader(http.StatusServiceUnavailable)
			_, _ = rw.Write([]byte(`{"error":"E_TIMEOUT","cause":"store_slow","reason":"the record store took too long"}`))
			return
		}
		hub.ServeHTTP(rw, req)
	}))
	defer flakyHubSrv.Close()

	flakyClient, err := muster.NewHTTP(muster.HTTPConfig{
		BaseURL:     flakyHubSrv.URL,
		StaticToken: "sextant",
	})
	if err != nil {
		t.Fatal(err)
	}
	wFlaky, err := New(Config{
		Muster:            flakyClient,
		Fetcher:           newTestFetcher(t, targetSrv, nil, 5*time.Second),
		Registry:          reg,
		Tables:            testTables(),
		Spool:             sp,
		PollInterval:      10 * time.Millisecond,
		HeartbeatInterval: 50 * time.Millisecond,
		Drain:             true,
	})
	if err != nil {
		t.Fatal(err)
	}

	// PA-0011 is observed and staged to spool, then /v1/complete gets 503.
	_, err = wFlaky.StepOnce(context.Background())
	if err == nil {
		t.Fatalf("expected 503 error on /v1/complete flush")
	}
	pending, err := sp.Pending()
	if err != nil || len(pending) != 1 || pending[0].ItemID != "PA-0011" {
		t.Fatalf("expected PA-0011 staged in spool across 503, got %+v (err=%v)", pending, err)
	}

	// Simulate pod restart: reopen spool, unblock /v1/complete, flush succeeds!
	blockComplete.Store(false)
	spRecovered, err := spool.Open(spoolDir)
	if err != nil {
		t.Fatal(err)
	}
	wRecovered, err := New(Config{
		Muster:            flakyClient,
		Fetcher:           newTestFetcher(t, targetSrv, nil, 5*time.Second),
		Registry:          reg,
		Tables:            testTables(),
		Spool:             spRecovered,
		PollInterval:      10 * time.Millisecond,
		HeartbeatInterval: 50 * time.Millisecond,
		Drain:             true,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Trigger Shipkeeper digest change to replay paused PA-0010, and Countersign
	// /v1/release to release held PA-0012!
	serveValid.Store(true)
	status, _ := postMusterJSON(t, hubSrv.URL, "shipkeeper", "/v1/health", map[string]any{
		"contract": "v1",
		"reports": []map[string]any{
			{
				"component": "sextant",
				"state":     "healthy",
				"digest":    "sha256:1111111111111111111111111111111111111111111111111111111111111111",
			},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("first health report returned %d", status)
	}
	status, _ = postMusterJSON(t, hubSrv.URL, "shipkeeper", "/v1/health", map[string]any{
		"contract": "v1",
		"reports": []map[string]any{
			{
				"component": "sextant",
				"state":     "healthy",
				"digest":    "sha256:2222222222222222222222222222222222222222222222222222222222222222",
			},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("second health report returned %d", status)
	}

	// Verify Bellbuoy received incidents before draining Sextant.
	bellbuoyClient, err := muster.NewHTTP(muster.HTTPConfig{
		BaseURL:     hubSrv.URL,
		StaticToken: "bellbuoy",
	})
	if err != nil {
		t.Fatal(err)
	}
	incStep, err := bellbuoyClient.Lease(context.Background())
	if err != nil || incStep == nil || incStep.Kind != "incident" {
		t.Fatalf("expected Bellbuoy incident step, got %+v (err=%v)", incStep, err)
	}

	// Countersign releases held PA-0012.
	status, _ = postMusterJSON(t, hubSrv.URL, "countersign", "/v1/release", map[string]any{
		"contract": "v1",
		"id":       "PA-0012",
	})
	if status != http.StatusOK {
		t.Fatalf("countersign release returned %d", status)
	}

	// Drain Sextant: flushes PA-0011 from spool, then observes replayed PA-0010 and released PA-0012!
	if err := wRecovered.Run(context.Background()); err != nil {
		t.Fatalf("wRecovered.Run: %v", err)
	}

	remaining, err := spRecovered.Pending()
	if err != nil || len(remaining) != 0 {
		t.Fatalf("expected empty spool after drain, got %+v (err=%v)", remaining, err)
	}

	// Verify all three (PA-0010, PA-0011, PA-0012) are now queued for Assay in state "observed".
	assayClient, err := muster.NewHTTP(muster.HTTPConfig{
		BaseURL:     hubSrv.URL,
		StaticToken: "assay",
	})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for i := 0; i < 3; i++ {
		st, err := assayClient.Lease(context.Background())
		if err != nil || st == nil {
			t.Fatalf("assay lease %d: st=%+v err=%v", i, st, err)
		}
		got[st.ID] = true
	}
	for _, want := range []string{"PA-0010", "PA-0011", "PA-0012"} {
		if !got[want] {
			t.Fatalf("expected %s to reach observed for assay, got %v", want, got)
		}
	}
}

func TestWorkerDropsExpiredSpoolLeaseAndQuarantinesDisallowedMove(t *testing.T) {
	hub := muster.NewStandIn()
	hubSrv := httptest.NewServer(hub)
	defer hubSrv.Close()

	musterClient, err := muster.NewHTTP(muster.HTTPConfig{
		BaseURL:     hubSrv.URL,
		StaticToken: "sextant",
	})
	if err != nil {
		t.Fatal(err)
	}
	reg, err := tools.Load()
	if err != nil {
		t.Fatal(err)
	}
	spoolDir := t.TempDir()
	sp, err := spool.Open(spoolDir)
	if err != nil {
		t.Fatal(err)
	}

	// Stage a stale lease entry that Muster no longer holds -> 409 lease_not_held -> dropped cleanly.
	if _, err := sp.Stage(spool.Entry{
		Lease:   "PA-9999.1.1",
		ItemID:  "PA-9999",
		Outcome: spool.OutcomeNeedsSubtool,
		Code:    errcode.NeedsSubtool,
		Reason:  errcode.NoMatchingSubTool,
	}); err != nil {
		t.Fatal(err)
	}

	targetSrv := httptest.NewTLSServer(http.NotFoundHandler())
	defer targetSrv.Close()

	w, err := New(Config{
		Muster:   musterClient,
		Fetcher:  newTestFetcher(t, targetSrv, nil, time.Second),
		Registry: reg,
		Spool:    sp,
		Drain:    true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Run(context.Background()); err != nil {
		t.Fatalf("Run with stale lease: %v", err)
	}
	pending, err := sp.Pending()
	if err != nil || len(pending) != 0 {
		t.Fatalf("expected stale lease entry removed, got %+v", pending)
	}
}

func TestWorkerPausesAnItemWhoseCitationsHaveNoRow(t *testing.T) {
	govHTML := mustReadFixture(t, "gov-portal/fixtures/observed-calculator/page.html")
	targetSrv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(govHTML)
	}))
	defer targetSrv.Close()

	hub := muster.NewStandIn()
	hub.Seed([]muster.SeedItem{{
		ID: "PA-0001", State: "recorded",
		Found: &muster.Found{Source: "sam-gov", Title: "Gov Calc", Address: "https://calc.example.gov/tool"},
	}})
	hubSrv := httptest.NewServer(hub)
	defer hubSrv.Close()
	client, err := muster.NewHTTP(muster.HTTPConfig{BaseURL: hubSrv.URL, StaticToken: "sextant"})
	if err != nil {
		t.Fatal(err)
	}
	reg, err := tools.Load()
	if err != nil {
		t.Fatal(err)
	}
	sp, err := spool.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var logBuf bytes.Buffer
	w, err := New(Config{Muster: client, Fetcher: newTestFetcher(t, targetSrv, nil, 5*time.Second), Registry: reg, Spool: sp, Log: &logBuf, Drain: true})
	if err != nil {
		t.Fatal(err)
	}
	if worked, err := w.StepOnce(context.Background()); err != nil || !worked {
		t.Fatalf("StepOnce: worked=%v err=%v", worked, err)
	}
	if !strings.Contains(logBuf.String(), string(derive.NoCitationRow)) {
		t.Errorf("log does not name the pause reason: %s", logBuf.String())
	}
	assay, err := muster.NewHTTP(muster.HTTPConfig{BaseURL: hubSrv.URL, StaticToken: "assay"})
	if err != nil {
		t.Fatal(err)
	}
	if st, err := assay.Lease(context.Background()); err != nil || st != nil {
		t.Fatalf("a paused item must not reach Assay: step=%+v err=%v", st, err)
	}
}

// leaseCounter is a stand-in Muster that offers the same item every time, as
// contract v1 does for a failed step. The path of the address follows paths,
// one entry per lease, the last one repeating.
type leaseCounter struct {
	leases, fails int
	cancel        context.CancelFunc
	maxLeases     int
	paths         []string
}

func (c *leaseCounter) Lease(context.Context) (*muster.Step, error) {
	c.leases++
	if c.leases >= c.maxLeases {
		c.cancel() // a runaway loop ends here, so the test cannot hang
	}
	path := c.paths[min(c.leases, len(c.paths))-1]
	return &muster.Step{
		Lease: "PA-0001.1.1", Kind: "item", ID: "PA-0001", State: "recorded", Lane: "normal", Attempt: 1,
		Expires: "2026-01-15T10:00:00Z",
		Found:   &muster.Found{Address: "https://calc.example.gov/" + path},
	}, nil
}
func (c *leaseCounter) Heartbeat(context.Context, string) (*muster.HeartbeatReply, error) {
	return &muster.HeartbeatReply{}, nil
}
func (c *leaseCounter) Complete(context.Context, string, string, any) (*muster.CompleteReply, error) {
	return nil, errors.New("unexpected complete")
}
func (c *leaseCounter) Fail(context.Context, string, errcode.Code) (*muster.FailReply, error) {
	c.fails++
	return &muster.FailReply{}, nil
}
func (c *leaseCounter) NeedsSubtool(context.Context, string) (*muster.PauseReply, error) {
	return &muster.PauseReply{}, nil
}

// runWorkerOn runs a worker against the stand-in on a virtual clock and
// returns the sleeps it asked for. The path "bad" makes the fetch time out (a
// failed step); the path "pause" gives a page no sub-tool matches (a paused
// step, not a failed one). The run ends at the stopAfter-th sleep.
func runWorkerOn(t *testing.T, paths []string, stopAfter int) (leases int, sleeps []time.Duration) {
	t.Helper()
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/robots.txt":
			http.NotFound(w, r)
		case "/pause":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte("<!doctype html><html><body><h1>Static notice</h1></body></html>"))
		default:
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer target.Close()
	reg, err := tools.Load()
	if err != nil {
		t.Fatal(err)
	}
	sp, err := spool.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &leaseCounter{cancel: cancel, maxLeases: 500, paths: paths}
	w, err := New(Config{Muster: c, Fetcher: newTestFetcher(t, target, nil, time.Second), Registry: reg, Spool: sp,
		PollInterval: 2 * time.Second, HeartbeatInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	w.sleep = func(_ context.Context, d time.Duration) bool {
		sleeps = append(sleeps, d)
		return len(sleeps) < stopAfter
	}
	if err := w.Run(ctx); err != nil {
		t.Fatal(err)
	}
	return c.leases, sleeps
}

func seconds(n ...int) []time.Duration {
	out := make([]time.Duration, len(n))
	for i, v := range n {
		out[i] = time.Duration(v) * time.Second
	}
	return out
}

// With a 2 s poll interval the pauses are 1x, 2x, 4x, 8x, 16x, 32x and then
// 32x again: doubled for each failure in a row, and no more than 32x.
func TestBackoffDoublesToAFixedCapAndStaysThere(t *testing.T) {
	leases, sleeps := runWorkerOn(t, []string{"bad"}, 8)
	want := seconds(2, 4, 8, 16, 32, 64, 64, 64)
	if fmt.Sprint(sleeps) != fmt.Sprint(want) {
		t.Fatalf("sleeps = %v, want %v", sleeps, want)
	}
	if leases != 8 {
		t.Fatalf("%d lease calls for 8 pauses", leases)
	}
	l2, s2 := runWorkerOn(t, []string{"bad"}, 8)
	if l2 != leases || fmt.Sprint(s2) != fmt.Sprint(sleeps) {
		t.Fatal("backoff is not deterministic")
	}
}

// A step that is not a failure ends the streak: the next failure waits 1x.
func TestBackoffResetsAfterAStepThatDidNotFail(t *testing.T) {
	_, sleeps := runWorkerOn(t, []string{"bad", "bad", "bad", "pause", "bad"}, 4)
	want := seconds(2, 4, 8, 2) // the paused step adds no pause, the next failure starts again
	if fmt.Sprint(sleeps) != fmt.Sprint(want) {
		t.Fatalf("sleeps = %v, want %v", sleeps, want)
	}
}

// TestWorkerObservesAPageOnTheLocalTestOrigin is the test-run path: one local
// http origin is fetched, and the item still reaches "observed".
func TestWorkerObservesAPageOnTheLocalTestOrigin(t *testing.T) {
	page := mustReadFixture(t, "saas-marketing/fixtures/observed-free-rate-tool/page.html")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(page)
	}))
	defer srv.Close()

	hub := muster.NewStandIn()
	hub.Seed([]muster.SeedItem{{ID: "PA-0001", State: "recorded", Found: &muster.Found{Source: "web-sweep", Title: "Local", Address: srv.URL + "/pricing"}}})
	hubSrv := httptest.NewServer(hub)
	defer hubSrv.Close()
	client, err := muster.NewHTTP(muster.HTTPConfig{BaseURL: hubSrv.URL, StaticToken: "sextant"})
	if err != nil {
		t.Fatal(err)
	}
	reg, err := tools.Load()
	if err != nil {
		t.Fatal(err)
	}
	sp, err := spool.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w, err := New(Config{
		Muster: client, Fetcher: fetch.New(fetch.Config{Timeout: 5 * time.Second, LocalTestOrigin: srv.URL}),
		Registry: reg, Tables: testTables(), Spool: sp, Drain: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if worked, err := w.StepOnce(context.Background()); err != nil || !worked {
		t.Fatalf("StepOnce: worked=%v err=%v", worked, err)
	}
	assay, err := muster.NewHTTP(muster.HTTPConfig{BaseURL: hubSrv.URL, StaticToken: "assay"})
	if err != nil {
		t.Fatal(err)
	}
	st, err := assay.Lease(context.Background())
	if err != nil || st == nil || st.ID != "PA-0001" || st.State != "observed" {
		t.Fatalf("the local page must reach observed: step=%+v err=%v", st, err)
	}
	if strings.Contains(string(st.Results["sextant"]), "127.0.0.1") {
		t.Fatal("the address must not reach the result")
	}
}

func TestOnlyTheLocalTestOriginIsRewrittenToHTTPS(t *testing.T) {
	local := fetch.New(fetch.Config{LocalTestOrigin: "http://127.0.0.1:8080"})
	parse := func(raw string) *url.URL {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	if got := observationURL(local, parse("http://127.0.0.1:8080/page?x=1")); got.String() != "https://127.0.0.1:8080/page?x=1" {
		t.Errorf("the local origin must be rewritten, got %s", got)
	}
	for _, raw := range []string{"http://example.com/x", "http://127.0.0.1:9090/page", "http://[::1]:8080/page", "https://example.com/x"} {
		if got := observationURL(local, parse(raw)); got.String() != raw {
			t.Errorf("%s must not be rewritten, got %s", raw, got)
		}
	}
	// With no local origin configured nothing is rewritten at all.
	if got := observationURL(fetch.New(fetch.Config{}), parse("http://127.0.0.1:8080/page")); got.Scheme != "http" {
		t.Errorf("rewritten without a local origin: %s", got)
	}
}
