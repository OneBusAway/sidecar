// Package vehicles implements the fuzzy vehicle-id search the "find my bus"
// UI calls. The matching rule is a deliberate port of the reference
// implementation's, quirks included; see Filter.
package vehicles

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/OneBusAway/sidecar/internal/cache"
	"github.com/OneBusAway/sidecar/internal/obaapi"
	"github.com/OneBusAway/sidecar/internal/regions"
)

const (
	// MinQueryRunes is spec §10: shorter queries return an empty list without
	// touching the upstream, so a search box firing per keystroke cannot
	// trigger a full-fleet scan on the first character.
	MinQueryRunes = 3

	// MaxQueryRunes bounds the cache key. The query cache is keyed by
	// attacker-controlled input on an unauthenticated endpoint, so capping
	// the entry count alone would still permit 4096 megabyte-long keys. No
	// real vehicle id approaches 64 characters.
	MaxQueryRunes = 64

	// MaxResults caps the response. A three-character query against a large
	// numeric fleet can match thousands of vehicles; this is a deliberate
	// divergence from the reference, which returns everything.
	MaxResults = 250
)

// Match is one search result, shaped as the apps expect it.
type Match struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	VehicleID string `json:"vehicle_id"`
}

// Normalize applies the query rules: trim, lowercase, and bounds-check by
// rune count. It reports false when the query must yield an empty result
// without any upstream call.
//
// Rune count, not byte length: the reference is Rails, whose String#length
// counts characters, and two CJK characters are six bytes.
func Normalize(raw string) (string, bool) {
	q := strings.ToLower(strings.TrimSpace(raw))
	n := utf8.RuneCountInString(q)
	if n < MinQueryRunes || n > MaxQueryRunes {
		return "", false
	}
	return q, true
}

// filter selects the fleet entries whose id contains q, preserving fleet
// order and truncating at MaxResults. It also reports whether the result was
// actually truncated -- i.e. at least one further match existed beyond
// MaxResults -- so Search can warn only when truncation really happened. A
// fleet with exactly MaxResults matches and no more is not truncated, and
// warning about it on every such search would be a permanent false alarm.
//
// Only the query has been lowered; the vehicle ids are matched raw. This is
// required by spec §10 and is not an oversight: implementing true
// case-insensitivity would make this server disagree with every shipped
// client on any fleet with uppercase ids.
//
// Unexported: nothing outside this package's own tests calls it. Search is
// the only real caller, and it needs the truncation flag filter alone
// carries.
func filter(fleet []obaapi.Vehicle, q string) ([]Match, bool) {
	out := make([]Match, 0, 16)
	for _, v := range fleet {
		if !strings.Contains(v.VehicleID, q) {
			continue
		}
		if len(out) == MaxResults {
			return out, true
		}
		out = append(out, Match{ID: v.AgencyID, Name: v.AgencyName, VehicleID: v.VehicleID})
	}
	return out, false
}

// Service answers searches, caching both the region's fleet and each query's
// results. The two caches serve different purposes: the fleet cache stops N
// searches in a region from costing N full-fleet fetches, and the query cache
// stops a search box firing per keystroke from re-filtering the fleet.
type Service struct {
	oba    obaapi.Client
	fleet  *cache.Cache[[]obaapi.Vehicle]
	result *cache.Cache[[]Match]
	logger *slog.Logger
}

// NewService wires a Service. The caches are constructed by the caller so
// their TTLs, caps, and clock stay configuration rather than constants buried
// here.
func NewService(oba obaapi.Client, fleet *cache.Cache[[]obaapi.Vehicle], result *cache.Cache[[]Match], logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{oba: oba, fleet: fleet, result: result, logger: logger}
}

// Search returns the matches for rawQuery in region. A query that fails
// Normalize returns an empty slice and no error, without any upstream call.
func (s *Service) Search(ctx context.Context, region regions.Region, rawQuery string) ([]Match, error) {
	q, ok := Normalize(rawQuery)
	if !ok {
		return []Match{}, nil
	}

	key := strconv.FormatInt(region.ID, 10) + "|" + q
	return s.result.Get(ctx, key, func(ctx context.Context) ([]Match, error) {
		fleet, err := s.fleet.Get(ctx, strconv.FormatInt(region.ID, 10),
			func(ctx context.Context) ([]obaapi.Vehicle, error) {
				return s.oba.Fleet(ctx, region)
			})
		if err != nil {
			return nil, fmt.Errorf("vehicles: fleet for region %d: %w", region.ID, err)
		}
		matches, truncated := filter(fleet, q)
		if truncated {
			s.logger.Warn("vehicles: results truncated",
				"region_id", region.ID, "cap", MaxResults)
		}
		return matches, nil
	})
}
