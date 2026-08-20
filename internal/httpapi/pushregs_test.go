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

func TestRegister_StickyOnRePost(t *testing.T) {
	t.Parallel()
	h, pushRepo, regionRepo := newPushTestServer(t)
	putRegion(t, regionRepo, 1)

	first := "token=tok1&operating_system=ios&locale=fr-FR&test_device=true&description=Aaron%27s+iPhone&apns_sandbox=true"
	if rec := pushRequest(t, h, http.MethodPost, "/api/v2/regions/1/push_registrations", formCT, first); rec.Code != http.StatusNoContent {
		t.Fatalf("first register: status = %d, want 204; body = %s", rec.Code, rec.Body.String())
	}

	second := "token=tok1&operating_system=ios"
	rec := pushRequest(t, h, http.MethodPost, "/api/v2/regions/1/push_registrations", formCT, second)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("re-register: status = %d, want 204; body = %s", rec.Code, rec.Body.String())
	}

	reg := getReg(t, pushRepo, 1, "tok1")
	if reg.Locale != "fr-FR" {
		t.Errorf("Locale = %q, want fr-FR (sticky)", reg.Locale)
	}
	if !reg.TestDevice {
		t.Errorf("TestDevice = false, want true (sticky)")
	}
	if reg.Description != "Aaron's iPhone" {
		t.Errorf("Description = %q, want %q (sticky)", reg.Description, "Aaron's iPhone")
	}
	if reg.APNSSandbox {
		t.Errorf("APNSSandbox = true, want false (non-sticky, reset absent apns_sandbox)")
	}
}

func TestRegister_ExplicitFalseDemotes(t *testing.T) {
	t.Parallel()
	h, pushRepo, regionRepo := newPushTestServer(t)
	putRegion(t, regionRepo, 1)

	first := "token=tok1&operating_system=ios&test_device=true&description=Aaron%27s+iPhone"
	if rec := pushRequest(t, h, http.MethodPost, "/api/v2/regions/1/push_registrations", formCT, first); rec.Code != http.StatusNoContent {
		t.Fatalf("first register: status = %d, want 204; body = %s", rec.Code, rec.Body.String())
	}

	second := "token=tok1&operating_system=ios&test_device=false"
	rec := pushRequest(t, h, http.MethodPost, "/api/v2/regions/1/push_registrations", formCT, second)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("demote: status = %d, want 204; body = %s", rec.Code, rec.Body.String())
	}

	reg := getReg(t, pushRepo, 1, "tok1")
	if reg.TestDevice {
		t.Errorf("TestDevice = true, want false")
	}
	if reg.Description != "" {
		t.Errorf("Description = %q, want empty (cleared on demotion)", reg.Description)
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

func TestRegister_TestDeviceRePostKeepsStoredDescription(t *testing.T) {
	t.Parallel()
	h, pushRepo, regionRepo := newPushTestServer(t)
	putRegion(t, regionRepo, 1)

	first := "token=tok1&operating_system=ios&test_device=true&description=Aaron%27s+iPhone"
	if rec := pushRequest(t, h, http.MethodPost, "/api/v2/regions/1/push_registrations", formCT, first); rec.Code != http.StatusNoContent {
		t.Fatalf("first register: status = %d, want 204; body = %s", rec.Code, rec.Body.String())
	}

	// A routine re-POST that repeats test_device=true without repeating the
	// description must not 422 -- the invariant is checked against the
	// merged row, and the stored description already satisfies it.
	second := "token=tok1&operating_system=ios&test_device=true"
	rec := pushRequest(t, h, http.MethodPost, "/api/v2/regions/1/push_registrations", formCT, second)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("re-register: status = %d, want 204; body = %s", rec.Code, rec.Body.String())
	}

	reg := getReg(t, pushRepo, 1, "tok1")
	if reg.Description != "Aaron's iPhone" {
		t.Errorf("Description = %q, want %q (kept from stored row)", reg.Description, "Aaron's iPhone")
	}
	if !reg.TestDevice {
		t.Errorf("TestDevice = false, want true")
	}
}

func TestRegister_DescriptionAloneUpdatesTestDevice(t *testing.T) {
	t.Parallel()
	h, pushRepo, regionRepo := newPushTestServer(t)
	putRegion(t, regionRepo, 1)

	first := "token=tok1&operating_system=ios&test_device=true&description=Old+name"
	if rec := pushRequest(t, h, http.MethodPost, "/api/v2/regions/1/push_registrations", formCT, first); rec.Code != http.StatusNoContent {
		t.Fatalf("first register: status = %d, want 204; body = %s", rec.Code, rec.Body.String())
	}

	second := "token=tok1&operating_system=ios&description=New+name"
	rec := pushRequest(t, h, http.MethodPost, "/api/v2/regions/1/push_registrations", formCT, second)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("update description: status = %d, want 204; body = %s", rec.Code, rec.Body.String())
	}

	reg := getReg(t, pushRepo, 1, "tok1")
	if reg.Description != "New name" {
		t.Errorf("Description = %q, want %q", reg.Description, "New name")
	}
	if !reg.TestDevice {
		t.Errorf("TestDevice = false, want true (still a test device)")
	}
}

func TestRegister_DescriptionIgnoredForNonTestDevice(t *testing.T) {
	t.Parallel()
	h, pushRepo, regionRepo := newPushTestServer(t)
	putRegion(t, regionRepo, 1)

	first := "token=tok1&operating_system=ios"
	if rec := pushRequest(t, h, http.MethodPost, "/api/v2/regions/1/push_registrations", formCT, first); rec.Code != http.StatusNoContent {
		t.Fatalf("first register: status = %d, want 204; body = %s", rec.Code, rec.Body.String())
	}

	second := "token=tok1&operating_system=ios&description=X"
	rec := pushRequest(t, h, http.MethodPost, "/api/v2/regions/1/push_registrations", formCT, second)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body = %s", rec.Code, rec.Body.String())
	}

	reg := getReg(t, pushRepo, 1, "tok1")
	if reg.Description != "" {
		t.Errorf("Description = %q, want empty (ignored for non-test devices)", reg.Description)
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

	const addr = "1.2.3.4:5555"

	post := func(token string) *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodPost,
			"/api/v2/regions/1/push_registrations", strings.NewReader("token="+token+"&operating_system=ios"))
		req.Header.Set("Content-Type", formCT)
		req.RemoteAddr = addr
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
	delReq.RemoteAddr = addr
	delRec := httptest.NewRecorder()
	h.ServeHTTP(delRec, delReq)
	if delRec.Code != http.StatusTooManyRequests {
		t.Fatalf("delete: status = %d, want 429; body = %s", delRec.Code, delRec.Body.String())
	}

	// A different client IP has its own bucket.
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost,
		"/api/v2/regions/1/push_registrations", strings.NewReader("token=tok4&operating_system=ios"))
	req.Header.Set("Content-Type", formCT)
	req.RemoteAddr = "9.9.9.9:1111"
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
type erroringPushRepo struct{ upsertErr error }

func (erroringPushRepo) Get(context.Context, int64, string) (pushreg.Registration, error) {
	return pushreg.Registration{}, pushreg.ErrNotFound
}
func (s erroringPushRepo) Upsert(context.Context, pushreg.Upsert, time.Time) error {
	return s.upsertErr
}
func (erroringPushRepo) Delete(context.Context, int64, string) error { return nil }
func (erroringPushRepo) DeleteByToken(context.Context, string) (int64, error) {
	return 0, nil
}
func (erroringPushRepo) Prune(context.Context, time.Time) (int64, error) { return 0, nil }

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
