// Package liveactivities implements the OneBusAway sidecar spec §6 (iOS Live
// Activities): a stateful Lock Screen subscription the sidecar updates once
// a minute via ActivityKit pushes until it expires, runs dry, or its token
// dies. Design: docs/superpowers/specs/2026-08-24-live-activities-design.md.
package liveactivities

import (
	"context"
	"errors"
	"time"
)

var (
	// ErrNotFound reports that no live activity matches the given region and token.
	ErrNotFound = errors.New("live activity not found")
	// ErrDuplicate reports an Upsert that lost the concurrent first-registration
	// race on (region, activity_id); callers retry once (design spec §2.1).
	ErrDuplicate = errors.New("duplicate live activity registration")
)

// Lifecycle constants (design spec §5). Durations are absolute-instant
// arithmetic; no zone is ever consulted.
const (
	// Lifetime is the hard expiry set at registration (Apple's 8h HIG ceiling).
	Lifetime = 8 * time.Hour
	// KeepaliveInterval is deliberately under the 1-minute cadence: the
	// last-push timestamp is stamped after the push round-trip, so at exactly
	// 60s the next cycle misses by milliseconds and the widget updates every
	// other minute (spec §6.3).
	KeepaliveInterval = 55 * time.Second
	// StaleAfter is the update push's stale-date offset.
	StaleAfter = 10 * time.Minute
	// DismissAfterEnd is the end push's dismissal-date offset.
	DismissAfterEnd = 15 * time.Minute
	// StopCacheTTL bounds how long one stop's arrivals are shared across
	// subscriptions (spec §6.3 cost control).
	StopCacheTTL = 55 * time.Second
	// StopFetchBudget bounds one shared upstream fetch; above obaapi's 4s
	// per-request timeout with no retries.
	StopFetchBudget = 6 * time.Second

	// MaxConsecutiveFailures ends an activity after this many empty/error
	// cycles in a row (spec §6.3).
	MaxConsecutiveFailures int64 = 3
	// MaxArrivals caps the content state (spec §6.2).
	MaxArrivals = 3
	// LookbackMinutes and LookaheadMinutes are the OBA query window.
	LookbackMinutes  int64 = 5
	LookaheadMinutes int64 = 120
)

// LiveActivity is one subscription as stored. Identity is bookmark-scoped
// (stop + route + headsign); the trip fields are display metadata only.
type LiveActivity struct {
	ID          int64
	RegionID    int64
	Token       string // public address (spec §2.4)
	ActivityID  string // ActivityKit activity id; upsert key with RegionID
	PushToken   string // ActivityKit push token (not the device alert token)
	APNSSandbox bool

	StopID         string
	RouteShortName string
	TripHeadsign   string
	TripID         string // "" = omitted
	ServiceDate    int64  // epoch ms; 0 = omitted
	VehicleID      string // "" = omitted
	StopSequence   *int64 // nil = omitted; 0 is a real value

	LastContentState    ContentState
	LastPushedAt        *time.Time // nil = never pushed
	ConsecutiveFailures int64
	ExpiresAt           time.Time
	CreatedAt           time.Time
}

// NewLiveActivity is the input to Repository.Upsert. Token and ExpiresAt
// are used only when the upsert inserts (design spec §2.1).
type NewLiveActivity struct {
	RegionID    int64
	Token       string
	ExpiresAt   time.Time
	ActivityID  string
	PushToken   string
	APNSSandbox bool

	StopID         string
	RouteShortName string
	TripHeadsign   string
	TripID         string
	ServiceDate    int64
	VehicleID      string
	StopSequence   *int64
}

// Repository persists live activities. Implementations must be safe for
// concurrent use: the updater sweep runs List/RecordFailure/RecordPush/
// DeleteByID concurrently with the HTTP handlers' Upsert/Delete and the
// feedback webhook's DeleteByPushToken.
type Repository interface {
	// Upsert inserts on a new (region, activity_id) or rewrites the
	// registration fields of an existing one, preserving token, expiry, and
	// the updater's bookkeeping. ErrDuplicate on the first-registration race.
	Upsert(ctx context.Context, in NewLiveActivity, now time.Time) (LiveActivity, error)
	// Delete removes the activity matching regionID and token. ErrNotFound
	// when no row matches (204 contract for a missing/expired activity).
	Delete(ctx context.Context, regionID int64, token string) error
	// DeleteByID removes the activity by primary key; a missing row is success.
	DeleteByID(ctx context.Context, id int64) error
	// DeleteByPushToken removes every activity registered with pushToken,
	// returning the count removed (APNs feedback: token no longer valid).
	DeleteByPushToken(ctx context.Context, pushToken string) (int64, error)
	// List returns every live activity, for the updater sweep.
	List(ctx context.Context) ([]LiveActivity, error)
	// RecordFailure increments the activity's consecutive-failure streak and
	// returns the new streak value.
	RecordFailure(ctx context.Context, id int64) (int64, error)
	// ResetFailures zeroes the activity's consecutive-failure streak.
	ResetFailures(ctx context.Context, id int64) error
	// RecordPush stores the content state just pushed and the time it was
	// pushed, and resets the failure streak.
	RecordPush(ctx context.Context, id int64, state ContentState, pushedAt time.Time) error
}
