package httpapi

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/apikey"
	"github.com/OneBusAway/sidecar/internal/auth"
	"github.com/OneBusAway/sidecar/internal/ratelimit"
)

// mintRegionKey creates a live region key in the fixture's store and returns
// its raw form. The raw key exists only here and in the Authorization header
// the test sends -- exactly as in production.
func (f *adminFixture) mintRegionKey(t *testing.T, regionID int64) string {
	t.Helper()
	raw, hash, err := apikey.NewRegionKey(regionID)
	if err != nil {
		t.Fatalf("NewRegionKey: %v", err)
	}
	_, err = f.store.APIKeys().CreateRegionKey(context.Background(), regionID, "test",
		hash, nil, apikey.Actor{Kind: apikey.ActorCLI}, testNow)
	if err != nil {
		t.Fatalf("CreateRegionKey: %v", err)
	}
	return raw
}

// mintPrincipal creates a live service principal and returns its raw key.
func (f *adminFixture) mintPrincipal(t *testing.T) string {
	t.Helper()
	raw, hash, err := apikey.NewPrincipalKey()
	if err != nil {
		t.Fatalf("NewPrincipalKey: %v", err)
	}
	if _, err := f.store.APIKeys().CreatePrincipal(context.Background(), "test", hash, testNow); err != nil {
		t.Fatalf("CreatePrincipal: %v", err)
	}
	return raw
}

// sendBearer issues a request authenticated by an Authorization header. It
// deliberately sends NO Sec-Fetch-Site and NO Origin, which is what a
// server-side HTTP client looks like and what crossSiteGuard already passes.
func sendBearer(h http.Handler, method, target, body, authorization string) *httptest.ResponseRecorder {
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequestWithContext(context.Background(), method, target, r)
	req.Host = "sidecar.test"
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestBearer_ValidRegionKeyAuthenticates is the happy path: no cookie, no
// browser headers, just the header Rails will send.
func TestBearer_ValidRegionKeyAuthenticates(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	raw := f.mintRegionKey(t, regionPuget)

	rec := sendBearer(f.handler, http.MethodGet, "/api/admin/v1/regions/1/alerts", "", "Bearer "+raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
}

// TestBearer_ValidPrincipalAuthenticates proves the obasp_ family resolves
// too. There is no route a service principal may reach yet (key management
// arrives with the key routes), so the observable proof is that it gets the
// allow-list refusal rather than the credential refusal: a 403, not a 401.
func TestBearer_ValidPrincipalAuthenticates(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	raw := f.mintPrincipal(t)

	rec := sendBearer(f.handler, http.MethodGet, "/api/admin/v1/regions/1/alerts", "", "Bearer "+raw)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body = %s", rec.Code, rec.Body.String())
	}
	if got, want := bodyText(rec), `{"error":"forbidden"}`; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
	// The policy refusal must stay textually distinct from the guard's, so a
	// consumer can tell "your credential may not do this" from "your browser
	// origin is wrong". Compared at the constant, not at this one response:
	// the point is that the two messages can never converge.
	if policy := `{"error":"` + forbiddenBody + `"}`; policy == crossSiteBody {
		t.Errorf("the allow-list 403 body %q is the cross-site 403 body", policy)
	}
}

// TestBearer_BeatsCookie: an Authorization header means cookies are ignored
// ENTIRELY. Falling back to the cookie would let a browser session silently
// rescue a request that was meant to be, and failed as, a bearer call --
// hiding a revoked key from whoever revoked it.
func TestBearer_BeatsCookie(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/admin/v1/session", nil)
	req.Host = "sidecar.test"
	req.Header.Set("Authorization", "Bearer obask_1_not-a-real-key-not-a-real-key-not-a-real")
	req.AddCookie(f.cookie) // a perfectly good operator session
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body = %s", rec.Code, rec.Body.String())
	}
	if got, want := bodyText(rec), `{"error":"invalid api key"}`; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}

// TestBearer_MalformedHeadersAre401 walks every shape that must NOT fall
// through to the cookie path.
func TestBearer_MalformedHeadersAre401(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	live := f.mintRegionKey(t, regionPuget)

	tests := []struct {
		name   string
		header string
	}{
		{"empty value", ""},
		{"no scheme", live},
		{"wrong scheme", "Token " + live},
		{"basic", "Basic " + live},
		{"two spaces", "Bearer  " + live},
		{"no space", "Bearer" + live},
		{"unparseable prefix", "Bearer sk_live_abcdef"},
		{"unknown key", "Bearer obask_1_" + strings.Repeat("A", 43)},
		{"over long", "Bearer " + strings.Repeat("A", 200)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// An empty header value must be "a bearer attempt that failed",
			// not "no header at all"; Set with "" would drop it, so the map
			// is written directly.
			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/admin/v1/regions/1/alerts", nil)
			req.Host = "sidecar.test"
			req.Header["Authorization"] = []string{tc.header}
			rec := httptest.NewRecorder()
			f.handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401; body = %s", rec.Code, rec.Body.String())
			}
			if got, want := bodyText(rec), `{"error":"invalid api key"}`; got != want {
				t.Errorf("body = %q, want %q", got, want)
			}
		})
	}
}

// TestBearer_DuplicateHeaderIs401: two Authorization headers is ambiguous,
// and picking one is how a proxy-injected header gets silently preferred
// over the client's.
func TestBearer_DuplicateHeaderIs401(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	live := f.mintRegionKey(t, regionPuget)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/admin/v1/regions/1/alerts", nil)
	req.Host = "sidecar.test"
	req.Header["Authorization"] = []string{"Bearer " + live, "Bearer " + live}
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body = %s", rec.Code, rec.Body.String())
	}
	// The body, not just the status: an httptest recorder keeps the FIRST
	// WriteHeader, so a middleware that rejected and then fell through to
	// another authenticator would still read as 401 here.
	if got, want := bodyText(rec), `{"error":"invalid api key"}`; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}

// TestBearer_RevokedKeyIsLoggedDistinctly. A revoked key being replayed is
// the clearest signal a credential leaked, so it must be greppable --
// reason=revoked -- and it must never carry any part of the secret.
func TestBearer_RevokedKeyIsLoggedDistinctly(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	f := newAdminFixtureWithDeps(t, func(d *Deps) {
		d.Logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	})
	raw := f.mintRegionKey(t, regionPuget)
	keys, err := f.store.APIKeys().ListRegionKeys(context.Background(), regionPuget)
	if err != nil {
		t.Fatalf("ListRegionKeys: %v", err)
	}
	if err := f.store.APIKeys().RevokeRegionKey(context.Background(), regionPuget, keys[0].ID,
		apikey.Actor{Kind: apikey.ActorCLI}, testNow); err != nil {
		t.Fatalf("RevokeRegionKey: %v", err)
	}

	rec := sendBearer(f.handler, http.MethodGet, "/api/admin/v1/regions/1/alerts", "", "Bearer "+raw)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if !strings.Contains(buf.String(), "reason=revoked") {
		t.Errorf("log missing reason=revoked:\n%s", buf.String())
	}
	secret := strings.TrimPrefix(raw, "obask_1_")
	if strings.Contains(buf.String(), secret) {
		t.Errorf("log leaked the key's random segment:\n%s", buf.String())
	}
}

// TestBearer_NoFailDelay. A 256-bit random key is not guessable, so a delay
// would defend nothing while pinning a goroutine per garbage request. Deps.
// Sleep is the recorder that proves the login delay is NOT applied here.
func TestBearer_NoFailDelay(t *testing.T) {
	t.Parallel()

	var slept int
	f := newAdminFixtureWithDeps(t, func(d *Deps) {
		d.FailDelay = time.Hour
		d.Sleep = func(time.Duration) { slept++ }
	})
	sendBearer(f.handler, http.MethodGet, "/api/admin/v1/regions/1/alerts", "", "Bearer obask_1_nope")
	if slept != 0 {
		t.Errorf("Sleep called %d times on a bearer failure, want 0", slept)
	}
}

// TestBearer_ThrottleChargesFailuresOnly. Rails's successful calls are the
// hot path and must be unmetered; garbage from anywhere else is not.
func TestBearer_ThrottleChargesFailuresOnly(t *testing.T) {
	t.Parallel()

	f := newAdminFixtureWithDeps(t, func(d *Deps) {
		d.BearerFailLimiter = ratelimit.New(2, time.Minute)
	})
	raw := f.mintRegionKey(t, regionPuget)

	// Ten successes do not consume the bucket.
	for i := 0; i < 10; i++ {
		if rec := sendBearer(f.handler, http.MethodGet, "/api/admin/v1/regions/1/alerts", "", "Bearer "+raw); rec.Code != http.StatusOK {
			t.Fatalf("success %d: status = %d, want 200", i, rec.Code)
		}
	}
	// Two failures fit; the third is refused outright.
	for i := 0; i < 2; i++ {
		if rec := sendBearer(f.handler, http.MethodGet, "/api/admin/v1/regions/1/alerts", "", "Bearer obask_1_nope"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("failure %d: status = %d, want 401", i, rec.Code)
		}
	}
	if rec := sendBearer(f.handler, http.MethodGet, "/api/admin/v1/regions/1/alerts", "", "Bearer obask_1_nope"); rec.Code != http.StatusTooManyRequests {
		t.Errorf("third failure: status = %d, want 429", rec.Code)
	}
	// A valid key still works: the throttle bounds guessing, not service.
	if rec := sendBearer(f.handler, http.MethodGet, "/api/admin/v1/regions/1/alerts", "", "Bearer "+raw); rec.Code != http.StatusOK {
		t.Errorf("valid key after throttling: status = %d, want 200", rec.Code)
	}
}

// TestBearer_TouchAtMostHourly. last_used_at is what tells an operator
// whether an old key is still in use before revoking it, and it must not
// cost a write on every request.
func TestBearer_TouchAtMostHourly(t *testing.T) {
	t.Parallel()

	now := testNow
	f := newAdminFixtureWithDeps(t, func(d *Deps) {
		d.Now = func() time.Time { return now }
	})
	raw := f.mintRegionKey(t, regionPuget)
	ctx := context.Background()

	read := func() *time.Time {
		list, err := f.store.APIKeys().ListRegionKeys(ctx, regionPuget)
		if err != nil {
			t.Fatalf("ListRegionKeys: %v", err)
		}
		return list[0].LastUsedAt
	}

	sendBearer(f.handler, http.MethodGet, "/api/admin/v1/regions/1/alerts", "", "Bearer "+raw)
	first := read()
	if first == nil || !first.Equal(testNow) {
		t.Fatalf("first use: LastUsedAt = %v, want %v", first, testNow)
	}

	now = testNow.Add(59 * time.Minute)
	sendBearer(f.handler, http.MethodGet, "/api/admin/v1/regions/1/alerts", "", "Bearer "+raw)
	if got := read(); got == nil || !got.Equal(testNow) {
		t.Errorf("after 59m: LastUsedAt = %v, want it unchanged at %v", got, testNow)
	}

	now = testNow.Add(time.Hour)
	sendBearer(f.handler, http.MethodGet, "/api/admin/v1/regions/1/alerts", "", "Bearer "+raw)
	if got := read(); got == nil || !got.Equal(now) {
		t.Errorf("after 60m: LastUsedAt = %v, want %v", got, now)
	}
}

// TestBearer_PrefixRowMismatchIs401. The region id in the plaintext is a
// debugging aid; the hash decides. If the two ever disagree the key is not
// trustworthy for either region.
func TestBearer_PrefixRowMismatchIs401(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	// Mint the plaintext for region 1 but store the row against region 0.
	raw, hash, err := apikey.NewRegionKey(regionPuget)
	if err != nil {
		t.Fatalf("NewRegionKey: %v", err)
	}
	if _, err := f.store.APIKeys().CreateRegionKey(context.Background(), regionTampa, "mismatched",
		hash, nil, apikey.Actor{Kind: apikey.ActorCLI}, testNow); err != nil {
		t.Fatalf("CreateRegionKey: %v", err)
	}

	rec := sendBearer(f.handler, http.MethodGet, "/api/admin/v1/regions/1/alerts", "", "Bearer "+raw)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body = %s", rec.Code, rec.Body.String())
	}
	if got, want := bodyText(rec), `{"error":"invalid api key"}`; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}

// TestBearer_NilAPIKeysRejects: bearer auth is not configured, so an
// Authorization header is a 401 rather than a fall-through to the cookie.
func TestBearer_NilAPIKeysRejects(t *testing.T) {
	t.Parallel()

	f := newAdminFixtureWithDeps(t, func(d *Deps) { d.APIKeys = nil })
	rec := sendBearer(f.handler, http.MethodGet, "/api/admin/v1/regions/1/alerts", "", "Bearer obask_1_whatever")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if got, want := bodyText(rec), `{"error":"invalid api key"}`; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}

// TestBearer_NilAPIKeysWithCookieStillRejects is the sharper half of the
// previous test: even a perfectly good operator cookie must not rescue a
// request that announced itself as a bearer call. Without this, "bearer auth
// is not configured" would quietly mean "cookies again".
func TestBearer_NilAPIKeysWithCookieStillRejects(t *testing.T) {
	t.Parallel()

	f := newAdminFixtureWithDeps(t, func(d *Deps) { d.APIKeys = nil })
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/admin/v1/regions/1/alerts", nil)
	req.Host = "sidecar.test"
	req.Header.Set("Authorization", "Bearer obask_1_whatever")
	req.AddCookie(f.cookie)
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body = %s", rec.Code, rec.Body.String())
	}
	// The body is the load-bearing half: a recorder keeps the first
	// WriteHeader, so a cookie fall-through would leave the status at 401
	// and only show up as a second response body.
	if got, want := bodyText(rec), `{"error":"invalid api key"}`; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}

// TestBearer_CrossSiteGuardStillApplies. A bearer request that carries a
// browser's foreign Origin is rejected BEFORE authentication -- the guard is
// outermost, and no bearer bypass was added to it.
func TestBearer_CrossSiteGuardStillApplies(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	raw := f.mintRegionKey(t, regionPuget)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost,
		"/api/admin/v1/regions/1/alerts", strings.NewReader(minimalAlertBody("x")))
	req.Host = "sidecar.test"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("Authorization", "Bearer "+raw)
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if got := bodyText(rec); got != crossSiteBody {
		t.Errorf("body = %q, want %q", got, crossSiteBody)
	}
}

// TestPrincipalLogValueOmitsPasswordHash. principal embeds auth.User, which
// carries the argon2 PHC string. A future slog.Any("principal", p) must not
// print it, and only a LogValue that omits it makes that structural.
func TestPrincipalLogValueOmitsPasswordHash(t *testing.T) {
	t.Parallel()

	const phc = "$argon2id$v=19$m=65536,t=3,p=4$c2VjcmV0$SECRETHASHVALUE"
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	logger.Info("who",
		"principal", principal{
			kind: principalOperator,
			user: auth.User{ID: 1, Username: "admin", PasswordHash: phc},
		})
	logger.Info("who",
		"principal", principal{kind: principalRegionKey, regionID: 1, keyID: 8})

	if strings.Contains(buf.String(), phc) || strings.Contains(buf.String(), "SECRETHASHVALUE") {
		t.Errorf("log leaked the password hash:\n%s", buf.String())
	}
	for _, want := range []string{"username=admin", "region_id=1", "key_id=8"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("log missing %q:\n%s", want, buf.String())
		}
	}
}

// TestPrincipalCanAccessRegion pins the tenancy fence itself, independently
// of any route: an operator reaches every region, a region key reaches
// exactly its own, and a service principal reaches none -- its whole reach
// is the key-management family, which is fenced separately.
func TestPrincipalCanAccessRegion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		p    principal
		id   int64
		want bool
	}{
		{"operator, any region", principal{kind: principalOperator}, regionPuget, true},
		{"operator, region zero", principal{kind: principalOperator}, regionTampa, true},
		{"region key, own region", principal{kind: principalRegionKey, regionID: regionPuget}, regionPuget, true},
		{"region key, other region", principal{kind: principalRegionKey, regionID: regionPuget}, regionTampa, false},
		// Region 0 is a real region, so a zero-value principal must not be
		// able to reach it by accident.
		{"zero principal, region zero", principal{}, regionTampa, false},
		{"service principal", principal{kind: principalService, keyID: 3}, regionPuget, false},
	}
	for _, tc := range tests {
		if got := tc.p.canAccessRegion(tc.id); got != tc.want {
			t.Errorf("%s: canAccessRegion(%d) = %v, want %v", tc.name, tc.id, got, tc.want)
		}
	}
}

// TestPrincipalActor pins what lands in a key's created_by/revoked_by
// columns. The strings are the ones the CHECK constraints enforce, so a
// wrong kind here is a constraint failure at write time, not a wrong audit
// row.
func TestPrincipalActor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		p    principal
		want apikey.Actor
	}{
		{"operator", principal{kind: principalOperator, user: auth.User{ID: 7}},
			apikey.Actor{Kind: apikey.ActorOperator, ID: 7}},
		{"service principal", principal{kind: principalService, keyID: 4},
			apikey.Actor{Kind: apikey.ActorPrincipal, ID: 4}},
		// Unreachable through the router; it must still be a kind the CHECK
		// constraints accept rather than an empty string.
		{"region key", principal{kind: principalRegionKey, regionID: 1, keyID: 9},
			apikey.Actor{Kind: apikey.ActorCLI}},
	}
	for _, tc := range tests {
		if got := tc.p.actor(); got != tc.want {
			t.Errorf("%s: actor() = %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

// TestPrincipalSets pins the three allow-lists as policy rather than as
// whatever the route table happens to reference today. The load-bearing
// entries are the exclusions: a region key must never reach the
// key-management family, and a service principal may do nothing else.
func TestPrincipalSets(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		set  principalSet
		kind principalKind
		want bool
	}{
		{"operatorOnly admits an operator", operatorOnly, principalOperator, true},
		{"operatorOnly refuses a region key", operatorOnly, principalRegionKey, false},
		{"operatorOnly refuses a service principal", operatorOnly, principalService, false},
		{"operatorOrKey admits an operator", operatorOrKey, principalOperator, true},
		{"operatorOrKey admits a region key", operatorOrKey, principalRegionKey, true},
		{"operatorOrKey refuses a service principal", operatorOrKey, principalService, false},
		{"operatorOrService admits an operator", operatorOrService, principalOperator, true},
		{"operatorOrService admits a service principal", operatorOrService, principalService, true},
		{"operatorOrService refuses a region key", operatorOrService, principalRegionKey, false},
		// The zero value is not a kind: a principal that never went through
		// requirePrincipal must not satisfy any allow-list.
		{"operatorOnly refuses the zero kind", operatorOnly, principalKind(0), false},
		{"operatorOrKey refuses the zero kind", operatorOrKey, principalKind(0), false},
		{"operatorOrService refuses the zero kind", operatorOrService, principalKind(0), false},
	}
	for _, tc := range tests {
		if got := tc.set.has(tc.kind); got != tc.want {
			t.Errorf("%s: has(%v) = %v, want %v", tc.name, tc.kind, got, tc.want)
		}
	}
}

// TestRejectBearer_NilLimiterFailsOpen. The throttle is an abuse brake, not
// the authentication decision: a middleware built without a limiter must
// still answer 401 rather than nil-deref. NewRouter always supplies one, so
// only a hand-built authMiddleware -- which this package's tests construct --
// can reach it.
func TestRejectBearer_NilLimiterFailsOpen(t *testing.T) {
	t.Parallel()

	mw := &authMiddleware{deps: Deps{
		Now:    func() time.Time { return testNow },
		Logger: discardLogger(),
		// APIKeys and BearerFailLimiter both nil: bearer auth is not
		// configured, so every header is a rejection, and there is no bucket
		// to charge it against.
	}}
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the handler ran on a rejected bearer request")
	})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/admin/v1/regions/1/alerts", nil)
	req.Header.Set("Authorization", "Bearer obask_1_whatever")
	rec := httptest.NewRecorder()
	mw.requirePrincipal(operatorOrKey, next).ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body = %s", rec.Code, rec.Body.String())
	}
	if got, want := bodyText(rec), `{"error":"invalid api key"}`; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}

// TestRejectBearer_ThrottledFailuresAreNotLogged. This is the repo's one
// unauthenticated code path that reaches the key tables, so a flood of
// garbage headers must not be able to write a Warn line per request: the
// bucket is consulted before the log, capping the failure lines at the same
// 60/minute/peer the responses are capped at. The throttled requests still
// say so at Debug, which production's Info handler drops.
func TestRejectBearer_ThrottledFailuresAreNotLogged(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	f := newAdminFixtureWithDeps(t, func(d *Deps) {
		d.BearerFailLimiter = ratelimit.New(1, time.Minute)
		d.Logger = slog.New(slog.NewTextHandler(&buf, nil)) // Info, as production
	})

	for range 5 {
		sendBearer(f.handler, http.MethodGet, "/api/admin/v1/regions/1/alerts", "", "Bearer obask_1_nope")
	}
	if got := strings.Count(buf.String(), "bearer authentication failed"); got != 1 {
		t.Errorf("logged %d failure lines for 5 attempts against a 1/minute bucket, want 1:\n%s",
			got, buf.String())
	}
	// The last request must still have been refused, so the cap is on the
	// logging rather than on the enforcement.
	rec := sendBearer(f.handler, http.MethodGet, "/api/admin/v1/regions/1/alerts", "", "Bearer obask_1_nope")
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", rec.Code)
	}
}
