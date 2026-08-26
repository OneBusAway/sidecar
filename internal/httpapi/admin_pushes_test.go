package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/alertpush"
	"github.com/OneBusAway/sidecar/internal/alerts"
	"github.com/OneBusAway/sidecar/internal/pushreg"
	"github.com/OneBusAway/sidecar/internal/store/sqlitetest"
)

// recordingWaker stands in for the dispatcher: the admin routes must poke it
// after every enqueue, and nothing but a recorder can prove a call that has
// no other observable effect on the response.
type recordingWaker struct{ calls int }

// Wake records one poke.
func (w *recordingWaker) Wake() { w.calls++ }

// seedAlert writes an alert straight to the store. These tests are about the
// push routes, so authoring goes through the repository rather than through a
// second endpoint whose failure would be reported here as a push bug.
func (f *adminFixture) seedAlert(t *testing.T, regionID int64, isTest bool) int64 {
	t.Helper()
	a, err := f.store.Alerts().Create(context.Background(), alerts.NewAlert{
		RegionID:        regionID,
		AgencyID:        "1",
		HeaderText:      "Route 40 detour",
		DescriptionText: "Southbound stops are closed until further notice.",
		Cause:           "CONSTRUCTION",
		Effect:          "DETOUR",
		Severity:        "WARNING",
		StartTime:       testNow,
		IsTest:          isTest,
	}, testNow)
	if err != nil {
		t.Fatalf("seed alert in region %d: %v", regionID, err)
	}
	return a.ID
}

// seedPublishedAlert is seedAlert plus the publish an enqueue requires.
func (f *adminFixture) seedPublishedAlert(t *testing.T, regionID int64, isTest bool) int64 {
	t.Helper()
	id := f.seedAlert(t, regionID, isTest)
	if err := f.store.Alerts().SetPublished(context.Background(), id, true, testNow); err != nil {
		t.Fatalf("publish alert %d: %v", id, err)
	}
	return id
}

// seedRegistration upserts one iOS registration; testDevice marks it as an
// admin test device, which is the only membership the "test" audience has.
func (f *adminFixture) seedRegistration(t *testing.T, regionID int64, token string, testDevice bool) {
	t.Helper()
	if err := f.store.PushRegs().Upsert(context.Background(), pushreg.Upsert{
		RegionID:        regionID,
		Token:           token,
		OperatingSystem: "ios",
		TestDevice:      &testDevice,
	}, testNow); err != nil {
		t.Fatalf("seed registration %q in region %d: %v", token, regionID, err)
	}
}

// pushesPath is POST/GET /alerts/{id}/pushes.
func pushesPath(alertID int64) string {
	return fmt.Sprintf("/api/admin/v1/alerts/%d/pushes", alertID)
}

// pushJSONFields pins the exact wire field names the SPA is written against;
// a rename is a silent break of a client this repo does not compile.
var pushJSONFields = []string{
	"id", "alert_id", "region_id", "audience", "status", "device_count",
	"submitted_count", "failed_count", "attempts", "last_error", "messages",
	"failure_reasons", "created_at", "started_at", "completed_at",
}

// TestAdminCreatePushQueuesAndWakes pins the whole 202 contract, including
// the dispatcher poke: without it the send waits for the next 15s tick, which
// no assertion on the response body could ever notice.
func TestAdminCreatePushQueuesAndWakes(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	id := f.seedPublishedAlert(t, regionPuget, false)
	f.seedRegistration(t, regionPuget, "tok-1", false)

	got := object(t, f.do(http.MethodPost, pushesPath(id), `{"audience":"all"}`), http.StatusAccepted)
	assertKeys(t, "pushJSON", got, pushJSONFields)

	if v := str(t, got, "status"); v != "queued" {
		t.Errorf("status = %q, want %q", v, "queued")
	}
	if v := str(t, got, "audience"); v != "all" {
		t.Errorf("audience = %q, want %q", v, "all")
	}
	if v, ok := got["alert_id"].(float64); !ok || int64(v) != id {
		t.Errorf("alert_id = %v, want %d", got["alert_id"], id)
	}
	if v, ok := got["region_id"].(float64); !ok || int64(v) != regionPuget {
		t.Errorf("region_id = %v, want %d", got["region_id"], regionPuget)
	}
	// device_count is the audience size at *send* start, which has not
	// happened yet: reporting the preview count here would make the SPA claim
	// devices were reached before a single batch was submitted.
	if v, ok := got["device_count"].(float64); !ok || v != 0 {
		t.Errorf("device_count = %v, want 0", got["device_count"])
	}
	if v := str(t, got, "last_error"); v != "" {
		t.Errorf("last_error = %q, want the empty string (never null)", v)
	}
	if got["started_at"] != nil || got["completed_at"] != nil {
		t.Errorf("started_at/completed_at = %v/%v, want null on a queued push", got["started_at"], got["completed_at"])
	}
	if v := str(t, got, "created_at"); v != testNow.Format(time.RFC3339) {
		t.Errorf("created_at = %q, want the injected clock", v)
	}

	messages, ok := got["messages"].(map[string]any)
	if !ok {
		t.Fatalf("messages = %v (%T), want an object", got["messages"], got["messages"])
	}
	if _, ok := messages["en"].(map[string]any); !ok {
		t.Errorf("messages lacks en: %v", messages)
	}
	// A push with no failures yet must still be an array, not null: the SPA
	// iterates it unconditionally.
	if reasons, ok := got["failure_reasons"].([]any); !ok || len(reasons) != 0 {
		t.Errorf("failure_reasons = %v (%T), want []", got["failure_reasons"], got["failure_reasons"])
	}

	if f.waker.calls != 1 {
		t.Errorf("Wake calls = %d, want 1", f.waker.calls)
	}
}

// TestAdminCreatePushAcceptsAnEmptyBody: the SPA's "send" button has nothing
// to say beyond the alert id, and a create that demands "{}" would make the
// simplest possible client the one that fails.
func TestAdminCreatePushAcceptsAnEmptyBody(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	id := f.seedPublishedAlert(t, regionPuget, false)
	f.seedRegistration(t, regionPuget, "tok-1", false)

	got := object(t, f.do(http.MethodPost, pushesPath(id), ""), http.StatusAccepted)
	if v := str(t, got, "audience"); v != "all" {
		t.Errorf("audience = %q, want %q (an absent audience means all)", v, "all")
	}
}

// TestAdminCreatePushPreconditions walks every refusal the enqueue can
// produce, plus the two behaviors that are easy to get backwards: a test
// alert cannot be sent to the whole region, and a second push cannot be
// queued while one is in flight.
func TestAdminCreatePushPreconditions(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	draft := f.seedAlert(t, regionPuget, false)
	published := f.seedPublishedAlert(t, regionPuget, false)
	testAlert := f.seedPublishedAlert(t, regionPuget, true)
	emptyRegionAlert := f.seedPublishedAlert(t, regionTampa, false)
	f.seedRegistration(t, regionPuget, "tok-1", false)
	f.seedRegistration(t, regionPuget, "qa", true)

	cases := []struct {
		name    string
		alertID int64
		body    string
		want    int
	}{
		{"unpublished", draft, `{}`, http.StatusConflict},
		{"unknown alert", 9999, `{}`, http.StatusNotFound},
		{"bad audience", published, `{"audience":"everyone"}`, http.StatusBadRequest},
		{"malformed body", published, `{`, http.StatusBadRequest},
		// A published alert in regionTampa, which has no registrations.
		{"empty audience", emptyRegionAlert, `{}`, http.StatusConflict},
	}
	for _, c := range cases {
		res := f.do(http.MethodPost, pushesPath(c.alertID), c.body)
		if res.Code != c.want {
			t.Errorf("%s: status = %d, want %d (%s)", c.name, res.Code, c.want, res.Body)
		}
	}
	if f.waker.calls != 0 {
		t.Errorf("Wake calls = %d after refusals only, want 0", f.waker.calls)
	}

	// A test alert is forced onto the test audience however the request is
	// spelled: "is_test" is the author's promise that no rider sees this.
	forced := object(t, f.do(http.MethodPost, pushesPath(testAlert), `{"audience":"all"}`), http.StatusAccepted)
	if v := str(t, forced, "audience"); v != "test" {
		t.Errorf("test alert audience = %q, want %q", v, "test")
	}

	if res := f.do(http.MethodPost, pushesPath(published), `{}`); res.Code != http.StatusAccepted {
		t.Fatalf("first push: status = %d, want 202 (%s)", res.Code, res.Body)
	}
	inFlight := f.do(http.MethodPost, pushesPath(published), `{}`)
	if inFlight.Code != http.StatusConflict {
		t.Errorf("second push while in flight: status = %d, want 409 (%s)", inFlight.Code, inFlight.Body)
	}
	// The refusal explains itself and carries nothing from the store: a
	// wrapped sentinel would put the failing SQL statement on the operator's
	// screen (design spec §5).
	if got := errorText(t, inFlight, http.StatusConflict); got != alertpush.ErrInFlight.Error() {
		t.Errorf("in-flight error = %q, want %q", got, alertpush.ErrInFlight.Error())
	}
}

// TestAdminListCancelAndAudience covers the three read/lifecycle routes,
// including the two scoping rules a naive implementation gets wrong: a list
// is per-alert, and a push id from another alert is a 404 rather than a
// cancel of somebody else's send.
func TestAdminListCancelAndAudience(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	id := f.seedPublishedAlert(t, regionPuget, false)
	other := f.seedPublishedAlert(t, regionPuget, false)
	f.seedRegistration(t, regionPuget, "tok-1", false)
	f.seedRegistration(t, regionPuget, "qa", true)

	aud := object(t, f.do(http.MethodGet, fmt.Sprintf("/api/admin/v1/alerts/%d/push_audience", id), ""), http.StatusOK)
	assertKeys(t, "audienceJSON", aud, []string{"all", "test", "forced_test"})
	body := f.do(http.MethodGet, fmt.Sprintf("/api/admin/v1/alerts/%d/push_audience", id), "").Body.String()
	if !strings.Contains(body, `"all":{"total":2,"ios":2,"android":0}`) {
		t.Errorf("audience all = %s, want total 2 / ios 2 / android 0", body)
	}
	if !strings.Contains(body, `"test":{"total":1,"ios":1,"android":0}`) {
		t.Errorf("audience test = %s, want total 1", body)
	}
	if boolean(t, aud, "forced_test") {
		t.Errorf("forced_test = true for a non-test alert")
	}
	if res := f.do(http.MethodGet, "/api/admin/v1/alerts/9999/push_audience", ""); res.Code != http.StatusNotFound {
		t.Errorf("audience of an unknown alert: status = %d, want 404", res.Code)
	}

	created := object(t, f.do(http.MethodPost, pushesPath(id), `{}`), http.StatusAccepted)
	pushID := jsonID(t, created)

	list := array(t, f.do(http.MethodGet, pushesPath(id), ""), http.StatusOK)
	if len(list) != 1 {
		t.Fatalf("list = %v, want exactly the one push", list)
	}
	assertKeys(t, "listed pushJSON", list[0], pushJSONFields)
	if got := jsonID(t, list[0]); got != pushID {
		t.Errorf("listed push id = %d, want %d", got, pushID)
	}
	// An alert with no pushes is an empty array, never null.
	if got := array(t, f.do(http.MethodGet, pushesPath(other), ""), http.StatusOK); len(got) != 0 {
		t.Errorf("other alert's list = %v, want []", got)
	}
	// An unknown alert is a 404, not an empty list: "no pushes" and "no such
	// alert" are different answers and the SPA renders them differently.
	if res := f.do(http.MethodGet, pushesPath(9999), ""); res.Code != http.StatusNotFound {
		t.Errorf("list for an unknown alert: status = %d, want 404 (%s)", res.Code, res.Body)
	}

	cancelPath := func(alert, push int64) string {
		return fmt.Sprintf("/api/admin/v1/alerts/%d/pushes/%d", alert, push)
	}
	if res := f.do(http.MethodDelete, cancelPath(other, pushID), ""); res.Code != http.StatusNotFound {
		t.Errorf("cross-alert cancel: status = %d, want 404", res.Code)
	}
	if res := f.do(http.MethodDelete, cancelPath(id, 9999), ""); res.Code != http.StatusNotFound {
		t.Errorf("cancel of an unknown push: status = %d, want 404", res.Code)
	}
	// A non-numeric pushId is a malformed request, not a missing push: 400
	// tells the caller their URL is wrong, where 404 would send them looking
	// for a push that was never named.
	bad := f.do(http.MethodDelete, fmt.Sprintf("%s/not-a-number", pushesPath(id)), "")
	if bad.Code != http.StatusBadRequest {
		t.Errorf("cancel with a non-numeric pushId: status = %d, want 400 (%s)", bad.Code, bad.Body)
	}
	assertContains(t, "non-numeric pushId error", errorText(t, bad, http.StatusBadRequest),
		"invalid pushId", "not-a-number")
	if res := f.do(http.MethodDelete, cancelPath(id, pushID), ""); res.Code != http.StatusNoContent {
		t.Errorf("cancel: status = %d, want 204 (%s)", res.Code, res.Body)
	}
	twice := f.do(http.MethodDelete, cancelPath(id, pushID), "")
	if twice.Code != http.StatusConflict {
		t.Errorf("cancel twice: status = %d, want 409", twice.Code)
	}
	if got := errorText(t, twice, http.StatusConflict); got != alertpush.ErrTerminal.Error() {
		t.Errorf("cancel-twice error = %q, want %q (no store wrapper)", got, alertpush.ErrTerminal.Error())
	}
	// The cross-alert cancel above must not have canceled anything: the push
	// only reached "canceled" through its own alert's route.
	stored, err := f.store.AlertPushes().Get(context.Background(), pushID)
	if err != nil {
		t.Fatalf("read back push %d: %v", pushID, err)
	}
	if stored.Status != "canceled" {
		t.Errorf("stored status = %q, want %q", stored.Status, "canceled")
	}
}

// TestAdminPushAudienceForcedTest: a test alert reports forced_test so the
// SPA hides the audience choice instead of offering one the server overrules.
func TestAdminPushAudienceForcedTest(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	id := f.seedPublishedAlert(t, regionPuget, true)
	aud := object(t, f.do(http.MethodGet, fmt.Sprintf("/api/admin/v1/alerts/%d/push_audience", id), ""), http.StatusOK)
	if !boolean(t, aud, "forced_test") {
		t.Errorf("forced_test = false for a test alert")
	}
}

// pushRoutePatterns is the §2.9 route set, spelled here rather than derived
// from adminRoutes so the table cannot silently lose one and still pass.
var pushRoutePatterns = []string{
	"POST /api/admin/v1/alerts/{id}/pushes",
	"GET /api/admin/v1/alerts/{id}/pushes",
	"DELETE /api/admin/v1/alerts/{id}/pushes/{pushId}",
	"GET /api/admin/v1/alerts/{id}/push_audience",
}

// TestAdminPushRoutesRequireSessionAndAreAbsentWithoutWaker is the wiring
// half of §2.9. The waker is the dispatcher, and main supplies it only when a
// transport is configured, so it doubles as the "a push can actually be sent"
// signal: without it the routes must not exist at all, rather than accepting
// a push that could only ever sit queued. With it, every one of the four is a
// normal session-required, cross-site-guarded admin route.
func TestAdminPushRoutesRequireSessionAndAreAbsentWithoutWaker(t *testing.T) {
	t.Parallel()

	t.Run("absent without a waker", func(t *testing.T) {
		t.Parallel()
		f := newAdminFixtureWithDeps(t, func(d *Deps) { d.AlertPushWaker = nil })

		for _, pattern := range adminRoutes(f.deps) {
			for _, want := range pushRoutePatterns {
				if pattern.pattern == want {
					t.Errorf("route %q is in the table with no waker configured", want)
				}
			}
		}
		// Not merely unlisted: not reachable either, even with a valid
		// session, so a hand-written request cannot queue an unsendable push.
		for _, pattern := range pushRoutePatterns {
			method, target := concreteRoute(t, pattern)
			if res := f.do(method, target, "{}"); res.Code != http.StatusNotFound {
				t.Errorf("%s %s with no waker: status = %d, want 404 (%s)", method, target, res.Code, res.Body)
			}
		}
	})

	t.Run("listed and session-required when fully wired", func(t *testing.T) {
		t.Parallel()
		f := newAdminFixture(t)

		// Membership only. What each route then *does* about a missing
		// session and a cross-site write is proven for all eighteen routes by
		// TestAdminRoutes_EveryRouteRequiresASession and
		// TestAdminRoutes_EveryWriteIsCrossSiteGuarded, which now walk this
		// same table; the one thing those sweeps cannot notice is a route
		// quietly dropping out of it, which is what this half pins.
		listed := map[string]bool{}
		for _, rt := range adminRoutes(f.deps) {
			for _, want := range pushRoutePatterns {
				if rt.pattern != want {
					continue
				}
				listed[want] = true
				if !rt.requiresSession {
					t.Errorf("route %q: requiresSession = false, want true", want)
				}
			}
		}
		for _, want := range pushRoutePatterns {
			if !listed[want] {
				t.Errorf("route %q is missing from the table with both deps set", want)
			}
		}
	})
}

// TestNewRouter_AlertPushRoutesRequirePushRegs: the enqueue counts the
// audience, so a router wired for push routes without the registry would
// nil-deref inside the first handler, which net/http recovers per connection
// -- the operator would see a reset request some time after deployment rather
// than a startup failure.
func TestNewRouter_AlertPushRoutesRequirePushRegs(t *testing.T) {
	t.Parallel()

	store := sqlitetest.Open(t)
	deps := Deps{
		Alerts:         store.Alerts(),
		Regions:        store.Regions(),
		Auth:           store.Auth(),
		Now:            func() time.Time { return testNow },
		Logger:         discardLogger(),
		AlertPushes:    store.AlertPushes(),
		AlertPushWaker: &recordingWaker{},
	}

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("NewRouter returned normally with AlertPushes set and PushRegs missing")
		}
		msg, ok := r.(string)
		if !ok {
			t.Fatalf("panic value = %v (%T), want a string message", r, r)
		}
		assertContains(t, "panic message", msg, "Deps.PushRegs")
	}()
	NewRouter(deps)
}

// ---------------------------------------------------------------------------
// failing stubs, for the invariant sweeps
// ---------------------------------------------------------------------------

// failingAlertPushes fails every call, so the push routes' store-error paths
// can be exercised without corrupting a real database. Every method fails,
// for the same reason failingAlerts does: a repository that broke only on the
// call a test happened to pick would let a missed error check survive.
//
// It doubles as the non-nil AlertPushes the invariant sweeps need in order to
// see the conditional routes at all.
type failingAlertPushes struct{}

func (failingAlertPushes) Create(context.Context, alertpush.NewPush, time.Time) (alertpush.Push, error) {
	return alertpush.Push{}, errStoreBroken
}
func (failingAlertPushes) Get(context.Context, int64) (alertpush.Push, error) {
	return alertpush.Push{}, errStoreBroken
}
func (failingAlertPushes) ListByAlert(context.Context, int64) ([]alertpush.Push, error) {
	return nil, errStoreBroken
}
func (failingAlertPushes) InFlightForAlert(context.Context, int64) (bool, error) {
	return false, errStoreBroken
}
func (failingAlertPushes) Claim(context.Context, time.Time, time.Time) ([]alertpush.Push, error) {
	return nil, errStoreBroken
}
func (failingAlertPushes) SetDeviceCount(context.Context, int64, int64, time.Time) error {
	return errStoreBroken
}
func (failingAlertPushes) AdvanceCursor(context.Context, int64, int64, int64, int64, time.Time) (bool, error) {
	return false, errStoreBroken
}
func (failingAlertPushes) RecordFailure(context.Context, int64, string, string, time.Time) (bool, error) {
	return false, errStoreBroken
}
func (failingAlertPushes) RecordAttempt(context.Context, int64, string, time.Time) (int64, error) {
	return 0, errStoreBroken
}
func (failingAlertPushes) MarkCompleted(context.Context, int64, alertpush.Status, string, time.Time) (bool, error) {
	return false, errStoreBroken
}
func (failingAlertPushes) Cancel(context.Context, int64, time.Time) error { return errStoreBroken }

// failingPushRegs is the registry equivalent. The push routes require a
// non-nil one at boot (they count the audience before enqueueing), so the
// store-failure sweep has to supply something; failing every call keeps it
// honest rather than quietly serving a working repository.
type failingPushRegs struct{}

func (failingPushRegs) Get(context.Context, int64, string) (pushreg.Registration, error) {
	return pushreg.Registration{}, errStoreBroken
}
func (failingPushRegs) Upsert(context.Context, pushreg.Upsert, time.Time) error {
	return errStoreBroken
}
func (failingPushRegs) Delete(context.Context, int64, string) error { return errStoreBroken }
func (failingPushRegs) DeleteByToken(context.Context, string) (int64, error) {
	return 0, errStoreBroken
}
func (failingPushRegs) Prune(context.Context, time.Time) (int64, error) { return 0, errStoreBroken }
func (failingPushRegs) ListAudience(context.Context, int64, bool, int64, int) ([]pushreg.Registration, error) {
	return nil, errStoreBroken
}
func (failingPushRegs) CountAudience(context.Context, int64, bool) (pushreg.AudienceCount, error) {
	return pushreg.AudienceCount{}, errStoreBroken
}

// TestAdminPushes_AlertPushStoreFailuresAre500 covers the error paths the
// store-failure sweep cannot reach. That sweep breaks the *alerts* store, so
// every push route fails at its first alert lookup; here the alert catalog
// works and only the push repository is broken, which is the only way to
// reach Create/InFlightForAlert and ListByAlert's own failures.
func TestAdminPushes_AlertPushStoreFailuresAre500(t *testing.T) {
	t.Parallel()

	f := newAdminFixtureWithDeps(t, func(d *Deps) { d.AlertPushes = failingAlertPushes{} })
	id := f.seedPublishedAlert(t, regionPuget, false)
	f.seedRegistration(t, regionPuget, "tok-1", false)

	for _, tc := range []struct {
		name, method, target, body string
	}{
		{"create", http.MethodPost, pushesPath(id), `{}`},
		{"list", http.MethodGet, pushesPath(id), ""},
		{"cancel", http.MethodDelete, fmt.Sprintf("%s/1", pushesPath(id)), ""},
	} {
		rec := f.do(tc.method, tc.target, tc.body)
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("%s: status = %d, want 500; body = %s", tc.name, rec.Code, rec.Body.String())
			continue
		}
		if got, want := bodyText(rec), `{"error":"internal error"}`; got != want {
			t.Errorf("%s: body = %q, want %q", tc.name, got, want)
		}
	}
}
