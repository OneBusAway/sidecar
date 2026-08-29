package pushreg

import (
	"context"
	"log/slog"
	"time"
)

// Prune is one pruning pass: it deletes registrations unseen for maxAge
// (spec §4: 180 days). cmd/sidecar runs it daily (§12) through a
// lease.Runner, whose boot-time first tick is what lets a long-stopped
// deployment catch up at once instead of a day later.
//
// now is injected rather than read from the wall clock, per the package-wide
// time.Now ban (time.Now is only allowed in cmd/). A Prune error is logged,
// not returned: the next pass tries again, and nothing upstream can do more
// than that -- this is the only thing bounding the registrations table's
// growth, so it must keep being scheduled.
func Prune(ctx context.Context, repo Repository, maxAge time.Duration, now func() time.Time, logger *slog.Logger) {
	n, err := repo.Prune(ctx, now().Add(-maxAge))
	if err != nil {
		logger.Error("pushreg: prune", "err", err)
		return
	}
	if n > 0 {
		logger.Info("pushreg: pruned stale registrations", "count", n)
	}
}
