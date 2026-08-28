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
	"github.com/OneBusAway/sidecar/internal/alertpush"
	"github.com/OneBusAway/sidecar/internal/alerts"
	"github.com/OneBusAway/sidecar/internal/apikey"
	"github.com/OneBusAway/sidecar/internal/auth"
	"github.com/OneBusAway/sidecar/internal/clientip"
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
	Auth auth.Repository
	// APIKeys backs bearer authentication and the region API key routes
	// (design spec §4.2, §5.6). Nil means bearer auth is not configured: any
	// Authorization header is a 401 -- never a fall-through to the session
	// cookie -- and the key routes are not registered. main always sets it.
	APIKeys apikey.Repository
	// BearerFailLimiter bounds FAILED bearer attempts per peer address.
	// NewRouter defaults it (60/minute); successful calls are never charged,
	// so a busy consumer is unmetered.
	BearerFailLimiter *ratelimit.Limiter
	Now               func() time.Time
	Logger            *slog.Logger
	// ClientIP is the key every per-IP throttle (and the failed-login log
	// line) uses. NewRouter defaults it to the TCP peer, clientip.Peer; main
	// sets a header-reading resolver only when SIDECAR_TRUSTED_PROXY opts in
	// (README, Deployment). Tests inject their own to pin bucket identity.
	ClientIP clientip.Resolver

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

	// AlertPushes backs the feedback webhook's alert-push failure accounting
	// (design spec §2.8) and, together with AlertPushWaker, the admin
	// alert-push routes (§2.9). main always sets it: the webhook must keep
	// accounting even when no transport is configured.
	AlertPushes alertpush.Repository
	// AlertPushWaker is the dispatcher, poked after every enqueue so a send
	// starts at once rather than at the next tick. main sets it only when a
	// push transport is configured, so it doubles as the "pushes can be
	// sent" signal: the admin routes are registered only when both this and
	// AlertPushes are non-nil, and the SPA shows "not configured" instead of
	// letting an operator queue a push that can only fail.
	AlertPushWaker alertpush.Waker

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

// clientIP resolves the throttle key through Deps.ClientIP, defaulting to
// the TCP peer so handlers built without NewRouter (tests) behave like the
// unconfigured server.
func (d Deps) clientIP(r *http.Request) string {
	if d.ClientIP == nil {
		return clientip.Peer(r)
	}
	return d.ClientIP(r)
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
	// through crossSiteGuard or requirePrincipal, both of which assume a JSON
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
		// Charged only on a failed bearer attempt (see rejectBearer), so this
		// bucket never meters a working consumer.
		if deps.BearerFailLimiter == nil {
			deps.BearerFailLimiter = ratelimit.New(bearerFailuresPerMinute, time.Minute)
		}
		// The alert push routes count the audience before they enqueue
		// (design spec §2.9); Alerts, Regions and Now are already guaranteed
		// above, so the registry is the one thing left that would nil-deref
		// inside the first handler instead of at boot.
		if alertPushRoutesEnabled(deps) {
			if missing := missingDeps(map[string]bool{"Deps.PushRegs": deps.PushRegs == nil}); len(missing) > 0 {
				panic("httpapi: " + strings.Join(missing, ", ") +
					" required when Deps.AlertPushes and Deps.AlertPushWaker are set")
			}
		}
		registerAdminRoutes(mux, deps)
	}
	return requestLog(deps, mux)
}

// adminRoute is one admin API route plus the middleware it must carry.
//
// The routes are a table rather than a run of mux.Handle calls for one
// reason: http.ServeMux cannot be enumerated, so a route registered without
// requirePrincipal would be invisible to every test in this package. The
// table is the thing a test can walk, which is what makes "every admin route
// names the credentials it accepts" checkable rather than merely intended.
type adminRoute struct {
	pattern string
	handler http.HandlerFunc
	// allowed is the set of principal kinds this route accepts. A nil set
	// means no principal is required at all, which is true of exactly two
	// routes, both deliberate and both documented at their entry in
	// adminRoutes.
	allowed principalSet
	// scope is which region middleware wraps the handler. It is a column of
	// the table rather than a wrapper spelled at each entry so a test can
	// assert it agrees with the pattern: a route carrying {regionId} without
	// a scope would parse a region id nobody had checked the caller against.
	scope routeScope
}

// adminRoutes is the admin API route table (design spec §5). It takes Deps so
// the handlers close over the same dependencies the router was built with;
// a caller that only wants to inspect the table can pass a zero Deps.
func adminRoutes(deps Deps) []adminRoute {
	session := &sessionHandler{deps: deps}
	alertsAdmin := &adminAlertsHandler{deps: deps}
	regionsAdmin := &adminRegionsHandler{deps: deps}

	routes := []adminRoute{
		// Login has no session to require yet; the cross-site guard is what
		// protects it (design spec §4.4).
		{"POST /api/admin/v1/session", session.login, nil, scopeNone},
		// Logout sits outside requirePrincipal so it stays idempotent: the
		// SPA calls it to tidy up after a 401, and answering that with
		// another 401 gives the client nothing it can act on. It is not a
		// hole -- the handler only ever deletes the session named by the
		// token the caller presented, and the cross-site guard still covers
		// it.
		{"DELETE /api/admin/v1/session", session.logout, nil, scopeNone},
		// whoami answers "which operator am I", which only an operator has.
		{"GET /api/admin/v1/session", session.whoami, operatorOnly, scopeNone},

		// The region list is cross-region by construction: it is the one
		// admin route that hands back every region at once, so a key scoped
		// to a single region has no business reading it -- and it is the one
		// region route that therefore cannot be region-scoped.
		{"GET /api/admin/v1/regions", regionsAdmin.list, operatorOnly, scopeNone},
		{"GET /api/admin/v1/regions/{regionId}", regionsAdmin.get, operatorOrKey, scopeRegion},
		{"PATCH /api/admin/v1/regions/{regionId}", regionsAdmin.patch, operatorOrKey, scopeRegion},

		{"GET /api/admin/v1/regions/{regionId}/alerts", alertsAdmin.list, operatorOrKey, scopeRegion},
		{"POST /api/admin/v1/regions/{regionId}/alerts", alertsAdmin.create, operatorOrKey, scopeRegion},
		{"GET /api/admin/v1/regions/{regionId}/alerts/{id}", alertsAdmin.get, operatorOrKey, scopeRegion},
		{"PATCH /api/admin/v1/regions/{regionId}/alerts/{id}", alertsAdmin.patch, operatorOrKey, scopeRegion},
		{"DELETE /api/admin/v1/regions/{regionId}/alerts/{id}", alertsAdmin.delete, operatorOrKey, scopeRegion},
		{"POST /api/admin/v1/regions/{regionId}/alerts/{id}/publish", alertsAdmin.setPublished(true), operatorOrKey, scopeRegion},
		{"POST /api/admin/v1/regions/{regionId}/alerts/{id}/unpublish", alertsAdmin.setPublished(false), operatorOrKey, scopeRegion},
		{"PUT /api/admin/v1/regions/{regionId}/alerts/{id}/translations/{lang}", alertsAdmin.putTranslation, operatorOrKey, scopeRegion},
		{"DELETE /api/admin/v1/regions/{regionId}/alerts/{id}/translations/{lang}", alertsAdmin.deleteTranslation, operatorOrKey, scopeRegion},
	}

	// The alert push routes are conditional (design spec §2.9) but still
	// come from this one table: a route mounted outside it would be
	// invisible to the tests that prove every admin route carries its
	// middleware.
	if alertPushRoutesEnabled(deps) {
		pushesAdmin := &adminPushesHandler{deps: deps}
		// Sending and cancelling a push are operator-only: they reach every
		// rider's device, which is the one blast radius a leaked region key
		// must not have (design spec §4.5). Reading what was sent, and
		// counting the audience beforehand, stay open to a region key.
		routes = append(routes,
			adminRoute{"POST /api/admin/v1/regions/{regionId}/alerts/{id}/pushes", pushesAdmin.create, operatorOnly, scopeRegion},
			adminRoute{"GET /api/admin/v1/regions/{regionId}/alerts/{id}/pushes", pushesAdmin.list, operatorOrKey, scopeRegion},
			adminRoute{"DELETE /api/admin/v1/regions/{regionId}/alerts/{id}/pushes/{pushId}", pushesAdmin.cancel, operatorOnly, scopeRegion},
			adminRoute{"GET /api/admin/v1/regions/{regionId}/alerts/{id}/push_audience", pushesAdmin.audience, operatorOrKey, scopeRegion},
		)
	}

	// The study and survey authoring family (design spec section 2.13,
	// section 5.7). Gated on deps.Surveys, the same field that gates the
	// rider-facing survey feed above, whose block already guarantees
	// Deps.Now and Deps.Regions.
	if deps.Surveys != nil {
		surveysAdmin := &adminSurveysHandler{deps: deps}
		responsesAdmin := &adminResponsesHandler{deps: deps}
		routes = append(routes,
			adminRoute{"GET /api/admin/v1/regions/{regionId}/studies", surveysAdmin.listStudies, operatorOrKey, scopeRegion},
			adminRoute{"POST /api/admin/v1/regions/{regionId}/studies", surveysAdmin.createStudy, operatorOrKey, scopeRegion},
			adminRoute{"GET /api/admin/v1/regions/{regionId}/studies/{id}", surveysAdmin.getStudy, operatorOrKey, scopeRegion},
			adminRoute{"PATCH /api/admin/v1/regions/{regionId}/studies/{id}", surveysAdmin.patchStudy, operatorOrKey, scopeRegion},
			adminRoute{"GET /api/admin/v1/regions/{regionId}/surveys", surveysAdmin.listSurveys, operatorOrKey, scopeRegion},
			adminRoute{"POST /api/admin/v1/regions/{regionId}/surveys", surveysAdmin.createSurvey, operatorOrKey, scopeRegion},
			adminRoute{"GET /api/admin/v1/regions/{regionId}/surveys/{id}", surveysAdmin.getSurvey, operatorOrKey, scopeRegion},
			adminRoute{"PUT /api/admin/v1/regions/{regionId}/surveys/{id}", surveysAdmin.putSurvey, operatorOrKey, scopeRegion},
			adminRoute{"DELETE /api/admin/v1/regions/{regionId}/surveys/{id}", surveysAdmin.deleteSurvey, operatorOrKey, scopeRegion},
			// Responses (design spec section 2.14): read-only, so a region
			// key reads them exactly like it reads the survey itself.
			adminRoute{"GET /api/admin/v1/regions/{regionId}/surveys/{id}/responses", responsesAdmin.listResponses, operatorOrKey, scopeRegion},
			adminRoute{"GET /api/admin/v1/regions/{regionId}/surveys/{id}/responses.csv", responsesAdmin.responsesCSV, operatorOrKey, scopeRegion},
			adminRoute{"GET /api/admin/v1/regions/{regionId}/survey_responses/{publicId}", responsesAdmin.getResponse, operatorOrKey, scopeRegion},
		)
	}

	// The ghost bus report read surface (design spec section 5): the JSON
	// list, the CSV export, and lookup by public id. Gated on deps.GhostBus,
	// the same field that gates the rider-facing create route above, whose
	// block already guarantees Deps.Now and Deps.Regions. Read-only: reports
	// are rider-submitted, so there is no admin write route here and the
	// rider-facing create route's 422 already_reported contract is
	// untouched.
	if deps.GhostBus != nil {
		ghostBusAdmin := &adminGhostBusHandler{deps: deps}
		routes = append(routes,
			adminRoute{"GET /api/admin/v1/regions/{regionId}/ghost_bus_reports", ghostBusAdmin.list, operatorOrKey, scopeRegion},
			adminRoute{"GET /api/admin/v1/regions/{regionId}/ghost_bus_reports.csv", ghostBusAdmin.csv, operatorOrKey, scopeRegion},
			adminRoute{"GET /api/admin/v1/regions/{regionId}/ghost_bus_reports/{publicId}", ghostBusAdmin.get, operatorOrKey, scopeRegion},
		)
	}

	// The key-management family is the one place a service principal is
	// granted anything, and the one family a region key must never reach --
	// hence scopeKeyAdmin rather than scopeRegion (design spec §5.6).
	if deps.APIKeys != nil {
		keysAdmin := &adminAPIKeysHandler{deps: deps}
		routes = append(routes,
			adminRoute{"POST /api/admin/v1/regions/{regionId}/api_keys", keysAdmin.create, operatorOrService, scopeKeyAdmin},
			adminRoute{"GET /api/admin/v1/regions/{regionId}/api_keys", keysAdmin.list, operatorOrService, scopeKeyAdmin},
			adminRoute{"DELETE /api/admin/v1/regions/{regionId}/api_keys/{keyId}", keysAdmin.revoke, operatorOrService, scopeKeyAdmin},
		)
	}

	// The alarm read-only surface (design spec section 5.4), gated on the same
	// deps.Alarms field the rider-facing v1/v2 alarm routes above are gated
	// on. NewRouter already panics at boot when Deps.Alarms is set without
	// PushRegs, Regions, and Now, so no additional guard belongs here.
	if deps.Alarms != nil {
		alarmsAdmin := &adminAlarmsHandler{deps: deps}
		routes = append(routes,
			adminRoute{"GET /api/admin/v1/regions/{regionId}/alarms", alarmsAdmin.list, operatorOrKey, scopeRegion},
			adminRoute{"GET /api/admin/v1/regions/{regionId}/alarms/{id}", alarmsAdmin.get, operatorOrKey, scopeRegion},
		)
	}

	// Push registration counts (design spec section 5.5), gated on
	// deps.PushRegs directly rather than on deps.Alarms: a deployment can run
	// push registrations without alarms, and this route's only dependency is
	// the registration store.
	if deps.PushRegs != nil {
		pushRegsAdmin := &adminPushRegsHandler{deps: deps}
		routes = append(routes,
			adminRoute{"GET /api/admin/v1/regions/{regionId}/push_registrations/count", pushRegsAdmin.count, operatorOrKey, scopeRegion},
		)
	}
	return routes
}

// alertPushRoutesEnabled reports whether a push can actually be sent. The
// waker is the dispatcher, which main supplies only when a transport is
// configured: without it the routes must not exist, so an operator gets
// "not configured" from the SPA rather than a push that sits queued forever.
func alertPushRoutesEnabled(deps Deps) bool {
	return deps.AlertPushes != nil && deps.AlertPushWaker != nil
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
// guard is separate from requirePrincipal (design spec §4.4) -- and
// everything except login and logout also requires an authenticated
// principal drawn from the route's own allow-list.
//
// The region scope is wrapped INSIDE requirePrincipal, because deciding
// whether a caller may reach a region means first knowing who the caller is.
//
// Every registration must come from adminRoutes; see routeRegistrar.
func registerAdminRoutes(mux routeRegistrar, deps Deps) {
	mw := &authMiddleware{deps: deps}
	for _, route := range adminRoutes(deps) {
		var h http.Handler = route.handler
		switch route.scope {
		case scopeRegion:
			h = mw.requireRegion(h)
		case scopeKeyAdmin:
			h = mw.requireKeyAdminRegion(h)
		case scopeNone:
			// No {regionId} to fence; the route-table test proves the
			// pattern agrees.
		}
		if route.allowed != nil {
			h = mw.requirePrincipal(route.allowed, h)
		}
		mux.Handle(route.pattern, crossSiteGuard(deps.Logger, h))
	}
}

// adminFeatures lists the admin route families registered in this
// deployment (design spec section 5.1), so a consumer can tell "family not
// enabled here" from a 404. It is derived from the same Deps fields the
// route table gates on, so the two cannot drift.
func adminFeatures(deps Deps) []string {
	features := []string{"alerts"}
	if alertPushRoutesEnabled(deps) {
		features = append(features, "pushes")
	}
	if deps.Surveys != nil {
		features = append(features, "surveys")
	}
	if deps.GhostBus != nil {
		features = append(features, "ghost_bus_reports")
	}
	if deps.Alarms != nil {
		features = append(features, "alarms")
	}
	if deps.PushRegs != nil {
		features = append(features, "push_registrations")
	}
	if deps.APIKeys != nil {
		// APIKeys also backs bearer authentication, and now the
		// key-management routes themselves; the two are wired from the same
		// field, so this cannot drift from adminRoutes' own APIKeys gate.
		features = append(features, "api_keys")
	}
	return features
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
