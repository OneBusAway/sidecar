package vehicles

import (
	"context"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/cache"
	"github.com/OneBusAway/sidecar/internal/obaapi"
	"github.com/OneBusAway/sidecar/internal/regions"
)

// fleetByRegion is an obaapi.Client stub that hands back a fixed, distinct
// fleet per region id and counts how many times each region's fleet was
// actually fetched. The count exists because the entire reason Service
// wraps a fleet cache around obaapi.Client is to make that number 1
// regardless of how many searches land on the same region.
type fleetByRegion struct {
	fleets map[int64][]obaapi.Vehicle
	calls  map[int64]*atomic.Int64
}

func newFleetByRegion(fleets map[int64][]obaapi.Vehicle) *fleetByRegion {
	calls := make(map[int64]*atomic.Int64, len(fleets))
	for id := range fleets {
		calls[id] = &atomic.Int64{}
	}
	return &fleetByRegion{fleets: fleets, calls: calls}
}

func (f *fleetByRegion) Fleet(_ context.Context, region regions.Region) ([]obaapi.Vehicle, error) {
	f.calls[region.ID].Add(1)
	return f.fleets[region.ID], nil
}

// ArrivalAndDeparture is unused by these tests; it exists only so
// fleetByRegion still satisfies obaapi.Client.
func (f *fleetByRegion) ArrivalAndDeparture(context.Context, regions.Region, obaapi.DepartureQuery) (obaapi.Departure, error) {
	return obaapi.Departure{}, nil
}

func newTestService(t *testing.T, oba obaapi.Client) *Service {
	t.Helper()
	now := func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }
	return NewService(oba,
		cache.New[[]obaapi.Vehicle](30*time.Minute, 8, time.Second, now),
		cache.New[[]Match](5*time.Minute, 64, time.Second, now),
		slog.New(slog.DiscardHandler),
	)
}

// TestServiceIsolatesRegionsByID is the regression test for the critical
// finding that nothing defended the region id component of either cache
// key: with it dropped, one region's cached fleet or search results leak
// into another's response on this unauthenticated endpoint. The two
// sub-tests are written so a mutation dropping the region id from *either*
// key is caught: the first uses the same query string for two regions (so a
// query-cache key that forgets the region id collides), and the second uses
// a distinct query per region (so the query cache cannot mask a broken
// fleet-cache key -- a collision there serves region A's fleet to region
// B's filter, which then matches nothing).
func TestServiceIsolatesRegionsByID(t *testing.T) {
	regionA := regions.Region{ID: 1, Name: "A"}
	regionB := regions.Region{ID: 2, Name: "B"}
	oba := newFleetByRegion(map[int64][]obaapi.Vehicle{
		regionA.ID: {{AgencyID: "1", AgencyName: "A Transit", VehicleID: "1_4361"}},
		regionB.ID: {{AgencyID: "2", AgencyName: "B Transit", VehicleID: "2_4361"}},
	})
	svc := newTestService(t, oba)
	ctx := context.Background()

	t.Run("query cache key includes region id", func(t *testing.T) {
		gotA, err := svc.Search(ctx, regionA, "436")
		if err != nil {
			t.Fatalf("Search(region A): %v", err)
		}
		gotB, err := svc.Search(ctx, regionB, "436")
		if err != nil {
			t.Fatalf("Search(region B): %v", err)
		}
		if len(gotA) != 1 || gotA[0].VehicleID != "1_4361" {
			t.Fatalf("region A = %+v, want just 1_4361", gotA)
		}
		if len(gotB) != 1 || gotB[0].VehicleID != "2_4361" {
			t.Fatalf("region B = %+v, want just 2_4361 (region A's cached result leaked in if this fails)", gotB)
		}
	})

	t.Run("fleet cache key includes region id", func(t *testing.T) {
		// A different query string than the sub-test above: the query-result
		// cache cannot possibly short-circuit this lookup, so a match here
		// can only come from the fleet cache returning the right fleet.
		gotB, err := svc.Search(ctx, regionB, "2_43")
		if err != nil {
			t.Fatalf("Search(region B, distinct query): %v", err)
		}
		if len(gotB) != 1 || gotB[0].VehicleID != "2_4361" {
			t.Fatalf("region B = %+v, want just 2_4361 (empty means region B was filtered against region A's cached fleet)", gotB)
		}
	})
}

// TestServiceCachesFleetAcrossQueries is the regression test for the fleet
// cache existing at all: two searches against the same region, with
// different query strings (so the query-result cache cannot be why the
// second one is fast), must still fetch the upstream fleet only once.
func TestServiceCachesFleetAcrossQueries(t *testing.T) {
	region := regions.Region{ID: 1, Name: "A"}
	oba := newFleetByRegion(map[int64][]obaapi.Vehicle{
		region.ID: {
			{AgencyID: "1", AgencyName: "A", VehicleID: "1_4361"},
			{AgencyID: "1", AgencyName: "A", VehicleID: "1_9999"},
		},
	})
	svc := newTestService(t, oba)
	ctx := context.Background()

	if _, err := svc.Search(ctx, region, "436"); err != nil {
		t.Fatalf("Search #1: %v", err)
	}
	if _, err := svc.Search(ctx, region, "999"); err != nil {
		t.Fatalf("Search #2: %v", err)
	}
	if got := oba.calls[region.ID].Load(); got != 1 {
		t.Errorf("fleet fetched %d times for two distinct queries in the same region, want 1 (fleet cache not in effect)", got)
	}
}
