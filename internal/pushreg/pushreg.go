// Package pushreg holds the push registration domain model: the registrations
// that route APNs alerts to mobile devices, the repository interface that
// persists them, and the pure NormalizeLocale function.
//
// Registrations store the raw reported locale as supplied by the client;
// NormalizeLocale is applied at fan-out time against whatever translation
// catalog then exists. The spec describes registration-time normalization, but
// the catalog in this implementation (languages present in alert_translations)
// is mutable. Normalizing at registration time against a snapshot would strand
// rows when translations are added — a device would never receive alerts in a
// language supported after it registered. Storing raw + normalizing at fan-out
// time is strictly more faithful to rider intent; the function itself matches
// the spec's algorithm exactly.
package pushreg

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound is returned when a registration cannot be found.
var ErrNotFound = errors.New("push registration not found")

// OS values are the only two the API admits.
const (
	OSIOS     = "ios"
	OSAndroid = "android"
)

// Registration is one registered mobile device, as stored.
type Registration struct {
	ID              int64 // row id; the alert push cursor pages on it (design spec §2.3)
	RegionID        int64
	Token           string
	OperatingSystem string // OSIOS | OSAndroid
	Locale          string // raw BCP-47 tag as reported; "" = none
	APNSSandbox     bool
	TestDevice      bool
	Description     string
	LastSeenAt      time.Time
	CreatedAt       time.Time
}

// AudienceCount is the size of one push audience split by platform
// (design spec §2.3); Total = IOS + Android.
type AudienceCount struct {
	Total   int64
	IOS     int64
	Android int64
}

// Upsert carries one registration write. Pointer fields implement the §4
// sticky semantics: nil = keep the stored value; non-nil = overwrite.
// OperatingSystem and APNSSandbox are deliberately non-sticky (always
// written): each registration states its own build's platform and APNs
// environment, and absent apns_sandbox means production (§2.7).
type Upsert struct {
	RegionID        int64
	Token           string
	OperatingSystem string
	APNSSandbox     bool
	Locale          *string
	TestDevice      *bool
	Description     *string
}

// Repository stores push registrations. Implementations must be safe for
// concurrent use.
type Repository interface {
	// Upsert inserts or refreshes the (region, token) row, always updating
	// last_seen_at. Must be atomic under concurrent first registration.
	Upsert(ctx context.Context, in Upsert, now time.Time) error
	Get(ctx context.Context, regionID int64, token string) (Registration, error)
	// Delete removes one region's registration; ErrNotFound if absent.
	Delete(ctx context.Context, regionID int64, token string) error
	// DeleteByToken removes the token everywhere (terminal APNs feedback is
	// not region-scoped); returns rows removed.
	DeleteByToken(ctx context.Context, token string) (int64, error)
	// Prune deletes rows whose last_seen_at is before cutoff; returns count.
	Prune(ctx context.Context, cutoff time.Time) (int64, error)
	// ListAudience returns up to limit registrations in regionID with
	// id > afterID, ascending by id; testOnly restricts to test devices.
	// Paging on the monotonic id (not last_seen_at) is what makes the alert
	// push cursor stable across a long send (design spec §2.3).
	ListAudience(ctx context.Context, regionID int64, testOnly bool, afterID int64, limit int) ([]Registration, error)
	// CountAudience is the size of the same set, split by platform.
	CountAudience(ctx context.Context, regionID int64, testOnly bool) (AudienceCount, error)
}
