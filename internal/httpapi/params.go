package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// requestBodyLimit caps every rider-facing POST/DELETE body this package
// parses. The largest legitimate field is a 4096-char push token; 64 KB
// leaves room for every documented field several times over while denying
// the free memory amplifier an unbounded read would hand an unauthenticated
// caller (spec §2.6).
const requestBodyLimit = 64 << 10

type params struct{ m map[string]any }

// errBodyTooLarge is the one parse failure whose text is fit for a rider-
// facing body; handlers that distinguish it from "malformed" check with
// errors.Is rather than comparing message text.
var errBodyTooLarge = errors.New("request body too large")

func parseRequestParams(w http.ResponseWriter, r *http.Request, maxBytes int64) (params, error) {
	m := make(map[string]any)
	for k, vs := range r.URL.Query() {
		if len(vs) > 0 {
			m[k] = vs[0]
		}
	}
	// The body is read explicitly rather than via r.ParseForm: net/http only
	// parses form bodies for POST/PUT/PATCH, but spec §4's opt-out DELETE
	// carries its token as "query or body parameter".
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return params{}, errBodyTooLarge
		}
		return params{}, fmt.Errorf("read request body: %w", err)
	}
	if len(body) == 0 {
		// An empty body is an empty param set, whatever the content type --
		// a DELETE with only ?token= must not fail a JSON decode.
		return params{m: m}, nil
	}
	// An unparseable Content-Type is treated as "not JSON", same as a
	// missing header: the body still falls through to form parsing below.
	ct, _, parseErr := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if parseErr == nil && ct == "application/json" {
		var decoded map[string]any
		if unmarshalErr := json.Unmarshal(body, &decoded); unmarshalErr != nil {
			return params{}, fmt.Errorf("invalid JSON body: %w", unmarshalErr)
		}
		for k, v := range decoded {
			if v == nil {
				continue // JSON null counts as absent (spec §4)
			}
			m[k] = v
		}
		return params{m: m}, nil
	}
	form, err := url.ParseQuery(string(body))
	if err != nil {
		return params{}, fmt.Errorf("invalid form body: %w", err)
	}
	for k, vs := range form {
		if len(vs) > 0 {
			m[k] = vs[0]
		}
	}
	return params{m: m}, nil
}

func (p params) str(key string) (string, bool) {
	v, ok := p.m[key]
	if !ok {
		return "", false
	}
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t), true
	case bool:
		return strconv.FormatBool(t), true
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64), true
	default:
		return "", false
	}
}

// rawString returns key only if it arrived as a string, untrimmed and
// uncoerced -- for payloads that are themselves encoded documents (a
// JSON-array string), where str's trimming and number/bool coercion would
// either corrupt the document or accept a native array the contract
// forbids.
func (p params) rawString(key string) (string, bool) {
	s, ok := p.m[key].(string)
	return s, ok
}

func (p params) int64(key string) (int64, bool) {
	v, ok := p.m[key]
	if !ok {
		return 0, false
	}
	switch t := v.(type) {
	case float64:
		if t != math.Trunc(t) {
			return 0, false
		}
		return int64(t), true
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64)
		if err != nil {
			return 0, false
		}
		return n, true
	default:
		return 0, false
	}
}

func (p params) boolish(key string) (val, present bool) {
	s, ok := p.str(key)
	if !ok {
		return false, false
	}
	switch strings.ToLower(s) {
	case "1", "t", "true", "on":
		return true, true
	case "0", "f", "false", "off":
		return false, true
	default:
		return false, false
	}
}

// parseAPNSSandbox applies the §2.7 allow-list via boolish -- one truth
// table, defined once. The two failure directions are asymmetric: a
// production token misrouted to the sandbox bounces in front of a rider,
// so anything unrecognized is production, logged rather than guessed.
func parseAPNSSandbox(p params, logger *slog.Logger) bool {
	val, present := p.boolish("apns_sandbox")
	if present {
		return val
	}
	if raw, ok := p.str("apns_sandbox"); ok && raw != "" {
		logger.Warn("httpapi: unrecognized apns_sandbox value treated as production", "value", raw)
	}
	return false
}

// tripIdentity is the optional trip-instance metadata alarms (spec §2.x)
// and Live Activities (spec §6.1) both accept alongside a stop: parsed the
// same way in both because both feed the same obaapi lookups.
type tripIdentity struct {
	TripID       string // "" = omitted
	ServiceDate  int64  // epoch ms; 0 = omitted (non-numeric input reads as 0)
	VehicleID    string // "" = omitted
	StopSequence *int64 // nil = omitted; 0 is a real value
}

func parseTripIdentity(p params) tripIdentity {
	var id tripIdentity
	id.TripID, _ = p.str("trip_id")
	id.ServiceDate, _ = p.int64("service_date")
	id.VehicleID, _ = p.str("vehicle_id")
	if v, ok := p.int64("stop_sequence"); ok {
		id.StopSequence = &v
	}
	return id
}
