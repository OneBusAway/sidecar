package httpapi

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/MobilityData/gtfs-realtime-bindings/golang/gtfs"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/OneBusAway/sidecar/internal/alerts"
)

// notFoundBody is the exact 404 body the design spec (§1.2, §2.5) requires
// for an unrecognised region.
const notFoundBody = `{"error":"Couldn't find Region"}`

// alertsHandler serves the GTFS-realtime service alerts feed.
type alertsHandler struct {
	deps Deps
}

// feedCacheControl lets a CDN or the app's URL cache hold a successful feed
// for a minute and keep serving it for ten more if the origin is down --
// which it is for ~30 s on every deploy, since the SQLite disk pins the
// service to one restarting instance (README, Deployment). Sixty seconds is
// well inside how quickly riders expect a newly published alert to appear,
// and the feed is the most-requested route by a wide margin. Error responses
// carry no Cache-Control and so are not cached.
const feedCacheControl = "public, max-age=60, stale-if-error=600"

// feedBinary serves GET /api/v1/regions/{regionId}/alerts: the feed as a
// binary protobuf.
func (h *alertsHandler) feedBinary(w http.ResponseWriter, r *http.Request) {
	msg, id, ok := h.buildFeed(w, r)
	if !ok {
		return
	}

	body, err := proto.Marshal(msg)
	if err != nil {
		writeServerError(w, h.deps.Logger, id, "marshal protobuf", err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", feedCacheControl)
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
		writeServerError(w, h.deps.Logger, id, "marshal pbtext", err)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("Cache-Control", feedCacheControl)
	w.WriteHeader(http.StatusOK)
	h.writeBody(w, id, body)
}

// buildFeed resolves the region segment, looks up the region, fetches the
// feed rows, and renders them. It writes the response itself on failure (404
// or 500) and returns ok=false, so callers only need to marshal on success.
func (h *alertsHandler) buildFeed(w http.ResponseWriter, r *http.Request) (msg *gtfs.FeedMessage, regionID int64, ok bool) {
	region, ok := resolveRegion(w, r, h.deps)
	if !ok {
		return nil, 0, false
	}
	id, ctx := region.ID, r.Context()

	rows, err := h.deps.Alerts.Feed(ctx, id, includeTest(r), alerts.FeedLimit)
	if err != nil {
		writeServerError(w, h.deps.Logger, id, "feed alerts", err)
		return nil, 0, false
	}

	built := alerts.BuildFeed(rows, alerts.FeedOptions{
		Now:             h.deps.Now(),
		DefaultDuration: alerts.DefaultDuration,
		OnUnknownEnum: func(kind, name string) {
			h.deps.Logger.Warn("httpapi: unmappable enum value, degrading to UNKNOWN_*",
				"region_id", id, "kind", kind, "name", name)
		},
	})
	return built, id, true
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
