package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"

	"github.com/OneBusAway/sidecar/internal/auth"
)

// contextKey is this package's private context key type, so no other package
// can collide with (or read) the values requirePrincipal stores.
type contextKey int

const (
	// principalContextKey holds the principal requirePrincipal attached.
	principalContextKey contextKey = iota + 1
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
		if !allowed.has(p.kind) {
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
	cookie, err := r.Cookie(auth.CookieName)
	if err != nil {
		h.unauthorized(w, "no session cookie")
		return principal{}, false
	}

	ctx := r.Context()
	session, err := h.deps.Auth.GetSession(ctx, auth.HashToken(cookie.Value), h.deps.Now())
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
