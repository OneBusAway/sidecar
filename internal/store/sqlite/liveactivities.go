package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/OneBusAway/sidecar/internal/liveactivities"
	"github.com/OneBusAway/sidecar/internal/store/sqlite/gen"
)

// liveActivityRepo implements liveactivities.Repository. Error strings never
// embed tokens (device-addressable secrets, spec §13); callers still wrap
// with sanitizeToken as defense in depth.
type liveActivityRepo struct {
	q      *gen.Queries
	logger *slog.Logger
}

func (r *liveActivityRepo) fromRow(row gen.LiveActivity) liveactivities.LiveActivity {
	// One corrupt cell must not fail List for every subscription; the next
	// successful push overwrites it (design spec §3). The two ways a cell can
	// be unusable get distinct messages: a parse failure (err != nil) versus
	// valid JSON that decoded to a null "arrivals" (err == nil, Arrivals ==
	// nil) -- the latter should never be written by this adapter (RecordPush
	// and Upsert both normalize to an empty slice), so seeing it means
	// something else wrote the row.
	state := liveactivities.EmptyContentState()
	switch err := json.Unmarshal([]byte(row.LastContentState), &state); {
	case err != nil:
		r.logger.Warn("sqlite: live activity content state failed to parse; treating as empty",
			"id", row.ID, "region_id", row.RegionID, "err", err)
		state = liveactivities.EmptyContentState()
	case state.Arrivals == nil:
		r.logger.Warn("sqlite: live activity content state had a null arrivals list; treating as empty",
			"id", row.ID, "region_id", row.RegionID)
		state = liveactivities.EmptyContentState()
	}
	return liveactivities.LiveActivity{
		ID: row.ID, RegionID: row.RegionID, Token: row.Token, ActivityID: row.ActivityID,
		PushToken: row.PushToken, APNSSandbox: row.ApnsSandbox,
		StopID: row.StopID, RouteShortName: row.RouteShortName, TripHeadsign: row.TripHeadsign,
		TripID: row.TripID, ServiceDate: row.ServiceDate, VehicleID: row.VehicleID,
		StopSequence:        nullInt64ToPtr(row.StopSequence),
		LastContentState:    state,
		LastPushedAt:        nullUnixToTime(row.LastPushedAt),
		ConsecutiveFailures: row.ConsecutiveFailures,
		ExpiresAt:           unixToTime(row.ExpiresAt),
		CreatedAt:           unixToTime(row.CreatedAt),
	}
}

// Upsert tries the update first; sql.ErrNoRows means no such registration,
// so it inserts. Two concurrent first registrations both miss the update;
// the loser's insert violates live_activities_activity_idx and surfaces as
// ErrDuplicate for the caller's single retry (design spec §2.1).
func (r *liveActivityRepo) Upsert(ctx context.Context, in liveactivities.NewLiveActivity, now time.Time) (liveactivities.LiveActivity, error) {
	ts := now.Unix()
	row, err := r.q.UpdateLiveActivityRegistration(ctx, gen.UpdateLiveActivityRegistrationParams{
		PushToken: in.PushToken, ApnsSandbox: in.APNSSandbox,
		StopID: in.StopID, RouteShortName: in.RouteShortName, TripHeadsign: in.TripHeadsign,
		TripID: in.TripID, ServiceDate: in.ServiceDate, VehicleID: in.VehicleID,
		StopSequence: int64ToNullInt64(in.StopSequence),
		Now:          ts, RegionID: in.RegionID, ActivityID: in.ActivityID,
	})
	switch {
	case err == nil:
		return r.fromRow(row), nil
	case !errors.Is(err, sql.ErrNoRows):
		return liveactivities.LiveActivity{}, fmt.Errorf("sqlite: update live activity (region %d): %w", in.RegionID, err)
	}
	row, err = r.q.InsertLiveActivity(ctx, gen.InsertLiveActivityParams{
		RegionID: in.RegionID, Token: in.Token, ActivityID: in.ActivityID,
		PushToken: in.PushToken, ApnsSandbox: in.APNSSandbox,
		StopID: in.StopID, RouteShortName: in.RouteShortName, TripHeadsign: in.TripHeadsign,
		TripID: in.TripID, ServiceDate: in.ServiceDate, VehicleID: in.VehicleID,
		StopSequence: int64ToNullInt64(in.StopSequence),
		ExpiresAt:    in.ExpiresAt.Unix(), Now: ts,
	})
	if err != nil {
		return liveactivities.LiveActivity{}, mapInsertErr(err, in.RegionID)
	}
	return r.fromRow(row), nil
}

// mapInsertErr wraps an InsertLiveActivity error, translating the
// live_activities_activity_idx UNIQUE violation (two concurrent first
// registrations racing Upsert -- see Upsert's comment) into ErrDuplicate so
// the caller can retry once; every other error passes through wrapped only
// with context.
func mapInsertErr(err error, regionID int64) error {
	if strings.Contains(err.Error(), "UNIQUE constraint failed: live_activities.region_id") {
		return fmt.Errorf("sqlite: insert live activity (region %d): %w", regionID, liveactivities.ErrDuplicate)
	}
	return fmt.Errorf("sqlite: insert live activity (region %d): %w", regionID, err)
}

func (r *liveActivityRepo) Delete(ctx context.Context, regionID int64, token string) error {
	n, err := r.q.DeleteLiveActivityByToken(ctx, gen.DeleteLiveActivityByTokenParams{RegionID: regionID, Token: token})
	if err != nil {
		return fmt.Errorf("sqlite: delete live activity (region %d): %w", regionID, err)
	}
	if n == 0 {
		return fmt.Errorf("sqlite: delete live activity (region %d): %w", regionID, liveactivities.ErrNotFound)
	}
	return nil
}

// DeleteByID treats zero rows as success: the updater may race a rider's
// own DELETE, and either way the row being gone is the goal.
func (r *liveActivityRepo) DeleteByID(ctx context.Context, id int64) error {
	if _, err := r.q.DeleteLiveActivityByID(ctx, id); err != nil {
		return fmt.Errorf("sqlite: delete live activity %d: %w", id, err)
	}
	return nil
}

func (r *liveActivityRepo) DeleteByPushToken(ctx context.Context, pushToken string) (int64, error) {
	n, err := r.q.DeleteLiveActivitiesByPushToken(ctx, pushToken)
	if err != nil {
		return 0, fmt.Errorf("sqlite: delete live activities by push token: %w", err)
	}
	return n, nil
}

func (r *liveActivityRepo) List(ctx context.Context) ([]liveactivities.LiveActivity, error) {
	rows, err := r.q.ListLiveActivities(ctx)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list live activities: %w", err)
	}
	out := make([]liveactivities.LiveActivity, len(rows))
	for i, row := range rows {
		out[i] = r.fromRow(row)
	}
	return out, nil
}

func (r *liveActivityRepo) RecordFailure(ctx context.Context, id int64) (int64, error) {
	n, err := r.q.RecordLiveActivityFailure(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("sqlite: record failure for live activity %d: %w", id, liveactivities.ErrNotFound)
		}
		return 0, fmt.Errorf("sqlite: record failure for live activity %d: %w", id, err)
	}
	return n, nil
}

func (r *liveActivityRepo) ResetFailures(ctx context.Context, id int64) error {
	if err := r.q.ResetLiveActivityFailures(ctx, id); err != nil {
		return fmt.Errorf("sqlite: reset failures for live activity %d: %w", id, err)
	}
	return nil
}

func (r *liveActivityRepo) RecordPush(ctx context.Context, id int64, state liveactivities.ContentState, pushedAt time.Time) error {
	if state.Arrivals == nil {
		state = liveactivities.EmptyContentState()
	}
	b, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("sqlite: marshal content state for live activity %d: %w", id, err)
	}
	err = r.q.RecordLiveActivityPush(ctx, gen.RecordLiveActivityPushParams{
		LastContentState: string(b), PushedAt: sql.NullInt64{Int64: pushedAt.Unix(), Valid: true}, ID: id,
	})
	if err != nil {
		return fmt.Errorf("sqlite: record push for live activity %d: %w", id, err)
	}
	return nil
}
