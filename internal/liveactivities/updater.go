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
	u := &Updater{Repo: repo, Regions: regionRepo, OBA: oba, Sender: sender, Now: now, Logger: logger}
	u.stops = u.newStopCache()
	return u
}

func (u *Updater) newStopCache() *cache.Cache[[]obaapi.StopArrival] {
	return cache.New[[]obaapi.StopArrival](StopCacheTTL, stopCacheEntries, StopFetchBudget, u.Now)
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
	started := u.Now()
	rows, err := u.Repo.List(ctx)
	if err != nil {
		u.Logger.Error("liveactivities: list", "err", err)
		return
	}
	// The stop cache exists to share one upstream call across every
	// subscription on a stop WITHIN a cycle. Its TTL is stamped when a fetch
	// completes, so an entry filled late in a long sweep (many rows, a slow
	// upstream) would still be live when the next cycle starts and that stop
	// would push a keepalive built from minute-old arrivals. Starting every
	// cycle with a fresh cache makes the TTL a bound, not the mechanism.
	u.stops = u.newStopCache()
	// One region fetch per region per cycle, not per subscription. The
	// failure is cached alongside the region: check has to tell "this region
	// is gone" (count/reap the subscription) apart from "the store
	// hiccuped" (leave it alone), so a nil region on its own is not enough
	// information (mirrors alarms.Scheduler.CheckAll).
	regionCache := make(map[int64]regionLookup)
	var mu sync.Mutex
	regionFor := func(id int64) regionLookup {
		mu.Lock()
		r, ok := regionCache[id]
		mu.Unlock()
		if ok {
			return r
		}
		// Fetched with the lock released: checkConcurrency goroutines share
		// this cache, and holding mu across the store round trip would make
		// the first touch of two different regions serialize on the database
		// instead of overlapping. A simultaneous first touch of the same
		// region can now fetch twice, which is far cheaper than serializing
		// every miss -- and both writers store the same value.
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
	u.Logger.Info("liveactivities: cycle", "activities", len(rows), "ms", u.Now().Sub(started).Milliseconds())
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
	ts := pushTimestamp(la.LastPushedAt, now)
	err = u.Sender.SendLiveActivity(ctx, push.LiveActivityPush{
		Token: la.PushToken, Sandbox: la.APNSSandbox, Event: "update",
		ContentState: state, Timestamp: ts,
		StaleDate: now.Add(StaleAfter),
	})
	if err != nil {
		// Leave the row; next cycle retries. Not a §6.3 failure: the
		// upstream is fine, our transport is not.
		u.Logger.Error("liveactivities: update push failed", "region_id", la.RegionID, "err", err)
		return
	}
	// Record the later of now and ts: when pushTimestamp bumped past now
	// (the last+1s branch, clock stalled or moving slower than pushes), the
	// stored watermark must not trail the timestamp APNs actually saw, or
	// the next cycle could compute a pushTimestamp that repeats or goes
	// backwards relative to what was delivered.
	recordedAt := now
	if ts.After(recordedAt) {
		recordedAt = ts
	}
	if err := u.Repo.RecordPush(ctx, la.ID, state, recordedAt); err != nil {
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

// pushTimestamp is max(now, last+1s) at whole-second granularity: APNs
// drops a Live Activity push whose timestamp does not advance (design spec
// §2.5), and the wire value is epoch seconds (as is the stored watermark),
// so a sub-second advance is no advance at all.
func pushTimestamp(lastPushedAt *time.Time, now time.Time) time.Time {
	ts := now.Truncate(time.Second)
	if lastPushedAt != nil {
		last := lastPushedAt.Truncate(time.Second)
		if !ts.After(last) {
			return last.Add(time.Second)
		}
	}
	return ts
}

func (u *Updater) countFailure(ctx context.Context, la LiveActivity, reason string) {
	streak, err := u.Repo.RecordFailure(ctx, la.ID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			// The row was deleted between List and now (rider DELETE or the
			// feedback webhook); gone is the goal, same as DeleteByID.
			return
		}
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
	// Compare-and-delete on the revision this sweep listed: a token
	// rotation that re-registered the row since then must survive, or the
	// phone's new token never hears from us again. The end push above went
	// to the old token, which is what the rotation retired -- harmless.
	deleted, err := u.Repo.DeleteByID(ctx, la.ID, la.Revision)
	if err != nil {
		u.Logger.Error("liveactivities: delete ended activity", "region_id", la.RegionID, "err", err)
		return
	}
	if !deleted {
		u.Logger.Info("liveactivities: ended row was re-registered mid-sweep; kept",
			"region_id", la.RegionID, "stop_id", la.StopID, "reason", reason)
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
