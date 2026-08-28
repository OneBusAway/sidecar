package httpapi

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/OneBusAway/sidecar/internal/regions"
	"github.com/OneBusAway/sidecar/internal/surveys"
)

// maxSurveyBody caps a survey authoring document at 256 KB. A survey
// document is questions and copy, not payloads: the largest real document in
// this codebase's fixtures is a few KB, so 256 KB is generous headroom for a
// study with dozens of questions while still refusing the "someone pasted a
// data URI into a question" mistake before it reaches storage.
const maxSurveyBody = 256 << 10

// adminStudyJSON is the study response shape (design spec section 2.13).
// Named adminStudyJSON, not studyJSON: that name already belongs to the
// rider feed's shape in surveys.go, and reusing it here would silently
// shadow the feed's field set for anyone reading this package by name alone.
type adminStudyJSON struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
}

// toAdminStudyJSON renders a stored study for the admin API.
//
// Timestamps use surveys.FormatTime, not formatInstant: every other admin
// family (alerts, regions, ...) keeps formatInstant's plain RFC 3339, but
// the whole survey family formats through surveys.FormatTime instead, since
// GET /surveys/{id} returns surveys.DocumentFromSurvey(s) verbatim and PUT
// accepts that same document back -- the two have to round-trip byte for
// byte, and studies keep the same convention so an author sees one date
// format across the whole family rather than two.
func toAdminStudyJSON(s surveys.Study) adminStudyJSON {
	return adminStudyJSON{
		ID: s.ID, Name: s.Name, Description: s.Description,
		CreatedAt: surveys.FormatTime(s.CreatedAt), UpdatedAt: surveys.FormatTime(s.UpdatedAt),
	}
}

// adminSurveySummaryJSON is one entry of GET /surveys: everything a list
// screen needs, without the questions a detail view (GET /surveys/{id})
// carries. ResponseCount is computed per row (CountResponses), matching
// `sidecar-admin survey list`.
type adminSurveySummaryJSON struct {
	ID                      int64    `json:"id"`
	StudyID                 int64    `json:"study_id"`
	Name                    string   `json:"name"`
	Available               bool     `json:"available"`
	StartDate               *string  `json:"start_date"`
	EndDate                 *string  `json:"end_date"`
	ShowOnMap               bool     `json:"show_on_map"`
	ShowOnStops             bool     `json:"show_on_stops"`
	AlwaysVisible           bool     `json:"always_visible"`
	AllowsMultipleResponses bool     `json:"allows_multiple_responses"`
	VisibleStopList         []string `json:"visible_stop_list"`
	VisibleRouteList        []string `json:"visible_route_list"`
	ResponseCount           int64    `json:"response_count"`
	CreatedAt               string   `json:"created_at"`
	UpdatedAt               string   `json:"updated_at"`
}

// toAdminSurveySummaryJSON renders one list entry. See toAdminStudyJSON for
// why timestamps go through surveys.FormatTime rather than formatInstant.
func toAdminSurveySummaryJSON(s surveys.Survey, responseCount int64) adminSurveySummaryJSON {
	out := adminSurveySummaryJSON{
		ID: s.ID, StudyID: s.StudyID, Name: s.Name, Available: s.Available,
		ShowOnMap: s.ShowOnMap, ShowOnStops: s.ShowOnStops, AlwaysVisible: s.AlwaysVisible,
		AllowsMultipleResponses: s.AllowsMultipleResponses,
		VisibleStopList:         s.VisibleStopList, VisibleRouteList: s.VisibleRouteList,
		ResponseCount: responseCount,
		CreatedAt:     surveys.FormatTime(s.CreatedAt), UpdatedAt: surveys.FormatTime(s.UpdatedAt),
	}
	if s.StartTime != nil {
		v := surveys.FormatTime(*s.StartTime)
		out.StartDate = &v
	}
	if s.EndTime != nil {
		v := surveys.FormatTime(*s.EndTime)
		out.EndDate = &v
	}
	return out
}

// createStudyRequest is the POST /studies body.
type createStudyRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// patchStudyRequest is the PATCH /studies/{id} body. Both fields are
// pointers so an absent field leaves the stored value alone, matching every
// other PATCH in this API.
type patchStudyRequest struct {
	Name        *string `json:"name"`
	Description *string `json:"description"`
}

// surveyWriteRequest is the POST and PUT /surveys body: the authoring
// document plus study_id. StudyID is a pointer so "absent" and "0" stay
// distinguishable, and because PUT must REJECT it rather than ignore it --
// design spec section 2.13 gives a survey exactly one study for its whole
// lifetime.
type surveyWriteRequest struct {
	StudyID *int64 `json:"study_id"`
	surveys.Document
}

// rejectServerOwned refuses a write whose body carries one of the
// server-owned keys that round-trip out of GET /surveys/{id}: id, study,
// created_at, updated_at. A client that pipes `show` into `create` (or PUT)
// must be told, not silently obeyed by having the field quietly ignored
// (design spec section 5.2).
func rejectServerOwned(doc surveys.Document) error {
	switch {
	case doc.ID != nil:
		return errors.New("id is server-owned and cannot be set")
	case doc.Study != nil:
		return errors.New("study is server-owned and cannot be set")
	case doc.CreatedAt != "":
		return errors.New("created_at is server-owned and cannot be set")
	case doc.UpdatedAt != "":
		return errors.New("updated_at is server-owned and cannot be set")
	default:
		return nil
	}
}

// adminSurveysHandler serves the authenticated study and survey authoring
// endpoints (design spec section 2.13, section 5.7).
type adminSurveysHandler struct {
	deps Deps
}

// listStudies handles GET /api/admin/v1/regions/{regionId}/studies.
func (h *adminSurveysHandler) listStudies(w http.ResponseWriter, r *http.Request) {
	region, ok := mustRegion(w, r, h.deps)
	if !ok {
		return
	}
	list, err := h.deps.Surveys.ListStudies(r.Context(), region.ID)
	if err != nil {
		writeSurveyStoreError(w, h.deps.Logger, "list studies", err)
		return
	}
	out := make([]adminStudyJSON, 0, len(list))
	for _, s := range list {
		out = append(out, toAdminStudyJSON(s))
	}
	writeJSON(w, h.deps.Logger, http.StatusOK, out)
}

// createStudy handles POST /api/admin/v1/regions/{regionId}/studies.
func (h *adminSurveysHandler) createStudy(w http.ResponseWriter, r *http.Request) {
	region, ok := mustRegion(w, r, h.deps)
	if !ok {
		return
	}
	var req createStudyRequest
	if err := decodeJSON(w, r, maxAdminBody, &req); err != nil {
		writeJSONError(w, h.deps.Logger, http.StatusBadRequest, err.Error())
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		// A blank name is a well-formed body that fails domain validation:
		// 422, not 400 (design spec section 5, "Status codes").
		writeJSONError(w, h.deps.Logger, http.StatusUnprocessableEntity, "name cannot be blank")
		return
	}
	created, err := h.deps.Surveys.CreateStudy(r.Context(), region.ID, name, strings.TrimSpace(req.Description), h.deps.Now())
	if err != nil {
		writeSurveyStoreError(w, h.deps.Logger, "create study", err)
		return
	}
	w.Header().Set("Location", fmt.Sprintf("/api/admin/v1/regions/%d/studies/%d", region.ID, created.ID))
	writeJSON(w, h.deps.Logger, http.StatusCreated, toAdminStudyJSON(created))
}

// getStudy handles GET /api/admin/v1/regions/{regionId}/studies/{id}.
func (h *adminSurveysHandler) getStudy(w http.ResponseWriter, r *http.Request) {
	st, ok := loadStudy(w, r, h.deps)
	if !ok {
		return
	}
	writeJSON(w, h.deps.Logger, http.StatusOK, toAdminStudyJSON(st))
}

// patchStudy handles PATCH /api/admin/v1/regions/{regionId}/studies/{id}.
// It merges onto the loaded study rather than replacing it: an absent field
// keeps its current value, which is what makes this a patch.
func (h *adminSurveysHandler) patchStudy(w http.ResponseWriter, r *http.Request) {
	region, ok := mustRegion(w, r, h.deps)
	if !ok {
		return
	}
	current, ok := loadStudy(w, r, h.deps)
	if !ok {
		return
	}
	var req patchStudyRequest
	if err := decodeJSON(w, r, maxAdminBody, &req); err != nil {
		writeJSONError(w, h.deps.Logger, http.StatusBadRequest, err.Error())
		return
	}
	name := current.Name
	if req.Name != nil {
		name = strings.TrimSpace(*req.Name)
		if name == "" {
			writeJSONError(w, h.deps.Logger, http.StatusUnprocessableEntity, "name cannot be blank")
			return
		}
	}
	description := current.Description
	if req.Description != nil {
		description = strings.TrimSpace(*req.Description)
	}
	updated, err := h.deps.Surveys.UpdateStudy(r.Context(), region.ID, current.ID, name, description, h.deps.Now())
	if err != nil {
		writeSurveyStoreError(w, h.deps.Logger, "update study", err)
		return
	}
	writeJSON(w, h.deps.Logger, http.StatusOK, toAdminStudyJSON(updated))
}

// listSurveys handles GET /api/admin/v1/regions/{regionId}/surveys.
func (h *adminSurveysHandler) listSurveys(w http.ResponseWriter, r *http.Request) {
	region, ok := mustRegion(w, r, h.deps)
	if !ok {
		return
	}
	list, err := h.deps.Surveys.ListSurveys(r.Context(), region.ID)
	if err != nil {
		writeSurveyStoreError(w, h.deps.Logger, "list surveys", err)
		return
	}
	ctx := r.Context()
	out := make([]adminSurveySummaryJSON, 0, len(list))
	for _, s := range list {
		n, countErr := h.deps.Surveys.CountResponses(ctx, s.ID)
		if countErr != nil {
			writeSurveyStoreError(w, h.deps.Logger, "count survey responses", countErr)
			return
		}
		out = append(out, toAdminSurveySummaryJSON(s, n))
	}
	writeJSON(w, h.deps.Logger, http.StatusOK, out)
}

// definitionFromRequest converts an authoring document into a validated
// Definition, reading its dates against the request's region.
//
// The region only ever supplies the timezone named in a parse error --
// parseInstantJSON still requires an explicit UTC offset, so nothing is
// interpreted in the region's zone. It exists so POST and PUT cannot drift
// on how a document's dates are read.
func definitionFromRequest(doc surveys.Document, region regions.Region) (surveys.Definition, error) {
	return surveys.DefinitionFromDocument(doc, func(s string) (time.Time, error) {
		return parseInstantJSON(s, region)
	})
}

// createSurvey handles POST /api/admin/v1/regions/{regionId}/surveys.
//
// The document is decoded strictly (decodeJSONStrict): see json.go for why
// this is the one authoring body in the whole admin API that rejects an
// unknown field rather than ignoring it. study_id resolves through
// CreateSurveyInRegion's own JOIN against the region, so a study_id
// belonging to another region -- or to no study at all -- is ErrNotFound
// decided in SQL, never a loader-then-compare a later refactor could drop.
func (h *adminSurveysHandler) createSurvey(w http.ResponseWriter, r *http.Request) {
	region, ok := mustRegion(w, r, h.deps)
	if !ok {
		return
	}
	var req surveyWriteRequest
	if err := decodeJSONStrict(w, r, maxSurveyBody, &req); err != nil {
		writeJSONError(w, h.deps.Logger, http.StatusBadRequest, err.Error())
		return
	}
	if err := rejectServerOwned(req.Document); err != nil {
		writeJSONError(w, h.deps.Logger, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if req.StudyID == nil {
		writeJSONError(w, h.deps.Logger, http.StatusUnprocessableEntity, "study_id is required")
		return
	}
	def, err := definitionFromRequest(req.Document, region)
	if err != nil {
		writeJSONError(w, h.deps.Logger, http.StatusUnprocessableEntity, err.Error())
		return
	}
	created, err := h.deps.Surveys.CreateSurveyInRegion(r.Context(), region.ID, *req.StudyID, def, h.deps.Now())
	if err != nil {
		if errors.Is(err, surveys.ErrNotFound) {
			writeJSONError(w, h.deps.Logger, http.StatusNotFound, "study not found")
			return
		}
		writeSurveyStoreError(w, h.deps.Logger, "create survey", err)
		return
	}
	w.Header().Set("Location", fmt.Sprintf("/api/admin/v1/regions/%d/surveys/%d", region.ID, created.ID))
	writeJSON(w, h.deps.Logger, http.StatusCreated, surveys.DocumentFromSurvey(created))
}

// getSurvey handles GET /api/admin/v1/regions/{regionId}/surveys/{id},
// writing the exact document PUT accepts back (design spec section 2.13).
func (h *adminSurveysHandler) getSurvey(w http.ResponseWriter, r *http.Request) {
	s, ok := loadSurvey(w, r, h.deps)
	if !ok {
		return
	}
	writeJSON(w, h.deps.Logger, http.StatusOK, surveys.DocumentFromSurvey(s))
}

// putSurvey handles PUT /api/admin/v1/regions/{regionId}/surveys/{id}. A
// present study_id is refused rather than obeyed: PUT edits a survey in
// place and must not move it to a different study (design spec section
// 2.13). surveys.ErrQuestionsFrozen -- the question set changed on a
// survey that already has responses -- is a 409 carrying the sentinel's own
// text, not err.Error(), which would leak the store's "update survey 5 with
// 3 responses" framing.
func (h *adminSurveysHandler) putSurvey(w http.ResponseWriter, r *http.Request) {
	region, ok := mustRegion(w, r, h.deps)
	if !ok {
		return
	}
	current, ok := loadSurvey(w, r, h.deps)
	if !ok {
		return
	}
	var req surveyWriteRequest
	if err := decodeJSONStrict(w, r, maxSurveyBody, &req); err != nil {
		writeJSONError(w, h.deps.Logger, http.StatusBadRequest, err.Error())
		return
	}
	if err := rejectServerOwned(req.Document); err != nil {
		writeJSONError(w, h.deps.Logger, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if req.StudyID != nil {
		writeJSONError(w, h.deps.Logger, http.StatusUnprocessableEntity,
			"study_id cannot be changed; a survey cannot be moved between studies")
		return
	}
	def, err := definitionFromRequest(req.Document, region)
	if err != nil {
		writeJSONError(w, h.deps.Logger, http.StatusUnprocessableEntity, err.Error())
		return
	}
	updated, err := h.deps.Surveys.UpdateSurvey(r.Context(), current.ID, def, h.deps.Now())
	if err != nil {
		writeSurveyStoreError(w, h.deps.Logger, "update survey", err)
		return
	}
	writeJSON(w, h.deps.Logger, http.StatusOK, surveys.DocumentFromSurvey(updated))
}

// deleteSurvey handles DELETE /api/admin/v1/regions/{regionId}/surveys/{id}.
// surveys.ErrHasResponses is a 409 carrying the sentinel's own text: responses
// are retained indefinitely, so a survey that has any cannot be deleted
// (design spec section 2.15).
func (h *adminSurveysHandler) deleteSurvey(w http.ResponseWriter, r *http.Request) {
	s, ok := loadSurvey(w, r, h.deps)
	if !ok {
		return
	}
	if err := h.deps.Surveys.DeleteSurvey(r.Context(), s.ID); err != nil {
		writeSurveyStoreError(w, h.deps.Logger, "delete survey", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeSurveyStoreError maps a survey repository error onto a response. It
// is reached only after a handler's own loader (loadStudy / loadSurvey) has
// already fenced the request by region, so surveys.ErrNotFound arriving
// here is a race -- the row vanished between the load and the write --
// rather than the ordinary tenancy 404 those loaders already answer with a
// noun-specific body ("study not found" / "survey not found").
//
// ErrQuestionsFrozen and ErrHasResponses are reported with the sentinel's
// own .Error() text, never err.Error(): the repository wraps both in the
// failing SQL statement ("sqlite: update survey 5 with 3 responses: ..."),
// which is exactly the internal detail design spec section 5 says a 4xx
// body must not carry.
func writeSurveyStoreError(w http.ResponseWriter, logger *slog.Logger, op string, err error) {
	switch {
	case errors.Is(err, surveys.ErrNotFound):
		writeJSONError(w, logger, http.StatusNotFound, "not found")
	case errors.Is(err, surveys.ErrQuestionsFrozen):
		writeJSONError(w, logger, http.StatusConflict, surveys.ErrQuestionsFrozen.Error())
	case errors.Is(err, surveys.ErrHasResponses):
		writeJSONError(w, logger, http.StatusConflict, surveys.ErrHasResponses.Error())
	default:
		serverErrorJSON(w, logger, op, err)
	}
}
