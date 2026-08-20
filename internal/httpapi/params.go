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

type params struct{ m map[string]any }

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
			return params{}, errors.New("request body too large")
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

// parseAPNSSandbox applies the §2.7 allow-list. The two failure directions
// are asymmetric -- a production token misrouted to the sandbox bounces in
// front of a rider -- so anything unrecognized is production, logged rather
// than guessed.
func parseAPNSSandbox(p params, logger *slog.Logger) bool {
	raw, ok := p.str("apns_sandbox")
	if !ok || raw == "" {
		return false
	}
	switch strings.ToLower(raw) {
	case "1", "t", "true", "on":
		return true
	case "0", "f", "false", "off":
		return false
	default:
		logger.Warn("httpapi: unrecognized apns_sandbox value treated as production", "value", raw)
		return false
	}
}
