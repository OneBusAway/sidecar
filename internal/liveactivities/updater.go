package liveactivities

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/OneBusAway/sidecar/internal/cache"
	"github.com/OneBusAway/sidecar/internal/obaapi"
	"github.com/OneBusAway/sidecar/internal/push"
	"github.com/OneBusAway/sidecar/internal/regions"
)

// ArrivalsSource is the one obaapi method the updater needs, declared
// consumer-side so tests fake three lines instead of the whole Client.
type ArrivalsSource interface {
	ArrivalsAndDeparturesForStop(ctx context.Context, region regions.Region, q obaapi.StopArrivalsQuery) ([]obaapi.StopArrival, error)
}

// checkConcurrency bounds parallel checks per cycle, matching alarms.
const checkConcurrency = 8

// stopCacheEntries bounds the per-stop cache; beyond this many distinct
// stops per 55s the oldest are evicted and refetched.
const stopCacheEntries = 1024

// Updater runs the §6.3 update cycle: once per cycle it lists every
// subscription, builds its content state from a per-stop shared fetch, and
// pushes an update, a keepalive, or an end.
type Updater struct {
	Repo    Repository
	Regions regions.Repository
	OBA     ArrivalsSource
	// Sender may be nil (store-only mode: no push transport configured).
	// Expiry and reaping still run so rows cannot accumulate; nothing is
	// sent or recorded as pushed.
	Sender push.LiveActivitySender
	Now    func() time.Time
	Logger *slog.Logger

	stops *cache.Cache[[]obaapi.StopArrival]
}

// NewUpdater builds an Updater whose per-stop cache reads the injected
// clock (design spec §2.6).
func NewUpdater(repo Repository, regionRepo regions.Repository, oba ArrivalsSource,
	sender push.LiveActivitySender, now func() time.Time, logger *slog.Logger) *Updater {
	return &Updater{
		Repo: repo, Regions: regionRepo, OBA: oba, Sender: sender, Now: now, Logger: logger,
		stops: cache.New[[]obaapi.StopArrival](StopCacheTTL, stopCacheEntries, StopFetchBudget, now),
	}
}

// regionLookup is one cycle's cached resolution of a region (see
// alarms.Scheduler for why the error is kept alongside the region).
type regionLookup struct {
	region *regions.Region
	err    error
}

// CheckAll runs one cycle over every subscription. Exported so tests and
// the loop wiring drive cycles without a ticker.
func (u *Updater) CheckAll(ctx context.Context) {
	rows, err := u.Repo.List(ctx)
	if err != nil {
		u.Logger.Error("liveactivities: list", "err", err)
		return
	}
	regionCache := make(map[int64]regionLookup)
	var mu sync.Mutex
	regionFor := func(id int64) regionLookup {
		mu.Lock()
		r, ok := regionCache[id]
		mu.Unlock()
		if ok {
			return r
		}
		region, err := u.Regions.Get(ctx, id)
		resolved := regionLookup{err: err}
		if err == nil {
			resolved.region = &region
		}
		mu.Lock()
		regionCache[id] = resolved
		mu.Unlock()
		return resolved
	}

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(checkConcurrency)
	for _, la := range rows {
		g.Go(func() error {
			u.check(gctx, la, regionFor(la.RegionID))
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		u.Logger.Error("liveactivities: check cycle", "err", err)
	}
}

func (u *Updater) check(ctx context.Context, la LiveActivity, lookup regionLookup) {
	now := u.Now()
	if now.After(la.ExpiresAt) {
		u.end(ctx, la, "expired")
		return
	}
	if lookup.region == nil {
		if !errors.Is(lookup.err, regions.ErrNotFound) {
			// A store hiccup is a fact about the database, not this row.
			u.Logger.Warn("liveactivities: resolve region", "region_id", la.RegionID, "err", lookup.err)
			return
		}
		u.countFailure(ctx, la, "region not found")
		return
	}

	entries, err := u.fetch(ctx, *lookup.region, la.StopID)
	if err != nil {
		// Spec §6.3 step 2: OBA/network errors count, unlike the alarm
		// scheduler -- a Live Activity that cannot be updated is worthless
		// and three minutes is the cutoff. Don't "fix" this to match alarms.
		u.countFailure(ctx, la, "fetch failed")
		return
	}
	state := BuildContentState(entries, la.RouteShortName, la.TripHeadsign, now)
	if len(state.Arrivals) == 0 {
		// Night headways and feed gaps produce valid-but-empty responses on
		// healthy subscriptions; only a streak ends the activity.
		u.countFailure(ctx, la, "no matching upcoming arrivals")
		return
	}
	if la.ConsecutiveFailures > 0 {
		if resetErr := u.Repo.ResetFailures(ctx, la.ID); resetErr != nil {
			u.Logger.Warn("liveactivities: reset failures", "region_id", la.RegionID, "err", resetErr)
		}
	}

	if !Changed(la.LastContentState, state) && !keepaliveDue(la.LastPushedAt, now) {
		return
	}
	if u.Sender == nil {
		return // store-only mode
	}
	err = u.Sender.SendLiveActivity(ctx, push.LiveActivityPush{
		Token: la.PushToken, Sandbox: la.APNSSandbox, Event: "update",
		ContentState: state, Timestamp: pushTimestamp(la.LastPushedAt, now),
		StaleDate: now.Add(StaleAfter),
	})
	if err != nil {
		// Leave the row; next cycle retries. Not a §6.3 failure: the
		// upstream is fine, our transport is not.
		u.Logger.Error("liveactivities: update push failed", "region_id", la.RegionID, "err", err)
		return
	}
	if err := u.Repo.RecordPush(ctx, la.ID, state, now); err != nil {
		u.Logger.Error("liveactivities: record push", "region_id", la.RegionID, "err", err)
	}
}

// fetch shares one upstream call per (region, stop) per StopCacheTTL across
// every subscription on that stop (spec §6.3 cost control).
func (u *Updater) fetch(ctx context.Context, region regions.Region, stopID string) ([]obaapi.StopArrival, error) {
	key := fmt.Sprintf("%d/%s", region.ID, stopID)
	return u.stops.Get(ctx, key, func(ctx context.Context) ([]obaapi.StopArrival, error) {
		return u.OBA.ArrivalsAndDeparturesForStop(ctx, region, obaapi.StopArrivalsQuery{
			StopID: stopID, MinutesBefore: LookbackMinutes, MinutesAfter: LookaheadMinutes,
		})
	})
}

// keepaliveDue reports whether KeepaliveInterval has elapsed (>=, a
// deliberate boundary choice: the intent is to push on every cycle).
func keepaliveDue(lastPushedAt *time.Time, now time.Time) bool {
	return lastPushedAt == nil || now.Sub(*lastPushedAt) >= KeepaliveInterval
}

// pushTimestamp is max(now, last+1s): APNs drops a Live Activity push whose
// timestamp does not advance (design spec §2.5).
func pushTimestamp(lastPushedAt *time.Time, now time.Time) time.Time {
	if lastPushedAt != nil && !now.After(*lastPushedAt) {
		return lastPushedAt.Add(time.Second)
	}
	return now
}

func (u *Updater) countFailure(ctx context.Context, la LiveActivity, reason string) {
	streak, err := u.Repo.RecordFailure(ctx, la.ID)
	if err != nil {
		u.Logger.Warn("liveactivities: record failure", "region_id", la.RegionID, "err", err)
		return
	}
	u.Logger.Warn("liveactivities: "+reason, "region_id", la.RegionID, "stop_id", la.StopID, "streak", streak)
	if streak >= MaxConsecutiveFailures {
		u.end(ctx, la, reason)
	}
}

// end sends a best-effort end push and ALWAYS deletes the row (spec §6.4):
// a dead token must not keep the row being re-checked forever.
func (u *Updater) end(ctx context.Context, la LiveActivity, reason string) {
	now := u.Now()
	u.Logger.Info("liveactivities: ending", "region_id", la.RegionID, "stop_id", la.StopID, "reason", reason)
	if u.Sender != nil {
		state := la.LastContentState
		if state.Arrivals == nil {
			state = EmptyContentState()
		}
		err := u.Sender.SendLiveActivity(ctx, push.LiveActivityPush{
			Token: la.PushToken, Sandbox: la.APNSSandbox, Event: "end",
			ContentState: state, Timestamp: pushTimestamp(la.LastPushedAt, now),
			DismissalDate: now.Add(DismissAfterEnd),
		})
		if err != nil {
			u.Logger.Warn("liveactivities: best-effort end push failed", "region_id", la.RegionID, "err", err)
		}
	}
	if err := u.Repo.DeleteByID(ctx, la.ID); err != nil {
		u.Logger.Error("liveactivities: delete ended activity", "region_id", la.RegionID, "err", err)
	}
}

// RunLoop calls CheckAll every interval until ctx is done (§6.3: once per
// minute). Mirrors alarms.Scheduler.RunLoop.
func (u *Updater) RunLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			u.CheckAll(ctx)
		}
	}
}
