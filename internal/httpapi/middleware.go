package httpapi

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/OneBusAway/sidecar/internal/auth"
)

// contextKey is this package's private context key type, so no other package
// can collide with (or read) the values requirePrincipal stores.
type contextKey int

const (
	// principalContextKey holds the principal requirePrincipal attached.
	principalContextKey contextKey = iota + 1
	// regionContextKey holds the regions.Region a region scope loaded. It is
	// the only region a scoped handler may act on: fetching one by hand is
	// how tenancy becomes a check per handler instead of a check per route.
	regionContextKey
)

// crossSiteGuard rejects state-changing requests that the browser itself
// marked as cross-site. It applies to ALL admin routes including POST
// /session: login CSRF (logging a victim into the attacker's account) is
// cheap to close, and the login handler deliberately sits outside
// requirePrincipal, so this check must not be coupled to that middleware
// (design spec §4.4).
//
// The rule: if Sec-Fetch-Site is present it must be same-origin or none;
// otherwise, if Origin is present its host must equal the request Host.
// Requests with neither header -- curl, the CLI, any non-browser client --
// pass, because they carry no ambient credentials an attacker could ride.
//
// The Origin-vs-Host fallback requires the deployment's reverse proxy to
// preserve the public Host header (nginx: proxy_set_header Host $host; its
// default rewrites Host to the upstream address, which would 403 every admin
// write) -- a deployment requirement that belongs beside the
// X-Forwarded-Proto trust note in the README.
func crossSiteGuard(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if sfs := r.Header.Get("Sec-Fetch-Site"); sfs != "" {
				if sfs != "same-origin" && sfs != "none" {
					rejectCrossSite(w, logger, r, "Sec-Fetch-Site="+sfs)
					return
				}
			} else if origin := r.Header.Get("Origin"); origin != "" {
				u, err := url.Parse(origin)
				if err != nil || u.Host != r.Host {
					rejectCrossSite(w, logger, r, "Origin="+origin)
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

// rejectCrossSite writes the 403 and logs why, including the request Host:
// the most likely cause in practice is a proxy that rewrote Host rather than
// an actual attack, and that misconfiguration is invisible without both
// values side by side.
func rejectCrossSite(w http.ResponseWriter, logger *slog.Logger, r *http.Request, reason string) {
	logger.Warn("httpapi: cross-site request rejected",
		"method", r.Method, "path", r.URL.Path, "host", r.Host, "reason", reason)
	writeJSONError(w, logger, http.StatusForbidden, "cross-site request rejected")
}

// authMiddleware turns the session cookie into an authenticated user.
type authMiddleware struct {
	deps Deps
}

// requirePrincipal authenticates a request and enforces the route's
// allow-list. It replaces requireSession: the admin API now has three kinds
// of caller, and a route says which it accepts rather than every handler
// re-deriving it.
func (h *authMiddleware) requirePrincipal(allowed principalSet, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var (
			p  principal
			ok bool
		)
		// A present Authorization header means cookies are ignored entirely
		// (design spec §4.2). r.Header.Values, not Get: a present-but-empty
		// value is a failed bearer attempt, not "absent".
		if values, present := r.Header["Authorization"]; present {
			p, ok = h.authenticateBearer(w, r, values)
		} else {
			p, ok = h.authenticateSession(w, r)
		}
		if !ok {
			return
		}
		if !allowed.admits(p) {
			h.deps.Logger.Warn("httpapi: principal not allowed on route",
				"principal", p, "path", r.URL.Path, "method", r.Method)
			writeJSONError(w, h.deps.Logger, http.StatusForbidden, forbiddenBody)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalContextKey, p)))
	})
}

// authenticateSession resolves the session cookie into an operator
// principal. It writes the response itself on every failure and reports
// ok=false, so requirePrincipal only has to decide whether to continue.
//
// Missing, unknown, and expired sessions are one 401 with one message:
// GetSession's contract folds expiry into ErrNotFound (and deletes the row on
// the way), so there is nothing here to distinguish.
func (h *authMiddleware) authenticateSession(w http.ResponseWriter, r *http.Request) (principal, bool) {
	cookie, err := r.Cookie(sessionCookieName(r))
	if err != nil {
		h.unauthorized(w, "no session cookie")
		return principal{}, false
	}

	ctx := r.Context()
	now := h.deps.Now()
	tokenHash := auth.HashToken(cookie.Value)
	session, err := h.deps.Auth.GetSession(ctx, tokenHash, now)
	if err != nil {
		if !errors.Is(err, auth.ErrNotFound) {
			// A broken store is not a logged-out user. Saying 401 here
			// would send an operator hunting for an expired session that
			// never expired.
			serverErrorJSON(w, h.deps.Logger, "get session", err)
			return principal{}, false
		}
		h.unauthorized(w, "no live session for cookie")
		return principal{}, false
	}
	if !h.liveAndSlid(r, w, session, now) {
		return principal{}, false
	}

	user, err := h.deps.Auth.GetUserByID(ctx, session.UserID)
	if err != nil {
		if !errors.Is(err, auth.ErrNotFound) {
			serverErrorJSON(w, h.deps.Logger, "get session user", err)
			return principal{}, false
		}
		// The user row is gone but its session outlived it. Treat it as
		// logged out, and say so: it means a delete failed to cascade.
		h.deps.Logger.Warn("httpapi: session references a missing user", "user_id", session.UserID)
		h.unauthorized(w, "session user no longer exists")
		return principal{}, false
	}

	return principal{kind: principalOperator, user: user}, true
}

// unauthorized writes the single 401 body the SPA keys off (design spec §4.5)
// and logs the reason, which the client never sees.
func (h *authMiddleware) unauthorized(w http.ResponseWriter, reason string) {
	h.deps.Logger.Debug("httpapi: authentication required", "reason", reason)
	writeJSONError(w, h.deps.Logger, http.StatusUnauthorized, "authentication required")
}

// liveAndSlid applies SC-25's two clocks to a session GetSession reported
// live. The absolute lifetime is measured from created_at here, at request
// time, rather than trusted from expires_at, so lowering
// --admin-session-lifetime also ends sessions minted under a longer one. The
// idle timeout lives in expires_at, which this slides forward to now+idle
// (capped at the absolute deadline). The write is skipped unless it would
// move the deadline by more than a twentieth of the idle window -- or pull
// it in, which a row minted under longer settings needs once -- so a burst
// of requests costs one UPDATE, not one each. A failed slide is logged and
// the request proceeds: the session is live right now, and the worst case
// is that it idles out up to one granule early.
//
// It writes the 401 itself and reports false when the session is past its
// absolute lifetime.
func (h *authMiddleware) liveAndSlid(r *http.Request, w http.ResponseWriter, s auth.Session, now time.Time) bool {
	ctx := r.Context()
	// Zero means a hand-built middleware (tests); NewRouter always sets both.
	lifetime := cmp.Or(h.deps.SessionLifetime, auth.SessionLifetime)
	idle := cmp.Or(h.deps.SessionIdleTimeout, auth.SessionIdleTimeout)
	hardDeadline := s.CreatedAt.Add(lifetime)
	if !now.Before(hardDeadline) {
		if err := h.deps.Auth.DeleteSession(ctx, s.TokenHash); err != nil && !errors.Is(err, auth.ErrNotFound) {
			h.deps.Logger.Warn("httpapi: delete session past its lifetime", "err", err)
		}
		h.unauthorized(w, "session past its absolute lifetime")
		return false
	}
	next := now.Add(idle)
	if next.After(hardDeadline) {
		next = hardDeadline
	}
	granule := max(idle/20, time.Second)
	if next.Sub(s.ExpiresAt) >= granule || next.Before(s.ExpiresAt) {
		if err := h.deps.Auth.ExtendSession(ctx, s.TokenHash, next); err != nil {
			h.deps.Logger.Warn("httpapi: slide session idle deadline", "err", err)
		}
	}
	return true
}
