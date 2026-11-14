package httpapi

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/ratelimit"
)

// TestLogin_FailedLoginLogTruncatesUsername pins SC-30 at the HTTP layer:
// the username in the failed-login Warn line is whatever the caller sent, up
// to the 8 KB body cap, so it must be cut down before it is logged.
func TestLogin_FailedLoginLogTruncatesUsername(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	sleeps := &sleepRecorder{}
	h := newTestRouter(newStubAuth(), func(d *Deps) {
		d.Logger = slog.New(slog.NewTextHandler(&buf, nil))
		d.Sleep = sleeps.sleep
	})
	f := &loginFixture{handler: h}
	long := strings.Repeat("a", 7000)
	rec := f.postLogin(credentials(long, "wrong password!!"), nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body = %s", rec.Code, rec.Body.String())
	}
	logged := buf.String()
	if !strings.Contains(logged, "failed login") {
		t.Fatalf("no failed-login line logged: %q", logged)
	}
	if strings.Contains(logged, strings.Repeat("a", 65)) {
		t.Errorf("failed-login line carries more than 64 characters of the username (%d bytes logged)", len(logged))
	}
}

// testIPHeader lets a login test choose the client address per request; the
// fixtures below install a ClientIP resolver that reads it.
const testIPHeader = "X-Test-Client-Ip"

func fromIP(ip string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set(testIPHeader, ip) }
}

// newThrottleFixture is newLoginFixture with the login throttles and the
// client-address resolver under the test's control.
func newThrottleFixture(t *testing.T, mutate func(*Deps)) *loginFixture {
	t.Helper()
	repo := newStubAuth()
	sleeps := &sleepRecorder{}
	verifies := &verifyRecorder{}
	h := newTestRouter(repo, func(d *Deps) {
		d.FailDelay = testFailDelay
		d.Sleep = sleeps.sleep
		d.VerifyPassword = verifies.verify
		d.ClientIP = func(r *http.Request) string { return r.Header.Get(testIPHeader) }
		if mutate != nil {
			mutate(d)
		}
	})
	f := &loginFixture{handler: h, repo: repo, sleeps: sleeps, verifies: verifies}
	f.user = repo.addUser("admin", testHash())
	return f
}

const wrongPassword = "definitely-not-it"

// assertThrottled checks the SC-02 contract for a refused attempt: 429, no
// password verification, no failure delay, no session.
func assertThrottled(t *testing.T, label string, f *loginFixture, rec *httptest.ResponseRecorder, verifiesBefore, sleepsBefore int) {
	t.Helper()
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("%s: status = %d, want 429; body = %s", label, rec.Code, rec.Body.String())
	}
	if got := len(f.verifies.recorded()); got != verifiesBefore {
		t.Errorf("%s: VerifyPassword ran (%d -> %d calls); a throttled attempt must be refused before hashing", label, verifiesBefore, got)
	}
	if got := len(f.sleeps.durations()); got != sleepsBefore {
		t.Errorf("%s: failure delay applied to a throttled attempt; it would pin a goroutine per flood request", label)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Errorf("%s: no Retry-After on a 429", label)
	}
	if got := bodyText(rec); !strings.Contains(got, `"error"`) {
		t.Errorf("%s: body = %q, want the {\"error\": ...} shape the SPA displays", label, got)
	}
}

// TestLogin_ThrottlesFailuresPerIP: SC-02's per-address brake. Only failures
// are charged, and the attempt after the budget is spent is refused BEFORE
// any argon2 work -- even when it carries the right password.
func TestLogin_ThrottlesFailuresPerIP(t *testing.T) {
	t.Parallel()

	f := newThrottleFixture(t, func(d *Deps) {
		d.LoginIPLimiter = ratelimit.New(3, 15*time.Minute)
		d.LoginUserLimiter = ratelimit.New(1000, 15*time.Minute)
	})
	for i := range 3 {
		if rec := f.postLogin(credentials("admin", wrongPassword), fromIP("198.51.100.1")); rec.Code != http.StatusUnauthorized {
			t.Fatalf("failure %d: status = %d, want 401", i+1, rec.Code)
		}
	}
	v, s := len(f.verifies.recorded()), len(f.sleeps.durations())
	rec := f.postLogin(credentials("admin", testPassword), fromIP("198.51.100.1"))
	assertThrottled(t, "4th attempt from the same address", f, rec, v, s)
	if f.repo.sessionCount() != 0 {
		t.Error("a throttled attempt created a session")
	}

	// Unknown usernames are throttled identically, so the 429 is no oracle.
	rec = f.postLogin(credentials("nobody-here", wrongPassword), fromIP("198.51.100.1"))
	assertThrottled(t, "unknown user from the throttled address", f, rec, v, s)

	// Another address has its own budget.
	if rec := f.postLogin(credentials("admin", testPassword), fromIP("198.51.100.2")); rec.Code != http.StatusOK {
		t.Fatalf("other address: status = %d, want 200", rec.Code)
	}
}

// TestLogin_SuccessIsNotCharged: a working operator signing in repeatedly
// spends nothing.
func TestLogin_SuccessIsNotCharged(t *testing.T) {
	t.Parallel()

	f := newThrottleFixture(t, func(d *Deps) {
		d.LoginIPLimiter = ratelimit.New(1, 15*time.Minute)
		d.LoginUserLimiter = ratelimit.New(1, 15*time.Minute)
	})
	for i := range 5 {
		if rec := f.postLogin(credentials("admin", testPassword), fromIP("198.51.100.1")); rec.Code != http.StatusOK {
			t.Fatalf("login %d: status = %d, want 200", i+1, rec.Code)
		}
	}
}

// TestLogin_ThrottlesFailuresPerUsername: the distributed-guessing brake,
// and the property that keeps it from becoming a lockout weapon. Once a
// username's failure budget is spent, an address that has itself failed is
// refused before hashing -- but an address with a clean record still gets
// its attempt verified, so a legitimate operator is never locked out by
// someone else hammering their username. Each such clean address gets one
// guess per window, after which its own failure marks it.
func TestLogin_ThrottlesFailuresPerUsername(t *testing.T) {
	t.Parallel()

	f := newThrottleFixture(t, func(d *Deps) {
		d.LoginIPLimiter = ratelimit.New(1000, 15*time.Minute)
		d.LoginUserLimiter = ratelimit.New(3, 15*time.Minute)
	})
	for _, ip := range []string{"203.0.113.1", "203.0.113.2", "203.0.113.3"} {
		if rec := f.postLogin(credentials("admin", wrongPassword), fromIP(ip)); rec.Code != http.StatusUnauthorized {
			t.Fatalf("failure from %s: status = %d, want 401", ip, rec.Code)
		}
	}

	v, s := len(f.verifies.recorded()), len(f.sleeps.durations())
	rec := f.postLogin(credentials("admin", testPassword), fromIP("203.0.113.1"))
	assertThrottled(t, "an address that already failed, username exhausted", f, rec, v, s)

	// The username key is the normalized form: case and padding do not
	// mint a fresh bucket.
	rec = f.postLogin(credentials("  ADMIN ", wrongPassword), fromIP("203.0.113.2"))
	assertThrottled(t, "same username, different spelling", f, rec, v, s)

	// The operator, from an address that has not failed, still signs in.
	if got := f.postLogin(credentials("admin", testPassword), fromIP("192.0.2.50")); got.Code != http.StatusOK {
		t.Fatalf("operator from a clean address: status = %d, want 200 (no lockout)", got.Code)
	}

	// A clean attacker address gets exactly one verified guess.
	if got := f.postLogin(credentials("admin", wrongPassword), fromIP("192.0.2.66")); got.Code != http.StatusUnauthorized {
		t.Fatalf("first guess from a clean address: status = %d, want 401", got.Code)
	}
	v, s = len(f.verifies.recorded()), len(f.sleeps.durations())
	rec = f.postLogin(credentials("admin", wrongPassword), fromIP("192.0.2.66"))
	assertThrottled(t, "second guess from that address", f, rec, v, s)

	// Another username is unaffected.
	f.repo.addUser("second", testHash())
	if rec := f.postLogin(credentials("second", testPassword), fromIP("203.0.113.1")); rec.Code != http.StatusOK {
		t.Fatalf("another username: status = %d, want 200", rec.Code)
	}
}

// TestLogin_DefaultThrottle pins NewRouter's default per-address budget, so
// a deployment that wires nothing still has the brake.
func TestLogin_DefaultThrottle(t *testing.T) {
	t.Parallel()

	f := newThrottleFixture(t, nil)
	for i := range loginFailuresPerIP {
		if rec := f.postLogin(credentials("admin", wrongPassword), fromIP("198.51.100.9")); rec.Code != http.StatusUnauthorized {
			t.Fatalf("failure %d: status = %d, want 401", i+1, rec.Code)
		}
	}
	v, s := len(f.verifies.recorded()), len(f.sleeps.durations())
	rec := f.postLogin(credentials("admin", wrongPassword), fromIP("198.51.100.9"))
	assertThrottled(t, "default budget spent", f, rec, v, s)
}

// blockingVerifier is a VerifyPassword stand-in that parks every call until
// released, recording the peak number in flight.
type blockingVerifier struct {
	mu       sync.Mutex
	inFlight int
	peak     int
	calls    int
	entered  chan struct{}
	release  chan struct{}
	once     sync.Once
}

// unblock releases every parked and future call; safe to call twice, so a
// test can both release explicitly and register it as a cleanup that keeps a
// failing assertion from hanging the suite.
func (b *blockingVerifier) unblock() { b.once.Do(func() { close(b.release) }) }

func newBlockingVerifier() *blockingVerifier {
	return &blockingVerifier{entered: make(chan struct{}, 64), release: make(chan struct{})}
}

func (b *blockingVerifier) verify(phc, password string) (bool, error) {
	b.mu.Lock()
	b.inFlight++
	b.calls++
	if b.inFlight > b.peak {
		b.peak = b.inFlight
	}
	b.mu.Unlock()
	b.entered <- struct{}{}
	<-b.release
	b.mu.Lock()
	b.inFlight--
	b.mu.Unlock()
	return false, nil
}

func (b *blockingVerifier) snapshot() (inFlight, peak, calls int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.inFlight, b.peak, b.calls
}

// TestLogin_VerificationConcurrencyBounded: SC-02's global semaphore. Each
// argon2id verification costs ~19 MiB, so N concurrent logins from N
// addresses (which no per-address limit sees) must still never run more
// than K derivations at once.
func TestLogin_VerificationConcurrencyBounded(t *testing.T) {
	t.Parallel()

	const slots, logins = 2, 6
	bv := newBlockingVerifier()
	t.Cleanup(bv.unblock)
	f := newThrottleFixture(t, func(d *Deps) {
		d.LoginVerifySlots = slots
		d.LoginVerifyWait = time.Minute
		d.VerifyPassword = bv.verify
	})

	var wg sync.WaitGroup
	codes := make(chan int, logins)
	for i := range logins {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ip := "198.51.100." + strconv.Itoa(10+i)
			codes <- f.postLogin(credentials("admin", wrongPassword), fromIP(ip)).Code
		}()
	}
	for range slots {
		<-bv.entered
	}
	// The slots are full; give the other goroutines every chance to
	// (wrongly) start a derivation before checking.
	for range 20 {
		runtime.Gosched()
		time.Sleep(time.Millisecond)
	}
	if inFlight, _, _ := bv.snapshot(); inFlight != slots {
		t.Fatalf("in flight = %d with %d slots full", inFlight, slots)
	}
	bv.unblock()
	wg.Wait()
	close(codes)
	for c := range codes {
		if c != http.StatusUnauthorized {
			t.Errorf("queued login status = %d, want 401 (queued, not dropped)", c)
		}
	}
	if _, peak, calls := bv.snapshot(); peak > slots || calls != logins {
		t.Errorf("peak concurrent verifications = %d (calls %d), want <= %d (calls %d)", peak, calls, slots, logins)
	}
}

// TestLogin_VerificationQueueTimeout: a login that cannot get a verification
// slot in time is a 503, not an unbounded wait, and never hashes.
func TestLogin_VerificationQueueTimeout(t *testing.T) {
	t.Parallel()

	bv := newBlockingVerifier()
	t.Cleanup(bv.unblock)
	f := newThrottleFixture(t, func(d *Deps) {
		d.LoginVerifySlots = 1
		d.LoginVerifyWait = 20 * time.Millisecond
		d.VerifyPassword = bv.verify
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.postLogin(credentials("admin", wrongPassword), fromIP("198.51.100.1"))
	}()
	<-bv.entered

	// Without a semaphore this second login would park in the verifier;
	// release it after a beat so the failure is an assertion, not a hang.
	go func() {
		time.Sleep(2 * time.Second)
		bv.unblock()
	}()
	rec := f.postLogin(credentials("admin", testPassword), fromIP("198.51.100.2"))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body = %s", rec.Code, rec.Body.String())
	}
	if _, _, calls := bv.snapshot(); calls != 1 {
		t.Errorf("verifications = %d, want 1 (the queued login must not hash)", calls)
	}
	bv.unblock()
	<-done
}

// TestLogin_ThrottleLeavesBearerAuthAlone: the login buckets are their own.
// An address that has exhausted its sign-in budget still authenticates by
// region key, and a bad bearer key from it is still the bearer path's 401.
func TestLogin_ThrottleLeavesBearerAuthAlone(t *testing.T) {
	t.Parallel()

	f := newAdminFixtureWithDeps(t, func(d *Deps) {
		d.LoginIPLimiter = ratelimit.New(1, 15*time.Minute)
		d.Sleep = func(time.Duration) {}
	})
	key := f.mintRegionKey(t, regionTampa)
	login := func() int {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodPost,
			"/api/admin/v1/session", strings.NewReader(credentials("admin", wrongPassword)))
		req.Host = "sidecar.test"
		rec := httptest.NewRecorder()
		f.handler.ServeHTTP(rec, req)
		return rec.Code
	}
	login()
	if code := login(); code != http.StatusTooManyRequests {
		t.Fatalf("second failed login = %d, want 429", code)
	}
	if rec := sendBearer(f.handler, http.MethodGet, "/api/admin/v1/regions/0/alerts", "", "Bearer "+key); rec.Code != http.StatusOK {
		t.Errorf("valid region key after login throttle: status = %d, want 200", rec.Code)
	}
	if rec := sendBearer(f.handler, http.MethodGet, "/api/admin/v1/regions/0/alerts", "", "Bearer obask_0_nope"); rec.Code != http.StatusUnauthorized {
		t.Errorf("bad region key after login throttle: status = %d, want 401", rec.Code)
	}
}
