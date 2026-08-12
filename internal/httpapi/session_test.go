package httpapi

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/auth"
)

// testPassword is the plaintext behind testHash; it is long enough to satisfy
// auth.MinPasswordLen so the fixture matches what a real account looks like.
const testPassword = "correct-horse-battery-staple"

// testHash is the argon2id hash of testPassword, derived once for the whole
// package: at m=19456,t=2 each derivation costs real milliseconds and the
// login tests need the hash many times over.
var testHash = sync.OnceValue(func() string {
	h, err := auth.HashPassword(testPassword)
	if err != nil {
		panic("hash test password: " + err.Error())
	}
	return h
})

// testFailDelay is the configured login failure delay in these tests. No test
// actually sleeps it -- Deps.Sleep is a recorder -- so the value only needs to
// be distinctive enough that an assertion on it is meaningful.
const testFailDelay = 250 * time.Millisecond

// sleepRecorder stands in for time.Sleep so tests can assert the login
// failure brake is applied without measuring elapsed wall-clock time (which
// would need time.Now, banned outside cmd/) and without spending the delay.
type sleepRecorder struct {
	mu    sync.Mutex
	calls []time.Duration
}

func (s *sleepRecorder) sleep(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, d)
}

func (s *sleepRecorder) durations() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Duration(nil), s.calls...)
}

// loginFixture is a router wired to a stub store holding one admin account,
// plus the sleep recorder the failure-delay assertions read.
type loginFixture struct {
	handler http.Handler
	repo    *stubAuth
	sleeps  *sleepRecorder
	user    auth.User
}

func newLoginFixture(t *testing.T) *loginFixture {
	t.Helper()
	repo := newStubAuth()
	user := repo.addUser("admin", testHash())
	sleeps := &sleepRecorder{}
	h := newTestRouter(repo, func(d *Deps) {
		d.FailDelay = testFailDelay
		d.Sleep = sleeps.sleep
	})
	return &loginFixture{handler: h, repo: repo, sleeps: sleeps, user: user}
}

// postLogin sends a login request with no browser headers, which the
// cross-site guard passes through (the curl case); mutate can add them.
func (f *loginFixture) postLogin(body string, mutate func(*http.Request)) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost,
		"/api/admin/v1/session", strings.NewReader(body))
	req.Host = "sidecar.test"
	req.Header.Set("Content-Type", "application/json")
	if mutate != nil {
		mutate(req)
	}
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

func credentials(username, password string) string {
	return `{"username":` + quote(username) + `,"password":` + quote(password) + `}`
}

func quote(s string) string { return `"` + s + `"` }

// responseSessionCookie extracts the session cookie from a response, failing
// the test if it is absent.
func responseSessionCookie(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.CookieName {
			return c
		}
	}
	t.Fatalf("no %s cookie in response; Set-Cookie = %v", auth.CookieName, rec.Header().Values("Set-Cookie"))
	return nil
}

// TestLogin_Success pins the whole success contract: the JSON body, every
// cookie attribute the spec §4.2 names, and the storage invariant that only
// the hash of the token is persisted.
func TestLogin_Success(t *testing.T) {
	t.Parallel()

	f := newLoginFixture(t)
	rec := f.postLogin(credentials("admin", testPassword), nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if got, want := bodyText(rec), `{"username":"admin"}`; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}

	c := responseSessionCookie(t, rec)
	if c.Value == "" {
		t.Fatal("session cookie has an empty value")
	}
	if !c.HttpOnly {
		t.Error("session cookie is not HttpOnly")
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want Lax", c.SameSite)
	}
	if c.Path != "/" {
		t.Errorf("Path = %q, want /", c.Path)
	}
	if want := int(auth.SessionLifetime / time.Second); c.MaxAge != want {
		t.Errorf("Max-Age = %d, want %d", c.MaxAge, want)
	}
	if c.MaxAge != 2592000 {
		t.Errorf("Max-Age = %d, want 2592000 (30 days)", c.MaxAge)
	}
	if c.Secure {
		t.Error("Secure set on a plain-HTTP request; the cookie would never be sent back")
	}

	// Only the hash is stored: a database copy must not be replayable.
	hashes := f.repo.sessionHashes()
	if len(hashes) != 1 {
		t.Fatalf("stored sessions = %d, want 1", len(hashes))
	}
	if hashes[0] == c.Value {
		t.Error("raw token was stored; only its SHA-256 may be persisted")
	}
	if want := auth.HashToken(c.Value); hashes[0] != want {
		t.Errorf("stored hash = %q, want SHA-256 of the cookie token %q", hashes[0], want)
	}

	if got := f.sleeps.durations(); len(got) != 0 {
		t.Errorf("Sleep called %v on the success path, want no calls", got)
	}
}

// TestLogin_SecureFlag covers spec §4.2's rule for the Secure attribute:
// present when the request really is HTTPS (directly or via a terminating
// proxy), absent otherwise so plain-HTTP development still works.
func TestLogin_SecureFlag(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		mutate     func(*http.Request)
		wantSecure bool
	}{
		{name: "plain http", mutate: nil, wantSecure: false},
		{
			name:       "direct TLS",
			mutate:     func(r *http.Request) { r.TLS = &tls.ConnectionState{} },
			wantSecure: true,
		},
		{
			name:       "proxy terminated TLS",
			mutate:     func(r *http.Request) { r.Header.Set("X-Forwarded-Proto", "https") },
			wantSecure: true,
		},
		{
			name:       "proxy reports http",
			mutate:     func(r *http.Request) { r.Header.Set("X-Forwarded-Proto", "http") },
			wantSecure: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newLoginFixture(t)
			rec := f.postLogin(credentials("admin", testPassword), tt.mutate)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
			}
			c := responseSessionCookie(t, rec)
			if c.Secure != tt.wantSecure {
				t.Errorf("Secure = %v, want %v (Set-Cookie: %q)",
					c.Secure, tt.wantSecure, rec.Header().Get("Set-Cookie"))
			}
		})
	}
}

// TestLogin_FailuresAreIndistinguishable is the core anti-enumeration test:
// an unknown username, a wrong password, and empty credentials must produce
// byte-identical responses, so an attacker cannot use the login endpoint to
// discover which accounts exist. Each failure must also pay the configured
// delay.
func TestLogin_FailuresAreIndistinguishable(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		body string
	}{
		{"unknown user", credentials("nosuchuser", testPassword)},
		{"wrong password", credentials("admin", "wrong-password-entirely")},
		{"empty username", credentials("", testPassword)},
		{"empty password", credentials("admin", "")},
		{"missing fields", `{}`},
	}

	type response struct {
		status      int
		body        []byte
		contentType string
	}
	got := make([]response, len(cases))

	for i, tc := range cases {
		f := newLoginFixture(t)
		rec := f.postLogin(tc.body, nil)

		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s: status = %d, want 401; body = %s", tc.name, rec.Code, rec.Body.String())
		}
		if want := `{"error":"invalid credentials"}`; bodyText(rec) != want {
			t.Errorf("%s: body = %q, want %q", tc.name, bodyText(rec), want)
		}
		if cookies := rec.Result().Cookies(); len(cookies) != 0 {
			t.Errorf("%s: failed login set cookies %v", tc.name, cookies)
		}
		if n := f.repo.sessionCount(); n != 0 {
			t.Errorf("%s: failed login created %d sessions", tc.name, n)
		}
		if want := []time.Duration{testFailDelay}; !equalDurations(f.sleeps.durations(), want) {
			t.Errorf("%s: Sleep calls = %v, want %v", tc.name, f.sleeps.durations(), want)
		}
		got[i] = response{status: rec.Code, body: rec.Body.Bytes(), contentType: rec.Header().Get("Content-Type")}
	}

	for i := 1; i < len(got); i++ {
		if got[i].status != got[0].status {
			t.Errorf("%s status = %d but %s status = %d: failure kinds must not be distinguishable",
				cases[i].name, got[i].status, cases[0].name, got[0].status)
		}
		if !bytes.Equal(got[i].body, got[0].body) {
			t.Errorf("%s body = %q but %s body = %q: failure kinds must not be distinguishable",
				cases[i].name, got[i].body, cases[0].name, got[0].body)
		}
		if got[i].contentType != got[0].contentType {
			t.Errorf("%s Content-Type = %q but %s Content-Type = %q",
				cases[i].name, got[i].contentType, cases[0].name, got[0].contentType)
		}
	}
}

func equalDurations(a, b []time.Duration) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestLogin_UnknownUserStillHashes checks the timing-equalisation half of
// spec §4.3: an unknown username must still burn an argon2 verification
// against the dummy hash. The observable proxy is that auth.DummyPHC is a
// valid hash that verifies false rather than erroring -- if the handler
// skipped the work, this response would be a 500 or arrive without the delay.
func TestLogin_UnknownUserStillHashes(t *testing.T) {
	t.Parallel()

	ok, err := auth.VerifyPassword(auth.DummyPHC, testPassword)
	if err != nil {
		t.Fatalf("auth.DummyPHC is not a usable dummy hash: %v", err)
	}
	if ok {
		t.Fatal("auth.DummyPHC matched a password")
	}
}

// TestLogin_BadRequests covers the two malformed-input shapes that must be
// 400s rather than 401s or 500s, including the 8 KB body cap.
func TestLogin_BadRequests(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
	}{
		{"malformed JSON", `{"username": "admin", `},
		{"not an object", `["admin","hunter2"]`},
		{"empty body", ``},
		{"over the 8 KB cap", `{"username":"admin","password":"` + strings.Repeat("x", 9000) + `"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newLoginFixture(t)
			rec := f.postLogin(tt.body, nil)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}
			if !strings.Contains(rec.Body.String(), `"error"`) {
				t.Errorf("body = %q, want a JSON error object", rec.Body.String())
			}
			if n := f.repo.sessionCount(); n != 0 {
				t.Errorf("malformed login created %d sessions", n)
			}
		})
	}
}

// TestLogin_UnknownFieldsIgnored: the SPA may send extra fields (or a future
// version may), and decodeJSON deliberately does not set
// DisallowUnknownFields.
func TestLogin_UnknownFieldsIgnored(t *testing.T) {
	t.Parallel()

	f := newLoginFixture(t)
	rec := f.postLogin(`{"username":"admin","password":"`+testPassword+`","remember":true}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
}

// TestLogin_UsernameIsNormalized: usernames are case-insensitive everywhere
// else (auth.NormalizeUsername on every write and lookup), so login must not
// be the one surface where "Admin" fails.
func TestLogin_UsernameIsNormalized(t *testing.T) {
	t.Parallel()

	f := newLoginFixture(t)
	rec := f.postLogin(credentials("  ADMIN  ", testPassword), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if got, want := bodyText(rec), `{"username":"admin"}`; got != want {
		t.Errorf("body = %q, want %q (the stored username, not the typed one)", got, want)
	}
}

// TestLogin_StoreErrors: a broken store is a 500 with the detail logged, not
// leaked, and never a 401 that would send an operator hunting for a password
// problem that does not exist.
func TestLogin_StoreErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*stubAuth)
	}{
		{"user lookup fails", func(s *stubAuth) { s.getUserErr = errors.New("database is locked") }},
		{"session insert fails", func(s *stubAuth) { s.createSessionErr = errors.New("disk full") }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newLoginFixture(t)
			tt.mutate(f.repo)

			rec := f.postLogin(credentials("admin", testPassword), nil)
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500; body = %s", rec.Code, rec.Body.String())
			}
			if got, want := bodyText(rec), `{"error":"internal error"}`; got != want {
				t.Errorf("body = %q, want %q (store detail must never reach the client)", got, want)
			}
			if strings.Contains(rec.Body.String(), "database is locked") || strings.Contains(rec.Body.String(), "disk full") {
				t.Errorf("store error leaked to the client: %s", rec.Body.String())
			}
			if cookies := rec.Result().Cookies(); len(cookies) != 0 {
				t.Errorf("failed login set cookies %v", cookies)
			}
		})
	}
}

// TestLogin_RunsSessionGC pins spec §4.2's lazy garbage collection: every
// successful login sweeps expired sessions, which is the only thing that ever
// deletes them in bulk (there is no background goroutine).
func TestLogin_RunsSessionGC(t *testing.T) {
	t.Parallel()

	f := newLoginFixture(t)
	f.repo.addSession("stale-hash", f.user.ID, testNow.Add(-time.Hour))

	rec := f.postLogin(credentials("admin", testPassword), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if got := f.repo.gcCount(); got != 1 {
		t.Errorf("DeleteExpiredSessions called %d times, want 1", got)
	}
	for _, h := range f.repo.sessionHashes() {
		if h == "stale-hash" {
			t.Error("expired session survived the login-time sweep")
		}
	}
}

// TestLogin_SleepDefaultsToTimeSleep: production leaves Deps.Sleep nil and
// NewRouter fills it in. Without that default the first failed login in
// production would panic on a nil call. FailDelay stays zero here so the test
// does not actually sleep.
func TestLogin_SleepDefaultsToTimeSleep(t *testing.T) {
	t.Parallel()

	repo := newStubAuth()
	repo.addUser("admin", testHash())
	h := newTestRouter(repo, nil) // Sleep nil, FailDelay 0

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost,
		"/api/admin/v1/session", strings.NewReader(credentials("admin", "wrong-password-entirely")))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body = %s", rec.Code, rec.Body.String())
	}
}

// login performs a successful login and returns the session cookie.
func (f *loginFixture) login(t *testing.T) *http.Cookie {
	t.Helper()
	rec := f.postLogin(credentials("admin", testPassword), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("login: status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	return responseSessionCookie(t, rec)
}

func (f *loginFixture) do(method, target string, cookie *http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), method, target, nil)
	req.Host = "sidecar.test"
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

// TestLogout_DeletesSessionAndClearsCookie: the session row must actually go
// away -- clearing the cookie alone would leave a live token that anyone
// holding a copy could keep using for 30 days.
func TestLogout_DeletesSessionAndClearsCookie(t *testing.T) {
	t.Parallel()

	f := newLoginFixture(t)
	c := f.login(t)
	if f.repo.sessionCount() != 1 {
		t.Fatalf("sessions after login = %d, want 1", f.repo.sessionCount())
	}

	rec := f.do(http.MethodDelete, "/api/admin/v1/session", c)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body = %s", rec.Code, rec.Body.String())
	}
	if n := f.repo.sessionCount(); n != 0 {
		t.Errorf("sessions after logout = %d, want 0", n)
	}

	cleared := responseSessionCookie(t, rec)
	if cleared.MaxAge >= 0 {
		t.Errorf("Max-Age = %d, want a negative value to clear the cookie", cleared.MaxAge)
	}
	if cleared.Value != "" {
		t.Errorf("cleared cookie value = %q, want empty", cleared.Value)
	}

	// The token must be dead for subsequent requests, not just forgotten by
	// this one browser.
	after := f.do(http.MethodGet, "/api/admin/v1/session", c)
	if after.Code != http.StatusUnauthorized {
		t.Errorf("whoami after logout: status = %d, want 401; body = %s", after.Code, after.Body.String())
	}
}

// TestLogout_Idempotent: logging out without a session is a success. The SPA
// calls it on a 401 to tidy up, and a 4xx there would be noise.
func TestLogout_Idempotent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		cookie *http.Cookie
	}{
		{"no cookie", nil},
		{"unknown token", &http.Cookie{Name: auth.CookieName, Value: "not-a-real-token"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newLoginFixture(t)
			rec := f.do(http.MethodDelete, "/api/admin/v1/session", tt.cookie)
			if rec.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want 204; body = %s", rec.Code, rec.Body.String())
			}
			if rec.Body.Len() != 0 {
				t.Errorf("204 body = %q, want empty", rec.Body.String())
			}
			// The clearing cookie goes out either way, so a browser holding a
			// stale token drops it even when the row was already gone.
			if cleared := responseSessionCookie(t, rec); cleared.MaxAge >= 0 {
				t.Errorf("Max-Age = %d, want a negative value to clear the cookie", cleared.MaxAge)
			}
		})
	}
}

// TestLogout_StoreFailureIsNotSilent: if the delete fails the session is
// still live, so reporting 204 and clearing the cookie would tell the user a
// lie they cannot act on -- the token would stay valid with no way left to
// revoke it.
func TestLogout_StoreFailureIsNotSilent(t *testing.T) {
	t.Parallel()

	f := newLoginFixture(t)
	c := f.login(t)
	f.repo.deleteSessionErr = errors.New("database is locked")

	rec := f.do(http.MethodDelete, "/api/admin/v1/session", c)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body = %s", rec.Code, rec.Body.String())
	}
	if got, want := bodyText(rec), `{"error":"internal error"}`; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
	if cookies := rec.Result().Cookies(); len(cookies) != 0 {
		t.Errorf("failed logout cleared the cookie (%v) while the session is still live", cookies)
	}
}

// TestLogout_CrossSiteRejected: logout is a state-changing admin route, so it
// sits behind the cross-site guard like every other non-GET route.
func TestLogout_CrossSiteRejected(t *testing.T) {
	t.Parallel()

	f := newLoginFixture(t)
	c := f.login(t)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodDelete, "/api/admin/v1/session", nil)
	req.Host = "sidecar.test"
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.AddCookie(c)
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body = %s", rec.Code, rec.Body.String())
	}
	if n := f.repo.sessionCount(); n != 1 {
		t.Errorf("sessions = %d, want 1: a rejected logout must not delete anything", n)
	}
}

// TestWhoami covers spec §4.5's boot check: the SPA asks who it is without a
// sacrificial data request, and gets a 401 when the session is gone.
func TestWhoami(t *testing.T) {
	t.Parallel()

	t.Run("logged in", func(t *testing.T) {
		t.Parallel()
		f := newLoginFixture(t)
		c := f.login(t)

		rec := f.do(http.MethodGet, "/api/admin/v1/session", c)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
		}
		if got, want := bodyText(rec), `{"username":"admin"}`; got != want {
			t.Errorf("body = %q, want %q", got, want)
		}
	})

	t.Run("logged out", func(t *testing.T) {
		t.Parallel()
		f := newLoginFixture(t)

		rec := f.do(http.MethodGet, "/api/admin/v1/session", nil)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401; body = %s", rec.Code, rec.Body.String())
		}
		if got := bodyText(rec); got != unauthorizedBody {
			t.Errorf("body = %q, want %q", got, unauthorizedBody)
		}
	})
}

// TestAdminRoutesUnregisteredWithoutAuth: NewRouter must keep working for
// callers that never configure an auth store (the feed-only tests, and any
// deployment that has not created a user yet). The admin routes simply do not
// exist then -- they must not appear as handlers that panic on a nil store.
func TestAdminRoutesUnregisteredWithoutAuth(t *testing.T) {
	t.Parallel()

	h := NewRouter(Deps{Now: func() time.Time { return testNow }, Logger: discardLogger()})

	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
		req := httptest.NewRequestWithContext(context.Background(), method, "/api/admin/v1/session", nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s /api/admin/v1/session: status = %d, want 404", method, rec.Code)
		}
	}
}
