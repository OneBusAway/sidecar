package httpapi_test

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/httpapi"
	"github.com/OneBusAway/sidecar/internal/pushreg"
	"github.com/OneBusAway/sidecar/internal/store/sqlitetest"
)

// fakePushRepo wraps a real pushreg.Repository but allows test control over
// the DeleteByToken method (to verify it was called and with what token).
type fakePushRepo struct {
	real                pushreg.Repository
	deleteByTokenCalled bool
	deleteByTokenToken  string
	deleteByTokenErr    error
}

func (f *fakePushRepo) Get(ctx context.Context, regionID int64, token string) (pushreg.Registration, error) {
	return f.real.Get(ctx, regionID, token)
}

func (f *fakePushRepo) Upsert(ctx context.Context, in pushreg.Upsert, now time.Time) error {
	return f.real.Upsert(ctx, in, now)
}

func (f *fakePushRepo) Delete(ctx context.Context, regionID int64, token string) error {
	return f.real.Delete(ctx, regionID, token)
}

func (f *fakePushRepo) DeleteByToken(ctx context.Context, token string) (int64, error) {
	f.deleteByTokenCalled = true
	f.deleteByTokenToken = token
	if f.deleteByTokenErr != nil {
		return 0, f.deleteByTokenErr
	}
	return f.real.DeleteByToken(ctx, token)
}

func (f *fakePushRepo) Prune(ctx context.Context, cutoff time.Time) (int64, error) {
	return f.real.Prune(ctx, cutoff)
}

// feedbackRequest issues a POST to /webhooks/gorush with a JSON body.
func feedbackRequest(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/webhooks/gorush", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestFeedbackTerminalDeletesRegistration(t *testing.T) {
	t.Parallel()
	store := sqlitetest.Open(t)
	realRegions := store.Regions()
	realRepo := store.PushRegs()
	fakeRepo := &fakePushRepo{real: realRepo}

	// Seed regions
	putRegion(t, realRegions, 1)
	putRegion(t, realRegions, 2)

	// Create router with fake repo
	deps := httpapi.Deps{
		PushRegs: fakeRepo,
		Regions:  realRegions,
		Now:      func() time.Time { return base },
		Logger:   slog.New(slog.DiscardHandler),
	}
	h := httpapi.NewRouter(deps)

	// Seed registrations in multiple regions
	if err := realRepo.Upsert(context.Background(), pushreg.Upsert{
		RegionID:        1,
		Token:           "tok1",
		OperatingSystem: "ios",
	}, base); err != nil {
		t.Fatalf("upsert to region 1: %v", err)
	}
	if err := realRepo.Upsert(context.Background(), pushreg.Upsert{
		RegionID:        2,
		Token:           "tok1",
		OperatingSystem: "ios",
	}, base); err != nil {
		t.Fatalf("upsert to region 2: %v", err)
	}

	// POST terminal error feedback
	body := `{"type":"failed-push","platform":"ios","token":"tok1","error":"Unregistered"}`
	rec := feedbackRequest(t, h, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	// Verify DeleteByToken was called
	if !fakeRepo.deleteByTokenCalled {
		t.Errorf("DeleteByToken was not called")
	}
	if fakeRepo.deleteByTokenToken != "tok1" {
		t.Errorf("DeleteByToken called with token %q, want tok1", fakeRepo.deleteByTokenToken)
	}

	// Verify registrations are gone
	if _, err := realRepo.Get(context.Background(), 1, "tok1"); err == nil {
		t.Errorf("region 1: Get returned registration, want error")
	}
	if _, err := realRepo.Get(context.Background(), 2, "tok1"); err == nil {
		t.Errorf("region 2: Get returned registration, want error")
	}
}

func TestFeedbackTransientKeepsRegistration(t *testing.T) {
	t.Parallel()
	store := sqlitetest.Open(t)
	putRegion(t, store.Regions(), 1)
	realRepo := store.PushRegs()
	fakeRepo := &fakePushRepo{real: realRepo}

	deps := httpapi.Deps{
		PushRegs: fakeRepo,
		Regions:  store.Regions(),
		Now:      func() time.Time { return base },
		Logger:   slog.New(slog.DiscardHandler),
	}
	h := httpapi.NewRouter(deps)

	// Seed registration
	if err := realRepo.Upsert(context.Background(), pushreg.Upsert{
		RegionID:        1,
		Token:           "tok1",
		OperatingSystem: "ios",
	}, base); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// POST transient error feedback (ExpiredProviderToken is transient)
	body := `{"type":"failed-push","platform":"ios","token":"tok1","error":"ExpiredProviderToken"}`
	rec := feedbackRequest(t, h, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	// Verify DeleteByToken was NOT called (transient error)
	if fakeRepo.deleteByTokenCalled {
		t.Errorf("DeleteByToken was called for transient error")
	}

	// Verify registration still exists
	_, err := realRepo.Get(context.Background(), 1, "tok1")
	if err != nil {
		t.Errorf("Get returned %v, want registration", err)
	}
}

func TestFeedbackUnknownTokenIsOK(t *testing.T) {
	t.Parallel()
	store := sqlitetest.Open(t)
	putRegion(t, store.Regions(), 1)
	realRepo := store.PushRegs()
	fakeRepo := &fakePushRepo{real: realRepo}

	deps := httpapi.Deps{
		PushRegs: fakeRepo,
		Regions:  store.Regions(),
		Now:      func() time.Time { return base },
		Logger:   slog.New(slog.DiscardHandler),
	}
	h := httpapi.NewRouter(deps)

	// POST terminal error for token that doesn't exist
	body := `{"type":"failed-push","platform":"ios","token":"nonexistent","error":"Unregistered"}`
	rec := feedbackRequest(t, h, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	// Verify DeleteByToken was still called (should be idempotent)
	if !fakeRepo.deleteByTokenCalled {
		t.Errorf("DeleteByToken was not called")
	}
}

func TestFeedbackMalformedBodyIs400(t *testing.T) {
	t.Parallel()
	store := sqlitetest.Open(t)
	fakeRepo := &fakePushRepo{real: store.PushRegs()}

	deps := httpapi.Deps{
		PushRegs: fakeRepo,
		Regions:  store.Regions(),
		Now:      func() time.Time { return base },
		Logger:   slog.New(slog.DiscardHandler),
	}
	h := httpapi.NewRouter(deps)

	// POST garbage JSON
	body := `{this is not json`
	rec := feedbackRequest(t, h, body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
}

func TestFeedbackNeverLogsToken(t *testing.T) {
	t.Parallel()
	const token = "supersecrettoken-abc123"

	store := sqlitetest.Open(t)
	putRegion(t, store.Regions(), 1)

	// Use a fake repo that returns an error containing the token
	fakeRepo := &fakePushRepo{
		real:             store.PushRegs(),
		deleteByTokenErr: fmt.Errorf("database constraint failed for token %s", token),
	}

	var buf bytes.Buffer
	deps := httpapi.Deps{
		PushRegs: fakeRepo,
		Regions:  store.Regions(),
		Now:      func() time.Time { return base },
		Logger:   slog.New(slog.NewTextHandler(&buf, nil)),
	}
	h := httpapi.NewRouter(deps)

	// POST terminal error feedback
	body := `{"type":"failed-push","platform":"ios","token":"` + token + `","error":"Unregistered"}`
	rec := feedbackRequest(t, h, body)
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

func TestFeedbackSuccessLogNeverLogsToken(t *testing.T) {
	t.Parallel()
	const token = "supersecrettoken-abc123"

	store := sqlitetest.Open(t)
	putRegion(t, store.Regions(), 1)
	realRepo := store.PushRegs()

	// Seed a real registration
	if err := realRepo.Upsert(context.Background(), pushreg.Upsert{
		RegionID:        1,
		Token:           token,
		OperatingSystem: "ios",
	}, base); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	// Create router with logs captured to a buffer
	var buf bytes.Buffer
	deps := httpapi.Deps{
		PushRegs: realRepo,
		Regions:  store.Regions(),
		Now:      func() time.Time { return base },
		Logger:   slog.New(slog.NewTextHandler(&buf, nil)),
	}
	h := httpapi.NewRouter(deps)

	// POST terminal error feedback for the real token
	body := `{"type":"failed-push","platform":"ios","token":"` + token + `","error":"Unregistered"}`
	rec := feedbackRequest(t, h, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	// Verify the registration is gone
	_, err := realRepo.Get(context.Background(), 1, token)
	if err == nil {
		t.Errorf("Get returned registration, want error (registration should be deleted)")
	}

	logOutput := buf.String()

	// Verify the success log fired (contains the key phrase that proves the path was taken)
	if !strings.Contains(logOutput, "pruned dead push token") {
		t.Errorf("log output missing success log line: %s", logOutput)
	}

	// Verify the raw token does NOT appear in the logs
	if strings.Contains(logOutput, token) {
		t.Errorf("log output contains the raw token: %s", logOutput)
	}

	// Verify no attempt to log the token (this would appear as the token itself,
	// not as [token], since we don't sanitize on the success path intentionally)
	if strings.Contains(logOutput, "[token]") {
		t.Errorf("log output should not contain [token] marker on success path, got: %s", logOutput)
	}
}
