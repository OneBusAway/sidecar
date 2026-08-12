package regions

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// Sync fetches the directory once and upserts the result into repo. It never
// deletes: a region absent from a fetch keeps its row and its alerts, which
// cascade from regions on delete. now is injected rather than read from the
// wall clock so callers stay testable and the package-wide time.Now ban
// holds.
//
// A failed fetch returns an error and leaves repo untouched, so the feed
// keeps serving the last known good data instead of going dark because the
// directory is temporarily unreachable.
func Sync(ctx context.Context, client *Client, repo Repository, now func() time.Time) error {
	fetched, err := client.Fetch(ctx)
	if err != nil {
		return fmt.Errorf("regions: sync: %w", err)
	}
	if err := repo.UpsertFromDirectory(ctx, fetched, now()); err != nil {
		return fmt.Errorf("regions: sync: upsert: %w", err)
	}
	return nil
}

// minSyncInterval is the floor RunSyncLoop enforces on interval.
// time.NewTicker panics on a duration <= 0; cmd/sidecar validates --refresh
// before it ever reaches here, but RunSyncLoop is exported and this is a
// goroutine with no recover, so a bad value from any other caller (a test, a
// future caller) must not be able to crash the process this way.
const minSyncInterval = time.Minute

// RunSyncLoop runs Sync once immediately, then again every interval, until
// ctx is cancelled. A failed Sync is logged and does not stop the loop: the
// next tick tries again, and existing rows are left untouched by the
// failure.
//
// interval must be positive; a non-positive value is logged and replaced
// with minSyncInterval rather than being passed to time.NewTicker, which
// panics on one.
func RunSyncLoop(ctx context.Context, client *Client, repo Repository, interval time.Duration, now func() time.Time, logger *slog.Logger) {
	runOnce := func() {
		if err := Sync(ctx, client, repo, now); err != nil {
			logger.Error("regions: sync failed", "error", err)
		}
	}

	runOnce()

	if interval <= 0 {
		logger.Error("regions: invalid refresh interval, using fallback", "interval", interval, "fallback", minSyncInterval)
		interval = minSyncInterval
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runOnce()
		}
	}
}
