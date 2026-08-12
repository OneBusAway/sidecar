package httpapi

import (
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
)

// spaHandler serves the embedded admin SPA per spec section 6.5: real files
// as-is, unknown paths fall back to index.html for client-side routing --
// except under _app/, where a missing asset is a stale-deploy artifact and
// must 404 rather than hand HTML to a script tag.
type spaHandler struct {
	fs     fs.FS
	logger *slog.Logger
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
			// web` (design spec §2.3). Say so plainly rather than serving a
			// generic 404 that looks like a routing bug.
			h.logger.Warn("httpapi: admin UI not built", "path", r.URL.Path)
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
