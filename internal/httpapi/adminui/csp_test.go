package adminui_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/OneBusAway/sidecar/internal/httpapi"
	"github.com/OneBusAway/sidecar/internal/httpapi/adminui"
)

// TestBuiltSPAFitsTheCSP pins SC-17 against the real SvelteKit build, not a
// fixture: every inline <script> in the built index.html must be allowed by
// the CSP the server sends for /admin (by hash), and the page must carry no
// inline style at all -- the policy has no 'unsafe-inline', so a style=""
// attribute or <style> block a future SvelteKit or app.html change adds
// would be silently dropped by the browser. Needs `make web`.
func TestBuiltSPAFitsTheCSP(t *testing.T) {
	fsys := adminui.FS()
	index, err := fs.ReadFile(fsys, "index.html")
	if err != nil {
		t.Fatalf("index.html missing from the embed -- run make web: %v", err)
	}

	rec := httptest.NewRecorder()
	httpapi.NewRouter(httpapi.Deps{AdminUI: fsys, Logger: slog.New(slog.DiscardHandler)}).
		ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/admin", nil))
	csp := rec.Header().Get("Content-Security-Policy")
	if csp == "" {
		t.Fatal("no CSP on /admin")
	}

	scripts := regexp.MustCompile(`(?is)<script\b([^>]*)>(.*?)</script>`).FindAllSubmatch(index, -1)
	inline := 0
	for _, m := range scripts {
		if strings.Contains(strings.ToLower(string(m[1])), "src=") {
			continue
		}
		inline++
		sum := sha256.Sum256(m[2])
		if h := "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"; !strings.Contains(csp, h) {
			t.Errorf("inline script %q is not allowed by the CSP (want %s in %q)", m[2], h, csp)
		}
	}
	if inline == 0 {
		t.Error("no inline script found; SvelteKit's bootstrap moved -- re-check that the SPA still boots under the CSP")
	}
	if regexp.MustCompile(`(?i)\sstyle\s*=`).Match(index) || regexp.MustCompile(`(?i)<style\b`).Match(index) {
		t.Errorf("built index.html carries inline style, which the CSP blocks:\n%s", index)
	}
	// Style attributes the runtime writes (SvelteKit's route announcer) must
	// each be allowed by exact hash.
	err = fs.WalkDir(fsys, "_app", func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() || !strings.HasSuffix(p, ".js") {
			return walkErr
		}
		b, readErr := fs.ReadFile(fsys, p)
		if readErr != nil {
			return readErr
		}
		for _, m := range regexp.MustCompile(`style="([^"]*)"`).FindAllSubmatch(b, -1) {
			sum := sha256.Sum256(m[1])
			if h := "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"; !strings.Contains(csp, h) {
				t.Errorf("%s writes style %q, which the CSP does not allow", p, m[1])
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if regexp.MustCompile(`(?i)\son[a-z]+\s*=`).Match(index) {
		t.Errorf("built index.html carries an inline event handler, which the CSP blocks:\n%s", index)
	}
}
