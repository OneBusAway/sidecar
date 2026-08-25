// Package httpapi is the HTTP layer for the sidecar: the rider-facing feeds,
// which are unauthenticated by design, the admin API behind a session
// cookie, and the admin SPA itself (also unauthenticated -- the login page
// lives there, and everything sensitive is behind the API). It wires the
// repositories into stdlib handlers; nothing in this package reads the wall
// clock or sleeps directly, since the design spec bans time.Now outside
// cmd/ (see internal/alerts/feed.go) and the login failure delay has to be
// observable to tests.
package httpapi

import (
	"io/fs"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/OneBusAway/sidecar/internal/alarms"
	"github.com/OneBusAway/sidecar/internal/alerts"
	"github.com/OneBusAway/sidecar/internal/auth"
	"github.com/OneBusAway/sidecar/internal/ghostbus"
	"github.com/OneBusAway/sidecar/internal/liveactivities"
	"github.com/OneBusAway/sidecar/internal/obaapi"
	"github.com/OneBusAway/sidecar/internal/pushreg"
	"github.com/OneBusAway/sidecar/internal/ratelimit"
	"github.com/OneBusAway/sidecar/internal/regions"
	"github.com/OneBusAway/sidecar/internal/surveys"
	"github.com/OneBusAway/sidecar/internal/vehicles"
	"github.com/OneBusAway/sidecar/internal/weather"
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

	// Vehicles backs the vehicle search endpoint. Nil means the route is not
	// registered, which is how a feed-only deployment (or a feed-only test)
	// avoids having to supply one.
	Vehicles *vehicles.Service

	// Weather backs the forecast endpoint. Nil means the route is not
	// registered.
	Weather *weather.Service

	// PushRegs backs the push registration endpoints and the V2 alarm
	// side-effect upsert. Nil means those routes are not registered.
	PushRegs pushreg.Repository
	// PushLimiter is the §2.6 throttle for the push_registrations path.
	// NewRouter defaults it (30/minute per IP); tests inject tighter ones.
	PushLimiter *ratelimit.Limiter

	// FeedbackSecret, when non-empty, is the bearer token /webhooks/gorush
	// requires. Empty keeps the endpoint open, because a deployment whose
	// gorush cannot send a header must not lose its prune signal entirely --
	// but an open endpoint is throttled instead (see below), since it deletes
	// registrations across every region on a caller-supplied token.
	FeedbackSecret string
	// FeedbackLimiter bounds an unauthenticated /webhooks/gorush. NewRouter
	// defaults it; it is unused when FeedbackSecret is set.
	FeedbackLimiter *ratelimit.Limiter

	// Alarms backs the alarm create/delete endpoints (spec §5.1-§5.2). Nil
	// means those routes are not registered.
	Alarms alarms.Repository
	// OBA resolves the arrival/departure an alarm's creation-time message is
	// composed from. Nil (or any lookup failure) degrades every alarm to the
	// generic message rather than failing the create.
	OBA obaapi.Client

	// LiveActivities backs the Live Activity register/delete endpoints
	// (spec §6.1). Nil means those routes are not registered.
	LiveActivities liveactivities.Repository
	// LiveActivityLimiter throttles Live Activity registration POSTs
	// (design spec §4: a sidecar-specific 30/minute per IP, because every
	// distinct stop a registration names costs one upstream call per
	// minute for eight hours). NewRouter defaults it; tests inject tighter.
	LiveActivityLimiter *ratelimit.Limiter

	// Surveys backs the survey list and survey response endpoints (spec
	// §7). Nil means those routes are not registered.
	Surveys surveys.Repository
	// SurveyLimiter is the per-source throttle on survey response writes
	// (surveys design spec §2.9): one bucket shared by create and amend.
	// NewRouter defaults it (60/minute); tests inject tighter ones.
	SurveyLimiter *ratelimit.Limiter

	// GhostBus backs the ghost bus report endpoint (spec §8). Nil means the
	// route is not registered.
	GhostBus ghostbus.Repository
	// GhostBusIPLimiter is the §2.6 10/hour-per-IP throttle; NewRouter
	// defaults it, tests inject tighter ones.
	GhostBusIPLimiter *ratelimit.Limiter
	// GhostBusUserLimiter is the §2.6 20/day-per-user_identifier throttle.
	GhostBusUserLimiter *ratelimit.Limiter

	// FailDelay is the constant pause on a failed login: a brake on online
	// guessing, not a substitute for rate limiting (design spec §4.3).
	// Production sets 500ms; tests set whatever they assert on.
	FailDelay time.Duration
	// Sleep applies FailDelay. NewRouter defaults it to time.Sleep; tests
	// substitute a recorder so they can assert the delay was applied without
	// spending it and without reading the clock.
	Sleep func(time.Duration)
	// VerifyPassword checks a password against a PHC hash. NewRouter defaults
	// it to auth.VerifyPassword. It is injected for one reason: the login
	// handler must verify against auth.DummyPHC even when the username does
	// not exist (design spec §4.3), and that call has no observable effect on
	// the response -- only a recorder can prove it still happens.
	VerifyPassword func(phc, password string) (bool, error)

	// AdminUI is the built admin SPA, served under /admin. Nil means the
	// binary was built without it and those routes are not registered.
	AdminUI fs.FS

	// OBADefaultKeySet reports whether the process was started with an OBA
	// REST API key (--oba-api-key/SIDECAR_OBA_API_KEY). The admin regions
	// endpoint uses it to distinguish a region that inherits a working
	// default from one where calls will actually fail -- a distinction a
	// plain "is this region's column empty" boolean cannot make.
	OBADefaultKeySet bool
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
	if deps.VerifyPassword == nil {
		deps.VerifyPassword = auth.VerifyPassword
	}

	h := &alertsHandler{deps: deps}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", healthz)

	// The feed routes are unauthenticated by design (design spec §1.3) and
	// deliberately bypass every admin middleware.
	mux.HandleFunc("GET /api/v1/regions/{regionId}/alerts", h.feedBinary)
	mux.HandleFunc("GET /api/v1/regions/{regionId}/alerts.pbtext", h.feedText)

	if deps.Vehicles != nil {
		vh := &vehiclesHandler{deps: deps}
		mux.HandleFunc("GET /api/v1/regions/{regionId}/vehicles", vh.search)
	}

	if deps.Weather != nil {
		wh := &weatherHandler{deps: deps}
		mux.HandleFunc("GET /api/v1/regions/{regionId}/weather", wh.forecast)
	}

	if deps.PushRegs != nil {
		// The handlers deref Regions (resolveRegion) and Now on the first
		// request; fail at boot, naming everything missing so the fix is one
		// edit rather than three restarts, matching the Auth block below.
		if missing := missingDeps(map[string]bool{
			"Deps.Now": deps.Now == nil, "Deps.Regions": deps.Regions == nil,
		}); len(missing) > 0 {
			panic("httpapi: " + strings.Join(missing, ", ") + " required when Deps.PushRegs is set")
		}
		if deps.PushLimiter == nil {
			deps.PushLimiter = ratelimit.New(30, time.Minute)
		}
		ph := &pushRegsHandler{deps: deps}
		mux.HandleFunc("POST /api/v2/regions/{regionId}/push_registrations",
			throttleByIP(deps.PushLimiter, deps, ph.register))
		mux.HandleFunc("DELETE /api/v2/regions/{regionId}/push_registrations",
			throttleByIP(deps.PushLimiter, deps, ph.unregister))
	}

	if deps.Alarms != nil {
		// The V2 side-effect upsert (spec §5.2) needs the registry, and the
		// handlers deref Regions and Now; failing at boot beats a nil deref
		// on the first alarm. Checked independently of the PushRegs block
		// above rather than leaning on its guard: these are this block's own
		// requirements, and the panic should say which one is missing.
		if missing := missingDeps(map[string]bool{
			"Deps.PushRegs": deps.PushRegs == nil, "Deps.Now": deps.Now == nil,
			"Deps.Regions": deps.Regions == nil,
		}); len(missing) > 0 {
			panic("httpapi: " + strings.Join(missing, ", ") + " required when Deps.Alarms is set")
		}
		ah := &alarmsHandler{deps: deps}
		mux.HandleFunc("POST /api/v1/regions/{regionId}/alarms", ah.create(1))
		mux.HandleFunc("POST /api/v2/regions/{regionId}/alarms", ah.create(2))
		mux.HandleFunc("DELETE /api/v1/regions/{regionId}/alarms/{alarmToken}", ah.delete)
		mux.HandleFunc("DELETE /api/v2/regions/{regionId}/alarms/{alarmToken}", ah.delete)
	}

	if deps.LiveActivities != nil {
		if missing := missingDeps(map[string]bool{
			"Deps.Now": deps.Now == nil, "Deps.Regions": deps.Regions == nil,
		}); len(missing) > 0 {
			panic("httpapi: " + strings.Join(missing, ", ") + " required when Deps.LiveActivities is set")
		}
		if deps.LiveActivityLimiter == nil {
			deps.LiveActivityLimiter = ratelimit.New(liveActivityRegistrationsPerMinute, time.Minute)
		}
		lh := &liveActivitiesHandler{deps: deps}
		mux.HandleFunc("POST /api/v2/regions/{regionId}/live_activities",
			throttleByIP(deps.LiveActivityLimiter, deps, lh.register))
		// DELETE is unthrottled: dismissals are cheap and a throttled
		// dismissal would strand a row until expiry.
		mux.HandleFunc("DELETE /api/v2/regions/{regionId}/live_activities/{liveActivityToken}", lh.delete)
	}

	if deps.PushRegs != nil || deps.LiveActivities != nil {
		// The feedback webhook prunes whichever token tables are configured
		// (spec §6.5); it must exist for a Live-Activities-only deployment
		// too (design spec §2.8).
		//
		// gorush is our own infrastructure and throttling it would drop prune
		// signals during a mass bounce -- but that argument only holds for a
		// caller we can actually identify. With a shared secret configured
		// the webhook runs unthrottled as intended; without one it is an
		// unauthenticated endpoint that deletes a token's registrations in
		// every region, so it gets a bucket of its own. That bucket is far
		// looser than the push_registrations one: dropping a prune signal
		// only delays a dead token to the 180-day sweep, so the limit exists
		// to bound abuse, not to police normal volume.
		fh := &feedbackHandler{deps: deps}
		feedback := fh.receive
		if deps.FeedbackSecret == "" {
			if deps.FeedbackLimiter == nil {
				deps.FeedbackLimiter = ratelimit.New(feedbackLimitPerMinute, time.Minute)
			}
			feedback = throttleByIP(deps.FeedbackLimiter, deps, feedback)
		}
		mux.HandleFunc("POST /webhooks/gorush", feedback)
	}

	if deps.Surveys != nil {
		if missing := missingDeps(map[string]bool{
			"Deps.Now": deps.Now == nil, "Deps.Regions": deps.Regions == nil,
		}); len(missing) > 0 {
			panic("httpapi: " + strings.Join(missing, ", ") + " required when Deps.Surveys is set")
		}
		if deps.SurveyLimiter == nil {
			deps.SurveyLimiter = ratelimit.New(surveyWritesPerMinute, time.Minute)
		}
		sh := &surveysHandler{deps: deps}
		// Both apps fetch the Rails-style ".json" path; both POST the
		// create with a trailing slash ({$} is the exact-match pattern; the
		// mux does not strip it); iOS amends with PUT, Android with POST,
		// the OpenAPI with PATCH (surveys design spec §2.1).
		mux.HandleFunc("GET /api/v1/regions/{regionId}/surveys", sh.list)
		mux.HandleFunc("GET /api/v1/regions/{regionId}/surveys.json", sh.list)
		write := func(next http.HandlerFunc) http.HandlerFunc { return throttleByIP(deps.SurveyLimiter, deps, next) }
		mux.HandleFunc("POST /api/v1/survey_responses", write(sh.create))
		mux.HandleFunc("POST /api/v1/survey_responses/{$}", write(sh.create))
		mux.HandleFunc("POST /api/v1/survey_responses/{responseId}", write(sh.amend))
		mux.HandleFunc("PUT /api/v1/survey_responses/{responseId}", write(sh.amend))
		mux.HandleFunc("PATCH /api/v1/survey_responses/{responseId}", write(sh.amend))
	}

	if deps.GhostBus != nil {
		if missing := missingDeps(map[string]bool{
			"Deps.Now": deps.Now == nil, "Deps.Regions": deps.Regions == nil,
		}); len(missing) > 0 {
			panic("httpapi: " + strings.Join(missing, ", ") + " required when Deps.GhostBus is set")
		}
		if deps.GhostBusIPLimiter == nil {
			deps.GhostBusIPLimiter = ratelimit.New(10, time.Hour) // spec §2.6
		}
		if deps.GhostBusUserLimiter == nil {
			deps.GhostBusUserLimiter = ratelimit.New(20, 24*time.Hour) // spec §2.6
		}
		gh := &ghostBusHandler{deps: deps}
		mux.HandleFunc("POST /api/v2/regions/{regionId}/ghost_bus_reports",
			throttleByIP(deps.GhostBusIPLimiter, deps, gh.create))
	}

	// The admin SPA is registered independently of the admin API below, and
	// deliberately outside registerAdminRoutes / adminRoutes: it is served
	// unauthenticated (the login page is part of it) and must never pass
	// through crossSiteGuard or requireSession, both of which assume a JSON
	// API. Nil AdminUI means the binary was built without it, so the routes
	// are simply not registered.
	if deps.AdminUI != nil {
		h := &spaHandler{fs: deps.AdminUI, logger: deps.Logger}
		mux.HandleFunc("GET /admin", h.serve)
		mux.HandleFunc("GET /admin/{path...}", h.serve)
	}

	// The admin routes dereference all three of these on the first request that
	// reaches them, and a nil-deref inside a handler is recovered by net/http
	// per connection: the operator would see a reset request some time after
	// deployment rather than an error at startup. Now in particular cannot be
	// defaulted here, since time.Now is banned outside cmd/. Fail loudly while
	// there is still a stack trace worth reading, naming everything missing so
	// the fix is one edit rather than three restarts.
	if deps.Auth != nil {
		missing := missingDeps(map[string]bool{
			"Deps.Now": deps.Now == nil, "Deps.Alerts": deps.Alerts == nil,
			"Deps.Regions": deps.Regions == nil,
		})
		if len(missing) > 0 {
			panic("httpapi: " + strings.Join(missing, ", ") + " required when Deps.Auth is set")
		}
		registerAdminRoutes(mux, deps)
	}
	return mux
}

// adminRoute is one admin API route plus the middleware it must carry.
//
// The routes are a table rather than a run of mux.Handle calls for one
// reason: http.ServeMux cannot be enumerated, so a route registered without
// requireSession would be invisible to every test in this package. The table
// is the thing a test can walk, which is what makes "every admin route needs a
// session" checkable rather than merely intended.
type adminRoute struct {
	pattern string
	handler http.HandlerFunc
	// requiresSession is false for exactly two routes, both deliberate and
	// both documented at their entry in adminRoutes.
	requiresSession bool
}

// adminRoutes is the admin API route table (design spec §5). It takes Deps so
// the handlers close over the same dependencies the router was built with;
// a caller that only wants to inspect the table can pass a zero Deps.
func adminRoutes(deps Deps) []adminRoute {
	session := &sessionHandler{deps: deps}
	alertsAdmin := &adminAlertsHandler{deps: deps}
	regionsAdmin := &adminRegionsHandler{deps: deps}

	return []adminRoute{
		// Login has no session to require yet; the cross-site guard is what
		// protects it (design spec §4.4).
		{"POST /api/admin/v1/session", session.login, false},
		// Logout sits outside requireSession so it stays idempotent: the SPA
		// calls it to tidy up after a 401, and answering that with another
		// 401 gives the client nothing it can act on. It is not a hole -- the
		// handler only ever deletes the session named by the token the caller
		// presented, and the cross-site guard still covers it.
		{"DELETE /api/admin/v1/session", session.logout, false},
		{"GET /api/admin/v1/session", session.whoami, true},

		{"GET /api/admin/v1/alerts", alertsAdmin.list, true},
		{"POST /api/admin/v1/alerts", alertsAdmin.create, true},
		{"GET /api/admin/v1/alerts/{id}", alertsAdmin.get, true},
		{"PATCH /api/admin/v1/alerts/{id}", alertsAdmin.patch, true},
		{"DELETE /api/admin/v1/alerts/{id}", alertsAdmin.delete, true},
		{"POST /api/admin/v1/alerts/{id}/publish", alertsAdmin.setPublished(true), true},
		{"POST /api/admin/v1/alerts/{id}/unpublish", alertsAdmin.setPublished(false), true},
		{"PUT /api/admin/v1/alerts/{id}/translations/{lang}", alertsAdmin.putTranslation, true},
		{"DELETE /api/admin/v1/alerts/{id}/translations/{lang}", alertsAdmin.deleteTranslation, true},

		{"GET /api/admin/v1/regions", regionsAdmin.list, true},
		{"PATCH /api/admin/v1/regions/{id}", regionsAdmin.patch, true},
	}
}

// routeRegistrar is the one method registerAdminRoutes needs from
// *http.ServeMux. It is an interface purely so a test can pass a recorder and
// assert that the set of patterns actually registered equals the set in
// adminRoutes -- otherwise a stray mux.Handle added inside registerAdminRoutes
// would mount an admin handler that no test in this package can see, since
// http.ServeMux offers no way to enumerate what it holds.
type routeRegistrar interface {
	Handle(pattern string, handler http.Handler)
}

// registerAdminRoutes mounts the admin JSON API. Every route passes through
// crossSiteGuard -- including POST /session, which is the whole reason the
// guard is separate from requireSession (design spec §4.4) -- and everything
// except login and logout also requires a live session.
//
// Every registration must come from adminRoutes; see routeRegistrar.
func registerAdminRoutes(mux routeRegistrar, deps Deps) {
	mw := &authMiddleware{deps: deps}
	for _, route := range adminRoutes(deps) {
		var h http.Handler = route.handler
		if route.requiresSession {
			h = mw.requireSession(h)
		}
		mux.Handle(route.pattern, crossSiteGuard(deps.Logger, h))
	}
}

// missingDeps returns the names of the absent dependencies, sorted so the
// panic message is stable across runs (Go map iteration order is not).
func missingDeps(absent map[string]bool) []string {
	var missing []string
	for name, isAbsent := range absent {
		if isAbsent {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	return missing
}
