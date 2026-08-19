package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/cache"
	"github.com/OneBusAway/sidecar/internal/regions"
	"github.com/OneBusAway/sidecar/internal/weather"
)

type fakeProvider struct {
	snap  weather.Snapshot
	err   error
	calls int
}

func (f *fakeProvider) Fetch(context.Context, regions.LatLon) (weather.Snapshot, error) {
	f.calls++
	if f.err != nil {
		return weather.Snapshot{}, f.err
	}
	return f.snap, nil
}

func sampleSnapshot(at time.Time) weather.Snapshot {
	return weather.Snapshot{
		Units:        "us",
		TodaySummary: "Rain until evening.",
		Current: weather.Conditions{
			Icon: "rain", Summary: "Light Rain", Temperature: 48.31,
			TemperatureFeelsLike: 44.02, PrecipPerHour: 0.0213,
			PrecipProbability: 0.72, WindSpeed: 9.14, Time: 1767980400,
		},
		Hourly:      []weather.Conditions{{Icon: "rain", Time: 1767980400}},
		RetrievedAt: at,
	}
}

func newWeatherTestServer(t *testing.T, p weather.Provider, regs regions.Repository) http.Handler {
	t.Helper()
	now := func() time.Time { return time.Date(2026, 1, 9, 15, 0, 0, 0, time.UTC) }
	svc := weather.NewService(p, cache.New[weather.Snapshot](30*time.Minute, 8, 5*time.Second, now))
	return NewRouter(Deps{
		Regions: regs,
		Weather: svc,
		Now:     now,
		Logger:  slog.New(slog.DiscardHandler),
	})
}

func weatherRegion() regions.Region {
	return regions.Region{
		ID: 1, Name: "Puget Sound", OBABaseURL: "https://x/", Active: true,
		Centroid: &regions.LatLon{Lat: 47.75, Lon: -122.49},
	}
}

func TestWeatherUnknownRegionIs404(t *testing.T) {
	regs := newTestRegions(t, weatherRegion())
	srv := newWeatherTestServer(t, &fakeProvider{}, regs)

	for _, seg := range []string{"99", "nope", "99999999999999999999999"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/v1/regions/"+seg+"/weather", nil)
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("segment %q: status = %d, want 404", seg, rec.Code)
		}
		// Pins this handler's use of the shared writeRegionNotFound helper:
		// checking only rec.Code lets a bare w.WriteHeader(404) (no body, no
		// Content-Type) survive undetected.
		if got := rec.Body.String(); got != notFoundBody {
			t.Errorf("segment %q: body = %q, want %q", seg, got, notFoundBody)
		}
		if got := rec.Header().Get("Content-Type"); got != "application/json" {
			t.Errorf("segment %q: Content-Type = %q, want application/json", seg, got)
		}
	}
}

// 403, not 404: telling the app the region does not exist is a different and
// false claim, and shipped apps read any non-200 as "hide the weather UI".
func TestWeatherNilCentroidIs403(t *testing.T) {
	regs := newTestRegions(t, regions.Region{ID: 1, Name: "Unsynced", OBABaseURL: "https://x/", Active: true})
	p := &fakeProvider{snap: sampleSnapshot(time.Now())}
	srv := newWeatherTestServer(t, p, regs)

	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/v1/regions/1/weather", nil)
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
	if p.calls != 0 {
		t.Errorf("made %d provider calls, want 0", p.calls)
	}
	// writeUnavailable's contract: a client that decodes the body before
	// checking status must see valid JSON, not an empty response it fails to
	// parse.
	if got := rec.Body.String(); got != "{}" {
		t.Errorf("body = %q, want {}", got)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
}

// A nil provider is how cmd/sidecar signals "no --pirate-weather-key was
// configured" -- weather.NewService(nil, ...) must turn that into
// weather.ErrNoProvider without any network call, and the handler must turn
// that into 403 like every other failure mode, never a 5xx.
func TestWeatherNoProviderConfiguredIs403(t *testing.T) {
	regs := newTestRegions(t, weatherRegion())
	now := func() time.Time { return time.Date(2026, 1, 9, 15, 0, 0, 0, time.UTC) }
	svc := weather.NewService(nil, cache.New[weather.Snapshot](30*time.Minute, 8, 5*time.Second, now))
	srv := NewRouter(Deps{
		Regions: regs,
		Weather: svc,
		Now:     now,
		Logger:  slog.New(slog.DiscardHandler),
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/v1/regions/1/weather", nil)
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (no provider configured, never a 5xx)", rec.Code)
	}
}

func TestWeatherProviderErrorIs403(t *testing.T) {
	regs := newTestRegions(t, weatherRegion())
	srv := newWeatherTestServer(t, &fakeProvider{err: errors.New("upstream down")}, regs)

	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/v1/regions/1/weather", nil)
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (never 5xx: apps are tested against 403)", rec.Code)
	}
}

func TestWeatherSuccessShape(t *testing.T) {
	fetchedAt := time.Date(2026, 1, 9, 14, 31, 0, 0, time.UTC)
	regs := newTestRegions(t, weatherRegion())
	srv := newWeatherTestServer(t, &fakeProvider{snap: sampleSnapshot(fetchedAt)}, regs)

	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/v1/regions/1/weather", nil)
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Key set from openapi.yaml's WeatherForecast schema. A literal rather
	// than a golden file: a golden diff reads as something to accept, whereas
	// a missing key here reads as a broken contract.
	want := []string{
		"latitude", "longitude", "region_identifier", "region_name",
		"retrieved_at", "units", "today_summary", "current_forecast", "hourly_forecast",
	}
	for _, k := range want {
		if _, ok := raw[k]; !ok {
			t.Errorf("missing JSON key %q", k)
		}
	}
	if len(raw) != len(want) {
		t.Errorf("got %d keys, want %d: %v", len(raw), len(want), raw)
	}

	if raw["region_identifier"] != float64(1) {
		t.Errorf("region_identifier = %v, want 1", raw["region_identifier"])
	}
	if raw["region_name"] != "Puget Sound" {
		t.Errorf("region_name = %v, want Puget Sound", raw["region_name"])
	}
	if raw["latitude"] != 47.75 {
		t.Errorf("latitude = %v, want 47.75", raw["latitude"])
	}
	// weatherRegion's centroid is {47.75, -122.49} -- distinct digits on
	// both axes, so a one-sided mutation (e.g. Longitude: region.Centroid.Lat)
	// is caught here rather than only by a full-swap mutation.
	if raw["longitude"] != -122.49 {
		t.Errorf("longitude = %v, want -122.49", raw["longitude"])
	}

	// The OpenAPI schema says string/date-time. An epoch integer would pass
	// every other assertion here and violate the contract.
	got, ok := raw["retrieved_at"].(string)
	if !ok {
		t.Fatalf("retrieved_at = %T, want an RFC 3339 string", raw["retrieved_at"])
	}
	gotTime, err := time.Parse(time.RFC3339, got)
	if err != nil {
		t.Errorf("retrieved_at %q is not RFC 3339: %v", got, err)
	}
	// Pinned to fetchedAt (14:31), not the test server's fixed clock (15:00):
	// the request handler's Deps.Now is a constant in this suite, so a
	// handler that recomputed retrieved_at at serialization time instead of
	// reading it off the cached Snapshot would produce a value that still
	// parses as RFC 3339 and would slip past every other assertion here.
	if !gotTime.Equal(fetchedAt) {
		t.Errorf("retrieved_at = %v, want %v (the provider's fetch time, not serialization time)", gotTime, fetchedAt)
	}

	// The top-level key set alone leaves current_forecast/hourly_forecast as
	// opaque `any`, so renaming or dropping a field inside WeatherConditions
	// (e.g. json:"temperature_feels_like" -> "apparent_temperature") would
	// pass every assertion above while breaking every shipped app's stop
	// screen. wantConditionsKeys is openapi.yaml's WeatherConditions schema.
	wantConditionsKeys := []string{
		"icon", "summary", "temperature", "temperature_feels_like",
		"precip_per_hour", "precip_probability", "wind_speed", "time",
	}
	assertExactKeys := func(t *testing.T, label string, obj map[string]any) {
		t.Helper()
		for _, k := range wantConditionsKeys {
			if _, present := obj[k]; !present {
				t.Errorf("%s: missing JSON key %q", label, k)
			}
		}
		if len(obj) != len(wantConditionsKeys) {
			t.Errorf("%s: got %d keys, want %d: %v", label, len(obj), len(wantConditionsKeys), obj)
		}
	}

	current, ok := raw["current_forecast"].(map[string]any)
	if !ok {
		t.Fatalf("current_forecast = %T, want an object", raw["current_forecast"])
	}
	assertExactKeys(t, "current_forecast", current)

	hourly, ok := raw["hourly_forecast"].([]any)
	if !ok || len(hourly) == 0 {
		t.Fatalf("hourly_forecast = %v, want a non-empty array", raw["hourly_forecast"])
	}
	first, ok := hourly[0].(map[string]any)
	if !ok {
		t.Fatalf("hourly_forecast[0] = %T, want an object", hourly[0])
	}
	assertExactKeys(t, "hourly_forecast[0]", first)
}

// retrieved_at describes when the data was fetched, not when it was served.
// Recomputing it on a cache hit would tell a client 29-minute-old data is
// fresh.
func TestWeatherRetrievedAtIsStableAcrossCacheHits(t *testing.T) {
	fetchedAt := time.Date(2026, 1, 9, 14, 31, 0, 0, time.UTC)
	regs := newTestRegions(t, weatherRegion())
	p := &fakeProvider{snap: sampleSnapshot(fetchedAt)}
	srv := newWeatherTestServer(t, p, regs)

	read := func() string {
		rec := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/v1/regions/1/weather", nil)
		srv.ServeHTTP(rec, req)
		var raw map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
			t.Fatalf("decode: %v", err)
		}
		s, _ := raw["retrieved_at"].(string)
		return s
	}

	first, second := read(), read()
	if first != second {
		t.Errorf("retrieved_at changed across a cache hit: %q then %q", first, second)
	}
	if p.calls != 1 {
		t.Errorf("made %d provider calls, want 1", p.calls)
	}
}

// Two regions sharing a centroid must share one upstream call: the cache is
// keyed by coordinate, not region id. Keying by region id instead would pass
// every other test in this file while doubling upstream traffic for any
// directory that has two regions at the same point.
func TestWeatherSharedCentroidSharesOneUpstreamCall(t *testing.T) {
	fetchedAt := time.Date(2026, 1, 9, 14, 31, 0, 0, time.UTC)
	centroid := &regions.LatLon{Lat: 47.75, Lon: -122.49}
	regs := newTestRegions(t,
		regions.Region{ID: 1, Name: "Region One", OBABaseURL: "https://x/", Active: true, Centroid: centroid},
		regions.Region{ID: 2, Name: "Region Two", OBABaseURL: "https://y/", Active: true, Centroid: centroid},
	)
	p := &fakeProvider{snap: sampleSnapshot(fetchedAt)}
	srv := newWeatherTestServer(t, p, regs)

	for _, seg := range []string{"1", "2"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/v1/regions/"+seg+"/weather", nil)
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("region %s: status = %d, want 200", seg, rec.Code)
		}
	}

	if p.calls != 1 {
		t.Errorf("made %d provider calls, want 1 (regions share a centroid)", p.calls)
	}
}

// erroringRegions (defined in vehicles_test.go, same package) covers the one
// branch none of the ErrNotFound-only fixtures above ever reach: a real
// store failure. That must become a 500 with an empty body, not 403 --
// otherwise a database outage presents to operators as "riders see no
// weather" (a product state) instead of an infrastructure failure. Verified
// by mutation: substituting h.writeUnavailable(w) for the 500 branch in
// weatherHandler.forecast passes every other test in this package.
func TestWeatherRegionLookupFailureIs500(t *testing.T) {
	srv := newWeatherTestServer(t, &fakeProvider{}, erroringRegions{err: errors.New("db exploded")})

	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/v1/regions/1/weather", nil)
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	// Store errors are for the operator's log, never the rider's screen.
	if rec.Body.Len() != 0 {
		t.Errorf("body = %q, want empty", rec.Body.String())
	}
}

// TestWeatherLogsCancellationAtWarnOnly exercises slogLevelForUpstreamErr
// through the actual handler and a capturing slog.Handler, and separately
// pins the ErrNoProvider branch's own message and level. Without this test,
// every log call in weatherHandler.forecast's error paths could be silenced
// or have its level flattened to a single value and nothing else in this
// package would notice.
func TestWeatherLogsCancellationAtWarnOnly(t *testing.T) {
	tests := []struct {
		name        string
		err         error
		wantContain string
		wantAbsent  string
	}{
		{
			name:        "a client disconnecting mid-request (context.Canceled) logs at Warn",
			err:         context.Canceled,
			wantContain: "level=WARN",
			wantAbsent:  "level=ERROR",
		},
		{
			// The detached fetch's own budget elapsing: the upstream is slow
			// or down, which is exactly what should page, not get demoted.
			name:        "a fetch-budget timeout (context.DeadlineExceeded) logs at Error",
			err:         context.DeadlineExceeded,
			wantContain: "level=ERROR",
			wantAbsent:  "level=WARN",
		},
		{
			name:        "a plain provider error logs at Error",
			err:         errors.New("upstream exploded"),
			wantContain: "level=ERROR",
			wantAbsent:  "level=WARN",
		},
		{
			// A boot-time warning already told the operator once; a
			// per-request Error here would just be noise on every request to
			// an unconfigured deployment.
			name:        "no provider configured logs its own message at Warn",
			err:         weather.ErrNoProvider,
			wantContain: "level=WARN msg=\"httpapi: weather not configured\"",
			wantAbsent:  "level=ERROR",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// A real sentinel key on the region (not weatherRegion's default,
			// which carries none): the design spec's key-in-logs test
			// requires a real key on the path, and this is also what catches
			// a handler edit that logs the whole regions.Region instead of
			// picking region_id -- without regions.Region.LogValue omitting
			// OBAAPIKey, that substitution would print the key here in full.
			region := weatherRegion()
			region.OBAAPIKey = sentinelKey
			regs := newTestRegions(t, region)
			now := func() time.Time { return time.Date(2026, 1, 9, 15, 0, 0, 0, time.UTC) }
			var buf bytes.Buffer
			capturingLogger := slog.New(slog.NewTextHandler(&buf, nil))

			var provider weather.Provider
			if !errors.Is(tt.err, weather.ErrNoProvider) {
				provider = &fakeProvider{err: tt.err}
			}
			svc := weather.NewService(provider, cache.New[weather.Snapshot](30*time.Minute, 8, 5*time.Second, now))
			srv := NewRouter(Deps{Regions: regs, Weather: svc, Now: now, Logger: capturingLogger})

			rec := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/v1/regions/1/weather", nil)
			srv.ServeHTTP(rec, req)

			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", rec.Code)
			}
			if got := buf.String(); !strings.Contains(got, tt.wantContain) {
				t.Errorf("log output = %q, want it to contain %q", got, tt.wantContain)
			}
			if got := buf.String(); strings.Contains(got, tt.wantAbsent) {
				t.Errorf("log output = %q, want it NOT to contain %q", got, tt.wantAbsent)
			}
			if got := buf.String(); strings.Contains(got, sentinelKey) {
				t.Errorf("log output leaks the region's API key: %s", got)
			}
		})
	}
}
