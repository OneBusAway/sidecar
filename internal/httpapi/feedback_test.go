package httpapi_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/alertpush"
	"github.com/OneBusAway/sidecar/internal/httpapi"
	"github.com/OneBusAway/sidecar/internal/liveactivities"
	"github.com/OneBusAway/sidecar/internal/pushreg"
	"github.com/OneBusAway/sidecar/internal/ratelimit"
	"github.com/OneBusAway/sidecar/internal/regions"
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

func (f *fakePushRepo) ListAudience(ctx context.Context, regionID int64, testOnly bool, afterID int64, limit int) ([]pushreg.Registration, error) {
	return f.real.ListAudience(ctx, regionID, testOnly, afterID, limit)
}

func (f *fakePushRepo) CountAudience(ctx context.Context, regionID int64, testOnly bool) (pushreg.AudienceCount, error) {
	return f.real.CountAudience(ctx, regionID, testOnly)
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

// feedbackServerWithSecret builds a router whose gorush webhook requires the
// given bearer token, plus the fake repo so a test can prove no delete ran.
func feedbackServerWithSecret(t *testing.T, secret string) (http.Handler, *fakePushRepo) {
	t.Helper()
	store := sqlitetest.Open(t)
	repo := &fakePushRepo{real: store.PushRegs()}
	return httpapi.NewRouter(httpapi.Deps{
		PushRegs:       repo,
		Regions:        store.Regions(),
		Now:            func() time.Time { return base },
		Logger:         slog.New(slog.DiscardHandler),
		FeedbackSecret: secret,
	}), repo
}

// authedFeedbackRequest is feedbackRequest with an Authorization header.
func authedFeedbackRequest(t *testing.T, h http.Handler, authorization, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/webhooks/gorush", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestFeedbackSecretRequiredWhenConfigured pins the shared-secret gate. The
// webhook deletes a token's registrations in every region on a caller-supplied
// value, so an unauthorized caller must be turned away before the body is even
// read -- otherwise the response distinguishes a token that exists from one
// that does not.
func TestFeedbackSecretRequiredWhenConfigured(t *testing.T) {
	t.Parallel()
	const secret = "s3cr3t-value"
	const body = `{"type":"failed-push","platform":"ios","token":"tok1","error":"Unregistered"}`

	for _, tc := range []struct {
		name       string
		authHeader string
		wantStatus int
		wantDelete bool
	}{
		{"correct secret", "Bearer " + secret, http.StatusOK, true},
		{"wrong secret", "Bearer nope", http.StatusUnauthorized, false},
		{"no header at all", "", http.StatusUnauthorized, false},
		// The prefix is optional on purpose: requiring it would add no
		// security (the secret still has to match) while breaking a sender
		// that can only set a raw header value.
		{"right value, no Bearer prefix", secret, http.StatusOK, true},
		{"prefix of the real secret", "Bearer s3cr3t", http.StatusUnauthorized, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h, repo := feedbackServerWithSecret(t, secret)
			rec := authedFeedbackRequest(t, h, tc.authHeader, body)
			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d; body = %s", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if repo.deleteByTokenCalled != tc.wantDelete {
				t.Errorf("DeleteByToken called = %v, want %v", repo.deleteByTokenCalled, tc.wantDelete)
			}
		})
	}
}

// TestFeedbackOpenWebhookIsThrottled pins the other half of the trade: with no
// secret configured the endpoint stays open so a deployment whose gorush
// cannot send a header keeps its prune signal, but it is no longer an
// unmetered path to cross-region deletes.
func TestFeedbackOpenWebhookIsThrottled(t *testing.T) {
	t.Parallel()
	store := sqlitetest.Open(t)
	h := httpapi.NewRouter(httpapi.Deps{
		PushRegs: store.PushRegs(),
		Regions:  store.Regions(),
		Now:      func() time.Time { return base },
		Logger:   slog.New(slog.DiscardHandler),
		// No FeedbackSecret: the open configuration.
		FeedbackLimiter: ratelimit.New(2, time.Minute),
	})

	const body = `{"type":"failed-push","platform":"ios","token":"tok1","error":"Timeout"}`
	post := func(port int) int {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/webhooks/gorush", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		// A fresh source port per request, as real ones have: the bucket
		// keys on the host, so reusing one address would pass even against
		// a limiter keyed on host:port.
		req.RemoteAddr = fmt.Sprintf("1.2.3.4:%d", port)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	for i, port := range []int{5001, 5002} {
		if got := post(port); got != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200", i+1, got)
		}
	}
	if got := post(5003); got != http.StatusTooManyRequests {
		t.Errorf("third request: status = %d, want 429", got)
	}
	// A different host keeps its own budget.
	if got := post2(t, h, "9.9.9.9:5001", body); got != http.StatusOK {
		t.Errorf("other IP: status = %d, want 200", got)
	}
}

func post2(t *testing.T, h http.Handler, addr, body string) int {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/webhooks/gorush", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = addr
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

// TestFeedbackAuthenticatedWebhookIsNotThrottled pins the reason the secret
// exists. gorush reports one failure per dead token, so a mass uninstall
// arrives as a burst; throttling a caller we have already authenticated would
// drop those prune signals for no security gain. The limiter injected here is
// deliberately tiny, so this asserts the routing decision -- no bucket at all
// on the authenticated path -- rather than that the default bucket happens to
// be roomy enough.
func TestFeedbackAuthenticatedWebhookIsNotThrottled(t *testing.T) {
	t.Parallel()
	const secret = "s3cr3t-value"
	store := sqlitetest.Open(t)
	h := httpapi.NewRouter(httpapi.Deps{
		PushRegs:        store.PushRegs(),
		Regions:         store.Regions(),
		Now:             func() time.Time { return base },
		Logger:          slog.New(slog.DiscardHandler),
		FeedbackSecret:  secret,
		FeedbackLimiter: ratelimit.New(2, time.Minute),
	})

	for i := range 5 {
		body := fmt.Sprintf(`{"type":"failed-push","platform":"ios","token":"tok%d","error":"Unregistered"}`, i)
		if got := authedFeedbackRequest(t, h, "Bearer "+secret, body).Code; got != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200 (an authenticated webhook must never be throttled)", i, got)
		}
	}
}

func seedLiveActivity(t *testing.T, repo liveactivities.Repository, regionRepo regions.Repository, pushToken string) {
	t.Helper()
	putRegionWithBaseURL(t, regionRepo, 1, "https://sidecar.example")
	_, err := repo.Upsert(context.Background(), liveactivities.NewLiveActivity{
		RegionID: 1, Token: "la-" + pushToken, ExpiresAt: base.Add(time.Hour), ActivityID: "act-" + pushToken,
		PushToken: pushToken, StopID: "1_570", RouteShortName: "44", TripHeadsign: "Ballard",
	}, base)
	if err != nil {
		t.Fatal(err)
	}
}

func TestFeedbackTerminalDeletesLiveActivityAndRegistration(t *testing.T) {
	t.Parallel()
	store := sqlitetest.Open(t)
	seedLiveActivity(t, store.LiveActivities(), store.Regions(), "dead-tok")
	if err := store.PushRegs().Upsert(context.Background(), pushreg.Upsert{RegionID: 1, Token: "dead-tok", OperatingSystem: "ios"}, base); err != nil {
		t.Fatal(err)
	}
	h := httpapi.NewRouter(httpapi.Deps{
		PushRegs: store.PushRegs(), LiveActivities: store.LiveActivities(), Regions: store.Regions(),
		Now: func() time.Time { return base }, Logger: slog.New(slog.DiscardHandler), FeedbackSecret: "s3",
	})
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/webhooks/gorush",
		strings.NewReader(`{"type":"failed-push","platform":"ios","token":"dead-tok","error":"Unregistered"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer s3")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if list, _ := store.LiveActivities().List(context.Background()); len(list) != 0 {
		t.Errorf("live activity survived terminal feedback: %+v", list)
	}
	if n, _ := store.PushRegs().DeleteByToken(context.Background(), "dead-tok"); n != 0 {
		t.Errorf("registration survived: %d", n)
	}
}

// gorush does not retry a webhook on a non-2xx, so a failing registration
// delete must not skip the Live Activity prune -- the row would otherwise be
// pushed to every minute until expiry. Both run; the status reports the
// failure.
func TestFeedbackPrunesLiveActivityWhenRegistrationDeleteFails(t *testing.T) {
	t.Parallel()
	store := sqlitetest.Open(t)
	seedLiveActivity(t, store.LiveActivities(), store.Regions(), "dead-tok")
	h := httpapi.NewRouter(httpapi.Deps{
		PushRegs:       erroringPushRepo{deleteErr: errors.New("db locked")},
		LiveActivities: store.LiveActivities(), Regions: store.Regions(),
		Now: func() time.Time { return base }, Logger: slog.New(slog.DiscardHandler), FeedbackSecret: "s3",
	})
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/webhooks/gorush",
		strings.NewReader(`{"token":"dead-tok","error":"Unregistered"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer s3")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 (a delete failed)", rec.Code)
	}
	if list, _ := store.LiveActivities().List(context.Background()); len(list) != 0 {
		t.Errorf("live activity must still be pruned when the registration delete fails: %+v", list)
	}
}

func TestFeedbackRegisteredWithLiveActivitiesOnly(t *testing.T) {
	t.Parallel()
	store := sqlitetest.Open(t)
	seedLiveActivity(t, store.LiveActivities(), store.Regions(), "dead-tok")
	h := httpapi.NewRouter(httpapi.Deps{
		LiveActivities: store.LiveActivities(), Regions: store.Regions(),
		Now: func() time.Time { return base }, Logger: slog.New(slog.DiscardHandler), FeedbackSecret: "s3",
	})
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/webhooks/gorush",
		strings.NewReader(`{"token":"dead-tok","error":"BadDeviceToken"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer s3")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (webhook must exist without PushRegs)", rec.Code)
	}
	if list, _ := store.LiveActivities().List(context.Background()); len(list) != 0 {
		t.Errorf("live activity survived: %+v", list)
	}
}

func TestFeedbackNonTerminalKeepsLiveActivity(t *testing.T) {
	t.Parallel()
	store := sqlitetest.Open(t)
	seedLiveActivity(t, store.LiveActivities(), store.Regions(), "ok-tok")
	h := httpapi.NewRouter(httpapi.Deps{
		LiveActivities: store.LiveActivities(), Regions: store.Regions(),
		Now: func() time.Time { return base }, Logger: slog.New(slog.DiscardHandler), FeedbackSecret: "s3",
	})
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/webhooks/gorush",
		strings.NewReader(`{"token":"ok-tok","error":"ExpiredProviderToken"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer s3")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if list, _ := store.LiveActivities().List(context.Background()); len(list) != 1 {
		t.Errorf("non-terminal reason must not delete: %+v", list)
	}
}

// TestFeedbackRecordsAlertPushFailureByNotifID is the §2.8 correlation: a
// bounce carrying the notif_id the fan-out stamped on its batch counts
// against that push, whether or not the reason is terminal. Most gorush
// bounces are non-terminal, so accounting that only ran on the terminal path
// would report a near-zero failure count for a send that mostly failed.
func TestFeedbackRecordsAlertPushFailureByNotifID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := sqlitetest.Open(t)
	putRegion(t, store.Regions(), 1)
	alertID := publishAlert(t, store.Alerts(), 1, "Route 40 detour", false)

	// tok3 and tok4 exist so the unknown-notif_id and no-notif_id cases each
	// carry a token this push has never recorded a failure for: RecordFailure
	// deduplicates on (push_id, token), so reusing tok2 there would hold
	// failed_count at 2 even if the handler wrongly recorded the bounce.
	for _, token := range []string{"tok", "tok2", "tok3", "tok4"} {
		if err := store.PushRegs().Upsert(ctx, pushreg.Upsert{
			RegionID: 1, Token: token, OperatingSystem: "ios",
		}, base); err != nil {
			t.Fatalf("upsert %q: %v", token, err)
		}
	}

	enq := &alertpush.Enqueuer{Repo: store.AlertPushes(), Alerts: store.Alerts(), PushRegs: store.PushRegs()}
	p, err := enq.Enqueue(ctx, alertID, alertpush.AudienceAll, base)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	h := httpapi.NewRouter(httpapi.Deps{
		PushRegs:    store.PushRegs(),
		AlertPushes: store.AlertPushes(),
		Regions:     store.Regions(),
		Now:         func() time.Time { return base },
		Logger:      slog.New(slog.DiscardHandler),
	})

	notifID := alertpush.NotifID(p.ID)
	terminal := fmt.Sprintf(
		`{"type":"failed-push","platform":"ios","token":"tok","error":"Unregistered","notif_id":%q}`, notifID)
	feedbackOK(t, h, "terminal feedback", terminal)
	assertPushFailedCount(t, store.AlertPushes(), p.ID, 1, "after one terminal bounce")
	// The accounting is additional to the prune, not instead of it.
	if _, getErr := store.PushRegs().Get(ctx, 1, "tok"); !errors.Is(getErr, pushreg.ErrNotFound) {
		t.Errorf("registration survived a terminal bounce: err = %v, want ErrNotFound", getErr)
	}

	// gorush can replay its feedback; a replayed token must not be counted
	// twice or the failure total would drift above the audience size.
	feedbackOK(t, h, "replayed feedback", terminal)
	assertPushFailedCount(t, store.AlertPushes(), p.ID, 1, "after a replay")

	// A non-terminal reason still counts against the push, and still leaves
	// the registration alone: the token is alive, the delivery is not.
	nonTerminal := fmt.Sprintf(
		`{"type":"failed-push","platform":"ios","token":"tok2","error":"TooManyRequests","notif_id":%q}`, notifID)
	feedbackOK(t, h, "non-terminal feedback", nonTerminal)
	assertPushFailedCount(t, store.AlertPushes(), p.ID, 2, "after a non-terminal bounce")
	if _, getErr := store.PushRegs().Get(ctx, 1, "tok2"); getErr != nil {
		t.Errorf("non-terminal bounce deleted the registration: %v", getErr)
	}

	// A stale or foreign notif_id is a dead end, not a 500: gorush does not
	// retry, so answering 500 would only lose the prune signal in the same
	// payload.
	unknown := `{"type":"failed-push","platform":"ios","token":"tok3","error":"TooManyRequests","notif_id":"alertpush:999999"}`
	feedbackOK(t, h, "unknown notif_id", unknown)
	assertPushFailedCount(t, store.AlertPushes(), p.ID, 2, "after an unknown notif_id (nothing recorded)")

	// Feedback with no notif_id behaves exactly as it did before §2.8.
	plain := `{"type":"failed-push","platform":"ios","token":"tok4","error":"TooManyRequests"}`
	feedbackOK(t, h, "no notif_id", plain)
	assertPushFailedCount(t, store.AlertPushes(), p.ID, 2, "after feedback with no notif_id")
}

// feedbackOK posts one gorush feedback payload and fails the test unless the
// webhook answered 200 -- the webhook contract every case below shares.
func feedbackOK(t *testing.T, h http.Handler, what, body string) {
	t.Helper()
	if rec := feedbackRequest(t, h, body); rec.Code != http.StatusOK {
		t.Fatalf("%s: status = %d, want 200; body = %s", what, rec.Code, rec.Body.String())
	}
}

// assertPushFailedCount reads a push back and pins its failure total.
func assertPushFailedCount(t *testing.T, repo alertpush.Repository, id, want int64, what string) {
	t.Helper()
	got, err := repo.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("get push %d: %v", id, err)
	}
	if got.FailedCount != want {
		t.Errorf("failed_count %s = %d, want %d", what, got.FailedCount, want)
	}
}

// TestFeedbackAccountingWithoutAlertPushes: a deployment with no alert push
// repository wired (a feed-only or Live-Activities-only sidecar) must still
// answer the webhook normally rather than nil-deref on a notif_id it has no
// table for.
func TestFeedbackAccountingWithoutAlertPushes(t *testing.T) {
	t.Parallel()
	store := sqlitetest.Open(t)
	putRegion(t, store.Regions(), 1)
	if err := store.PushRegs().Upsert(context.Background(), pushreg.Upsert{
		RegionID: 1, Token: "tok", OperatingSystem: "ios",
	}, base); err != nil {
		t.Fatal(err)
	}
	h := httpapi.NewRouter(httpapi.Deps{
		PushRegs: store.PushRegs(), Regions: store.Regions(),
		Now: func() time.Time { return base }, Logger: slog.New(slog.DiscardHandler),
	})
	body := `{"type":"failed-push","platform":"ios","token":"tok","error":"Unregistered","notif_id":"alertpush:1"}`
	if rec := feedbackRequest(t, h, body); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if _, err := store.PushRegs().Get(context.Background(), 1, "tok"); !errors.Is(err, pushreg.ErrNotFound) {
		t.Errorf("terminal prune stopped working: err = %v, want ErrNotFound", err)
	}
}
