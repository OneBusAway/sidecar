package ghostbus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/obaapi"
	"github.com/OneBusAway/sidecar/internal/regions"
)

// snapBase is a fixed instant every test schedules against, so the
// scheduler's Now func never touches the wall clock.
var snapBase = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

const snapTestRegionID = 1

var snapTestRegion = regions.Region{ID: snapTestRegionID, Name: "Test Region", Active: true}

// --- fakes ---------------------------------------------------------------

// snapCall records one call the scheduler made against fakeSnapRepo's
// terminal methods, so tests can assert exactly which ones fired (and, just
// as importantly, which did not).
type snapCall struct {
	method       string // "captured", "unavailable", or "failure"
	id           int64
	snapshotJSON string
}

// fakeSnapRepo is a slice-backed Repository recording every terminal call.
// CheckAll is sequential (no goroutines), so no locking is needed.
type fakeSnapRepo struct {
	reports       []Report
	calls         []snapCall
	failureReturn int64
	failureErr    error
}

func (r *fakeSnapRepo) Create(context.Context, NewReport, time.Time) (Report, error) {
	panic("not used by the scheduler")
}

func (r *fakeSnapRepo) ListPendingSnapshots(_ context.Context, limit int64) ([]Report, error) {
	if int64(len(r.reports)) > limit {
		return r.reports[:limit], nil
	}
	return r.reports, nil
}

func (r *fakeSnapRepo) MarkSnapshotCaptured(_ context.Context, id int64, snapshotJSON string, _ time.Time) error {
	r.calls = append(r.calls, snapCall{method: "captured", id: id, snapshotJSON: snapshotJSON})
	return nil
}

func (r *fakeSnapRepo) MarkSnapshotUnavailable(_ context.Context, id int64, _ time.Time) error {
	r.calls = append(r.calls, snapCall{method: "unavailable", id: id})
	return nil
}

func (r *fakeSnapRepo) RecordSnapshotFailure(_ context.Context, id int64, _ time.Time) (int64, error) {
	r.calls = append(r.calls, snapCall{method: "failure", id: id})
	return r.failureReturn, r.failureErr
}

func (r *fakeSnapRepo) ListForExport(context.Context, int64, int64) ([]Report, error) {
	panic("not used by the scheduler")
}

func (r *fakeSnapRepo) GetByPublicID(context.Context, int64, string) (Report, error) {
	panic("not used by the scheduler")
}

func (r *fakeSnapRepo) callsOf(method string) []snapCall {
	var out []snapCall
	for _, c := range r.calls {
		if c.method == method {
			out = append(out, c)
		}
	}
	return out
}

// fakeSnapRegions is a scriptable regions.Repository; only Get is exercised
// by the scheduler, so every other method panics if the scheduler ever
// calls it.
type fakeSnapRegions struct {
	fn    func(ctx context.Context, id int64) (regions.Region, error)
	calls int
}

func (f *fakeSnapRegions) Get(ctx context.Context, id int64) (regions.Region, error) {
	f.calls++
	return f.fn(ctx, id)
}

func (f *fakeSnapRegions) List(context.Context) ([]regions.Region, error) {
	panic("not used by the scheduler")
}

func (f *fakeSnapRegions) UpsertFromDirectory(context.Context, []regions.Region, time.Time) error {
	panic("not used by the scheduler")
}

func (f *fakeSnapRegions) SetLocalFields(context.Context, int64, regions.LocalFields, time.Time) error {
	panic("not used by the scheduler")
}

// fakeTripDetailsSource implements TripDetailsSource with an injectable
// func so each test can script the upstream response, and records every
// query it was asked.
type fakeTripDetailsSource struct {
	fn      func(ctx context.Context, region regions.Region, q obaapi.TripDetailsQuery) (json.RawMessage, error)
	queries []obaapi.TripDetailsQuery
}

func (f *fakeTripDetailsSource) TripDetails(ctx context.Context, region regions.Region, q obaapi.TripDetailsQuery) (json.RawMessage, error) {
	f.queries = append(f.queries, q)
	return f.fn(ctx, region, q)
}

// --- helpers ---------------------------------------------------------------

// snapFixture builds a distinct pending Report for regionID, with every
// identity field derived from id so TestSnapshotSchedulerPassesReportIdentity
// can tell fields apart.
func snapFixture(id, regionID int64) Report {
	return Report{
		ID:              id,
		RegionID:        regionID,
		PublicID:        fmt.Sprintf("pub-%d", id),
		TripIdentifier:  fmt.Sprintf("1_trip%d", id),
		ServiceDate:     1700000000000 + id,
		RouteIdentifier: fmt.Sprintf("1_route%d", id),
		StopIdentifier:  fmt.Sprintf("1_stop%d", id),
		SnapshotStatus:  SnapshotPending,
	}
}

func testSnapLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newSnapScheduler(repo Repository, regionsRepo regions.Repository, oba TripDetailsSource) *SnapshotScheduler {
	return &SnapshotScheduler{
		Repo:    repo,
		Regions: regionsRepo,
		OBA:     oba,
		Now:     func() time.Time { return snapBase },
		Logger:  testSnapLogger(),
	}
}

func singleRegionFake() *fakeSnapRegions {
	return &fakeSnapRegions{fn: func(_ context.Context, id int64) (regions.Region, error) {
		if id != snapTestRegionID {
			return regions.Region{}, regions.ErrNotFound
		}
		return snapTestRegion, nil
	}}
}

// --- tests -------------------------------------------------------------

func TestSnapshotSchedulerCaptures(t *testing.T) {
	t.Parallel()
	rep := snapFixture(1, snapTestRegionID)
	repo := &fakeSnapRepo{reports: []Report{rep}}
	wantJSON := json.RawMessage(`{"current_time":1,"status":{"phase":"x"}}`)
	oba := &fakeTripDetailsSource{fn: func(context.Context, regions.Region, obaapi.TripDetailsQuery) (json.RawMessage, error) {
		return wantJSON, nil
	}}
	s := newSnapScheduler(repo, singleRegionFake(), oba)

	s.CheckAll(context.Background())

	captured := repo.callsOf("captured")
	if len(captured) != 1 {
		t.Fatalf("captured calls = %d; want 1", len(captured))
	}
	if captured[0].id != rep.ID {
		t.Errorf("captured id = %d; want %d", captured[0].id, rep.ID)
	}
	if captured[0].snapshotJSON != string(wantJSON) {
		t.Errorf("captured JSON = %q; want %q", captured[0].snapshotJSON, string(wantJSON))
	}
	if len(repo.callsOf("failure")) != 0 {
		t.Errorf("failure calls = %d; want 0", len(repo.callsOf("failure")))
	}
	if len(repo.callsOf("unavailable")) != 0 {
		t.Errorf("unavailable calls = %d; want 0", len(repo.callsOf("unavailable")))
	}
}

func TestSnapshotSchedulerNotFoundIsUnavailable(t *testing.T) {
	t.Parallel()
	rep := snapFixture(1, snapTestRegionID)
	repo := &fakeSnapRepo{reports: []Report{rep}}
	oba := &fakeTripDetailsSource{fn: func(context.Context, regions.Region, obaapi.TripDetailsQuery) (json.RawMessage, error) {
		return nil, obaapi.ErrNotFound
	}}
	s := newSnapScheduler(repo, singleRegionFake(), oba)

	s.CheckAll(context.Background())

	if len(repo.callsOf("unavailable")) != 1 {
		t.Fatalf("unavailable calls = %d; want 1", len(repo.callsOf("unavailable")))
	}
	if len(repo.callsOf("failure")) != 0 {
		t.Errorf("failure calls = %d; want 0 (a definitive miss never burns a retry)", len(repo.callsOf("failure")))
	}
}

func TestSnapshotSchedulerNoKeyIsUnavailable(t *testing.T) {
	t.Parallel()
	rep := snapFixture(1, snapTestRegionID)
	repo := &fakeSnapRepo{reports: []Report{rep}}
	oba := &fakeTripDetailsSource{fn: func(context.Context, regions.Region, obaapi.TripDetailsQuery) (json.RawMessage, error) {
		return nil, obaapi.ErrNotConfigured
	}}
	s := newSnapScheduler(repo, singleRegionFake(), oba)

	s.CheckAll(context.Background())

	if len(repo.callsOf("unavailable")) != 1 {
		t.Fatalf("unavailable calls = %d; want 1", len(repo.callsOf("unavailable")))
	}
	if len(repo.callsOf("failure")) != 0 {
		t.Errorf("failure calls = %d; want 0", len(repo.callsOf("failure")))
	}
}

func TestSnapshotSchedulerTransientCountsFailure(t *testing.T) {
	t.Parallel()
	rep := snapFixture(1, snapTestRegionID)
	repo := &fakeSnapRepo{reports: []Report{rep}}
	oba := &fakeTripDetailsSource{fn: func(context.Context, regions.Region, obaapi.TripDetailsQuery) (json.RawMessage, error) {
		return nil, errors.New("boom")
	}}
	s := newSnapScheduler(repo, singleRegionFake(), oba)

	s.CheckAll(context.Background())

	if len(repo.callsOf("failure")) != 1 {
		t.Fatalf("failure calls = %d; want 1", len(repo.callsOf("failure")))
	}
	if len(repo.callsOf("captured")) != 0 {
		t.Errorf("captured calls = %d; want 0", len(repo.callsOf("captured")))
	}
	if len(repo.callsOf("unavailable")) != 0 {
		t.Errorf("unavailable calls = %d; want 0 (the repository owns the cap flip, not the scheduler)", len(repo.callsOf("unavailable")))
	}
}

func TestSnapshotSchedulerRegionGoneIsUnavailable(t *testing.T) {
	t.Parallel()
	rep := snapFixture(1, snapTestRegionID)
	repo := &fakeSnapRepo{reports: []Report{rep}}
	regionsFake := &fakeSnapRegions{fn: func(context.Context, int64) (regions.Region, error) {
		return regions.Region{}, regions.ErrNotFound
	}}
	oba := &fakeTripDetailsSource{fn: func(context.Context, regions.Region, obaapi.TripDetailsQuery) (json.RawMessage, error) {
		t.Fatal("OBA must not be called when the region is gone")
		return nil, nil
	}}
	s := newSnapScheduler(repo, regionsFake, oba)

	s.CheckAll(context.Background())

	if len(repo.callsOf("unavailable")) != 1 {
		t.Fatalf("unavailable calls = %d; want 1", len(repo.callsOf("unavailable")))
	}
	if len(oba.queries) != 0 {
		t.Errorf("OBA queries = %d; want 0", len(oba.queries))
	}
}

func TestSnapshotSchedulerRegionStoreErrorSkips(t *testing.T) {
	t.Parallel()
	rep := snapFixture(1, snapTestRegionID)
	repo := &fakeSnapRepo{reports: []Report{rep}}
	regionsFake := &fakeSnapRegions{fn: func(context.Context, int64) (regions.Region, error) {
		return regions.Region{}, errors.New("db locked")
	}}
	oba := &fakeTripDetailsSource{fn: func(context.Context, regions.Region, obaapi.TripDetailsQuery) (json.RawMessage, error) {
		t.Fatal("OBA must not be called on a region store error")
		return nil, nil
	}}
	s := newSnapScheduler(repo, regionsFake, oba)

	s.CheckAll(context.Background())

	if len(repo.calls) != 0 {
		t.Fatalf("repo calls = %v; want none (a store hiccup must leave the report pending)", repo.calls)
	}
}

func TestSnapshotSchedulerRegionFetchedOncePerCycle(t *testing.T) {
	t.Parallel()
	reports := []Report{
		snapFixture(1, snapTestRegionID),
		snapFixture(2, snapTestRegionID),
		snapFixture(3, snapTestRegionID),
	}
	repo := &fakeSnapRepo{reports: reports}
	regionsFake := singleRegionFake()
	oba := &fakeTripDetailsSource{fn: func(context.Context, regions.Region, obaapi.TripDetailsQuery) (json.RawMessage, error) {
		return json.RawMessage(`{}`), nil
	}}
	s := newSnapScheduler(repo, regionsFake, oba)

	s.CheckAll(context.Background())

	if regionsFake.calls != 1 {
		t.Errorf("region Get calls = %d; want 1 (per-cycle cache)", regionsFake.calls)
	}
	if len(repo.callsOf("captured")) != 3 {
		t.Errorf("captured calls = %d; want 3", len(repo.callsOf("captured")))
	}
}

func TestSnapshotSchedulerPassesReportIdentity(t *testing.T) {
	t.Parallel()
	rep := snapFixture(42, snapTestRegionID)
	repo := &fakeSnapRepo{reports: []Report{rep}}
	oba := &fakeTripDetailsSource{fn: func(context.Context, regions.Region, obaapi.TripDetailsQuery) (json.RawMessage, error) {
		return json.RawMessage(`{}`), nil
	}}
	s := newSnapScheduler(repo, singleRegionFake(), oba)

	s.CheckAll(context.Background())

	if len(oba.queries) != 1 {
		t.Fatalf("OBA queries = %d; want 1", len(oba.queries))
	}
	got := oba.queries[0]
	want := obaapi.TripDetailsQuery{
		TripID:      rep.TripIdentifier,
		ServiceDate: rep.ServiceDate,
		RouteID:     rep.RouteIdentifier,
		StopID:      rep.StopIdentifier,
	}
	if got != want {
		t.Errorf("query = %+v; want %+v", got, want)
	}
}
