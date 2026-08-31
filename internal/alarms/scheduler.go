package alarms

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/OneBusAway/sidecar/internal/obaapi"
	"github.com/OneBusAway/sidecar/internal/push"
	"github.com/OneBusAway/sidecar/internal/pushreg"
	"github.com/OneBusAway/sidecar/internal/regions"
)

// DepartureSource is the one obaapi method the scheduler needs, declared
// consumer-side so tests fake three lines instead of the whole Client.
type DepartureSource interface {
	ArrivalAndDeparture(ctx context.Context, region regions.Region, q obaapi.DepartureQuery) (obaapi.Departure, error)
}

// checkConcurrency bounds parallel OBA lookups per cycle; alarms across
// riders are independent, but the upstream deserves the same politeness as
// the vehicle fan-out.
const checkConcurrency = 8

// maxLookupFailures is the §5.3 reaping threshold: an alarm whose OBA lookup
// fails this many cycles in a row (ErrNotFound/ErrNotConfigured, or an
// unresolvable region) can never fire and is deleted rather than checked
// forever.
const maxLookupFailures = 3

// MinDeferral is the shortest time a Wait is allowed to push an alarm's
// next check out by. Below it, deferring saves at most a lookup or two per
// alarm while widening the window in which an early-running bus goes
// unnoticed, so the alarm stays on the once-a-minute cadence instead.
const MinDeferral = 2 * time.Minute

// MaxDeferral caps a single deferral. Halving already bounds how far a
// departure can move earlier unnoticed to half the slack, but for an alarm
// set a day ahead that is still twelve hours; an hour keeps the lookups
// saved (a handful either way) and bounds the blind window.
const MaxDeferral = time.Hour

// Scheduler runs the §5.3 firing loop: once per cycle it lists every alarm
// that is due (see ListDue and deferCheck), resolves each one's departure
// via OBA, and either waits, fires, or expires it.
type Scheduler struct {
	Repo    Repository
	Regions regions.Repository
	OBA     DepartureSource
	// Sender may be nil (store-only mode: no push transport configured).
	// The lifecycle bookkeeping -- expiry, reaping -- still runs; only the
	// fire step is skipped, leaving the alarm to expire.
	Sender push.Sender
	Now    func() time.Time
	Logger *slog.Logger
}

// CheckAll runs one §5.3 cycle over every alarm that is due (a Wait can
// defer an alarm's next check; see deferCheck). Exported so tests (and the
// loop wiring) drive cycles without a ticker.
func (s *Scheduler) CheckAll(ctx context.Context) {
	started := s.Now()
	pending, err := s.Repo.ListDue(ctx, started)
	if err != nil {
		s.Logger.Error("alarms: list pending", "err", err)
		return
	}

	// One region fetch per region per cycle, not per alarm. The failure is
	// cached alongside the region: check has to tell "this region is gone"
	// (reap the alarm) apart from "the store hiccuped" (leave it alone), so
	// a nil region on its own is not enough information.
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
		region, err := s.Regions.Get(ctx, id)
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
	for _, alarm := range pending {
		g.Go(func() error {
			s.check(gctx, alarm, regionFor(alarm.RegionID))
			return nil
		})
	}
	// check never returns an error, so Wait always returns nil; the check
	// still satisfies errcheck and matches obaapi's errgroup convention.
	if err := g.Wait(); err != nil {
		s.Logger.Error("alarms: check cycle", "err", err)
	}
	s.Logger.Info("alarms: cycle", "alarms", len(pending), "ms", s.Now().Sub(started).Milliseconds())
}

// regionLookup is one cycle's cached resolution of a region: the region if
// it resolved, or the error that stopped it. The error is kept because the
// two failure directions are not the same fact -- a region that is gone
// dooms its alarms, a store that is briefly unavailable says nothing about
// them.
type regionLookup struct {
	region *regions.Region
	err    error
}

func (s *Scheduler) check(ctx context.Context, alarm Alarm, lookup regionLookup) {
	if lookup.region == nil {
		if !errors.Is(lookup.err, regions.ErrNotFound) {
			// A transient store failure is a fact about the database, not
			// about this alarm. Counting it would let one bad minute of
			// SQLite reap every pending alarm in the deployment three
			// cycles later; same rule as the OBA transient branch below.
			s.Logger.Warn("alarms: resolve region", "region_id", alarm.RegionID, "err", lookup.err)
			return
		}
		// An alarm whose region vanished can never resolve; let the streak
		// reap it rather than re-checking forever.
		s.countFailure(ctx, alarm)
		return
	}
	if alarm.StopID == "" || alarm.TripID == "" {
		// The client never supplied a trip identity -- §5.2 leaves those
		// fields unvalidated at creation -- so no lookup can ever resolve
		// this alarm: the SDK rejects an empty stop id locally, before any
		// request, and an absent trip id is a 4xx from the upstream. Neither
		// is the 404 the reaper counts, so without this the alarm is
		// immortal: re-checked, and logged, every cycle forever.
		s.countFailure(ctx, alarm)
		return
	}
	region := lookup.region
	dep, err := s.OBA.ArrivalAndDeparture(ctx, *region, obaapi.DepartureQuery{
		StopID: alarm.StopID, TripID: alarm.TripID, ServiceDate: alarm.ServiceDate,
		VehicleID: alarm.VehicleID, StopSequence: alarm.StopSequence,
	})
	switch {
	case errors.Is(err, obaapi.ErrNotFound), errors.Is(err, obaapi.ErrNotConfigured):
		// Trip aged out (or the region has no key and never will resolve):
		// both count toward the §5.3 reaping streak.
		s.countFailure(ctx, alarm)
		return
	case err != nil:
		// Transient upstream/network failure: deliberately uncounted.
		s.Logger.Warn("alarms: lookup failed", "region_id", alarm.RegionID, "err", err)
		return
	}

	if alarm.FailureCount > 0 {
		if err := s.Repo.ResetFailures(ctx, alarm.ID); err != nil {
			s.Logger.Warn("alarms: reset failures", "err", err)
		}
	}

	departureMs := dep.ScheduledDepartureTime
	if dep.Predicted && dep.PredictedDepartureTime > 0 {
		departureMs = dep.PredictedDepartureTime
	}
	// dep.*DepartureTime is epoch ms; Decide and Now both work in seconds.
	until := departureMs/1000 - s.Now().Unix()

	switch Decide(until, alarm.SecondsBefore) {
	case Wait:
		s.deferCheck(ctx, alarm, until-alarm.SecondsBefore)
		return
	case Expire:
		// The bus already left; waking the rider is worse than silence.
		if err := s.Repo.DeleteByID(ctx, alarm.ID); err != nil {
			s.Logger.Warn("alarms: delete expired", "err", err)
		}
	case Fire:
		if s.Sender == nil {
			// Store-only mode (no push transport configured): leave the
			// alarm; the Expire branch bounds its lifetime, and the boot
			// warning already told the operator pushes cannot happen.
			return
		}
		platform := push.PlatformIOS
		if alarm.OperatingSystem == pushreg.OSAndroid {
			platform = push.PlatformAndroid
		}
		err := s.Sender.Send(ctx, push.Notification{
			Tokens: []string{alarm.UserPushID}, Platform: platform,
			Sandbox: alarm.APNSSandbox, Title: "OneBusAway",
			Message: alarm.Message, Data: alarm.PushData(),
		})
		if err != nil {
			// Keep the alarm: it retries next cycle until the departure
			// passes. At-least-once beats losing the wake-up (spec §12).
			s.Logger.Error("alarms: push send failed", "region_id", alarm.RegionID, "err", err)
			return
		}
		// Delete only after the send returned: a crash in the gap re-fires,
		// which is the accepted duplicate (spec §12).
		if err := s.Repo.DeleteByID(ctx, alarm.ID); err != nil {
			s.Logger.Error("alarms: delete fired alarm", "err", err)
		}
	}
}

// deferCheck hides a waiting alarm from the sweep until halfway through its
// slack -- the seconds left before the fire window opens. This deliberately
// relaxes spec section 5.3's once-per-minute check for far-off alarms: the
// saved OBA lookups are the point. Halving, rather than sleeping until the
// window, keeps an early-running bus catchable: each re-check halves the
// remaining slack again (3h out becomes 90m, 45m, 22m, ...) until the next
// halving would be shorter than MinDeferral -- under 2*MinDeferral of
// slack -- and the alarm is back on the per-minute cadence; MaxDeferral
// caps the first steps for alarms set many hours ahead. A failed Defer
// just means one more minute-cadence check.
func (s *Scheduler) deferCheck(ctx context.Context, alarm Alarm, slackSeconds int64) {
	wait := min(time.Duration(slackSeconds/2)*time.Second, MaxDeferral)
	if wait < MinDeferral {
		return
	}
	if err := s.Repo.Defer(ctx, alarm.ID, s.Now().Add(wait)); err != nil {
		s.Logger.Warn("alarms: defer check", "region_id", alarm.RegionID, "alarm_id", alarm.ID, "err", err)
	}
}

func (s *Scheduler) countFailure(ctx context.Context, alarm Alarm) {
	streak, err := s.Repo.RecordFailure(ctx, alarm.ID)
	if err != nil {
		s.Logger.Warn("alarms: record failure", "err", err)
		return
	}
	if streak >= maxLookupFailures {
		if err := s.Repo.DeleteByID(ctx, alarm.ID); err != nil {
			s.Logger.Warn("alarms: reap unresolvable", "err", err)
			return
		}
		s.Logger.Info("alarms: reaped unresolvable alarm", "region_id", alarm.RegionID, "failures", streak)
	}
}
