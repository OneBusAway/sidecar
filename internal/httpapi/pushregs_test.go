package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/clientip"
	"github.com/OneBusAway/sidecar/internal/httpapi"
	"github.com/OneBusAway/sidecar/internal/pushreg"
	"github.com/OneBusAway/sidecar/internal/ratelimit"
	"github.com/OneBusAway/sidecar/internal/regions"
	"github.com/OneBusAway/sidecar/internal/store/sqlitetest"
)

const (
	formCT = "application/x-www-form-urlencoded"
	jsonCT = "application/json"
)

// newPushTestServer builds a router over a freshly migrated SQLite store with
// the push registration routes registered, and hands back the router plus
// the repositories tests seed fixtures through directly.
func newPushTestServer(t *testing.T) (http.Handler, pushreg.Repository, regions.Repository) {
	t.Helper()

	store := sqlitetest.Open(t)
	deps := httpapi.Deps{
		PushRegs:    store.PushRegs(),
		PushLimiter: ratelimit.New(30, time.Minute),
		Regions:     store.Regions(),
		Now:         func() time.Time { return base },
		Logger:      slog.New(slog.DiscardHandler),
	}
	return httpapi.NewRouter(deps), store.PushRegs(), store.Regions()
}

// pushRequest issues one request against h and returns the recorded
// response. An empty contentType leaves the header unset, which is how the
// query-only DELETE requests are exercised.
func pushRequest(t *testing.T, h http.Handler, method, target, contentType, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *strings.Reader
	if body != "" {
		r = strings.NewReader(body)
	} else {
		r = strings.NewReader("")
	}
	req := httptest.NewRequestWithContext(context.Background(), method, target, r)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// errBody is the §2.5 {"error", "messages"} 422 shape.
type errBody struct {
	Error    string   `json:"error"`
	Messages []string `json:"messages"`
}

func decodeErrBody(t *testing.T, rec *httptest.ResponseRecorder) errBody {
	t.Helper()
	var eb errBody
	if err := json.Unmarshal(rec.Body.Bytes(), &eb); err != nil {
		t.Fatalf("decode error body: %v; body = %s", err, rec.Body.String())
	}
	return eb
}

func getReg(t *testing.T, repo pushreg.Repository, regionID int64, token string) pushreg.Registration {
	t.Helper()
	reg, err := repo.Get(context.Background(), regionID, token)
	if err != nil {
		t.Fatalf("Get(%d, %q): %v", regionID, token, err)
	}
	return reg
}

func TestRegister_MinimalIOS(t *testing.T) {
	t.Parallel()
	h, pushRepo, regionRepo := newPushTestServer(t)
	putRegion(t, regionRepo, 1)

	rec := pushRequest(t, h, http.MethodPost, "/api/v2/regions/1/push_registrations", formCT, "token=tok1&operating_system=ios")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body = %s", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Errorf("body = %q, want empty", rec.Body.String())
	}

	reg := getReg(t, pushRepo, 1, "tok1")
	if reg.APNSSandbox {
		t.Errorf("APNSSandbox = true, want false")
	}
	if reg.Locale != "" {
		t.Errorf("Locale = %q, want empty", reg.Locale)
	}
}

func TestRegister_JSONBody(t *testing.T) {
	t.Parallel()
	h, pushRepo, regionRepo := newPushTestServer(t)
	putRegion(t, regionRepo, 1)

	body := `{"token":"tok1","operating_system":"android","locale":"es-MX","apns_sandbox":"true"}`
	rec := pushRequest(t, h, http.MethodPost, "/api/v2/regions/1/push_registrations", jsonCT, body)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body = %s", rec.Code, rec.Body.String())
	}

	reg := getReg(t, pushRepo, 1, "tok1")
	if reg.Locale != "es-MX" {
		t.Errorf("Locale = %q, want es-MX", reg.Locale)
	}
	if reg.APNSSandbox {
		t.Errorf("APNSSandbox = true, want false (android always clears it)")
	}
}

func TestRegister_SandboxIOS(t *testing.T) {
	t.Parallel()
	h, pushRepo, regionRepo := newPushTestServer(t)
	putRegion(t, regionRepo, 1)

	rec := pushRequest(t, h, http.MethodPost, "/api/v2/regions/1/push_registrations", formCT, "token=tok1&operating_system=ios&apns_sandbox=true")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body = %s", rec.Code, rec.Body.String())
	}
	if reg := getReg(t, pushRepo, 1, "tok1"); !reg.APNSSandbox {
		t.Errorf("APNSSandbox = false, want true")
	}

	rec = pushRequest(t, h, http.MethodPost, "/api/v2/regions/1/push_registrations", formCT, "token=tok1&operating_system=ios&apns_sandbox=yes")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body = %s", rec.Code, rec.Body.String())
	}
	if reg := getReg(t, pushRepo, 1, "tok1"); reg.APNSSandbox {
		t.Errorf("APNSSandbox = true, want false (unrecognized value treated as production)")
	}
}

// TestRegister_StickyFieldMerge covers the §4 sticky-field rules. They are
// one behavior with several cases -- what a second registration does to the
// row the first one left -- and were six near-identical tests whose request
// and assertion plumbing was copied per case. Every case now asserts all
// four merged fields rather than the subset its original test happened to
// check, so each row states the whole resulting row.
func TestRegister_StickyFieldMerge(t *testing.T) {
	t.Parallel()
	const path = "/api/v2/regions/1/push_registrations"
	for _, tc := range []struct {
		name            string
		first, second   string
		wantLocale      string
		wantTestDevice  bool
		wantDescription string
		wantSandbox     bool
	}{
		{
			name:   "absent fields keep the stored values",
			first:  "token=tok1&operating_system=ios&locale=fr-FR&test_device=true&description=Aaron%27s+iPhone&apns_sandbox=true",
			second: "token=tok1&operating_system=ios",
			// apns_sandbox is deliberately NOT sticky: an absent one resets.
			wantLocale: "fr-FR", wantTestDevice: true, wantDescription: "Aaron's iPhone", wantSandbox: false,
		},
		{
			name:       "blank fields count as absent, like omitted ones",
			first:      "token=tok1&operating_system=ios&locale=fr-FR&test_device=true&description=Aaron%27s+iPhone",
			second:     "token=tok1&operating_system=ios&locale=&description=&test_device=true",
			wantLocale: "fr-FR", wantTestDevice: true, wantDescription: "Aaron's iPhone", wantSandbox: false,
		},
		{
			name:       "an explicit false demotes and clears the description",
			first:      "token=tok1&operating_system=ios&test_device=true&description=Aaron%27s+iPhone",
			second:     "token=tok1&operating_system=ios&test_device=false",
			wantLocale: "", wantTestDevice: false, wantDescription: "", wantSandbox: false,
		},
		{
			name: "repeating test_device without the description keeps the stored one",
			// The invariant is checked against the merged row, so this must
			// not 422 even though the request carries no description.
			first:      "token=tok1&operating_system=ios&test_device=true&description=Aaron%27s+iPhone",
			second:     "token=tok1&operating_system=ios&test_device=true",
			wantLocale: "", wantTestDevice: true, wantDescription: "Aaron's iPhone", wantSandbox: false,
		},
		{
			name:       "a description alone updates a row that is already a test device",
			first:      "token=tok1&operating_system=ios&test_device=true&description=Old+name",
			second:     "token=tok1&operating_system=ios&description=New+name",
			wantLocale: "", wantTestDevice: true, wantDescription: "New name", wantSandbox: false,
		},
		{
			name:       "a description is ignored for a non-test device",
			first:      "token=tok1&operating_system=ios",
			second:     "token=tok1&operating_system=ios&description=X",
			wantLocale: "", wantTestDevice: false, wantDescription: "", wantSandbox: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h, pushRepo, regionRepo := newPushTestServer(t)
			putRegion(t, regionRepo, 1)

			if rec := pushRequest(t, h, http.MethodPost, path, formCT, tc.first); rec.Code != http.StatusNoContent {
				t.Fatalf("first register: status = %d, want 204; body = %s", rec.Code, rec.Body.String())
			}
			if rec := pushRequest(t, h, http.MethodPost, path, formCT, tc.second); rec.Code != http.StatusNoContent {
				t.Fatalf("re-register: status = %d, want 204; body = %s", rec.Code, rec.Body.String())
			}

			reg := getReg(t, pushRepo, 1, "tok1")
			if reg.Locale != tc.wantLocale {
				t.Errorf("Locale = %q, want %q", reg.Locale, tc.wantLocale)
			}
			if reg.TestDevice != tc.wantTestDevice {
				t.Errorf("TestDevice = %v, want %v", reg.TestDevice, tc.wantTestDevice)
			}
			if reg.Description != tc.wantDescription {
				t.Errorf("Description = %q, want %q", reg.Description, tc.wantDescription)
			}
			if reg.APNSSandbox != tc.wantSandbox {
				t.Errorf("APNSSandbox = %v, want %v", reg.APNSSandbox, tc.wantSandbox)
			}
		})
	}
}

func TestRegister_TestDeviceRequiresDescription(t *testing.T) {
	t.Parallel()
	h, _, regionRepo := newPushTestServer(t)
	putRegion(t, regionRepo, 1)

	rec := pushRequest(t, h, http.MethodPost, "/api/v2/regions/1/push_registrations", formCT, "token=tok1&operating_system=ios&test_device=true")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body = %s", rec.Code, rec.Body.String())
	}
	eb := decodeErrBody(t, rec)
	if eb.Error != "Unable to register device" {
		t.Errorf("Error = %q, want %q", eb.Error, "Unable to register device")
	}
	want := []string{"Description can't be blank"}
	if len(eb.Messages) != len(want) || eb.Messages[0] != want[0] {
		t.Errorf("Messages = %v, want %v", eb.Messages, want)
	}
}

func TestRegister_Validation(t *testing.T) {
	t.Parallel()
	h, _, regionRepo := newPushTestServer(t)
	putRegion(t, regionRepo, 1)

	longToken := strings.Repeat("a", 4097)
	longDescription := strings.Repeat("d", 256)

	tests := []struct {
		name string
		body string
		want string
	}{
		{"missing token", "operating_system=ios", "Token can't be blank"},
		{"token too long", "token=" + longToken + "&operating_system=ios", "Token is too long (maximum is 4096 characters)"},
		{"missing os", "token=tok1", "Operating system can't be blank"},
		{"unknown os", "token=tok1&operating_system=windows", "Operating system is not included in the list"},
		{"description too long", "token=tok1&operating_system=ios&test_device=true&description=" + longDescription, "Description is too long (maximum is 255 characters)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := pushRequest(t, h, http.MethodPost, "/api/v2/regions/1/push_registrations", formCT, tt.body)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422; body = %s", rec.Code, rec.Body.String())
			}
			eb := decodeErrBody(t, rec)
			if eb.Error != "Unable to register device" {
				t.Errorf("Error = %q, want %q", eb.Error, "Unable to register device")
			}
			if len(eb.Messages) != 1 || eb.Messages[0] != tt.want {
				t.Errorf("Messages = %v, want [%q]", eb.Messages, tt.want)
			}
		})
	}
}

func TestRegister_UnknownRegion(t *testing.T) {
	t.Parallel()
	h, _, _ := newPushTestServer(t)

	rec := pushRequest(t, h, http.MethodPost, "/api/v2/regions/99/push_registrations", formCT, "token=tok1&operating_system=ios")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
	}
	// httpapi_test cannot see the unexported notFoundBody constant; this is
	// its exact contract (design spec §1.2, §2.5), also pinned by
	// alerts_test.go's in-package tests.
	const wantBody = `{"error":"Couldn't find Region"}`
	if got := rec.Body.String(); got != wantBody {
		t.Errorf("body = %q, want %q", got, wantBody)
	}
}

func TestUnregister(t *testing.T) {
	t.Parallel()
	h, _, regionRepo := newPushTestServer(t)
	putRegion(t, regionRepo, 1)

	for _, tok := range []string{"tok1", "tok2"} {
		body := "token=" + tok + "&operating_system=ios"
		if rec := pushRequest(t, h, http.MethodPost, "/api/v2/regions/1/push_registrations", formCT, body); rec.Code != http.StatusNoContent {
			t.Fatalf("register %s: status = %d, want 204; body = %s", tok, rec.Code, rec.Body.String())
		}
	}

	rec := pushRequest(t, h, http.MethodDelete, "/api/v2/regions/1/push_registrations?token=tok1", "", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("first delete: status = %d, want 204; body = %s", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Errorf("first delete: body = %q, want empty", rec.Body.String())
	}

	rec = pushRequest(t, h, http.MethodDelete, "/api/v2/regions/1/push_registrations?token=tok1", "", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("second delete: status = %d, want 404; body = %s", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Errorf("second delete: body = %q, want empty", rec.Body.String())
	}

	rec = pushRequest(t, h, http.MethodDelete, "/api/v2/regions/1/push_registrations", formCT, "token=tok2")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete by body: status = %d, want 204; body = %s", rec.Code, rec.Body.String())
	}
}

func TestThrottle(t *testing.T) {
	t.Parallel()
	store := sqlitetest.Open(t)
	deps := httpapi.Deps{
		PushRegs:    store.PushRegs(),
		PushLimiter: ratelimit.New(2, time.Minute),
		Regions:     store.Regions(),
		Now:         func() time.Time { return base },
		Logger:      slog.New(slog.DiscardHandler),
	}
	h := httpapi.NewRouter(deps)
	putRegion(t, store.Regions(), 1)

	// Every request gets a fresh ephemeral source port, the way real ones
	// arrive. Reusing one fixed RemoteAddr would let this test pass against
	// a limiter keyed on host:port -- which in production would hand every
	// request its own bucket and make the throttle a no-op.
	port := 5000
	nextAddr := func(ip string) string {
		port++
		return fmt.Sprintf("%s:%d", ip, port)
	}

	post := func(token string) *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodPost,
			"/api/v2/regions/1/push_registrations", strings.NewReader("token="+token+"&operating_system=ios"))
		req.Header.Set("Content-Type", formCT)
		req.RemoteAddr = nextAddr("1.2.3.4")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	if rec := post("tok1"); rec.Code != http.StatusNoContent {
		t.Fatalf("request 1: status = %d, want 204; body = %s", rec.Code, rec.Body.String())
	}
	if rec := post("tok2"); rec.Code != http.StatusNoContent {
		t.Fatalf("request 2: status = %d, want 204; body = %s", rec.Code, rec.Body.String())
	}

	rec := post("tok3")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("request 3: status = %d, want 429; body = %s", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Errorf("429 body = %q, want empty", rec.Body.String())
	}

	// The DELETE endpoint shares the POST bucket for this path/IP, so it is
	// throttled too even though it has made no requests of its own.
	delReq := httptest.NewRequestWithContext(context.Background(), http.MethodDelete,
		"/api/v2/regions/1/push_registrations?token=tok1", nil)
	delReq.RemoteAddr = nextAddr("1.2.3.4")
	delRec := httptest.NewRecorder()
	h.ServeHTTP(delRec, delReq)
	if delRec.Code != http.StatusTooManyRequests {
		t.Fatalf("delete: status = %d, want 429; body = %s", delRec.Code, delRec.Body.String())
	}

	// A different client IP has its own bucket.
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost,
		"/api/v2/regions/1/push_registrations", strings.NewReader("token=tok4&operating_system=ios"))
	req.Header.Set("Content-Type", formCT)
	req.RemoteAddr = "9.9.9.9:5001" // a port 1.2.3.4 already used: only the host differs
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusNoContent {
		t.Fatalf("other IP: status = %d, want 204; body = %s", rec2.Code, rec2.Body.String())
	}
}

// erroringPushRepo is a pushreg.Repository stub whose Upsert always fails
// with an error that contains the token it was asked to write -- the
// worst case a store driver could hand back, and the one sanitizeToken
// exists for.
type erroringPushRepo struct{ upsertErr, deleteErr error }

func (erroringPushRepo) Get(context.Context, int64, string) (pushreg.Registration, error) {
	return pushreg.Registration{}, pushreg.ErrNotFound
}
func (s erroringPushRepo) Upsert(context.Context, pushreg.Upsert, time.Time) error {
	return s.upsertErr
}
func (erroringPushRepo) Delete(context.Context, int64, string) error { return nil }
func (s erroringPushRepo) DeleteByToken(context.Context, string) (int64, error) {
	return 0, s.deleteErr
}
func (erroringPushRepo) Prune(context.Context, time.Time) (int64, error) { return 0, nil }
func (erroringPushRepo) ListAudience(context.Context, int64, bool, int64, int) ([]pushreg.Registration, error) {
	return nil, nil
}
func (erroringPushRepo) CountAudience(context.Context, int64, bool) (pushreg.AudienceCount, error) {
	return pushreg.AudienceCount{}, nil
}

func TestRegister_TokenNeverLogged(t *testing.T) {
	t.Parallel()
	const token = "supersecrettoken-abc123"

	store := sqlitetest.Open(t)
	putRegion(t, store.Regions(), 1)

	var buf bytes.Buffer
	deps := httpapi.Deps{
		PushRegs:    erroringPushRepo{upsertErr: fmt.Errorf("constraint failed for token %s", token)},
		PushLimiter: ratelimit.New(30, time.Minute),
		Regions:     store.Regions(),
		Now:         func() time.Time { return base },
		Logger:      slog.New(slog.NewTextHandler(&buf, nil)),
	}
	h := httpapi.NewRouter(deps)

	rec := pushRequest(t, h, http.MethodPost, "/api/v2/regions/1/push_registrations", formCT, "token="+token+"&operating_system=ios")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body = %s", rec.Code, rec.Body.String())
	}

	logOutput := buf.String()
	if strings.Contains(logOutput, token) {
		t.Errorf("log output contains the raw token: %s", logOutput)
	}
	if !strings.Contains(logOutput, "[token]") {
		t.Errorf("log output missing sanitized [token] marker: %s", logOutput)
	}
}

// countingPushRegs counts Get calls so a test can assert that a request
// never reached the store. It delegates everything else.
type countingPushRegs struct {
	pushreg.Repository
	gets int
}

func (c *countingPushRegs) Get(ctx context.Context, regionID int64, token string) (pushreg.Registration, error) {
	c.gets++
	return c.Repository.Get(ctx, regionID, token)
}

// TestRegister_InvalidRequestCostsNoStoreRead pins the short-circuit in
// register: this endpoint is unauthenticated (§2.6), so a request that
// cannot possibly succeed must be rejected before it costs a database read,
// or anyone can turn cheap garbage into store load. Nothing observable on
// the wire changes if the early return is deleted -- the same 422 comes back
// either way -- so only counting the reads can hold this, the same shape as
// ratelimit's sweep-gate test.
func TestRegister_InvalidRequestCostsNoStoreRead(t *testing.T) {
	t.Parallel()

	store := sqlitetest.Open(t)
	counting := &countingPushRegs{Repository: store.PushRegs()}
	h := httpapi.NewRouter(httpapi.Deps{
		PushRegs:    counting,
		PushLimiter: ratelimit.New(30, time.Minute),
		Regions:     store.Regions(),
		Now:         func() time.Time { return base },
		Logger:      slog.New(slog.DiscardHandler),
	})
	putRegion(t, store.Regions(), 1)

	for _, body := range []string{
		"operating_system=ios",                   // blank token
		"token=tok1",                             // blank operating system
		"token=tok1&operating_system=blackberry", // unknown operating system
	} {
		rec := pushRequest(t, h, http.MethodPost, "/api/v2/regions/1/push_registrations", formCT, body)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("body %q: status = %d, want 422", body, rec.Code)
		}
	}
	if counting.gets != 0 {
		t.Errorf("store Get called %d times for requests that can only 422; want 0", counting.gets)
	}

	// A request that can still succeed does read, so the assertion above is
	// about the short-circuit and not about Get being unreachable.
	if rec := pushRequest(t, h, http.MethodPost, "/api/v2/regions/1/push_registrations", formCT,
		"token=tok1&operating_system=ios"); rec.Code != http.StatusNoContent {
		t.Fatalf("valid register: status = %d, want 204; body = %s", rec.Code, rec.Body.String())
	}
	if counting.gets != 1 {
		t.Errorf("store Get called %d times for a valid request; want 1", counting.gets)
	}
}

// TestThrottle_TrustedProxyHeader pins that Deps.ClientIP, not RemoteAddr, is
// the bucket key: two clients arriving through one proxy address are
// throttled separately, and a client with no header falls back to the peer.
func TestThrottle_TrustedProxyHeader(t *testing.T) {
	t.Parallel()
	store := sqlitetest.Open(t)
	deps := httpapi.Deps{
		PushRegs:    store.PushRegs(),
		PushLimiter: ratelimit.New(1, time.Minute),
		Regions:     store.Regions(),
		Now:         func() time.Time { return base },
		Logger:      slog.New(slog.DiscardHandler),
		ClientIP:    clientip.Header("CF-Connecting-IP"),
	}
	h := httpapi.NewRouter(deps)
	putRegion(t, store.Regions(), 1)

	post := func(token, clientHeader string) int {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodPost,
			"/api/v2/regions/1/push_registrations", strings.NewReader("token="+token+"&operating_system=ios"))
		req.Header.Set("Content-Type", formCT)
		req.RemoteAddr = "10.0.0.1:443" // the proxy, same for everyone
		if clientHeader != "" {
			req.Header.Set("CF-Connecting-IP", clientHeader)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	if got := post("a", "203.0.113.1"); got != http.StatusNoContent {
		t.Fatalf("first client: %d", got)
	}
	if got := post("b", "203.0.113.2"); got != http.StatusNoContent {
		t.Fatalf("second client behind the same proxy must have its own bucket: %d", got)
	}
	if got := post("c", "203.0.113.1"); got != http.StatusTooManyRequests {
		t.Fatalf("first client's second request: %d, want 429", got)
	}
	// No header: keyed on the peer, which is the proxy -- shared by every
	// headerless request, but not by the header-bearing ones above.
	if got := post("d", ""); got != http.StatusNoContent {
		t.Fatalf("headerless request: %d", got)
	}
	if got := post("e", ""); got != http.StatusTooManyRequests {
		t.Fatalf("second headerless request: %d, want 429", got)
	}
}
