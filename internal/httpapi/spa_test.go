package httpapi_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/OneBusAway/sidecar/internal/httpapi"
)

// spaTestFS is the fstest.MapFS every SPA test in this file shares: an
// index.html for the fallback and top-level requests, a hashed asset under
// _app/immutable/ (the SvelteKit convention this repo's cache rule keys
// off), an unhashed _app/version.json (design spec §6.5's carve-out), and a
// top-level static asset. Injecting fstest.MapFS rather than the real
// embedded dist directory is deliberate: the embed is empty until Task 9
// builds the SvelteKit project, so it cannot exercise any of these rules.
func spaTestFS() fstest.MapFS {
	return fstest.MapFS{
		"index.html":                  {Data: []byte("<html>admin shell</html>")},
		"_app/immutable/chunk.abc.js": {Data: []byte("console.log('chunk')")},
		"_app/version.json":           {Data: []byte(`{"version":"1"}`)},
		"favicon.png":                 {Data: []byte("fake-png-bytes")},
	}
}

// newSPARouter builds a router with only the SPA wired in -- no Auth, so
// NewRouter's fail-loud panic for missing admin dependencies never fires --
// which keeps these tests focused on spa.go's own rules rather than the
// admin API's.
func newSPARouter(fsys fstest.MapFS) http.Handler {
	return httpapi.NewRouter(httpapi.Deps{AdminUI: fsys, Logger: slog.New(slog.DiscardHandler)})
}

func TestSPA_RootServesIndexWithNoCache(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/admin", nil)
	newSPARouter(spaTestFS()).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != "<html>admin shell</html>" {
		t.Errorf("body = %q, want index.html contents", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control = %q, want %q", got, "no-cache")
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("Content-Type = %q, want it to contain text/html", ct)
	}
}

func TestSPA_UnknownPathFallsBackToIndex(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/admin/alerts/17", nil)
	newSPARouter(spaTestFS()).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != "<html>admin shell</html>" {
		t.Errorf("body = %q, want index.html contents (SPA fallback)", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control = %q, want %q", got, "no-cache")
	}
}

func TestSPA_ImmutableAssetGetsLongCache(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/admin/_app/immutable/chunk.abc.js", nil)
	newSPARouter(spaTestFS()).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != "console.log('chunk')" {
		t.Errorf("body = %q, want the chunk's own contents", got)
	}
	want := "public, max-age=31536000, immutable"
	if got := rec.Header().Get("Cache-Control"); got != want {
		t.Errorf("Cache-Control = %q, want %q", got, want)
	}
}

func TestSPA_UnhashedAppFileGetsNoCache(t *testing.T) {
	t.Parallel()

	// _app/version.json sits directly under _app/, not _app/immutable/: it
	// is SvelteKit's own unhashed manifest, and design spec §6.5 requires it
	// stay no-cache like everything else that isn't content-hashed, even
	// though it lives inside the _app/ tree that also carries the
	// no-fallback rule.
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/admin/_app/version.json", nil)
	newSPARouter(spaTestFS()).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control = %q, want %q (unhashed _app file must not be treated as immutable)", got, "no-cache")
	}
}

func TestSPA_MissingAppAssetIs404NotFallback(t *testing.T) {
	t.Parallel()

	// design spec §6.5's no-fallback rule: a missing file under _app/ is a
	// stale-deploy artifact (an old index.html referencing a chunk that no
	// longer exists), and must 404 rather than hand back index.html, which
	// would feed HTML to a <script> tag.
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/admin/_app/immutable/missing.js", nil)
	newSPARouter(spaTestFS()).ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if got := rec.Body.String(); strings.Contains(got, "admin shell") {
		t.Errorf("body = %q, must not contain index.html's contents", got)
	}
}

func TestSPA_TopLevelStaticAssetGetsNoCache(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/admin/favicon.png", nil)
	newSPARouter(spaTestFS()).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != "fake-png-bytes" {
		t.Errorf("body = %q, want favicon.png's own contents", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control = %q, want %q", got, "no-cache")
	}
}

func TestSPA_UnbuiltUIReturns503(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/admin", nil)
	newSPARouter(fstest.MapFS{}).ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if got := rec.Body.String(); !strings.Contains(got, "admin UI not built; run make web") {
		t.Errorf("body = %q, want it to contain %q", got, "admin UI not built; run make web")
	}
}

func TestSPA_NilAdminUIRegistersNoRoutes(t *testing.T) {
	t.Parallel()

	h := httpapi.NewRouter(httpapi.Deps{Logger: slog.New(slog.DiscardHandler)})

	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/admin", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (no /admin route registered at all)", rec.Code)
	}
}
