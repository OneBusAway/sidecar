package alertpush

import (
	"context"
	"fmt"
	"time"

	"github.com/OneBusAway/sidecar/internal/alerts"
	"github.com/OneBusAway/sidecar/internal/pushreg"
)

// Enqueuer applies the send preconditions (design spec §2.2) and inserts
// the queued push. It is shared by the admin API and the CLI so the two
// trigger surfaces cannot drift.
type Enqueuer struct {
	Repo     Repository
	Alerts   alerts.Repository
	PushRegs pushreg.Repository

	// AlertSpacing and RegionHourlyLimit are the SC-27 cooldown on "all"
	// pushes; zero disables each. The admin API sets them, the CLI does not.
	AlertSpacing      time.Duration
	RegionHourlyLimit int
}

// AudienceReport is the reach preview for one alert: both audiences'
// counts, and whether the alert's test flag forces the test audience.
type AudienceReport struct {
	All        pushreg.AudienceCount
	Test       pushreg.AudienceCount
	ForcedTest bool
}

// Enqueue validates and inserts a queued push for alertID. Errors:
// alerts.ErrNotFound, ErrNotPublished, ErrInFlight, ErrEmptyAudience,
// ErrInvalidMessages. A test alert is always sent to the test audience
// regardless of audience. A nil messages derives the copy snapshot from the
// alert (BuildMessages); a non-nil one is the caller's own copy, validated
// by ValidateMessages and stored verbatim -- OBACloud's copywriter output
// (migration design spec section 2.3).
func (e *Enqueuer) Enqueue(ctx context.Context, alertID int64, audience Audience, messages Messages, now time.Time) (Push, error) {
	if messages != nil {
		if err := ValidateMessages(messages); err != nil {
			return Push{}, err
		}
	}
	a, err := e.Alerts.Get(ctx, alertID)
	if err != nil {
		return Push{}, err
	}
	if !a.Published {
		return Push{}, ErrNotPublished
	}
	if a.IsTest {
		audience = AudienceTest
	}
	if audience == "" {
		audience = AudienceAll
	}
	inFlight, err := e.Repo.InFlightForAlert(ctx, alertID)
	if err != nil {
		return Push{}, err
	}
	if inFlight {
		return Push{}, ErrInFlight
	}
	if audience == AudienceAll {
		if cooldownErr := e.checkCooldown(ctx, a, now); cooldownErr != nil {
			return Push{}, cooldownErr
		}
	}
	count, err := e.PushRegs.CountAudience(ctx, a.RegionID, audience == AudienceTest)
	if err != nil {
		return Push{}, fmt.Errorf("alertpush: count audience: %w", err)
	}
	if count.Total == 0 {
		return Push{}, ErrEmptyAudience
	}
	if messages == nil {
		messages = BuildMessages(a)
	}
	return e.Repo.Create(ctx, NewPush{
		AlertID: a.ID, RegionID: a.RegionID, Audience: audience, Messages: messages,
	}, now)
}

// AudienceFor reports both audiences' sizes for alertID's region.
func (e *Enqueuer) AudienceFor(ctx context.Context, alertID int64) (AudienceReport, error) {
	a, err := e.Alerts.Get(ctx, alertID)
	if err != nil {
		return AudienceReport{}, err
	}
	all, err := e.PushRegs.CountAudience(ctx, a.RegionID, false)
	if err != nil {
		return AudienceReport{}, fmt.Errorf("alertpush: count audience: %w", err)
	}
	test, err := e.PushRegs.CountAudience(ctx, a.RegionID, true)
	if err != nil {
		return AudienceReport{}, fmt.Errorf("alertpush: count test audience: %w", err)
	}
	return AudienceReport{All: all, Test: test, ForcedTest: a.IsTest}, nil
}

// checkCooldown applies SC-27 to an "all" push of a: no second one for the
// same alert within AlertSpacing of the last, and no more than
// RegionHourlyLimit in the region per trailing hour. Only pushes that may
// have reached a device count. The test audience never gets here: it is how
// operators check copy, and it reaches only their own devices.
func (e *Enqueuer) checkCooldown(ctx context.Context, a alerts.Alert, now time.Time) error {
	if e.AlertSpacing > 0 {
		prior, err := e.Repo.ListByAlert(ctx, a.ID)
		if err != nil {
			return fmt.Errorf("alertpush: list pushes for cooldown: %w", err)
		}
		for _, p := range prior {
			if p.Audience == AudienceAll && p.MayHaveReached() && now.Sub(p.CreatedAt) < e.AlertSpacing {
				return fmt.Errorf("%w: this alert was pushed to all devices less than %s ago; wait before sending it again",
					ErrCooldown, e.AlertSpacing)
			}
		}
	}
	if e.RegionHourlyLimit > 0 {
		n, err := e.Repo.CountReachingAllSince(ctx, a.RegionID, now.Add(-time.Hour))
		if err != nil {
			return fmt.Errorf("alertpush: count pushes for cooldown: %w", err)
		}
		if n >= int64(e.RegionHourlyLimit) {
			return fmt.Errorf("%w: this region has already sent %d pushes to all devices in the last hour; wait before sending another",
				ErrCooldown, n)
		}
	}
	return nil
}
