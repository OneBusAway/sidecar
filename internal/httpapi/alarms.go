package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/OneBusAway/sidecar/internal/alarms"
	"github.com/OneBusAway/sidecar/internal/obaapi"
	"github.com/OneBusAway/sidecar/internal/pushreg"
	"github.com/OneBusAway/sidecar/internal/regions"
	"github.com/OneBusAway/sidecar/internal/securetoken"
)

type alarmsHandler struct{ deps Deps }

const (
	// alarmCreatesPerMinute is the per-source throttle shared by the V1 and
	// V2 create routes (SC-01). A rider sets an alarm by hand, a few a
	// minute at most; the headroom is for many riders behind one carrier
	// NAT address (an IPv6 client is keyed on its /64).
	alarmCreatesPerMinute = 60
	// MaxAlarmsPerDevice caps the live alarms one user_push_id may hold
	// across every region (SC-01). Each alarm is one departure a rider
	// asked to be woken for, and alarms delete themselves once they fire
	// or the bus leaves, so a real device holds a handful.
	MaxAlarmsPerDevice = 50
	// maxAlarmIDLen caps stop_id, trip_id and vehicle_id (SC-01). OBA ids
	// are agency-prefixed GTFS ids, tens of bytes long.
	maxAlarmIDLen = 255
)

// create handles both API versions (spec §5.1-§5.2). The two versions share
// almost everything -- validation of user_push_id, trip-field handling,
// message composition, token minting -- and differ only in: V2 requires
// operating_system where V1 defaults an invalid/absent one to "ios"; V2
// accepts apns_sandbox where V1 ignores it; V1 dedupes on (region, user,
// trip, stop, service_date) and never touches the push registry; V2 always
// upserts the push registry as a side effect.
func (h *alarmsHandler) create(version int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		region, ok := resolveRegion(w, r, h.deps)
		if !ok {
			return
		}
		p, err := parseRequestParams(w, r, requestBodyLimit)
		if err != nil {
			errorWithMessages(w, h.deps.Logger, "Unable to register alarm", []string{err.Error()})
			return
		}

		var msgs []string
		userPushID, _ := p.str("user_push_id")
		if userPushID == "" {
			msgs = append(msgs, "Push identifier can't be blank")
		}
		os, _ := p.str("operating_system")
		if version == 2 {
			switch {
			case os == "":
				msgs = append(msgs, "Operating system can't be blank")
			case os != pushreg.OSIOS && os != pushreg.OSAndroid:
				msgs = append(msgs, "Operating system is not included in the list")
			}
		} else if os != pushreg.OSIOS && os != pushreg.OSAndroid {
			// V1 treats an invalid value like an absent one (spec §5.2).
			os = pushreg.OSIOS
		}
		stopID, _ := p.str("stop_id")
		trip := parseTripIdentity(p)
		msgs = append(msgs, alarmBoundsMessages(userPushID, stopID, trip, h.deps.Now())...)
		if len(msgs) > 0 {
			errorWithMessages(w, h.deps.Logger, "Unable to register alarm", msgs)
			return
		}

		sandbox := false
		if version == 2 && os == pushreg.OSIOS {
			sandbox = parseAPNSSandbox(p, h.deps.Logger)
		}

		tripID, serviceDate, vehicleID, stopSeq := trip.TripID, trip.ServiceDate, trip.VehicleID, trip.StopSequence
		sb, sbOK := p.int64("seconds_before")
		secondsBefore := alarms.NormalizeSecondsBefore(sb, sbOK)

		v1Key := alarms.V1Key{RegionID: region.ID, UserPushID: userPushID,
			TripID: tripID, StopID: stopID, ServiceDate: serviceDate}
		if version == 1 {
			// Idempotent re-POST: hand back the existing alarm untouched
			// (spec §5.1 -- legacy clients re-POST aggressively).
			if done := h.respondExistingV1(w, r, region, v1Key); done {
				return
			}
		}
		if full, capErr := h.deviceAtCap(r.Context(), userPushID); capErr != nil {
			writeServerError(w, h.deps.Logger, region.ID, "count device alarms", sanitizeToken(capErr, userPushID))
			return
		} else if full {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}

		message := h.composeMessage(r.Context(), region, stopID, tripID, serviceDate, vehicleID, stopSeq, secondsBefore)

		token, err := securetoken.New()
		if err != nil {
			writeServerError(w, h.deps.Logger, region.ID, "mint alarm token", err)
			return
		}
		created, err := h.deps.Alarms.Create(r.Context(), alarms.NewAlarm{
			RegionID: region.ID, Token: token, APIVersion: version,
			UserPushID: userPushID, OperatingSystem: os, APNSSandbox: sandbox,
			StopID: stopID, TripID: tripID, ServiceDate: serviceDate,
			VehicleID: vehicleID, StopSequence: stopSeq,
			SecondsBefore: secondsBefore, Message: message,
		}, h.deps.Now())
		if err != nil {
			if version == 1 && errors.Is(err, alarms.ErrDuplicate) {
				// Lost the race to a concurrent identical registration; the
				// winner is the alarm this client asked for.
				if done := h.respondExistingV1(w, r, region, v1Key); done {
					return
				}
			}
			// Both secrets can appear in a store error: user_push_id is a
			// query key on the failed insert, and the freshly minted alarm
			// token is a column value on the same row.
			writeServerError(w, h.deps.Logger, region.ID, "create alarm",
				sanitizeToken(sanitizeToken(err, userPushID), token))
			return
		}

		if version == 2 {
			// Every V2 alarm creation also refreshes the alert-push audience
			// (spec §5.2): OS and last_seen_at only, no locale, and -- the
			// documented reference wart -- no apns_sandbox propagation. This
			// upsert bypasses the registration endpoint's own validation; the
			// length cap that endpoint enforces (maxTokenLen, spec §2.6) was
			// already applied to user_push_id above (alarmBoundsMessages),
			// and the alarm throttle bounds how often this path runs.
			if err := h.deps.PushRegs.Upsert(r.Context(), pushreg.Upsert{
				RegionID: region.ID, Token: userPushID, OperatingSystem: os,
			}, h.deps.Now()); err != nil {
				// The alarm exists and its 201 must stand; the registry miss
				// only costs alert-push reach. err's Token is user_push_id on
				// this path (pushreg.Upsert.Token), never logged raw.
				h.deps.Logger.Warn("httpapi: alarm side-effect registration failed",
					"region_id", region.ID, "err", sanitizeToken(err, userPushID))
			}
		}

		writeJSON(w, h.deps.Logger, http.StatusCreated,
			map[string]string{"url": alarmURL(region, r, version, created.Token)})
	}
}

// alarmBoundsMessages applies the SC-01 size and age limits: the device
// token is capped like every other push token (maxTokenLen), the trip
// identifiers at maxAlarmIDLen, and a non-zero service date must be within
// alarms.ServiceDateWindow of now.
func alarmBoundsMessages(userPushID, stopID string, trip tripIdentity, now time.Time) []string {
	var msgs []string
	if len(userPushID) > maxTokenLen {
		msgs = append(msgs, fmt.Sprintf("Push identifier is too long (maximum is %d characters)", maxTokenLen))
	}
	for _, f := range []struct{ label, value string }{
		{"Stop", stopID}, {"Trip", trip.TripID}, {"Vehicle", trip.VehicleID},
	} {
		if len(f.value) > maxAlarmIDLen {
			msgs = append(msgs, fmt.Sprintf("%s is too long (maximum is %d characters)", f.label, maxAlarmIDLen))
		}
	}
	if !alarms.ServiceDateInWindow(trip.ServiceDate, now) {
		msgs = append(msgs, "Service date is out of range")
	}
	return msgs
}

// deviceAtCap reports whether userPushID already holds MaxAlarmsPerDevice
// live alarms. Checked after the V1 idempotency lookup, so re-POSTing an
// alarm the device already has still answers 201.
func (h *alarmsHandler) deviceAtCap(ctx context.Context, userPushID string) (bool, error) {
	n, err := h.deps.Alarms.CountByUserPushID(ctx, userPushID)
	if err != nil {
		return false, err
	}
	return n >= MaxAlarmsPerDevice, nil
}

// respondExistingV1 answers a V1 registration from the idempotency key
// (spec section 5.1): an existing alarm gets its original URL back with no
// fields applied, and a store failure gets a sanitized 500. It reports
// whether it wrote a response; ErrNotFound reports false so the caller
// proceeds to create. The version in the URL is always 1 -- only V1
// dedupes.
func (h *alarmsHandler) respondExistingV1(w http.ResponseWriter, r *http.Request, region regions.Region, key alarms.V1Key) bool {
	existing, err := h.deps.Alarms.FindV1(r.Context(), key)
	switch {
	case err == nil:
		writeJSON(w, h.deps.Logger, http.StatusCreated,
			map[string]string{"url": alarmURL(region, r, 1, existing.Token)})
		return true
	case errors.Is(err, alarms.ErrNotFound):
		return false
	default:
		writeServerError(w, h.deps.Logger, region.ID, "find v1 alarm", sanitizeToken(err, key.UserPushID))
		return true
	}
}

// composeMessage resolves the arrival at creation time (spec §5.2). Any
// failure -- missing trip identity, unconfigured key, upstream error --
// degrades to the generic message on both versions: V1 here is the
// spec-sanctioned thin alias of V2, so V1's historical unstructured 500 is
// deliberately not reproduced.
func (h *alarmsHandler) composeMessage(ctx context.Context, region regions.Region,
	stopID, tripID string, serviceDate int64, vehicleID string, stopSeq *int64,
	secondsBefore int64) string {
	if h.deps.OBA == nil || stopID == "" || tripID == "" || serviceDate == 0 {
		return alarms.GenericMessage(secondsBefore)
	}
	dep, err := h.deps.OBA.ArrivalAndDeparture(ctx, region, obaapi.DepartureQuery{
		StopID: stopID, TripID: tripID, ServiceDate: serviceDate,
		VehicleID: vehicleID, StopSequence: stopSeq,
	})
	if err != nil || dep.RouteShortName == "" || dep.TripHeadsign == "" {
		return alarms.GenericMessage(secondsBefore)
	}
	return alarms.ComposeMessage(dep.RouteShortName, dep.TripHeadsign, secondsBefore)
}

// resourceURL builds a §2.4 creation-response URL for path (which starts
// with "/api/…"). The region's directory sidecarBaseUrl wins; a region
// without one falls back to this request's Host over https (the only scheme
// the apps will talk to us on).
func resourceURL(region regions.Region, r *http.Request, path string) string {
	base := strings.TrimRight(region.SidecarBaseURL, "/")
	if base == "" {
		base = "https://" + r.Host
	}
	return base + path
}

// alarmURL builds the §2.4 creation-response URL. The region's directory
// sidecarBaseUrl wins; a region without one falls back to this request's
// Host over https (the only scheme the apps will talk to us on).
func alarmURL(region regions.Region, r *http.Request, version int, token string) string {
	return resourceURL(region, r, fmt.Sprintf("/api/v%d/regions/%d/alarms/%s", version, region.ID, token))
}

func (h *alarmsHandler) delete(w http.ResponseWriter, r *http.Request) {
	region, ok := resolveRegion(w, r, h.deps)
	if !ok {
		return
	}
	token := r.PathValue("alarmToken")
	err := h.deps.Alarms.Delete(r.Context(), region.ID, token)
	switch {
	case errors.Is(err, alarms.ErrNotFound):
		w.WriteHeader(http.StatusNotFound)
	case err != nil:
		// 204 is a binding "it's cancelled" (spec §2.5); a failed delete
		// must surface as a 5xx, never a false positive. The store error can
		// embed the path token (it's the delete's WHERE-clause value); never
		// log it raw.
		writeServerError(w, h.deps.Logger, region.ID, "delete alarm", sanitizeToken(err, token))
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}
