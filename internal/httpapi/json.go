package httpapi

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
)

// writeJSON writes v as a JSON body with the given status. Encoding happens
// after the status line is committed, so a failure here can only be logged --
// there is no way left to turn it into a different status code.
func writeJSON(w http.ResponseWriter, logger *slog.Logger, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		logger.Warn("httpapi: encode json response", "err", err)
	}
}

// writeJSONError writes the {"error": "..."} shape every admin endpoint uses
// for failures (design spec §5).
func writeJSONError(w http.ResponseWriter, logger *slog.Logger, status int, msg string) {
	writeJSON(w, logger, status, map[string]string{"error": msg})
}

// serverErrorJSON logs op with its underlying error and writes a generic 500.
// Store errors are for the operator's log, never the client's screen (design
// spec §5).
func serverErrorJSON(w http.ResponseWriter, logger *slog.Logger, op string, err error) {
	logger.Error("httpapi: "+op, "err", err)
	writeJSONError(w, logger, http.StatusInternalServerError, "internal error")
}

// decodeJSON reads at most maxBytes of the request body into dst. It returns a
// caller-safe error message; the HTTP layer maps any non-nil return to 400.
//
// Unknown fields are deliberately ignored (DisallowUnknownFields is not set):
// a newer SPA sending a field this server has not learned about yet should not
// have its request rejected outright.
func decodeJSON(w http.ResponseWriter, r *http.Request, maxBytes int64, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	return nil
}
