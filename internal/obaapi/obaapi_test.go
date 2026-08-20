package obaapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/regions"
)

const sentinelKey = "SENTINEL-API-KEY-do-not-log"

// obaServer stands in for a region's OBA REST API server. vehicleStatus lets
// a test make one agency's call fail with a specific code.
type obaServer struct {
	*httptest.Server
	agencyCalls   atomic.Int64
	vehicleStatus map[string]int
}

func newOBAServer(t *testing.T, agencies []struct{ ID, Name string }, vehicles map[string][]string) *obaServer {
	t.Helper()
	s := &obaServer{vehicleStatus: map[string]int{}}
	mux := http.NewServeMux()

	mux.HandleFunc("/api/where/agencies-with-coverage.json", func(w http.ResponseWriter, r *http.Request) {
		s.agencyCalls.Add(1)
		list := []map[string]any{}
		refs := []map[string]any{}
		for _, a := range agencies {
			list = append(list, map[string]any{
				"agencyId": a.ID, "lat": 47.6, "lon": -122.3, "latSpan": 0.1, "lonSpan": 0.1,
			})
			refs = append(refs, map[string]any{"id": a.ID, "name": a.Name})
		}
		writeOBA(w, map[string]any{
			"list":       list,
			"references": map[string]any{"agencies": refs, "routes": []any{}, "situations": []any{}, "stops": []any{}, "stopTimes": []any{}, "trips": []any{}},
		})
	})

	mux.HandleFunc("/api/where/vehicles-for-agency/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/where/vehicles-for-agency/"), ".json")
		if code, ok := s.vehicleStatus[id]; ok {
			w.WriteHeader(code)
			return
		}
		list := []map[string]any{}
		for _, v := range vehicles[id] {
			list = append(list, map[string]any{
				"vehicleId": v, "lastUpdateTime": 0, "lastLocationUpdateTime": 0,
			})
		}
		writeOBA(w, map[string]any{
			"list":       list,
			"references": map[string]any{"agencies": []any{}, "routes": []any{}, "situations": []any{}, "stops": []any{}, "stopTimes": []any{}, "trips": []any{}},
		})
	})

	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func writeOBA(w http.ResponseWriter, data map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"code": 200, "version": 2, "currentTime": 0, "text": "OK", "data": data,
	})
}

func testRegion(baseURL, key string) regions.Region {
	return regions.Region{ID: 1, Name: "Test", OBABaseURL: baseURL, OBAAPIKey: key, Active: true}
}

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

func TestFleetResolvesAgencyNamesFromReferences(t *testing.T) {
	srv := newOBAServer(t,
		[]struct{ ID, Name string }{{"1", "Metro Transit"}, {"3", "Community Transit"}},
		map[string][]string{"1": {"1_4361", "1_4362"}, "3": {"3_99"}},
	)

	got, err := New("", srv.Client(), slog.New(slog.DiscardHandler)).
		Fleet(context.Background(), testRegion(srv.URL, sentinelKey))
	if err != nil {
		t.Fatalf("Fleet: %v", err)
	}

	want := []Vehicle{
		{AgencyID: "1", AgencyName: "Metro Transit", VehicleID: "1_4361"},
		{AgencyID: "1", AgencyName: "Metro Transit", VehicleID: "1_4362"},
		{AgencyID: "3", AgencyName: "Community Transit", VehicleID: "3_99"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d vehicles, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("vehicle %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// Parallel completion order is not deterministic, so the result must be
// reassembled by index rather than by arrival. Each run is checked against a
// pinned expected order, not against an earlier run: comparing only to run 1
// would let a deterministic-but-wrong permutation pass, and would only catch
// a completion-order bug on whichever runs happen to race unevenly.
func TestFleetOrderIsDeterministic(t *testing.T) {
	agencies := []struct{ ID, Name string }{}
	vehicles := map[string][]string{}
	var want []Vehicle
	for _, id := range []string{"1", "2", "3", "4", "5", "6"} {
		agencies = append(agencies, struct{ ID, Name string }{id, "Agency " + id})
		vehicles[id] = []string{id + "_a", id + "_b"}
		want = append(want,
			Vehicle{AgencyID: id, AgencyName: "Agency " + id, VehicleID: id + "_a"},
			Vehicle{AgencyID: id, AgencyName: "Agency " + id, VehicleID: id + "_b"},
		)
	}
	srv := newOBAServer(t, agencies, vehicles)
	c := New("", srv.Client(), slog.New(slog.DiscardHandler))

	for i := 0; i < 6; i++ {
		got, err := c.Fleet(context.Background(), testRegion(srv.URL, sentinelKey))
		if err != nil {
			t.Fatalf("run %d: Fleet: %v", i, err)
		}
		if len(got) != len(want) {
			t.Fatalf("run %d: got %d vehicles, want %d: %+v", i, len(got), len(want), got)
		}
		for j := range want {
			if got[j] != want[j] {
				t.Fatalf("run %d vehicle %d = %+v, want %+v", i, j, got[j], want[j])
			}
		}
	}
}

// An agency with no realtime feed answers 4xx forever. Failing the whole
// fetch would brick vehicle search for the region permanently.
func TestFleetTolerates4xxFromOneAgency(t *testing.T) {
	srv := newOBAServer(t,
		[]struct{ ID, Name string }{{"1", "Metro"}, {"2", "NoRealtime"}},
		map[string][]string{"1": {"1_1"}},
	)
	srv.vehicleStatus["2"] = http.StatusNotFound

	got, err := New("", srv.Client(), slog.New(slog.DiscardHandler)).
		Fleet(context.Background(), testRegion(srv.URL, sentinelKey))
	if err != nil {
		t.Fatalf("Fleet: %v", err)
	}
	if len(got) != 1 || got[0].VehicleID != "1_1" {
		t.Errorf("Fleet = %+v, want just 1_1", got)
	}
}

// A 5xx is a real failure: caching a fleet with an agency silently missing
// tells every rider on its routes that their bus does not exist.
func TestFleetFailsOn5xxFromOneAgency(t *testing.T) {
	srv := newOBAServer(t,
		[]struct{ ID, Name string }{{"1", "Metro"}, {"2", "Broken"}},
		map[string][]string{"1": {"1_1"}, "2": {"2_2"}},
	)
	srv.vehicleStatus["2"] = http.StatusInternalServerError

	_, err := New("", srv.Client(), slog.New(slog.DiscardHandler)).
		Fleet(context.Background(), testRegion(srv.URL, sentinelKey))
	if err == nil {
		t.Fatal("Fleet succeeded, want an error when an agency returns 500")
	}
	// This is the one path that actually puts the key on the wire in a
	// non-2xx response: the SDK's error formats as a string containing the
	// full request URL, key included. A redact bug here is invisible to
	// TestErrorsDoNotLeakTheKey, which only exercises a transport failure.
	if errChainContains(err, sentinelKey) {
		t.Errorf("error text leaks the API key: %v", err)
	}
}

// 408 and 429 are transient, not a durable "this agency has no feed" fact,
// so they must fail the fetch like a 5xx rather than being tolerated like a
// 404. Tolerating them would silently drop a rate-limited agency's vehicles
// from the fleet.
func TestFleetFailsOn429FromOneAgency(t *testing.T) {
	srv := newOBAServer(t,
		[]struct{ ID, Name string }{{"1", "Metro"}, {"2", "RateLimited"}},
		map[string][]string{"1": {"1_1"}, "2": {"2_2"}},
	)
	srv.vehicleStatus["2"] = http.StatusTooManyRequests

	if _, err := New("", srv.Client(), slog.New(slog.DiscardHandler)).
		Fleet(context.Background(), testRegion(srv.URL, sentinelKey)); err == nil {
		t.Fatal("Fleet succeeded, want an error when an agency returns 429")
	}
}

// Cancellation must survive redact so a caller can tell a shutdown apart
// from a real upstream failure -- but the SDK returns this as a bare
// sentinel, never wrapped in *url.Error, so it needs its own path through
// redact rather than falling out of the *url.Error branch.
func TestFleetPreservesContextCancellation(t *testing.T) {
	srv := newOBAServer(t, []struct{ ID, Name string }{{"1", "Metro"}}, map[string][]string{"1": {"1_1"}})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := New("", srv.Client(), slog.New(slog.DiscardHandler)).
		Fleet(ctx, testRegion(srv.URL, sentinelKey))
	if err == nil {
		t.Fatal("Fleet succeeded with an already-cancelled context, want an error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Fleet err = %v, want context.Canceled in the chain", err)
	}
	if errChainContains(err, sentinelKey) {
		t.Errorf("error text leaks the API key: %v", err)
	}
}

func TestFleetWithoutKeyMakesNoRequest(t *testing.T) {
	srv := newOBAServer(t, []struct{ ID, Name string }{{"1", "Metro"}}, map[string][]string{"1": {"1_1"}})

	_, err := New("", srv.Client(), slog.New(slog.DiscardHandler)).
		Fleet(context.Background(), testRegion(srv.URL, ""))
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("Fleet err = %v, want ErrNotConfigured", err)
	}
	if srv.agencyCalls.Load() != 0 {
		t.Errorf("made %d requests, want 0", srv.agencyCalls.Load())
	}
}

func TestRegionKeyOverridesDefault(t *testing.T) {
	var seen atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Store(r.URL.Query().Get("key"))
		writeOBA(w, map[string]any{
			"list":       []any{},
			"references": map[string]any{"agencies": []any{}, "routes": []any{}, "situations": []any{}, "stops": []any{}, "stopTimes": []any{}, "trips": []any{}},
		})
	}))
	defer srv.Close()

	_, _ = New("process-default", srv.Client(), slog.New(slog.DiscardHandler)).
		Fleet(context.Background(), testRegion(srv.URL, "region-key"))
	if got, _ := seen.Load().(string); got != "region-key" {
		t.Errorf("key = %q, want region-key", got)
	}

	_, _ = New("process-default", srv.Client(), slog.New(slog.DiscardHandler)).
		Fleet(context.Background(), testRegion(srv.URL, ""))
	if got, _ := seen.Load().(string); got != "process-default" {
		t.Errorf("key = %q, want process-default", got)
	}
}

// The SDK puts the key in the query string, and *url.Error embeds the full
// URL. An error logged verbatim would write the secret to disk.
func TestErrorsDoNotLeakTheKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Close() // closed immediately: every request is a transport failure

	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))

	_, err := New("", srv.Client(), logger).
		Fleet(context.Background(), testRegion(srv.URL, sentinelKey))
	if err == nil {
		t.Fatal("Fleet succeeded against a closed server, want an error")
	}
	if errChainContains(err, sentinelKey) {
		t.Errorf("error text leaks the API key: %v", err)
	}
	if strings.Contains(logs.String(), sentinelKey) {
		t.Errorf("log output leaks the API key: %s", logs.String())
	}
}

// The SDK puts the key in the query string, and the default *http.Client sets
// Referer to the previous request's full URL -- query included -- on any
// redirect it follows. New must refuse to follow redirects so a region whose
// OBABaseURL (sourced from the remote regions directory, with no guarantee of
// being well-behaved) points at a redirecting server never hands the key to
// the redirect target.
//
// Mirrors weather's TestPirateWeatherDoesNotFollowRedirectsWithReferer.
func TestFleetDoesNotFollowRedirectsWithReferer(t *testing.T) {
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

	_, err := New("", origin.Client(), slog.New(slog.DiscardHandler)).
		Fleet(context.Background(), testRegion(origin.URL, sentinelKey))
	if err == nil {
		t.Fatal("Fleet succeeded against a redirecting server, want an error (a 302 is not a 200)")
	}
	if targetHit {
		t.Errorf("redirect target was contacted; Referer = %q", gotReferer)
	}
	if errChainContains(err, sentinelKey) {
		t.Errorf("error leaks the key: %v", err)
	}
	if strings.Contains(gotReferer, sentinelKey) {
		t.Errorf("Referer header leaked the key: %q", gotReferer)
	}
}

// redact checks *url.Error before the context-cancellation sentinel. net/http's
// client Timeout produces an error that is BOTH wrapped in a key-bearing
// *url.Error AND satisfies errors.Is(err, context.DeadlineExceeded) -- so a
// client whose Timeout elapses mid-request is the reachable case the branch
// order guards against. This pins both halves: the key must never appear, and
// errors.Is(err, context.DeadlineExceeded) must still hold for callers.
func TestFleetTimeoutDoesNotLeakKeyAndPreservesDeadlineExceeded(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	defer srv.Close()
	defer close(block)

	httpClient := &http.Client{Timeout: 20 * time.Millisecond}

	_, err := New("", httpClient, slog.New(slog.DiscardHandler)).
		Fleet(context.Background(), testRegion(srv.URL, sentinelKey))
	if err == nil {
		t.Fatal("Fleet succeeded against a server that never responds, want a timeout error")
	}
	if errChainContains(err, sentinelKey) {
		t.Errorf("error leaks the key: %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Fleet err = %v, want context.DeadlineExceeded in the chain", err)
	}
}

// New must apply usable defaults for a nil http.Client and a nil logger --
// the latter matters because the 4xx-tolerance path calls c.logger.Warn, and
// a nil *slog.Logger there panics rather than merely misbehaving.
func TestNewAppliesDefaultsWithoutPanicking(t *testing.T) {
	srv := newOBAServer(t,
		[]struct{ ID, Name string }{{"1", "Metro"}, {"2", "NoRealtime"}},
		map[string][]string{"1": {"1_1"}},
	)
	srv.vehicleStatus["2"] = http.StatusNotFound

	got, err := New("", nil, nil).Fleet(context.Background(), testRegion(srv.URL, sentinelKey))
	if err != nil {
		t.Fatalf("Fleet: %v", err)
	}
	if len(got) != 1 || got[0].VehicleID != "1_1" {
		t.Errorf("Fleet = %+v, want just 1_1", got)
	}
}

// TestFleetWarnsWhenEveryAgencyDeclines pins the observability of the case
// where every agency answers 4xx: the fleet is empty and stays cached, so an
// operator's only signal that a region has gone dark is this log line. The
// status code is unchanged (an empty fleet is a legitimate 200), which is why
// the log has to carry the weight.
func TestFleetWarnsWhenEveryAgencyDeclines(t *testing.T) {
	srv := newOBAServer(t,
		[]struct{ ID, Name string }{{"1", "Metro"}, {"2", "Other"}},
		map[string][]string{},
	)
	srv.vehicleStatus["1"] = http.StatusNotFound
	srv.vehicleStatus["2"] = http.StatusNotFound

	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))

	fleet, err := New("", srv.Client(), logger).
		Fleet(context.Background(), testRegion(srv.URL, sentinelKey))
	if err != nil {
		t.Fatalf("Fleet: %v", err)
	}
	if len(fleet) != 0 {
		t.Fatalf("fleet = %+v, want empty", fleet)
	}
	if !strings.Contains(logs.String(), "every agency declined") {
		t.Errorf("no all-declined warning logged; operator would have no signal:\n%s", logs.String())
	}
}
