package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/MobilityData/gtfs-realtime-bindings/golang/gtfs"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/OneBusAway/sidecar/internal/alerts"
	"github.com/OneBusAway/sidecar/internal/regions"
)

// notFoundBody is the exact 404 body the design spec (§1.2, §2.5) requires
// for an unrecognised region.
const notFoundBody = `{"error":"Couldn't find Region"}`

// alertsHandler serves the GTFS-realtime service alerts feed.
type alertsHandler struct {
	deps Deps
}

// feedBinary serves GET /api/v1/regions/{regionId}/alerts: the feed as a
// binary protobuf.
func (h *alertsHandler) feedBinary(w http.ResponseWriter, r *http.Request) {
	msg, id, ok := h.buildFeed(w, r)
	if !ok {
		return
	}

	body, err := proto.Marshal(msg)
	if err != nil {
		h.serverError(w, id, "marshal protobuf", err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	h.writeBody(w, id, body)
}

// feedText serves GET /api/v1/regions/{regionId}/alerts.pbtext: the same
// feed rendered as protobuf JSON, for debugging.
func (h *alertsHandler) feedText(w http.ResponseWriter, r *http.Request) {
	msg, id, ok := h.buildFeed(w, r)
	if !ok {
		return
	}

	body, err := protojson.Marshal(msg)
	if err != nil {
		h.serverError(w, id, "marshal pbtext", err)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	h.writeBody(w, id, body)
}

// buildFeed resolves the region segment, looks up the region, fetches the
// feed rows, and renders them. It writes the response itself on failure (404
// or 500) and returns ok=false, so callers only need to marshal on success.
func (h *alertsHandler) buildFeed(w http.ResponseWriter, r *http.Request) (msg *gtfs.FeedMessage, regionID int64, ok bool) {
	id, parsed := ParseRegionSegment(r.PathValue("regionId"))
	if !parsed {
		h.writeNotFound(w)
		return nil, 0, false
	}

	ctx := r.Context()
	if _, err := h.deps.Regions.Get(ctx, id); err != nil {
		if errors.Is(err, regions.ErrNotFound) {
			h.writeNotFound(w)
			return nil, 0, false
		}
		h.serverError(w, id, "get region", err)
		return nil, 0, false
	}

	rows, err := h.deps.Alerts.Feed(ctx, id, includeTest(r), alerts.FeedLimit)
	if err != nil {
		h.serverError(w, id, "feed alerts", err)
		return nil, 0, false
	}

	built := alerts.BuildFeed(rows, alerts.FeedOptions{
		Now:             h.deps.Now(),
		DefaultDuration: alerts.DefaultDuration,
	})
	return built, id, true
}

// serverError writes an empty 500 body and logs the failure with the region
// id, per the design spec: a store error is never surfaced to the rider.
func (h *alertsHandler) serverError(w http.ResponseWriter, regionID int64, op string, err error) {
	h.deps.Logger.Error("httpapi: "+op, "region_id", regionID, "err", err)
	w.WriteHeader(http.StatusInternalServerError)
}

// writeNotFound writes the exact 404 contract for an unrecognised region.
func (h *alertsHandler) writeNotFound(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	if _, err := w.Write([]byte(notFoundBody)); err != nil {
		h.deps.Logger.Warn("httpapi: write 404 body", "err", err)
	}
}

// writeBody writes a successful response body after the status line and
// headers are already committed, so a failure here can only be logged, not
// turned into a different status code.
func (h *alertsHandler) writeBody(w http.ResponseWriter, regionID int64, body []byte) {
	if _, err := w.Write(body); err != nil {
		h.deps.Logger.Warn("httpapi: write response body", "region_id", regionID, "err", err)
	}
}

// includeTest reports whether test alerts should appear. Any non-blank value
// enables them, so ?test=0 includes them; blank means empty or
// whitespace-only, matching the reference implementation's Rails `blank?`.
func includeTest(r *http.Request) bool {
	return strings.TrimSpace(r.URL.Query().Get("test")) != ""
}

// ParseRegionSegment takes the leading run of ASCII digits from seg, parses
// it as an int64, and reports whether it succeeded. It ignores anything
// after the digits, because server-generated resource URLs carry an
// id-prefixed slug in the region segment (e.g. "1-puget-sound") and shipped
// clients replay those verbatim (design spec §2.4). Every malformed form —
// no leading digits, or a run that overflows int64 — reports false; callers
// must map that to 404, not 500, since unrecognised region identifiers are a
// normal condition (design spec §1.2).
func ParseRegionSegment(seg string) (int64, bool) {
	end := 0
	for end < len(seg) && seg[end] >= '0' && seg[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, false
	}
	id, err := strconv.ParseInt(seg[:end], 10, 64)
	if err != nil {
		return 0, false
	}
	return id, true
}
