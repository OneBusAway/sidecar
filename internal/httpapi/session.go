package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/OneBusAway/sidecar/internal/auth"
	"github.com/OneBusAway/sidecar/internal/ratelimit"
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
// Every failure -- unknown username, wrong password, blank credentials, a
// password changed while it was being verified -- returns the same status and
// the same bytes after the same argon2 cost and the same fixed delay, so the
// endpoint cannot be used to discover which accounts exist (design spec §4.3).
//
// Failures are also charged to a per-address and a per-username budget, and
// a spent budget is a 429 before anything is hashed (SC-02); the 429 is
// decided on the address and the typed username alone, so it is identical
// for real and unknown accounts. Verifications themselves run under a
// process-wide semaphore, so a flood from many addresses cannot run more
// than LoginVerifySlots argon2 derivations at once.
func (h *sessionHandler) login(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := decodeJSON(w, r, maxAuthBody, &req); err != nil {
		writeJSONError(w, h.deps.Logger, http.StatusBadRequest, err.Error())
		return
	}

	ip, userKey := h.deps.clientIP(r), loginUserKey(req.Username)
	if h.throttled(ip, userKey) {
		h.deps.Logger.Debug("httpapi: login throttled", "remote", ip)
		w.Header().Set("Retry-After", strconv.Itoa(int(loginFailureWindow/time.Second)))
		writeJSONError(w, h.deps.Logger, http.StatusTooManyRequests, "too many failed sign-in attempts; try again later")
		return
	}

	fail := func() {
		// Charged before the delay, so a burst of concurrent guesses is
		// counted as soon as each one is known to have failed.
		h.deps.LoginIPLimiter.Allow(ip, h.deps.Now())
		h.deps.LoginUserLimiter.Allow(userKey, h.deps.Now())
		h.deps.Sleep(h.deps.FailDelay)
		h.deps.Logger.Warn("httpapi: failed login",
			"username", auth.LogSafeUsername(req.Username), "remote", ip)
		writeJSONError(w, h.deps.Logger, http.StatusUnauthorized, "invalid credentials")
	}

	ctx := r.Context()
	user, err := h.deps.Auth.GetUserByUsername(ctx, req.Username)
	if err != nil && !errors.Is(err, auth.ErrNotFound) {
		serverErrorJSON(w, h.deps.Logger, "get user", err)
		return
	}
	known := err == nil

	release, ok := h.acquireVerifySlot(ctx)
	if !ok {
		h.deps.Logger.Warn("httpapi: login verification queue full", "remote", ip)
		writeJSONError(w, h.deps.Logger, http.StatusServiceUnavailable, "sign-in is busy; try again shortly")
		return
	}
	if !known {
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
		release()
		fail()
		return
	}

	ok, err = h.deps.VerifyPassword(user.PasswordHash, req.Password)
	release()
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
	if err := h.deps.Auth.CreateSession(ctx, tokenHash, user.ID, user.PasswordHash, now, now.Add(auth.SessionLifetime)); err != nil {
		if errors.Is(err, auth.ErrPasswordChanged) || errors.Is(err, auth.ErrNotFound) {
			// `user passwd` or `user delete` committed while this login was
			// verifying, so the password it checked no longer opens the
			// account. Report it exactly like a wrong password.
			fail()
			return
		}
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

// throttled reports whether a login from ip for userKey must be refused
// before hashing (SC-02).
//
// The address budget is a plain brake: an address that has spent it waits
// out the window. The username budget is what stops a guesser spreading one
// account's attempts across many addresses -- but applied the same way it
// would let anyone lock the operator out of their own account, forever, by
// hammering the username. So it is a throttle, not a lockout: once a
// username's budget is spent, only addresses that have themselves failed
// recently are refused. An address with a clean record still has its attempt
// verified, which is one guess per clean address per window for an attacker
// (each failure marks the address) and no lockout for an operator signing in
// from somewhere that has not been guessing. The trade-off is documented in
// the README (Admin API hardening).
func (h *sessionHandler) throttled(ip, userKey string) bool {
	now := h.deps.Now()
	if h.deps.LoginIPLimiter.Exhausted(ip, now) {
		return true
	}
	return h.deps.LoginUserLimiter.Exhausted(userKey, now) && h.deps.LoginIPLimiter.Count(ip, now) > 0
}

// acquireVerifySlot takes one of the LoginVerifySlots verification slots,
// waiting at most LoginVerifyWait (or until the client goes away). It
// returns the release function and true, or false when no slot came free.
func (h *sessionHandler) acquireVerifySlot(ctx context.Context) (func(), bool) {
	ctx, cancel := context.WithTimeout(ctx, h.deps.LoginVerifyWait)
	defer cancel()
	select {
	case h.deps.loginSlots <- struct{}{}:
		return func() { <-h.deps.loginSlots }, true
	case <-ctx.Done():
		return nil, false
	}
}

// loginUserKey is the per-username bucket key: the normalized form, so case
// and padding cannot mint fresh buckets, cut to the longest valid username
// so an 8 KB "username" cannot become an 8 KB map key. (Every name longer
// than that is invalid, so the cut only ever merges invalid names.)
func loginUserKey(username string) string {
	u := auth.NormalizeUsername(username)
	if len(u) > auth.MaxUsernameLen {
		u = u[:auth.MaxUsernameLen]
	}
	return u
}

// applyLoginDefaults fills the SC-02 throttle and semaphore settings a
// caller left zero, and builds the semaphore.
func applyLoginDefaults(d *Deps) {
	if d.LoginIPLimiter == nil {
		d.LoginIPLimiter = ratelimit.New(loginFailuresPerIP, loginFailureWindow)
	}
	if d.LoginUserLimiter == nil {
		d.LoginUserLimiter = ratelimit.New(loginFailuresPerUser, loginFailureWindow)
	}
	if d.LoginVerifySlots <= 0 {
		d.LoginVerifySlots = loginVerifySlots
	}
	if d.LoginVerifyWait <= 0 {
		d.LoginVerifyWait = loginVerifyWait
	}
	d.loginSlots = make(chan struct{}, d.LoginVerifySlots)
}

// Login throttle defaults (SC-02). Fixed windows, charged on failure only.
const (
	loginFailuresPerIP   = 10
	loginFailuresPerUser = 20
	loginFailureWindow   = 15 * time.Minute
	loginVerifySlots     = 2
	loginVerifyWait      = 10 * time.Second
)
