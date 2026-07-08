package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ai-club/sandbox-host/internal/apimodel"
)

// newTestPreviewSetup boots a fake guest HTTP server plus a previewManager in
// tunnel mode, opens one route to the guest, and returns an api value whose
// previewRouterHandler can be exercised with httptest.
func newTestPreviewSetup(t *testing.T, signingSecret string) (*api, string) {
	t.Helper()

	guest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "hello from guest "+r.URL.Path)
	}))
	t.Cleanup(guest.Close)
	guestURL, err := url.Parse(guest.URL)
	if err != nil {
		t.Fatal(err)
	}
	guestPort, err := strconv.Atoi(guestURL.Port())
	if err != nil {
		t.Fatal(err)
	}

	cfg := apiConfig{
		PreviewBindHost:      "127.0.0.1",
		PreviewExternalHost:  "127.0.0.1",
		PreviewPortStart:     42600,
		PreviewPortEnd:       42640,
		PreviewDomain:        "previews.example.com",
		PreviewSigningSecret: signingSecret,
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	pm := newPreviewManager(cfg, logger)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	route, err := pm.open(ctx, "sess-test", guestURL.Hostname(), guestPort)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(route.URL, "https://") || !strings.HasSuffix(route.URL, ".previews.example.com") {
		t.Fatalf("tunnel-mode route URL malformed: %q", route.URL)
	}
	if len(route.Subdomain) > 63 {
		t.Fatalf("subdomain label exceeds DNS 63-char limit: %q (%d)", route.Subdomain, len(route.Subdomain))
	}

	a := &api{config: cfg, previews: pm, logger: logger}
	host := route.Subdomain + ".previews.example.com"
	return a, host
}

func routerGet(t *testing.T, a *api, target, host string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", target, nil)
	req.Host = host
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	a.previewRouterHandler().ServeHTTP(rec, req)
	return rec
}

func TestPreviewRouterProxiesWithoutAuthWhenNoSecret(t *testing.T) {
	a, host := newTestPreviewSetup(t, "")
	rec := routerGet(t, a, "http://"+host+"/index.html", host, nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "hello from guest /index.html") {
		t.Fatalf("expected proxied 200, got %d %q", rec.Code, rec.Body.String())
	}
}

func TestPreviewRouterUnknownSubdomain(t *testing.T) {
	a, _ := newTestPreviewSetup(t, "")
	host := "p-doesnotexist-5173.previews.example.com"
	rec := routerGet(t, a, "http://"+host+"/", host, nil)
	if rec.Code != 404 {
		t.Fatalf("expected 404 for unknown subdomain, got %d", rec.Code)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("404 must be no-store, got %q", cc)
	}
}

func TestPreviewRouterForeignHostRejected(t *testing.T) {
	a, _ := newTestPreviewSetup(t, "")
	for _, host := range []string{
		"previews.example.com",              // apex, empty label
		"a.b.previews.example.com",          // multi-level label
		"evil.example.com",                  // wrong domain
		"p-x-5173.previews.example.com.evil.com", // suffix spoof
	} {
		rec := routerGet(t, a, "http://"+host+"/", host, nil)
		if rec.Code != 404 {
			t.Fatalf("host %q: expected 404, got %d", host, rec.Code)
		}
	}
}

func TestPreviewRouterSignedTokenFlow(t *testing.T) {
	const secret = "0123456789abcdef0123456789abcdef"
	a, host := newTestPreviewSetup(t, secret)

	// 1. No token → 403, and nothing proxied.
	rec := routerGet(t, a, "http://"+host+"/", host, nil)
	if rec.Code != 403 {
		t.Fatalf("expected 403 without token, got %d", rec.Code)
	}

	// 2. Valid query token → 200 + Set-Cookie.
	exp := time.Now().Add(time.Hour).Unix()
	token := mintPreviewToken([]byte(secret), host, exp)
	rec = routerGet(t, a, "http://"+host+"/?"+previewTokenQueryParam+"="+url.QueryEscape(token), host, nil)
	if rec.Code != 200 {
		t.Fatalf("expected 200 with query token, got %d %q", rec.Code, rec.Body.String())
	}
	setCookie := rec.Header().Get("Set-Cookie")
	for _, want := range []string{previewCookieName + "=", "SameSite=None", "Secure", "HttpOnly", "Partitioned"} {
		if !strings.Contains(setCookie, want) {
			t.Fatalf("Set-Cookie missing %q: %s", want, setCookie)
		}
	}

	// 3. Cookie alone (subresource / HMR websocket case) → 200, no new cookie.
	rec = routerGet(t, a, "http://"+host+"/assets/app.js", host, &http.Cookie{Name: previewCookieName, Value: token})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "/assets/app.js") {
		t.Fatalf("expected proxied 200 via cookie, got %d %q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Set-Cookie") != "" {
		t.Fatal("cookie-authorized request should not re-set the cookie")
	}

	// 4. Token for a different host → 403 (host binding).
	otherToken := mintPreviewToken([]byte(secret), "p-other-5173.previews.example.com", exp)
	rec = routerGet(t, a, "http://"+host+"/?"+previewTokenQueryParam+"="+url.QueryEscape(otherToken), host, nil)
	if rec.Code != 403 {
		t.Fatalf("expected 403 for host-mismatched token, got %d", rec.Code)
	}

	// 5. Security headers present on success and failure.
	for _, code := range []int{200, 403} {
		var r *httptest.ResponseRecorder
		if code == 200 {
			r = routerGet(t, a, "http://"+host+"/?"+previewTokenQueryParam+"="+url.QueryEscape(token), host, nil)
		} else {
			r = routerGet(t, a, "http://"+host+"/", host, nil)
		}
		if r.Header().Get("Referrer-Policy") != "no-referrer" || r.Header().Get("X-Robots-Tag") != "noindex" {
			t.Fatalf("security headers missing on %d response", code)
		}
	}
}

func TestPreviewRouterRouteClosedAfterStop(t *testing.T) {
	a, host := newTestPreviewSetup(t, "")
	label := strings.TrimSuffix(host, ".previews.example.com")
	if _, ok := a.previews.lookup(label); !ok {
		t.Fatal("route should be registered")
	}
	// close() is keyed by Subdomain for the router map (HostPort only matters
	// for the legacy port listener, which nil-checks).
	a.previews.close([]apimodel.Route{{Subdomain: label}})
	rec := routerGet(t, a, "http://"+host+"/", host, nil)
	if rec.Code != 404 {
		t.Fatalf("expected 404 after close, got %d", rec.Code)
	}
}
