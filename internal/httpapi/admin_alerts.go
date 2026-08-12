package httpapi

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/OneBusAway/sidecar/internal/alerts"
	"github.com/OneBusAway/sidecar/internal/regions"
)

// maxAdminBody caps admin JSON request bodies at 64 KB. Alert text is prose,
// not payloads; anything larger is either a mistake or an attempt to make the
// server buffer for free.
const maxAdminBody = 64 * 1024

// alertJSON is the response shape for a single alert (design spec §5).
//
// Translations are grouped per language here even though storage is per
// field: an author edits "the Spanish version" of an alert, not "the Spanish
// header row" and "the Spanish description row".
//
// Translations are populated by the single-alert endpoints. The list endpoint
// leaves the array empty -- §5 gives translations to `GET /alerts/{id}`, and
// the repository has no bulk query for the authoring view, so filling them in
// would mean one extra round trip per listed alert for data the list screen
// does not show.
type alertJSON struct {
	ID           int64             `json:"id"`
	RegionID     int64             `json:"region_id"`
	AgencyID     string            `json:"agency_id"`
	Header       string            `json:"header"`
	Description  string            `json:"description"`
	URL          string            `json:"url"`
	Cause        string            `json:"cause"`
	Effect       string            `json:"effect"`
	Severity     string            `json:"severity"`
	StartTime    string            `json:"start_time"`
	EndTime      *string           `json:"end_time"`
	Published    bool              `json:"published"`
	IsTest       bool              `json:"is_test"`
	CreatedAt    string            `json:"created_at"`
	UpdatedAt    string            `json:"updated_at"`
	Translations []translationJSON `json:"translations"`
}

// translationJSON is one language's rendering of an alert. A nil Header or
// Description means that field has no translation, which is different from a
// translation whose text is empty.
type translationJSON struct {
	Language    string  `json:"language"`
	Header      *string `json:"header"`
	Description *string `json:"description"`
}

// createAlertRequest is the POST /alerts body.
type createAlertRequest struct {
	// RegionID is a pointer because region 0 is a real region (Tampa Bay):
	// an absent region_id must be an error, not a silent write to region 0.
	RegionID    *int64  `json:"region_id"`
	AgencyID    string  `json:"agency_id"`
	Header      string  `json:"header"`
	Description string  `json:"description"`
	URL         string  `json:"url"`
	Cause       string  `json:"cause"`
	Effect      string  `json:"effect"`
	Severity    string  `json:"severity"`
	StartTime   string  `json:"start_time"`
	EndTime     *string `json:"end_time"`
	IsTest      bool    `json:"is_test"`
}

// patchAlertRequest is the PATCH /alerts/{id} body. Every field is a pointer
// so that "absent" and "set to the zero value" stay distinguishable, which is
// what makes this a patch rather than a replace.
type patchAlertRequest struct {
	AgencyID    *string `json:"agency_id"`
	Header      *string `json:"header"`
	Description *string `json:"description"`
	URL         *string `json:"url"`
	Cause       *string `json:"cause"`
	Effect      *string `json:"effect"`
	Severity    *string `json:"severity"`
	StartTime   *string `json:"start_time"`
	EndTime     *string `json:"end_time"`
	// ClearEndTime reverts to the feed's default fallback duration. JSON
	// cannot distinguish an explicit null from an absent field, so clearing
	// needs a flag of its own; it is the CLI's --no-end.
	ClearEndTime bool  `json:"clear_end_time"`
	IsTest       *bool `json:"is_test"`
}

// translationRequest is the PUT /alerts/{id}/translations/{lang} body. Both
// fields are pointers: an absent field leaves that field's translation alone,
// while an empty string is a real (if empty) translation.
type translationRequest struct {
	Header      *string `json:"header"`
	Description *string `json:"description"`
}

// adminAlertsHandler serves the authenticated alert-authoring endpoints. It is
// separate from alertsHandler, which serves the unauthenticated rider feed.
type adminAlertsHandler struct {
	deps Deps
}

// list handles GET /api/admin/v1/alerts.
//
// `region` is a filter, not a resource lookup: absent means every region, and
// an unknown id is an empty list rather than a 404. Absent cannot be spelled
// as region 0, because region 0 is Tampa Bay.
func (h *adminAlertsHandler) list(w http.ResponseWriter, r *http.Request) {
	var filter alerts.ListFilter
	if raw := r.URL.Query()["region"]; len(raw) > 0 {
		id, err := strconv.ParseInt(raw[0], 10, 64)
		if err != nil {
			writeJSONError(w, h.deps.Logger, http.StatusBadRequest,
				fmt.Sprintf("invalid region %q: must be an integer", raw[0]))
			return
		}
		filter.RegionID = &id
	}

	list, err := h.deps.Alerts.List(r.Context(), filter)
	if err != nil {
		h.storeError(w, "list alerts", err)
		return
	}

	// Built with make so an empty result marshals as [] rather than null; a
	// nil slice would make every caller special-case the empty case.
	out := make([]alertJSON, 0, len(list))
	for _, a := range list {
		out = append(out, toAlertJSON(a))
	}
	writeJSON(w, h.deps.Logger, http.StatusOK, out)
}

// get handles GET /api/admin/v1/alerts/{id}.
func (h *adminAlertsHandler) get(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeJSONError(w, h.deps.Logger, http.StatusBadRequest, err.Error())
		return
	}
	a, err := h.deps.Alerts.Get(r.Context(), id)
	if err != nil {
		h.storeError(w, "get alert", err)
		return
	}
	writeJSON(w, h.deps.Logger, http.StatusOK, toAlertJSON(a))
}

// create handles POST /api/admin/v1/alerts.
//
// Everything that can be checked is checked before the write -- region,
// timestamps, window, resolved agency id, enums -- so a rejected create never
// leaves a partial row behind and the client gets a message naming the actual
// problem. The repository re-validates as a backstop; see storeError for why a
// backstop failure is a 500 rather than a 400.
func (h *adminAlertsHandler) create(w http.ResponseWriter, r *http.Request) {
	var req createAlertRequest
	if err := decodeJSON(w, r, maxAdminBody, &req); err != nil {
		writeJSONError(w, h.deps.Logger, http.StatusBadRequest, err.Error())
		return
	}
	if req.RegionID == nil {
		writeJSONError(w, h.deps.Logger, http.StatusBadRequest,
			"region_id is required (region 0 is a real region, so there is no default)")
		return
	}
	if req.Header == "" {
		writeJSONError(w, h.deps.Logger, http.StatusBadRequest, "header is required")
		return
	}

	ctx := r.Context()
	region, err := h.deps.Regions.Get(ctx, *req.RegionID)
	if err != nil {
		writeRegionError(w, h.deps.Logger, "get region", *req.RegionID, err)
		return
	}

	startTime, err := parseInstantJSON(req.StartTime, region)
	if err != nil {
		writeJSONError(w, h.deps.Logger, http.StatusBadRequest, err.Error())
		return
	}
	var endTime *time.Time
	if req.EndTime != nil {
		end, endErr := parseInstantJSON(*req.EndTime, region)
		if endErr != nil {
			writeJSONError(w, h.deps.Logger, http.StatusBadRequest, endErr.Error())
			return
		}
		endTime = &end
	}
	if winErr := alerts.ValidateWindow(startTime, endTime, h.deps.Now()); winErr != nil {
		writeJSONError(w, h.deps.Logger, http.StatusBadRequest, winErr.Error())
		return
	}

	// agency_id resolves at author time exactly as the CLI's `alert create`
	// does: the resolved value is what gets stored, so a published alert never
	// changes agency underneath a later directory sync or region edit.
	agencyID := req.AgencyID
	if agencyID == "" {
		agencyID = region.DefaultAgencyID
	}
	if agencyID == "" {
		writeJSONError(w, h.deps.Logger, http.StatusBadRequest, fmt.Sprintf(
			"no agency_id given and region %d has no default agency id; "+
				"set one with PATCH /api/admin/v1/regions/%d or pass agency_id",
			region.ID, region.ID))
		return
	}

	cause, err := alerts.ParseCause(req.Cause)
	if err != nil {
		writeJSONError(w, h.deps.Logger, http.StatusBadRequest, err.Error())
		return
	}
	effect, err := alerts.ParseEffect(req.Effect)
	if err != nil {
		writeJSONError(w, h.deps.Logger, http.StatusBadRequest, err.Error())
		return
	}
	severity, err := alerts.ParseSeverity(req.Severity)
	if err != nil {
		writeJSONError(w, h.deps.Logger, http.StatusBadRequest, err.Error())
		return
	}

	created, err := h.deps.Alerts.Create(ctx, alerts.NewAlert{
		RegionID:        region.ID,
		AgencyID:        agencyID,
		HeaderText:      req.Header,
		DescriptionText: req.Description,
		URL:             req.URL,
		Cause:           cause,
		Effect:          effect,
		Severity:        severity,
		StartTime:       startTime,
		EndTime:         endTime,
		IsTest:          req.IsTest,
	}, h.deps.Now())
	if err != nil {
		h.storeError(w, "create alert", err)
		return
	}

	w.Header().Set("Location", fmt.Sprintf("/api/admin/v1/alerts/%d", created.ID))
	writeJSON(w, h.deps.Logger, http.StatusCreated, toAlertJSON(created))
}

// patch handles PATCH /api/admin/v1/alerts/{id}, mapping the request 1:1 onto
// alerts.Patch.
//
// The window is validated against the *merged* view -- the stored row with the
// patch applied -- so editing only the end time is still checked against the
// alert's existing start, and vice versa.
func (h *adminAlertsHandler) patch(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeJSONError(w, h.deps.Logger, http.StatusBadRequest, err.Error())
		return
	}
	var req patchAlertRequest
	if decodeErr := decodeJSON(w, r, maxAdminBody, &req); decodeErr != nil {
		writeJSONError(w, h.deps.Logger, http.StatusBadRequest, decodeErr.Error())
		return
	}
	if req.EndTime != nil && req.ClearEndTime {
		writeJSONError(w, h.deps.Logger, http.StatusBadRequest,
			"send only one of end_time and clear_end_time")
		return
	}

	ctx := r.Context()
	current, err := h.deps.Alerts.Get(ctx, id)
	if err != nil {
		h.storeError(w, "get alert", err)
		return
	}

	patch := alerts.Patch{
		DescriptionText: req.Description,
		URL:             req.URL,
		ClearEndTime:    req.ClearEndTime,
		IsTest:          req.IsTest,
	}

	if req.Header != nil {
		// The column is NOT NULL but not non-empty: an empty header stores
		// fine and then ships a header-less alert to every rider reading the
		// feed. Same reasoning as the agency_id check just below.
		if *req.Header == "" {
			writeJSONError(w, h.deps.Logger, http.StatusBadRequest, "header must not be empty")
			return
		}
		patch.HeaderText = req.Header
	}

	if req.AgencyID != nil {
		// The column is NOT NULL but not non-empty: an empty agency id stores
		// fine and then produces an informed_entity no OBA app matches.
		if *req.AgencyID == "" {
			writeJSONError(w, h.deps.Logger, http.StatusBadRequest, "agency_id must not be empty")
			return
		}
		patch.AgencyID = req.AgencyID
	}

	if req.StartTime != nil || req.EndTime != nil {
		// Only needed for the timestamp error message, so it is only fetched
		// when a timestamp is actually being parsed.
		region, regErr := h.deps.Regions.Get(ctx, current.RegionID)
		if regErr != nil {
			writeRegionError(w, h.deps.Logger, "get region", current.RegionID, regErr)
			return
		}
		if req.StartTime != nil {
			start, startErr := parseInstantJSON(*req.StartTime, region)
			if startErr != nil {
				writeJSONError(w, h.deps.Logger, http.StatusBadRequest, startErr.Error())
				return
			}
			patch.StartTime = &start
		}
		if req.EndTime != nil {
			end, endErr := parseInstantJSON(*req.EndTime, region)
			if endErr != nil {
				writeJSONError(w, h.deps.Logger, http.StatusBadRequest, endErr.Error())
				return
			}
			patch.EndTime = &end
		}
	}

	for _, e := range []struct {
		in    *string
		out   **string
		parse func(string) (string, error)
	}{
		{req.Cause, &patch.Cause, alerts.ParseCause},
		{req.Effect, &patch.Effect, alerts.ParseEffect},
		{req.Severity, &patch.Severity, alerts.ParseSeverity},
	} {
		if e.in == nil {
			continue
		}
		name, parseErr := e.parse(*e.in)
		if parseErr != nil {
			writeJSONError(w, h.deps.Logger, http.StatusBadRequest, parseErr.Error())
			return
		}
		*e.out = &name
	}

	effectiveStart := current.StartTime
	if patch.StartTime != nil {
		effectiveStart = *patch.StartTime
	}
	effectiveEnd := current.EndTime
	switch {
	case patch.ClearEndTime:
		effectiveEnd = nil
	case patch.EndTime != nil:
		effectiveEnd = patch.EndTime
	}
	if winErr := alerts.ValidateWindow(effectiveStart, effectiveEnd, h.deps.Now()); winErr != nil {
		writeJSONError(w, h.deps.Logger, http.StatusBadRequest, winErr.Error())
		return
	}

	if _, updateErr := h.deps.Alerts.Update(ctx, id, patch, h.deps.Now()); updateErr != nil {
		h.storeError(w, "update alert", updateErr)
		return
	}
	h.respondWithAlert(w, r, id, "get updated alert")
}

// delete handles DELETE /api/admin/v1/alerts/{id}.
func (h *adminAlertsHandler) delete(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeJSONError(w, h.deps.Logger, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.deps.Alerts.Delete(r.Context(), id); err != nil {
		h.storeError(w, "delete alert", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// setPublished builds the handler for POST /alerts/{id}/publish and its
// unpublish twin.
func (h *adminAlertsHandler) setPublished(published bool) http.HandlerFunc {
	op := "publish alert"
	if !published {
		op = "unpublish alert"
	}
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := pathID(r)
		if err != nil {
			writeJSONError(w, h.deps.Logger, http.StatusBadRequest, err.Error())
			return
		}
		if err := h.deps.Alerts.SetPublished(r.Context(), id, published, h.deps.Now()); err != nil {
			h.storeError(w, op, err)
			return
		}
		h.respondWithAlert(w, r, id, op)
	}
}

// putTranslation handles PUT /api/admin/v1/alerts/{id}/translations/{lang}.
//
// Each provided field is stored with the SHA-256 of the alert's *current*
// English text for that field. Editing the English afterwards changes its
// hash, which marks the translation stale and makes the feed withhold it --
// riders read accurate English rather than outdated translated text.
func (h *adminAlertsHandler) putTranslation(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeJSONError(w, h.deps.Logger, http.StatusBadRequest, err.Error())
		return
	}
	language, err := pathLanguage(r)
	if err != nil {
		writeJSONError(w, h.deps.Logger, http.StatusBadRequest, err.Error())
		return
	}

	var req translationRequest
	if decodeErr := decodeJSON(w, r, maxAdminBody, &req); decodeErr != nil {
		writeJSONError(w, h.deps.Logger, http.StatusBadRequest, decodeErr.Error())
		return
	}
	if req.Header == nil && req.Description == nil {
		writeJSONError(w, h.deps.Logger, http.StatusBadRequest, "provide header and/or description")
		return
	}

	ctx := r.Context()
	current, err := h.deps.Alerts.Get(ctx, id)
	if err != nil {
		h.storeError(w, "get alert", err)
		return
	}

	for _, t := range []struct {
		text   *string
		field  alerts.Field
		source string
	}{
		{req.Header, alerts.FieldHeader, current.HeaderText},
		{req.Description, alerts.FieldDescription, current.DescriptionText},
	} {
		if t.text == nil {
			continue
		}
		upsertErr := h.deps.Alerts.UpsertTranslation(ctx, id, alerts.Translation{
			Language:     language,
			Field:        t.field,
			Text:         *t.text,
			SourceSHA256: alerts.SourceHash(t.source),
		}, h.deps.Now())
		if upsertErr != nil {
			h.storeError(w, "upsert translation", upsertErr)
			return
		}
	}

	h.respondWithAlert(w, r, id, "get translated alert")
}

// deleteTranslation handles DELETE /api/admin/v1/alerts/{id}/translations/{lang},
// removing every field row for that language.
func (h *adminAlertsHandler) deleteTranslation(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		writeJSONError(w, h.deps.Logger, http.StatusBadRequest, err.Error())
		return
	}
	language, err := pathLanguage(r)
	if err != nil {
		writeJSONError(w, h.deps.Logger, http.StatusBadRequest, err.Error())
		return
	}
	if delErr := h.deps.Alerts.DeleteTranslation(r.Context(), id, language); delErr != nil {
		h.storeError(w, "delete translation", delErr)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// respondWithAlert re-reads an alert and writes it as the 200 body. Update and
// SetPublished do not return translations, so every mutation that answers with
// the whole alert reads it back rather than reporting a translation-less
// version the SPA would render as "all translations gone".
func (h *adminAlertsHandler) respondWithAlert(w http.ResponseWriter, r *http.Request, id int64, op string) {
	a, err := h.deps.Alerts.Get(r.Context(), id)
	if err != nil {
		h.storeError(w, op, err)
		return
	}
	writeJSON(w, h.deps.Logger, http.StatusOK, toAlertJSON(a))
}

// storeError maps a repository error onto a response.
//
// Handlers pre-validate everything they can before calling the repository, so
// a validation error arriving here is a bug in this package, not a client
// mistake: reporting it as a 400 would tell the operator their request was
// wrong and hide the defect. Only the not-found sentinels are 4xx; everything
// else is a logged 500 with a fixed body.
func (h *adminAlertsHandler) storeError(w http.ResponseWriter, op string, err error) {
	writeStoreError(w, h.deps.Logger, op, err)
}

// writeStoreError is storeError's implementation, shared with the regions
// handler.
func writeStoreError(w http.ResponseWriter, logger *slog.Logger, op string, err error) {
	switch {
	case errors.Is(err, alerts.ErrNotFound):
		writeJSONError(w, logger, http.StatusNotFound, "alert not found")
	case errors.Is(err, regions.ErrNotFound):
		writeJSONError(w, logger, http.StatusNotFound, "region not found")
	default:
		serverErrorJSON(w, logger, op, err)
	}
}

// writeRegionError is writeStoreError for the region lookups that know which
// id was asked for -- the ones where the client typed the id and the message
// is more useful for repeating it back.
func writeRegionError(w http.ResponseWriter, logger *slog.Logger, op string, id int64, err error) {
	if errors.Is(err, regions.ErrNotFound) {
		writeJSONError(w, logger, http.StatusNotFound, fmt.Sprintf("region %d not found", id))
		return
	}
	serverErrorJSON(w, logger, op, err)
}

// toAlertJSON renders a stored alert for the API: timestamps as RFC 3339 UTC,
// translations regrouped from per-field rows into one entry per language.
func toAlertJSON(a alerts.Alert) alertJSON {
	out := alertJSON{
		ID:           a.ID,
		RegionID:     a.RegionID,
		AgencyID:     a.AgencyID,
		Header:       a.HeaderText,
		Description:  a.DescriptionText,
		URL:          a.URL,
		Cause:        a.Cause,
		Effect:       a.Effect,
		Severity:     a.Severity,
		StartTime:    formatInstant(a.StartTime),
		Published:    a.Published,
		IsTest:       a.IsTest,
		CreatedAt:    formatInstant(a.CreatedAt),
		UpdatedAt:    formatInstant(a.UpdatedAt),
		Translations: groupTranslations(a.Translations),
	}
	if a.EndTime != nil {
		end := formatInstant(*a.EndTime)
		out.EndTime = &end
	}
	return out
}

// groupTranslations collapses per-field translation rows into one entry per
// language, sorted by language so the response is stable.
func groupTranslations(in []alerts.Translation) []translationJSON {
	byLanguage := make(map[string]*translationJSON, len(in))
	for _, t := range in {
		entry, ok := byLanguage[t.Language]
		if !ok {
			entry = &translationJSON{Language: t.Language}
			byLanguage[t.Language] = entry
		}
		text := t.Text
		switch t.Field {
		case alerts.FieldHeader:
			entry.Header = &text
		case alerts.FieldDescription:
			entry.Description = &text
		}
	}

	// make, not nil: the field is always an array in the response.
	out := make([]translationJSON, 0, len(byLanguage))
	for _, entry := range byLanguage {
		out = append(out, *entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Language < out[j].Language })
	return out
}

// formatInstant renders a stored instant as RFC 3339 UTC (design spec §5).
func formatInstant(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// parseInstantJSON requires an explicit UTC offset, exactly as the CLI's
// parseInstant does. A naive datetime is rejected rather than guessed at:
// interpreting it in the server's local zone would place an alert hours from
// where the author meant, and the regions directory carries no timezone to
// fall back on. The error names the region's configured zone so the author
// knows which offset to write.
//
// This duplicates two lines of the CLI's logic because that copy lives in
// cmd/sidecar-admin and cannot be imported; each side's error copy is written
// for its own audience anyway.
func parseInstantJSON(s string, region regions.Region) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		// An empty Timezone would render as "region 16 is configured as :",
		// which reads like a truncated sentence rather than the fact it is.
		// PATCH /regions refuses to store an empty zone, so this only comes up
		// for a row written before that check existed -- rare enough to be
		// confusing, which is exactly when the message has to carry itself.
		zone := fmt.Sprintf("region %d is configured as %s", region.ID, region.Timezone)
		if region.Timezone == "" {
			zone = fmt.Sprintf("region %d has no configured timezone", region.ID)
		}
		return time.Time{}, fmt.Errorf(
			"%q must be RFC 3339 with an explicit offset (e.g. 2026-08-15T14:00:00-07:00); "+
				"%s: %w", s, zone, err)
	}
	return t.UTC(), nil
}

// pathLanguage parses and normalizes the {lang} path wildcard.
//
// A tag that normalizes to nothing -- ".../translations/%20" is the only way
// to spell it, since {lang} cannot match an empty path segment -- is rejected
// rather than passed on. An empty language would otherwise store a translation
// row with no language tag at all, which the feed would then emit as an
// unlabelled alternative to the English text. Both translation handlers use
// this, so identical input gets an identical answer from each: the alternative
// (PUT rejecting it, DELETE reporting 404 because it matched no rows) is a
// difference with no meaning behind it.
func pathLanguage(r *http.Request) (string, error) {
	raw := r.PathValue("lang")
	language := alerts.NormalizeLanguage(raw)
	if language == "" {
		return "", fmt.Errorf("invalid language %q: must be a BCP-47 tag such as es", raw)
	}
	return language, nil
}

// pathID parses the {id} path wildcard, returning a caller-safe message the
// HTTP layer maps to 400.
func pathID(r *http.Request) (int64, error) {
	raw := r.PathValue("id")
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid id %q: must be an integer", raw)
	}
	return v, nil
}
