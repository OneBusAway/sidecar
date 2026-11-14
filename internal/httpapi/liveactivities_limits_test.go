package httpapi_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/httpapi"
	"github.com/OneBusAway/sidecar/internal/liveactivities"
	"github.com/OneBusAway/sidecar/internal/ratelimit"
	"github.com/OneBusAway/sidecar/internal/store/sqlitetest"
)

func newCappedLiveActivitiesServer(t *testing.T, caps liveactivities.Caps) (http.Handler, liveactivities.Repository) {
	t.Helper()
	store := sqlitetest.Open(t)
	putRegionWithBaseURL(t, store.Regions(), 1, "https://sidecar.example")
	putRegionWithBaseURL(t, store.Regions(), 2, "https://sidecar.example")
	h := httpapi.NewRouter(httpapi.Deps{
		LiveActivities:      store.LiveActivities(),
		LiveActivityLimiter: ratelimit.New(1000, time.Minute),
		LiveActivityCaps:    caps,
		Regions:             store.Regions(),
		Now:                 func() time.Time { return base },
		Logger:              slog.New(slog.DiscardHandler),
	})
	return h, store.LiveActivities()
}

func laBody(fields map[string]string) string {
	v := url.Values{
		"activity_id": {"8D3B5E0A-6C1F-4F6B-9C7E-2D1A0B3C4D5E"}, "push_token": {"ptok-1"}, "stop_id": {"1_570"},
		"route_short_name": {"44"}, "trip_headsign": {"Ballard"},
	}
	for k, val := range fields {
		v.Set(k, val)
	}
	return v.Encode()
}

func laPost(t *testing.T, h http.Handler, region string, fields map[string]string) int {
	t.Helper()
	return alarmRequest(t, h, http.MethodPost, "/api/v2/regions/"+region+"/live_activities", formCT, laBody(fields)).Code
}

func laCount(t *testing.T, repo liveactivities.Repository) int {
	t.Helper()
	list, err := repo.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return len(list)
}

// TestLiveActivityFieldLengthCaps (SC-03, SC-10): every rider-supplied text
// field is capped; activity_id at 255 bytes. Its shape is NOT enforced:
// ActivityKit documents Activity.id only as an opaque String and the iOS
// app forwards it verbatim.
func TestLiveActivityFieldLengthCaps(t *testing.T) {
	t.Parallel()
	long := func(n int) string { return strings.Repeat("x", n) }
	for _, tc := range []struct{ field, want string }{
		{"activity_id", "Activity is too long (maximum is 255 characters)"},
		{"stop_id", "Stop is too long (maximum is 255 characters)"},
		{"route_short_name", "Route short name is too long (maximum is 255 characters)"},
		{"trip_headsign", "Trip headsign is too long (maximum is 255 characters)"},
		{"trip_id", "Trip is too long (maximum is 255 characters)"},
		{"vehicle_id", "Vehicle is too long (maximum is 255 characters)"},
	} {
		t.Run(tc.field, func(t *testing.T) {
			t.Parallel()
			h, repo := newCappedLiveActivitiesServer(t, liveactivities.Caps{})
			n := 256
			if tc.field == "stop_id" {
				n = 10 * 1024
			}
			rec := alarmRequest(t, h, http.MethodPost, "/api/v2/regions/1/live_activities", formCT, laBody(map[string]string{tc.field: long(n)}))
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422; body = %s", rec.Code, rec.Body.String())
			}
			if eb := decodeErrBody(t, rec); len(eb.Messages) != 1 || eb.Messages[0] != tc.want {
				t.Errorf("messages = %v, want [%q]", eb.Messages, tc.want)
			}
			if laCount(t, repo) != 0 {
				t.Fatal("a rejected registration was stored")
			}
		})
	}
	t.Run("non-UUID activity id within the cap is accepted", func(t *testing.T) {
		t.Parallel()
		h, _ := newCappedLiveActivitiesServer(t, liveactivities.Caps{})
		if code := laPost(t, h, "1", map[string]string{"activity_id": "activity-123"}); code != http.StatusCreated {
			t.Fatalf("status = %d, want 201", code)
		}
		if code := laPost(t, h, "1", map[string]string{"activity_id": long(255), "push_token": "p2"}); code != http.StatusCreated {
			t.Fatalf("255-byte activity id: status = %d, want 201", code)
		}
	})
}

// TestLiveActivityPerPushTokenCap (SC-03): a push token cannot fan out into
// unbounded subscriptions; re-registering an existing activity always works.
func TestLiveActivityPerPushTokenCap(t *testing.T) {
	t.Parallel()
	h, repo := newCappedLiveActivitiesServer(t, liveactivities.Caps{PerPushToken: 2})
	for _, act := range []string{"a1", "a2"} {
		if code := laPost(t, h, "1", map[string]string{"activity_id": act}); code != http.StatusCreated {
			t.Fatalf("%s: status = %d", act, code)
		}
	}
	if code := laPost(t, h, "1", map[string]string{"activity_id": "a3"}); code != http.StatusTooManyRequests {
		t.Fatalf("third activity on one push token: status = %d, want 429", code)
	}
	if code := laPost(t, h, "1", map[string]string{"activity_id": "a1", "trip_headsign": "Downtown"}); code != http.StatusCreated {
		t.Fatalf("re-registration at the cap: status = %d, want 201", code)
	}
	if n := laCount(t, repo); n != 2 {
		t.Fatalf("rows = %d, want 2", n)
	}
}

// TestLiveActivityCapacityCaps (SC-03): region rows, table rows and distinct
// stops per region are bounded; over them a NEW registration is a 503.
func TestLiveActivityCapacityCaps(t *testing.T) {
	t.Parallel()
	t.Run("per region", func(t *testing.T) {
		t.Parallel()
		h, _ := newCappedLiveActivitiesServer(t, liveactivities.Caps{PerRegion: 2})
		laPost(t, h, "1", map[string]string{"activity_id": "a1", "push_token": "p1"})
		laPost(t, h, "1", map[string]string{"activity_id": "a2", "push_token": "p2"})
		if code := laPost(t, h, "1", map[string]string{"activity_id": "a3", "push_token": "p3"}); code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", code)
		}
		if code := laPost(t, h, "2", map[string]string{"activity_id": "a3", "push_token": "p3"}); code != http.StatusCreated {
			t.Fatalf("another region: status = %d, want 201", code)
		}
		if code := laPost(t, h, "1", map[string]string{"activity_id": "a1", "push_token": "p1-rotated"}); code != http.StatusCreated {
			t.Fatalf("token rotation at the cap: status = %d, want 201", code)
		}
	})
	t.Run("table-wide", func(t *testing.T) {
		t.Parallel()
		h, _ := newCappedLiveActivitiesServer(t, liveactivities.Caps{Total: 2})
		laPost(t, h, "1", map[string]string{"activity_id": "a1", "push_token": "p1"})
		laPost(t, h, "2", map[string]string{"activity_id": "a2", "push_token": "p2"})
		if code := laPost(t, h, "1", map[string]string{"activity_id": "a3", "push_token": "p3"}); code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", code)
		}
	})
	t.Run("distinct stops per region", func(t *testing.T) {
		t.Parallel()
		h, repo := newCappedLiveActivitiesServer(t, liveactivities.Caps{StopsPerRegion: 1})
		laPost(t, h, "1", map[string]string{"activity_id": "a1", "push_token": "p1"})
		if code := laPost(t, h, "1", map[string]string{"activity_id": "a2", "push_token": "p2"}); code != http.StatusCreated {
			t.Fatalf("same stop: status = %d, want 201", code)
		}
		if code := laPost(t, h, "1", map[string]string{"activity_id": "a3", "push_token": "p3", "stop_id": "1_999"}); code != http.StatusServiceUnavailable {
			t.Fatalf("new stop: status = %d, want 503", code)
		}
		if n := laCount(t, repo); n != 2 {
			t.Fatalf("rows = %d, want 2", n)
		}
	})
}
