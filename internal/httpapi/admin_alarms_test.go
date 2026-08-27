package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/OneBusAway/sidecar/internal/alarms"
)

// seedAlarm writes an alarm straight to the store. These tests are about the
// admin read routes, so authoring goes through the repository rather than
// through the rider-facing v1/v2 endpoints, whose own tests already cover
// creation.
func (f *adminFixture) seedAlarm(t *testing.T, regionID int64, token, userPushID string) int64 {
	t.Helper()
	a, err := f.store.Alarms().Create(context.Background(), alarms.NewAlarm{
		RegionID: regionID, Token: token, APIVersion: 2, UserPushID: userPushID,
		OperatingSystem: "ios", StopID: "1_570", TripID: "1_604370",
		ServiceDate: 1754809200000, VehicleID: "1_4361",
		SecondsBefore: 600, Message: "The 44 to Ballard leaves in 10 minutes",
	}, testNow)
	if err != nil {
		t.Fatalf("seed alarm in region %d: %v", regionID, err)
	}
	return a.ID
}

// TestAdminAlarms_OmitsPushCredentials is the whole reason this route has a
// hand-written wire shape rather than marshalling alarms.Alarm: token and
// user_push_id are push credentials, not UI data, and a struct that grew a
// field by default would ship them.
func TestAdminAlarms_OmitsPushCredentials(t *testing.T) {
	t.Parallel()

	f := newFullAdminFixture(t)
	id := f.seedAlarm(t, regionPuget, "secret-token", "secret-user-push-id")

	for _, target := range []string{
		"/api/admin/v1/regions/1/alarms",
		fmt.Sprintf("/api/admin/v1/regions/1/alarms/%d", id),
	} {
		rec := f.do(http.MethodGet, target, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200; body = %s", target, rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		for _, secret := range []string{"secret-token", "secret-user-push-id", "\"token\"", "user_push_id"} {
			if strings.Contains(body, secret) {
				t.Errorf("%s leaked %q:\n%s", target, secret, body)
			}
		}
	}

	one := object(t, f.do(http.MethodGet, fmt.Sprintf("/api/admin/v1/regions/1/alarms/%d", id), ""), http.StatusOK)
	assertKeys(t, "alarm", one, []string{
		"id", "api_version", "operating_system", "stop_id", "trip_id", "service_date",
		"vehicle_id", "stop_sequence", "seconds_before", "message", "failure_count", "created_at",
	})
	// service_date is epoch milliseconds and passes through as an integer.
	if _, ok := one["service_date"].(float64); !ok {
		t.Errorf("service_date = %#v, want a JSON number", one["service_date"])
	}
}

// TestAdminAlarms_RegionScoped.
func TestAdminAlarms_RegionScoped(t *testing.T) {
	t.Parallel()

	f := newFullAdminFixture(t)
	id := f.seedAlarm(t, regionPuget, "t", "u")
	if rec := f.do(http.MethodGet, fmt.Sprintf("/api/admin/v1/regions/0/alarms/%d", id), ""); rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	if list := array(t, f.do(http.MethodGet, "/api/admin/v1/regions/0/alarms", ""), http.StatusOK); len(list) != 0 {
		t.Errorf("region 0 shows %d of region 1's alarms", len(list))
	}
}
