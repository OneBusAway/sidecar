// Package ghostbus implements the OneBusAway sidecar spec §8 (ghost bus
// reports): rider-filed "the app promised a bus that never came" records,
// deduped per trip instance and enriched asynchronously with a trip-details
// snapshot from the region's OBA server.
//
// WaitDurationChoices and CommentMaxLen are mirrored by hand in the iOS app
// (OBAKit/Trip/TripPage/GhostBusReportView.swift: waitChoices /
// commentMaxLength); a change here needs a matching client change.
package ghostbus

import (
	"context"
	"errors"
	"slices"
	"time"
)

const (
	CommentMaxLen       = 1000 // runes; mirrored in GhostBusReportView.swift
	MaxSnapshotAttempts = 3    // total tries, matching OBACloud retry_on attempts: 3

	SnapshotPending     = "pending"
	SnapshotCaptured    = "captured"
	SnapshotUnavailable = "unavailable"
)

var WaitDurationChoices = []int64{5, 10, 15, 20, 30}

var (
	ErrDuplicate      = errors.New("duplicate ghost bus report")  // dedupe-index hit → already_reported 422
	ErrTokenCollision = errors.New("public identifier collision") // re-mint and retry once, never already_reported
	ErrNotFound       = errors.New("ghost bus report not found")
)

type Report struct {
	ID                       int64
	RegionID                 int64
	PublicID                 string
	UserIdentifier           string
	TripIdentifier           string
	ServiceDate              int64 // epoch ms, as received (dedupe key component)
	RouteIdentifier          string
	StopIdentifier           string
	VehicleIdentifier        string
	StopSequence             *int64
	Predicted                *bool // three-state: nil = client didn't say
	ScheduleDeviationMinutes *int64
	WaitDurationMinutes      int64
	Comment                  string
	UserLatitude             *float64
	UserLongitude            *float64
	ScheduledArrivalAt       *int64 // epoch ms
	PredictedArrivalAt       *int64 // epoch ms
	PredictionLastUpdatedAt  *int64 // epoch ms
	SnapshotStatus           string
	SnapshotJSON             string // "" until captured
	SnapshotCapturedAt       *time.Time
	SnapshotAttempts         int64
	CreatedAt                time.Time
}

// NewReport is the input to Repository.Create: a Report before the store
// assigns ID, snapshot bookkeeping, and CreatedAt.
type NewReport struct {
	RegionID                 int64
	PublicID                 string
	UserIdentifier           string
	TripIdentifier           string
	ServiceDate              int64
	RouteIdentifier          string
	StopIdentifier           string
	VehicleIdentifier        string
	StopSequence             *int64
	Predicted                *bool
	ScheduleDeviationMinutes *int64
	WaitDurationMinutes      int64
	Comment                  string
	UserLatitude             *float64
	UserLongitude            *float64
	ScheduledArrivalAt       *int64
	PredictedArrivalAt       *int64
	PredictionLastUpdatedAt  *int64
}

type Repository interface {
	Create(ctx context.Context, in NewReport, now time.Time) (Report, error) // ErrDuplicate | ErrTokenCollision
	ListPendingSnapshots(ctx context.Context, limit int64) ([]Report, error) // pending AND attempts < MaxSnapshotAttempts, oldest first
	MarkSnapshotCaptured(ctx context.Context, id int64, snapshotJSON string, now time.Time) error
	MarkSnapshotUnavailable(ctx context.Context, id int64, now time.Time) error
	// RecordSnapshotFailure increments snapshot_attempts and, when the
	// increment reaches MaxSnapshotAttempts, flips snapshot_status to
	// 'unavailable' in the same UPDATE. Returns the post-increment count.
	RecordSnapshotFailure(ctx context.Context, id int64, now time.Time) (int64, error)
	ListForExport(ctx context.Context, regionID int64, sinceUnix int64) ([]Report, error) // created_at >= sinceUnix; 0 = all
}

// ValidWaitDuration reports whether v is one of the §8 choices. slices is
// fine here; the list is five entries.
func ValidWaitDuration(v int64) bool {
	return slices.Contains(WaitDurationChoices, v)
}
