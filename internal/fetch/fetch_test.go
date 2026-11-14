package fetch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ClearedStack/sextant/internal/errcode"
	"github.com/ClearedStack/sextant/internal/subtool"
	browserjob "github.com/ClearedStack/sextant/tools/on-demand-browser-job"
)

type stubBrowser struct {
	cap *browserjob.Capture
	err error
}

func (s stubBrowser) Render(_ context.Context, _ *url.URL) (*browserjob.Capture, error) {
	return s.cap, s.err
}

func TestFetchRespectsRobotsPaywallAntiBotSizeAndBrowserJob(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/robots.txt":
			w.Write([]byte("User-agent: sextant\nDisallow: /disallowed\n"))
		case "/ok":
			w.Write([]byte("<!DOCTYPE html><html><body><form><input name='a'></form></body></html>"))
		case "/disallowed":
			t.Error("fetcher requested a path disallowed by robots.txt")
			w.WriteHeader(http.StatusOK)
		case "/paywall":
			w.WriteHeader(http.StatusPaymentRequired)
		case "/captcha":
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte("<!DOCTYPE html><html><body><div class='cf-turnstile'></div></body></html>"))
		case "/slow":
			time.Sleep(150 * time.Millisecond)
			w.WriteHeader(http.StatusOK)
		case "/huge":
			w.Write(make([]byte, subtool.MaxPageBytes+16))
		case "/spa":
			w.Write([]byte("<!DOCTYPE html><html><body><div id='app'></div><script src='/app.js'></script></body></html>"))
		case "/http-redirect":
			http.Redirect(w, r, "http://example.com/insecure", http.StatusFound)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	base := New(Config{
		Timeout:              80 * time.Millisecond,
		Transport:            srv.Client().Transport,
		AllowPrivateNetworks: true,
	})

	res, err := base.Fetch(context.Background(), srv.URL+"/ok")
	if err != nil || len(res.Body) == 0 {
		t.Fatalf("Fetch(/ok) = %v, %v", res, err)
	}
	res.Wipe()
	if res.Body != nil {
		t.Fatal("Wipe did not clear Body")
	}

	expectCode := func(path string, f *Fetcher, code errcode.Code, reason errcode.Reason) {
		t.Helper()
		_, err := f.Fetch(context.Background(), srv.URL+path)
		coded, ok := errcode.From(err)
		if !ok || coded.Code != code || coded.Reason != reason {
			t.Errorf("%s: got %v, want %s:%s", path, err, code, reason)
		}
	}

	expectCode("/disallowed", base, errcode.NeedsSubtool, errcode.BlockedByRobots)
	expectCode("/paywall", base, errcode.NeedsSubtool, errcode.PaywallOrAuthStatus)
	expectCode("/captcha", base, errcode.NeedsSubtool, errcode.AntiBotGate)
	expectCode("/slow", base, errcode.Timeout, errcode.DeadlineExceeded)
	expectCode("/huge", base, errcode.NeedsSubtool, errcode.PageLargerThanBudget)
	expectCode("/spa", base, errcode.NeedsSubtool, errcode.BrowserJobUnavailable)
	expectCode("/http-redirect", base, errcode.Parse, errcode.ForbiddenTargetHost)

	withBrowser := New(Config{
		Timeout:              time.Second,
		Transport:            srv.Client().Transport,
		AllowPrivateNetworks: true,
		Browser: stubBrowser{cap: &browserjob.Capture{
			RenderedHTML: "<!DOCTYPE html><html><body><form><input name='x'></form></body></html>",
		}},
	})
	spaRes, err := withBrowser.Fetch(context.Background(), srv.URL+"/spa")
	if err != nil || !strings.Contains(string(spaRes.Body), browserjob.CaptureFormat) {
		t.Fatalf("Fetch(/spa) with browser = %v, %v", spaRes, err)
	}
}

func TestSSRFProtectionBlocksClusterAndPrivateHosts(t *testing.T) {
	f := New(Config{Timeout: time.Second})
	blocked := []string{
		"http://example.gov/plain",
		"https://user:pass@example.gov/secret",
		"https://localhost/admin",
		"https://muster.clearedstack.svc.cluster.local:8080/v1/lease",
		"https://logbook.clearedstack.svc/db",
		"https://127.0.0.1/loop",
		"https://10.42.0.1/pod",
		"https://169.254.169.254/latest/meta-data",
		"https://100.64.1.2/cgnat",
	}
	for _, raw := range blocked {
		_, err := f.Fetch(context.Background(), raw)
		coded, ok := errcode.From(err)
		if !ok || coded.Code != errcode.Parse || coded.Reason != errcode.ForbiddenTargetHost {
			t.Errorf("Fetch(%q) = %v, want E_PARSE:target_address_is_not_public_https", raw, err)
		}
	}

	if !isNonPublicIP(netip.MustParseAddr("192.168.1.10")) {
		t.Fatal("192.168.1.10 must be non-public")
	}
	if isNonPublicIP(netip.MustParseAddr("93.184.216.34")) {
		t.Fatal("93.184.216.34 must be public")
	}
}
