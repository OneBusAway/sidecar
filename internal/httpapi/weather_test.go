package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
	svc := weather.NewService(p, cache.New[weather.Snapshot](30*time.Minute, 8, 5*time.Second, now), slog.New(slog.DiscardHandler))
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
}

// A nil provider is how cmd/sidecar signals "no --pirate-weather-key was
// configured" -- weather.NewService(nil, ...) must turn that into
// weather.ErrNoProvider without any network call, and the handler must turn
// that into 403 like every other failure mode, never a 5xx.
func TestWeatherNoProviderConfiguredIs403(t *testing.T) {
	regs := newTestRegions(t, weatherRegion())
	now := func() time.Time { return time.Date(2026, 1, 9, 15, 0, 0, 0, time.UTC) }
	svc := weather.NewService(nil, cache.New[weather.Snapshot](30*time.Minute, 8, 5*time.Second, now), slog.New(slog.DiscardHandler))
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
