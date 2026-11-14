package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/auth"
)

// testClock is a settable Deps.Now for the session-lifetime tests.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

func newSessionClockFixture(t *testing.T, lifetime, idle time.Duration) (*loginFixture, *testClock) {
	t.Helper()
	clock := &testClock{t: testNow}
	repo := newStubAuth()
	h := newTestRouter(repo, func(d *Deps) {
		d.Now = clock.now
		d.Sleep = func(time.Duration) {}
		d.SessionLifetime = lifetime
		d.SessionIdleTimeout = idle
	})
	f := &loginFixture{handler: h, repo: repo, sleeps: &sleepRecorder{}, verifies: &verifyRecorder{}}
	f.user = repo.addUser("admin", testHash())
	return f, clock
}

func viaHTTPS(r *http.Request) { r.Header.Set("X-Forwarded-Proto", "https") }

// whoamiAt sends GET /session with the cookie at the clock's instant.
func (f *loginFixture) whoamiWith(cookie *http.Cookie, mutate func(*http.Request)) int {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/admin/v1/session", nil)
	req.Host = "sidecar.test"
	if mutate != nil {
		mutate(req)
	}
	if cookie != nil {
		req.AddCookie(&http.Cookie{Name: cookie.Name, Value: cookie.Value})
	}
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec.Code
}

// TestSession_IdleTimeout pins SC-25's idle timeout: a session used within
// the idle window keeps sliding forward; one left alone for longer than it
// dies, long before its absolute lifetime.
func TestSession_IdleTimeout(t *testing.T) {
	t.Parallel()

	f, clock := newSessionClockFixture(t, 24*time.Hour, 2*time.Hour)
	c := f.login(t)

	clock.set(testNow.Add(2*time.Hour - time.Minute))
	if code := f.whoamiWith(c, nil); code != http.StatusOK {
		t.Fatalf("inside the first idle window: status = %d, want 200", code)
	}
	// Past the ORIGINAL idle deadline: alive only because the last request
	// slid it.
	clock.set(testNow.Add(3*time.Hour + 50*time.Minute))
	if code := f.whoamiWith(c, nil); code != http.StatusOK {
		t.Fatalf("after activity slid the deadline: status = %d, want 200", code)
	}
	clock.set(testNow.Add(3*time.Hour + 50*time.Minute + 2*time.Hour + time.Second))
	if code := f.whoamiWith(c, nil); code != http.StatusUnauthorized {
		t.Fatalf("idle for longer than the timeout: status = %d, want 401", code)
	}
}

// TestSession_AbsoluteLifetime: activity cannot keep a session alive past
// its absolute lifetime, and the lifetime is measured from created_at at
// request time -- so lowering it also ends sessions minted under a longer
// one (the 30-day sessions that predate SC-25).
func TestSession_AbsoluteLifetime(t *testing.T) {
	t.Parallel()

	f, clock := newSessionClockFixture(t, 4*time.Hour, 2*time.Hour)
	c := f.login(t)
	for h := 1; h < 4; h++ {
		clock.set(testNow.Add(time.Duration(h) * time.Hour))
		if code := f.whoamiWith(c, nil); code != http.StatusOK {
			t.Fatalf("active at +%dh: status = %d, want 200", h, code)
		}
	}
	clock.set(testNow.Add(4 * time.Hour))
	if code := f.whoamiWith(c, nil); code != http.StatusUnauthorized {
		t.Fatalf("at the absolute lifetime: status = %d, want 401", code)
	}
	if n := f.repo.sessionCount(); n != 0 {
		t.Errorf("sessions after absolute expiry = %d, want 0 (the row must be deleted)", n)
	}

	// A pre-SC-25 row: minted 25 hours ago with a 30-day expiry.
	f2, _ := newSessionClockFixture(t, auth.SessionLifetime, auth.SessionIdleTimeout)
	token, hash, err := auth.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	f2.repo.mu.Lock()
	f2.repo.sessions[hash] = auth.Session{TokenHash: hash, UserID: f2.user.ID,
		CreatedAt: testNow.Add(-25 * time.Hour), ExpiresAt: testNow.Add(29 * 24 * time.Hour)}
	f2.repo.mu.Unlock()
	if code := f2.whoamiWith(&http.Cookie{Name: auth.CookieName, Value: token}, nil); code != http.StatusUnauthorized {
		t.Errorf("legacy 30-day session older than the default lifetime: status = %d, want 401", code)
	}
}

// TestSession_SlidingIsThrottled: the idle timeout must not put an UPDATE in
// front of every admin request.
func TestSession_SlidingIsThrottled(t *testing.T) {
	t.Parallel()

	f, clock := newSessionClockFixture(t, 24*time.Hour, 2*time.Hour)
	c := f.login(t)
	for i := range 5 {
		clock.set(testNow.Add(time.Duration(i) * time.Second))
		if code := f.whoamiWith(c, nil); code != http.StatusOK {
			t.Fatalf("request %d: status = %d", i, code)
		}
	}
	if n := f.repo.extendCount(); n != 0 {
		t.Errorf("ExtendSession calls for five requests within seconds of login = %d, want 0", n)
	}
	clock.set(testNow.Add(time.Hour))
	f.whoamiWith(c, nil)
	if n := f.repo.extendCount(); n != 1 {
		t.Errorf("ExtendSession calls an hour later = %d, want 1", n)
	}
	sess, ok := f.repo.session(auth.HashToken(c.Value))
	if !ok || !sess.ExpiresAt.Equal(testNow.Add(3*time.Hour)) {
		t.Errorf("expires_at after sliding = %v, want %v", sess.ExpiresAt, testNow.Add(3*time.Hour))
	}
}

// findSetCookie returns the named cookie from a response, or nil.
func findSetCookie(rec *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// TestLogin_HostPrefixedCookieOverHTTPS pins SC-25's cookie hardening: over
// HTTPS the session cookie is __Host- prefixed (Secure, Path=/, no Domain --
// so a sibling subdomain cannot plant or overwrite it), and the legacy
// unprefixed cookie is cleared. Plain-HTTP development keeps the plain name,
// since browsers reject a __Host- cookie that is not Secure.
func TestLogin_HostPrefixedCookieOverHTTPS(t *testing.T) {
	t.Parallel()

	f, _ := newSessionClockFixture(t, 24*time.Hour, 2*time.Hour)
	rec := f.postLogin(credentials("admin", testPassword), viaHTTPS)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body = %s", rec.Code, rec.Body.String())
	}
	c := findSetCookie(rec, auth.SecureCookieName)
	if c == nil {
		t.Fatalf("no %s cookie; Set-Cookie = %v", auth.SecureCookieName, rec.Header().Values("Set-Cookie"))
	}
	if !strings.HasPrefix(auth.SecureCookieName, "__Host-") {
		t.Errorf("SecureCookieName = %q, want the __Host- prefix", auth.SecureCookieName)
	}
	if !c.Secure || c.Path != "/" || c.Domain != "" || !c.HttpOnly || c.Value == "" {
		t.Errorf("__Host- cookie = %+v, want Secure, HttpOnly, Path=/, no Domain", c)
	}
	if c.MaxAge != int((24 * time.Hour).Seconds()) {
		t.Errorf("Max-Age = %d, want the absolute lifetime (%d)", c.MaxAge, int((24 * time.Hour).Seconds()))
	}
	if legacy := findSetCookie(rec, auth.CookieName); legacy == nil || legacy.MaxAge >= 0 {
		t.Errorf("legacy %s cookie not cleared over HTTPS: %+v", auth.CookieName, legacy)
	}

	plain := f.postLogin(credentials("admin", testPassword), nil)
	if findSetCookie(plain, auth.SecureCookieName) != nil {
		t.Error("a __Host- cookie was set over plain HTTP; browsers reject it and the operator could never sign in")
	}
	if pc := findSetCookie(plain, auth.CookieName); pc == nil || pc.Value == "" || pc.Secure {
		t.Errorf("plain-HTTP login cookie = %+v, want %s, not Secure", pc, auth.CookieName)
	}
}

// TestSession_CookieNameFollowsScheme: over HTTPS only the __Host- cookie
// authenticates -- accepting the unprefixed name there would hand back the
// cookie-tossing hole the prefix closes, so pre-SC-25 HTTPS sessions sign in
// again once (documented in the README) -- and over HTTP only the plain one.
func TestSession_CookieNameFollowsScheme(t *testing.T) {
	t.Parallel()

	f, _ := newSessionClockFixture(t, 24*time.Hour, 2*time.Hour)
	rec := f.postLogin(credentials("admin", testPassword), viaHTTPS)
	secure := findSetCookie(rec, auth.SecureCookieName)
	if secure == nil {
		t.Fatal("no secure cookie")
	}
	if code := f.whoamiWith(secure, viaHTTPS); code != http.StatusOK {
		t.Errorf("__Host- cookie over HTTPS: status = %d, want 200", code)
	}
	legacy := &http.Cookie{Name: auth.CookieName, Value: secure.Value}
	if code := f.whoamiWith(legacy, viaHTTPS); code != http.StatusUnauthorized {
		t.Errorf("unprefixed cookie over HTTPS: status = %d, want 401", code)
	}
	if code := f.whoamiWith(secure, nil); code != http.StatusUnauthorized {
		t.Errorf("__Host- cookie over plain HTTP: status = %d, want 401", code)
	}

	// Logout over HTTPS revokes the prefixed session and clears both names.
	req := httptest.NewRequestWithContext(context.Background(), http.MethodDelete, "/api/admin/v1/session", nil)
	req.Host = "sidecar.test"
	viaHTTPS(req)
	req.AddCookie(&http.Cookie{Name: secure.Name, Value: secure.Value})
	out := httptest.NewRecorder()
	f.handler.ServeHTTP(out, req)
	if out.Code != http.StatusNoContent {
		t.Fatalf("logout status = %d", out.Code)
	}
	if _, ok := f.repo.session(auth.HashToken(secure.Value)); ok {
		t.Error("logout over HTTPS left the __Host- session alive")
	}
	for _, name := range []string{auth.SecureCookieName, auth.CookieName} {
		if c := findSetCookie(out, name); c == nil || c.MaxAge >= 0 {
			t.Errorf("logout did not clear %s: %+v", name, c)
		}
	}
}
