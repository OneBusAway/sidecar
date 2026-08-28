package httpapi

import (
	"net/http"

	"github.com/OneBusAway/sidecar/internal/alarms"
)

// adminAlarmJSON is one alarm as the admin API renders it (design spec
// section 5.4).
//
// Token and UserPushID are DELIBERATELY ABSENT. They are push credentials,
// not UI data, and this is a hand-written projection rather than a
// marshalled alarms.Alarm precisely so a field added to the domain type
// cannot start shipping them. RegionID is also absent: the route is already
// region-scoped by {regionId}, so it would be redundant on every row.
//
// ServiceDate is epoch MILLISECONDS -- an OBA identifier, not an instant --
// and passes through as an integer. CreatedAt is a real instant and is
// RFC 3339 UTC.
type adminAlarmJSON struct {
	ID              int64  `json:"id"`
	APIVersion      int    `json:"api_version"`
	OperatingSystem string `json:"operating_system"`
	StopID          string `json:"stop_id"`
	TripID          string `json:"trip_id"`
	ServiceDate     int64  `json:"service_date"`
	VehicleID       string `json:"vehicle_id"`
	StopSequence    *int64 `json:"stop_sequence"`
	SecondsBefore   int64  `json:"seconds_before"`
	Message         string `json:"message"`
	FailureCount    int64  `json:"failure_count"`
	CreatedAt       string `json:"created_at"`
}

// toAdminAlarmJSON renders a stored alarm for the admin API, deliberately
// leaving Token and UserPushID behind.
func toAdminAlarmJSON(a alarms.Alarm) adminAlarmJSON {
	return adminAlarmJSON{
		ID:              a.ID,
		APIVersion:      a.APIVersion,
		OperatingSystem: a.OperatingSystem,
		StopID:          a.StopID,
		TripID:          a.TripID,
		ServiceDate:     a.ServiceDate,
		VehicleID:       a.VehicleID,
		StopSequence:    a.StopSequence,
		SecondsBefore:   a.SecondsBefore,
		Message:         a.Message,
		FailureCount:    a.FailureCount,
		CreatedAt:       formatInstant(a.CreatedAt),
	}
}

// adminAlarmsHandler serves the read-only alarm routes (design spec section
// 5.4). There is no write surface here -- alarms are created and deleted
// only through the rider-facing v1/v2 endpoints -- so this handler is a view
// onto rows the push scheduler already owns.
type adminAlarmsHandler struct {
	deps Deps
}

// list handles GET /api/admin/v1/regions/{regionId}/alarms.
func (h *adminAlarmsHandler) list(w http.ResponseWriter, r *http.Request) {
	region, ok := mustRegion(w, r, h.deps)
	if !ok {
		return
	}
	list, err := h.deps.Alarms.ListByRegion(r.Context(), region.ID)
	if err != nil {
		serverErrorJSON(w, h.deps.Logger, "list alarms", err)
		return
	}
	// make, not nil: the response is always an array.
	out := make([]adminAlarmJSON, 0, len(list))
	for _, a := range list {
		out = append(out, toAdminAlarmJSON(a))
	}
	writeJSON(w, h.deps.Logger, http.StatusOK, out)
}

// get handles GET /api/admin/v1/regions/{regionId}/alarms/{id}.
func (h *adminAlarmsHandler) get(w http.ResponseWriter, r *http.Request) {
	a, ok := loadAlarm(w, r, h.deps)
	if !ok {
		return
	}
	writeJSON(w, h.deps.Logger, http.StatusOK, toAdminAlarmJSON(a))
}
