package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/auth"
	"github.com/OneBusAway/sidecar/internal/regions"
	"github.com/OneBusAway/sidecar/internal/store/sqlitetest"
)

// testNow is the fixed instant Deps.Now returns in this package's tests; the
// httpapi package must never read the wall clock.
var testNow = time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)

func discardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// stubAuth is an in-memory auth.Repository. These handler tests are about
// HTTP semantics -- status codes, cookies, headers, indistinguishable failure
// bodies -- so they use maps instead of SQLite; Task 6 exercises the real
// store. Every field is guarded by mu because -race runs the suite and the
// stub is shared across a request's middleware and handler.
type stubAuth struct {
	mu          sync.Mutex
	usersByName map[string]auth.User
	usersByID   map[int64]auth.User
	sessions    map[string]auth.Session
	nextID      int64

	// Forced failures, for the store-error paths.
	getUserErr       error
	getUserByIDErr   error
	getSessionErr    error
	createSessionErr error
	deleteSessionErr error

	// Call counters.
	gcCalls int
}

func newStubAuth() *stubAuth {
	return &stubAuth{
		usersByName: map[string]auth.User{},
		usersByID:   map[int64]auth.User{},
		sessions:    map[string]auth.Session{},
	}
}

// addUser inserts a user with an already-hashed password.
func (s *stubAuth) addUser(username, passwordHash string) auth.User {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	u := auth.User{
		ID:           s.nextID,
		Username:     auth.NormalizeUsername(username),
		PasswordHash: passwordHash,
		CreatedAt:    testNow,
		UpdatedAt:    testNow,
	}
	s.usersByName[u.Username] = u
	s.usersByID[u.ID] = u
	return u
}

// addSession inserts a session directly, so tests can build expired ones.
func (s *stubAuth) addSession(tokenHash string, userID int64, expiresAt time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[tokenHash] = auth.Session{
		TokenHash: tokenHash,
		UserID:    userID,
		CreatedAt: testNow,
		ExpiresAt: expiresAt,
	}
}

func (s *stubAuth) sessionCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sessions)
}

func (s *stubAuth) sessionHashes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	hashes := make([]string, 0, len(s.sessions))
	for h := range s.sessions {
		hashes = append(hashes, h)
	}
	return hashes
}

func (s *stubAuth) gcCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gcCalls
}

var errStubUnused = errors.New("stubAuth: method not used by httpapi tests")

func (s *stubAuth) CreateUser(context.Context, string, string, time.Time) (auth.User, error) {
	return auth.User{}, errStubUnused
}

func (s *stubAuth) GetUserByUsername(_ context.Context, username string) (auth.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.getUserErr != nil {
		return auth.User{}, s.getUserErr
	}
	u, ok := s.usersByName[auth.NormalizeUsername(username)]
	if !ok {
		return auth.User{}, auth.ErrNotFound
	}
	return u, nil
}

func (s *stubAuth) GetUserByID(_ context.Context, id int64) (auth.User, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.getUserByIDErr != nil {
		return auth.User{}, s.getUserByIDErr
	}
	u, ok := s.usersByID[id]
	if !ok {
		return auth.User{}, auth.ErrNotFound
	}
	return u, nil
}

func (s *stubAuth) ListUsers(context.Context) ([]auth.User, error) { return nil, errStubUnused }

func (s *stubAuth) DeleteUser(context.Context, string) error { return errStubUnused }

func (s *stubAuth) UpdatePassword(context.Context, string, string, time.Time) error {
	return errStubUnused
}

func (s *stubAuth) CreateSession(_ context.Context, tokenHash string, userID int64, now, expiresAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.createSessionErr != nil {
		return s.createSessionErr
	}
	s.sessions[tokenHash] = auth.Session{TokenHash: tokenHash, UserID: userID, CreatedAt: now, ExpiresAt: expiresAt}
	return nil
}

// GetSession mirrors the Repository contract: unknown or expired tokens are
// both ErrNotFound, and an expired row is deleted on the way out.
func (s *stubAuth) GetSession(_ context.Context, tokenHash string, now time.Time) (auth.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.getSessionErr != nil {
		return auth.Session{}, s.getSessionErr
	}
	sess, ok := s.sessions[tokenHash]
	if !ok {
		return auth.Session{}, auth.ErrNotFound
	}
	if !sess.ExpiresAt.After(now) {
		delete(s.sessions, tokenHash)
		return auth.Session{}, auth.ErrNotFound
	}
	return sess, nil
}

func (s *stubAuth) DeleteSession(_ context.Context, tokenHash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deleteSessionErr != nil {
		return s.deleteSessionErr
	}
	if _, ok := s.sessions[tokenHash]; !ok {
		return auth.ErrNotFound
	}
	delete(s.sessions, tokenHash)
	return nil
}

func (s *stubAuth) DeleteUserSessions(context.Context, int64) (int64, error) {
	return 0, errStubUnused
}

func (s *stubAuth) DeleteExpiredSessions(_ context.Context, now time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcCalls++
	var n int64
	for hash, sess := range s.sessions {
		if !sess.ExpiresAt.After(now) {
			delete(s.sessions, hash)
			n++
		}
	}
	return n, nil
}

// newTestRouter builds the router with the admin routes registered, letting a
// test tweak the deps (FailDelay, Sleep, Now) before NewRouter sees them.
func newTestRouter(repo auth.Repository, mutate func(*Deps)) http.Handler {
	deps := Deps{
		Auth:   repo,
		Now:    func() time.Time { return testNow },
		Logger: discardLogger(),
	}
	if mutate != nil {
		mutate(&deps)
	}
	return NewRouter(deps)
}

// bodyText is the response body with the encoder's trailing newline removed.
func bodyText(rec *httptest.ResponseRecorder) string {
	return strings.TrimSuffix(rec.Body.String(), "\n")
}

const crossSiteBody = `{"error":"cross-site request rejected"}`

// TestCrossSiteGuard covers the spec §4.4 rule in both directions: the guard
// must reject browser-marked cross-site writes *without running the next
// handler* (a guard that 403s after the side effect is no guard at all), and
// it must not reject the legitimate traffic shapes -- same-origin fetches,
// address-bar navigations (Sec-Fetch-Site: none), and header-less clients
// like curl.
func TestCrossSiteGuard(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		method     string
		host       string
		headers    map[string]string
		wantStatus int
		wantNext   bool
	}{
		{
			name: "GET is never blocked", method: http.MethodGet, host: "sidecar.test",
			headers:    map[string]string{"Sec-Fetch-Site": "cross-site"},
			wantStatus: http.StatusNoContent, wantNext: true,
		},
		{
			name: "HEAD is never blocked", method: http.MethodHead, host: "sidecar.test",
			headers:    map[string]string{"Sec-Fetch-Site": "cross-site"},
			wantStatus: http.StatusNoContent, wantNext: true,
		},
		{
			name: "POST same-origin", method: http.MethodPost, host: "sidecar.test",
			headers:    map[string]string{"Sec-Fetch-Site": "same-origin"},
			wantStatus: http.StatusNoContent, wantNext: true,
		},
		{
			name: "POST none (address bar)", method: http.MethodPost, host: "sidecar.test",
			headers:    map[string]string{"Sec-Fetch-Site": "none"},
			wantStatus: http.StatusNoContent, wantNext: true,
		},
		{
			name: "POST cross-site", method: http.MethodPost, host: "sidecar.test",
			headers:    map[string]string{"Sec-Fetch-Site": "cross-site"},
			wantStatus: http.StatusForbidden, wantNext: false,
		},
		{
			name: "POST same-site", method: http.MethodPost, host: "sidecar.test",
			headers:    map[string]string{"Sec-Fetch-Site": "same-site"},
			wantStatus: http.StatusForbidden, wantNext: false,
		},
		{
			name: "DELETE cross-site", method: http.MethodDelete, host: "sidecar.test",
			headers:    map[string]string{"Sec-Fetch-Site": "cross-site"},
			wantStatus: http.StatusForbidden, wantNext: false,
		},
		{
			// Sec-Fetch-Site wins outright: a matching Origin does not rescue
			// a request the browser already labelled cross-site.
			name: "Sec-Fetch-Site beats a matching Origin", method: http.MethodPost, host: "sidecar.test",
			headers:    map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://sidecar.test"},
			wantStatus: http.StatusForbidden, wantNext: false,
		},
		{
			name: "POST foreign Origin", method: http.MethodPost, host: "sidecar.test",
			headers:    map[string]string{"Origin": "http://evil.test"},
			wantStatus: http.StatusForbidden, wantNext: false,
		},
		{
			name: "POST matching Origin", method: http.MethodPost, host: "sidecar.test",
			headers:    map[string]string{"Origin": "https://sidecar.test"},
			wantStatus: http.StatusNoContent, wantNext: true,
		},
		{
			name: "POST matching Origin with port", method: http.MethodPost, host: "sidecar.test:8080",
			headers:    map[string]string{"Origin": "http://sidecar.test:8080"},
			wantStatus: http.StatusNoContent, wantNext: true,
		},
		{
			name: "POST with no browser headers (curl)", method: http.MethodPost, host: "sidecar.test",
			headers:    nil,
			wantStatus: http.StatusNoContent, wantNext: true,
		},
		{
			// Spec §4.4's documented deployment failure: a reverse proxy that
			// rewrites Host to the upstream address makes every admin write
			// look cross-site. The test exists so the failure is legible.
			name: "proxy rewrote Host", method: http.MethodPost, host: "localhost:8080",
			headers:    map[string]string{"Origin": "https://alerts.example.org"},
			wantStatus: http.StatusForbidden, wantNext: false,
		},
		{
			name: "malformed Origin", method: http.MethodPost, host: "sidecar.test",
			headers:    map[string]string{"Origin": "http://[::1"},
			wantStatus: http.StatusForbidden, wantNext: false,
		},
		{
			name: "empty Origin header", method: http.MethodPost, host: "sidecar.test",
			headers:    map[string]string{"Origin": ""},
			wantStatus: http.StatusNoContent, wantNext: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var called bool
			next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				called = true
				w.WriteHeader(http.StatusNoContent)
			})
			h := crossSiteGuard(discardLogger(), next)

			req := httptest.NewRequestWithContext(context.Background(), tt.method, "/api/admin/v1/session", nil)
			req.Host = tt.host
			for k, v := range tt.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d; body = %s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if called != tt.wantNext {
				t.Errorf("next handler called = %v, want %v", called, tt.wantNext)
			}
			if tt.wantStatus == http.StatusForbidden {
				if got := bodyText(rec); got != crossSiteBody {
					t.Errorf("body = %q, want %q", got, crossSiteBody)
				}
				if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
					t.Errorf("Content-Type = %q, want application/json", ct)
				}
			}
		})
	}
}

// TestCrossSiteGuard_AppliesToLogin pins the spec §4.4 requirement that the
// guard covers POST /session too: login CSRF logs a victim into the
// attacker's account, and login deliberately sits outside requireSession, so
// a guard bolted onto requireSession would leave this route open.
func TestCrossSiteGuard_AppliesToLogin(t *testing.T) {
	t.Parallel()

	repo := newStubAuth()
	repo.addUser("admin", testHash())
	h := newTestRouter(repo, nil)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost,
		"/api/admin/v1/session", strings.NewReader(`{"username":"admin","password":"`+testPassword+`"}`))
	req.Host = "sidecar.test"
	req.Header.Set("Origin", "http://evil.test")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body = %s", rec.Code, rec.Body.String())
	}
	if got := bodyText(rec); got != crossSiteBody {
		t.Errorf("body = %q, want %q", got, crossSiteBody)
	}
	if n := repo.sessionCount(); n != 0 {
		t.Errorf("sessions created = %d, want 0: the guard must run before login", n)
	}
	if cookies := rec.Result().Cookies(); len(cookies) != 0 {
		t.Errorf("Set-Cookie = %v, want none", cookies)
	}
}

const unauthorizedBody = `{"error":"authentication required"}`

// TestRequireSession covers the cookie-to-user path: every way of arriving
// without a live session is a 401 with one message, and the middleware must
// actually consult the store rather than waving requests through.
func TestRequireSession(t *testing.T) {
	t.Parallel()

	liveToken, liveHash, err := auth.NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	expiredToken, expiredHash, err := auth.NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	orphanToken, orphanHash, err := auth.NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}

	newRepo := func() *stubAuth {
		repo := newStubAuth()
		u := repo.addUser("admin", testHash())
		repo.addSession(liveHash, u.ID, testNow.Add(time.Hour))
		repo.addSession(expiredHash, u.ID, testNow.Add(-time.Second))
		repo.addSession(orphanHash, 999, testNow.Add(time.Hour)) // user row gone
		return repo
	}

	cookie := func(value string) *http.Cookie {
		return &http.Cookie{Name: auth.CookieName, Value: value}
	}

	tests := []struct {
		name       string
		cookie     *http.Cookie // nil means send no cookie at all
		mutate     func(*stubAuth)
		wantStatus int
		wantBody   string
		wantUser   string
	}{
		{name: "no cookie", cookie: nil, wantStatus: http.StatusUnauthorized, wantBody: unauthorizedBody},
		{name: "garbage token", cookie: cookie("not-a-real-token"), wantStatus: http.StatusUnauthorized, wantBody: unauthorizedBody},
		{name: "empty cookie value", cookie: cookie(""), wantStatus: http.StatusUnauthorized, wantBody: unauthorizedBody},
		{name: "wrong cookie name", cookie: &http.Cookie{Name: "session", Value: liveToken}, wantStatus: http.StatusUnauthorized, wantBody: unauthorizedBody},
		{name: "expired token", cookie: cookie(expiredToken), wantStatus: http.StatusUnauthorized, wantBody: unauthorizedBody},
		{name: "session outlived its user", cookie: cookie(orphanToken), wantStatus: http.StatusUnauthorized, wantBody: unauthorizedBody},
		{
			// A stored hash is not a token: presenting the database value
			// must not authenticate anyone, which is the entire point of
			// storing only the hash.
			name: "stored hash presented as the token", cookie: cookie(liveHash),
			wantStatus: http.StatusUnauthorized, wantBody: unauthorizedBody,
		},
		{
			name: "store failure is not a silent logout", cookie: cookie(liveToken),
			mutate:     func(s *stubAuth) { s.getSessionErr = errors.New("database is locked") },
			wantStatus: http.StatusInternalServerError, wantBody: `{"error":"internal error"}`,
		},
		{
			// The second lookup has the same rule as the first: a broken
			// store is a 500, not a 401 that would read as an expired login.
			name: "user lookup failure is not a silent logout", cookie: cookie(liveToken),
			mutate:     func(s *stubAuth) { s.getUserByIDErr = errors.New("database is locked") },
			wantStatus: http.StatusInternalServerError, wantBody: `{"error":"internal error"}`,
		},
		{name: "live token", cookie: cookie(liveToken), wantStatus: http.StatusOK, wantUser: "admin"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			repo := newRepo()
			if tt.mutate != nil {
				tt.mutate(repo)
			}
			mw := &authMiddleware{deps: Deps{
				Auth:   repo,
				Now:    func() time.Time { return testNow },
				Logger: discardLogger(),
			}}

			var gotUser string
			var gotOK bool
			var called bool
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
				u, ok := userFrom(r.Context())
				gotUser, gotOK = u.Username, ok
				w.WriteHeader(http.StatusOK)
			})

			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/admin/v1/session", nil)
			if tt.cookie != nil {
				req.AddCookie(tt.cookie)
			}
			rec := httptest.NewRecorder()
			mw.requireSession(next).ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if tt.wantStatus != http.StatusOK {
				if called {
					t.Error("next handler ran despite a rejected request")
				}
				if got := bodyText(rec); got != tt.wantBody {
					t.Errorf("body = %q, want %q", got, tt.wantBody)
				}
				return
			}
			if !called {
				t.Fatal("next handler did not run for a live session")
			}
			if !gotOK {
				t.Fatal("userFrom(ctx) reported no user for a live session")
			}
			if gotUser != tt.wantUser {
				t.Errorf("userFrom(ctx).Username = %q, want %q", gotUser, tt.wantUser)
			}
		})
	}
}

// TestRequireSession_ExpiredSessionIsPurged pins the half of §3.2's contract
// the middleware depends on: it does not distinguish expired from unknown
// because GetSession deletes the expired row itself.
func TestRequireSession_ExpiredSessionIsPurged(t *testing.T) {
	t.Parallel()

	token, hash, err := auth.NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	repo := newStubAuth()
	u := repo.addUser("admin", testHash())
	repo.addSession(hash, u.ID, testNow.Add(-time.Second))

	h := newTestRouter(repo, nil)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/admin/v1/session", nil)
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: token})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body = %s", rec.Code, rec.Body.String())
	}
	if n := repo.sessionCount(); n != 0 {
		t.Errorf("expired session still present after lookup: %d rows", n)
	}
}

// TestFeedRoutesStayUnauthenticated is the regression test for the one thing
// this task could break in production: the rider-facing feed is
// unauthenticated by design (design spec §1.3) and must not acquire a session
// requirement or a cross-site guard just because an admin store is now
// configured. It runs against a real store because the point is the whole
// wired router, not a stub.
func TestFeedRoutesStayUnauthenticated(t *testing.T) {
	t.Parallel()

	store := sqlitetest.Open(t)
	ctx := context.Background()
	if err := store.Regions().UpsertFromDirectory(ctx, []regions.Region{{
		ID: 1, Name: "Test Region", OBABaseURL: "https://example.org/", Active: true,
	}}, testNow); err != nil {
		t.Fatalf("UpsertFromDirectory: %v", err)
	}

	h := NewRouter(Deps{
		Alerts:  store.Alerts(),
		Regions: store.Regions(),
		Auth:    newStubAuth(),
		Now:     func() time.Time { return testNow },
		Logger:  discardLogger(),
	})

	for _, target := range []string{"/api/v1/regions/1/alerts", "/api/v1/regions/1/alerts.pbtext"} {
		req := httptest.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		// Hostile-looking headers a browser would attach to a cross-origin
		// embed: the feed is public, so none of them matter here.
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		req.Header.Set("Origin", "http://evil.test")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s: status = %d, want 200; body = %s", target, rec.Code, rec.Body.String())
		}
	}
}

// TestUserFrom_NoUser guards the accessor itself: a context that never went
// through requireSession must report ok=false, not a zero-value user that a
// caller could mistake for an authenticated admin.
func TestUserFrom_NoUser(t *testing.T) {
	t.Parallel()

	if u, ok := userFrom(context.Background()); ok {
		t.Errorf("userFrom(background) = (%+v, true), want ok=false", u)
	}
}
