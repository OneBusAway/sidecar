package httpapi

import (
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"sync"
)

// spaHandler serves the embedded admin SPA per spec section 6.5: real files
// as-is, unknown paths fall back to index.html for client-side routing --
// except under _app/, where a missing asset is a stale-deploy artifact and
// must 404 rather than hand HTML to a script tag.
type spaHandler struct {
	fs     fs.FS
	logger *slog.Logger

	// warnUnbuiltOnce caps the "admin UI not built" log at one line for the
	// life of the handler. Every request against an unbuilt embed takes this
	// same path -- a health check or a user hammering reload before the
	// operator notices would otherwise write one WARN per request, burying
	// the one fact the log exists to surface.
	warnUnbuiltOnce sync.Once
}

// serve implements spaHandler's routing: an exact file match is served as
// itself; anything else falls back to index.html unless it is under _app/,
// which never falls back (design spec §6.5). Cache-Control follows the same
// split: content-hashed files under _app/immutable/ get a year of immutable
// caching, everything else -- including index.html itself and unhashed
// _app/ files like version.json -- gets no-cache, so a deploy is visible on
// the next load rather than stuck behind a cached shell.
func (h *spaHandler) serve(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/admin"), "/")
	if p == "" {
		p = "index.html"
	}
	if !fileExists(h.fs, p) {
		if strings.HasPrefix(p, "_app/") {
			http.NotFound(w, r)
			return
		}
		p = "index.html"
		if !fileExists(h.fs, p) {
			// The embed exists but is empty -- a binary built without `make
			// web` (design spec §2.3). Warn once so the operator has
			// something to find in the logs, rather than nothing -- but only
			// once: every request against an unbuilt embed hits this same
			// branch, and the response body already tells the caller what's
			// wrong on every request, so the log only needs to say it once.
			h.warnUnbuiltOnce.Do(func() {
				h.logger.Warn("httpapi: admin UI not built", "path", r.URL.Path)
			})
			http.Error(w, "admin UI not built; run make web", http.StatusServiceUnavailable)
			return
		}
	}
	if strings.HasPrefix(p, "_app/immutable/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	http.ServeFileFS(w, r, h.fs, p)
}

// fileExists reports whether name names a regular file (not a directory) in
// fsys. A directory match would otherwise let http.ServeFileFS serve a
// listing or redirect where a 404-and-fallback was intended.
func fileExists(fsys fs.FS, name string) bool {
	info, err := fs.Stat(fsys, name)
	if err != nil {
		return false
	}
	return !info.IsDir()
}
