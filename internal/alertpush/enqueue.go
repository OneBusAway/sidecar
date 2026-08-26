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
}

// AudienceReport is the reach preview for one alert: both audiences'
// counts, and whether the alert's test flag forces the test audience.
type AudienceReport struct {
	All        pushreg.AudienceCount
	Test       pushreg.AudienceCount
	ForcedTest bool
}

// Enqueue validates and inserts a queued push for alertID. Errors:
// alerts.ErrNotFound, ErrNotPublished, ErrInFlight, ErrEmptyAudience. A
// test alert is always sent to the test audience regardless of audience.
func (e *Enqueuer) Enqueue(ctx context.Context, alertID int64, audience Audience, now time.Time) (Push, error) {
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
	count, err := e.PushRegs.CountAudience(ctx, a.RegionID, audience == AudienceTest)
	if err != nil {
		return Push{}, fmt.Errorf("alertpush: count audience: %w", err)
	}
	if count.Total == 0 {
		return Push{}, ErrEmptyAudience
	}
	return e.Repo.Create(ctx, NewPush{
		AlertID: a.ID, RegionID: a.RegionID, Audience: audience, Messages: BuildMessages(a),
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
