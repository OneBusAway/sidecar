package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/OneBusAway/sidecar/internal/surveys"
)

// Paging for the admin lists over rider rows (SC-21): alarms, survey
// responses and ghost bus reports. A caller that sends neither parameter
// gets every row, as before, up to the cap; past it the response carries
// truncatedHeader and a nextCursorHeader to resume from. The cap bounds one
// response's memory and time; the CSV exports never truncate, they stream
// in pages of the cap.
const (
	// defaultAdminListCap is the default for Deps.AdminListCap.
	defaultAdminListCap = 10000
	// truncatedHeader is "true" when more rows follow this page.
	truncatedHeader = "X-Sidecar-Truncated"
	// nextCursorHeader is the opaque cursor to pass back as ?cursor= for
	// the next page. Present exactly when truncatedHeader is.
	nextCursorHeader = "X-Sidecar-Next-Cursor"
)

var errBadPaging = errors.New("limit must be a positive integer and cursor a value from " + nextCursorHeader)

// adminListCap is Deps.AdminListCap, defaulted.
func (d Deps) adminListCap() int {
	if d.AdminListCap <= 0 {
		return defaultAdminListCap
	}
	return d.AdminListCap
}

// pageLimit reads ?limit= (1..cap; larger values are clamped to the cap;
// absent is the cap).
func pageLimit(r *http.Request, listCap int) (int, error) {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return listCap, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0, errBadPaging
	}
	return min(n, listCap), nil
}

// idCursor reads ?cursor= as a non-negative row id (alarms, ghost bus
// reports); absent is 0, the start.
func idCursor(r *http.Request) (int64, error) {
	raw := r.URL.Query().Get("cursor")
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		return 0, errBadPaging
	}
	return n, nil
}

// responseCursor reads ?cursor= as "<created_at unix>.<id>" (survey
// responses, which are ordered by creation time then id).
func responseCursor(r *http.Request) (surveys.ResponseCursor, error) {
	raw := r.URL.Query().Get("cursor")
	if raw == "" {
		return surveys.ResponseCursor{}, nil
	}
	created, id, ok := strings.Cut(raw, ".")
	c, err1 := strconv.ParseInt(created, 10, 64)
	n, err2 := strconv.ParseInt(id, 10, 64)
	if !ok || err1 != nil || err2 != nil || c < 0 || n < 0 {
		return surveys.ResponseCursor{}, errBadPaging
	}
	return surveys.ResponseCursor{CreatedAtUnix: c, ID: n}, nil
}

func formatResponseCursor(c surveys.ResponseCursor) string {
	return strconv.FormatInt(c.CreatedAtUnix, 10) + "." + strconv.FormatInt(c.ID, 10)
}

// trimPage cuts a page fetched with limit+1 rows back to limit and, when
// the extra row proved more follow, sets the truncation headers with the
// cursor of the last row kept.
func trimPage[T any](w http.ResponseWriter, rows []T, limit int, cursorOf func(T) string) []T {
	if len(rows) <= limit {
		return rows
	}
	rows = rows[:limit]
	w.Header().Set(truncatedHeader, "true")
	w.Header().Set(nextCursorHeader, cursorOf(rows[len(rows)-1]))
	return rows
}
