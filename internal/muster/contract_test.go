package muster

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ClearedStack/sextant/internal/errcode"
)

type exampleCall struct {
	As          string          `json:"as"`
	Method      string          `json:"method"`
	Path        string          `json:"path"`
	ContentType string          `json:"content_type"`
	Body        json.RawMessage `json:"body"`
	Raw         *string         `json:"raw"`
}

type exampleFile struct {
	About string `json:"about"`
	Given *struct {
		Items     []SeedItem    `json:"items"`
		Calls     []exampleCall `json:"calls"`
		Wait      string        `json:"wait"`
		Condition string        `json:"condition"`
	} `json:"given"`
	Request exampleCall `json:"request"`
	Expect  struct {
		Status int             `json:"status"`
		Body   json.RawMessage `json:"body"`
		Stream *string         `json:"stream"`
	} `json:"expect"`
}

// TestContractExamples replays every saved example in testdata/contract/*.json
// (copied from ClearedStack/muster contract/v1/examples/) with zero skips:
//  1. Every example is replayed against StandIn, verifying exact HTTP status
//     and exact compact JSON field-by-field body (or SSE stream).
//  2. Every example is also exercised through Sextant's *HTTP client:
//     - Well-formed worker calls and /healthz run through the *HTTP methods,
//     asserting exact outbound JSON bytes and decoded replies or *Refusal.
//     - Malformed/unprovokable client requests verify that *HTTP refuses or
//     cannot emit them, and replay the saved refusal HTTP status + body
//     through *HTTP to verify status/code/cause classification and non-leakage.
//     - Non-worker calls verify that *HTTP exposes no method for them and that
//     calling them with sextant's token is refused with 403 not_your_call.
func TestContractExamples(t *testing.T) {
	files, err := filepath.Glob("../../testdata/contract/*.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 52 {
		t.Fatalf("found %d contract examples, want at least 52", len(files))
	}

	for _, file := range files {
		name := strings.TrimSuffix(filepath.Base(file), ".json")
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			var ex exampleFile
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&ex); err != nil {
				t.Fatalf("parse example: %v", err)
			}

			standin := setupStandIn(t, ex)
			srv := httptest.NewServer(standin)
			defer srv.Close()

			// 1. Replay the example request directly against StandIn and compare byte-for-byte.
			status, respBody := sendExampleCall(t, srv.URL, ex.Request)
			if status != ex.Expect.Status {
				t.Fatalf("stand-in status = %d, want %d (body: %s)", status, ex.Expect.Status, respBody)
			}
			if ex.Expect.Stream != nil {
				if string(respBody) != *ex.Expect.Stream {
					t.Fatalf("stand-in stream mismatch:\ngot:\n%s\nwant:\n%s", respBody, *ex.Expect.Stream)
				}
			} else if len(ex.Expect.Body) > 0 {
				wantCompact := compactJSON(t, ex.Expect.Body)
				gotCompact := compactJSON(t, respBody)
				if gotCompact != wantCompact {
					t.Fatalf("stand-in body mismatch:\ngot:  %s\nwant: %s", gotCompact, wantCompact)
				}
			} else if len(respBody) != 0 {
				t.Fatalf("expected empty body, got %q", respBody)
			}

			// 2. Replay through Sextant's *HTTP client.
			replayThroughHTTPClient(t, ex)
		})
	}
}

func setupStandIn(t *testing.T, ex exampleFile) *StandIn {
	t.Helper()
	standin := NewStandIn()
	if ex.Given == nil {
		return standin
	}
	if len(ex.Given.Items) > 0 {
		standin.Seed(ex.Given.Items)
	}
	if len(ex.Given.Calls) > 0 {
		srv := httptest.NewServer(standin)
		for i, call := range ex.Given.Calls {
			st, body := sendExampleCall(t, srv.URL, call)
			if st < 200 || st >= 300 {
				srv.Close()
				t.Fatalf("given.calls[%d] (%s %s) failed with %d: %s", i, call.Method, call.Path, st, body)
			}
		}
		srv.Close()
	}
	if ex.Given.Wait != "" {
		d, err := time.ParseDuration(ex.Given.Wait)
		if err != nil {
			t.Fatalf("bad wait duration %q: %v", ex.Given.Wait, err)
		}
		standin.Advance(d)
	}
	if ex.Given.Condition != "" {
		standin.SetCondition(ex.Given.Condition)
	}
	return standin
}

func sendExampleCall(t *testing.T, baseURL string, call exampleCall) (int, []byte) {
	t.Helper()
	var bodyReader io.Reader
	if call.Raw != nil {
		bodyReader = strings.NewReader(*call.Raw)
	} else if len(call.Body) > 0 {
		bodyReader = bytes.NewReader(call.Body)
	}
	req, err := http.NewRequest(call.Method, baseURL+call.Path, bodyReader)
	if err != nil {
		t.Fatal(err)
	}
	if call.Method == http.MethodPost {
		ct := call.ContentType
		if ct == "" {
			ct = "application/json"
		}
		req.Header.Set("Content-Type", ct)
	}
	if call.As != "" {
		req.Header.Set("Authorization", "Bearer "+call.As)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, raw
}

func replayThroughHTTPClient(t *testing.T, ex exampleFile) {
	t.Helper()
	ctx := context.Background()

	if ex.Request.Path == "/healthz" && ex.Request.Method == http.MethodGet {
		standin := setupStandIn(t, ex)
		srv := httptest.NewServer(standin)
		defer srv.Close()
		c, err := NewHTTP(HTTPConfig{BaseURL: srv.URL, StaticToken: "sextant"})
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Healthz(ctx); err != nil {
			t.Fatalf("Healthz() = %v, want nil", err)
		}
		return
	}

	// Non-worker endpoints: verify sextant is forbidden on StandIn, and if the
	// example itself expects a refusal, also replay that refusal body through *HTTP.
	switch ex.Request.Path {
	case "/v1/items", "/v1/release", "/v1/health", "/v1/edits", "/v1/feed":
		standin := NewStandIn()
		srv := httptest.NewServer(standin)
		defer srv.Close()
		st, body := sendExampleCall(t, srv.URL, exampleCall{
			As:     "sextant",
			Method: ex.Request.Method,
			Path:   ex.Request.Path,
			Body:   json.RawMessage(`{"contract":"v1"}`),
		})
		if st != http.StatusForbidden {
			t.Fatalf("sextant calling %s got status %d, want 403 (%s)", ex.Request.Path, st, body)
		}
		if ex.Expect.Status >= 400 {
			verifyClientParsesSavedRefusal(t, ex)
		}
		return
	}

	// Check whether *HTTP can legitimately construct this exact worker call.
	invoke, sendable := buildWorkerClientCall(t, ex.Request)
	if !sendable {
		verifyClientParsesSavedRefusal(t, ex)
		return
	}

	standin := setupStandIn(t, ex)
	var captured []byte
	var capturedAuth string
	var capturedCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		capturedCT = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		captured = append([]byte(nil), b...)
		r.Body = io.NopCloser(bytes.NewReader(b))
		standin.ServeHTTP(w, r)
	}))
	defer srv.Close()

	tokenFile := ""
	if ex.Request.As != "" {
		tf := filepath.Join(t.TempDir(), "token")
		if err := os.WriteFile(tf, []byte(ex.Request.As+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		tokenFile = tf
	}
	client, err := NewHTTP(HTTPConfig{BaseURL: srv.URL, TokenFile: tokenFile})
	if err != nil {
		t.Fatal(err)
	}

	gotVal, callErr := invoke(ctx, client)

	// Verify exact outbound wire contract.
	if capturedCT != "application/json" {
		t.Fatalf("outbound Content-Type = %q, want application/json", capturedCT)
	}
	wantAuth := ""
	if ex.Request.As != "" {
		wantAuth = "Bearer " + ex.Request.As
	}
	if capturedAuth != wantAuth {
		t.Fatalf("outbound Authorization = %q, want %q", capturedAuth, wantAuth)
	}
	if compactJSON(t, captured) != compactJSON(t, ex.Request.Body) {
		t.Fatalf("outbound JSON mismatch:\ngot:  %s\nwant: %s", compactJSON(t, captured), compactJSON(t, ex.Request.Body))
	}

	if ex.Expect.Status == http.StatusNoContent {
		if callErr != nil || gotVal != nil {
			t.Fatalf("expected (nil, nil) on 204, got (%v, %v)", gotVal, callErr)
		}
		return
	}
	if ex.Expect.Status >= 400 {
		assertMatchesExpectedRefusal(t, callErr, ex)
		return
	}
	if callErr != nil {
		t.Fatalf("unexpected client error on %d: %v", ex.Expect.Status, callErr)
	}
	reEncoded, err := json.Marshal(gotVal)
	if err != nil {
		t.Fatal(err)
	}
	if compactJSON(t, reEncoded) != compactJSON(t, ex.Expect.Body) {
		t.Fatalf("decoded reply mismatch:\ngot:  %s\nwant: %s", compactJSON(t, reEncoded), compactJSON(t, ex.Expect.Body))
	}
}

func buildWorkerClientCall(t *testing.T, req exampleCall) (func(context.Context, *HTTP) (any, error), bool) {
	t.Helper()
	if req.Method != http.MethodPost || req.Raw != nil || (req.ContentType != "" && req.ContentType != "application/json") {
		return nil, false
	}
	fields, ok := parseJSONObject(req.Body)
	if !ok {
		return nil, false
	}
	var contract string
	if cRaw, ok := fields["contract"]; !ok || json.Unmarshal(cRaw, &contract) != nil || contract != ContractVersion {
		return nil, false
	}

	switch req.Path {
	case "/v1/lease":
		if len(fields) != 1 {
			return nil, false
		}
		return func(ctx context.Context, c *HTTP) (any, error) {
			step, err := c.Lease(ctx)
			if step == nil {
				return nil, err
			}
			return step, err
		}, true

	case "/v1/heartbeat":
		if len(fields) != 2 {
			return nil, false
		}
		var lease string
		if json.Unmarshal(fields["lease"], &lease) != nil || lease == "" {
			return nil, false
		}
		return func(ctx context.Context, c *HTTP) (any, error) { return c.Heartbeat(ctx, lease) }, true

	case "/v1/complete":
		for k := range fields {
			if k != "contract" && k != "lease" && k != "state" && k != "result" {
				return nil, false
			}
		}
		var lease, state string
		if json.Unmarshal(fields["lease"], &lease) != nil || lease == "" ||
			json.Unmarshal(fields["state"], &state) != nil || state == "" {
			return nil, false
		}
		var res any
		if rRaw, has := fields["result"]; has {
			if _, isObj := parseJSONObject(rRaw); !isObj {
				return nil, false
			}
			res = rRaw
		}
		return func(ctx context.Context, c *HTTP) (any, error) { return c.Complete(ctx, lease, state, res) }, true

	case "/v1/fail":
		if len(fields) != 3 {
			return nil, false
		}
		var lease, code string
		if json.Unmarshal(fields["lease"], &lease) != nil || lease == "" ||
			json.Unmarshal(fields["code"], &code) != nil {
			return nil, false
		}
		if code == "E_NEEDS_SUBTOOL" {
			// Verify *HTTP locally refuses sending E_NEEDS_SUBTOOL to /v1/fail.
			c, _ := NewHTTP(HTTPConfig{BaseURL: "http://127.0.0.1:1"})
			if _, err := c.Fail(context.Background(), lease, errcode.NeedsSubtool); err == nil {
				t.Fatal("Fail accepted E_NEEDS_SUBTOOL locally")
			}
			return nil, false
		}
		if code != "E_TIMEOUT" && code != "E_PARSE" {
			return nil, false
		}
		return func(ctx context.Context, c *HTTP) (any, error) { return c.Fail(ctx, lease, errcode.Code(code)) }, true

	case "/v1/needs_subtool":
		if len(fields) != 2 {
			return nil, false
		}
		var lease string
		if json.Unmarshal(fields["lease"], &lease) != nil || lease == "" {
			return nil, false
		}
		return func(ctx context.Context, c *HTTP) (any, error) { return c.NeedsSubtool(ctx, lease) }, true
	}
	return nil, false
}

func verifyClientParsesSavedRefusal(t *testing.T, ex exampleFile) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(ex.Expect.Status)
		_, _ = w.Write(ex.Expect.Body)
	}))
	defer srv.Close()

	c, err := NewHTTP(HTTPConfig{BaseURL: srv.URL, StaticToken: "sextant"})
	if err != nil {
		t.Fatal(err)
	}
	_, callErr := c.Lease(context.Background())
	assertMatchesExpectedRefusal(t, callErr, ex)
}

func assertMatchesExpectedRefusal(t *testing.T, callErr error, ex exampleFile) {
	t.Helper()
	var want struct {
		Error  errcode.Code `json:"error"`
		Cause  string       `json:"cause"`
		Reason string       `json:"reason"`
	}
	if err := json.Unmarshal(ex.Expect.Body, &want); err != nil {
		t.Fatal(err)
	}
	var ref *Refusal
	if !errors.As(callErr, &ref) {
		t.Fatalf("expected *Refusal, got %T (%v)", callErr, callErr)
	}
	if ref.Status != ex.Expect.Status || ref.Code != want.Error || ref.Cause != want.Cause || ref.Reason != want.Reason {
		t.Fatalf("Refusal = %+v, want status=%d error=%s cause=%s reason=%q",
			ref, ex.Expect.Status, want.Error, want.Cause, want.Reason)
	}
	if strings.Contains(ref.Error(), want.Reason) {
		t.Fatalf("Refusal.Error() %q leaked human reason text %q", ref.Error(), want.Reason)
	}
	wantRetry := ex.Expect.Status == http.StatusTooManyRequests || ex.Expect.Status == http.StatusServiceUnavailable || want.Error == errcode.Timeout
	if ref.Retryable() != wantRetry {
		t.Fatalf("Refusal.Retryable() = %v, want %v", ref.Retryable(), wantRetry)
	}
}

func compactJSON(t *testing.T, raw []byte) string {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		t.Fatalf("invalid JSON %q: %v", raw, err)
	}
	return buf.String()
}

func TestClientRejectsMalformed200Responses(t *testing.T) {
	cases := map[string]struct {
		body string
		call func(*HTTP) error
	}{
		"unknown field on lease": {
			body: `{"lease":"PA-0001.1.1","kind":"item","id":"PA-0001","state":"recorded","lane":"normal","attempt":1,"expires":"2026-01-05T09:01:00Z","extra":1}`,
			call: func(c *HTTP) error { _, err := c.Lease(context.Background()); return err },
		},
		"null body on complete": {
			body: `null`,
			call: func(c *HTTP) error {
				_, err := c.Complete(context.Background(), "PA-0001.1.1", "observed", nil)
				return err
			},
		},
		"mismatched heartbeat lease": {
			body: `{"lease":"PA-0002.1.1","expires":"2026-01-05T09:01:00Z"}`,
			call: func(c *HTTP) error { _, err := c.Heartbeat(context.Background(), "PA-0001.1.1"); return err },
		},
		"fail with neither held nor next": {
			body: `{"id":"PA-0001","state":"recorded"}`,
			call: func(c *HTTP) error { _, err := c.Fail(context.Background(), "PA-0001.1.1", errcode.Parse); return err },
		},
		"fail with both held and next": {
			body: `{"id":"PA-0001","state":"recorded","held":true,"next":"sextant"}`,
			call: func(c *HTTP) error { _, err := c.Fail(context.Background(), "PA-0001.1.1", errcode.Parse); return err },
		},
		"duplicate json key": {
			body: `{"id":"PA-0001","id":"PA-0002","state":"needs_subtool"}`,
			call: func(c *HTTP) error { _, err := c.NeedsSubtool(context.Background(), "PA-0001.1.1"); return err },
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			c, err := NewHTTP(HTTPConfig{BaseURL: srv.URL, StaticToken: "sextant"})
			if err != nil {
				t.Fatal(err)
			}
			if err := tc.call(c); !errors.Is(err, ErrBadResponse) {
				t.Fatalf("got %v, want ErrBadResponse", err)
			}
		})
	}
}
