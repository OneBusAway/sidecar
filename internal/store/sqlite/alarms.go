package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/OneBusAway/sidecar/internal/alarms"
	"github.com/OneBusAway/sidecar/internal/store/sqlite/gen"
)

// Error strings from this repo deliberately never embed token and user_push_id values:
// tokens are device-addressable secrets (spec section 13), errors get
// logged verbatim by callers, and scrubbing at the source is what makes
// httpapi's sanitizeToken defense-in-depth rather than the only line.
type alarmRepo struct {
	q *gen.Queries
}

// int64ToNullInt64 converts alarms.NewAlarm.StopSequence (*int64, nil =
// omitted) to the NULLable column form.
func int64ToNullInt64(v *int64) sql.NullInt64 {
	if v == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: *v, Valid: true}
}

// nullInt64ToPtr is int64ToNullInt64's inverse: it must preserve 0 as a real
// value (the trip's first stop), not collapse it to nil.
func nullInt64ToPtr(v sql.NullInt64) *int64 {
	if !v.Valid {
		return nil
	}
	n := v.Int64
	return &n
}

func alarmFromRow(a gen.Alarm) alarms.Alarm {
	return alarms.Alarm{
		ID:              a.ID,
		RegionID:        a.RegionID,
		Token:           a.Token,
		APIVersion:      int(a.ApiVersion),
		UserPushID:      a.UserPushID,
		OperatingSystem: a.OperatingSystem,
		APNSSandbox:     a.ApnsSandbox,
		StopID:          a.StopID,
		TripID:          a.TripID,
		ServiceDate:     a.ServiceDate,
		VehicleID:       a.VehicleID,
		StopSequence:    nullInt64ToPtr(a.StopSequence),
		SecondsBefore:   a.SecondsBefore,
		Message:         a.Message,
		FailureCount:    a.FailureCount,
		CreatedAt:       unixToTime(a.CreatedAt),
	}
}

// Create maps the alarms_v1_dedupe_idx unique-constraint violation to
// alarms.ErrDuplicate: a V1 client that re-POSTs an identical registration
// loses the race to whichever concurrent insert won the row, and the caller
// re-fetches that winner via FindV1 rather than minting a second alarm that
// would fire twice. The in.APIVersion == 1 guard is belt-and-suspenders --
// the constraint message alone already distinguishes this from the (in
// practice unreachable) global token UNIQUE violation, which must stay a
// plain 500.
func (r *alarmRepo) Create(ctx context.Context, in alarms.NewAlarm, now time.Time) (alarms.Alarm, error) {
	ts := now.Unix()
	row, err := r.q.CreateAlarm(ctx, gen.CreateAlarmParams{
		RegionID:        in.RegionID,
		Token:           in.Token,
		ApiVersion:      int64(in.APIVersion),
		UserPushID:      in.UserPushID,
		OperatingSystem: in.OperatingSystem,
		ApnsSandbox:     in.APNSSandbox,
		StopID:          in.StopID,
		TripID:          in.TripID,
		ServiceDate:     in.ServiceDate,
		VehicleID:       in.VehicleID,
		StopSequence:    int64ToNullInt64(in.StopSequence),
		SecondsBefore:   in.SecondsBefore,
		Message:         in.Message,
		Now:             ts,
	})
	if err != nil {
		if in.APIVersion == 1 && strings.Contains(err.Error(), "UNIQUE constraint failed: alarms.region_id") {
			return alarms.Alarm{}, fmt.Errorf("sqlite: create alarm (region %d): %w", in.RegionID, alarms.ErrDuplicate)
		}
		return alarms.Alarm{}, fmt.Errorf("sqlite: create alarm (region %d): %w", in.RegionID, err)
	}
	return alarmFromRow(row), nil
}

func (r *alarmRepo) FindV1(ctx context.Context, key alarms.V1Key) (alarms.Alarm, error) {
	row, err := r.q.FindV1Alarm(ctx, gen.FindV1AlarmParams{
		RegionID:    key.RegionID,
		UserPushID:  key.UserPushID,
		TripID:      key.TripID,
		StopID:      key.StopID,
		ServiceDate: key.ServiceDate,
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return alarms.Alarm{}, fmt.Errorf("sqlite: find v1 alarm (region %d): %w", key.RegionID, alarms.ErrNotFound)
		}
		return alarms.Alarm{}, fmt.Errorf("sqlite: find v1 alarm (region %d): %w", key.RegionID, err)
	}
	return alarmFromRow(row), nil
}

// Delete reports alarms.ErrNotFound for an unknown (region, token) pair --
// including a token that exists but in a different region, which is the
// region-scoping guarantee: a rider's DELETE against region 2 must not
// remove a row registered in region 1.
func (r *alarmRepo) Delete(ctx context.Context, regionID int64, token string) error {
	n, err := r.q.DeleteAlarmByToken(ctx, gen.DeleteAlarmByTokenParams{RegionID: regionID, Token: token})
	if err != nil {
		return fmt.Errorf("sqlite: delete alarm (region %d): %w", regionID, err)
	}
	if n == 0 {
		return fmt.Errorf("sqlite: delete alarm (region %d): %w", regionID, alarms.ErrNotFound)
	}
	return nil
}

// DeleteByID treats zero affected rows as success, not alarms.ErrNotFound:
// the scheduler sweep may race a rider's own cancel, and either way the row
// being gone is the goal.
func (r *alarmRepo) DeleteByID(ctx context.Context, id int64) error {
	if _, err := r.q.DeleteAlarmByID(ctx, id); err != nil {
		return fmt.Errorf("sqlite: delete alarm %d: %w", id, err)
	}
	return nil
}

func (r *alarmRepo) List(ctx context.Context) ([]alarms.Alarm, error) {
	rows, err := r.q.ListAlarms(ctx)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list alarms: %w", err)
	}
	out := make([]alarms.Alarm, len(rows))
	for i, row := range rows {
		out[i] = alarmFromRow(row)
	}
	return out, nil
}

// RecordFailure returns the new consecutive-failure streak via RETURNING,
// rather than a separate increment-then-read pair that would race a
// concurrent RecordFailure/ResetFailures call. The query stamps updated_at
// with SQLite's own unixepoch() rather than a bound now: RecordFailure takes
// no now time.Time (see queries/alarms.sql for why).
func (r *alarmRepo) RecordFailure(ctx context.Context, id int64) (int64, error) {
	n, err := r.q.RecordAlarmFailure(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("sqlite: record failure for alarm %d: %w", id, alarms.ErrNotFound)
		}
		return 0, fmt.Errorf("sqlite: record failure for alarm %d: %w", id, err)
	}
	return n, nil
}

func (r *alarmRepo) ResetFailures(ctx context.Context, id int64) error {
	if err := r.q.ResetAlarmFailures(ctx, id); err != nil {
		return fmt.Errorf("sqlite: reset failures for alarm %d: %w", id, err)
	}
	return nil
}
