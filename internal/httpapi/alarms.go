package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/OneBusAway/sidecar/internal/alarms"
	"github.com/OneBusAway/sidecar/internal/obaapi"
	"github.com/OneBusAway/sidecar/internal/pushreg"
	"github.com/OneBusAway/sidecar/internal/regions"
	"github.com/OneBusAway/sidecar/internal/securetoken"
)

// alarmsBodyLimit caps registration bodies the same way pushRegsBodyLimit
// does (spec §2.6): generous for every documented field, bounded against an
// unauthenticated caller's free memory amplifier.
const alarmsBodyLimit = 64 << 10

type alarmsHandler struct{ deps Deps }

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
		p, err := parseRequestParams(w, r, alarmsBodyLimit)
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
		if len(msgs) > 0 {
			errorWithMessages(w, h.deps.Logger, "Unable to register alarm", msgs)
			return
		}

		sandbox := false
		if version == 2 && os == pushreg.OSIOS {
			sandbox = parseAPNSSandbox(p, h.deps.Logger)
		}

		stopID, _ := p.str("stop_id")
		tripID, _ := p.str("trip_id")
		serviceDate, _ := p.int64("service_date") // non-numeric -> 0 = omitted
		vehicleID, _ := p.str("vehicle_id")
		var stopSeq *int64
		if v, ok := p.int64("stop_sequence"); ok {
			stopSeq = &v
		}
		sb, sbOK := p.int64("seconds_before")
		secondsBefore := alarms.NormalizeSecondsBefore(sb, sbOK)

		if version == 1 {
			key := alarms.V1Key{RegionID: region.ID, UserPushID: userPushID,
				TripID: tripID, StopID: stopID, ServiceDate: serviceDate}
			if existing, findErr := h.deps.Alarms.FindV1(r.Context(), key); findErr == nil {
				// Idempotent re-POST: hand back the existing alarm untouched
				// (spec §5.1 -- legacy clients re-POST aggressively).
				writeJSON(w, h.deps.Logger, http.StatusCreated,
					map[string]string{"url": alarmURL(region, r, version, existing.Token)})
				return
			} else if !errors.Is(findErr, alarms.ErrNotFound) {
				writeServerError(w, h.deps.Logger, region.ID, "find v1 alarm", sanitizeToken(findErr, userPushID))
				return
			}
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
				key := alarms.V1Key{RegionID: region.ID, UserPushID: userPushID,
					TripID: tripID, StopID: stopID, ServiceDate: serviceDate}
				if existing, ferr := h.deps.Alarms.FindV1(r.Context(), key); ferr == nil {
					writeJSON(w, h.deps.Logger, http.StatusCreated,
						map[string]string{"url": alarmURL(region, r, version, existing.Token)})
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
			// documented reference wart -- no apns_sandbox propagation.
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

// alarmURL builds the §2.4 creation-response URL. The region's directory
// sidecarBaseUrl wins; a region without one falls back to this request's
// Host over https (the only scheme the apps will talk to us on).
func alarmURL(region regions.Region, r *http.Request, version int, token string) string {
	base := strings.TrimRight(region.SidecarBaseURL, "/")
	if base == "" {
		base = "https://" + r.Host
	}
	return fmt.Sprintf("%s/api/v%d/regions/%d/alarms/%s", base, version, region.ID, token)
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
