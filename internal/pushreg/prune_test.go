package pushreg_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/pushreg"
)

// base is a fixed instant the test schedules cutoffs relative to, so
// RunPruneLoop's injected now func never touches the wall clock.
var base = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

// fakePruneRepo is a pushreg.Repository stub that only implements Prune (the
// only method RunPruneLoop calls); the rest panic if ever reached, so a
// bug that starts calling them fails loudly instead of silently no-oping.
// It records every cutoff it was called with, and can be told to fail its
// first call to exercise the error-then-continue path.
type fakePruneRepo struct {
	mu        sync.Mutex
	cutoffs   []time.Time
	failFirst bool
	calls     int
}

func (r *fakePruneRepo) Prune(_ context.Context, cutoff time.Time) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.cutoffs = append(r.cutoffs, cutoff)
	if r.failFirst && r.calls == 1 {
		return 0, errors.New("boom")
	}
	return 0, nil
}

func (r *fakePruneRepo) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.cutoffs)
}

func (r *fakePruneRepo) Upsert(context.Context, pushreg.Upsert, time.Time) error {
	panic("RunPruneLoop must not call Upsert")
}

func (r *fakePruneRepo) Get(context.Context, int64, string) (pushreg.Registration, error) {
	panic("RunPruneLoop must not call Get")
}

func (r *fakePruneRepo) Delete(context.Context, int64, string) error {
	panic("RunPruneLoop must not call Delete")
}

func (r *fakePruneRepo) DeleteByToken(context.Context, string) (int64, error) {
	panic("RunPruneLoop must not call DeleteByToken")
}

// TestRunPruneLoop_PrunesImmediately proves the loop prunes once
// immediately, before the first tick -- mirroring regions.RunSyncLoop, per
// the brief, so a long-stopped deployment catches up at boot instead of
// waiting a full interval. interval is deliberately far longer than the
// deadline this test allows for the first call: if RunPruneLoop only pruned
// on ticks, no call would ever land and the loop below would time out
// instead of observing one.
func TestRunPruneLoop_PrunesImmediately(t *testing.T) {
	t.Parallel()

	repo := &fakePruneRepo{}
	now := func() time.Time { return base }
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		pushreg.RunPruneLoop(ctx, repo, time.Hour, 180*24*time.Hour, now, logger)
		close(done)
	}()

	deadline := time.After(time.Second)
waitForCall:
	for {
		select {
		case <-deadline:
			cancel()
			t.Fatal("RunPruneLoop did not call Prune before the first tick (interval=1h, waited 1s)")
		default:
			if repo.callCount() >= 1 {
				break waitForCall
			}
			time.Sleep(time.Millisecond)
		}
	}
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("RunPruneLoop did not return after ctx was cancelled")
	}
}

// TestRunPruneLoop_ImmediateThenTicks proves the loop prunes again on
// subsequent ticks (not just the immediate first pass), each time with a
// cutoff of exactly now-maxAge, until ctx is cancelled.
func TestRunPruneLoop_ImmediateThenTicks(t *testing.T) {
	t.Parallel()

	repo := &fakePruneRepo{}
	now := func() time.Time { return base }
	maxAge := 180 * 24 * time.Hour
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		pushreg.RunPruneLoop(ctx, repo, 5*time.Millisecond, maxAge, now, logger)
		close(done)
	}()

	time.Sleep(25 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("RunPruneLoop did not return after ctx was cancelled")
	}

	// >= 2, not >= 1: the immediate first pass alone would already satisfy
	// "at least 1", so this specifically proves the ticker keeps firing
	// after it.
	if n := repo.callCount(); n < 2 {
		t.Fatalf("Prune called %d times over 25ms with a 5ms interval, want at least 2 (immediate pass + at least one tick)", n)
	}

	wantCutoff := base.Add(-maxAge)
	for i, got := range repo.cutoffs {
		if !got.Equal(wantCutoff) {
			t.Errorf("call %d: cutoff = %v, want %v (now - maxAge)", i, got, wantCutoff)
		}
	}
}

// TestRunPruneLoop_ErrorLogsAndContinues proves a Prune error is logged and
// does not stop the loop: a second call still happens on the next tick.
func TestRunPruneLoop_ErrorLogsAndContinues(t *testing.T) {
	t.Parallel()

	repo := &fakePruneRepo{failFirst: true}
	now := func() time.Time { return base }
	var logBuf syncBuffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		pushreg.RunPruneLoop(ctx, repo, 5*time.Millisecond, 180*24*time.Hour, now, logger)
		close(done)
	}()

	deadline := time.After(time.Second)
	for repo.callCount() < 2 {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for a second Prune call after the first errored")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("RunPruneLoop did not return after ctx was cancelled")
	}

	if !logBuf.Contains("boom") {
		t.Errorf("log output = %q, want the Prune error logged", logBuf.String())
	}
}

// syncBuffer is a concurrency-safe io.Writer + string accessor: the test
// goroutine writes to it via slog while the main goroutine polls it.
type syncBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	return len(p), nil
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

func (b *syncBuffer) Contains(substr string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.Contains(string(b.buf), substr)
}
