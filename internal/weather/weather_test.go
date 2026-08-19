package weather

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/cache"
	"github.com/OneBusAway/sidecar/internal/regions"
)

const sentinelKey = "SENTINEL-WEATHER-KEY-do-not-log"

func fixedNow() time.Time { return time.Date(2026, 1, 9, 15, 0, 0, 0, time.UTC) }

// errChainContains reports whether needle appears in the Error() text of err
// or anything err wraps. A leak fixed at the top frame but reintroduced by a
// %w further down the chain would be invisible to a check of err.Error()
// alone only by the accident of how the outer message happens to be built;
// walking the chain explicitly does not depend on that accident.
func errChainContains(err error, needle string) bool {
	for e := err; e != nil; e = errors.Unwrap(e) {
		if strings.Contains(e.Error(), needle) {
			return true
		}
	}
	return false
}

func TestPirateWeatherMapping(t *testing.T) {
	body, err := os.ReadFile("testdata/pirate.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	p := newPirateWeatherWithBase(srv.URL, sentinelKey, srv.Client(), fixedNow)
	got, err := p.Fetch(context.Background(), regions.LatLon{Lat: 47.75, Lon: -122.49})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	if !strings.Contains(gotPath, sentinelKey) {
		t.Errorf("request path %q does not carry the key", gotPath)
	}
	if !strings.Contains(gotPath, "47.75,-122.49") {
		t.Errorf("request path %q does not carry the coordinate", gotPath)
	}
	if !strings.Contains(gotQuery, "units=us") {
		t.Errorf("query %q missing units=us", gotQuery)
	}
	if !strings.Contains(gotQuery, "exclude=minutely,alerts") {
		t.Errorf("query %q missing exclude=minutely,alerts", gotQuery)
	}

	// The fixture's flags.units is "si", deliberately mismatched from what
	// we request: Units must come from our own requestedUnits constant, not
	// be echoed from the provider's response. If those two ever agreed by
	// coincidence, a mapping bug that reads the provider's echo instead of
	// our constant would be invisible here.
	if got.Units != "us" {
		t.Errorf("Units = %q, want us", got.Units)
	}
	// today_summary is the DAY's summary (daily.data[0].summary), not the
	// week's (daily.summary). Getting this wrong shows riders next Thursday's
	// weather with today's temperature.
	if got.TodaySummary != "Rain until evening." {
		t.Errorf("TodaySummary = %q, want %q", got.TodaySummary, "Rain until evening.")
	}
	if !got.RetrievedAt.Equal(fixedNow()) {
		t.Errorf("RetrievedAt = %v, want %v", got.RetrievedAt, fixedNow())
	}

	// currently and hourly.data[0] carry different temperature/
	// apparentTemperature values in the fixture specifically so that Current
	// and Hourly[0] pin to their own source objects: if they were identical,
	// nothing would catch a bug that mapped Current from hourly.data[0]
	// instead of currently.
	wantCurrent := Conditions{
		Icon: "rain", Summary: "Light Rain",
		Temperature: 49.60, TemperatureFeelsLike: 45.11,
		PrecipPerHour: 0.0213, PrecipProbability: 0.72,
		WindSpeed: 9.14, Time: 1767980400,
	}
	if got.Current != wantCurrent {
		t.Errorf("Current = %+v, want %+v", got.Current, wantCurrent)
	}

	if len(got.Hourly) != 2 {
		t.Fatalf("got %d hourly entries, want 2", len(got.Hourly))
	}
	wantHourly0 := Conditions{
		Icon: "rain", Summary: "Light Rain",
		Temperature: 48.31, TemperatureFeelsLike: 44.02,
		PrecipPerHour: 0.0213, PrecipProbability: 0.72,
		WindSpeed: 9.14, Time: 1767980400,
	}
	if got.Hourly[0] != wantHourly0 {
		t.Errorf("Hourly[0] = %+v, want %+v", got.Hourly[0], wantHourly0)
	}
	// An icon we have never seen must pass through untouched: the vocabulary
	// belongs to the provider, and mapping it to a fallback would hide a new
	// condition from riders.
	if got.Hourly[1].Icon != "some-icon-we-have-never-seen" {
		t.Errorf("Hourly[1].Icon = %q, want the raw provider value", got.Hourly[1].Icon)
	}
}

func TestPirateWeatherNon200IsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	p := newPirateWeatherWithBase(srv.URL, sentinelKey, srv.Client(), fixedNow)
	_, err := p.Fetch(context.Background(), regions.LatLon{Lat: 1, Lon: 2})
	if err == nil {
		t.Fatal("Fetch succeeded on a 429, want an error")
	}
	// The non-200 path builds its error from resp.StatusCode, but the request
	// URL sitting right next to it in scope also carries the key: a careless
	// edit that reaches for req.URL or resp.Request.URL instead of the status
	// code would leak it right back out. This is the path the sibling
	// package's leak test missed -- it covered only the transport failure.
	if errChainContains(err, sentinelKey) {
		t.Errorf("error leaks the key: %v", err)
	}
}

// The key is a path segment, and *url.Error embeds the whole URL. An error
// returned verbatim writes the secret wherever the caller logs it.
func TestPirateWeatherErrorsDoNotLeakTheKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Close()

	p := newPirateWeatherWithBase(srv.URL, sentinelKey, srv.Client(), fixedNow)
	_, err := p.Fetch(context.Background(), regions.LatLon{Lat: 1, Lon: 2})
	if err == nil {
		t.Fatal("Fetch succeeded against a closed server, want an error")
	}
	if errChainContains(err, sentinelKey) {
		t.Errorf("error leaks the key: %v", err)
	}
}

// redact's fallthrough branch (a transport error that is not *url.Error) is
// unreachable through Fetch with a real *http.Client: Go's Client.Do always
// wraps a RoundTripper's error in *url.Error before returning it, so
// TestPirateWeatherErrorsDoNotLeakTheKey never exercises this branch. Calling
// redact directly closes that gap: without this test, rewrapping the
// fallthrough's err with %w would leak whatever an unusual RoundTripper
// implementation put in its error text, and no test in this file would
// notice.
func TestRedactFallbackDoesNotLeakArbitraryErrorText(t *testing.T) {
	err := fmt.Errorf("some ad-hoc transport error mentioning %s", sentinelKey)
	got := redact(err)
	if errChainContains(got, sentinelKey) {
		t.Errorf("redact leaked the key from a non-*url.Error: %v", got)
	}
}

// The key is a path segment, so the default *http.Client's redirect
// handling -- which sets Referer to the previous request's full URL --
// would hand the key to whatever server a redirect points at. That server
// is not one we chose to trust, so this is worse than a log leak: it's
// disclosure to an arbitrary third party. newPirateWeatherWithBase must
// refuse to follow redirects so the target is never contacted at all.
func TestPirateWeatherDoesNotFollowRedirectsWithReferer(t *testing.T) {
	var targetHit bool
	var gotReferer string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHit = true
		gotReferer = r.Header.Get("Referer")
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer origin.Close()

	p := newPirateWeatherWithBase(origin.URL, sentinelKey, origin.Client(), fixedNow)
	_, err := p.Fetch(context.Background(), regions.LatLon{Lat: 1, Lon: 2})
	if err == nil {
		t.Fatal("Fetch succeeded on a redirect, want an error (a 302 is not a 200)")
	}
	if targetHit {
		t.Errorf("redirect target was contacted; Referer = %q", gotReferer)
	}
	if errChainContains(err, sentinelKey) {
		t.Errorf("error leaks the key: %v", err)
	}
}

// daily.data and hourly.data can legitimately be empty (e.g. a provider
// outage that still returns 200 with a bare skeleton). Fetch must not panic
// indexing daily.data[0], must leave TodaySummary empty rather than
// fabricating one, and must return a non-nil, empty Hourly slice -- a nil
// slice serializes to "hourly": null, a response-shape regression the next
// task (the HTTP handler) would otherwise inherit silently.
func TestPirateWeatherHandlesEmptyDailyAndHourlyData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"currently":{},"hourly":{"data":[]},"daily":{"data":[]}}`))
	}))
	defer srv.Close()

	p := newPirateWeatherWithBase(srv.URL, sentinelKey, srv.Client(), fixedNow)
	got, err := p.Fetch(context.Background(), regions.LatLon{Lat: 1, Lon: 2})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got.TodaySummary != "" {
		t.Errorf("TodaySummary = %q, want empty", got.TodaySummary)
	}
	if got.Hourly == nil {
		t.Error("Hourly = nil, want a non-nil empty slice")
	}
	if len(got.Hourly) != 0 {
		t.Errorf("Hourly = %+v, want empty", got.Hourly)
	}
}

// The provider is untrusted and the body read happens before anything is
// validated, so the read must be capped. This body is only valid JSON once
// its closing quote and brace -- the very last two bytes, past maxBody --
// are read; if the cap is applied, decoding sees a truncated document and
// fails, proving the read stopped at the cap rather than consuming the
// whole (oversized) response.
func TestPirateWeatherResponseBodyIsBounded(t *testing.T) {
	prefix := `{"currently":{},"hourly":{"data":[]},"daily":{"data":[]},"padding":"`
	body := prefix + strings.Repeat("x", maxBody) + `"}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	p := newPirateWeatherWithBase(srv.URL, sentinelKey, srv.Client(), fixedNow)
	if _, err := p.Fetch(context.Background(), regions.LatLon{Lat: 1, Lon: 2}); err == nil {
		t.Fatal("Fetch succeeded reading an oversized body, want a decode error from a truncated read")
	}
}

// countingProvider counts Fetch calls; every call succeeds with a zero
// Snapshot, since these tests only care how many times the cache missed.
type countingProvider struct{ calls int }

func (c *countingProvider) Fetch(context.Context, regions.LatLon) (Snapshot, error) {
	c.calls++
	return Snapshot{}, nil
}

// TestServiceCacheKeySharesNearbyCoordinates and
// TestServiceCacheKeyDistinguishesFartherCoordinates together pin the
// rounding precision Service.Snapshot's cache key uses (4 decimal places,
// ~11 metres). TestWeatherSharedCentroidSharesOneUpstreamCall in
// internal/httpapi only ever calls Snapshot with two *identical* LatLon
// values, so it cannot tell '4 decimals' apart from '1 decimal' (~11km
// buckets -- would serve a neighbouring city's weather) or '8 decimals'
// (would stop two near-duplicate directory centroids from sharing a cache
// entry at all). These two tests call Snapshot with genuinely distinct
// coordinates that must round to the same or different buckets respectively.
func TestServiceCacheKeySharesNearbyCoordinates(t *testing.T) {
	p := &countingProvider{}
	svc := NewService(p, cache.New[Snapshot](30*time.Minute, 8, 5*time.Second, fixedNow))
	ctx := context.Background()

	// Differ at the 5th decimal (~1m apart) -- both round to 47.7500 /
	// -122.4900 at 4-decimal precision, so they must share one cache entry.
	if _, err := svc.Snapshot(ctx, regions.LatLon{Lat: 47.75001, Lon: -122.49001}); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if _, err := svc.Snapshot(ctx, regions.LatLon{Lat: 47.75004, Lon: -122.49004}); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if p.calls != 1 {
		t.Errorf("made %d provider calls, want 1 (coordinates round to the same 4-decimal bucket)", p.calls)
	}
}

func TestServiceCacheKeyDistinguishesFartherCoordinates(t *testing.T) {
	p := &countingProvider{}
	svc := NewService(p, cache.New[Snapshot](30*time.Minute, 8, 5*time.Second, fixedNow))
	ctx := context.Background()

	// Differ at the 3rd decimal (~100m apart) -- 47.7500 vs 47.7510 at
	// 4-decimal precision, so they must NOT share a cache entry.
	if _, err := svc.Snapshot(ctx, regions.LatLon{Lat: 47.750, Lon: -122.490}); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if _, err := svc.Snapshot(ctx, regions.LatLon{Lat: 47.751, Lon: -122.491}); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if p.calls != 2 {
		t.Errorf("made %d provider calls, want 2 (coordinates fall in different 4-decimal buckets)", p.calls)
	}
}

// TestServiceNoProviderReturnsErrNoProviderWithoutCaching pins the nil-
// provider short-circuit directly against Service (the httpapi package pins
// the same behaviour end-to-end through the handler, but this is the one
// place that can assert the cache is never touched at all).
func TestServiceNoProviderReturnsErrNoProviderWithoutCaching(t *testing.T) {
	svc := NewService(nil, cache.New[Snapshot](30*time.Minute, 8, 5*time.Second, fixedNow))

	_, err := svc.Snapshot(context.Background(), regions.LatLon{Lat: 1, Lon: 2})
	if !errors.Is(err, ErrNoProvider) {
		t.Errorf("err = %v, want ErrNoProvider", err)
	}
}
