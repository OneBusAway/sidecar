// Package weather proxies a weather provider into the normalized shape the
// apps render on the stop screen. The response vocabulary is Dark Sky's,
// which the apps were built against; provider values pass through rather than
// being remapped.
package weather

import (
	"context"
	"time"

	"github.com/OneBusAway/sidecar/internal/regions"
)

// Conditions is one forecast point, current or hourly.
type Conditions struct {
	Icon                 string  `json:"icon"`
	Summary              string  `json:"summary"`
	Temperature          float64 `json:"temperature"`
	TemperatureFeelsLike float64 `json:"temperature_feels_like"`
	PrecipPerHour        float64 `json:"precip_per_hour"`
	PrecipProbability    float64 `json:"precip_probability"`
	WindSpeed            float64 `json:"wind_speed"`
	Time                 int64   `json:"time"`
}

// Snapshot is everything a provider supplies for one coordinate. It carries
// no region identity: the region-specific envelope is assembled per request,
// so a renamed region is correct immediately and two regions sharing a
// centroid share one upstream call.
type Snapshot struct {
	Units        string
	TodaySummary string
	Current      Conditions
	Hourly       []Conditions

	// RetrievedAt is stamped when the provider call returned, and travels
	// with the cached value rather than being recomputed on a cache hit --
	// its whole purpose is to let a client see the data is 29 minutes old.
	RetrievedAt time.Time
}

// Provider fetches current conditions for a coordinate.
type Provider interface {
	Fetch(ctx context.Context, at regions.LatLon) (Snapshot, error)
}
