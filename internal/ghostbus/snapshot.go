package ghostbus

// The §8 enrichment worker: a DB-as-queue polling loop. The report row is
// the queue entry -- there is no durable job queue in this deployment, and
// a request-time goroutine would strand reports 'pending' forever after a
// crash between the 201 and the capture. Single-instance by construction,
// like the alarm scheduler: one loop per process, one process per
// database.
//
// Enrichment is best-effort and never touches the rider path: every
// failure direction lands on snapshot_status = 'unavailable' (definitive)
// or a bounded retry (transient), and the poll predicate's attempts guard
// means no row is retried forever.

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/OneBusAway/sidecar/internal/obaapi"
	"github.com/OneBusAway/sidecar/internal/regions"
)

const (
	// SnapshotInterval is the poll cadence. Enrichment is not
	// latency-sensitive; 30s keeps the trip's realtime state close to what
	// the rider saw without hammering the store.
	SnapshotInterval = 30 * time.Second
	// snapshotBatchSize bounds one cycle's work. At the §2.6 write throttles'
	// ceiling a backlog deeper than this cannot accumulate from legitimate
	// traffic.
	snapshotBatchSize = 100
)

// TripDetailsSource is the one obaapi method the scheduler needs, declared
// consumer-side so tests fake one function instead of the whole Client
// (same pattern as alarms.DepartureSource).
type TripDetailsSource interface {
	TripDetails(ctx context.Context, region regions.Region, q obaapi.TripDetailsQuery) (json.RawMessage, error)
}

// SnapshotScheduler runs the §8 enrichment loop: once per cycle it lists
// every pending report, resolves each one's trip-details snapshot via OBA,
// and marks it captured, unavailable, or leaves it pending for a bounded
// retry.
type SnapshotScheduler struct {
	Repo    Repository
	Regions regions.Repository
	OBA     TripDetailsSource
	Now     func() time.Time
	Logger  *slog.Logger
}

// CheckAll runs one polling cycle: claim up to snapshotBatchSize pending
// reports and try to capture each. Sequential deliberately -- the batch is
// small, the upstream deserves politeness, and nothing downstream waits on
// this loop.
func (s *SnapshotScheduler) CheckAll(ctx context.Context) {
	pending, err := s.Repo.ListPendingSnapshots(ctx, snapshotBatchSize)
	if err != nil {
		s.Logger.Error("ghostbus: list pending snapshots", "err", err)
		return
	}
	// One region fetch per region per cycle. The error is cached alongside
	// the region because "region is gone" (unavailable) and "store
	// hiccuped" (skip, retry next cycle) are different facts -- same
	// distinction the alarm scheduler draws.
	type lookup struct {
		region *regions.Region
		err    error
	}
	cache := map[int64]lookup{}
	for _, rep := range pending {
		l, ok := cache[rep.RegionID]
		if !ok {
			region, err := s.Regions.Get(ctx, rep.RegionID)
			l = lookup{err: err}
			if err == nil {
				l.region = &region
			}
			cache[rep.RegionID] = l
		}
		s.capture(ctx, rep, l.region, l.err)
	}
}

func (s *SnapshotScheduler) capture(ctx context.Context, rep Report, region *regions.Region, regionErr error) {
	if region == nil {
		if !errors.Is(regionErr, regions.ErrNotFound) {
			// Transient store failure: a fact about the database, not this
			// report. Recording anything would let one bad minute of SQLite
			// mark every pending report unavailable.
			s.Logger.Warn("ghostbus: resolve region", "region_id", rep.RegionID, "err", regionErr)
			return
		}
		s.markUnavailable(ctx, rep, "region gone")
		return
	}
	snap, err := s.OBA.TripDetails(ctx, *region, obaapi.TripDetailsQuery{
		TripID:      rep.TripIdentifier,
		ServiceDate: rep.ServiceDate,
		RouteID:     rep.RouteIdentifier,
		StopID:      rep.StopIdentifier,
	})
	switch {
	case errors.Is(err, obaapi.ErrNotFound), errors.Is(err, obaapi.ErrNotConfigured):
		// Definitive: the trip is unknown upstream, or the region has no
		// key and never will resolve. Retrying cannot help.
		s.markUnavailable(ctx, rep, "lookup definitive miss")
		return
	case err != nil:
		// Transient. The repository flips the row to unavailable when the
		// increment reaches the cap -- in the same UPDATE, so a crash here
		// cannot strand a row both capped and pending.
		if _, ferr := s.Repo.RecordSnapshotFailure(ctx, rep.ID, s.Now()); ferr != nil {
			s.Logger.Warn("ghostbus: record snapshot failure", "region_id", rep.RegionID, "err", ferr)
		}
		s.Logger.Warn("ghostbus: snapshot lookup failed", "region_id", rep.RegionID, "err", err)
		return
	}
	if err := s.Repo.MarkSnapshotCaptured(ctx, rep.ID, string(snap), s.Now()); err != nil {
		s.Logger.Error("ghostbus: mark snapshot captured", "region_id", rep.RegionID, "err", err)
	}
}

func (s *SnapshotScheduler) markUnavailable(ctx context.Context, rep Report, why string) {
	if err := s.Repo.MarkSnapshotUnavailable(ctx, rep.ID, s.Now()); err != nil {
		s.Logger.Warn("ghostbus: mark snapshot unavailable", "err", err)
		return
	}
	s.Logger.Info("ghostbus: snapshot unavailable", "region_id", rep.RegionID, "reason", why)
}

// The SnapshotInterval cadence is cmd/sidecar's, through a lease.Runner,
// as for alarms.Scheduler.CheckAll.
