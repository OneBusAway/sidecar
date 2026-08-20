// Package alarms implements the OneBusAway sidecar spec §5 (departure alarms).
//
// An alarm is one-shot server-owned timing that fires a single push
// notification when a departure is imminent. The region_id in PushData is
// the PUBLIC region identifier, deliberately not an internal row id (§5.4
// reference quirk).
package alarms

import (
	"context"
	"errors"
	"fmt"
	"time"
)

var (
	ErrNotFound = errors.New("alarm not found")
	// ErrDuplicate reports a V1 create that lost the dedupe race to a
	// concurrent identical registration; callers re-fetch the winner.
	ErrDuplicate = errors.New("duplicate v1 alarm")
)

const DefaultSecondsBefore = 600 // spec §5.2

type Alarm struct {
	ID              int64
	RegionID        int64
	Token           string
	APIVersion      int // 1 | 2
	UserPushID      string
	OperatingSystem string // "ios" | "android"
	APNSSandbox     bool
	// Trip-identity fields are client obligations, not server-enforced
	// (spec §5.2): zero values mean the client omitted them and the push
	// payload carries null there.
	StopID        string
	TripID        string
	ServiceDate   int64  // epoch ms; 0 = omitted
	VehicleID     string // "" = omitted
	StopSequence  *int64 // nil = omitted; 0 is a real value (first stop)
	SecondsBefore int64
	Message       string // composed at creation; what eventually gets pushed
	FailureCount  int64  // consecutive failed OBA lookups (spec §5.3)
	CreatedAt     time.Time
}

type NewAlarm struct {
	RegionID        int64
	Token           string
	APIVersion      int
	UserPushID      string
	OperatingSystem string
	APNSSandbox     bool
	StopID, TripID  string
	ServiceDate     int64
	VehicleID       string
	StopSequence    *int64
	SecondsBefore   int64
	Message         string
}

// V1Key is the §5.1 idempotency key for legacy clients.
type V1Key struct {
	RegionID    int64
	UserPushID  string
	TripID      string
	StopID      string
	ServiceDate int64
}

type Repository interface {
	Create(ctx context.Context, in NewAlarm, now time.Time) (Alarm, error) // ErrDuplicate on V1 race
	FindV1(ctx context.Context, key V1Key) (Alarm, error)                  // ErrNotFound
	Delete(ctx context.Context, regionID int64, token string) error        // ErrNotFound; 204 contract
	DeleteByID(ctx context.Context, id int64) error
	List(ctx context.Context) ([]Alarm, error)                  // scheduler sweep, all regions
	RecordFailure(ctx context.Context, id int64) (int64, error) // ++failure_count, returns streak
	ResetFailures(ctx context.Context, id int64) error
}

// NormalizeSecondsBefore applies the §5.2 rule: absent, non-numeric, or
// <= 0 becomes the 600-second default.
func NormalizeSecondsBefore(v int64, ok bool) int64 {
	if !ok || v <= 0 {
		return DefaultSecondsBefore
	}
	return v
}

// minutesPhrase renders the lead time the way the apps' copy expects,
// clamping to at least one minute so a short lead never reads "0 minutes".
func minutesPhrase(secondsBefore int64) string {
	m := secondsBefore / 60
	if m < 1 {
		m = 1
	}
	if m == 1 {
		return "1 minute"
	}
	return fmt.Sprintf("%d minutes", m)
}

// ComposeMessage is the creation-time human message: "The 44 to Ballard
// leaves in 10 minutes". Minutes derive from secondsBefore because that is
// the lead time the push actually fires at.
func ComposeMessage(routeShortName, headsign string, secondsBefore int64) string {
	return fmt.Sprintf("The %s to %s leaves in %s", routeShortName, headsign, minutesPhrase(secondsBefore))
}

// GenericMessage is the §5.2 fallback: "The bus leaves in 10 minutes".
func GenericMessage(secondsBefore int64) string {
	return fmt.Sprintf("The bus leaves in %s", minutesPhrase(secondsBefore))
}

// PushData is the §5.4 wire contract the apps deep-link from. Exact key
// set and nesting; omitted trip fields are JSON null, never "".
func (a Alarm) PushData() map[string]any {
	nullableStr := func(s string) any {
		if s == "" {
			return nil
		}
		return s
	}
	ad := map[string]any{
		"region_id":  a.RegionID,
		"stop_id":    nullableStr(a.StopID),
		"trip_id":    nullableStr(a.TripID),
		"vehicle_id": nullableStr(a.VehicleID),
	}
	if a.ServiceDate != 0 {
		ad["service_date"] = a.ServiceDate
	} else {
		ad["service_date"] = nil
	}
	if a.StopSequence != nil {
		ad["stop_sequence"] = *a.StopSequence
	} else {
		ad["stop_sequence"] = nil
	}
	return map[string]any{"arrival_and_departure": ad}
}

type Decision int

const (
	Wait Decision = iota
	Fire
	Expire // departure already passed: delete without pushing (spec §5.3)
)

// Decide implements the §5.3 firing rules given seconds until departure.
func Decide(secondsUntilDeparture, secondsBefore int64) Decision {
	switch {
	case secondsUntilDeparture > secondsBefore:
		return Wait
	case secondsUntilDeparture < 0:
		return Expire
	default:
		return Fire
	}
}
