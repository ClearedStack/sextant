package fetch

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ClearedStack/sextant/internal/errcode"
)

func TestParseLocalTestOrigin(t *testing.T) {
	good := []string{"http://127.0.0.1:8080", "http://127.0.0.1:1", "http://127.0.0.1:65535", "http://[::1]:9000"}
	for _, s := range good {
		if got, ok := ParseLocalTestOrigin(s); !ok || got != s {
			t.Errorf("ParseLocalTestOrigin(%q) = %q, %v; want it accepted", s, got, ok)
		}
	}
	bad := []string{
		"", " ", "http://127.0.0.1", "http://127.0.0.1:", "http://127.0.0.1:0", "http://127.0.0.1:65536",
		"http://127.0.0.1:08080", "http://127.0.0.1:8080/", "http://127.0.0.1:8080/x", "http://127.0.0.1:8080?x=1",
		"http://127.0.0.1:8080#f", " http://127.0.0.1:8080", "http://127.0.0.1:8080 ", "http://127.0.0.1:8080\n",
		"https://127.0.0.1:8080", "HTTP://127.0.0.1:8080", "http://localhost:8080", "http://127.0.0.2:8080",
		"http://127.1:8080", "http://0.0.0.0:8080", "http://10.0.0.1:8080", "http://[::1]", "http://[::2]:8080",
		"http://[0:0:0:0:0:0:0:1]:8080", "http://user@127.0.0.1:8080", "http://127.0.0.1:8080@evil.example:80",
		"http://127.0.0.1:80a", "http://127.0.0.1:+80", "http://127.0.0.1:8080,http://127.0.0.1:8081",
		"http://example.com:8080", "//127.0.0.1:8080", "127.0.0.1:8080",
	}
	for _, s := range bad {
		if got, ok := ParseLocalTestOrigin(s); ok {
			t.Errorf("ParseLocalTestOrigin(%q) = %q; want it refused", s, got)
		}
	}
}

func FuzzParseLocalTestOrigin(f *testing.F) {
	for _, s := range []string{"http://127.0.0.1:8080", "http://[::1]:9", "http://127.0.0.1:0", "https://127.0.0.1:8080", "http://127.0.0.1:8080/", ""} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		got, ok := ParseLocalTestOrigin(s)
		if !ok {
			if got != "" {
				t.Fatalf("refused value returned %q", got)
			}
			return
		}
		if got != s {
			t.Fatalf("accepted %q but returned %q", s, got)
		}
		u, err := url.Parse(s)
		if err != nil || u.Scheme != "http" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
			t.Fatalf("accepted %q which is not a bare origin", s)
		}
		if h := u.Hostname(); h != "127.0.0.1" && h != "::1" {
			t.Fatalf("accepted %q with host %q", s, h)
		}
		if _, port, err := net.SplitHostPort(u.Host); err != nil || port == "" || port[0] == '0' || len(port) > 5 {
			t.Fatalf("accepted %q with port %q", s, port)
		}
	})
}

// localServer serves a page, a redirect to elsewhere and a redirect inside
// the origin on 127.0.0.1.
func localServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/robots.txt":
			http.NotFound(w, r)
		case "/page":
			w.Write([]byte("<!DOCTYPE html><html><body><table><tr><td>x</td></tr></table></body></html>"))
		case "/inside":
			http.Redirect(w, r, "/page", http.StatusFound)
		case "/away":
			http.Redirect(w, r, "https://example.com/page", http.StatusFound)
		case "/other-port":
			http.Redirect(w, r, "http://127.0.0.1:1/page", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func wantForbidden(t *testing.T, err error) {
	t.Helper()
	if coded, ok := errcode.From(err); !ok || coded.Code != errcode.Parse || coded.Reason != errcode.ForbiddenTargetHost {
		t.Errorf("got %v, want E_PARSE target_address_is_not_public_https", err)
	}
}

func TestLocalTestOriginIsFetchedAndNothingElseIs(t *testing.T) {
	srv := localServer(t)
	f := New(Config{Timeout: 2 * time.Second, LocalTestOrigin: srv.URL})

	res, err := f.Fetch(context.Background(), srv.URL+"/page")
	if err != nil || !strings.Contains(string(res.Body), "<table>") {
		t.Fatalf("Fetch(local page) = %v, %v", res, err)
	}
	if res, err := f.Fetch(context.Background(), srv.URL+"/inside"); err != nil || res.FinalURL.Path != "/page" {
		t.Fatalf("a redirect inside the origin must work: %v, %v", res, err)
	}

	// Redirects away from the origin, or to another port, are refused.
	for _, p := range []string{"/away", "/other-port"} {
		_, err := f.Fetch(context.Background(), srv.URL+p)
		wantForbidden(t, err)
	}
	// Everything else keeps the public-https checks.
	for _, raw := range []string{
		"http://127.0.0.1:1/page", "https://127.0.0.1:1/page", "http://[::1]:" + strings.TrimPrefix(srv.URL, "http://127.0.0.1:") + "/page",
		"http://localhost" + strings.TrimPrefix(srv.URL, "http://127.0.0.1") + "/page", "https://10.0.0.1/x", "http://example.com/x",
	} {
		_, err := f.Fetch(context.Background(), raw)
		wantForbidden(t, err)
	}
}

func TestWithoutLocalTestOriginLocalhostIsRefused(t *testing.T) {
	srv := localServer(t)
	_, err := New(Config{Timeout: time.Second}).Fetch(context.Background(), srv.URL+"/page")
	wantForbidden(t, err)
	// A bad value turns nothing on.
	_, err = New(Config{Timeout: time.Second, LocalTestOrigin: srv.URL + "/"}).Fetch(context.Background(), srv.URL+"/page")
	wantForbidden(t, err)
}

func TestIsLocalTestOriginIsExact(t *testing.T) {
	f := New(Config{LocalTestOrigin: "http://127.0.0.1:8080"})
	for raw, want := range map[string]bool{
		"http://127.0.0.1:8080/x": true, "http://127.0.0.1:8080": true,
		"https://127.0.0.1:8080/x": false, "http://127.0.0.1:8081/x": false, "http://localhost:8080/x": false,
		"http://u@127.0.0.1:8080/x": false, "http://example.com/x": false,
	} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if got := f.IsLocalTestOrigin(u); got != want {
			t.Errorf("IsLocalTestOrigin(%s) = %v, want %v", raw, got, want)
		}
	}
	if f.IsLocalTestOrigin(nil) || New(Config{}).IsLocalTestOrigin(&url.URL{Scheme: "http", Host: "127.0.0.1:8080"}) {
		t.Error("nil, or no configured origin, must be false")
	}
}

func TestARedirectIntoTheLocalOriginFromElsewhereIsRefused(t *testing.T) {
	srv := localServer(t)
	f := New(Config{Timeout: time.Second, LocalTestOrigin: srv.URL})
	from, _ := url.Parse("https://example.com/start")
	to, _ := url.Parse(srv.URL + "/page")
	req := &http.Request{URL: to}
	wantForbidden(t, f.client.CheckRedirect(req, []*http.Request{{URL: from}}))
}
