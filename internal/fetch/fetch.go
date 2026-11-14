// Package fetch performs passive public HTTPS fetches for Sextant, enforcing
// robots.txt, private-network SSRF protections, size budgets, anti-bot/paywall
// stop conditions, and optional on-demand browser Job delegation for SPA shells.
package fetch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/ClearedStack/sextant/internal/errcode"
	"github.com/ClearedStack/sextant/internal/observation"
	"github.com/ClearedStack/sextant/internal/robots"
	"github.com/ClearedStack/sextant/internal/subtool"
	browserjob "github.com/ClearedStack/sextant/tools/on-demand-browser-job"
)

const (
	defaultUserAgent = "Sextant/0.1 (+https://clearedstack.com/platform/sextant/)"
	maxRedirects     = 5
	maxRobotsBytes   = 512 << 10
)

// BrowserRunner executes an ephemeral headless browser Job for a JavaScript
// single-page application and returns the captured DOM and network log.
type BrowserRunner interface {
	Render(ctx context.Context, target *url.URL) (*browserjob.Capture, error)
}

// Result holds the fetched target URL (after any safe HTTPS redirects) and the
// raw document or browser-capture bytes to pass to tools.Registry.Observe.
type Result struct {
	FinalURL *url.URL
	Body     []byte
}

// Wipe overwrites Body in place so raw vendor HTML or PDFs do not linger in
// heap slices after deterministic fact extraction finishes.
func (r *Result) Wipe() {
	if r == nil {
		return
	}
	clear(r.Body)
	r.Body = nil
}

// Config configures a passive Fetcher.
type Config struct {
	Timeout              time.Duration
	Transport            http.RoundTripper
	Browser              BrowserRunner
	AllowPrivateNetworks bool // for local httptest servers in unit tests only
}

// Fetcher fetches public HTTPS pages passively.
type Fetcher struct {
	timeout      time.Duration
	client       *http.Client
	browser      BrowserRunner
	allowPrivate bool
}

// New builds a passive HTTPS Fetcher.
func New(cfg Config) *Fetcher {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	f := &Fetcher{
		timeout:      timeout,
		browser:      cfg.Browser,
		allowPrivate: cfg.AllowPrivateNetworks,
	}
	rt := cfg.Transport
	if rt == nil {
		dialer := &net.Dialer{Timeout: 10 * time.Second}
		rt = &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          4,
			IdleConnTimeout:       30 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: timeout,
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				if !f.allowPrivate {
					if err := checkDialAddress(ctx, addr); err != nil {
						return nil, err
					}
				}
				return dialer.DialContext(ctx, network, addr)
			},
		}
	}
	f.client = &http.Client{
		Transport: rt,
		Timeout:   timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return errcode.New(errcode.NeedsSubtool, errcode.RedirectLimitExceeded)
			}
			return f.validateTargetURL(req.URL)
		},
	}
	return f
}

// Fetch checks robots.txt and passively GETs rawURL over HTTPS.
func (f *Fetcher) Fetch(ctx context.Context, rawURL string) (*Result, error) {
	if len(rawURL) == 0 || len(rawURL) > observation.MaxAddressBytes || strings.ContainsAny(rawURL, " \t\r\n") {
		return nil, errcode.New(errcode.Parse, errcode.ForbiddenTargetHost)
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, errcode.New(errcode.Parse, errcode.ForbiddenTargetHost)
	}
	u.Fragment, u.RawFragment = "", ""
	if err := f.validateTargetURL(u); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithTimeout(ctx, f.timeout)
	defer cancel()

	if err := f.checkRobots(ctx, u); err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, errcode.New(errcode.Parse, errcode.ForbiddenTargetHost)
	}
	req.Header.Set("User-Agent", defaultUserAgent)
	req.Header.Set("Accept", "text/html,application/pdf;q=0.9,*/*;q=0.1")

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, classifyTransportError(err)
	}
	defer resp.Body.Close()

	finalURL := resp.Request.URL
	if err := f.validateTargetURL(finalURL); err != nil {
		return nil, err
	}
	if finalURL.Host != u.Host {
		if err := f.checkRobots(ctx, finalURL); err != nil {
			return nil, err
		}
	}

	if hasChallengeHeader(resp.Header) {
		return nil, errcode.New(errcode.NeedsSubtool, errcode.AntiBotGate)
	}

	switch {
	case resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return nil, errcode.New(errcode.Timeout, errcode.UpstreamUnavailable)
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusPaymentRequired || resp.StatusCode == http.StatusProxyAuthRequired:
		return nil, errcode.New(errcode.NeedsSubtool, errcode.PaywallOrAuthStatus)
	case resp.StatusCode == http.StatusForbidden:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		clearBody := isAntiBotHTML(body)
		clear(body)
		if clearBody {
			return nil, errcode.New(errcode.NeedsSubtool, errcode.AntiBotGate)
		}
		return nil, errcode.New(errcode.NeedsSubtool, errcode.PaywallOrAuthStatus)
	case resp.StatusCode != http.StatusOK:
		return nil, errcode.New(errcode.Parse, errcode.EmptyPage)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, subtool.MaxPageBytes+1))
	if err != nil {
		clear(body)
		return nil, classifyTransportError(err)
	}
	if len(body) > subtool.MaxPageBytes {
		clear(body)
		return nil, errcode.New(errcode.NeedsSubtool, errcode.PageLargerThanBudget)
	}

	if isAntiBotHTML(body) {
		clear(body)
		return nil, errcode.New(errcode.NeedsSubtool, errcode.AntiBotGate)
	}

	if browserjob.NeedsBrowserJob(body) {
		clear(body)
		if f.browser == nil {
			return nil, errcode.New(errcode.NeedsSubtool, errcode.BrowserJobUnavailable)
		}
		capDoc, err := f.browser.Render(ctx, finalURL)
		if err != nil {
			if coded, ok := errcode.From(err); ok {
				return nil, coded
			}
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
				return nil, errcode.New(errcode.Timeout, errcode.BrowserJobTimedOut)
			}
			return nil, errcode.New(errcode.Parse, errcode.BrokenBrowserCapture)
		}
		if capDoc == nil {
			return nil, errcode.New(errcode.Parse, errcode.BrokenBrowserCapture)
		}
		if capDoc.Format == "" {
			capDoc.Format = browserjob.CaptureFormat
		}
		if capDoc.NetworkRequests == nil {
			capDoc.NetworkRequests = []browserjob.NetworkRequest{}
		}
		encoded, err := json.Marshal(capDoc)
		if err != nil {
			return nil, errcode.New(errcode.Parse, errcode.BrokenBrowserCapture)
		}
		if len(encoded) > subtool.MaxPageBytes {
			clear(encoded)
			return nil, errcode.New(errcode.NeedsSubtool, errcode.PageLargerThanBudget)
		}
		return &Result{FinalURL: finalURL, Body: encoded}, nil
	}

	return &Result{FinalURL: finalURL, Body: body}, nil
}

func (f *Fetcher) checkRobots(ctx context.Context, target *url.URL) error {
	robotsURL := &url.URL{
		Scheme: target.Scheme,
		Host:   target.Host,
		Path:   "/robots.txt",
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, robotsURL.String(), nil)
	if err != nil {
		return errcode.New(errcode.Parse, errcode.ForbiddenTargetHost)
	}
	req.Header.Set("User-Agent", defaultUserAgent)

	resp, err := f.client.Do(req)
	if err != nil {
		return classifyTransportError(err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return errcode.New(errcode.Timeout, errcode.UpstreamUnavailable)
	case resp.StatusCode != http.StatusOK:
		// Per RFC 9309 section 2.3.1.3, 4xx (other than 429) means no robots restrictions.
		return nil
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxRobotsBytes))
	if err != nil {
		return classifyTransportError(err)
	}
	grp := robots.Parse(raw)
	clear(raw)

	pathWithQuery := target.EscapedPath()
	if pathWithQuery == "" {
		pathWithQuery = "/"
	}
	if target.RawQuery != "" {
		pathWithQuery += "?" + target.RawQuery
	}
	if !grp.Allowed(pathWithQuery) {
		return errcode.New(errcode.NeedsSubtool, errcode.BlockedByRobots)
	}
	return nil
}

func (f *Fetcher) validateTargetURL(u *url.URL) error {
	if u == nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Opaque != "" {
		return errcode.New(errcode.Parse, errcode.ForbiddenTargetHost)
	}
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "" {
		return errcode.New(errcode.Parse, errcode.ForbiddenTargetHost)
	}
	if !f.allowPrivate {
		if host == "localhost" || strings.HasSuffix(host, ".localhost") ||
			strings.HasSuffix(host, ".local") || strings.HasSuffix(host, ".internal") ||
			strings.HasSuffix(host, ".svc") || strings.HasSuffix(host, ".cluster.local") {
			return errcode.New(errcode.Parse, errcode.ForbiddenTargetHost)
		}
		if ip, err := netip.ParseAddr(host); err == nil && isNonPublicIP(ip) {
			return errcode.New(errcode.Parse, errcode.ForbiddenTargetHost)
		}
	}
	return nil
}

func checkDialAddress(ctx context.Context, addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return errcode.New(errcode.Parse, errcode.ForbiddenTargetHost)
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		if isNonPublicIP(ip) {
			return errcode.New(errcode.Parse, errcode.ForbiddenTargetHost)
		}
		return nil
	}
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil || len(ips) == 0 {
		return errcode.New(errcode.Timeout, errcode.UpstreamUnavailable)
	}
	for _, ip := range ips {
		if isNonPublicIP(ip) {
			return errcode.New(errcode.Parse, errcode.ForbiddenTargetHost)
		}
	}
	return nil
}

var cgnatPrefix = netip.MustParsePrefix("100.64.0.0/10")
var reservedPrefix = netip.MustParsePrefix("240.0.0.0/4")

func isNonPublicIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	if ip.Is4() && (cgnatPrefix.Contains(ip) || reservedPrefix.Contains(ip)) {
		return true
	}
	return false
}

func hasChallengeHeader(h http.Header) bool {
	return strings.EqualFold(strings.TrimSpace(h.Get("Cf-Mitigated")), "challenge")
}

// isAntiBotHTML detects dedicated anti-bot / CAPTCHA gate pages that hold no
// public tool form or feature table.
func isAntiBotHTML(raw []byte) bool {
	lower := bytes.ToLower(raw)
	markers := [][]byte{
		[]byte("cf-turnstile"),
		[]byte("g-recaptcha"),
		[]byte("h-captcha"),
		[]byte("challenge-platform"),
	}
	matched := false
	for _, m := range markers {
		if bytes.Contains(lower, m) {
			matched = true
			break
		}
	}
	if !matched {
		return false
	}
	return !bytes.Contains(lower, []byte("<table"))
}

func classifyTransportError(err error) error {
	if coded, ok := errcode.From(err); ok {
		return coded
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return errcode.New(errcode.Timeout, errcode.DeadlineExceeded)
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return errcode.New(errcode.Timeout, errcode.DeadlineExceeded)
	}
	return errcode.New(errcode.Timeout, errcode.UpstreamUnavailable)
}
