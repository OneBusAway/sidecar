// Package httpapi is the HTTP layer for the sidecar: the rider-facing feeds,
// which are unauthenticated by design, and the admin API behind a session
// cookie. It wires the repositories into stdlib handlers; nothing in this
// package reads the wall clock or sleeps directly, since the design spec bans
// time.Now outside cmd/ (see internal/alerts/feed.go) and the login failure
// delay has to be observable to tests.
package httpapi

import (
	"io/fs"
	"log/slog"
	"net/http"
	"time"

	"github.com/OneBusAway/sidecar/internal/alerts"
	"github.com/OneBusAway/sidecar/internal/auth"
	"github.com/OneBusAway/sidecar/internal/regions"
)

// Deps carries everything the router needs from the outside world. Now and
// Sleep are injected rather than called through the time package directly, so
// handler tests are deterministic, cost no wall-clock time, and the package
// stays clear of the repo-wide time.Now ban.
type Deps struct {
	Alerts  alerts.Repository
	Regions regions.Repository
	// Auth backs the admin session endpoints. When it is nil the admin routes
	// are not registered at all, so a feed-only deployment (or a feed-only
	// test) never has to supply one.
	Auth   auth.Repository
	Now    func() time.Time
	Logger *slog.Logger

	// FailDelay is the constant pause on a failed login: a brake on online
	// guessing, not a substitute for rate limiting (design spec §4.3).
	// Production sets 500ms; tests set whatever they assert on.
	FailDelay time.Duration
	// Sleep applies FailDelay. NewRouter defaults it to time.Sleep; tests
	// substitute a recorder so they can assert the delay was applied without
	// spending it and without reading the clock.
	Sleep func(time.Duration)

	// AdminUI is the built admin SPA, served under /admin. Nil means the
	// binary was built without it and those routes are not registered.
	AdminUI fs.FS
}

// ServerConfig configures the HTTP server NewServer builds.
type ServerConfig struct {
	// Addr is the address http.Server.ListenAndServe binds, e.g. ":8080".
	Addr string
	Deps Deps
}

// NewServer builds the HTTP server for the sidecar's public feeds.
//
// Timeouts are set explicitly: this endpoint is unauthenticated by design
// (design spec §1.3), and Go's http.Server defaults every timeout to zero,
// so a trivial slowloris client would otherwise hold a goroutine and file
// descriptor open indefinitely.
func NewServer(cfg ServerConfig) *http.Server {
	return &http.Server{
		Addr:              cfg.Addr,
		Handler:           NewRouter(cfg.Deps),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
}

// NewRouter builds the sidecar's HTTP handler.
func NewRouter(deps Deps) http.Handler {
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	if deps.Sleep == nil {
		deps.Sleep = time.Sleep
	}

	h := &alertsHandler{deps: deps}

	mux := http.NewServeMux()
	// The feed routes are unauthenticated by design (design spec §1.3) and
	// deliberately bypass every admin middleware.
	mux.HandleFunc("GET /api/v1/regions/{regionId}/alerts", h.feedBinary)
	mux.HandleFunc("GET /api/v1/regions/{regionId}/alerts.pbtext", h.feedText)

	if deps.Auth != nil {
		registerAdminRoutes(mux, deps)
	}
	return mux
}

// registerAdminRoutes mounts the admin JSON API. Every route passes through
// crossSiteGuard -- including POST /session, which is the whole reason the
// guard is separate from requireSession (design spec §4.4) -- and everything
// except login also requires a live session.
func registerAdminRoutes(mux *http.ServeMux, deps Deps) {
	session := &sessionHandler{deps: deps}
	mw := &authMiddleware{deps: deps}

	guarded := func(h http.HandlerFunc) http.Handler {
		return crossSiteGuard(deps.Logger, h)
	}
	authed := func(h http.HandlerFunc) http.Handler {
		return crossSiteGuard(deps.Logger, mw.requireSession(h))
	}

	mux.Handle("POST /api/admin/v1/session", guarded(session.login))
	// Logout sits outside requireSession so it stays idempotent: the SPA
	// calls it to tidy up after a 401, and answering that with another 401
	// gives the client nothing it can act on. It is not a hole -- the handler
	// only ever deletes the session named by the token the caller presented,
	// and the cross-site guard still covers it.
	mux.Handle("DELETE /api/admin/v1/session", guarded(session.logout))
	mux.Handle("GET /api/admin/v1/session", authed(session.whoami))
}
