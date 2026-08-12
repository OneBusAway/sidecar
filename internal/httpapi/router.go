// Package httpapi is the HTTP layer for the sidecar's rider-facing feeds. It
// wires the alerts and regions repositories into stdlib handlers; nothing in
// this package reads the wall clock directly, since the design spec bans
// time.Now outside cmd/ (see internal/alerts/feed.go).
package httpapi

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/OneBusAway/sidecar/internal/alerts"
	"github.com/OneBusAway/sidecar/internal/regions"
)

// Deps carries everything the router needs from the outside world. Now is
// injected rather than read from time.Now so handler tests are deterministic
// and the package stays clear of the repo-wide time.Now ban.
type Deps struct {
	Alerts  alerts.Repository
	Regions regions.Repository
	Now     func() time.Time
	Logger  *slog.Logger
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

	h := &alertsHandler{deps: deps}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/regions/{regionId}/alerts", h.feedBinary)
	mux.HandleFunc("GET /api/v1/regions/{regionId}/alerts.pbtext", h.feedText)
	return mux
}
