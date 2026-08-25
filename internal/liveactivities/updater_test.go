package liveactivities_test

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/liveactivities"
	"github.com/OneBusAway/sidecar/internal/obaapi"
	"github.com/OneBusAway/sidecar/internal/push"
	"github.com/OneBusAway/sidecar/internal/regions"
)

// --- fakes ---------------------------------------------------------------

type fakeRepo struct {
	mu   sync.Mutex
	rows map[int64]liveactivities.LiveActivity
}

func newFakeRepo(rows ...liveactivities.LiveActivity) *fakeRepo {
	r := &fakeRepo{rows: map[int64]liveactivities.LiveActivity{}}
	for _, la := range rows {
		r.rows[la.ID] = la
	}
	return r
}

func (r *fakeRepo) Upsert(context.Context, liveactivities.NewLiveActivity, time.Time) (liveactivities.LiveActivity, error) {
	return liveactivities.LiveActivity{}, errors.New("not implemented")
}
func (r *fakeRepo) Delete(context.Context, int64, string) error { return liveactivities.ErrNotFound }
func (r *fakeRepo) DeleteByID(_ context.Context, id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.rows, id)
	return nil
}
func (r *fakeRepo) DeleteByPushToken(context.Context, string) (int64, error) { return 0, nil }
func (r *fakeRepo) List(context.Context) ([]liveactivities.LiveActivity, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]liveactivities.LiveActivity, 0, len(r.rows))
	for _, la := range r.rows {
		out = append(out, la)
	}
	return out, nil
}
func (r *fakeRepo) RecordFailure(_ context.Context, id int64) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	la, ok := r.rows[id]
	if !ok {
		return 0, liveactivities.ErrNotFound
	}
	la.ConsecutiveFailures++
	r.rows[id] = la
	return la.ConsecutiveFailures, nil
}
func (r *fakeRepo) ResetFailures(_ context.Context, id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	la := r.rows[id]
	la.ConsecutiveFailures = 0
	r.rows[id] = la
	return nil
}
func (r *fakeRepo) RecordPush(_ context.Context, id int64, state liveactivities.ContentState, at time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	la := r.rows[id]
	la.LastContentState = state
	la.LastPushedAt = &at
	r.rows[id] = la
	return nil
}
func (r *fakeRepo) get(id int64) (liveactivities.LiveActivity, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	la, ok := r.rows[id]
	return la, ok
}

type fakeRegions struct {
	regions map[int64]regions.Region
	err     error // non-nil: every Get fails with this
}

func (f fakeRegions) Get(_ context.Context, id int64) (regions.Region, error) {
	if f.err != nil {
		return regions.Region{}, f.err
	}
	r, ok := f.regions[id]
	if !ok {
		return regions.Region{}, regions.ErrNotFound
	}
	return r, nil
}
func (f fakeRegions) List(context.Context) ([]regions.Region, error) { return nil, nil }
func (f fakeRegions) UpsertFromDirectory(context.Context, []regions.Region, time.Time) error {
	return nil
}
func (f fakeRegions) SetLocalFields(context.Context, int64, regions.LocalFields, time.Time) error {
	return nil
}

type fakeOBA struct {
	mu    sync.Mutex
	calls int
	fn    func(q obaapi.StopArrivalsQuery) ([]obaapi.StopArrival, error)
}

func (f *fakeOBA) ArrivalsAndDeparturesForStop(_ context.Context, _ regions.Region, q obaapi.StopArrivalsQuery) ([]obaapi.StopArrival, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	return f.fn(q)
}
func (f *fakeOBA) count() int { f.mu.Lock(); defer f.mu.Unlock(); return f.calls }

type fakeSender struct {
	mu   sync.Mutex
	sent []push.LiveActivityPush
	err  error
}

func (s *fakeSender) SendLiveActivity(_ context.Context, p push.LiveActivityPush) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.sent = append(s.sent, p)
	return nil
}
func (s *fakeSender) pushes() []push.LiveActivityPush {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]push.LiveActivityPush(nil), s.sent...)
}

// clock is an advancing fake: a fixed Now would never expire the stop cache
// or advance the push timestamp (design spec §8).
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) advance(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

var base = time.Date(2026, 1, 9, 18, 0, 0, 0, time.UTC)

func upcoming(offset time.Duration) obaapi.StopArrival {
	// entry() (contentstate_test.go) measures offsets from that file's `now`;
	// folding the difference into the offset rebases the entry onto base.
	return entry(base.Sub(now)+offset, 0)
}

func activity(id int64, stop string) liveactivities.LiveActivity {
	return liveactivities.LiveActivity{
		ID: id, RegionID: 1, Token: "tok", ActivityID: "act", PushToken: "push-" + stop, APNSSandbox: true,
		StopID: stop, RouteShortName: "44", TripHeadsign: "Ballard",
		LastContentState: liveactivities.EmptyContentState(), ExpiresAt: base.Add(8 * time.Hour),
	}
}

type harness struct {
	repo   *fakeRepo
	oba    *fakeOBA
	sender *fakeSender
	clk    *clock
	u      *liveactivities.Updater
}

func newHarness(t *testing.T, sender *fakeSender, rows ...liveactivities.LiveActivity) *harness {
	t.Helper()
	h := &harness{
		repo: newFakeRepo(rows...),
		oba: &fakeOBA{fn: func(obaapi.StopArrivalsQuery) ([]obaapi.StopArrival, error) {
			return []obaapi.StopArrival{upcoming(5 * time.Minute)}, nil
		}},
		sender: sender,
		clk:    &clock{t: base},
	}
	var s push.LiveActivitySender
	if sender != nil {
		s = sender
	}
	h.u = liveactivities.NewUpdater(h.repo,
		fakeRegions{regions: map[int64]regions.Region{1: {ID: 1, OBABaseURL: "https://example.org/", OBAAPIKey: "k"}}},
		h.oba, s, h.clk.now, slog.New(slog.DiscardHandler))
	return h
}

// cycle runs one CheckAll after advancing the clock one minute, like the
// production ticker.
func (h *harness) cycle() { h.clk.advance(time.Minute); h.u.CheckAll(context.Background()) }

// --- tests ---------------------------------------------------------------

func TestFirstCyclePushesUpdateWithStaleDateAndRecords(t *testing.T) {
	s := &fakeSender{}
	h := newHarness(t, s, activity(1, "1_570"))
	h.cycle()
	p := s.pushes()
	if len(p) != 1 {
		t.Fatalf("pushes = %d, want 1", len(p))
	}
	if p[0].Event != "update" || p[0].Token != "push-1_570" || !p[0].Sandbox {
		t.Errorf("push = %+v", p[0])
	}
	if !p[0].StaleDate.Equal(h.clk.now().Add(liveactivities.StaleAfter)) || !p[0].DismissalDate.IsZero() {
		t.Errorf("stale=%v dismissal=%v", p[0].StaleDate, p[0].DismissalDate)
	}
	if !p[0].Timestamp.Equal(h.clk.now()) {
		t.Errorf("timestamp = %v, want %v", p[0].Timestamp, h.clk.now())
	}
	state, ok := p[0].ContentState.(liveactivities.ContentState)
	if !ok || len(state.Arrivals) != 1 {
		t.Errorf("content state = %#v", p[0].ContentState)
	}
	row, _ := h.repo.get(1)
	if row.LastPushedAt == nil || !row.LastPushedAt.Equal(h.clk.now()) || len(row.LastContentState.Arrivals) != 1 {
		t.Errorf("RecordPush not applied: %+v", row)
	}
}

func TestKeepaliveBoundaryAndUnchangedState(t *testing.T) {
	s := &fakeSender{}
	h := newHarness(t, s, activity(1, "1_570"))
	h.cycle() // first push at base+60s
	h.clk.advance(54 * time.Second)
	h.u.CheckAll(context.Background())
	if len(s.pushes()) != 1 {
		t.Fatalf("unchanged state at 54s must not push; got %d", len(s.pushes()))
	}
	h.clk.advance(time.Second) // 55s since last push
	h.u.CheckAll(context.Background())
	p := s.pushes()
	if len(p) != 2 {
		t.Fatalf("keepalive at 55s must push; got %d", len(p))
	}
	// Pins pushTimestamp's now-wins branch with a non-nil last: the clock
	// has advanced past LastPushedAt, so the keepalive's timestamp must be
	// exactly now, not last+1s.
	if !p[1].Timestamp.Equal(h.clk.now()) {
		t.Errorf("keepalive timestamp = %v, want now (%v)", p[1].Timestamp, h.clk.now())
	}
}

// TestChangedStateInsideKeepaliveWindowPushes is simplified per the brief's
// Step 4 fallback: the original choreography (advance past the stop cache
// TTL while staying inside the keepalive window) fights the cache in a way
// that's fragile to reason about. Instead: record a push at `now` with the
// EMPTY state (Changed is true against whatever comes back from the still-
// warm cache; the keepalive is not due), then run CheckAll without moving
// the clock and assert a second push happened.
func TestChangedStateInsideKeepaliveWindowPushes(t *testing.T) {
	s := &fakeSender{}
	h := newHarness(t, s, activity(1, "1_570"))
	h.cycle()
	_ = h.repo.RecordPush(context.Background(), 1, liveactivities.EmptyContentState(), h.clk.now())
	h.u.CheckAll(context.Background())
	if len(s.pushes()) != 2 {
		t.Fatalf("changed state must push even inside the keepalive window; got %d", len(s.pushes()))
	}
}

// TestTimestampAdvancesWhenClockDoesNot is simplified per the brief's Step 4
// fallback for the same reason as above: record a push at `now` with the
// EMPTY state (Changed true, keepalive not due), run CheckAll without
// advancing the clock, and assert the new push's Timestamp is now+1s.
func TestTimestampAdvancesWhenClockDoesNot(t *testing.T) {
	s := &fakeSender{}
	h := newHarness(t, s, activity(1, "1_570"))
	h.cycle()
	_ = h.repo.RecordPush(context.Background(), 1, liveactivities.EmptyContentState(), h.clk.now())
	h.u.CheckAll(context.Background())
	p := s.pushes()
	if len(p) != 2 {
		t.Fatalf("pushes = %d", len(p))
	}
	if !p[1].Timestamp.After(p[0].Timestamp) || !p[1].Timestamp.Equal(h.clk.now().Add(time.Second)) {
		t.Errorf("timestamp must be last_pushed_at+1s when the clock has not moved: got %v (prev %v, now %v)", p[1].Timestamp, p[0].Timestamp, h.clk.now())
	}
	// The stored watermark must not trail the timestamp APNs actually saw:
	// RecordPush must be called with ts (now+1s here), not the stalled now.
	row, ok := h.repo.get(1)
	if !ok || row.LastPushedAt == nil || !row.LastPushedAt.Equal(p[1].Timestamp) {
		t.Errorf("RecordPush watermark = %v, want %v (the pushed timestamp, not now)", row.LastPushedAt, p[1].Timestamp)
	}
}

func TestExpiredEndsWithEndPushThenDelete(t *testing.T) {
	s := &fakeSender{}
	la := activity(1, "1_570")
	la.ExpiresAt = base.Add(30 * time.Second)
	la.LastContentState = liveactivities.ContentState{Arrivals: []liveactivities.ArrivalInfo{{DepartureTime: 1, ScheduleStatus: "on_time"}}}
	h := newHarness(t, s, la)
	h.cycle()
	p := s.pushes()
	if len(p) != 1 || p[0].Event != "end" {
		t.Fatalf("pushes = %+v, want one end", p)
	}
	if !p[0].DismissalDate.Equal(h.clk.now().Add(liveactivities.DismissAfterEnd)) || !p[0].StaleDate.IsZero() {
		t.Errorf("end push dates: %+v", p[0])
	}
	if st := p[0].ContentState.(liveactivities.ContentState); len(st.Arrivals) != 1 {
		t.Errorf("end push must reuse last state: %+v", st)
	}
	if _, ok := h.repo.get(1); ok {
		t.Error("expired row must be deleted")
	}
	if h.oba.count() != 0 {
		t.Error("expiry must not fetch upstream")
	}
}

func TestThreeEmptyCyclesEndTwoDoNot(t *testing.T) {
	s := &fakeSender{}
	h := newHarness(t, s, activity(1, "1_570"))
	h.oba.fn = func(obaapi.StopArrivalsQuery) ([]obaapi.StopArrival, error) { return nil, nil }
	h.cycle()
	h.cycle()
	row, ok := h.repo.get(1)
	if !ok || row.ConsecutiveFailures != 2 || len(s.pushes()) != 0 {
		t.Fatalf("after 2 empties: ok=%v row=%+v pushes=%d", ok, row, len(s.pushes()))
	}
	h.cycle()
	if _, ok := h.repo.get(1); ok {
		t.Error("third empty cycle must end the activity")
	}
	if p := s.pushes(); len(p) != 1 || p[0].Event != "end" {
		t.Errorf("want one end push, got %+v", p)
	}
}

func TestFetchErrorsCountAndSuccessResets(t *testing.T) {
	s := &fakeSender{}
	h := newHarness(t, s, activity(1, "1_570"))
	h.oba.fn = func(obaapi.StopArrivalsQuery) ([]obaapi.StopArrival, error) { return nil, errors.New("boom") }
	h.cycle()
	h.oba.fn = func(obaapi.StopArrivalsQuery) ([]obaapi.StopArrival, error) { return nil, obaapi.ErrNotFound }
	h.cycle()
	if row, _ := h.repo.get(1); row.ConsecutiveFailures != 2 {
		t.Fatalf("transient and not-found must both count (spec §6.3): %+v", row)
	}
	h.oba.fn = func(obaapi.StopArrivalsQuery) ([]obaapi.StopArrival, error) {
		return []obaapi.StopArrival{upcoming(5 * time.Minute)}, nil
	}
	h.cycle()
	if row, _ := h.repo.get(1); row.ConsecutiveFailures != 0 {
		t.Errorf("success must reset: %+v", row)
	}
}

func TestSendFailureLeavesRowAndLastPushedAt(t *testing.T) {
	s := &fakeSender{err: errors.New("gorush down")}
	h := newHarness(t, s, activity(1, "1_570"))
	h.cycle()
	row, ok := h.repo.get(1)
	if !ok || row.LastPushedAt != nil || row.ConsecutiveFailures != 0 {
		t.Errorf("send failure must not record a push or count a failure: ok=%v %+v", ok, row)
	}
}

func TestEndPushFailureStillDeletes(t *testing.T) {
	s := &fakeSender{err: errors.New("gorush down")}
	la := activity(1, "1_570")
	la.ExpiresAt = base
	h := newHarness(t, s, la)
	h.cycle()
	if _, ok := h.repo.get(1); ok {
		t.Error("row must be deleted even when the end push fails (spec §6.4)")
	}
}

func TestStoreOnlyModeExpiresWithoutSending(t *testing.T) {
	la := activity(1, "1_570")
	la.ExpiresAt = base
	h := newHarness(t, nil, la, activity(2, "1_571"))
	h.cycle()
	if _, ok := h.repo.get(1); ok {
		t.Error("expired row must be deleted in store-only mode")
	}
	if row, _ := h.repo.get(2); row.LastPushedAt != nil {
		t.Error("store-only mode must not record pushes")
	}
}

func TestRegionErrors(t *testing.T) {
	s := &fakeSender{}
	h := newHarness(t, s, activity(1, "1_570"))
	h.u.Regions = fakeRegions{regions: map[int64]regions.Region{}} // region gone
	h.cycle()
	if row, _ := h.repo.get(1); row.ConsecutiveFailures != 1 {
		t.Errorf("ErrNotFound region must count: %+v", row)
	}
	h.u.Regions = fakeRegions{err: errors.New("db locked")}
	h.cycle()
	if row, _ := h.repo.get(1); row.ConsecutiveFailures != 1 {
		t.Errorf("transient region error must not count: %+v", row)
	}
	if h.oba.count() != 0 {
		t.Error("no upstream call without a region")
	}
}

func TestSubscriptionsOnOneStopShareOneFetchPerCycle(t *testing.T) {
	s := &fakeSender{}
	b := activity(2, "1_570")
	b.PushToken = "push-b"
	h := newHarness(t, s, activity(1, "1_570"), b, activity(3, "1_999"))
	h.cycle()
	if h.oba.count() != 2 {
		t.Fatalf("two stops -> two fetches, got %d", h.oba.count())
	}
	if len(s.pushes()) != 3 {
		t.Errorf("every subscription pushes: %d", len(s.pushes()))
	}
	h.cycle() // 60s later: cache (55s) expired
	if h.oba.count() != 4 {
		t.Errorf("cache must expire after 55s: fetches = %d", h.oba.count())
	}
}

func TestQueryWindowIsLookbackAndLookahead(t *testing.T) {
	s := &fakeSender{}
	h := newHarness(t, s, activity(1, "1_570"))
	var got obaapi.StopArrivalsQuery
	h.oba.fn = func(q obaapi.StopArrivalsQuery) ([]obaapi.StopArrival, error) { got = q; return nil, nil }
	h.cycle()
	if got.StopID != "1_570" || got.MinutesBefore != liveactivities.LookbackMinutes || got.MinutesAfter != liveactivities.LookaheadMinutes {
		t.Errorf("query = %+v", got)
	}
}
