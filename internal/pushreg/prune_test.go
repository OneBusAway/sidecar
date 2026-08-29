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
// Prune's injected now func never touches the wall clock.
var base = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

// fakePruneRepo is a pushreg.Repository stub that only implements Prune (the
// only method Prune calls); the rest panic if ever reached, so a
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
	panic("Prune must not call Upsert")
}

func (r *fakePruneRepo) Get(context.Context, int64, string) (pushreg.Registration, error) {
	panic("Prune must not call Get")
}

func (r *fakePruneRepo) Delete(context.Context, int64, string) error {
	panic("Prune must not call Delete")
}

func (r *fakePruneRepo) DeleteByToken(context.Context, string) (int64, error) {
	panic("Prune must not call DeleteByToken")
}

func (r *fakePruneRepo) ListAudience(context.Context, int64, bool, int64, int) ([]pushreg.Registration, error) {
	panic("Prune must not call ListAudience")
}

func (r *fakePruneRepo) CountAudience(context.Context, int64, bool) (pushreg.AudienceCount, error) {
	panic("Prune must not call CountAudience")
}

// TestPrune_CutoffIsNowMinusMaxAge proves one pass prunes exactly once,
// with a cutoff of now-maxAge.
func TestPrune_CutoffIsNowMinusMaxAge(t *testing.T) {
	t.Parallel()

	repo := &fakePruneRepo{}
	maxAge := 180 * 24 * time.Hour
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	pushreg.Prune(context.Background(), repo, maxAge, func() time.Time { return base }, logger)

	if n := repo.callCount(); n != 1 {
		t.Fatalf("Prune called %d times, want 1", n)
	}
	if want := base.Add(-maxAge); !repo.cutoffs[0].Equal(want) {
		t.Errorf("cutoff = %v, want %v (now - maxAge)", repo.cutoffs[0], want)
	}
}

// TestPrune_ErrorIsLogged proves a repository error is logged rather than
// swallowed -- the pass has no caller to return it to.
func TestPrune_ErrorIsLogged(t *testing.T) {
	t.Parallel()

	repo := &fakePruneRepo{failFirst: true}
	var logBuf syncBuffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))

	pushreg.Prune(context.Background(), repo, 180*24*time.Hour, func() time.Time { return base }, logger)

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
