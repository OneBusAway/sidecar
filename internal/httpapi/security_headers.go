package httpapi

import (
	"crypto/sha256"
	"encoding/base64"
	"io/fs"
	"net/http"
	"regexp"
	"strings"
)

// adminAPICSP is the policy on every /api/admin/ response. The API only ever
// returns JSON or CSV, so a browser rendering one as a document should be
// allowed to do nothing at all.
const adminAPICSP = "default-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'"

// hstsValue is sent on admin responses that arrived over HTTPS only: over
// plain-HTTP local development it would be ignored at best, and on a real
// hostname it would pin browsers to an HTTPS port that is not there.
const hstsValue = "max-age=31536000"

// inlineScript matches an inline <script> element; a src attribute marks an
// external one, which 'self' already covers.
var inlineScript = regexp.MustCompile(`(?is)<script\b([^>]*)>(.*?)</script>`)

// styleAttr matches a style attribute literal inside a built JS chunk.
// SvelteKit's root component renders its route announcer (the visually
// hidden aria-live region) from a template string carrying one; the browser
// treats that as an inline style.
var styleAttr = regexp.MustCompile(`style="([^"]*)"`)

// spaCSP builds the admin SPA's policy (SC-17). Everything is 'self'; the
// only script allowance beyond that is the SHA-256 of each inline script in
// index.html -- SvelteKit's bootstrap, whose content changes every build --
// computed from the embedded file itself, so a rebuild can never silently
// fall out of step with the header. There is no 'unsafe-inline' for scripts
// or styles. index.html carries no inline style (web/admin/src/app.html
// uses a class), and the one style attribute SvelteKit's runtime writes --
// the route announcer's, a fixed string in its source -- is allowed by its
// exact hash under 'unsafe-hashes', found by scanning the built chunks, so
// it covers that value and nothing else. The adminui package test checks
// the real build against this policy.
func spaCSP(ui fs.FS) string {
	scripts := []string{"'self'"}
	styles := []string{"'self'"}
	if ui != nil {
		if index, err := fs.ReadFile(ui, "index.html"); err == nil {
			for _, m := range inlineScript.FindAllSubmatch(index, -1) {
				if strings.Contains(strings.ToLower(string(m[1])), "src=") {
					continue
				}
				scripts = append(scripts, cspHash(m[2]))
			}
		}
		styles = append(styles, chunkStyleHashes(ui)...)
	}
	return strings.Join([]string{
		"default-src 'self'",
		"script-src " + strings.Join(scripts, " "),
		"style-src " + strings.Join(styles, " "),
		"img-src 'self' data:",
		"font-src 'self'",
		"connect-src 'self'",
		"object-src 'none'",
		"base-uri 'none'",
		"form-action 'self'",
		"frame-ancestors 'none'",
	}, "; ")
}

// cspHash is the CSP source expression for content's SHA-256.
func cspHash(content []byte) string {
	sum := sha256.Sum256(content)
	return "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
}

// chunkStyleHashes returns 'unsafe-hashes' plus the hash of every distinct
// style attribute literal in the built JS under _app/, or nothing when there
// is none.
func chunkStyleHashes(ui fs.FS) []string {
	seen := map[string]bool{}
	var out []string
	//nolint:errcheck // a walk error just stops early with fewer allowances: the page can lose a style, never gain one
	_ = fs.WalkDir(ui, "_app", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(p, ".js") {
			return nil
		}
		b, readErr := fs.ReadFile(ui, p)
		if readErr != nil {
			return readErr
		}
		for _, m := range styleAttr.FindAllSubmatch(b, -1) {
			if h := cspHash(m[1]); !seen[h] {
				seen[h] = true
				out = append(out, h)
			}
		}
		return nil
	})
	if len(out) == 0 {
		return nil
	}
	return append([]string{"'unsafe-hashes'"}, out...)
}

// securityHeaders adds SC-17's headers to the admin surface -- the SPA under
// /admin and the JSON API under /api/admin/ -- before the mux routes the
// request, so every response carries them: handler output, the mux's own
// 404 and 405, and a 401 from the auth middleware alike. Rider-facing routes
// are untouched; apps and monitors, not browsers, consume them.
//
// Admin JSON is additionally no-store: it carries tenant data and is fetched
// with ambient credentials, and no intermediary or browser cache should keep
// it. The SPA keeps spa.go's own cache rules (immutable hashed assets,
// no-cache shell).
func securityHeaders(ui fs.FS, next http.Handler) http.Handler {
	spaPolicy := spaCSP(ui)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		isAPI := strings.HasPrefix(p, "/api/admin/")
		isSPA := p == "/admin" || strings.HasPrefix(p, "/admin/")
		if isAPI || isSPA {
			h := w.Header()
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("Referrer-Policy", "no-referrer")
			h.Set("X-Frame-Options", "DENY")
			if isHTTPS(r) {
				h.Set("Strict-Transport-Security", hstsValue)
			}
			if isAPI {
				h.Set("Content-Security-Policy", adminAPICSP)
				h.Set("Cache-Control", "no-store")
			} else {
				h.Set("Content-Security-Policy", spaPolicy)
			}
		}
		next.ServeHTTP(w, r)
	})
}
