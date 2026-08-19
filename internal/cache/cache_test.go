package cache

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock is the injected clock; the package may not call time.Now.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func TestHitWithinTTL(t *testing.T) {
	clk := newClock()
	c := New[int](time.Minute, 8, time.Second, clk.Now)
	var calls atomic.Int64

	fetch := func(context.Context) (int, error) { calls.Add(1); return 42, nil }

	for i := 0; i < 3; i++ {
		got, err := c.Get(context.Background(), "k", fetch)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got != 42 {
			t.Fatalf("Get = %d, want 42", got)
		}
	}
	if calls.Load() != 1 {
		t.Errorf("fetch called %d times, want 1", calls.Load())
	}
}

func TestMissAfterTTL(t *testing.T) {
	clk := newClock()
	c := New[int](time.Minute, 8, time.Second, clk.Now)
	var calls atomic.Int64
	fetch := func(context.Context) (int, error) { calls.Add(1); return 1, nil }

	if _, err := c.Get(context.Background(), "k", fetch); err != nil {
		t.Fatalf("Get: %v", err)
	}
	clk.Advance(time.Minute + time.Nanosecond)
	if _, err := c.Get(context.Background(), "k", fetch); err != nil {
		t.Fatalf("Get after expiry: %v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("fetch called %d times, want 2", calls.Load())
	}
}

// A cached failure turns a five-second upstream blip into a thirty-minute
// outage. Errors must never be stored.
func TestErrorsAreNotCached(t *testing.T) {
	clk := newClock()
	c := New[int](time.Minute, 8, time.Second, clk.Now)
	var calls atomic.Int64
	boom := errors.New("boom")
	fetch := func(context.Context) (int, error) { calls.Add(1); return 0, boom }

	for i := 0; i < 2; i++ {
		if _, err := c.Get(context.Background(), "k", fetch); !errors.Is(err, boom) {
			t.Fatalf("Get err = %v, want boom", err)
		}
	}
	if calls.Load() != 2 {
		t.Errorf("fetch called %d times, want 2", calls.Load())
	}
}

// A burst of keystrokes on a cold cache must cost one upstream call.
func TestSingleflightCollapsesConcurrentGets(t *testing.T) {
	clk := newClock()
	c := New[int](time.Minute, 8, time.Second, clk.Now)

	var calls atomic.Int64
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	fetch := func(context.Context) (int, error) {
		calls.Add(1)
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		return 7, nil
	}

	const n = 20
	var wg sync.WaitGroup
	// launched confirms every goroutine has actually been scheduled before
	// the shared fetch is allowed to complete. Without this, waiting on
	// "entered" alone only proves ONE goroutine reached Get: a straggler
	// among the other 19 can then call Get after the in-flight singleflight
	// call has already finished, triggering a second upstream fetch and
	// making this test flaky rather than deterministic.
	var launched sync.WaitGroup
	launched.Add(n)
	results := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			launched.Done()
			v, err := c.Get(context.Background(), "k", fetch)
			if err != nil {
				t.Errorf("Get: %v", err)
				return
			}
			results[i] = v
		}(i)
	}

	launched.Wait()
	<-entered
	// Even after launched.Wait(), a goroutine may have signaled "about to
	// call Get" but not yet reached the singleflight registration inside it.
	// x/sync's own singleflight tests use the same wg-then-sleep pattern for
	// exactly this reason: there is no external hook for "call registered".
	time.Sleep(10 * time.Millisecond)
	close(release)
	wg.Wait()

	if calls.Load() != 1 {
		t.Errorf("fetch called %d times, want 1", calls.Load())
	}
	for i, v := range results {
		if v != 7 {
			t.Errorf("results[%d] = %d, want 7", i, v)
		}
	}
}

// The critical case: a cancelled caller must stop waiting, and the shared
// fetch must nevertheless finish and be cached for everyone else. This fails
// if singleflight.Do is used instead of DoChan, and it fails if the fetch
// context is not detached from the caller's.
func TestCancelledCallerDoesNotKillSharedFetch(t *testing.T) {
	clk := newClock()
	c := New[int](time.Minute, 8, time.Second, clk.Now)

	release := make(chan struct{})
	fetchCtxErr := make(chan error, 1)
	entered := make(chan struct{})
	fetch := func(ctx context.Context) (int, error) {
		close(entered)
		<-release
		fetchCtxErr <- ctx.Err()
		return 9, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := c.Get(ctx, "k", fetch)
		done <- err
	}()

	<-entered
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Get err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Get did not return after its caller was cancelled")
	}

	close(release)
	if err := <-fetchCtxErr; err != nil {
		t.Errorf("fetch context err = %v, want nil (the fetch must outlive its first caller)", err)
	}

	// The value the abandoned fetch produced must be cached.
	var calls atomic.Int64
	got, err := c.Get(context.Background(), "k", func(context.Context) (int, error) {
		calls.Add(1)
		return 0, errors.New("should not be called")
	})
	if err != nil {
		t.Fatalf("Get after cancellation: %v", err)
	}
	if got != 9 {
		t.Errorf("Get = %d, want 9", got)
	}
	if calls.Load() != 0 {
		t.Error("the abandoned fetch's value was not cached")
	}
}

// The fetch's budget is the cache's, measured from fetch start, not inherited
// from any caller.
func TestFetchBudgetApplies(t *testing.T) {
	clk := newClock()
	c := New[int](time.Minute, 8, 50*time.Millisecond, clk.Now)

	_, err := c.Get(context.Background(), "k", func(ctx context.Context) (int, error) {
		<-ctx.Done()
		return 0, ctx.Err()
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Get err = %v, want DeadlineExceeded", err)
	}
}

// The query cache is keyed by attacker-controlled input, so unbounded growth
// is a memory exhaustion vector on an unauthenticated endpoint.
func TestEvictionPrefersExpiredThenOldest(t *testing.T) {
	clk := newClock()
	c := New[int](time.Minute, 2, time.Second, clk.Now)
	ctx := context.Background()
	val := func(v int) func(context.Context) (int, error) {
		return func(context.Context) (int, error) { return v, nil }
	}

	if _, err := c.Get(ctx, "a", val(1)); err != nil {
		t.Fatal(err)
	}
	clk.Advance(time.Second)
	if _, err := c.Get(ctx, "b", val(2)); err != nil {
		t.Fatal(err)
	}
	// Inserting a third entry at capacity must drop "a", the oldest.
	clk.Advance(time.Second)
	if _, err := c.Get(ctx, "c", val(3)); err != nil {
		t.Fatal(err)
	}

	if c.Len() != 2 {
		t.Fatalf("Len = %d, want 2", c.Len())
	}
	var refetched atomic.Int64
	if _, err := c.Get(ctx, "a", func(context.Context) (int, error) {
		refetched.Add(1)
		return 1, nil
	}); err != nil {
		t.Fatal(err)
	}
	if refetched.Load() != 1 {
		t.Error(`"a" was not evicted; eviction must drop the oldest entry`)
	}
}
