package httpapi

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"

	"github.com/OneBusAway/sidecar/internal/pushreg"
	"github.com/OneBusAway/sidecar/internal/ratelimit"
)

const (
	maxTokenLen       = 4096
	maxDescriptionLen = 255
)

type pushRegsHandler struct{ deps Deps }

// errorWithMessages writes the §2.5 {"error", "messages"} 422 shape shared
// by push registrations and alarms.
func errorWithMessages(w http.ResponseWriter, logger *slog.Logger, summary string, msgs []string) {
	writeJSON(w, logger, http.StatusUnprocessableEntity, map[string]any{
		"error": summary, "messages": msgs,
	})
}

func (h *pushRegsHandler) register(w http.ResponseWriter, r *http.Request) {
	region, ok := resolveRegion(w, r, h.deps)
	if !ok {
		return
	}
	p, err := parseRequestParams(w, r, requestBodyLimit)
	if err != nil {
		errorWithMessages(w, h.deps.Logger, "Unable to register device", []string{err.Error()})
		return
	}

	var msgs []string
	token, _ := p.str("token")
	switch {
	case token == "":
		msgs = append(msgs, "Token can't be blank")
	case len(token) > maxTokenLen:
		msgs = append(msgs, fmt.Sprintf("Token is too long (maximum is %d characters)", maxTokenLen))
	}
	os, osPresent := p.str("operating_system")
	switch {
	case !osPresent || os == "":
		msgs = append(msgs, "Operating system can't be blank")
	case os != pushreg.OSIOS && os != pushreg.OSAndroid:
		msgs = append(msgs, "Operating system is not included in the list")
	}
	if len(msgs) > 0 {
		// Doomed requests must not cost a store read: the merged-row Get
		// below is only worth anything for a request that can still 204,
		// and this endpoint is unauthenticated (spec §2.6).
		errorWithMessages(w, h.deps.Logger, "Unable to register device", msgs)
		return
	}

	up := pushreg.Upsert{RegionID: region.ID, Token: token, OperatingSystem: os}
	if os == pushreg.OSIOS {
		up.APNSSandbox = parseAPNSSandbox(p, h.deps.Logger)
	}
	// Sticky fields (spec §4): only an actual value overwrites; a blank
	// value on a routine launch-time re-POST keeps the stored one. The
	// test-device invariants ("a test device must be traceable to a human",
	// "cleared for non-test devices") hold on the *merged* row -- a re-POST
	// of test_device=true without a description keeps the stored
	// description rather than 422ing -- so the stored row is read first.
	// The read-then-upsert race is benign: both writers carry full values.
	stored, err := h.deps.PushRegs.Get(r.Context(), region.ID, token)
	if err != nil && !errors.Is(err, pushreg.ErrNotFound) {
		writeServerError(w, h.deps.Logger, region.ID, "get push registration", sanitizeToken(err, token))
		return
	}
	if locale, ok := p.str("locale"); ok && locale != "" {
		up.Locale = &locale
	}
	testDevice, testDevicePresent := p.boolish("test_device")
	desc, descPresent := p.str("description")
	if desc == "" {
		descPresent = false // blank counts as absent, like locale
	}
	effectiveTest := stored.TestDevice
	if testDevicePresent {
		effectiveTest = testDevice
	}
	switch {
	case effectiveTest:
		merged := stored.Description
		if descPresent {
			merged = desc
		}
		switch {
		case merged == "":
			msgs = append(msgs, "Description can't be blank")
		case len(merged) > maxDescriptionLen:
			msgs = append(msgs, fmt.Sprintf("Description is too long (maximum is %d characters)", maxDescriptionLen))
		default:
			if testDevicePresent {
				up.TestDevice = &testDevice
			}
			if descPresent {
				up.Description = &desc
			}
		}
	case testDevicePresent:
		// An explicit false demotes and clears (spec §4).
		cleared := ""
		up.TestDevice = &testDevice
		up.Description = &cleared
	default:
		// Non-test row, no test_device in the request: a stray description
		// is deliberately ignored -- non-test rows carry none.
	}
	if len(msgs) > 0 {
		errorWithMessages(w, h.deps.Logger, "Unable to register device", msgs)
		return
	}

	if err := h.deps.PushRegs.Upsert(r.Context(), up, h.deps.Now()); err != nil {
		writeServerError(w, h.deps.Logger, region.ID, "upsert push registration", sanitizeToken(err, token))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// sanitizeToken makes a store error safe to log for a request that carried
// token: any occurrence of the token value in the error text is replaced.
// Driver errors do not normally echo bound values, but "normally" is not a
// guarantee the repo's no-token-logging rule can rest on.
func sanitizeToken(err error, token string) error {
	if err == nil || token == "" || !strings.Contains(err.Error(), token) {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), token, "[token]"))
}

func (h *pushRegsHandler) unregister(w http.ResponseWriter, r *http.Request) {
	region, ok := resolveRegion(w, r, h.deps)
	if !ok {
		return
	}
	p, err := parseRequestParams(w, r, requestBodyLimit)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	token, _ := p.str("token")
	if token == "" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if err := h.deps.PushRegs.Delete(r.Context(), region.ID, token); err != nil {
		if errors.Is(err, pushreg.ErrNotFound) {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		// A failed delete must never masquerade as a 204 (spec §2.5).
		writeServerError(w, h.deps.Logger, region.ID, "delete push registration", sanitizeToken(err, token))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// clientIP is the throttle key: the connection's remote host. Behind a
// reverse proxy every request shares the proxy's address, so deployments
// must preserve client addresses at the proxy layer (see README); trusting
// X-Forwarded-For here would let any client spoof its own bucket.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// throttleByIP applies the shared path-scoped bucket (spec §2.6: DELETEs
// share the POST bucket). Denials are an empty 429.
func throttleByIP(l *ratelimit.Limiter, deps Deps, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !l.Allow(clientIP(r), deps.Now()) {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		next(w, r)
	}
}
