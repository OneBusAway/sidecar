package httpapi

import (
	"errors"
	"fmt"
	"mime"
	"net/http"
	"unicode/utf8"

	"github.com/OneBusAway/sidecar/internal/ghostbus"
	"github.com/OneBusAway/sidecar/internal/securetoken"
)

// ghostBusJSONBodyLimit is the §2.6 cap on JSON report bodies. JSON only:
// iOS sends form bodies whose percent-encoding can push a legal
// emoji-heavy comment well past 8 KB, and the padding attack this cap
// exists for (overflow a bounded throttle body-read so the parse fails
// uncounted) has no form-body analog here -- the params bag is parsed
// once, so there is no separate throttle peek to defeat.
const ghostBusJSONBodyLimit = 8192

type ghostBusHandler struct{ deps Deps }

func (h *ghostBusHandler) create(w http.ResponseWriter, r *http.Request) {
	region, ok := resolveRegion(w, r, h.deps)
	if !ok {
		return
	}
	isJSON := hasJSONContentType(r)
	if isJSON && r.ContentLength > ghostBusJSONBodyLimit {
		// A JSON body declaring more than the cap can only be padding; a
		// legitimate report is far smaller (spec §2.6). Bodyless 403 --
		// distinct in logs from the cross-site guard's 403, which carries a
		// JSON body.
		w.WriteHeader(http.StatusForbidden)
		return
	}
	limit := int64(requestBodyLimit)
	if isJSON {
		limit = ghostBusJSONBodyLimit
	}
	p, err := parseRequestParams(w, r, limit)
	if err != nil {
		if isJSON && errors.Is(err, errBodyTooLarge) {
			// Chunked or lying Content-Length: same padding, same 403.
			w.WriteHeader(http.StatusForbidden)
			return
		}
		errorWithMessages(w, h.deps.Logger, "Unable to save report", []string{err.Error()})
		return
	}

	uid, _ := p.str("user_identifier")
	if len(uid) > maxIdentifierLen {
		// Rejected before the throttle so attacker-sized strings never
		// become limiter map keys (design §2.4).
		errorWithMessages(w, h.deps.Logger, "Unable to save report",
			[]string{fmt.Sprintf("User identifier is too long (maximum is %d characters)", maxIdentifierLen)})
		return
	}
	// Blank identifiers skip the counter (no pooled nil bucket) and fail
	// presence validation below instead. The key is the identifier alone,
	// not (region, identifier): rack-attack's discriminator is the bare
	// value, so a device rotating regions shares one daily budget there
	// too.
	if uid != "" && !h.deps.GhostBusUserLimiter.Allow(uid, h.deps.Now()) {
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}

	in, msgs := ghostBusReportFromParams(region.ID, p)
	if len(msgs) > 0 {
		errorWithMessages(w, h.deps.Logger, "Unable to save report", msgs)
		return
	}

	for attempt := 0; ; attempt++ {
		if in.PublicID, err = securetoken.New(); err != nil {
			writeServerError(w, h.deps.Logger, region.ID, "ghostbus: mint token", err)
			return
		}
		_, err = h.deps.GhostBus.Create(r.Context(), in, h.deps.Now())
		switch {
		case err == nil:
			writeJSON(w, h.deps.Logger, http.StatusCreated, map[string]any{"id": in.PublicID})
			return
		case errors.Is(err, ghostbus.ErrDuplicate):
			// Validation-caught and race-caught duplicates are one path
			// here; clients treat this as a benign "got it already"
			// (spec §8 -- a 500 on the race would make the app retry
			// forever).
			errorWithMessages(w, h.deps.Logger, "already_reported",
				[]string{"User has already reported this trip"})
			return
		case errors.Is(err, ghostbus.ErrTokenCollision) && attempt == 0:
			continue // astronomically unlikely; re-mint once
		default:
			writeServerError(w, h.deps.Logger, region.ID, "ghostbus: create report", err)
			return
		}
	}
}

// ghostBusReportFromParams validates and assembles the §8 create fields.
// Message strings mirror Rails full_messages for reference fidelity; no
// shipped client displays them (iOS keys on the bare 422), so exact copy
// is a courtesy, not a contract.
func ghostBusReportFromParams(regionID int64, p params) (ghostbus.NewReport, []string) {
	in := ghostbus.NewReport{RegionID: regionID}
	var msgs []string

	in.UserIdentifier, _ = p.str("user_identifier")
	if in.UserIdentifier == "" {
		msgs = append(msgs, "User identifier can't be blank")
	}
	in.TripIdentifier, _ = p.str("trip_identifier")
	switch {
	case in.TripIdentifier == "":
		msgs = append(msgs, "Trip identifier can't be blank")
	case len(in.TripIdentifier) > maxIdentifierLen:
		msgs = append(msgs, fmt.Sprintf("Trip identifier is too long (maximum is %d characters)", maxIdentifierLen))
	}
	for _, f := range []struct {
		key  string
		dst  *string
		name string
	}{
		{"route_identifier", &in.RouteIdentifier, "Route identifier"},
		{"stop_identifier", &in.StopIdentifier, "Stop identifier"},
		{"vehicle_identifier", &in.VehicleIdentifier, "Vehicle identifier"},
	} {
		*f.dst, _ = p.str(f.key)
		if len(*f.dst) > maxIdentifierLen {
			msgs = append(msgs, fmt.Sprintf("%s is too long (maximum is %d characters)", f.name, maxIdentifierLen))
		}
	}

	// Epoch-ms integers; a non-integer coerces to null (spec §8: never
	// fuzzily parsed -- service_date is a dedupe key component), and a null
	// service_date then fails presence.
	if v, ok := p.int64("service_date"); ok {
		in.ServiceDate = v
	} else {
		msgs = append(msgs, "Service date can't be blank")
	}
	in.ScheduledArrivalAt = optInt64(p, "scheduled_arrival_at")
	in.PredictedArrivalAt = optInt64(p, "predicted_arrival_at")
	in.PredictionLastUpdatedAt = optInt64(p, "prediction_last_updated_at")
	in.StopSequence = optInt64(p, "stop_sequence")
	in.ScheduleDeviationMinutes = optInt64(p, "schedule_deviation_minutes")

	if v, ok := p.int64("wait_duration_minutes"); ok && ghostbus.ValidWaitDuration(v) {
		in.WaitDurationMinutes = v
	} else {
		msgs = append(msgs, "Wait duration minutes is not included in the list")
	}

	if v, present := p.boolish("predicted"); present {
		in.Predicted = &v
	}

	in.Comment, _ = p.str("comment")
	if utf8.RuneCountInString(in.Comment) > ghostbus.CommentMaxLen {
		msgs = append(msgs, fmt.Sprintf("Comment is too long (maximum is %d characters)", ghostbus.CommentMaxLen))
	}

	var present, valid bool
	if in.UserLatitude, present, valid = coordinate(p, "user_latitude", 90); present && !valid {
		msgs = append(msgs, "User latitude must be between -90 and 90")
		in.UserLatitude = nil
	}
	if in.UserLongitude, present, valid = coordinate(p, "user_longitude", 180); present && !valid {
		msgs = append(msgs, "User longitude must be between -180 and 180")
		in.UserLongitude = nil
	}
	return in, msgs
}

func optInt64(p params, key string) *int64 {
	if v, ok := p.int64(key); ok {
		return &v
	}
	return nil
}

func hasJSONContentType(r *http.Request) bool {
	ct, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && ct == "application/json"
}
