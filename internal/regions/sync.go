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

// RunSyncLoop runs Sync once immediately, then again every interval, until
// ctx is cancelled. A failed Sync is logged and does not stop the loop: the
// next tick tries again, and existing rows are left untouched by the
// failure.
func RunSyncLoop(ctx context.Context, client *Client, repo Repository, interval time.Duration, now func() time.Time, logger *slog.Logger) {
	runOnce := func() {
		if err := Sync(ctx, client, repo, now); err != nil {
			logger.Error("regions: sync failed", "error", err)
		}
	}

	runOnce()

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
