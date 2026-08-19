package httpapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
)

// vehiclesHandler serves the fuzzy vehicle-id search.
type vehiclesHandler struct {
	deps Deps
}

// search serves GET /api/v1/regions/{regionId}/vehicles.
func (h *vehiclesHandler) search(w http.ResponseWriter, r *http.Request) {
	region, ok := resolveRegion(w, r, h.deps)
	if !ok {
		return
	}
	ctx := r.Context()

	matches, err := h.deps.Vehicles.Search(ctx, region, r.URL.Query().Get("query"))
	if err != nil {
		// 502, not an empty 200: an empty list is indistinguishable from "no
		// such vehicle", so a rider searching for a bus that exists would be
		// told, confidently, that it does not.
		level := slogLevelForUpstreamErr(err)
		h.deps.Logger.Log(ctx, level, "httpapi: vehicle search", "region_id", region.ID, "err", err)
		w.WriteHeader(http.StatusBadGateway)
		return
	}

	writeJSON(w, h.deps.Logger, http.StatusOK, matches)
}

// slogLevelForUpstreamErr downgrades exactly one case to Warn: a client
// disconnecting mid-request. cache.Get runs the upstream fetch on a context
// detached from the caller (see internal/cache's doc comment) and instead
// selects on the caller's own ctx.Done(), so a disconnect surfaces here as
// context.Canceled from cache.Get itself -- it never reaches the upstream
// client at all. That is routine traffic on a search-as-you-type or
// polled endpoint, not an operational signal, and logging it at Error would
// drown out the failures worth paging on. Shared by every handler in this
// package whose upstream call runs through a cache.Cache (vehicles, weather).
//
// context.DeadlineExceeded is deliberately excluded from that demotion: since
// the fetch is detached from the caller, the only way this error occurs is
// the upstream client's own per-attempt timeout or a cache budget elapsing on
// the detached context -- i.e. the upstream is genuinely slow or down, which
// is exactly what an operator needs to see at Error.
func slogLevelForUpstreamErr(err error) slog.Level {
	if errors.Is(err, context.Canceled) {
		return slog.LevelWarn
	}
	return slog.LevelError
}
