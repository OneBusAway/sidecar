package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/cache"
	"github.com/OneBusAway/sidecar/internal/obaapi"
	"github.com/OneBusAway/sidecar/internal/regions"
	"github.com/OneBusAway/sidecar/internal/store/sqlitetest"
	"github.com/OneBusAway/sidecar/internal/vehicles"
)

// fakeOBA is an obaapi.Client that returns a canned fleet or a canned error.
type fakeOBA struct {
	fleet []obaapi.Vehicle
	err   error
	calls int
}

func (f *fakeOBA) Fleet(context.Context, regions.Region) ([]obaapi.Vehicle, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.fleet, nil
}

// newTestRegions opens a migrated store and seeds it with the given regions.
// There is no shared helper for this in the package -- the existing tests each
// call sqlitetest.Open and seed inline -- so this one is defined here and
// reused by both the vehicles and weather handler tests.
func newTestRegions(t *testing.T, regs ...regions.Region) regions.Repository {
	t.Helper()
	store := sqlitetest.Open(t)
	if err := store.Regions().UpsertFromDirectory(context.Background(), regs, testNow); err != nil {
		t.Fatalf("seed regions: %v", err)
	}
	// UpsertFromDirectory deliberately ignores the locally-managed columns, so
	// any region needing an API key gets it through SetLocalFields.
	for _, r := range regs {
		if r.OBAAPIKey == "" {
			continue
		}
		if err := store.Regions().SetLocalFields(context.Background(), r.ID, regions.LocalFields{
			DefaultAgencyID: r.DefaultAgencyID, Timezone: r.Timezone, OBAAPIKey: r.OBAAPIKey,
		}, testNow); err != nil {
			t.Fatalf("set key for region %d: %v", r.ID, err)
		}
	}
	return store.Regions()
}

func newVehiclesTestServer(t *testing.T, oba obaapi.Client, regs regions.Repository) http.Handler {
	t.Helper()
	now := func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }
	svc := vehicles.NewService(oba,
		cache.New[[]obaapi.Vehicle](30*time.Minute, 8, 12*time.Second, now),
		cache.New[[]vehicles.Match](5*time.Minute, 64, 13*time.Second, now),
		discardLogger(),
	)
	return NewRouter(Deps{
		Regions:  regs,
		Vehicles: svc,
		Now:      now,
		Logger:   discardLogger(),
	})
}

func TestVehiclesUnknownRegionIs404(t *testing.T) {
	regs := newTestRegions(t, regions.Region{ID: 1, Name: "R", OBABaseURL: "https://x/", OBAAPIKey: "k", Active: true})
	srv := newVehiclesTestServer(t, &fakeOBA{}, regs)

	for _, seg := range []string{"99", "nope", "99999999999999999999999"} {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/v1/regions/"+seg+"/vehicles?query=abc", nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("segment %q: status = %d, want 404", seg, rec.Code)
		}
		if got := rec.Body.String(); got != notFoundBody {
			t.Errorf("segment %q: body = %q, want %q", seg, got, notFoundBody)
		}
	}
}

func TestVehiclesShortQueryReturnsEmptyArrayWithoutUpstream(t *testing.T) {
	regs := newTestRegions(t, regions.Region{ID: 1, Name: "R", OBABaseURL: "https://x/", OBAAPIKey: "k", Active: true})
	oba := &fakeOBA{fleet: []obaapi.Vehicle{{AgencyID: "1", AgencyName: "M", VehicleID: "1_4361"}}}
	srv := newVehiclesTestServer(t, oba, regs)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/v1/regions/1/vehicles?query=ab", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != "[]\n" && got != "[]" {
		t.Errorf("body = %q, want an empty JSON array", got)
	}
	if oba.calls != 0 {
		t.Errorf("made %d upstream calls, want 0", oba.calls)
	}
}

// A no-match search must serialize as [] and never as null: the schema says
// array, and a client decoding into an array chokes on null.
func TestVehiclesNoMatchIsEmptyArrayNotNull(t *testing.T) {
	regs := newTestRegions(t, regions.Region{ID: 1, Name: "R", OBABaseURL: "https://x/", OBAAPIKey: "k", Active: true})
	srv := newVehiclesTestServer(t, &fakeOBA{fleet: []obaapi.Vehicle{
		{AgencyID: "1", AgencyName: "M", VehicleID: "1_4361"},
	}}, regs)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/v1/regions/1/vehicles?query=zzz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got == "null\n" || got == "null" {
		t.Fatal("body = null, want []")
	}
	var out []vehicles.Match
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("got %d matches, want 0", len(out))
	}
}

func TestVehiclesSuccessShape(t *testing.T) {
	regs := newTestRegions(t, regions.Region{ID: 1, Name: "R", OBABaseURL: "https://x/", OBAAPIKey: "k", Active: true})
	srv := newVehiclesTestServer(t, &fakeOBA{fleet: []obaapi.Vehicle{
		{AgencyID: "1", AgencyName: "Metro Transit", VehicleID: "1_4361"},
	}}, regs)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/v1/regions/1/vehicles?query=436", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	// The JSON key set is the wire contract. Compare against a literal rather
	// than a golden file; these names come from openapi.yaml's VehicleMatch
	// schema (required: id, name, vehicle_id).
	var raw []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(raw) != 1 {
		t.Fatalf("got %d matches, want 1", len(raw))
	}
	wantKeys := map[string]bool{"id": true, "name": true, "vehicle_id": true}
	for k := range raw[0] {
		if !wantKeys[k] {
			t.Errorf("unexpected JSON key %q", k)
		}
		delete(wantKeys, k)
	}
	for k := range wantKeys {
		t.Errorf("missing JSON key %q", k)
	}
}

func TestVehiclesUpstreamFailureIs502(t *testing.T) {
	regs := newTestRegions(t, regions.Region{ID: 1, Name: "R", OBABaseURL: "https://x/", OBAAPIKey: "k", Active: true})
	srv := newVehiclesTestServer(t, &fakeOBA{err: errors.New("upstream down")}, regs)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/v1/regions/1/vehicles?query=436", nil))

	// 200 [] would be indistinguishable from "no such bus", telling a rider
	// their existing bus does not exist.
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
}

func TestVehiclesUnconfiguredKeyIs502(t *testing.T) {
	regs := newTestRegions(t, regions.Region{ID: 1, Name: "R", OBABaseURL: "https://x/", Active: true})
	srv := newVehiclesTestServer(t, &fakeOBA{err: obaapi.ErrNotConfigured}, regs)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/v1/regions/1/vehicles?query=436", nil))

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
}
