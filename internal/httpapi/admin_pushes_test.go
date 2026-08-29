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
	"github.com/OneBusAway/sidecar/internal/apikey"
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
	f.seedRegistrationOn(t, regionID, token, "ios", testDevice)
}

// seedRegistrationOn is seedRegistration with an explicit platform. It is a
// second helper rather than a widened seedRegistration signature, so every
// existing alert-push test that calls seedRegistration keeps compiling
// unchanged.
func (f *adminFixture) seedRegistrationOn(t *testing.T, regionID int64, token, platform string, testDevice bool) {
	t.Helper()
	if err := f.store.PushRegs().Upsert(context.Background(), pushreg.Upsert{
		RegionID:        regionID,
		Token:           token,
		OperatingSystem: platform,
		TestDevice:      &testDevice,
	}, testNow); err != nil {
		t.Fatalf("seed registration %q in region %d: %v", token, regionID, err)
	}
}

// pushesPath is POST/GET /regions/{regionId}/alerts/{id}/pushes.
func pushesPath(regionID, alertID int64) string {
	return fmt.Sprintf("/api/admin/v1/regions/%d/alerts/%d/pushes", regionID, alertID)
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

	got := object(t, f.do(http.MethodPost, pushesPath(regionPuget, id), `{"audience":"all"}`), http.StatusAccepted)
	assertKeys(t, "pushJSON", got, pushJSONFields)

	if v := str(t, got, "status"); v != "queued" {
		t.Errorf("status = %q, want %q", v, "queued")
	}
	if v := str(t, got, "audience"); v != "all" {
		t.Errorf("audience = %q, want %q", v, "all")
	}
	if v := int64(num(t, got, "alert_id")); v != id {
		t.Errorf("alert_id = %v, want %d", v, id)
	}
	if v := int64(num(t, got, "region_id")); v != regionPuget {
		t.Errorf("region_id = %v, want %d", v, regionPuget)
	}
	// device_count is the audience size at *send* start, which has not
	// happened yet: reporting the preview count here would make the SPA claim
	// devices were reached before a single batch was submitted.
	if v := num(t, got, "device_count"); v != 0 {
		t.Errorf("device_count = %v, want 0", v)
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

	assertEnglishMessages(t, got)
	// A push with no failures yet must still be an array, not null: the SPA
	// iterates it unconditionally.
	assertEmptyJSONArray(t, got, "failure_reasons")

	if f.waker.calls != 1 {
		t.Errorf("Wake calls = %d, want 1", f.waker.calls)
	}
}

// assertEnglishMessages fails unless the push body carries a messages object
// with the English copy every push must have.
func assertEnglishMessages(t *testing.T, got map[string]any) {
	t.Helper()
	messages, ok := got["messages"].(map[string]any)
	if !ok {
		t.Fatalf("messages = %v (%T), want an object", got["messages"], got["messages"])
	}
	if _, ok := messages["en"].(map[string]any); !ok {
		t.Errorf("messages lacks en: %v", messages)
	}
}

// assertEmptyJSONArray fails unless key holds an empty JSON array -- never
// null, which the SPA would not survive iterating.
func assertEmptyJSONArray(t *testing.T, got map[string]any, key string) {
	t.Helper()
	arr, ok := got[key].([]any)
	if !ok {
		t.Fatalf("%s = %v (%T), want an array", key, got[key], got[key])
	}
	if len(arr) != 0 {
		t.Errorf("%s = %v, want []", key, arr)
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

	got := object(t, f.do(http.MethodPost, pushesPath(regionPuget, id), ""), http.StatusAccepted)
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
		name     string
		regionID int64
		alertID  int64
		body     string
		want     int
	}{
		{"unpublished", regionPuget, draft, `{}`, http.StatusConflict},
		{"unknown alert", regionPuget, 9999, `{}`, http.StatusNotFound},
		{"bad audience", regionPuget, published, `{"audience":"everyone"}`, http.StatusBadRequest},
		{"malformed body", regionPuget, published, `{`, http.StatusBadRequest},
		// A published alert in regionTampa, which has no registrations.
		{"empty audience", regionTampa, emptyRegionAlert, `{}`, http.StatusConflict},
	}
	for _, c := range cases {
		res := f.do(http.MethodPost, pushesPath(c.regionID, c.alertID), c.body)
		if res.Code != c.want {
			t.Errorf("%s: status = %d, want %d (%s)", c.name, res.Code, c.want, res.Body)
		}
	}
	if f.waker.calls != 0 {
		t.Errorf("Wake calls = %d after refusals only, want 0", f.waker.calls)
	}

	// A test alert is forced onto the test audience however the request is
	// spelled: "is_test" is the author's promise that no rider sees this.
	forced := object(t, f.do(http.MethodPost, pushesPath(regionPuget, testAlert), `{"audience":"all"}`), http.StatusAccepted)
	if v := str(t, forced, "audience"); v != "test" {
		t.Errorf("test alert audience = %q, want %q", v, "test")
	}

	if res := f.do(http.MethodPost, pushesPath(regionPuget, published), `{}`); res.Code != http.StatusAccepted {
		t.Fatalf("first push: status = %d, want 202 (%s)", res.Code, res.Body)
	}
	inFlight := f.do(http.MethodPost, pushesPath(regionPuget, published), `{}`)
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

	assertPushAudienceReport(t, f, id)
	pushID := assertPushListIsPerAlert(t, f, id, other)
	assertPushCancelScoping(t, f, id, other, pushID)
}

// assertStatus issues one admin request and pins only its status code, which
// is all the scoping rules in these tests are about.
func (f *adminFixture) assertStatus(t *testing.T, method, target, body string, want int, what string) {
	t.Helper()
	if res := f.do(method, target, body); res.Code != want {
		t.Errorf("%s: status = %d, want %d (%s)", what, res.Code, want, res.Body)
	}
}

// audiencePath is GET /regions/{regionId}/alerts/{id}/push_audience.
func audiencePath(regionID, alertID int64) string {
	return fmt.Sprintf("/api/admin/v1/regions/%d/alerts/%d/push_audience", regionID, alertID)
}

// cancelPath is DELETE /regions/{regionId}/alerts/{id}/pushes/{pushId}.
func cancelPath(regionID, alertID, pushID int64) string {
	return fmt.Sprintf("/api/admin/v1/regions/%d/alerts/%d/pushes/%d", regionID, alertID, pushID)
}

// assertPushAudienceReport pins the reach preview: both audience sizes split
// by platform, forced_test off for a normal alert, and a 404 for an alert
// that does not exist.
func assertPushAudienceReport(t *testing.T, f *adminFixture, id int64) {
	t.Helper()
	aud := object(t, f.do(http.MethodGet, audiencePath(regionPuget, id), ""), http.StatusOK)
	assertKeys(t, "audienceJSON", aud, []string{"all", "test", "forced_test"})
	body := f.do(http.MethodGet, audiencePath(regionPuget, id), "").Body.String()
	if !strings.Contains(body, `"all":{"total":2,"ios":2,"android":0}`) {
		t.Errorf("audience all = %s, want total 2 / ios 2 / android 0", body)
	}
	if !strings.Contains(body, `"test":{"total":1,"ios":1,"android":0}`) {
		t.Errorf("audience test = %s, want total 1", body)
	}
	if boolean(t, aud, "forced_test") {
		t.Errorf("forced_test = true for a non-test alert")
	}
	f.assertStatus(t, http.MethodGet, audiencePath(regionPuget, 9999), "", http.StatusNotFound,
		"audience of an unknown alert")
}

// assertPushListIsPerAlert queues one push on id and pins that the list is
// scoped to its own alert, empty (never null) for an alert with no pushes,
// and a 404 for an alert that does not exist. It returns the queued push id.
func assertPushListIsPerAlert(t *testing.T, f *adminFixture, id, other int64) int64 {
	t.Helper()
	created := object(t, f.do(http.MethodPost, pushesPath(regionPuget, id), `{}`), http.StatusAccepted)
	pushID := jsonID(t, created)

	list := array(t, f.do(http.MethodGet, pushesPath(regionPuget, id), ""), http.StatusOK)
	if len(list) != 1 {
		t.Fatalf("list = %v, want exactly the one push", list)
	}
	assertKeys(t, "listed pushJSON", list[0], pushJSONFields)
	if got := jsonID(t, list[0]); got != pushID {
		t.Errorf("listed push id = %d, want %d", got, pushID)
	}
	// An alert with no pushes is an empty array, never null.
	if got := array(t, f.do(http.MethodGet, pushesPath(regionPuget, other), ""), http.StatusOK); len(got) != 0 {
		t.Errorf("other alert's list = %v, want []", got)
	}
	// An unknown alert is a 404, not an empty list: "no pushes" and "no such
	// alert" are different answers and the SPA renders them differently.
	f.assertStatus(t, http.MethodGet, pushesPath(regionPuget, 9999), "", http.StatusNotFound,
		"list for an unknown alert")
	return pushID
}

// assertPushCancelScoping pins the cancel route: another alert's id cannot
// cancel this push, an unknown push is a 404, a non-numeric pushId is a 400,
// the real cancel is a 204, and a second cancel is a 409 carrying the
// sentinel's own text.
func assertPushCancelScoping(t *testing.T, f *adminFixture, id, other, pushID int64) {
	t.Helper()
	f.assertStatus(t, http.MethodDelete, cancelPath(regionPuget, other, pushID), "", http.StatusNotFound,
		"cross-alert cancel")
	f.assertStatus(t, http.MethodDelete, cancelPath(regionPuget, id, 9999), "", http.StatusNotFound,
		"cancel of an unknown push")

	// A non-numeric pushId is a malformed request, not a missing push: 400
	// tells the caller their URL is wrong, where 404 would send them looking
	// for a push that was never named.
	bad := f.do(http.MethodDelete, pushesPath(regionPuget, id)+"/not-a-number", "")
	if bad.Code != http.StatusBadRequest {
		t.Errorf("cancel with a non-numeric pushId: status = %d, want 400 (%s)", bad.Code, bad.Body)
	}
	assertContains(t, "non-numeric pushId error", errorText(t, bad, http.StatusBadRequest),
		"invalid pushId", "not-a-number")

	f.assertStatus(t, http.MethodDelete, cancelPath(regionPuget, id, pushID), "", http.StatusNoContent, "cancel")
	twice := f.do(http.MethodDelete, cancelPath(regionPuget, id, pushID), "")
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
	aud := object(t, f.do(http.MethodGet, audiencePath(regionPuget, id), ""), http.StatusOK)
	if !boolean(t, aud, "forced_test") {
		t.Errorf("forced_test = false for a test alert")
	}
}

// pushRoutePatterns is the §2.9 route set, spelled here rather than derived
// from adminRoutes so the table cannot silently lose one and still pass.
var pushRoutePatterns = []string{
	"POST /api/admin/v1/regions/{regionId}/alerts/{id}/pushes",
	"GET /api/admin/v1/regions/{regionId}/alerts/{id}/pushes",
	"DELETE /api/admin/v1/regions/{regionId}/alerts/{id}/pushes/{pushId}",
	"GET /api/admin/v1/regions/{regionId}/alerts/{id}/push_audience",
}

// TestAdminPushRoutesRequireAPrincipalAndAreAbsentWithoutWaker is the wiring
// half of §2.9. The waker is the dispatcher, and main supplies it only when a
// transport is configured, so it doubles as the "a push can actually be sent"
// signal: without it the routes must not exist at all, rather than accepting
// a push that could only ever sit queued. With it, every one of the four is a
// normal principal-required, cross-site-guarded admin route.
func TestAdminPushRoutesRequireAPrincipalAndAreAbsentWithoutWaker(t *testing.T) {
	t.Parallel()

	t.Run("absent without a waker", func(t *testing.T) {
		t.Parallel()
		assertPushRoutesAbsentWithoutWaker(t)
	})

	t.Run("listed and principal-required when fully wired", func(t *testing.T) {
		t.Parallel()
		assertPushRoutesListedAndPrincipalRequired(t)
	})
}

// assertPushRoutesAbsentWithoutWaker pins that a sidecar with no dispatcher
// neither lists nor serves the push routes: a hand-written request must not
// be able to queue a push nothing would ever send.
func assertPushRoutesAbsentWithoutWaker(t *testing.T) {
	t.Helper()
	f := newAdminFixtureWithDeps(t, func(d *Deps) { d.AlertPushWaker = nil })

	wanted := pushRoutePatternSet()
	for _, pattern := range adminRoutes(f.deps) {
		if wanted[pattern.pattern] {
			t.Errorf("route %q is in the table with no waker configured", pattern.pattern)
		}
	}
	// Not merely unlisted: not reachable either, even with a valid session.
	for _, pattern := range pushRoutePatterns {
		method, target := concreteRoute(t, pattern)
		if res := f.do(method, target, "{}"); res.Code != http.StatusNotFound {
			t.Errorf("%s %s with no waker: status = %d, want 404 (%s)", method, target, res.Code, res.Body)
		}
	}
}

// assertPushRoutesListedAndPrincipalRequired pins membership only. What each
// route then *does* about a missing principal and a cross-site write is
// proven for all eighteen routes by TestAdminRoutes_EveryRouteRequiresAPrincipal
// and TestAdminRoutes_EveryWriteIsCrossSiteGuarded, which walk this same
// table; the one thing those sweeps cannot notice is a route quietly dropping
// out of it, which is what this half pins.
func assertPushRoutesListedAndPrincipalRequired(t *testing.T) {
	t.Helper()
	f := newAdminFixture(t)

	wanted := pushRoutePatternSet()
	listed := map[string]bool{}
	for _, rt := range adminRoutes(f.deps) {
		if !wanted[rt.pattern] {
			continue
		}
		listed[rt.pattern] = true
		if rt.allowed == nil {
			t.Errorf("route %q: allowed = nil, want a principal requirement", rt.pattern)
		}
	}
	for _, want := range pushRoutePatterns {
		if !listed[want] {
			t.Errorf("route %q is missing from the table with both deps set", want)
		}
	}
}

// pushRoutePatternSet indexes pushRoutePatterns for membership tests.
func pushRoutePatternSet() map[string]bool {
	set := make(map[string]bool, len(pushRoutePatterns))
	for _, p := range pushRoutePatterns {
		set[p] = true
	}
	return set
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
		{"create", http.MethodPost, pushesPath(regionPuget, id), `{}`},
		{"list", http.MethodGet, pushesPath(regionPuget, id), ""},
		{"cancel", http.MethodDelete, pushesPath(regionPuget, id) + "/1", ""},
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

// TestAdminPushes_PushScopedKeyCanSendAndCancel is the OBACloud send path
// end to end: a push-scoped region key queues and cancels a push for its
// own region, an unscoped key is refused with 403 on both, and the scope
// widens nothing else -- key management stays 403 and another region stays
// 404.
func TestAdminPushes_PushScopedKeyCanSendAndCancel(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	id := f.seedPublishedAlert(t, regionPuget, false)
	f.seedRegistration(t, regionPuget, "tok-1", false)
	pushKey := "Bearer " + f.mintRegionKeyWithScopes(t, regionPuget, apikey.Scopes{apikey.ScopePush})
	plainKey := "Bearer " + f.mintRegionKey(t, regionPuget)

	if rec := sendBearer(f.handler, http.MethodPost, pushesPath(regionPuget, id), `{"audience":"all"}`, plainKey); rec.Code != http.StatusForbidden {
		t.Errorf("unscoped key POST pushes: status = %d, want 403", rec.Code)
	}
	got := object(t, sendBearer(f.handler, http.MethodPost, pushesPath(regionPuget, id), `{"audience":"all"}`, pushKey), http.StatusAccepted)
	pushID := jsonID(t, got)

	cancelPath := fmt.Sprintf("%s/%d", pushesPath(regionPuget, id), pushID)
	if rec := sendBearer(f.handler, http.MethodDelete, cancelPath, "", plainKey); rec.Code != http.StatusForbidden {
		t.Errorf("unscoped key DELETE push: status = %d, want 403", rec.Code)
	}
	if rec := sendBearer(f.handler, http.MethodDelete, cancelPath, "", pushKey); rec.Code != http.StatusNoContent {
		t.Errorf("push-scoped key DELETE push: status = %d, want 204; body = %s", rec.Code, rec.Body.String())
	}

	if rec := sendBearer(f.handler, http.MethodPost, "/api/admin/v1/regions/1/api_keys", `{"name":"x"}`, pushKey); rec.Code != http.StatusForbidden {
		t.Errorf("push-scoped key on key management: status = %d, want 403", rec.Code)
	}
	if rec := sendBearer(f.handler, http.MethodPost, pushesPath(regionTampa, id), `{}`, pushKey); rec.Code != http.StatusNotFound {
		t.Errorf("push-scoped key on another region: status = %d, want 404", rec.Code)
	}
}
