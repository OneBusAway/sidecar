package httpapi_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/httpapi"
	"github.com/OneBusAway/sidecar/internal/liveactivities"
	"github.com/OneBusAway/sidecar/internal/ratelimit"
	"github.com/OneBusAway/sidecar/internal/regions"
	"github.com/OneBusAway/sidecar/internal/store/sqlitetest"
)

func newLiveActivitiesServer(t *testing.T, limiter *ratelimit.Limiter) (http.Handler, liveactivities.Repository, regions.Repository) {
	t.Helper()
	store := sqlitetest.Open(t)
	deps := httpapi.Deps{
		LiveActivities:      store.LiveActivities(),
		LiveActivityLimiter: limiter,
		Regions:             store.Regions(),
		Now:                 func() time.Time { return base },
		Logger:              slog.New(slog.DiscardHandler),
	}
	return httpapi.NewRouter(deps), store.LiveActivities(), store.Regions()
}

const laForm = "activity_id=act-1&push_token=ptok-1&stop_id=1_570&route_short_name=44&trip_headsign=Ballard&apns_sandbox=1&trip_id=1_604370&service_date=1754809200000&stop_sequence=0"

var laURLRe = regexp.MustCompile(`^https://sidecar\.example/api/v2/regions/1/live_activities/([A-Za-z0-9_-]{22})$`)

func createURL(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if !laURLRe.MatchString(body["url"]) {
		t.Fatalf("url = %q", body["url"])
	}
	return body["url"]
}

func TestLiveActivityCreateFormAndJSON(t *testing.T) {
	h, repo, regionRepo := newLiveActivitiesServer(t, nil)
	putRegionWithBaseURL(t, regionRepo, 1, "https://sidecar.example")

	url := createURL(t, alarmRequest(t, h, http.MethodPost, "/api/v2/regions/1/live_activities",
		"application/x-www-form-urlencoded", laForm))
	list, _ := repo.List(context.Background())
	if len(list) != 1 {
		t.Fatalf("rows = %d", len(list))
	}
	la := list[0]
	if la.ActivityID != "act-1" || la.PushToken != "ptok-1" || !la.APNSSandbox || la.StopID != "1_570" ||
		la.RouteShortName != "44" || la.TripHeadsign != "Ballard" || la.TripID != "1_604370" ||
		la.ServiceDate != 1754809200000 || la.StopSequence == nil || *la.StopSequence != 0 ||
		!la.ExpiresAt.Equal(base.Add(liveactivities.Lifetime)) || !strings.HasSuffix(url, la.Token) {
		t.Errorf("stored = %+v (url %s)", la, url)
	}

	jsonBody := `{"activity_id":"act-2","push_token":"ptok-2","stop_id":"1_570","route_short_name":"44","trip_headsign":"Ballard","apns_sandbox":"true","stop_sequence":3}`
	createURL(t, alarmRequest(t, h, http.MethodPost, "/api/v2/regions/1/live_activities", "application/json", jsonBody))
	list, _ = repo.List(context.Background())
	if len(list) != 2 {
		t.Errorf("rows = %d", len(list))
	}
}

func TestLiveActivityRePostIsUpsertWithSameURL(t *testing.T) {
	h, repo, regionRepo := newLiveActivitiesServer(t, nil)
	putRegionWithBaseURL(t, regionRepo, 1, "https://sidecar.example")
	first := createURL(t, alarmRequest(t, h, http.MethodPost, "/api/v2/regions/1/live_activities",
		"application/x-www-form-urlencoded", laForm))
	rotated := strings.Replace(laForm, "push_token=ptok-1", "push_token=ptok-rotated", 1)
	rotated = strings.Replace(rotated, "apns_sandbox=1", "apns_sandbox=0", 1)
	second := createURL(t, alarmRequest(t, h, http.MethodPost, "/api/v2/regions/1/live_activities",
		"application/x-www-form-urlencoded", rotated))
	if first != second {
		t.Errorf("re-registration URL changed: %s -> %s", first, second)
	}
	list, _ := repo.List(context.Background())
	if len(list) != 1 || list[0].PushToken != "ptok-rotated" || list[0].APNSSandbox {
		t.Errorf("upsert did not rewrite token/sandbox: %+v", list)
	}
}

func TestLiveActivityCreateValidation(t *testing.T) {
	h, _, regionRepo := newLiveActivitiesServer(t, nil)
	putRegionWithBaseURL(t, regionRepo, 1, "https://sidecar.example")
	rec := alarmRequest(t, h, http.MethodPost, "/api/v2/regions/1/live_activities", "application/x-www-form-urlencoded", "")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d", rec.Code)
	}
	var body struct {
		Error    string   `json:"error"`
		Messages []string `json:"messages"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	want := []string{"Activity can't be blank", "Push token can't be blank", "Stop can't be blank",
		"Route short name can't be blank", "Trip headsign can't be blank"}
	if body.Error != "Unable to register live activity" || strings.Join(body.Messages, "|") != strings.Join(want, "|") {
		t.Errorf("body = %+v", body)
	}
	long := strings.Replace(laForm, "push_token=ptok-1", "push_token="+strings.Repeat("x", 4097), 1)
	rec = alarmRequest(t, h, http.MethodPost, "/api/v2/regions/1/live_activities", "application/x-www-form-urlencoded", long)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "Push token is too long (maximum is 4096 characters)") {
		t.Errorf("long token: %d %s", rec.Code, rec.Body.String())
	}
	rec = alarmRequest(t, h, http.MethodPost, "/api/v2/regions/99/live_activities", "application/x-www-form-urlencoded", laForm)
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "Couldn't find Region") {
		t.Errorf("unknown region: %d %s", rec.Code, rec.Body.String())
	}
}

func TestLiveActivityDelete(t *testing.T) {
	h, _, regionRepo := newLiveActivitiesServer(t, nil)
	putRegionWithBaseURL(t, regionRepo, 1, "https://sidecar.example")
	putRegionWithBaseURL(t, regionRepo, 2, "https://sidecar.example")
	url := createURL(t, alarmRequest(t, h, http.MethodPost, "/api/v2/regions/1/live_activities",
		"application/x-www-form-urlencoded", laForm))
	path := strings.TrimPrefix(url, "https://sidecar.example")
	token := path[strings.LastIndex(path, "/")+1:]

	if rec := alarmRequest(t, h, http.MethodDelete, "/api/v2/regions/2/live_activities/"+token, "", ""); rec.Code != http.StatusNotFound {
		t.Errorf("wrong region: %d", rec.Code)
	}
	slug := "/api/v2/regions/1-puget-sound/live_activities/" + token
	if rec := alarmRequest(t, h, http.MethodDelete, slug, "", ""); rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
		t.Errorf("slug delete: %d %q", rec.Code, rec.Body.String())
	}
	if rec := alarmRequest(t, h, http.MethodDelete, path, "", ""); rec.Code != http.StatusNotFound {
		t.Errorf("second delete: %d", rec.Code)
	}
}

func TestLiveActivityPostThrottledDeleteNot(t *testing.T) {
	h, _, regionRepo := newLiveActivitiesServer(t, ratelimit.New(1, time.Minute))
	putRegionWithBaseURL(t, regionRepo, 1, "https://sidecar.example")
	url := createURL(t, alarmRequest(t, h, http.MethodPost, "/api/v2/regions/1/live_activities",
		"application/x-www-form-urlencoded", laForm))
	if rec := alarmRequest(t, h, http.MethodPost, "/api/v2/regions/1/live_activities",
		"application/x-www-form-urlencoded", laForm); rec.Code != http.StatusTooManyRequests {
		t.Errorf("second POST: %d, want 429", rec.Code)
	}
	if rec := alarmRequest(t, h, http.MethodDelete, strings.TrimPrefix(url, "https://sidecar.example"), "", ""); rec.Code != http.StatusNoContent {
		t.Errorf("DELETE must not be throttled: %d", rec.Code)
	}
}

func TestLiveActivityURLFallsBackToRequestHost(t *testing.T) {
	h, _, regionRepo := newLiveActivitiesServer(t, nil)
	putRegionWithBaseURL(t, regionRepo, 1, "")
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "http://sidecar.local:8080/api/v2/regions/1/live_activities", strings.NewReader(laForm))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), `"url":"https://sidecar.local:8080/api/v2/regions/1/live_activities/`) {
		t.Errorf("body = %s", rec.Body.String())
	}
}

func TestNewRouterPanicsWhenLiveActivitiesLacksRegionsOrNow(t *testing.T) {
	defer func() {
		r := recover()
		if r == nil || !strings.Contains(r.(string), "Deps.Regions") || !strings.Contains(r.(string), "Deps.Now") {
			t.Errorf("recover = %v", r)
		}
	}()
	httpapi.NewRouter(httpapi.Deps{LiveActivities: sqlitetest.Open(t).LiveActivities(), Logger: slog.New(slog.DiscardHandler)})
}
