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

// Scheduler runs the §5.3 firing loop: once per cycle it lists every pending
// alarm, resolves each one's departure via OBA, and either waits, fires, or
// expires it.
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

// CheckAll runs one §5.3 cycle over every pending alarm. Exported so tests
// (and the loop wiring) drive cycles without a ticker.
func (s *Scheduler) CheckAll(ctx context.Context) {
	pending, err := s.Repo.List(ctx)
	if err != nil {
		s.Logger.Error("alarms: list pending", "err", err)
		return
	}

	// One region fetch per region per cycle, not per alarm.
	regionCache := make(map[int64]*regions.Region)
	var mu sync.Mutex
	regionFor := func(id int64) *regions.Region {
		mu.Lock()
		defer mu.Unlock()
		if r, ok := regionCache[id]; ok {
			return r
		}
		region, err := s.Regions.Get(ctx, id)
		if err != nil {
			regionCache[id] = nil
			return nil
		}
		regionCache[id] = &region
		return &region
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
}

func (s *Scheduler) check(ctx context.Context, alarm Alarm, region *regions.Region) {
	if region == nil {
		// An alarm whose region vanished can never resolve; let the streak
		// reap it rather than re-checking forever.
		s.countFailure(ctx, alarm)
		return
	}
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

// RunLoop calls CheckAll every interval until ctx is done (§5.3: once per
// minute). Mirrors regions.RunSyncLoop's ticker shape.
func (s *Scheduler) RunLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.CheckAll(ctx)
		}
	}
}
