package httpapi

import (
	"errors"
	"net/http"
	"time"

	"github.com/OneBusAway/sidecar/internal/auth"
)

// maxAuthBody caps the request body on the auth endpoints at 8 KB. Credentials
// are small; anything larger is either a mistake or an attempt to make the
// server buffer for free.
const maxAuthBody = 8192

// sessionHandler serves the three session endpoints: login, logout, and the
// whoami check the SPA makes on boot (design spec §4.5).
type sessionHandler struct {
	deps Deps
}

// loginRequest is the POST /session body. Unknown fields are ignored.
type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// login handles POST /api/admin/v1/session.
//
// Every failure -- unknown username, wrong password, blank credentials --
// returns the same status and the same bytes after the same argon2 cost and
// the same fixed delay, so the endpoint cannot be used to discover which
// accounts exist (design spec §4.3).
func (h *sessionHandler) login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := decodeJSON(w, r, maxAuthBody, &req); err != nil {
		writeJSONError(w, h.deps.Logger, http.StatusBadRequest, err.Error())
		return
	}

	fail := func() {
		h.deps.Sleep(h.deps.FailDelay)
		h.deps.Logger.Warn("httpapi: failed login",
			"username", auth.NormalizeUsername(req.Username), "remote", r.RemoteAddr)
		writeJSONError(w, h.deps.Logger, http.StatusUnauthorized, "invalid credentials")
	}

	ctx := r.Context()
	user, err := h.deps.Auth.GetUserByUsername(ctx, req.Username)
	if err != nil {
		if !errors.Is(err, auth.ErrNotFound) {
			serverErrorJSON(w, h.deps.Logger, "get user", err)
			return
		}
		// Burn the same argon2 cost as a real verification so response timing
		// does not reveal which usernames exist (design spec §4.3). This runs
		// through the injected Deps.VerifyPassword purely so a test can prove
		// it happened: skipping it changes no status, no body, and no log, so
		// the timing oracle would otherwise reopen invisibly.
		if _, dummyErr := h.deps.VerifyPassword(auth.DummyPHC, req.Password); dummyErr != nil {
			// auth.DummyPHC is a compile-time constant in this repo's own
			// auth package; a parse failure means it was edited into
			// something invalid, which silently removes the timing defence.
			h.deps.Logger.Error("httpapi: dummy password hash is unusable", "err", dummyErr)
		}
		fail()
		return
	}

	ok, err := h.deps.VerifyPassword(user.PasswordHash, req.Password)
	if err != nil {
		serverErrorJSON(w, h.deps.Logger, "verify password", err)
		return
	}
	if !ok {
		fail()
		return
	}

	token, tokenHash, err := auth.NewToken()
	if err != nil {
		serverErrorJSON(w, h.deps.Logger, "mint session token", err)
		return
	}
	now := h.deps.Now()
	if err := h.deps.Auth.CreateSession(ctx, tokenHash, user.ID, now, now.Add(auth.SessionLifetime)); err != nil {
		serverErrorJSON(w, h.deps.Logger, "create session", err)
		return
	}

	// Lazy garbage collection: this is the only thing that bulk-deletes
	// expired sessions (design spec §4.2). Best-effort -- a sweep failure
	// must never cost the operator their login.
	if _, err := h.deps.Auth.DeleteExpiredSessions(ctx, now); err != nil {
		h.deps.Logger.Warn("httpapi: sweep expired sessions", "err", err)
	}

	http.SetCookie(w, sessionCookie(r, token, int(auth.SessionLifetime/time.Second)))
	writeJSON(w, h.deps.Logger, http.StatusOK, map[string]string{"username": user.Username})
}

// logout handles DELETE /api/admin/v1/session. It is idempotent: logging out
// without a live session is a success, because the SPA calls it to tidy up
// after a 401.
func (h *sessionHandler) logout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(auth.CookieName); err == nil {
		err := h.deps.Auth.DeleteSession(r.Context(), auth.HashToken(cookie.Value))
		if err != nil && !errors.Is(err, auth.ErrNotFound) {
			// The row is still there, so the token is still live. Clearing
			// the cookie now would take away the only handle the operator has
			// on a session they just asked to revoke.
			serverErrorJSON(w, h.deps.Logger, "delete session", err)
			return
		}
	}

	http.SetCookie(w, sessionCookie(r, "", -1))
	w.WriteHeader(http.StatusNoContent)
}

// whoami handles GET /api/admin/v1/session, so the SPA can answer "am I
// logged in?" on boot without a sacrificial data request (design spec §4.5).
func (h *sessionHandler) whoami(w http.ResponseWriter, r *http.Request) {
	p, ok := principalFrom(r.Context())
	if !ok {
		// Unreachable through the router: this route is registered behind
		// requirePrincipal. Reaching it means a route lost its middleware,
		// and answering 200 with an empty username would hide that.
		serverErrorJSON(w, h.deps.Logger, "whoami reached without an authenticated principal",
			errors.New("no principal on request context"))
		return
	}
	if p.kind != principalOperator {
		// Equally unreachable: this route's allow-list is operatorOnly, and
		// only an operator has a username to report. A non-operator here
		// means the route lost its allow-list, which a 200 carrying an empty
		// username would hide just as effectively.
		serverErrorJSON(w, h.deps.Logger, "whoami reached by a non-operator principal",
			errors.New("principal kind "+p.kind.String()))
		return
	}
	writeJSON(w, h.deps.Logger, http.StatusOK, map[string]string{"username": p.user.Username})
}

// sessionCookie builds the session cookie per design spec §4.2. A negative
// maxAge with an empty value is the clearing form logout sends.
//
// Secure is set only for requests that really are HTTPS -- directly, or
// terminated at a reverse proxy that says so with X-Forwarded-Proto -- because
// a Secure cookie on a plain-HTTP development server is one the browser will
// accept and then never send back.
func sessionCookie(r *http.Request, value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     auth.CookieName,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   maxAge,
		Secure:   r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
	}
}
