// Package regions holds the region domain model, the directory client that
// populates it, and the periodic refresh loop.
package regions

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// ErrNotFound is returned when no region has the requested id. The HTTP layer
// maps it to the 404 contract.
var ErrNotFound = errors.New("region not found")

// LatLon is a geographic point. It exists so a region's centroid is a pair or
// is absent -- a half-set centroid is not representable.
type LatLon struct {
	Lat float64
	Lon float64
}

// Region is one entry from the regions directory, plus locally-managed fields
// the directory does not supply.
type Region struct {
	ID             int64
	Name           string
	OBABaseURL     string
	SidecarBaseURL string
	Language       string
	Active         bool

	// Centroid is the region's area-weighted center, computed from the
	// directory's bounds rectangles. It is nil until a sync supplies usable
	// bounds: 0,0 is a real coordinate in the Gulf of Guinea, so "unset" must
	// not be spelled as a zero value. Weather is unavailable for a region
	// whose centroid is nil.
	Centroid *LatLon

	// DefaultAgencyID, Timezone, and OBAAPIKey are all locally managed: the
	// directory carries none of the three, and a refresh must never
	// overwrite any of them. OBAAPIKey is this region's key for its OBA REST
	// API server; empty means "inherit the process default", and it is never
	// echoed back by any surface.
	DefaultAgencyID string
	Timezone        string
	OBAAPIKey       string
}

// LogValue implements slog.LogValuer so a Region can be handed to a log call
// directly without every caller having to remember to pick fields by hand.
// OBAAPIKey is deliberately omitted: it is a live credential for this
// region's OBA REST API server, and a handler logging a whole Region (e.g.
// "region", region on an error path) must not be able to write it to disk.
// Omitting it here makes that leak unrepresentable rather than merely
// unwritten -- see internal/httpapi's log tests, which seed a real key on the
// region and assert it never appears in the captured output.
func (r Region) LogValue() slog.Value {
	return slog.GroupValue(
		slog.Int64("id", r.ID),
		slog.String("name", r.Name),
		slog.String("oba_base_url", r.OBABaseURL),
		slog.String("sidecar_base_url", r.SidecarBaseURL),
		slog.String("language", r.Language),
		slog.Bool("active", r.Active),
		slog.String("default_agency_id", r.DefaultAgencyID),
		slog.String("timezone", r.Timezone),
		slog.Bool("has_oba_api_key", r.OBAAPIKey != ""),
	)
}

// LocalFields carries the region columns the directory does not supply. It is
// a struct rather than three positional strings because three adjacent string
// parameters is the shape that silently swaps two of them.
type LocalFields struct {
	DefaultAgencyID string
	Timezone        string
	OBAAPIKey       string
}

// Repository stores regions. Implementations must be safe for concurrent use.
type Repository interface {
	Get(ctx context.Context, id int64) (Region, error)
	List(ctx context.Context) ([]Region, error)

	// UpsertFromDirectory writes directory-sourced columns only, leaving
	// DefaultAgencyID, Timezone, and OBAAPIKey untouched. It never deletes
	// rows: alerts cascade from regions, so removing a region that vanished
	// upstream would destroy every alert authored for it.
	UpsertFromDirectory(ctx context.Context, in []Region, now time.Time) error

	SetLocalFields(ctx context.Context, id int64, in LocalFields, now time.Time) error
}
