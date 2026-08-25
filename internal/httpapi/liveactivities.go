package httpapi

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/OneBusAway/sidecar/internal/liveactivities"
	"github.com/OneBusAway/sidecar/internal/securetoken"
)

// liveActivityRegistrationsPerMinute is the sidecar-specific POST throttle
// (design spec §4).
const liveActivityRegistrationsPerMinute = 30

type liveActivitiesHandler struct{ deps Deps }

// register is the §6.1 upsert on (region, activity_id). Every field is
// re-read on re-registration, including apns_sandbox: a rotated token comes
// from the same build that sent the original. The response is 201 on both
// insert and update; the URL is the same either way so the client's stored
// DELETE URL stays valid across token rotations (design spec §2.1).
func (h *liveActivitiesHandler) register(w http.ResponseWriter, r *http.Request) {
	region, ok := resolveRegion(w, r, h.deps)
	if !ok {
		return
	}
	p, err := parseRequestParams(w, r, requestBodyLimit)
	if err != nil {
		errorWithMessages(w, h.deps.Logger, "Unable to register live activity", []string{err.Error()})
		return
	}
	var msgs []string
	activityID, _ := p.str("activity_id")
	if activityID == "" {
		msgs = append(msgs, "Activity can't be blank")
	}
	pushToken, _ := p.str("push_token")
	switch {
	case pushToken == "":
		msgs = append(msgs, "Push token can't be blank")
	case len(pushToken) > maxTokenLen:
		// A sidecar addition (the reference only checks presence): a junk
		// token would be stored and pushed to every minute for eight hours.
		msgs = append(msgs, fmt.Sprintf("Push token is too long (maximum is %d characters)", maxTokenLen))
	}
	stopID, _ := p.str("stop_id")
	if stopID == "" {
		msgs = append(msgs, "Stop can't be blank")
	}
	routeShortName, _ := p.str("route_short_name")
	if routeShortName == "" {
		msgs = append(msgs, "Route short name can't be blank")
	}
	tripHeadsign, _ := p.str("trip_headsign")
	if tripHeadsign == "" {
		msgs = append(msgs, "Trip headsign can't be blank")
	}
	if len(msgs) > 0 {
		errorWithMessages(w, h.deps.Logger, "Unable to register live activity", msgs)
		return
	}

	tripID, _ := p.str("trip_id")
	vehicleID, _ := p.str("vehicle_id")
	serviceDate, _ := p.int64("service_date") // non-numeric -> 0 = omitted
	var stopSeq *int64
	if v, ok := p.int64("stop_sequence"); ok {
		stopSeq = &v
	}
	token, err := securetoken.New()
	if err != nil {
		writeServerError(w, h.deps.Logger, region.ID, "mint live activity token", err)
		return
	}
	now := h.deps.Now()
	in := liveactivities.NewLiveActivity{
		RegionID: region.ID, Token: token, ExpiresAt: now.Add(liveactivities.Lifetime),
		ActivityID: activityID, PushToken: pushToken, APNSSandbox: parseAPNSSandbox(p, h.deps.Logger),
		StopID: stopID, RouteShortName: routeShortName, TripHeadsign: tripHeadsign,
		TripID: tripID, ServiceDate: serviceDate, VehicleID: vehicleID, StopSequence: stopSeq,
	}
	la, err := h.deps.LiveActivities.Upsert(r.Context(), in, now)
	if errors.Is(err, liveactivities.ErrDuplicate) {
		// Lost the concurrent first-registration race; one row exists now,
		// so a single retry takes the update path (spec §6.1).
		la, err = h.deps.LiveActivities.Upsert(r.Context(), in, now)
	}
	if err != nil {
		writeServerError(w, h.deps.Logger, region.ID, "upsert live activity",
			sanitizeToken(sanitizeToken(err, pushToken), token))
		return
	}
	writeJSON(w, h.deps.Logger, http.StatusCreated, map[string]string{
		"url": resourceURL(region, r, fmt.Sprintf("/api/v2/regions/%d/live_activities/%s", region.ID, la.Token)),
	})
}

// delete is the client-initiated dismissal: 204 only after the row is gone,
// 404 for an unknown token in this region, no end push (spec §6.4).
func (h *liveActivitiesHandler) delete(w http.ResponseWriter, r *http.Request) {
	region, ok := resolveRegion(w, r, h.deps)
	if !ok {
		return
	}
	token := r.PathValue("liveActivityToken")
	err := h.deps.LiveActivities.Delete(r.Context(), region.ID, token)
	switch {
	case errors.Is(err, liveactivities.ErrNotFound):
		w.WriteHeader(http.StatusNotFound)
	case err != nil:
		writeServerError(w, h.deps.Logger, region.ID, "delete live activity", sanitizeToken(err, token))
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}
