package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// assertAdminAPIHeaders is SC-17's contract for every /api/admin/ response,
// whatever its status: no caching of admin JSON, no MIME sniffing, no
// framing, no referrer, and a CSP that lets the body do nothing if a browser
// ever renders it.
func assertAdminAPIHeaders(t *testing.T, label string, rec *httptest.ResponseRecorder) {
	t.Helper()
	h := rec.Header()
	want := map[string]string{
		"Cache-Control":          "no-store",
		"X-Content-Type-Options": "nosniff",
		"Referrer-Policy":        "no-referrer",
		"X-Frame-Options":        "DENY",
	}
	for k, v := range want {
		if got := h.Get(k); got != v {
			t.Errorf("%s (status %d): %s = %q, want %q", label, rec.Code, k, got, v)
		}
	}
	csp := h.Get("Content-Security-Policy")
	for _, d := range []string{"default-src 'none'", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, d) {
			t.Errorf("%s (status %d): CSP %q lacks %q", label, rec.Code, csp, d)
		}
	}
	if h.Get("Strict-Transport-Security") != "" {
		t.Errorf("%s: HSTS sent on a plain-HTTP request", label)
	}
}

// TestSecurityHeaders_EveryAdminRoute walks the whole admin route table,
// signed out and signed in, plus the mux's own 404 and 405 under the prefix:
// headers that only some handlers set are how one JSON body ends up cached
// by a shared proxy.
func TestSecurityHeaders_EveryAdminRoute(t *testing.T) {
	t.Parallel()

	f := newFullAdminFixture(t)
	// A 200 with a body, explicitly -- first, since the walk below includes
	// logout, which ends the fixture's session.
	rec := sendTo(f.handler, http.MethodGet, "/api/admin/v1/session", "", f.cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("whoami: status %d", rec.Code)
	}
	assertAdminAPIHeaders(t, "whoami 200", rec)
	for _, rt := range adminRoutes(f.deps) {
		method, target := concreteRoute(t, rt.pattern)
		assertAdminAPIHeaders(t, rt.pattern+" signed out", sendTo(f.handler, method, target, "", nil))
		assertAdminAPIHeaders(t, rt.pattern+" signed in", sendTo(f.handler, method, target, "", f.cookie))
	}
	if rec := sendTo(f.handler, http.MethodGet, "/api/admin/v1/no-such-route", "", f.cookie); rec.Code != http.StatusNotFound {
		t.Errorf("unknown admin route: status %d, want 404", rec.Code)
	} else {
		assertAdminAPIHeaders(t, "unknown admin route", rec)
	}
	if rec := sendTo(f.handler, http.MethodPut, "/api/admin/v1/session", "", nil); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("wrong method: status %d, want 405", rec.Code)
	} else {
		assertAdminAPIHeaders(t, "wrong method", rec)
	}
}

// TestSecurityHeaders_HSTSOnlyOverHTTPS: HSTS over plain-HTTP local
// development would be ignored at best and, on a real hostname, pin a
// browser to an HTTPS port that is not there.
func TestSecurityHeaders_HSTSOnlyOverHTTPS(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	for _, target := range []string{"/api/admin/v1/session", "/admin"} {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, target, nil)
		req.Host = "sidecar.test"
		req.Header.Set("X-Forwarded-Proto", "https")
		rec := httptest.NewRecorder()
		f.handler.ServeHTTP(rec, req)
		if got := rec.Header().Get("Strict-Transport-Security"); !strings.HasPrefix(got, "max-age=") {
			t.Errorf("%s over HTTPS: Strict-Transport-Security = %q, want max-age=...", target, got)
		}
	}
}

// TestSecurityHeaders_RiderRoutesUntouched: the rider-facing API and health
// check are consumed by apps and monitors, not browsers; SC-17 is scoped to
// the admin surface and must not change them.
func TestSecurityHeaders_RiderRoutesUntouched(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	for _, target := range []string{"/healthz", "/api/v1/regions/1/alerts.pbtext"} {
		rec := sendTo(f.handler, http.MethodGet, target, "", nil)
		if csp := rec.Header().Get("Content-Security-Policy"); csp != "" {
			t.Errorf("%s: CSP %q set on a rider route", target, csp)
		}
	}
}

const spaInlineScript = "\n\t\t\t\t{ __sveltekit_x = {base: \"/admin\"}; boot(); }\n\t\t\t"

func spaHeaderFS() fstest.MapFS {
	return fstest.MapFS{
		"index.html": {Data: []byte("<!doctype html><html><head>" +
			`<link href="/admin/_app/immutable/entry/start.js" rel="modulepreload">` +
			"</head><body><div>\n\t\t\t<script>" + spaInlineScript + "</script>\n\t\t</div></body></html>")},
		"_app/immutable/chunk.abc.js": {Data: []byte("console.log('chunk')")},
		"_app/immutable/entry/app.js": {Data: []byte("x=`<div id=\"svelte-announcer\" style=\"" + spaAnnouncerStyle + "\"></div>`")},
	}
}

const spaAnnouncerStyle = "position: absolute; width: 1px"

// TestSecurityHeaders_SPA: the admin SPA gets a strict CSP whose only
// script allowance beyond 'self' is the hash of the one inline bootstrap
// script SvelteKit writes into index.html, computed from the embedded file
// so a rebuild cannot silently break the page. No 'unsafe-inline' anywhere.
func TestSecurityHeaders_SPA(t *testing.T) {
	t.Parallel()

	sum := sha256.Sum256([]byte(spaInlineScript))
	wantHash := "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
	styleSum := sha256.Sum256([]byte(spaAnnouncerStyle))
	wantStyle := "style-src 'self' 'unsafe-hashes' 'sha256-" + base64.StdEncoding.EncodeToString(styleSum[:]) + "'"
	h := NewRouter(Deps{AdminUI: spaHeaderFS(), Logger: discardLogger()})

	for _, target := range []string{"/admin", "/admin/regions/1/alerts", "/admin/_app/immutable/chunk.abc.js"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, target, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d", target, rec.Code)
		}
		csp := rec.Header().Get("Content-Security-Policy")
		for _, d := range []string{
			"default-src 'self'", "script-src 'self' " + wantHash, wantStyle,
			"object-src 'none'", "base-uri 'none'", "form-action 'self'", "frame-ancestors 'none'",
		} {
			if !strings.Contains(csp, d) {
				t.Errorf("%s: CSP %q lacks %q", target, csp, d)
			}
		}
		if strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") {
			t.Errorf("%s: CSP %q allows unsafe-inline/unsafe-eval", target, csp)
		}
		for k, v := range map[string]string{"X-Content-Type-Options": "nosniff", "Referrer-Policy": "no-referrer", "X-Frame-Options": "DENY"} {
			if got := rec.Header().Get(k); got != v {
				t.Errorf("%s: %s = %q, want %q", target, k, got, v)
			}
		}
	}

	// The SPA keeps its own cache rules: no-store is for admin JSON only.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/admin/_app/immutable/chunk.abc.js", nil))
	if got := rec.Header().Get("Cache-Control"); !strings.Contains(got, "immutable") {
		t.Errorf("immutable asset Cache-Control = %q, want the long immutable cache kept", got)
	}
}
