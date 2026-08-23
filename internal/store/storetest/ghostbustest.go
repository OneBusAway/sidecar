package storetest

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/ghostbus"
	"github.com/OneBusAway/sidecar/internal/regions"
)

// newGhostBusStoreFunc is shorthand for the callback every ghost bus subtest
// receives: a fresh, migrated pair of repositories backed by the same
// underlying store.
type newGhostBusStoreFunc func(*testing.T) (ghostbus.Repository, regions.Repository)

// RunGhostBusRepository exercises a ghostbus.Repository against the
// behavioral contract every engine must satisfy.
func RunGhostBusRepository(t *testing.T, newStore func(*testing.T) (ghostbus.Repository, regions.Repository)) {
	t.Helper()
	t.Run("CreateRoundTrip", func(t *testing.T) { testGhostBusCreateRoundTrip(t, newStore) })
	t.Run("DuplicateReturnsErrDuplicate", func(t *testing.T) { testGhostBusDuplicate(t, newStore) })
	t.Run("ConcurrentDuplicateOneWins", func(t *testing.T) { testGhostBusConcurrentDuplicate(t, newStore) })
	t.Run("TokenCollisionIsNotDuplicate", func(t *testing.T) { testGhostBusTokenCollision(t, newStore) })
	t.Run("DedupeScope", func(t *testing.T) { testGhostBusDedupeScope(t, newStore) })
	t.Run("PendingSnapshotPoll", func(t *testing.T) { testGhostBusPendingPoll(t, newStore) })
	t.Run("FailureCapMarksUnavailable", func(t *testing.T) { testGhostBusFailureCap(t, newStore) })
	t.Run("CaptureRoundTrip", func(t *testing.T) { testGhostBusCaptureRoundTrip(t, newStore) })
	t.Run("ExportSinceFilter", func(t *testing.T) { testGhostBusExportSince(t, newStore) })
}

// createGhostBusRegion inserts a region row for ghost bus fixtures, using
// putStoretestRegion's shared pattern (see pushregtest.go), and returns its
// id. Every ghost bus subtest needs a region to satisfy
// ghost_bus_reports.region_id's foreign key before it can write a report.
func createGhostBusRegion(t *testing.T, regs regions.Repository) int64 {
	t.Helper()
	const id = 1
	putStoretestRegion(t, regs, id)
	return id
}

func ghostBusFixture(regionID int64, publicID, user, trip string, serviceDate int64) ghostbus.NewReport {
	return ghostbus.NewReport{
		RegionID: regionID, PublicID: publicID, UserIdentifier: user,
		TripIdentifier: trip, ServiceDate: serviceDate, WaitDurationMinutes: 15,
	}
}

func testGhostBusCreateRoundTrip(t *testing.T, newStore newGhostBusStoreFunc) {
	repo, regionsRepo := newStore(t)
	ctx := context.Background()
	regionID := createGhostBusRegion(t, regionsRepo) // same pattern as the pushreg suite's fixture

	pred := true
	seq := int64(3)
	lat, lon := 47.6097, -122.3422
	sched := int64(1756000000000)
	in := ghostBusFixture(regionID, "tok_roundtrip_0000000001", "user-a", "1_604370", 1754809200000)
	in.RouteIdentifier, in.StopIdentifier, in.VehicleIdentifier = "1_44", "1_570", "1_4361"
	in.StopSequence, in.Predicted, in.Comment = &seq, &pred, "never showed"
	in.UserLatitude, in.UserLongitude = &lat, &lon
	in.ScheduledArrivalAt = &sched

	got, err := repo.Create(ctx, in, base)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got.ID == 0 || got.PublicID != in.PublicID || got.SnapshotStatus != ghostbus.SnapshotPending {
		t.Errorf("Create returned %+v; want assigned ID, same PublicID, pending snapshot", got)
	}
	if got.Predicted == nil || !*got.Predicted || got.StopSequence == nil || *got.StopSequence != 3 {
		t.Errorf("pointer fields did not round-trip: %+v", got)
	}
	if got.ServiceDate != 1754809200000 || got.ScheduledArrivalAt == nil || *got.ScheduledArrivalAt != sched {
		t.Errorf("epoch-ms fields did not round-trip: %+v", got)
	}
	if !got.CreatedAt.Equal(base) {
		t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, base)
	}
}

func testGhostBusDuplicate(t *testing.T, newStore newGhostBusStoreFunc) {
	repo, regionsRepo := newStore(t)
	ctx := context.Background()
	regionID := createGhostBusRegion(t, regionsRepo)
	first := ghostBusFixture(regionID, "tok_dup_a_00000000001", "user-a", "trip-1", 1000)
	if _, err := repo.Create(ctx, first, base); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	second := ghostBusFixture(regionID, "tok_dup_b_00000000002", "user-a", "trip-1", 1000)
	_, err := repo.Create(ctx, second, base)
	if !errors.Is(err, ghostbus.ErrDuplicate) {
		t.Fatalf("duplicate Create err = %v, want ErrDuplicate", err)
	}
}

func testGhostBusConcurrentDuplicate(t *testing.T, newStore newGhostBusStoreFunc) {
	repo, regionsRepo := newStore(t)
	ctx := context.Background()
	regionID := createGhostBusRegion(t, regionsRepo)
	errs := make(chan error, 2)
	for i := range 2 {
		in := ghostBusFixture(regionID, fmt.Sprintf("tok_race_%016d", i), "user-a", "trip-1", 1000)
		go func() {
			_, err := repo.Create(ctx, in, base)
			errs <- err
		}()
	}
	var okCount, dupCount int
	for range 2 {
		switch err := <-errs; {
		case err == nil:
			okCount++
		case errors.Is(err, ghostbus.ErrDuplicate):
			dupCount++
		default:
			t.Fatalf("racing Create err = %v, want nil or ErrDuplicate", err)
		}
	}
	if okCount != 1 || dupCount != 1 {
		t.Fatalf("race outcome ok=%d dup=%d, want exactly one winner and one ErrDuplicate", okCount, dupCount)
	}
}

func testGhostBusTokenCollision(t *testing.T, newStore newGhostBusStoreFunc) {
	repo, regionsRepo := newStore(t)
	ctx := context.Background()
	regionID := createGhostBusRegion(t, regionsRepo)
	first := ghostBusFixture(regionID, "tok_same_000000000001", "user-a", "trip-1", 1000)
	if _, err := repo.Create(ctx, first, base); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	// Different dedupe key, same public identifier: this must NOT read as
	// already_reported -- a rider's first-ever report would be rejected.
	second := ghostBusFixture(regionID, "tok_same_000000000001", "user-b", "trip-2", 2000)
	_, err := repo.Create(ctx, second, base)
	if !errors.Is(err, ghostbus.ErrTokenCollision) {
		t.Fatalf("collision Create err = %v, want ErrTokenCollision", err)
	}
}

func testGhostBusDedupeScope(t *testing.T, newStore newGhostBusStoreFunc) {
	repo, regionsRepo := newStore(t)
	ctx := context.Background()
	regionID := createGhostBusRegion(t, regionsRepo)
	seed := ghostBusFixture(regionID, "tok_scope_00000000001", "user-a", "trip-1", 1000)
	if _, err := repo.Create(ctx, seed, base); err != nil {
		t.Fatalf("seed Create: %v", err)
	}
	// Each varies exactly one dedupe component; all must succeed.
	variants := []ghostbus.NewReport{
		ghostBusFixture(regionID, "tok_scope_00000000002", "user-b", "trip-1", 1000),
		ghostBusFixture(regionID, "tok_scope_00000000003", "user-a", "trip-2", 1000),
		ghostBusFixture(regionID, "tok_scope_00000000004", "user-a", "trip-1", 1001),
	}
	for i, in := range variants {
		if _, err := repo.Create(ctx, in, base); err != nil {
			t.Errorf("variant %d Create err = %v, want nil", i, err)
		}
	}
}

func testGhostBusPendingPoll(t *testing.T, newStore newGhostBusStoreFunc) {
	repo, regionsRepo := newStore(t)
	ctx := context.Background()
	regionID := createGhostBusRegion(t, regionsRepo)
	var ids []int64
	for i := range 3 {
		in := ghostBusFixture(regionID, fmt.Sprintf("tok_poll_%013d", i), "user-a", fmt.Sprintf("trip-%d", i), 1000)
		rep, err := repo.Create(ctx, in, base)
		if err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
		ids = append(ids, rep.ID)
	}
	if err := repo.MarkSnapshotCaptured(ctx, ids[0], `{"status":{}}`, base); err != nil {
		t.Fatalf("MarkSnapshotCaptured: %v", err)
	}
	// Exhaust the second report's attempts: at the cap it must stop
	// matching the poll even though the crash-window row would still say
	// pending -- the final increment flips the status itself.
	for range ghostbus.MaxSnapshotAttempts {
		if _, err := repo.RecordSnapshotFailure(ctx, ids[1], base); err != nil {
			t.Fatalf("RecordSnapshotFailure: %v", err)
		}
	}
	pending, err := repo.ListPendingSnapshots(ctx, 10)
	if err != nil {
		t.Fatalf("ListPendingSnapshots: %v", err)
	}
	if len(pending) != 1 || pending[0].ID != ids[2] {
		t.Fatalf("pending = %+v, want exactly the untouched report %d", pending, ids[2])
	}
}

func testGhostBusFailureCap(t *testing.T, newStore newGhostBusStoreFunc) {
	repo, regionsRepo := newStore(t)
	ctx := context.Background()
	regionID := createGhostBusRegion(t, regionsRepo)
	rep, err := repo.Create(ctx, ghostBusFixture(regionID, "tok_cap_000000000001", "user-a", "trip-1", 1000), base)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for want := int64(1); want < ghostbus.MaxSnapshotAttempts; want++ {
		n, err := repo.RecordSnapshotFailure(ctx, rep.ID, base)
		if err != nil || n != want {
			t.Fatalf("failure %d: n=%d err=%v", want, n, err)
		}
		if p, _ := repo.ListPendingSnapshots(ctx, 10); len(p) != 1 {
			t.Fatalf("after failure %d report should still be pollable", want)
		}
	}
	n, err := repo.RecordSnapshotFailure(ctx, rep.ID, base)
	if err != nil || n != ghostbus.MaxSnapshotAttempts {
		t.Fatalf("final failure: n=%d err=%v", n, err)
	}
	if p, _ := repo.ListPendingSnapshots(ctx, 10); len(p) != 0 {
		t.Fatalf("report at the cap must not be pollable; got %+v", p)
	}
	exported, err := repo.ListForExport(ctx, regionID, 0)
	if err != nil || len(exported) != 1 {
		t.Fatalf("export: %v / %d rows", err, len(exported))
	}
	if exported[0].SnapshotStatus != ghostbus.SnapshotUnavailable {
		t.Fatalf("status after cap = %q, want unavailable", exported[0].SnapshotStatus)
	}
}

func testGhostBusCaptureRoundTrip(t *testing.T, newStore newGhostBusStoreFunc) {
	repo, regionsRepo := newStore(t)
	ctx := context.Background()
	regionID := createGhostBusRegion(t, regionsRepo)
	rep, err := repo.Create(ctx, ghostBusFixture(regionID, "tok_cap2_00000000001", "user-a", "trip-1", 1000), base)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	snap := `{"current_time":1,"status":{"phase":"in_progress"}}`
	capturedAt := base.Add(45 * time.Second)
	if err := repo.MarkSnapshotCaptured(ctx, rep.ID, snap, capturedAt); err != nil {
		t.Fatalf("MarkSnapshotCaptured: %v", err)
	}
	got, err := repo.ListForExport(ctx, regionID, 0)
	if err != nil || len(got) != 1 {
		t.Fatalf("export: %v / %d rows", err, len(got))
	}
	r := got[0]
	if r.SnapshotStatus != ghostbus.SnapshotCaptured || r.SnapshotJSON != snap {
		t.Errorf("captured row = %+v", r)
	}
	if r.SnapshotCapturedAt == nil || !r.SnapshotCapturedAt.Equal(capturedAt) {
		t.Errorf("SnapshotCapturedAt = %v, want %v", r.SnapshotCapturedAt, capturedAt)
	}
}

func testGhostBusExportSince(t *testing.T, newStore newGhostBusStoreFunc) {
	repo, regionsRepo := newStore(t)
	ctx := context.Background()
	regionID := createGhostBusRegion(t, regionsRepo)
	early := ghostBusFixture(regionID, "tok_since_0000000001", "user-a", "trip-1", 1000)
	late := ghostBusFixture(regionID, "tok_since_0000000002", "user-a", "trip-2", 1000)
	if _, err := repo.Create(ctx, early, base); err != nil {
		t.Fatalf("early Create: %v", err)
	}
	if _, err := repo.Create(ctx, late, base.Add(2*time.Hour)); err != nil {
		t.Fatalf("late Create: %v", err)
	}
	got, err := repo.ListForExport(ctx, regionID, base.Add(time.Hour).Unix())
	if err != nil {
		t.Fatalf("ListForExport: %v", err)
	}
	if len(got) != 1 || got[0].PublicID != late.PublicID {
		t.Fatalf("since filter returned %+v, want only the late report", got)
	}
	all, err := repo.ListForExport(ctx, regionID, 0)
	if err != nil || len(all) != 2 {
		t.Fatalf("since=0 returned %d rows err=%v, want 2", len(all), err)
	}
}
