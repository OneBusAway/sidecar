package alarms_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/alarms"
	"github.com/OneBusAway/sidecar/internal/obaapi"
	"github.com/OneBusAway/sidecar/internal/push"
	"github.com/OneBusAway/sidecar/internal/pushreg"
	"github.com/OneBusAway/sidecar/internal/regions"
)

// base is a fixed instant every test schedules departures relative to, so
// the scheduler's Now func never touches the wall clock.
var base = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

// --- fakes ---------------------------------------------------------------

// fakeAlarmRepo is an in-memory alarms.Repository. Scheduler cycles run
// alarm checks concurrently (errgroup, SetLimit(8)), so every method locks.
type fakeAlarmRepo struct {
	mu       sync.Mutex
	alarms   map[int64]alarms.Alarm
	deferErr error // returned by every Defer when set
}

func newFakeAlarmRepo(as ...alarms.Alarm) *fakeAlarmRepo {
	r := &fakeAlarmRepo{alarms: make(map[int64]alarms.Alarm)}
	for _, a := range as {
		r.alarms[a.ID] = a
	}
	return r
}

func (r *fakeAlarmRepo) Create(context.Context, alarms.NewAlarm, time.Time) (alarms.Alarm, error) {
	return alarms.Alarm{}, errors.New("not implemented")
}

func (r *fakeAlarmRepo) FindV1(context.Context, alarms.V1Key) (alarms.Alarm, error) {
	return alarms.Alarm{}, alarms.ErrNotFound
}

func (r *fakeAlarmRepo) Delete(context.Context, int64, string) error {
	return alarms.ErrNotFound
}

func (r *fakeAlarmRepo) DeleteByID(_ context.Context, id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.alarms, id)
	return nil
}

func (r *fakeAlarmRepo) List(context.Context) ([]alarms.Alarm, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]alarms.Alarm, 0, len(r.alarms))
	for _, a := range r.alarms {
		out = append(out, a)
	}
	return out, nil
}

func (r *fakeAlarmRepo) ListDue(_ context.Context, now time.Time) ([]alarms.Alarm, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]alarms.Alarm, 0, len(r.alarms))
	for _, a := range r.alarms {
		if a.CheckAfter.After(now) {
			continue
		}
		out = append(out, a)
	}
	return out, nil
}

func (r *fakeAlarmRepo) Defer(_ context.Context, id int64, until time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.deferErr != nil {
		return r.deferErr
	}
	a, ok := r.alarms[id]
	if !ok {
		return nil
	}
	a.CheckAfter = until
	r.alarms[id] = a
	return nil
}

func (r *fakeAlarmRepo) RecordFailure(_ context.Context, id int64) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.alarms[id]
	if !ok {
		return 0, alarms.ErrNotFound
	}
	a.FailureCount++
	r.alarms[id] = a
	return a.FailureCount, nil
}

func (r *fakeAlarmRepo) ResetFailures(_ context.Context, id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.alarms[id]
	if !ok {
		return alarms.ErrNotFound
	}
	a.FailureCount = 0
	r.alarms[id] = a
	return nil
}

func (r *fakeAlarmRepo) ListByRegion(context.Context, int64) ([]alarms.Alarm, error) {
	panic("not used by the scheduler")
}

func (r *fakeAlarmRepo) GetInRegion(context.Context, int64, int64) (alarms.Alarm, error) {
	panic("not used by the scheduler")
}

func (r *fakeAlarmRepo) get(id int64) (alarms.Alarm, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.alarms[id]
	return a, ok
}

// fakeRegions is a fixed regions.Repository; only Get is exercised by the
// scheduler.
type fakeRegions struct {
	regions map[int64]regions.Region
}

func (f fakeRegions) Get(_ context.Context, id int64) (regions.Region, error) {
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

// fakeOBA implements alarms.DepartureSource with an injectable func so each
// test can script the upstream response per call.
type fakeOBA struct {
	fn func(ctx context.Context, region regions.Region, q obaapi.DepartureQuery) (obaapi.Departure, error)
}

func (f fakeOBA) ArrivalAndDeparture(ctx context.Context, region regions.Region, q obaapi.DepartureQuery) (obaapi.Departure, error) {
	return f.fn(ctx, region, q)
}

// fakeSender records every notification handed to it and can be told to
// fail every Send.
type fakeSender struct {
	mu   sync.Mutex
	sent []push.Notification
	err  error
}

func (f *fakeSender) Send(_ context.Context, n push.Notification) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, n)
	return f.err
}

func (f *fakeSender) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

func (f *fakeSender) last() push.Notification {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sent[len(f.sent)-1]
}

// --- helpers ---------------------------------------------------------------

const testRegionID = 1

var testRegion = regions.Region{ID: testRegionID, Name: "Test Region", Active: true}

func testAlarm(id int64, secondsBefore int64) alarms.Alarm {
	return alarms.Alarm{
		ID:              id,
		RegionID:        testRegionID,
		Token:           "tok",
		APIVersion:      2,
		UserPushID:      "device-push-token",
		OperatingSystem: pushreg.OSIOS,
		APNSSandbox:     true,
		StopID:          "1_570",
		TripID:          "1_604370",
		SecondsBefore:   secondsBefore,
		Message:         "The 44 to Ballard leaves in 10 minutes",
	}
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newScheduler(repo alarms.Repository, oba alarms.DepartureSource, sender push.Sender) *alarms.Scheduler {
	return &alarms.Scheduler{
		Repo:    repo,
		Regions: fakeRegions{regions: map[int64]regions.Region{testRegionID: testRegion}},
		OBA:     oba,
		Sender:  sender,
		Now:     func() time.Time { return base },
		Logger:  testLogger(),
	}
}

func msAt(offsetSeconds int64) int64 {
	return base.Add(time.Duration(offsetSeconds)*time.Second).Unix() * 1000
}

// --- tests -------------------------------------------------------------

func TestFiresAndDeletes(t *testing.T) {
	t.Parallel()
	alarm := testAlarm(1, 600)
	repo := newFakeAlarmRepo(alarm)
	oba := fakeOBA{fn: func(context.Context, regions.Region, obaapi.DepartureQuery) (obaapi.Departure, error) {
		return obaapi.Departure{Predicted: true, PredictedDepartureTime: msAt(300)}, nil
	}}
	sender := &fakeSender{}
	s := newScheduler(repo, oba, sender)

	s.CheckAll(context.Background())

	if sender.count() != 1 {
		t.Fatalf("sent = %d; want 1", sender.count())
	}
	n := sender.last()
	if !reflect.DeepEqual(n.Tokens, []string{alarm.UserPushID}) {
		t.Errorf("Tokens = %v; want [%s]", n.Tokens, alarm.UserPushID)
	}
	if n.Platform != push.PlatformIOS {
		t.Errorf("Platform = %v; want PlatformIOS", n.Platform)
	}
	if n.Sandbox != alarm.APNSSandbox {
		t.Errorf("Sandbox = %v; want %v", n.Sandbox, alarm.APNSSandbox)
	}
	if n.Title != "OneBusAway" {
		t.Errorf("Title = %q; want OneBusAway", n.Title)
	}
	if n.Message != alarm.Message {
		t.Errorf("Message = %q; want %q", n.Message, alarm.Message)
	}
	if !reflect.DeepEqual(n.Data, alarm.PushData()) {
		t.Errorf("Data = %v; want %v", n.Data, alarm.PushData())
	}
	if _, ok := repo.get(1); ok {
		t.Error("alarm still present after firing; want deleted")
	}
}

func TestUsesPredictedOverScheduled(t *testing.T) {
	t.Parallel()
	alarm := testAlarm(1, 600)
	repo := newFakeAlarmRepo(alarm)
	oba := fakeOBA{fn: func(context.Context, regions.Region, obaapi.DepartureQuery) (obaapi.Departure, error) {
		return obaapi.Departure{
			Predicted:              true,
			PredictedDepartureTime: msAt(300),
			ScheduledDepartureTime: msAt(3000), // scheduled would be a Wait; predicted must win
		}, nil
	}}
	sender := &fakeSender{}
	s := newScheduler(repo, oba, sender)

	s.CheckAll(context.Background())

	if sender.count() != 1 {
		t.Fatalf("sent = %d; want 1 (predicted should have won over scheduled)", sender.count())
	}
	if _, ok := repo.get(1); ok {
		t.Error("alarm still present; want deleted after fire")
	}
}

func TestFallsBackToScheduled(t *testing.T) {
	t.Parallel()
	alarm := testAlarm(1, 600)
	repo := newFakeAlarmRepo(alarm)
	oba := fakeOBA{fn: func(context.Context, regions.Region, obaapi.DepartureQuery) (obaapi.Departure, error) {
		return obaapi.Departure{
			Predicted:              false,
			PredictedDepartureTime: 0,
			ScheduledDepartureTime: msAt(300),
		}, nil
	}}
	sender := &fakeSender{}
	s := newScheduler(repo, oba, sender)

	s.CheckAll(context.Background())

	if sender.count() != 1 {
		t.Fatalf("sent = %d; want 1 (should have fallen back to scheduled)", sender.count())
	}
}

func TestWaitsWhenFar(t *testing.T) {
	t.Parallel()
	alarm := testAlarm(1, 600)
	alarm.FailureCount = 2 // must still be reset on a successful lookup, even a Wait
	repo := newFakeAlarmRepo(alarm)
	oba := fakeOBA{fn: func(context.Context, regions.Region, obaapi.DepartureQuery) (obaapi.Departure, error) {
		return obaapi.Departure{Predicted: true, PredictedDepartureTime: msAt(700)}, nil
	}}
	sender := &fakeSender{}
	s := newScheduler(repo, oba, sender)

	s.CheckAll(context.Background())

	if sender.count() != 0 {
		t.Fatalf("sent = %d; want 0 (departure too far out)", sender.count())
	}
	got, ok := repo.get(1)
	if !ok {
		t.Fatal("alarm deleted; want it to survive a Wait")
	}
	if got.FailureCount != 0 {
		t.Errorf("FailureCount = %d; want reset to 0 on successful lookup", got.FailureCount)
	}
}

// TestSweepSkipsDeferredAlarms pins the due-window half of the contract:
// an alarm whose CheckAfter is still ahead of the clock costs nothing this
// cycle -- no OBA lookup, no decision -- while a due one is checked as usual.
func TestSweepSkipsDeferredAlarms(t *testing.T) {
	t.Parallel()
	deferred := testAlarm(1, 600)
	deferred.CheckAfter = base.Add(time.Hour)
	due := testAlarm(2, 600)
	repo := newFakeAlarmRepo(deferred, due)
	var mu sync.Mutex
	var lookedUp []string
	oba := fakeOBA{fn: func(_ context.Context, _ regions.Region, q obaapi.DepartureQuery) (obaapi.Departure, error) {
		mu.Lock()
		lookedUp = append(lookedUp, q.TripID)
		mu.Unlock()
		return obaapi.Departure{Predicted: true, PredictedDepartureTime: msAt(300)}, nil
	}}
	sender := &fakeSender{}
	s := newScheduler(repo, oba, sender)

	s.CheckAll(context.Background())

	if len(lookedUp) != 1 {
		t.Fatalf("OBA lookups = %d; want 1 (only the due alarm)", len(lookedUp))
	}
	if sender.count() != 1 {
		t.Errorf("sent = %d; want 1", sender.count())
	}
	if _, ok := repo.get(1); !ok {
		t.Error("deferred alarm gone; want it untouched")
	}
	if _, ok := repo.get(2); ok {
		t.Error("due alarm still present; want fired and deleted")
	}
}

// TestFarWaitDefersNextCheck: a departure well out is re-checked halfway
// to its fire window, not every minute. Halving is deliberate -- each
// re-check halves the remaining slack again, so a bus that starts running
// early is still caught with plenty of margin, and once the slack is small
// the alarm is back to the once-a-minute cadence spec section 5.3 wants.
func TestFarWaitDefersNextCheck(t *testing.T) {
	t.Parallel()
	alarm := testAlarm(1, 600)
	repo := newFakeAlarmRepo(alarm)
	oba := fakeOBA{fn: func(context.Context, regions.Region, obaapi.DepartureQuery) (obaapi.Departure, error) {
		// Departure in 90m, fire window opens at 90m-10m: slack is 4800s.
		return obaapi.Departure{Predicted: true, PredictedDepartureTime: msAt(90 * 60)}, nil
	}}
	s := newScheduler(repo, oba, &fakeSender{})

	s.CheckAll(context.Background())

	got, ok := repo.get(1)
	if !ok {
		t.Fatal("alarm deleted; want it to survive a Wait")
	}
	want := base.Add(4800 / 2 * time.Second)
	if !got.CheckAfter.Equal(want) {
		t.Errorf("CheckAfter = %v; want %v (now + half the slack)", got.CheckAfter, want)
	}
}

// TestMinDeferralBoundary: a halving shorter than MinDeferral is skipped
// (the alarm stays due every cycle -- deferring would save nothing and
// risk missing an early bus); one of exactly MinDeferral is taken.
func TestMinDeferralBoundary(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		slack     int64 // seconds until the fire window opens
		wantDefer time.Duration
	}{
		{"just under", 2*int64(alarms.MinDeferral/time.Second) - 1, 0},
		{"exactly", 2 * int64(alarms.MinDeferral/time.Second), alarms.MinDeferral},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			alarm := testAlarm(1, 600)
			repo := newFakeAlarmRepo(alarm)
			oba := fakeOBA{fn: func(context.Context, regions.Region, obaapi.DepartureQuery) (obaapi.Departure, error) {
				return obaapi.Departure{Predicted: true, PredictedDepartureTime: msAt(600 + tc.slack)}, nil
			}}
			s := newScheduler(repo, oba, &fakeSender{})

			s.CheckAll(context.Background())

			got, ok := repo.get(1)
			if !ok {
				t.Fatal("alarm deleted; want it to survive a Wait")
			}
			var want time.Time
			if tc.wantDefer > 0 {
				want = base.Add(tc.wantDefer)
			}
			if !got.CheckAfter.Equal(want) {
				t.Errorf("CheckAfter = %v; want %v", got.CheckAfter, want)
			}
		})
	}
}

// TestDeferralIsCapped: a day-ahead alarm is not left unchecked for half a
// day; MaxDeferral bounds how long a trip can move earlier unnoticed.
func TestDeferralIsCapped(t *testing.T) {
	t.Parallel()
	alarm := testAlarm(1, 600)
	repo := newFakeAlarmRepo(alarm)
	oba := fakeOBA{fn: func(context.Context, regions.Region, obaapi.DepartureQuery) (obaapi.Departure, error) {
		return obaapi.Departure{Predicted: true, PredictedDepartureTime: msAt(24 * 3600)}, nil
	}}
	s := newScheduler(repo, oba, &fakeSender{})

	s.CheckAll(context.Background())

	got, _ := repo.get(1)
	if want := base.Add(alarms.MaxDeferral); !got.CheckAfter.Equal(want) {
		t.Errorf("CheckAfter = %v; want %v (capped)", got.CheckAfter, want)
	}
}

// TestDeferFailureIsNonFatal: a Defer the store refuses just means one more
// minute-cadence check -- the alarm survives and nothing is pushed.
func TestDeferFailureIsNonFatal(t *testing.T) {
	t.Parallel()
	alarm := testAlarm(1, 600)
	repo := newFakeAlarmRepo(alarm)
	repo.deferErr = errors.New("locked")
	oba := fakeOBA{fn: func(context.Context, regions.Region, obaapi.DepartureQuery) (obaapi.Departure, error) {
		return obaapi.Departure{Predicted: true, PredictedDepartureTime: msAt(3 * 3600)}, nil
	}}
	sender := &fakeSender{}
	s := newScheduler(repo, oba, sender)

	s.CheckAll(context.Background())

	if _, ok := repo.get(1); !ok {
		t.Fatal("alarm deleted; want it kept for the next cycle")
	}
	if sender.count() != 0 {
		t.Errorf("sent = %d; want 0", sender.count())
	}
}

func TestExpiredDeletesWithoutPush(t *testing.T) {
	t.Parallel()
	alarm := testAlarm(1, 600)
	repo := newFakeAlarmRepo(alarm)
	oba := fakeOBA{fn: func(context.Context, regions.Region, obaapi.DepartureQuery) (obaapi.Departure, error) {
		return obaapi.Departure{Predicted: true, PredictedDepartureTime: msAt(-60)}, nil
	}}
	sender := &fakeSender{}
	s := newScheduler(repo, oba, sender)

	s.CheckAll(context.Background())

	if sender.count() != 0 {
		t.Fatalf("sent = %d; want 0 (expired alarms must never push)", sender.count())
	}
	if _, ok := repo.get(1); ok {
		t.Error("alarm still present; want deleted once expired")
	}
}

func TestTransientErrorDoesNotCount(t *testing.T) {
	t.Parallel()
	alarm := testAlarm(1, 600)
	alarm.FailureCount = 1
	repo := newFakeAlarmRepo(alarm)
	oba := fakeOBA{fn: func(context.Context, regions.Region, obaapi.DepartureQuery) (obaapi.Departure, error) {
		return obaapi.Departure{}, errors.New("network blip")
	}}
	sender := &fakeSender{}
	s := newScheduler(repo, oba, sender)

	s.CheckAll(context.Background())

	got, ok := repo.get(1)
	if !ok {
		t.Fatal("alarm deleted; a transient error must never reap")
	}
	if got.FailureCount != 1 {
		t.Errorf("FailureCount = %d; want unchanged at 1 (transient errors don't count)", got.FailureCount)
	}
	if sender.count() != 0 {
		t.Errorf("sent = %d; want 0", sender.count())
	}
}

func TestSuccessResetsStreak(t *testing.T) {
	t.Parallel()
	alarm := testAlarm(1, 600)
	alarm.FailureCount = 2
	repo := newFakeAlarmRepo(alarm)
	oba := fakeOBA{fn: func(context.Context, regions.Region, obaapi.DepartureQuery) (obaapi.Departure, error) {
		return obaapi.Departure{Predicted: true, PredictedDepartureTime: msAt(700)}, nil // too early: Wait
	}}
	s := newScheduler(repo, oba, &fakeSender{})

	s.CheckAll(context.Background())

	got, ok := repo.get(1)
	if !ok {
		t.Fatal("alarm deleted; want it to survive")
	}
	if got.FailureCount != 0 {
		t.Errorf("FailureCount = %d; want 0 after a successful lookup", got.FailureCount)
	}
}

func TestSendFailureKeepsAlarm(t *testing.T) {
	t.Parallel()
	alarm := testAlarm(1, 600)
	repo := newFakeAlarmRepo(alarm)
	oba := fakeOBA{fn: func(context.Context, regions.Region, obaapi.DepartureQuery) (obaapi.Departure, error) {
		return obaapi.Departure{Predicted: true, PredictedDepartureTime: msAt(300)}, nil
	}}
	sender := &fakeSender{err: errors.New("gorush unreachable")}
	s := newScheduler(repo, oba, sender)

	s.CheckAll(context.Background())

	if sender.count() != 1 {
		t.Fatalf("sent attempts = %d; want 1", sender.count())
	}
	if _, ok := repo.get(1); !ok {
		t.Error("alarm deleted despite send failure; delete must happen only after Send returns nil")
	}
}

func TestNilSenderLeavesAlarm(t *testing.T) {
	t.Parallel()
	fireAlarm := testAlarm(1, 600)
	fireAlarm.TripID = "fire-trip"
	expiredAlarm := testAlarm(2, 600)
	expiredAlarm.TripID = "expired-trip"
	reapAlarm := testAlarm(3, 600)
	reapAlarm.TripID = "reap-trip"

	repo := newFakeAlarmRepo(fireAlarm, expiredAlarm, reapAlarm)
	oba := fakeOBA{fn: func(_ context.Context, _ regions.Region, q obaapi.DepartureQuery) (obaapi.Departure, error) {
		switch q.TripID {
		case "fire-trip":
			return obaapi.Departure{Predicted: true, PredictedDepartureTime: msAt(300)}, nil
		case "expired-trip":
			return obaapi.Departure{Predicted: true, PredictedDepartureTime: msAt(-60)}, nil
		default: // reap-trip
			return obaapi.Departure{}, obaapi.ErrNotFound
		}
	}}
	s := newScheduler(repo, oba, nil) // store-only mode: no push transport configured

	s.CheckAll(context.Background()) // cycle 1
	if _, ok := repo.get(1); !ok {
		t.Error("fire-window alarm deleted with nil Sender; want it to survive (leave to expire)")
	}
	if _, ok := repo.get(2); ok {
		t.Error("expired alarm still present with nil Sender; expiry must still delete without a Sender")
	}

	s.CheckAll(context.Background()) // cycle 2: reap streak 2
	s.CheckAll(context.Background()) // cycle 3: reap streak 3 -> deleted
	if _, ok := repo.get(3); ok {
		t.Error("unresolvable alarm still present after 3 failures with nil Sender; reaping must still run")
	}
	if _, ok := repo.get(1); !ok {
		t.Error("fire-window alarm vanished across later cycles; nil Sender must never delete it")
	}
}

// erroringRegions fails every Get with a transient store error -- never
// regions.ErrNotFound. Embedding fakeRegions supplies the rest of the
// interface; only Get is overridden.
type erroringRegions struct{ fakeRegions }

func (erroringRegions) Get(context.Context, int64) (regions.Region, error) {
	return regions.Region{}, errors.New("database is locked")
}

// TestRegionStoreErrorDoesNotCount is the companion to
// TestMissingRegionCountsAsFailure: a region that is *gone* dooms its
// alarms, but a region store that is briefly unavailable says nothing about
// them. Counting the latter would let one bad minute of SQLite reap every
// pending alarm in the deployment three cycles later.
func TestRegionStoreErrorDoesNotCount(t *testing.T) {
	t.Parallel()
	alarm := testAlarm(1, 600)
	alarm.FailureCount = 2 // one more failure would reap it
	repo := newFakeAlarmRepo(alarm)
	oba := fakeOBA{fn: func(context.Context, regions.Region, obaapi.DepartureQuery) (obaapi.Departure, error) {
		// t.Error, not t.Fatal: this runs on a CheckAll worker goroutine,
		// where FailNow would stop only that goroutine.
		t.Error("OBA lookup should not run when the region store fails")
		return obaapi.Departure{}, nil
	}}
	s := newScheduler(repo, oba, &fakeSender{})
	s.Regions = erroringRegions{}

	s.CheckAll(context.Background())

	got, ok := repo.get(1)
	if !ok {
		t.Fatal("alarm reaped on a transient region store error; a database hiccup must never delete a rider's alarm")
	}
	if got.FailureCount != 2 {
		t.Errorf("FailureCount = %d; want unchanged at 2 (store errors don't count)", got.FailureCount)
	}
}

func TestAndroidPlatform(t *testing.T) {
	t.Parallel()
	alarm := testAlarm(1, 600)
	alarm.OperatingSystem = pushreg.OSAndroid
	repo := newFakeAlarmRepo(alarm)
	oba := fakeOBA{fn: func(context.Context, regions.Region, obaapi.DepartureQuery) (obaapi.Departure, error) {
		return obaapi.Departure{Predicted: true, PredictedDepartureTime: msAt(300)}, nil
	}}
	sender := &fakeSender{}
	s := newScheduler(repo, oba, sender)

	s.CheckAll(context.Background())

	if sender.count() != 1 {
		t.Fatalf("sent = %d; want 1", sender.count())
	}
	if got := sender.last().Platform; got != push.PlatformAndroid {
		t.Errorf("Platform = %v; want PlatformAndroid", got)
	}
}

// TestUnresolvableAlarmsAreReaped covers every §5.3 way an alarm can turn out
// to be permanently unresolvable. All of them share one contract -- the streak
// climbs, nothing is ever pushed, and the row is gone after the third cycle --
// so they are one table rather than one copied test per cause. A transient
// failure is deliberately NOT here: it must not count, and TestTransientError-
// DoesNotCount and TestRegionStoreErrorDoesNotCount pin that instead.
func TestUnresolvableAlarmsAreReaped(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// mutate adjusts the alarm; obaErr is what the lookup returns. An
		// alarm with no trip identity never reaches the lookup at all.
		mutate func(*alarms.Alarm)
		obaErr error
	}{
		{"trip aged out of the upstream", nil, obaapi.ErrNotFound},
		{"region has no API key and never will", nil, obaapi.ErrNotConfigured},
		{"no stop id, so no lookup is possible", func(a *alarms.Alarm) { a.StopID = "" }, nil},
		{"no trip id, so no lookup is possible", func(a *alarms.Alarm) { a.TripID = "" }, nil},
		{"neither stop nor trip id", func(a *alarms.Alarm) { a.StopID, a.TripID = "", "" }, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			alarm := testAlarm(1, 600)
			if tc.mutate != nil {
				tc.mutate(&alarm)
			}
			repo := newFakeAlarmRepo(alarm)
			oba := fakeOBA{fn: func(context.Context, regions.Region, obaapi.DepartureQuery) (obaapi.Departure, error) {
				if tc.obaErr == nil {
					t.Error("OBA lookup ran for an alarm that cannot be looked up")
					return obaapi.Departure{}, obaapi.ErrNotFound
				}
				return obaapi.Departure{}, tc.obaErr
			}}
			sender := &fakeSender{}
			s := newScheduler(repo, oba, sender)

			for cycle := int64(1); cycle <= 2; cycle++ {
				s.CheckAll(context.Background())
				got, ok := repo.get(1)
				if !ok || got.FailureCount != cycle {
					t.Fatalf("after cycle %d: got=%+v ok=%v; want FailureCount=%d, present", cycle, got, ok, cycle)
				}
			}

			s.CheckAll(context.Background())
			if _, ok := repo.get(1); ok {
				t.Error("after cycle 3: alarm still present; want reaped at 3 consecutive failures")
			}
			if sender.count() != 0 {
				t.Errorf("sent = %d; want 0 across all cycles", sender.count())
			}
		})
	}
}

// A region that has vanished from the directory dooms its alarms the same
// way, but reaches the reaper through the region lookup rather than the OBA
// one, so it gets its own test.
func TestMissingRegionCountsAsFailure(t *testing.T) {
	t.Parallel()
	alarm := testAlarm(1, 600)
	alarm.RegionID = 999 // unknown to fakeRegions
	repo := newFakeAlarmRepo(alarm)
	oba := fakeOBA{fn: func(context.Context, regions.Region, obaapi.DepartureQuery) (obaapi.Departure, error) {
		t.Error("OBA lookup ran for an alarm whose region does not resolve")
		return obaapi.Departure{}, nil
	}}
	s := newScheduler(repo, oba, &fakeSender{})

	s.CheckAll(context.Background())
	if got, ok := repo.get(1); !ok || got.FailureCount != 1 {
		t.Fatalf("got=%+v ok=%v; want FailureCount=1 (missing region counts as a lookup failure)", got, ok)
	}
}
