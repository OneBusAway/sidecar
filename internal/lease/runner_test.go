package lease_test

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/lease"
)

// fakeRepo is an in-memory lease.Repository with the same semantics the
// storetest suite pins for the real one.
type fakeRepo struct {
	mu     sync.Mutex
	leases map[string]fakeLease
	// failAcquire makes every Acquire error, standing in for a database
	// that is briefly unreachable.
	failAcquire bool
}

type fakeLease struct {
	holder  string
	expires time.Time
}

func newFakeRepo() *fakeRepo { return &fakeRepo{leases: make(map[string]fakeLease)} }

// Acquire and Release honor ctx like a real driver would: a Release on
// the loop's own cancelled ctx must fail, which is what pins the runner's
// use of a fresh context at shutdown.
func (r *fakeRepo) Acquire(ctx context.Context, name, holder string, now time.Time, ttl time.Duration) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failAcquire {
		return false, context.DeadlineExceeded
	}
	cur, ok := r.leases[name]
	if ok && cur.holder != holder && cur.expires.After(now) {
		return false, nil
	}
	r.leases[name] = fakeLease{holder: holder, expires: now.Add(ttl)}
	return true, nil
}

func (r *fakeRepo) Release(ctx context.Context, name, holder string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if cur, ok := r.leases[name]; ok && cur.holder == holder {
		delete(r.leases, name)
	}
	return nil
}

func (r *fakeRepo) seed(name, holder string, expires time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.leases[name] = fakeLease{holder: holder, expires: expires}
}

func (r *fakeRepo) setFailAcquire(v bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failAcquire = v
}

func (r *fakeRepo) holderOf(name string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.leases[name].holder
}

func (r *fakeRepo) expiresOf(name string) time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.leases[name].expires
}

// counter is a Tick that counts its calls.
type counter struct{ n atomic.Int64 }

func (c *counter) tick(context.Context) { c.n.Add(1) }

func newRunner(repo lease.Repository) *lease.Runner {
	return &lease.Runner{
		Repo:   repo,
		Holder: "me",
		Now:    time.Now,
		Poll:   2 * time.Millisecond,
		Logger: slog.New(slog.DiscardHandler),
	}
}

// start runs the loop on a goroutine and returns a stop func that cancels
// it and waits for Run to return (failing the test if it does not).
func start(t *testing.T, r *lease.Runner, l lease.Loop) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx, l); close(done) }()
	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("Run did not return after ctx was cancelled")
		}
	}
}

// waitFor polls cond for up to a second.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestRunTicksImmediatelyOnAcquiringLease(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	c := &counter{}
	stop := start(t, newRunner(repo), lease.Loop{Name: "alarms", Interval: time.Hour, Tick: c.tick})
	defer stop()
	waitFor(t, "the first tick", func() bool { return c.n.Load() >= 1 })
	if got := repo.holderOf("alarms"); got != "me" {
		t.Errorf("holder = %q, want me", got)
	}
}

func TestRunTicksOnInterval(t *testing.T) {
	t.Parallel()
	c := &counter{}
	stop := start(t, newRunner(newFakeRepo()), lease.Loop{Name: "alarms", Interval: 5 * time.Millisecond, Tick: c.tick})
	defer stop()
	// >= 3: the immediate first tick alone would satisfy 1.
	waitFor(t, "three ticks", func() bool { return c.n.Load() >= 3 })
}

func TestRunNeverTicksWhileAnotherProcessHoldsTheLease(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	repo.seed("alarms", "other", time.Now().Add(time.Hour))
	c := &counter{}
	stop := start(t, newRunner(repo), lease.Loop{Name: "alarms", Interval: time.Millisecond, Tick: c.tick})
	time.Sleep(30 * time.Millisecond)
	stop()
	if n := c.n.Load(); n != 0 {
		t.Fatalf("ticked %d times while another holder had the lease, want 0", n)
	}
	if got := repo.holderOf("alarms"); got != "other" {
		t.Errorf("holder = %q, want other (must not be taken over or released)", got)
	}
}

func TestRunTakesOverAnExpiredLease(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	repo.seed("alarms", "other", time.Now().Add(-time.Second))
	c := &counter{}
	stop := start(t, newRunner(repo), lease.Loop{Name: "alarms", Interval: time.Hour, Tick: c.tick})
	defer stop()
	waitFor(t, "the takeover tick", func() bool { return c.n.Load() >= 1 })
	if got := repo.holderOf("alarms"); got != "me" {
		t.Errorf("holder = %q, want me", got)
	}
}

// TestRunRenewsWhileHolding: the holder re-acquires every poll, so a
// second process never finds the lease expired while the first is alive
// -- even though the TTL is only a few polls long.
func TestRunRenewsWhileHolding(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	r := newRunner(repo)
	stop := start(t, r, lease.Loop{Name: "alarms", Interval: time.Hour, Tick: (&counter{}).tick})
	defer stop()
	waitFor(t, "the lease", func() bool { return repo.holderOf("alarms") == "me" })
	first := repo.expiresOf("alarms")
	waitFor(t, "a renewal", func() bool { return repo.expiresOf("alarms").After(first) })
	if got := repo.holderOf("alarms"); got != "me" {
		t.Fatalf("holder = %q after renewal, want me", got)
	}
}

// TestRunRenewsDuringLongTick: a cycle that runs longer than the TTL must
// not lose the lease -- the holder is alive, just busy. Renewal therefore
// cannot share the tick's goroutine. The tick blocks on gate until the
// lease has been renewed at least twice underneath it.
func TestRunRenewsDuringLongTick(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	r := newRunner(repo)
	gate := make(chan struct{})
	var ticks atomic.Int64
	stop := start(t, r, lease.Loop{Name: "alarms", Interval: time.Hour, Tick: func(ctx context.Context) {
		if ticks.Add(1) == 1 {
			select {
			case <-gate:
			case <-ctx.Done():
			}
		}
	}})
	defer stop()
	waitFor(t, "the lease", func() bool { return repo.holderOf("alarms") == "me" })
	first := repo.expiresOf("alarms")
	waitFor(t, "two renewals during the tick", func() bool {
		return repo.expiresOf("alarms").Sub(first) >= 2*r.Poll
	})
	ok, err := repo.Acquire(context.Background(), "alarms", "other", time.Now(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("another holder took the lease during a long tick; want it renewed")
	}
	close(gate)
}

// TestRunNeverOverlapsItsOwnTicks: an interval fire during a running tick
// is skipped, not queued -- one runner never runs two cycles at once.
func TestRunNeverOverlapsItsOwnTicks(t *testing.T) {
	t.Parallel()
	r := newRunner(newFakeRepo())
	var inFlight, maxInFlight atomic.Int64
	stop := start(t, r, lease.Loop{Name: "alarms", Interval: time.Millisecond, Tick: func(context.Context) {
		n := inFlight.Add(1)
		for {
			m := maxInFlight.Load()
			if n <= m || maxInFlight.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
		inFlight.Add(-1)
	}})
	time.Sleep(50 * time.Millisecond)
	stop()
	if m := maxInFlight.Load(); m != 1 {
		t.Fatalf("max concurrent ticks = %d, want 1", m)
	}
}

func TestRunReleasesLeaseOnShutdown(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	c := &counter{}
	stop := start(t, newRunner(repo), lease.Loop{Name: "alarms", Interval: time.Hour, Tick: c.tick})
	waitFor(t, "the first tick", func() bool { return c.n.Load() >= 1 })
	stop()
	if got := repo.holderOf("alarms"); got != "" {
		t.Fatalf("holder after shutdown = %q, want released", got)
	}
}

func TestWakeTicksNowWhileHolding(t *testing.T) {
	t.Parallel()
	c := &counter{}
	wake := make(chan struct{}, 1)
	stop := start(t, newRunner(newFakeRepo()), lease.Loop{Name: "pushes", Interval: time.Hour, Wake: wake, Tick: c.tick})
	defer stop()
	waitFor(t, "the first tick", func() bool { return c.n.Load() >= 1 })
	wake <- struct{}{}
	waitFor(t, "the woken tick", func() bool { return c.n.Load() >= 2 })
}

func TestWakeIsIgnoredWhileNotHolding(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	repo.seed("pushes", "other", time.Now().Add(time.Hour))
	c := &counter{}
	wake := make(chan struct{}, 1)
	stop := start(t, newRunner(repo), lease.Loop{Name: "pushes", Interval: time.Hour, Wake: wake, Tick: c.tick})
	wake <- struct{}{}
	time.Sleep(30 * time.Millisecond)
	stop()
	if n := c.n.Load(); n != 0 {
		t.Fatalf("ticked %d times on Wake without the lease, want 0", n)
	}
}

func TestRunSurvivesAcquireErrors(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	repo.failAcquire = true
	c := &counter{}
	stop := start(t, newRunner(repo), lease.Loop{Name: "alarms", Interval: time.Millisecond, Tick: c.tick})
	defer stop()
	time.Sleep(10 * time.Millisecond)
	if n := c.n.Load(); n != 0 {
		t.Fatalf("ticked %d times while Acquire errored, want 0", n)
	}
	repo.setFailAcquire(false)
	waitFor(t, "a tick once the store recovered", func() bool { return c.n.Load() >= 1 })
}

// TestRunStopsTickingWhenLeaseIsLost is the split-brain case the package
// exists to prevent: once another process holds the lease (after a stall
// let it expire), this one must stop ticking -- on the interval and on
// Wake alike -- and must not take the lease back while it is live.
func TestRunStopsTickingWhenLeaseIsLost(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	c := &counter{}
	wake := make(chan struct{}, 1)
	r := newRunner(repo)
	stop := start(t, r, lease.Loop{Name: "alarms", Interval: time.Millisecond, Wake: wake, Tick: c.tick})
	defer stop()
	waitFor(t, "the first tick", func() bool { return c.n.Load() >= 1 })

	repo.seed("alarms", "other", time.Now().Add(time.Hour))
	time.Sleep(10 * r.Poll) // enough polls to notice the loss
	before := c.n.Load()
	wake <- struct{}{}
	time.Sleep(10 * r.Poll)
	if after := c.n.Load(); after != before {
		t.Fatalf("ticked %d more times after losing the lease, want 0", after-before)
	}
	if got := repo.holderOf("alarms"); got != "other" {
		t.Errorf("holder = %q, want other (a live lease must not be taken back)", got)
	}
}

// TestRunStopsTickingWhileStoreIsUnreachable: an Acquire error while
// holding means the lease cannot be renewed either, so ticking pauses
// until the store answers again.
func TestRunStopsTickingWhileStoreIsUnreachable(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	c := &counter{}
	r := newRunner(repo)
	stop := start(t, r, lease.Loop{Name: "alarms", Interval: time.Millisecond, Tick: c.tick})
	defer stop()
	waitFor(t, "the first tick", func() bool { return c.n.Load() >= 1 })

	repo.setFailAcquire(true)
	time.Sleep(10 * r.Poll)
	before := c.n.Load()
	time.Sleep(10 * r.Poll)
	if after := c.n.Load(); after != before {
		t.Fatalf("ticked %d more times while the store was unreachable, want 0", after-before)
	}
	repo.setFailAcquire(false)
	waitFor(t, "ticking to resume", func() bool { return c.n.Load() > before })
}

// TestRunWithDefaultPoll is the production configuration -- cmd/sidecar
// leaves Poll unset -- so the fallback must produce a working ticker
// rather than a time.NewTicker panic inside the goroutine.
func TestRunWithDefaultPoll(t *testing.T) {
	t.Parallel()
	c := &counter{}
	r := newRunner(newFakeRepo())
	r.Poll = 0
	stop := start(t, r, lease.Loop{Name: "alarms", Interval: time.Hour, Tick: c.tick})
	defer stop()
	waitFor(t, "the first tick", func() bool { return c.n.Load() >= 1 })
}

func TestGoPanicsOnMisconfiguration(t *testing.T) {
	t.Parallel()
	good := func() *lease.Runner { return newRunner(newFakeRepo()) }
	tick := (&counter{}).tick
	for name, tc := range map[string]struct {
		runner *lease.Runner
		loop   lease.Loop
	}{
		"empty holder": {func() *lease.Runner { r := good(); r.Holder = ""; return r }(), lease.Loop{Name: "a", Interval: time.Hour, Tick: tick}},
		"nil repo":     {func() *lease.Runner { r := good(); r.Repo = nil; return r }(), lease.Loop{Name: "a", Interval: time.Hour, Tick: tick}},
		"empty name":   {good(), lease.Loop{Interval: time.Hour, Tick: tick}},
		"nil tick":     {good(), lease.Loop{Name: "a", Interval: time.Hour}},
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("Go did not panic")
				}
			}()
			tc.runner.Go(context.Background(), tc.loop)
		})
	}
}

// TestRunFloorsNonPositiveInterval: time.NewTicker panics on a duration
// <= 0, and Run is a goroutine with no recover.
func TestRunFloorsNonPositiveInterval(t *testing.T) {
	t.Parallel()
	for _, interval := range []time.Duration{0, -time.Hour} {
		c := &counter{}
		stop := start(t, newRunner(newFakeRepo()), lease.Loop{Name: "alarms", Interval: interval, Tick: c.tick})
		waitFor(t, "the first tick", func() bool { return c.n.Load() >= 1 })
		stop()
	}
}

// TestWaitReturnsOnceEveryLoopHasReleased: Go/Wait is how cmd/sidecar
// keeps the process alive until every lease is released -- without it,
// exit races the loops' Release and a replacement waits out the full TTL.
func TestWaitReturnsOnceEveryLoopHasReleased(t *testing.T) {
	t.Parallel()
	repo := newFakeRepo()
	r := newRunner(repo)
	ctx, cancel := context.WithCancel(context.Background())
	c := &counter{}
	r.Go(ctx, lease.Loop{Name: "a", Interval: time.Hour, Tick: c.tick})
	r.Go(ctx, lease.Loop{Name: "b", Interval: time.Hour, Tick: c.tick})
	waitFor(t, "both leases acquired", func() bool { return repo.holderOf("a") == "me" && repo.holderOf("b") == "me" })

	cancel()
	done := make(chan struct{})
	go func() { r.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Wait did not return after ctx was cancelled")
	}
	if a, b := repo.holderOf("a"), repo.holderOf("b"); a != "" || b != "" {
		t.Fatalf("holders after Wait = %q, %q; want both released", a, b)
	}
}

// TestRunDoesNotSkipTicksOnTimerJitter pins the tick cadence against the
// trap spec section 6.3 describes for the Live Activity keepalive: a
// scheduler that stamps "next tick" after a tick returns and compares
// exactly skips every other ticker fire. 300ms at 20ms is ~15 ticks; the
// every-other-tick bug yields ~7.
func TestRunDoesNotSkipTicksOnTimerJitter(t *testing.T) {
	t.Parallel()
	c := &counter{}
	r := newRunner(newFakeRepo())
	r.Poll = 20 * time.Millisecond
	stop := start(t, r, lease.Loop{Name: "alarms", Interval: 20 * time.Millisecond, Tick: c.tick})
	time.Sleep(300 * time.Millisecond)
	stop()
	if n := c.n.Load(); n < 10 {
		t.Fatalf("ticked %d times in 300ms at a 20ms interval, want at least 10 (every-other-tick skip?)", n)
	}
}
