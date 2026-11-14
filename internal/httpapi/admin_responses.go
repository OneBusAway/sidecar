package httpapi

import (
	"fmt"
	"net/http"

	"github.com/OneBusAway/sidecar/internal/surveys"
)

// answerJSON is one element of a response's answers array (design spec
// section 2.14).
type answerJSON struct {
	QuestionID    int64  `json:"question_id"`
	QuestionType  string `json:"question_type"`
	QuestionLabel string `json:"question_label"`
	Answer        string `json:"answer"`
}

// adminResponseJSON is one survey response, in full: everything the CSV
// export's long format also carries, so the JSON and CSV routes agree on
// what a response is (design spec section 2.14).
type adminResponseJSON struct {
	ID             int64        `json:"id"`
	SurveyID       int64        `json:"survey_id"`
	PublicID       string       `json:"public_id"`
	UserIdentifier string       `json:"user_identifier"`
	StopIdentifier string       `json:"stop_identifier"`
	StopLatitude   *float64     `json:"stop_latitude"`
	StopLongitude  *float64     `json:"stop_longitude"`
	Answers        []answerJSON `json:"answers"`
	CreatedAt      string       `json:"created_at"`
	UpdatedAt      string       `json:"updated_at"`
}

// toAdminResponseJSON renders a stored response. Timestamps use
// surveys.FormatTime, not formatInstant -- see toAdminStudyJSON in
// admin_surveys.go for why the whole survey family keeps one date format.
func toAdminResponseJSON(r surveys.Response) adminResponseJSON {
	answers := make([]answerJSON, 0, len(r.Answers))
	for _, a := range r.Answers {
		answers = append(answers, answerJSON{
			QuestionID: a.QuestionID, QuestionType: a.QuestionType,
			QuestionLabel: a.QuestionLabel, Answer: a.Answer,
		})
	}
	return adminResponseJSON{
		ID: r.ID, SurveyID: r.SurveyID, PublicID: r.PublicID, UserIdentifier: r.UserIdentifier,
		StopIdentifier: r.StopIdentifier, StopLatitude: r.StopLatitude, StopLongitude: r.StopLongitude,
		Answers:   answers,
		CreatedAt: surveys.FormatTime(r.CreatedAt), UpdatedAt: surveys.FormatTime(r.UpdatedAt),
	}
}

// adminResponsesHandler serves the survey response read endpoints (region
// API keys and admin API design spec section 5.2; CSV format is surveys
// design spec section 2.14): the two views onto one survey's responses
// (JSON list, CSV export) and the single-response lookup by public id.
type adminResponsesHandler struct {
	deps Deps
}

// listResponses handles
// GET /api/admin/v1/regions/{regionId}/surveys/{id}/responses.
//
// loadSurvey runs first and is what makes a foreign survey id a 404 rather
// than an empty array: the survey itself is the tenancy fence here, not a
// per-row filter.
func (h *adminResponsesHandler) listResponses(w http.ResponseWriter, r *http.Request) {
	s, ok := loadSurvey(w, r, h.deps)
	if !ok {
		return
	}
	limit, err := pageLimit(r, h.deps.adminListCap())
	if err != nil {
		writeJSONError(w, h.deps.Logger, http.StatusBadRequest, err.Error())
		return
	}
	after, err := responseCursor(r)
	if err != nil {
		writeJSONError(w, h.deps.Logger, http.StatusBadRequest, err.Error())
		return
	}
	list, err := h.deps.Surveys.ListResponsesPage(r.Context(), s.ID, after, limit+1)
	if err != nil {
		writeSurveyStoreError(w, h.deps.Logger, "list survey responses", err)
		return
	}
	list = trimPage(w, list, limit, func(resp surveys.Response) string {
		return formatResponseCursor(surveys.CursorAfter(resp))
	})
	out := make([]adminResponseJSON, 0, len(list))
	for _, resp := range list {
		out = append(out, toAdminResponseJSON(resp))
	}
	writeJSON(w, h.deps.Logger, http.StatusOK, out)
}

// responsesCSV handles
// GET /api/admin/v1/regions/{regionId}/surveys/{id}/responses.csv.
//
// The filename is fixed and server-generated (writeCSVHeaders) even though
// the survey has a name: a name is author-supplied text, and putting it in
// a Content-Disposition header is exactly the injection this route must
// not reopen.
func (h *adminResponsesHandler) responsesCSV(w http.ResponseWriter, r *http.Request) {
	s, ok := loadSurvey(w, r, h.deps)
	if !ok {
		return
	}
	// The first page is read before any header is written, so a store
	// failure up front is still a clean 500. Later pages stream (SC-21):
	// memory is one page, never the whole survey.
	pageSize := h.deps.adminListCap()
	first, err := h.deps.Surveys.ListResponsesPage(r.Context(), s.ID, surveys.ResponseCursor{}, pageSize)
	if err != nil {
		writeSurveyStoreError(w, h.deps.Logger, "list survey responses", err)
		return
	}
	writeCSVHeaders(w, fmt.Sprintf("survey-%d-responses.csv", s.ID))
	// The status line is already committed by writeCSVHeaders' Content-Type
	// (the first Write below commits it if nothing already has); a write
	// failure past that point can only be logged, never turned into a
	// different status code.
	err = surveys.StreamResponsesCSV(w, s, func(yield func([]surveys.Response) error) error {
		page := first
		for {
			if yieldErr := yield(page); yieldErr != nil {
				return yieldErr
			}
			if len(page) < pageSize {
				return nil
			}
			next, pageErr := h.deps.Surveys.ListResponsesPage(r.Context(), s.ID, surveys.CursorAfter(page[len(page)-1]), pageSize)
			if pageErr != nil {
				return pageErr
			}
			page = next
		}
	})
	if err != nil {
		h.deps.Logger.Warn("httpapi: write survey responses csv", "err", err)
	}
}

// getResponse handles
// GET /api/admin/v1/regions/{regionId}/survey_responses/{publicId}.
func (h *adminResponsesHandler) getResponse(w http.ResponseWriter, r *http.Request) {
	resp, ok := loadResponse(w, r, h.deps)
	if !ok {
		return
	}
	writeJSON(w, h.deps.Logger, http.StatusOK, toAdminResponseJSON(resp))
}
