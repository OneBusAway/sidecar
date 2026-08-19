package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/OneBusAway/sidecar/internal/regions"
)

// vehiclesHandler serves the fuzzy vehicle-id search.
type vehiclesHandler struct {
	deps Deps
}

// search serves GET /api/v1/regions/{regionId}/vehicles.
func (h *vehiclesHandler) search(w http.ResponseWriter, r *http.Request) {
	id, parsed := ParseRegionSegment(r.PathValue("regionId"))
	if !parsed {
		h.writeNotFound(w)
		return
	}

	ctx := r.Context()
	region, err := h.deps.Regions.Get(ctx, id)
	if err != nil {
		if errors.Is(err, regions.ErrNotFound) {
			h.writeNotFound(w)
			return
		}
		h.deps.Logger.Error("httpapi: get region", "region_id", id, "err", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	matches, err := h.deps.Vehicles.Search(ctx, region, r.URL.Query().Get("query"))
	if err != nil {
		// 502, not an empty 200: an empty list is indistinguishable from "no
		// such vehicle", so a rider searching for a bus that exists would be
		// told, confidently, that it does not.
		//
		// A client disconnecting mid-search cancels ctx, and obaapi.Fleet
		// surfaces that as context.Canceled/DeadlineExceeded. That is
		// routine traffic, not an operational problem, so it is logged at
		// Warn rather than Error to keep the error log meaningful for
		// things an operator should act on.
		level := slogLevelForSearchErr(err)
		h.deps.Logger.Log(ctx, level, "httpapi: vehicle search", "region_id", id, "err", err)
		w.WriteHeader(http.StatusBadGateway)
		return
	}

	writeJSON(w, h.deps.Logger, http.StatusOK, matches)
}

// slogLevelForSearchErr downgrades a cancelled/timed-out request to Warn: a
// rider closing the app mid-search is routine traffic, not an operational
// signal, and logging it at Error would drown out failures worth paging on.
func slogLevelForSearchErr(err error) slog.Level {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return slog.LevelWarn
	}
	return slog.LevelError
}

// writeNotFound writes the exact 404 contract for an unrecognised region.
func (h *vehiclesHandler) writeNotFound(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	if _, err := w.Write([]byte(notFoundBody)); err != nil {
		h.deps.Logger.Warn("httpapi: write 404 body", "err", err)
	}
}
