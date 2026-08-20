package pushreg

import (
	"context"
	"log/slog"
	"time"
)

// RunPruneLoop deletes registrations unseen for maxAge, every interval, until
// ctx is done (spec §4: 180 days; §12: daily). Mirrors regions.RunSyncLoop:
// an immediate first pass, then the ticker, so a long-stopped deployment
// catches up at boot instead of a day later.
//
// now is injected rather than read from the wall clock, per the package-wide
// time.Now ban (time.Now is only allowed in cmd/). A Prune error is logged
// and the loop continues -- a transient database hiccup must not stop future
// pruning, since RunPruneLoop is the only thing bounding the registrations
// table's growth.
func RunPruneLoop(ctx context.Context, repo Repository, interval, maxAge time.Duration,
	now func() time.Time, logger *slog.Logger) {
	prune := func() {
		n, err := repo.Prune(ctx, now().Add(-maxAge))
		if err != nil {
			logger.Error("pushreg: prune", "err", err)
			return
		}
		if n > 0 {
			logger.Info("pushreg: pruned stale registrations", "count", n)
		}
	}

	prune()

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			prune()
		}
	}
}
