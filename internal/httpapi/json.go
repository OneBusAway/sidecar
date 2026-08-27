package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/OneBusAway/sidecar/internal/regions"
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

// writeCSVHeaders sets the response headers every CSV export shares.
// nosniff stops a browser from re-interpreting the body as HTML; no-store
// keeps rider data out of intermediary caches; the filename is fixed and
// server-generated, never derived from a name, so nothing rider- or
// author-supplied reaches a Content-Disposition header.
func writeCSVHeaders(w http.ResponseWriter, filename string) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
}

// writeRegionNotFound writes the exact 404 contract for an unrecognised
// region (design spec §1.2, §2.5). Every feed handler that takes a
// {regionId} path segment shares this one function rather than each
// defining its own copy: two independent copies is how one of them quietly
// drops the Content-Type header and nothing notices.
func writeRegionNotFound(w http.ResponseWriter, logger *slog.Logger) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	if _, err := w.Write([]byte(notFoundBody)); err != nil {
		logger.Warn("httpapi: write 404 body", "err", err)
	}
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
		// http.MaxBytesReader's error wraps a message written for a Go
		// developer ("http: request body too large"), not for whoever hit
		// this endpoint -- every other 4xx on this API is copy written for an
		// operator, and this is the one place that was leaking Go internals
		// instead.
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return errBodyTooLarge
		}
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	return nil
}

// decodeJSONStrict is decodeJSON with DisallowUnknownFields. It exists for
// the survey authoring document, where the CLI has always been strict for a
// concrete reason: a misspelled "show_on_maps" would decode as absent and
// silently ship a hidden survey. Everywhere else this API is deliberately
// lenient, so a newer SPA sending a field this server has not learned about
// yet is not rejected outright -- do not "unify" the two.
func decodeJSONStrict(w http.ResponseWriter, r *http.Request, maxBytes int64, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return errBodyTooLarge
		}
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	return nil
}

// writeServerError logs op against the region id and writes a bare, empty
// 500. Every {regionId} feed handler shares this one function for the same
// reason they share writeRegionNotFound: three independent copies is how one
// of them quietly grows a body, or logs under a different key, and nothing
// notices. Note this is deliberately NOT serverErrorJSON, which writes an
// {"error": ...} body -- a different wire contract used by the admin API.
func writeServerError(w http.ResponseWriter, logger *slog.Logger, regionID int64, op string, err error) {
	logger.Error("httpapi: "+op, "region_id", regionID, "err", err)
	w.WriteHeader(http.StatusInternalServerError)
}

// resolveRegion parses the {regionId} path segment and loads that region,
// writing the response itself on failure and reporting ok=false. It is the
// shared preamble of every rider-facing endpoint scoped to a region: an
// unparseable segment and an unknown id are both the 404 contract (design
// spec §1.2), while a store failure is a 500 the rider never sees the detail
// of. Callers differ only in what they do after a region is in hand.
func resolveRegion(w http.ResponseWriter, r *http.Request, deps Deps) (regions.Region, bool) {
	id, parsed := ParseRegionSegment(r.PathValue("regionId"))
	if !parsed {
		writeRegionNotFound(w, deps.Logger)
		return regions.Region{}, false
	}

	region, err := deps.Regions.Get(r.Context(), id)
	if err != nil {
		if errors.Is(err, regions.ErrNotFound) {
			writeRegionNotFound(w, deps.Logger)
			return regions.Region{}, false
		}
		writeServerError(w, deps.Logger, id, "get region", err)
		return regions.Region{}, false
	}
	return region, true
}
