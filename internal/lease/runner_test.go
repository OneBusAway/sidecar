package lease_test

import (
	"context"
	"io"
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

func (r *fakeRepo) Acquire(_ context.Context, name, holder string, now time.Time, ttl time.Duration) (bool, error) {
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

func (r *fakeRepo) Release(_ context.Context, name, holder string) error {
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

func (r *fakeRepo) holderOf(name string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.leases[name].holder
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
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
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
	time.Sleep(20 * r.Poll)
	ok, err := repo.Acquire(context.Background(), "alarms", "other", time.Now(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("another holder acquired the lease while the runner was alive; want it renewed")
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
	repo.mu.Lock()
	repo.failAcquire = false
	repo.mu.Unlock()
	waitFor(t, "a tick once the store recovered", func() bool { return c.n.Load() >= 1 })
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
