package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/OneBusAway/sidecar/internal/ghostbus"
	"github.com/OneBusAway/sidecar/internal/store/sqlite/gen"
)

// Error strings from this repo never embed public_identifier or
// user_identifier: the former is a dedupe token (spec section 13) and the
// latter is rider data, and callers log errors verbatim. Ids, region ids,
// and counts only.
//
// Duplicate discrimination is by constraint message text because modernc
// reports every unique violation as the same SQLITE_CONSTRAINT_UNIQUE code;
// the message names the violated index's columns (same pattern as
// alarms.go's V1 dedupe).
type ghostBusRepo struct{ q *gen.Queries }

// boolToNullInt64 converts ghostbus.NewReport.Predicted (*bool, nil =
// client didn't say) to the column form: the generated Predicted field is
// sql.NullInt64, not sql.NullBool, because the schema stores it as
// INTEGER (0/1) rather than a dedicated boolean type.
func boolToNullInt64(v *bool) sql.NullInt64 {
	if v == nil {
		return sql.NullInt64{}
	}
	if *v {
		return sql.NullInt64{Int64: 1, Valid: true}
	}
	return sql.NullInt64{Int64: 0, Valid: true}
}

// nullInt64ToBoolPtr is boolToNullInt64's inverse.
func nullInt64ToBoolPtr(v sql.NullInt64) *bool {
	if !v.Valid {
		return nil
	}
	b := v.Int64 != 0
	return &b
}

// ghostBusFromRow maps a stored row to the domain type. CreatedAt/
// SnapshotCapturedAt are epoch seconds; ServiceDate and the *_arrival_at /
// prediction_last_updated_at columns are epoch milliseconds, stored and
// returned unconverted (see migration 00007's comment).
func ghostBusFromRow(r gen.GhostBusReport) ghostbus.Report {
	var capturedAt *time.Time
	if r.SnapshotCapturedAt.Valid {
		t := unixToTime(r.SnapshotCapturedAt.Int64)
		capturedAt = &t
	}
	return ghostbus.Report{
		ID:                       r.ID,
		RegionID:                 r.RegionID,
		PublicID:                 r.PublicIdentifier,
		UserIdentifier:           r.UserIdentifier,
		TripIdentifier:           r.TripIdentifier,
		ServiceDate:              r.ServiceDate,
		RouteIdentifier:          r.RouteIdentifier,
		StopIdentifier:           r.StopIdentifier,
		VehicleIdentifier:        r.VehicleIdentifier,
		StopSequence:             nullInt64ToPtr(r.StopSequence),
		Predicted:                nullInt64ToBoolPtr(r.Predicted),
		ScheduleDeviationMinutes: nullInt64ToPtr(r.ScheduleDeviationMinutes),
		WaitDurationMinutes:      r.WaitDurationMinutes,
		Comment:                  r.Comment,
		UserLatitude:             nullToFloat(r.UserLatitude),
		UserLongitude:            nullToFloat(r.UserLongitude),
		ScheduledArrivalAt:       nullInt64ToPtr(r.ScheduledArrivalAt),
		PredictedArrivalAt:       nullInt64ToPtr(r.PredictedArrivalAt),
		PredictionLastUpdatedAt:  nullInt64ToPtr(r.PredictionLastUpdatedAt),
		SnapshotStatus:           r.SnapshotStatus,
		SnapshotJSON:             r.SnapshotJson,
		SnapshotCapturedAt:       capturedAt,
		SnapshotAttempts:         r.SnapshotAttempts,
		CreatedAt:                unixToTime(r.CreatedAt),
	}
}

// Create maps the two unique-constraint violations ghost_bus_reports can
// raise to distinct sentinels: a hit on the dedupe index
// (region_id, user_identifier, trip_identifier, service_date) means this
// exact trip instance was already reported (ErrDuplicate, -> already_reported
// 422); a hit on the public_identifier index means the client-minted token
// collided with an existing row for a *different* dedupe key (ErrTokenCollision
// -> re-mint and retry once). The dedupe index leads with region_id, which is
// what makes the two violation messages textually distinguishable.
func (r *ghostBusRepo) Create(ctx context.Context, in ghostbus.NewReport, now time.Time) (ghostbus.Report, error) {
	row, err := r.q.CreateGhostBusReport(ctx, gen.CreateGhostBusReportParams{
		RegionID:                 in.RegionID,
		PublicIdentifier:         in.PublicID,
		UserIdentifier:           in.UserIdentifier,
		TripIdentifier:           in.TripIdentifier,
		ServiceDate:              in.ServiceDate,
		RouteIdentifier:          in.RouteIdentifier,
		StopIdentifier:           in.StopIdentifier,
		VehicleIdentifier:        in.VehicleIdentifier,
		StopSequence:             int64ToNullInt64(in.StopSequence),
		Predicted:                boolToNullInt64(in.Predicted),
		ScheduleDeviationMinutes: int64ToNullInt64(in.ScheduleDeviationMinutes),
		WaitDurationMinutes:      in.WaitDurationMinutes,
		Comment:                  in.Comment,
		UserLatitude:             floatToNull(in.UserLatitude),
		UserLongitude:            floatToNull(in.UserLongitude),
		ScheduledArrivalAt:       int64ToNullInt64(in.ScheduledArrivalAt),
		PredictedArrivalAt:       int64ToNullInt64(in.PredictedArrivalAt),
		PredictionLastUpdatedAt:  int64ToNullInt64(in.PredictionLastUpdatedAt),
		Now:                      now.Unix(),
	})
	if err != nil {
		msg := err.Error()
		switch {
		case strings.Contains(msg, "ghost_bus_reports.region_id"):
			return ghostbus.Report{}, fmt.Errorf("sqlite: create ghost bus report (region %d): %w", in.RegionID, ghostbus.ErrDuplicate)
		case strings.Contains(msg, "ghost_bus_reports.public_identifier"):
			return ghostbus.Report{}, fmt.Errorf("sqlite: create ghost bus report (region %d): %w", in.RegionID, ghostbus.ErrTokenCollision)
		}
		return ghostbus.Report{}, fmt.Errorf("sqlite: create ghost bus report (region %d): %w", in.RegionID, err)
	}
	return ghostBusFromRow(row), nil
}

// ListPendingSnapshots returns reports awaiting a snapshot capture attempt,
// oldest first, capped at limit -- the snapshot worker's poll query.
func (r *ghostBusRepo) ListPendingSnapshots(ctx context.Context, limit int64) ([]ghostbus.Report, error) {
	rows, err := r.q.ListPendingSnapshotReports(ctx, limit)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list pending ghost bus snapshots: %w", err)
	}
	out := make([]ghostbus.Report, len(rows))
	for i, row := range rows {
		out[i] = ghostBusFromRow(row)
	}
	return out, nil
}

// MarkSnapshotCaptured records a successful snapshot capture. The generated
// Now field is sql.NullInt64 rather than plain int64 because the query
// binds one @now param into both the nullable snapshot_captured_at column
// and the non-null updated_at column, and sqlc widens the merged param to
// the nullable type -- Valid must always be true here, or the write would
// try to store NULL into the NOT NULL updated_at column.
func (r *ghostBusRepo) MarkSnapshotCaptured(ctx context.Context, id int64, snapshotJSON string, now time.Time) error {
	if err := r.q.MarkGhostBusSnapshotCaptured(ctx, gen.MarkGhostBusSnapshotCapturedParams{
		SnapshotJson: snapshotJSON,
		Now:          sql.NullInt64{Int64: now.Unix(), Valid: true},
		ID:           id,
	}); err != nil {
		return fmt.Errorf("sqlite: mark ghost bus report %d captured: %w", id, err)
	}
	return nil
}

// MarkSnapshotUnavailable records that no snapshot could be captured.
func (r *ghostBusRepo) MarkSnapshotUnavailable(ctx context.Context, id int64, now time.Time) error {
	if err := r.q.MarkGhostBusSnapshotUnavailable(ctx, gen.MarkGhostBusSnapshotUnavailableParams{
		Now: now.Unix(),
		ID:  id,
	}); err != nil {
		return fmt.Errorf("sqlite: mark ghost bus report %d unavailable: %w", id, err)
	}
	return nil
}

// RecordSnapshotFailure increments the attempt counter and, in the same
// UPDATE, flips snapshot_status to 'unavailable' once the increment reaches
// ghostbus.MaxSnapshotAttempts -- see the query comment in
// queries/ghostbus.sql for why the cap check and the status flip must not
// be split into two statements.
func (r *ghostBusRepo) RecordSnapshotFailure(ctx context.Context, id int64, now time.Time) (int64, error) {
	n, err := r.q.RecordGhostBusSnapshotFailure(ctx, gen.RecordGhostBusSnapshotFailureParams{
		Now: now.Unix(),
		ID:  id,
	})
	if err != nil {
		return 0, fmt.Errorf("sqlite: record ghost bus snapshot failure for report %d: %w", id, err)
	}
	return n, nil
}

// ListForExport returns every report in regionID created at or after
// sinceUnix (0 = all), for the agency CSV/JSON export.
func (r *ghostBusRepo) ListForExport(ctx context.Context, regionID int64, sinceUnix int64) ([]ghostbus.Report, error) {
	rows, err := r.q.ListGhostBusReportsForExport(ctx, gen.ListGhostBusReportsForExportParams{
		RegionID: regionID,
		Since:    sinceUnix,
	})
	if err != nil {
		return nil, fmt.Errorf("sqlite: list ghost bus reports for export (region %d): %w", regionID, err)
	}
	out := make([]ghostbus.Report, len(rows))
	for i, row := range rows {
		out[i] = ghostBusFromRow(row)
	}
	return out, nil
}
