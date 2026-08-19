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

	want := Conditions{
		Icon: "rain", Summary: "Light Rain",
		Temperature: 48.31, TemperatureFeelsLike: 44.02,
		PrecipPerHour: 0.0213, PrecipProbability: 0.72,
		WindSpeed: 9.14, Time: 1767980400,
	}
	if got.Current != want {
		t.Errorf("Current = %+v, want %+v", got.Current, want)
	}

	if len(got.Hourly) != 2 {
		t.Fatalf("got %d hourly entries, want 2", len(got.Hourly))
	}
	if got.Hourly[0] != want {
		t.Errorf("Hourly[0] = %+v, want %+v", got.Hourly[0], want)
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
