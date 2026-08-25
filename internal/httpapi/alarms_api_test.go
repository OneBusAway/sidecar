package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/alarms"
	"github.com/OneBusAway/sidecar/internal/httpapi"
	"github.com/OneBusAway/sidecar/internal/obaapi"
	"github.com/OneBusAway/sidecar/internal/pushreg"
	"github.com/OneBusAway/sidecar/internal/regions"
	"github.com/OneBusAway/sidecar/internal/store/sqlitetest"
)

// fakeAlarmsOBA is a configurable obaapi.Client stub for the alarm handler
// tests: a settable Departure/error plus a call counter, so
// TestCreateV2_MissingTripFieldsStillCreated can assert the handler never
// dials upstream for an unkeyable query rather than merely getting lucky
// with the fake's zero value.
type fakeAlarmsOBA struct {
	dep   obaapi.Departure
	err   error
	calls atomic.Int64
}

func (f *fakeAlarmsOBA) Fleet(context.Context, regions.Region) ([]obaapi.Vehicle, error) {
	panic("fakeAlarmsOBA.Fleet: unused by alarm tests")
}

func (f *fakeAlarmsOBA) ArrivalAndDeparture(context.Context, regions.Region, obaapi.DepartureQuery) (obaapi.Departure, error) {
	f.calls.Add(1)
	if f.err != nil {
		return obaapi.Departure{}, f.err
	}
	return f.dep, nil
}

func (f *fakeAlarmsOBA) TripDetails(context.Context, regions.Region, obaapi.TripDetailsQuery) (json.RawMessage, error) {
	panic("fakeAlarmsOBA.TripDetails: unused by alarm tests")
}

func (f *fakeAlarmsOBA) ArrivalsAndDeparturesForStop(context.Context, regions.Region, obaapi.StopArrivalsQuery) ([]obaapi.StopArrival, error) {
	panic("fakeAlarmsOBA.ArrivalsAndDeparturesForStop: unused by alarm tests")
}

// newAlarmsTestServer builds a router over a freshly migrated store with the
// alarm routes registered, plus the repositories tests seed fixtures and
// assert against directly.
func newAlarmsTestServer(t *testing.T, oba obaapi.Client) (http.Handler, alarms.Repository, pushreg.Repository, regions.Repository) {
	t.Helper()
	store := sqlitetest.Open(t)
	deps := httpapi.Deps{
		Alarms:   store.Alarms(),
		PushRegs: store.PushRegs(),
		Regions:  store.Regions(),
		OBA:      oba,
		Now:      func() time.Time { return base },
		Logger:   slog.New(slog.DiscardHandler),
	}
	return httpapi.NewRouter(deps), store.Alarms(), store.PushRegs(), store.Regions()
}

// putRegionWithBaseURL seeds a region carrying a SidecarBaseURL, which
// putRegion (alerts_test.go) never sets -- the alarm creation URL tests need
// one to assert the "region wins" half of alarmURL's fallback.
func putRegionWithBaseURL(t *testing.T, repo regions.Repository, id int64, sidecarBaseURL string) {
	t.Helper()
	if err := repo.UpsertFromDirectory(context.Background(), []regions.Region{{
		ID:             id,
		Name:           "Test Region",
		OBABaseURL:     "https://example.org/",
		SidecarBaseURL: sidecarBaseURL,
		Active:         true,
	}}, base); err != nil {
		t.Fatalf("UpsertFromDirectory(%d): %v", id, err)
	}
}

func alarmRequest(t *testing.T, h http.Handler, method, target, contentType, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), method, target, strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

type createBody struct {
	URL string `json:"url"`
}

func decodeCreateBody(t *testing.T, rec *httptest.ResponseRecorder) createBody {
	t.Helper()
	var cb createBody
	if err := json.Unmarshal(rec.Body.Bytes(), &cb); err != nil {
		t.Fatalf("decode create body: %v; body = %s", err, rec.Body.String())
	}
	return cb
}

// alarmTokenRe matches the trailing 22-character URL-safe base64 token
// securetoken.New produces.
var alarmTokenRe = regexp.MustCompile(`/alarms/([A-Za-z0-9_-]{22})$`)

func tokenFromURL(t *testing.T, url string) string {
	t.Helper()
	m := alarmTokenRe.FindStringSubmatch(url)
	if m == nil {
		t.Fatalf("url = %q, want it to end in /alarms/<22-char-token>", url)
	}
	return m[1]
}

func getAlarmByToken(t *testing.T, repo alarms.Repository, regionID int64, token string) alarms.Alarm {
	t.Helper()
	all, err := repo.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, a := range all {
		if a.RegionID == regionID && a.Token == token {
			return a
		}
	}
	t.Fatalf("no alarm found for region %d token %q", regionID, token)
	return alarms.Alarm{}
}

func TestCreateV2_ComposedMessage(t *testing.T) {
	t.Parallel()
	oba := &fakeAlarmsOBA{dep: obaapi.Departure{RouteShortName: "44", TripHeadsign: "Ballard"}}
	h, alarmRepo, _, regionRepo := newAlarmsTestServer(t, oba)
	putRegionWithBaseURL(t, regionRepo, 1, "https://sidecar.example.org")

	body := "user_push_id=push1&operating_system=ios&stop_id=1_100&trip_id=1_200&service_date=1700000000000&seconds_before=600"
	rec := alarmRequest(t, h, http.MethodPost, "/api/v2/regions/1/alarms", formCT, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", rec.Code, rec.Body.String())
	}

	cb := decodeCreateBody(t, rec)
	wantPrefix := "https://sidecar.example.org/api/v2/regions/1/alarms/"
	if !strings.HasPrefix(cb.URL, wantPrefix) {
		t.Fatalf("url = %q, want prefix %q", cb.URL, wantPrefix)
	}
	token := tokenFromURL(t, cb.URL)

	a := getAlarmByToken(t, alarmRepo, 1, token)
	if a.Message != "The 44 to Ballard leaves in 10 minutes" {
		t.Errorf("Message = %q, want %q", a.Message, "The 44 to Ballard leaves in 10 minutes")
	}
	if a.APIVersion != 2 {
		t.Errorf("APIVersion = %d, want 2", a.APIVersion)
	}
	if a.SecondsBefore != 600 {
		t.Errorf("SecondsBefore = %d, want 600", a.SecondsBefore)
	}
}

func TestCreateV2_JSONBody(t *testing.T) {
	t.Parallel()
	oba := &fakeAlarmsOBA{dep: obaapi.Departure{RouteShortName: "44", TripHeadsign: "Ballard"}}
	h, alarmRepo, _, regionRepo := newAlarmsTestServer(t, oba)
	putRegionWithBaseURL(t, regionRepo, 1, "https://sidecar.example.org")

	body := `{"user_push_id":"push1","operating_system":"ios","stop_id":"1_100","trip_id":"1_200",` +
		`"service_date":1700000000000,"stop_sequence":0,"seconds_before":600}`
	rec := alarmRequest(t, h, http.MethodPost, "/api/v2/regions/1/alarms", jsonCT, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", rec.Code, rec.Body.String())
	}

	cb := decodeCreateBody(t, rec)
	token := tokenFromURL(t, cb.URL)
	a := getAlarmByToken(t, alarmRepo, 1, token)
	if a.StopSequence == nil || *a.StopSequence != 0 {
		t.Errorf("StopSequence = %v, want ptr(0)", a.StopSequence)
	}
}

func TestCreateV2_LookupFailureDegradesToGeneric(t *testing.T) {
	t.Parallel()
	oba := &fakeAlarmsOBA{err: errors.New("upstream exploded")}
	h, alarmRepo, _, regionRepo := newAlarmsTestServer(t, oba)
	putRegionWithBaseURL(t, regionRepo, 1, "https://sidecar.example.org")

	body := "user_push_id=push1&operating_system=ios&stop_id=1_100&trip_id=1_200&service_date=1700000000000&seconds_before=600"
	rec := alarmRequest(t, h, http.MethodPost, "/api/v2/regions/1/alarms", formCT, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", rec.Code, rec.Body.String())
	}

	cb := decodeCreateBody(t, rec)
	token := tokenFromURL(t, cb.URL)
	a := getAlarmByToken(t, alarmRepo, 1, token)
	if a.Message != "The bus leaves in 10 minutes" {
		t.Errorf("Message = %q, want %q", a.Message, "The bus leaves in 10 minutes")
	}
}

func TestCreateV2_MissingTripFieldsStillCreated(t *testing.T) {
	t.Parallel()
	oba := &fakeAlarmsOBA{dep: obaapi.Departure{RouteShortName: "44", TripHeadsign: "Ballard"}}
	h, alarmRepo, _, regionRepo := newAlarmsTestServer(t, oba)
	putRegionWithBaseURL(t, regionRepo, 1, "https://sidecar.example.org")

	body := "user_push_id=push1&operating_system=ios"
	rec := alarmRequest(t, h, http.MethodPost, "/api/v2/regions/1/alarms", formCT, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", rec.Code, rec.Body.String())
	}

	cb := decodeCreateBody(t, rec)
	token := tokenFromURL(t, cb.URL)
	a := getAlarmByToken(t, alarmRepo, 1, token)
	if a.Message != "The bus leaves in 10 minutes" {
		t.Errorf("Message = %q, want %q", a.Message, "The bus leaves in 10 minutes")
	}
	if got := oba.calls.Load(); got != 0 {
		t.Errorf("OBA calls = %d, want 0 (missing trip fields must not dial upstream)", got)
	}
}

func TestCreateV2_SecondsBeforeDefaults(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
	}{
		{"zero", "user_push_id=push1&operating_system=ios&seconds_before=0"},
		{"negative", "user_push_id=push1&operating_system=ios&seconds_before=-3"},
		{"non-numeric", "user_push_id=push1&operating_system=ios&seconds_before=abc"},
		{"absent", "user_push_id=push1&operating_system=ios"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, alarmRepo, _, regionRepo := newAlarmsTestServer(t, &fakeAlarmsOBA{})
			putRegionWithBaseURL(t, regionRepo, 1, "https://sidecar.example.org")

			rec := alarmRequest(t, h, http.MethodPost, "/api/v2/regions/1/alarms", formCT, tt.body)
			if rec.Code != http.StatusCreated {
				t.Fatalf("status = %d, want 201; body = %s", rec.Code, rec.Body.String())
			}
			cb := decodeCreateBody(t, rec)
			token := tokenFromURL(t, cb.URL)
			a := getAlarmByToken(t, alarmRepo, 1, token)
			if a.SecondsBefore != 600 {
				t.Errorf("SecondsBefore = %d, want 600", a.SecondsBefore)
			}
		})
	}
}

func TestCreateV2_Validation(t *testing.T) {
	t.Parallel()
	h, _, _, regionRepo := newAlarmsTestServer(t, &fakeAlarmsOBA{})
	putRegionWithBaseURL(t, regionRepo, 1, "https://sidecar.example.org")

	tests := []struct {
		name string
		body string
		want string
	}{
		{"missing user_push_id", "operating_system=ios", "Push identifier can't be blank"},
		{"missing os", "user_push_id=push1", "Operating system can't be blank"},
		{"unknown os", "user_push_id=push1&operating_system=windows", "Operating system is not included in the list"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := alarmRequest(t, h, http.MethodPost, "/api/v2/regions/1/alarms", formCT, tt.body)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422; body = %s", rec.Code, rec.Body.String())
			}
			eb := decodeErrBody(t, rec)
			if eb.Error != "Unable to register alarm" {
				t.Errorf("Error = %q, want %q", eb.Error, "Unable to register alarm")
			}
			if len(eb.Messages) != 1 || eb.Messages[0] != tt.want {
				t.Errorf("Messages = %v, want [%q]", eb.Messages, tt.want)
			}
		})
	}
}

func TestCreateV2_SideEffectUpsertsPushRegistration(t *testing.T) {
	t.Parallel()
	h, _, pushRepo, regionRepo := newAlarmsTestServer(t, &fakeAlarmsOBA{})
	putRegionWithBaseURL(t, regionRepo, 1, "https://sidecar.example.org")

	body := "user_push_id=push1&operating_system=ios&apns_sandbox=true"
	rec := alarmRequest(t, h, http.MethodPost, "/api/v2/regions/1/alarms", formCT, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", rec.Code, rec.Body.String())
	}

	reg, err := pushRepo.Get(context.Background(), 1, "push1")
	if err != nil {
		t.Fatalf("Get(push1): %v", err)
	}
	if reg.OperatingSystem != pushreg.OSIOS {
		t.Errorf("OperatingSystem = %q, want %q", reg.OperatingSystem, pushreg.OSIOS)
	}
	if reg.APNSSandbox {
		t.Errorf("APNSSandbox = true, want false -- the alarm's sandbox=true must not propagate (spec §5.2 wart)")
	}
	if reg.Locale != "" {
		t.Errorf("Locale = %q, want empty", reg.Locale)
	}
}

func TestCreateV2_SideEffectRefreshesExisting(t *testing.T) {
	t.Parallel()
	h, _, pushRepo, regionRepo := newAlarmsTestServer(t, &fakeAlarmsOBA{})
	putRegionWithBaseURL(t, regionRepo, 1, "https://sidecar.example.org")

	// Seeded strictly before base -- the fixed instant the test server's
	// Deps.Now returns -- so a LastSeenAt of exactly base is unambiguous
	// proof the side-effect upsert ran, not an artifact of two writes
	// sharing one clock reading.
	earlier := base.Add(-time.Hour)
	locale := "es"
	if err := pushRepo.Upsert(context.Background(), pushreg.Upsert{
		RegionID: 1, Token: "push1", OperatingSystem: pushreg.OSIOS, Locale: &locale,
	}, earlier); err != nil {
		t.Fatalf("seed pushreg: %v", err)
	}

	rec := alarmRequest(t, h, http.MethodPost, "/api/v2/regions/1/alarms", formCT,
		"user_push_id=push1&operating_system=ios")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", rec.Code, rec.Body.String())
	}

	reg, err := pushRepo.Get(context.Background(), 1, "push1")
	if err != nil {
		t.Fatalf("Get(push1): %v", err)
	}
	if reg.Locale != "es" {
		t.Errorf("Locale = %q, want es (side-effect must not touch locale)", reg.Locale)
	}
	if !reg.LastSeenAt.Equal(base) {
		t.Errorf("LastSeenAt = %v, want %v (advanced by the side-effect)", reg.LastSeenAt, base)
	}
}

// TestCreateV2_SideEffectSkipsOversizedUserPushID asserts an oversized
// user_push_id (> maxTokenLen) still creates the alarm -- the side-effect
// registry upsert is not on the 201's critical path -- but the side-effect
// itself is skipped rather than reaching the store, since that upsert
// bypasses the registration endpoint's own length validation and throttle
// (spec §2.6).
func TestCreateV2_SideEffectSkipsOversizedUserPushID(t *testing.T) {
	t.Parallel()
	h, _, pushRepo, regionRepo := newAlarmsTestServer(t, &fakeAlarmsOBA{})
	putRegionWithBaseURL(t, regionRepo, 1, "https://sidecar.example.org")

	oversized := strings.Repeat("a", 4097)
	body := "user_push_id=" + oversized + "&operating_system=ios"
	rec := alarmRequest(t, h, http.MethodPost, "/api/v2/regions/1/alarms", formCT, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", rec.Code, rec.Body.String())
	}

	if _, err := pushRepo.Get(context.Background(), 1, oversized); !errors.Is(err, pushreg.ErrNotFound) {
		t.Errorf("PushRegs.Get(oversized user_push_id) = %v, want pushreg.ErrNotFound (side-effect must be skipped)", err)
	}
}

func TestCreateV1_DefaultsOSToIOS(t *testing.T) {
	t.Parallel()
	h, alarmRepo, _, regionRepo := newAlarmsTestServer(t, &fakeAlarmsOBA{})
	putRegionWithBaseURL(t, regionRepo, 1, "https://sidecar.example.org")

	rec := alarmRequest(t, h, http.MethodPost, "/api/v1/regions/1/alarms", formCT, "user_push_id=push1")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", rec.Code, rec.Body.String())
	}
	cb := decodeCreateBody(t, rec)
	token := tokenFromURL(t, cb.URL)
	a := getAlarmByToken(t, alarmRepo, 1, token)
	if a.OperatingSystem != pushreg.OSIOS {
		t.Errorf("OperatingSystem = %q, want ios", a.OperatingSystem)
	}

	rec = alarmRequest(t, h, http.MethodPost, "/api/v1/regions/1/alarms", formCT,
		"user_push_id=push2&operating_system=garbage")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", rec.Code, rec.Body.String())
	}
	cb = decodeCreateBody(t, rec)
	token = tokenFromURL(t, cb.URL)
	a = getAlarmByToken(t, alarmRepo, 1, token)
	if a.OperatingSystem != pushreg.OSIOS {
		t.Errorf("OperatingSystem = %q, want ios (invalid value treated as absent)", a.OperatingSystem)
	}
}

func TestCreateV1_Dedupe(t *testing.T) {
	t.Parallel()
	h, alarmRepo, _, regionRepo := newAlarmsTestServer(t, &fakeAlarmsOBA{})
	putRegionWithBaseURL(t, regionRepo, 1, "https://sidecar.example.org")

	body := "user_push_id=push1&operating_system=ios&trip_id=1_200&stop_id=1_100&service_date=1700000000000&seconds_before=600"
	rec1 := alarmRequest(t, h, http.MethodPost, "/api/v1/regions/1/alarms", formCT, body)
	if rec1.Code != http.StatusCreated {
		t.Fatalf("first: status = %d, want 201; body = %s", rec1.Code, rec1.Body.String())
	}
	url1 := decodeCreateBody(t, rec1).URL

	rec2 := alarmRequest(t, h, http.MethodPost, "/api/v1/regions/1/alarms", formCT, body)
	if rec2.Code != http.StatusCreated {
		t.Fatalf("second: status = %d, want 201; body = %s", rec2.Code, rec2.Body.String())
	}
	url2 := decodeCreateBody(t, rec2).URL
	if url1 != url2 {
		t.Errorf("url2 = %q, want same as url1 %q", url2, url1)
	}

	changedBody := "user_push_id=push1&operating_system=ios&trip_id=1_200&stop_id=1_100&service_date=1700000000000&seconds_before=120"
	rec3 := alarmRequest(t, h, http.MethodPost, "/api/v1/regions/1/alarms", formCT, changedBody)
	if rec3.Code != http.StatusCreated {
		t.Fatalf("third: status = %d, want 201; body = %s", rec3.Code, rec3.Body.String())
	}
	url3 := decodeCreateBody(t, rec3).URL
	if url3 != url1 {
		t.Errorf("url3 = %q, want same as url1 %q (dedupe hands back existing alarm)", url3, url1)
	}

	token := tokenFromURL(t, url1)
	a := getAlarmByToken(t, alarmRepo, 1, token)
	if a.SecondsBefore != 600 {
		t.Errorf("SecondsBefore = %d, want 600 (unchanged by the re-POST with seconds_before=120)", a.SecondsBefore)
	}

	all, err := alarmRepo.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	count := 0
	for _, x := range all {
		if x.RegionID == 1 && x.UserPushID == "push1" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("row count = %d, want exactly 1", count)
	}
}

func TestCreateV1_NoSideEffect(t *testing.T) {
	t.Parallel()
	h, _, pushRepo, regionRepo := newAlarmsTestServer(t, &fakeAlarmsOBA{})
	putRegionWithBaseURL(t, regionRepo, 1, "https://sidecar.example.org")

	rec := alarmRequest(t, h, http.MethodPost, "/api/v1/regions/1/alarms", formCT, "user_push_id=push1")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", rec.Code, rec.Body.String())
	}

	_, err := pushRepo.Get(context.Background(), 1, "push1")
	if !errors.Is(err, pushreg.ErrNotFound) {
		t.Errorf("Get(push1) err = %v, want ErrNotFound (V1 has no push registration side effect)", err)
	}
}

func TestCreateV1_NoSandbox(t *testing.T) {
	t.Parallel()
	h, alarmRepo, _, regionRepo := newAlarmsTestServer(t, &fakeAlarmsOBA{})
	putRegionWithBaseURL(t, regionRepo, 1, "https://sidecar.example.org")

	rec := alarmRequest(t, h, http.MethodPost, "/api/v1/regions/1/alarms", formCT,
		"user_push_id=push1&apns_sandbox=true")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", rec.Code, rec.Body.String())
	}
	cb := decodeCreateBody(t, rec)
	token := tokenFromURL(t, cb.URL)
	a := getAlarmByToken(t, alarmRepo, 1, token)
	if a.APNSSandbox {
		t.Errorf("APNSSandbox = true, want false (V1 ignores apns_sandbox)")
	}
}

func TestDelete(t *testing.T) {
	t.Parallel()
	h, _, _, regionRepo := newAlarmsTestServer(t, &fakeAlarmsOBA{})
	putRegionWithBaseURL(t, regionRepo, 1, "https://sidecar.example.org")

	rec := alarmRequest(t, h, http.MethodPost, "/api/v2/regions/1/alarms", formCT, "user_push_id=push1&operating_system=ios")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: status = %d, want 201; body = %s", rec.Code, rec.Body.String())
	}
	token := tokenFromURL(t, decodeCreateBody(t, rec).URL)

	del := alarmRequest(t, h, http.MethodDelete, "/api/v2/regions/1/alarms/"+token, "", "")
	if del.Code != http.StatusNoContent {
		t.Fatalf("delete: status = %d, want 204; body = %s", del.Code, del.Body.String())
	}
	if del.Body.Len() != 0 {
		t.Errorf("delete: body = %q, want empty", del.Body.String())
	}

	del2 := alarmRequest(t, h, http.MethodDelete, "/api/v2/regions/1/alarms/"+token, "", "")
	if del2.Code != http.StatusNotFound {
		t.Fatalf("re-delete: status = %d, want 404; body = %s", del2.Code, del2.Body.String())
	}
	if del2.Body.Len() != 0 {
		t.Errorf("re-delete: body = %q, want empty", del2.Body.String())
	}

	// Slug region path.
	rec2 := alarmRequest(t, h, http.MethodPost, "/api/v2/regions/1/alarms", formCT, "user_push_id=push2&operating_system=ios")
	token2 := tokenFromURL(t, decodeCreateBody(t, rec2).URL)
	delSlug := alarmRequest(t, h, http.MethodDelete, "/api/v2/regions/1-puget-sound/alarms/"+token2, "", "")
	if delSlug.Code != http.StatusNoContent {
		t.Fatalf("slug delete: status = %d, want 204; body = %s", delSlug.Code, delSlug.Body.String())
	}

	// V1 delete path removes a V2-created alarm; tokens are version-agnostic.
	rec3 := alarmRequest(t, h, http.MethodPost, "/api/v2/regions/1/alarms", formCT, "user_push_id=push3&operating_system=ios")
	token3 := tokenFromURL(t, decodeCreateBody(t, rec3).URL)
	delV1 := alarmRequest(t, h, http.MethodDelete, "/api/v1/regions/1/alarms/"+token3, "", "")
	if delV1.Code != http.StatusNoContent {
		t.Fatalf("v1 delete of v2 alarm: status = %d, want 204; body = %s", delV1.Code, delV1.Body.String())
	}
}

func TestCreate_UnknownRegion(t *testing.T) {
	t.Parallel()
	h, _, _, _ := newAlarmsTestServer(t, &fakeAlarmsOBA{})

	rec := alarmRequest(t, h, http.MethodPost, "/api/v2/regions/99/alarms", formCT, "user_push_id=push1&operating_system=ios")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != `{"error":"Couldn't find Region"}` {
		t.Errorf("body = %q, want the region-not-found contract", got)
	}
}

func TestCreate_FallbackURLWhenNoSidecarBaseURL(t *testing.T) {
	t.Parallel()
	h, _, _, regionRepo := newAlarmsTestServer(t, &fakeAlarmsOBA{})
	putRegion(t, regionRepo, 1) // no SidecarBaseURL set

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost,
		"/api/v2/regions/1/alarms", strings.NewReader("user_push_id=push1&operating_system=ios"))
	req.Header.Set("Content-Type", formCT)
	req.Host = "sidecar.local"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", rec.Code, rec.Body.String())
	}
	cb := decodeCreateBody(t, rec)
	if !strings.HasPrefix(cb.URL, "https://sidecar.local/") {
		t.Errorf("url = %q, want prefix %q", cb.URL, "https://sidecar.local/")
	}
}

// TestAlarmsRoutesRequireDeps documents the router's panic guard for the
// alarm routes: with Deps.Alarms set but PushRegs/Now/Regions missing,
// NewRouter must panic at construction rather than nil-deref on first
// request.
func TestAlarmsRoutesRequireDeps(t *testing.T) {
	t.Parallel()
	store := sqlitetest.Open(t)
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("NewRouter did not panic with Deps.Alarms set and PushRegs/Now/Regions missing")
		} else {
			msg := fmt.Sprint(r)
			for _, want := range []string{"PushRegs", "Now", "Regions"} {
				if !strings.Contains(msg, want) {
					t.Errorf("panic message %q missing %q", msg, want)
				}
			}
		}
	}()
	httpapi.NewRouter(httpapi.Deps{Alarms: store.Alarms()})
}

// erroringAlarmsRepo is an alarms.Repository stub whose Create and Delete
// always fail with an error that echoes back the secret(s) they were
// handed -- the worst case a store driver could produce, and the one the
// sanitizeToken calls in alarms.go exist for. Create's error embeds both
// user_push_id and the alarm token it was asked to store; Delete's embeds
// the path token. lastCreateToken captures the token Create actually
// received (it is minted inside the handler, so no test can know it in
// advance) so a test can assert the exact secret never reaches the log.
// The other five Repository methods are never exercised by the handler
// paths under test and panic if called, so an accidental extra call is
// caught immediately rather than silently returning a zero value.
type erroringAlarmsRepo struct {
	lastCreateToken *string
}

func (r erroringAlarmsRepo) Create(_ context.Context, in alarms.NewAlarm, _ time.Time) (alarms.Alarm, error) {
	if r.lastCreateToken != nil {
		*r.lastCreateToken = in.Token
	}
	return alarms.Alarm{}, fmt.Errorf("constraint failed: token=%s user_push_id=%s", in.Token, in.UserPushID)
}
func (erroringAlarmsRepo) FindV1(context.Context, alarms.V1Key) (alarms.Alarm, error) {
	return alarms.Alarm{}, alarms.ErrNotFound
}
func (erroringAlarmsRepo) Delete(_ context.Context, _ int64, token string) error {
	return fmt.Errorf("constraint failed: token=%s", token)
}
func (erroringAlarmsRepo) DeleteByID(context.Context, int64) error {
	panic("erroringAlarmsRepo.DeleteByID: unused by these tests")
}
func (erroringAlarmsRepo) List(context.Context) ([]alarms.Alarm, error) {
	panic("erroringAlarmsRepo.List: unused by these tests")
}
func (erroringAlarmsRepo) RecordFailure(context.Context, int64) (int64, error) {
	panic("erroringAlarmsRepo.RecordFailure: unused by these tests")
}
func (erroringAlarmsRepo) ResetFailures(context.Context, int64) error {
	panic("erroringAlarmsRepo.ResetFailures: unused by these tests")
}

// TestCreate_StoreErrorSanitizesUserPushIDAndToken covers the create-error
// log path: a failed Create's error can embed both user_push_id (the query
// key) and the freshly minted alarm token (a column value on the same
// failed row), and neither must reach the log raw.
func TestCreate_StoreErrorSanitizesUserPushIDAndToken(t *testing.T) {
	t.Parallel()
	store := sqlitetest.Open(t)
	putRegionWithBaseURL(t, store.Regions(), 1, "https://sidecar.example.org")

	var buf bytes.Buffer
	var mintedToken string
	deps := httpapi.Deps{
		Alarms:   erroringAlarmsRepo{lastCreateToken: &mintedToken},
		PushRegs: store.PushRegs(),
		Regions:  store.Regions(),
		OBA:      &fakeAlarmsOBA{},
		Now:      func() time.Time { return base },
		Logger:   slog.New(slog.NewTextHandler(&buf, nil)),
	}
	h := httpapi.NewRouter(deps)

	const userPushID = "supersecret-push-id-abc123"
	rec := alarmRequest(t, h, http.MethodPost, "/api/v2/regions/1/alarms", formCT,
		"user_push_id="+userPushID+"&operating_system=ios")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body = %s", rec.Code, rec.Body.String())
	}
	if mintedToken == "" {
		t.Fatal("erroringAlarmsRepo.Create was never called; test cannot capture the minted token")
	}

	logOutput := buf.String()
	if strings.Contains(logOutput, userPushID) {
		t.Errorf("log output contains the raw user_push_id: %s", logOutput)
	}
	if strings.Contains(logOutput, mintedToken) {
		t.Errorf("log output contains the raw minted alarm token %q: %s", mintedToken, logOutput)
	}
	if got := strings.Count(logOutput, "[token]"); got != 2 {
		t.Errorf("log output has %d sanitized [token] markers, want 2 (one per secret): %s", got, logOutput)
	}
}

// TestDelete_StoreErrorSanitizesToken covers the delete-error log path: a
// failed Delete's error can embed the path token, and it must not reach the
// log raw.
func TestDelete_StoreErrorSanitizesToken(t *testing.T) {
	t.Parallel()
	store := sqlitetest.Open(t)
	putRegionWithBaseURL(t, store.Regions(), 1, "https://sidecar.example.org")

	var buf bytes.Buffer
	deps := httpapi.Deps{
		Alarms:   erroringAlarmsRepo{},
		PushRegs: store.PushRegs(),
		Regions:  store.Regions(),
		Now:      func() time.Time { return base },
		Logger:   slog.New(slog.NewTextHandler(&buf, nil)),
	}
	h := httpapi.NewRouter(deps)

	const token = "supersecret-alarm-token-xyz789"
	rec := alarmRequest(t, h, http.MethodDelete, "/api/v2/regions/1/alarms/"+token, "", "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body = %s", rec.Code, rec.Body.String())
	}

	logOutput := buf.String()
	if strings.Contains(logOutput, token) {
		t.Errorf("log output contains the raw alarm token: %s", logOutput)
	}
	if !strings.Contains(logOutput, "[token]") {
		t.Errorf("log output missing sanitized [token] marker: %s", logOutput)
	}
}

// TestCreateV2_SideEffectErrorSanitizesUserPushID covers the V2
// side-effect warn log path: a failed push-registration upsert's error can
// embed user_push_id (pushreg.Upsert.Token on this call), and it must not
// reach the log raw. The alarm creation itself must still succeed -- the
// side effect failing never fails the 201 (spec §5.2).
func TestCreateV2_SideEffectErrorSanitizesUserPushID(t *testing.T) {
	t.Parallel()
	store := sqlitetest.Open(t)
	putRegionWithBaseURL(t, store.Regions(), 1, "https://sidecar.example.org")

	const userPushID = "supersecret-push-id-def456"
	var buf bytes.Buffer
	deps := httpapi.Deps{
		Alarms:   store.Alarms(),
		PushRegs: erroringPushRepo{upsertErr: fmt.Errorf("constraint failed for token %s", userPushID)},
		Regions:  store.Regions(),
		OBA:      &fakeAlarmsOBA{},
		Now:      func() time.Time { return base },
		Logger:   slog.New(slog.NewTextHandler(&buf, nil)),
	}
	h := httpapi.NewRouter(deps)

	rec := alarmRequest(t, h, http.MethodPost, "/api/v2/regions/1/alarms", formCT,
		"user_push_id="+userPushID+"&operating_system=ios")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (side-effect failure must not fail the create); body = %s", rec.Code, rec.Body.String())
	}

	logOutput := buf.String()
	if strings.Contains(logOutput, userPushID) {
		t.Errorf("log output contains the raw user_push_id: %s", logOutput)
	}
	if !strings.Contains(logOutput, "[token]") {
		t.Errorf("log output missing sanitized [token] marker: %s", logOutput)
	}
}
