package httpapi

import (
	"errors"
	"net/http"
	"time"

	"github.com/OneBusAway/sidecar/internal/regions"
	"github.com/OneBusAway/sidecar/internal/weather"
)

// forecastJSON is the wire shape from openapi.yaml's WeatherForecast schema.
// The region fields are assembled per request rather than cached, so a
// renamed region is correct immediately.
type forecastJSON struct {
	Latitude         float64              `json:"latitude"`
	Longitude        float64              `json:"longitude"`
	RegionIdentifier int64                `json:"region_identifier"`
	RegionName       string               `json:"region_name"`
	RetrievedAt      string               `json:"retrieved_at"`
	Units            string               `json:"units"`
	TodaySummary     string               `json:"today_summary"`
	CurrentForecast  weather.Conditions   `json:"current_forecast"`
	HourlyForecast   []weather.Conditions `json:"hourly_forecast"`
}

// weatherHandler serves the regional forecast.
type weatherHandler struct {
	deps Deps
}

// forecast serves GET /api/v1/regions/{regionId}/weather.
//
// Every failure that is not an unknown region is a 403, per spec §9: shipped
// apps treat any non-200 as "hide the weather UI", and 403 is the code they
// have been tested against. A 404 would tell the app the region does not
// exist, which is a different and false claim.
func (h *weatherHandler) forecast(w http.ResponseWriter, r *http.Request) {
	id, parsed := ParseRegionSegment(r.PathValue("regionId"))
	if !parsed {
		writeRegionNotFound(w, h.deps.Logger)
		return
	}

	ctx := r.Context()
	region, err := h.deps.Regions.Get(ctx, id)
	if err != nil {
		if errors.Is(err, regions.ErrNotFound) {
			writeRegionNotFound(w, h.deps.Logger)
			return
		}
		h.deps.Logger.Error("httpapi: get region", "region_id", id, "err", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if region.Centroid == nil {
		h.deps.Logger.Error("httpapi: weather unavailable, region has no centroid", "region_id", id)
		h.writeUnavailable(w)
		return
	}

	snap, err := h.deps.Weather.Snapshot(ctx, *region.Centroid)
	if err != nil {
		h.deps.Logger.Error("httpapi: weather fetch", "region_id", id, "err", err)
		h.writeUnavailable(w)
		return
	}

	writeJSON(w, h.deps.Logger, http.StatusOK, forecastJSON{
		Latitude:         region.Centroid.Lat,
		Longitude:        region.Centroid.Lon,
		RegionIdentifier: region.ID,
		RegionName:       region.Name,
		RetrievedAt:      snap.RetrievedAt.UTC().Format(time.RFC3339),
		Units:            snap.Units,
		TodaySummary:     snap.TodaySummary,
		CurrentForecast:  snap.Current,
		HourlyForecast:   snap.Hourly,
	})
}

// writeUnavailable is the 403 contract. The body is ignored by clients; an
// empty object is valid JSON for one that decodes before checking status.
func (h *weatherHandler) writeUnavailable(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	if _, err := w.Write([]byte("{}")); err != nil {
		h.deps.Logger.Warn("httpapi: write 403 body", "err", err)
	}
}
