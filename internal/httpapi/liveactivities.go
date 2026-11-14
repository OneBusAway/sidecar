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

// maxLiveActivityFieldLen caps every rider-supplied text field of a
// registration (SC-03, SC-10). activity_id is ActivityKit's opaque id (a
// UUID string in practice, but documented only as a String), the rest are
// GTFS ids and display strings tens of bytes long.
const maxLiveActivityFieldLen = 255

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
	trip := parseTripIdentity(p)
	for _, f := range []struct{ label, value string }{
		{"Stop", stopID}, {"Route short name", routeShortName},
		{"Trip headsign", tripHeadsign}, {"Trip", trip.TripID}, {"Vehicle", trip.VehicleID},
	} {
		if len(f.value) > maxLiveActivityFieldLen {
			msgs = append(msgs, fmt.Sprintf("%s is too long (maximum is %d characters)", f.label, maxLiveActivityFieldLen))
		}
	}
	if len(msgs) > 0 {
		errorWithMessages(w, h.deps.Logger, "Unable to register live activity", msgs)
		return
	}

	usage, err := h.deps.LiveActivities.Usage(r.Context(), region.ID, activityID, pushToken, stopID)
	if err != nil {
		writeServerError(w, h.deps.Logger, region.ID, "live activity usage", sanitizeToken(err, pushToken))
		return
	}
	switch h.deps.LiveActivityCaps.Check(usage, stopID) {
	case liveactivities.CapPushToken:
		w.WriteHeader(http.StatusTooManyRequests)
		return
	case liveactivities.CapCapacity:
		h.deps.Logger.Warn("httpapi: live activity registration refused at capacity",
			"region_id", region.ID, "rows", usage.Total, "region_rows", usage.InRegion, "region_stops", usage.StopsInRegion)
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	case liveactivities.CapOK:
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
		TripID: trip.TripID, ServiceDate: trip.ServiceDate, VehicleID: trip.VehicleID, StopSequence: trip.StopSequence,
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
