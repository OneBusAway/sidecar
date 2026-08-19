// Package weather proxies a weather provider into the normalized shape the
// apps render on the stop screen. The response vocabulary is Dark Sky's,
// which the apps were built against; provider values pass through rather than
// being remapped.
package weather

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/OneBusAway/sidecar/internal/cache"
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

// Service caches provider snapshots by coordinate. Two regions sharing a
// centroid therefore share one upstream call, and a renamed region needs no
// cache invalidation because the cached value holds no region identity.
type Service struct {
	provider Provider
	cache    *cache.Cache[Snapshot]
}

// NewService wires a Service. A nil provider means no key was configured, and
// Snapshot then reports ErrNoProvider without any network call.
//
// logger is accepted, not stored: unlike internal/vehicles' Service (which
// logs a truncation warning of its own), every condition Snapshot can return
// here is fully described by its returned error, so every caller so far logs
// it themselves rather than needing Service to. The parameter stays so the
// constructor's shape matches vehicles.NewService and can start logging
// without a signature change if that stops being true.
func NewService(provider Provider, c *cache.Cache[Snapshot]) *Service {
	return &Service{provider: provider, cache: c}
}

// ErrNoProvider means the process has no weather provider key configured.
var ErrNoProvider = errors.New("weather: no provider configured")

// Snapshot returns cached conditions for a coordinate, fetching on a miss.
func (s *Service) Snapshot(ctx context.Context, at regions.LatLon) (Snapshot, error) {
	if s.provider == nil {
		return Snapshot{}, ErrNoProvider
	}
	// Four decimals is roughly 11 metres -- far finer than any weather
	// gradient, and enough that two regions with the same centroid share a
	// cache entry.
	key := strconv.FormatFloat(at.Lat, 'f', 4, 64) + "," + strconv.FormatFloat(at.Lon, 'f', 4, 64)
	return s.cache.Get(ctx, key, func(ctx context.Context) (Snapshot, error) {
		return s.provider.Fetch(ctx, at)
	})
}
