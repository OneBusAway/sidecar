package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/OneBusAway/sidecar/internal/ghostbus"
)

// ghostBusReportJSON is one ghost bus report, in full (design spec section
// 5, region API keys and admin API spec). It carries every ghostbus.Report
// field: ServiceDate, ScheduledArrivalAt, PredictedArrivalAt and
// PredictionLastUpdatedAt are OBA identifiers and dedupe keys, not
// instants, so they cross the wire as the epoch-millisecond integers they
// arrived as -- reformatting one as RFC 3339 would break the dedupe key.
// CreatedAt and SnapshotCapturedAt are real instants and go through
// formatInstant.
type ghostBusReportJSON struct {
	ID                       int64           `json:"id"`
	RegionID                 int64           `json:"region_id"`
	PublicID                 string          `json:"public_id"`
	UserIdentifier           string          `json:"user_identifier"`
	TripIdentifier           string          `json:"trip_identifier"`
	ServiceDate              int64           `json:"service_date"`
	RouteIdentifier          string          `json:"route_identifier"`
	StopIdentifier           string          `json:"stop_identifier"`
	VehicleIdentifier        string          `json:"vehicle_identifier"`
	StopSequence             *int64          `json:"stop_sequence"`
	Predicted                *bool           `json:"predicted"`
	ScheduleDeviationMinutes *int64          `json:"schedule_deviation_minutes"`
	WaitDurationMinutes      int64           `json:"wait_duration_minutes"`
	Comment                  string          `json:"comment"`
	UserLatitude             *float64        `json:"user_latitude"`
	UserLongitude            *float64        `json:"user_longitude"`
	ScheduledArrivalAt       *int64          `json:"scheduled_arrival_at"`
	PredictedArrivalAt       *int64          `json:"predicted_arrival_at"`
	PredictionLastUpdatedAt  *int64          `json:"prediction_last_updated_at"`
	SnapshotStatus           string          `json:"snapshot_status"`
	SnapshotJSON             json.RawMessage `json:"snapshot_json"`
	SnapshotCapturedAt       *string         `json:"snapshot_captured_at"`
	SnapshotAttempts         int64           `json:"snapshot_attempts"`
	CreatedAt                string          `json:"created_at"`
}

// toGhostBusReportJSON renders a stored report. SnapshotJSON is emitted as
// the raw captured document (via json.RawMessage) when it parses, and as
// null when the column is empty or holds something that is not valid
// JSON: the stored column is the source of truth, and a malformed document
// degrades the response to null rather than failing it outright, matching
// WriteReportsCSV's own posture on the same column (see reportRow in
// internal/ghostbus/csv.go).
func toGhostBusReportJSON(r ghostbus.Report) ghostBusReportJSON {
	out := ghostBusReportJSON{
		ID: r.ID, RegionID: r.RegionID, PublicID: r.PublicID,
		UserIdentifier: r.UserIdentifier, TripIdentifier: r.TripIdentifier, ServiceDate: r.ServiceDate,
		RouteIdentifier: r.RouteIdentifier, StopIdentifier: r.StopIdentifier, VehicleIdentifier: r.VehicleIdentifier,
		StopSequence: r.StopSequence, Predicted: r.Predicted,
		ScheduleDeviationMinutes: r.ScheduleDeviationMinutes,
		WaitDurationMinutes:      r.WaitDurationMinutes, Comment: r.Comment,
		UserLatitude: r.UserLatitude, UserLongitude: r.UserLongitude,
		ScheduledArrivalAt: r.ScheduledArrivalAt, PredictedArrivalAt: r.PredictedArrivalAt,
		PredictionLastUpdatedAt: r.PredictionLastUpdatedAt,
		SnapshotStatus:          r.SnapshotStatus, SnapshotAttempts: r.SnapshotAttempts,
		CreatedAt: formatInstant(r.CreatedAt),
	}
	if r.SnapshotJSON != "" && json.Valid([]byte(r.SnapshotJSON)) {
		out.SnapshotJSON = json.RawMessage(r.SnapshotJSON)
	}
	if r.SnapshotCapturedAt != nil {
		s := formatInstant(*r.SnapshotCapturedAt)
		out.SnapshotCapturedAt = &s
	}
	return out
}

// parseSinceQuery reads ?since=RFC3339 as epoch seconds. Absent is 0, which
// ListForExport reads as "everything". An explicit UTC offset is required:
// interpreting a naive datetime in the server's local zone is exactly the
// machine-local dependence this repo bans everywhere else, and here it would
// silently shift an agency's export window.
func parseSinceQuery(r *http.Request) (int64, error) {
	raw := r.URL.Query().Get("since")
	if raw == "" {
		return 0, nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return 0, fmt.Errorf("since must be RFC 3339 with an explicit UTC offset: %w", err)
	}
	return t.Unix(), nil
}

// adminGhostBusHandler serves the read-only ghost bus report endpoints
// (region API keys and admin API design spec section 5): the JSON list, the
// CSV export, and the single-report lookup by public id. There is
// deliberately no write surface here -- reports are rider-submitted, and
// the rider-facing create route and its 422 already_reported contract are
// untouched by this family.
type adminGhostBusHandler struct {
	deps Deps
}

// list handles GET /api/admin/v1/regions/{regionId}/ghost_bus_reports.
func (h *adminGhostBusHandler) list(w http.ResponseWriter, r *http.Request) {
	region, ok := mustRegion(w, r, h.deps)
	if !ok {
		return
	}
	since, err := parseSinceQuery(r)
	if err != nil {
		writeJSONError(w, h.deps.Logger, http.StatusBadRequest, err.Error())
		return
	}
	list, err := h.deps.GhostBus.ListForExport(r.Context(), region.ID, since)
	if err != nil {
		serverErrorJSON(w, h.deps.Logger, "list ghost bus reports", err)
		return
	}
	out := make([]ghostBusReportJSON, 0, len(list))
	for _, rep := range list {
		out = append(out, toGhostBusReportJSON(rep))
	}
	writeJSON(w, h.deps.Logger, http.StatusOK, out)
}

// csv handles GET /api/admin/v1/regions/{regionId}/ghost_bus_reports.csv.
//
// The filename is fixed and server-generated (writeCSVHeaders), matching
// every other CSV export in this API: nothing rider-supplied reaches a
// Content-Disposition header.
func (h *adminGhostBusHandler) csv(w http.ResponseWriter, r *http.Request) {
	region, ok := mustRegion(w, r, h.deps)
	if !ok {
		return
	}
	since, err := parseSinceQuery(r)
	if err != nil {
		writeJSONError(w, h.deps.Logger, http.StatusBadRequest, err.Error())
		return
	}
	list, err := h.deps.GhostBus.ListForExport(r.Context(), region.ID, since)
	if err != nil {
		serverErrorJSON(w, h.deps.Logger, "list ghost bus reports", err)
		return
	}
	writeCSVHeaders(w, fmt.Sprintf("ghost-bus-reports-%d.csv", region.ID))
	// The status line is already committed by writeCSVHeaders' Content-Type
	// (the first Write below commits it if nothing already has); a write
	// failure past that point can only be logged, never turned into a
	// different status code.
	if err := ghostbus.WriteReportsCSV(w, region, list); err != nil {
		h.deps.Logger.Warn("httpapi: write ghost bus reports csv", "err", err)
	}
}

// get handles
// GET /api/admin/v1/regions/{regionId}/ghost_bus_reports/{publicId}.
func (h *adminGhostBusHandler) get(w http.ResponseWriter, r *http.Request) {
	rep, ok := loadReport(w, r, h.deps)
	if !ok {
		return
	}
	writeJSON(w, h.deps.Logger, http.StatusOK, toGhostBusReportJSON(rep))
}
