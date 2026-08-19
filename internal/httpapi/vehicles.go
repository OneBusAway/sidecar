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

	matches, err := h.deps.Vehicles.Search(ctx, region, r.URL.Query().Get("query"))
	if err != nil {
		// 502, not an empty 200: an empty list is indistinguishable from "no
		// such vehicle", so a rider searching for a bus that exists would be
		// told, confidently, that it does not.
		level := slogLevelForSearchErr(err)
		h.deps.Logger.Log(ctx, level, "httpapi: vehicle search", "region_id", id, "err", err)
		w.WriteHeader(http.StatusBadGateway)
		return
	}

	writeJSON(w, h.deps.Logger, http.StatusOK, matches)
}

// slogLevelForSearchErr downgrades exactly one case to Warn: a client
// disconnecting mid-search. cache.Get runs the upstream fetch on a context
// detached from the caller (see internal/cache's doc comment) and instead
// selects on the caller's own ctx.Done(), so a disconnect surfaces here as
// context.Canceled from cache.Get itself -- it never reaches obaapi.Fleet at
// all. That is routine traffic on a search-as-you-type endpoint, not an
// operational signal, and logging it at Error would drown out the failures
// worth paging on.
//
// context.DeadlineExceeded is deliberately excluded from that demotion: since
// the fetch is detached from the caller, the only way this error occurs is
// obaapi.Fleet's own per-attempt timeout or the fleet/query cache budgets
// elapsing on the detached context -- i.e. the upstream is genuinely slow or
// down, which is exactly what an operator needs to see at Error.
func slogLevelForSearchErr(err error) slog.Level {
	if errors.Is(err, context.Canceled) {
		return slog.LevelWarn
	}
	return slog.LevelError
}
