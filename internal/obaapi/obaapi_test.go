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
// reassembled by index rather than by arrival.
func TestFleetOrderIsDeterministic(t *testing.T) {
	agencies := []struct{ ID, Name string }{}
	vehicles := map[string][]string{}
	for _, id := range []string{"1", "2", "3", "4", "5", "6"} {
		agencies = append(agencies, struct{ ID, Name string }{id, "Agency " + id})
		vehicles[id] = []string{id + "_a", id + "_b"}
	}
	srv := newOBAServer(t, agencies, vehicles)
	c := New("", srv.Client(), slog.New(slog.DiscardHandler))

	first, err := c.Fleet(context.Background(), testRegion(srv.URL, sentinelKey))
	if err != nil {
		t.Fatalf("Fleet: %v", err)
	}
	for i := 0; i < 5; i++ {
		got, err := c.Fleet(context.Background(), testRegion(srv.URL, sentinelKey))
		if err != nil {
			t.Fatalf("Fleet: %v", err)
		}
		for j := range first {
			if got[j] != first[j] {
				t.Fatalf("run %d vehicle %d = %+v, want %+v", i, j, got[j], first[j])
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

	if _, err := New("", srv.Client(), slog.New(slog.DiscardHandler)).
		Fleet(context.Background(), testRegion(srv.URL, sentinelKey)); err == nil {
		t.Fatal("Fleet succeeded, want an error when an agency returns 500")
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
	if strings.Contains(err.Error(), sentinelKey) {
		t.Errorf("error text leaks the API key: %v", err)
	}
	if strings.Contains(logs.String(), sentinelKey) {
		t.Errorf("log output leaks the API key: %s", logs.String())
	}
}
