// Package regions holds the region domain model, the directory client that
// populates it, and the periodic refresh loop.
package regions

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound is returned when no region has the requested id. The HTTP layer
// maps it to the 404 contract.
var ErrNotFound = errors.New("region not found")

// Region is one entry from the regions directory, plus locally-managed fields
// the directory does not supply.
type Region struct {
	ID             int64
	Name           string
	OBABaseURL     string
	SidecarBaseURL string
	Language       string
	Active         bool

	// Locally managed. The directory carries neither, and the refresh must
	// never overwrite them.
	DefaultAgencyID string
	Timezone        string
}

// Repository stores regions. Implementations must be safe for concurrent use.
type Repository interface {
	Get(ctx context.Context, id int64) (Region, error)
	List(ctx context.Context) ([]Region, error)

	// UpsertFromDirectory writes directory-sourced columns only, leaving
	// DefaultAgencyID and Timezone untouched. It never deletes rows: alerts
	// cascade from regions, so removing a region that vanished upstream would
	// destroy every alert authored for it.
	UpsertFromDirectory(ctx context.Context, in []Region, now time.Time) error

	SetLocalFields(ctx context.Context, id int64, agencyID, timezone string, now time.Time) error
}
