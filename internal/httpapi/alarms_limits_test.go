package httpapi_test

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/alarms"
	"github.com/OneBusAway/sidecar/internal/httpapi"
	"github.com/OneBusAway/sidecar/internal/ratelimit"
	"github.com/OneBusAway/sidecar/internal/store/sqlitetest"
)

// newLimitedAlarmsServer is newAlarmsTestServer with a caller-chosen alarm
// limiter (nil = the router's default).
func newLimitedAlarmsServer(t *testing.T, limiter *ratelimit.Limiter) (http.Handler, alarms.Repository) {
	t.Helper()
	store := sqlitetest.Open(t)
	putRegionWithBaseURL(t, store.Regions(), 1, "https://sidecar.example.org")
	h := httpapi.NewRouter(httpapi.Deps{
		Alarms:       store.Alarms(),
		PushRegs:     store.PushRegs(),
		Regions:      store.Regions(),
		OBA:          &fakeAlarmsOBA{},
		AlarmLimiter: limiter,
		Now:          func() time.Time { return base },
		Logger:       slog.New(slog.DiscardHandler),
	})
	return h, store.Alarms()
}

// todayMs is a service date inside the alarm age window relative to base.
var todayMs = base.UnixMilli()

func alarmForm(fields map[string]string) string {
	v := url.Values{
		"user_push_id": {"push1"}, "operating_system": {"ios"}, "stop_id": {"1_100"},
		"trip_id": {"1_200"}, "service_date": {fmt.Sprint(todayMs)}, "seconds_before": {"600"},
	}
	for k, val := range fields {
		v.Set(k, val)
	}
	return v.Encode()
}

func countAlarms(t *testing.T, repo alarms.Repository) int {
	t.Helper()
	all, err := repo.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	return len(all)
}

// TestCreateAlarm_ThrottledPerIP (SC-01): both create routes share one
// per-source bucket; the request past it is a 429 and inserts nothing.
func TestCreateAlarm_ThrottledPerIP(t *testing.T) {
	t.Parallel()
	h, repo := newLimitedAlarmsServer(t, ratelimit.New(2, time.Minute))
	for i, path := range []string{"/api/v2/regions/1/alarms", "/api/v1/regions/1/alarms"} {
		body := alarmForm(map[string]string{"trip_id": fmt.Sprintf("1_%d", i)})
		if rec := alarmRequest(t, h, http.MethodPost, path, formCT, body); rec.Code != http.StatusCreated {
			t.Fatalf("request %d: status = %d, want 201; body = %s", i, rec.Code, rec.Body.String())
		}
	}
	rec := alarmRequest(t, h, http.MethodPost, "/api/v2/regions/1/alarms", formCT, alarmForm(map[string]string{"trip_id": "1_9"}))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("third request: status = %d, want 429", rec.Code)
	}
	if n := countAlarms(t, repo); n != 2 {
		t.Fatalf("alarms stored = %d, want 2 (the throttled one must not insert)", n)
	}
}

// TestCreateAlarm_DefaultLimiterAdmitsNormalUse: the router's default
// leaves room for many riders behind one carrier-grade NAT address.
func TestCreateAlarm_DefaultLimiterAdmitsNormalUse(t *testing.T) {
	t.Parallel()
	h, _ := newLimitedAlarmsServer(t, nil)
	for i := range 30 {
		body := alarmForm(map[string]string{"user_push_id": fmt.Sprintf("push-%d", i)})
		if rec := alarmRequest(t, h, http.MethodPost, "/api/v2/regions/1/alarms", formCT, body); rec.Code != http.StatusCreated {
			t.Fatalf("request %d: status = %d, want 201", i, rec.Code)
		}
	}
}

// TestCreateAlarm_FieldLengthCaps (SC-01): the identifiers used to be
// bounded only by the 64 KB body cap.
func TestCreateAlarm_FieldLengthCaps(t *testing.T) {
	t.Parallel()
	long := func(n int) string { return strings.Repeat("x", n) }
	for _, tc := range []struct {
		field string
		value string
		want  string
	}{
		{"user_push_id", long(4097), "Push identifier is too long (maximum is 4096 characters)"},
		{"stop_id", long(10 * 1024), "Stop is too long (maximum is 255 characters)"},
		{"trip_id", long(256), "Trip is too long (maximum is 255 characters)"},
		{"vehicle_id", long(256), "Vehicle is too long (maximum is 255 characters)"},
	} {
		t.Run(tc.field, func(t *testing.T) {
			t.Parallel()
			h, repo := newLimitedAlarmsServer(t, nil)
			for _, path := range []string{"/api/v2/regions/1/alarms", "/api/v1/regions/1/alarms"} {
				rec := alarmRequest(t, h, http.MethodPost, path, formCT, alarmForm(map[string]string{tc.field: tc.value}))
				if rec.Code != http.StatusUnprocessableEntity {
					t.Fatalf("%s: status = %d, want 422; body = %s", path, rec.Code, rec.Body.String())
				}
				if eb := decodeErrBody(t, rec); len(eb.Messages) != 1 || eb.Messages[0] != tc.want {
					t.Errorf("%s: messages = %v, want [%q]", path, eb.Messages, tc.want)
				}
			}
			if n := countAlarms(t, repo); n != 0 {
				t.Fatalf("alarms stored = %d, want 0", n)
			}
		})
	}
	t.Run("at the limit", func(t *testing.T) {
		t.Parallel()
		h, _ := newLimitedAlarmsServer(t, nil)
		body := alarmForm(map[string]string{"user_push_id": long(4096), "stop_id": long(255), "trip_id": long(255), "vehicle_id": long(255)})
		if rec := alarmRequest(t, h, http.MethodPost, "/api/v2/regions/1/alarms", formCT, body); rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201; body = %s", rec.Code, rec.Body.String())
		}
	})
}

// TestCreateAlarm_SecondsBeforeClamped (SC-01): a huge lead time is clamped,
// not rejected -- an app never sends one, and a 2xx keeps any odd client
// working.
func TestCreateAlarm_SecondsBeforeClamped(t *testing.T) {
	t.Parallel()
	h, repo := newLimitedAlarmsServer(t, nil)
	rec := alarmRequest(t, h, http.MethodPost, "/api/v2/regions/1/alarms", formCT, alarmForm(map[string]string{"seconds_before": "1000000000"}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", rec.Code, rec.Body.String())
	}
	a := getAlarmByToken(t, repo, 1, tokenFromURL(t, decodeCreateBody(t, rec).URL))
	if a.SecondsBefore != alarms.MaxSecondsBefore {
		t.Fatalf("SecondsBefore = %d, want %d", a.SecondsBefore, alarms.MaxSecondsBefore)
	}
}

// TestCreateAlarm_ServiceDateOutsideWindow (SC-01): an alarm for a service
// day far from today can never fire usefully and would otherwise sit in the
// table until the reaper caught it.
func TestCreateAlarm_ServiceDateOutsideWindow(t *testing.T) {
	t.Parallel()
	h, repo := newLimitedAlarmsServer(t, nil)
	for _, sd := range []int64{base.Add(-72 * time.Hour).UnixMilli(), base.Add(72 * time.Hour).UnixMilli(), base.Unix()} {
		rec := alarmRequest(t, h, http.MethodPost, "/api/v2/regions/1/alarms", formCT, alarmForm(map[string]string{"service_date": fmt.Sprint(sd)}))
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("service_date %d: status = %d, want 422", sd, rec.Code)
		}
		if eb := decodeErrBody(t, rec); len(eb.Messages) != 1 || eb.Messages[0] != "Service date is out of range" {
			t.Errorf("messages = %v", eb.Messages)
		}
	}
	if n := countAlarms(t, repo); n != 0 {
		t.Fatalf("alarms stored = %d, want 0", n)
	}
}

// TestCreateAlarm_PerDeviceCap (SC-01): one push token cannot accumulate an
// unbounded number of live alarms; a V1 idempotent re-POST of an existing
// alarm still succeeds at the cap.
func TestCreateAlarm_PerDeviceCap(t *testing.T) {
	t.Parallel()
	h, repo := newLimitedAlarmsServer(t, ratelimit.New(1000, time.Minute))
	for i := range httpapi.MaxAlarmsPerDevice {
		path := "/api/v2/regions/1/alarms"
		if i == 0 {
			path = "/api/v1/regions/1/alarms"
		}
		body := alarmForm(map[string]string{"trip_id": fmt.Sprintf("1_%d", i)})
		if rec := alarmRequest(t, h, http.MethodPost, path, formCT, body); rec.Code != http.StatusCreated {
			t.Fatalf("alarm %d: status = %d, want 201; body = %s", i, rec.Code, rec.Body.String())
		}
	}
	rec := alarmRequest(t, h, http.MethodPost, "/api/v2/regions/1/alarms", formCT, alarmForm(map[string]string{"trip_id": "1_over"}))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("over the cap: status = %d, want 429; body = %s", rec.Code, rec.Body.String())
	}
	rec = alarmRequest(t, h, http.MethodPost, "/api/v1/regions/1/alarms", formCT, alarmForm(map[string]string{"trip_id": "1_0"}))
	if rec.Code != http.StatusCreated {
		t.Fatalf("V1 re-POST at the cap: status = %d, want 201", rec.Code)
	}
	other := alarmForm(map[string]string{"user_push_id": "another-device"})
	if rec := alarmRequest(t, h, http.MethodPost, "/api/v2/regions/1/alarms", formCT, other); rec.Code != http.StatusCreated {
		t.Fatalf("another device: status = %d, want 201", rec.Code)
	}
	if n := countAlarms(t, repo); n != httpapi.MaxAlarmsPerDevice+1 {
		t.Fatalf("alarms stored = %d, want %d", n, httpapi.MaxAlarmsPerDevice+1)
	}
}
