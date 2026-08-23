package httpapi

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/OneBusAway/sidecar/internal/securetoken"
	"github.com/OneBusAway/sidecar/internal/surveys"
)

// surveyWritesPerMinute is the shared create+amend bucket (surveys design
// spec §2.9). The apps send one create and at most one amend per survey.
const surveyWritesPerMinute = 60

type surveysHandler struct{ deps Deps }

// writeErrors writes the spec §2.5 {"errors": [...]} shape the survey
// endpoints use -- distinct from errorWithMessages' {"error","messages"}.
func writeErrors(w http.ResponseWriter, logger *slog.Logger, msgs []string) {
	writeJSON(w, logger, http.StatusUnprocessableEntity, map[string]any{"errors": msgs})
}

// Wire shapes. Every key is always present: iOS hard-requires most of them
// and Android reads a null boolean as false (surveys design spec §2.3).
type studyJSON struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

type questionJSON struct {
	ID       int64           `json:"id"`
	Position int64           `json:"position"`
	Required bool            `json:"required"`
	Content  surveys.Content `json:"content"`
}

type surveyJSON struct {
	ID                      int64          `json:"id"`
	Name                    string         `json:"name"`
	StartDate               *string        `json:"start_date"`
	EndDate                 *string        `json:"end_date"`
	CreatedAt               string         `json:"created_at"`
	UpdatedAt               string         `json:"updated_at"`
	ShowOnMap               bool           `json:"show_on_map"`
	ShowOnStops             bool           `json:"show_on_stops"`
	AlwaysVisible           bool           `json:"always_visible"`
	AllowsMultipleResponses bool           `json:"allows_multiple_responses"`
	VisibleStopList         []string       `json:"visible_stop_list"`
	VisibleRouteList        []string       `json:"visible_route_list"`
	Study                   studyJSON      `json:"study"`
	Questions               []questionJSON `json:"questions"`
}

func surveyToJSON(s surveys.Survey) surveyJSON {
	out := surveyJSON{
		ID: s.ID, Name: s.Name,
		CreatedAt: surveys.FormatTime(s.CreatedAt), UpdatedAt: surveys.FormatTime(s.UpdatedAt),
		ShowOnMap: s.ShowOnMap, ShowOnStops: s.ShowOnStops, AlwaysVisible: s.AlwaysVisible,
		AllowsMultipleResponses: s.AllowsMultipleResponses,
		VisibleStopList:         s.VisibleStopList, VisibleRouteList: s.VisibleRouteList, // nil -> null
		Study:     studyJSON{ID: s.Study.ID, Name: s.Study.Name, Description: s.Study.Description},
		Questions: make([]questionJSON, 0, len(s.Questions)),
	}
	if s.StartTime != nil {
		v := surveys.FormatTime(*s.StartTime)
		out.StartDate = &v
	}
	if s.EndTime != nil {
		v := surveys.FormatTime(*s.EndTime)
		out.EndDate = &v
	}
	for _, q := range s.Questions {
		out.Questions = append(out.Questions, questionJSON{ID: q.ID, Position: q.Position, Required: q.Required, Content: q.Content})
	}
	return out
}

// list serves GET /api/v1/regions/{regionId}/surveys[.json] (spec §7.1).
// user_id is required and otherwise unused, as in the reference; the apps
// track completion locally.
func (h *surveysHandler) list(w http.ResponseWriter, r *http.Request) {
	region, ok := resolveRegion(w, r, h.deps)
	if !ok {
		return
	}
	if strings.TrimSpace(r.URL.Query().Get("user_id")) == "" {
		writeErrors(w, h.deps.Logger, []string{"user_id is required"})
		return
	}
	list, err := h.deps.Surveys.ListActiveSurveys(r.Context(), region.ID, h.deps.Now())
	if err != nil {
		writeServerError(w, h.deps.Logger, region.ID, "list surveys", err)
		return
	}
	out := make([]surveyJSON, 0, len(list))
	for _, s := range list {
		out = append(out, surveyToJSON(s))
	}
	writeJSON(w, h.deps.Logger, http.StatusOK, map[string]any{
		"surveys": out,
		"region":  map[string]any{"id": region.ID, "name": region.Name},
	})
}

const (
	surveyNotFoundBody   = "Couldn't find Survey"
	responseNotFoundBody = "Couldn't find SurveyResponse"
)

// parseSurveyParams applies the §4.3 step-1 body rules: the size error is
// the one parser message fit for the wire; anything else (which embeds
// encoding/json internals) is logged and replaced with a fixed message.
func (h *surveysHandler) parseSurveyParams(w http.ResponseWriter, r *http.Request, op string) (params, bool) {
	p, err := parseRequestParams(w, r, requestBodyLimit)
	if err == nil {
		return p, true
	}
	msg := "request body is invalid"
	if err.Error() == "request body too large" {
		msg = err.Error()
	}
	h.deps.Logger.Info("httpapi: rejected "+op, "reason", "unparseable body", "err", err)
	writeErrors(w, h.deps.Logger, []string{msg})
	return params{}, false
}

// coordinate parses an optional stop coordinate: (value, present, valid).
func coordinate(p params, key string, limit float64) (v *float64, present, valid bool) {
	s, ok := p.str(key)
	if !ok || s == "" {
		return nil, false, true
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return nil, true, false
	}
	if math.Abs(f) > limit {
		return &f, true, false
	}
	return &f, true, true
}

func responseBody(resp surveys.Response) map[string]any {
	return map[string]any{"survey_response": map[string]any{
		"id":              resp.PublicID,
		"user_identifier": resp.UserIdentifier,
		"update_path":     "/api/v1/survey_responses/" + resp.PublicID,
	}}
}

// create serves POST /api/v1/survey_responses[/] (spec §7.2). Validation
// collects every applicable message in the spec's order, including the
// responses parse, so a request with two problems reports both (surveys
// design spec §2.7).
func (h *surveysHandler) create(w http.ResponseWriter, r *http.Request) {
	p, ok := h.parseSurveyParams(w, r, "survey response")
	if !ok {
		return
	}
	surveyID, ok := p.int64("survey_id")
	if !ok {
		writeJSONError(w, h.deps.Logger, http.StatusNotFound, surveyNotFoundBody)
		return
	}
	survey, err := h.deps.Surveys.GetSurvey(r.Context(), surveyID)
	if err != nil {
		if errors.Is(err, surveys.ErrNotFound) {
			writeJSONError(w, h.deps.Logger, http.StatusNotFound, surveyNotFoundBody)
			return
		}
		writeServerError(w, h.deps.Logger, 0, fmt.Sprintf("get survey %d for response", surveyID), err)
		return
	}

	var msgs []string
	userID, _ := p.str("user_identifier")
	if userID == "" {
		msgs = append(msgs, "User identifier can't be blank")
	}
	stopID, _ := p.str("stop_identifier")
	lat, latPresent, latValid := coordinate(p, "stop_latitude", 90)
	lon, lonPresent, lonValid := coordinate(p, "stop_longitude", 180)
	// Android sends 0.0/0.0 with no identifier on every submission; only an
	// identifier without coordinates is an error (surveys design spec §2.7).
	if stopID != "" {
		if !latPresent || (lat == nil && !latValid) {
			msgs = append(msgs, "Stop latitude can't be blank")
		}
		if !lonPresent || (lon == nil && !lonValid) {
			msgs = append(msgs, "Stop longitude can't be blank")
		}
	}
	if lat != nil && !latValid {
		msgs = append(msgs, "Stop latitude is invalid")
	}
	if lon != nil && !lonValid {
		msgs = append(msgs, "Stop longitude is invalid")
	}
	raw, hasRaw := p.m["responses"].(string)
	var answers []surveys.Answer
	if !hasRaw {
		msgs = append(msgs, surveys.ErrMalformedAnswers.Error())
	} else if answers, err = surveys.ParseAnswers(raw); err != nil {
		msgs = append(msgs, err.Error())
	}
	if len(msgs) > 0 {
		h.deps.Logger.Info("httpapi: rejected survey response", "survey_id", survey.ID, "messages", len(msgs))
		writeErrors(w, h.deps.Logger, msgs)
		return
	}
	if !latValid || !lonValid {
		lat, lon = nil, nil
	}

	publicID, err := securetoken.New()
	if err != nil {
		writeServerError(w, h.deps.Logger, 0, "mint survey response id", err)
		return
	}
	resp, err := h.deps.Surveys.CreateResponse(r.Context(), surveys.NewResponse{
		SurveyID: survey.ID, PublicID: publicID, UserIdentifier: userID,
		StopIdentifier: stopID, StopLatitude: lat, StopLongitude: lon, Answers: answers,
	}, h.deps.Now())
	if err != nil {
		if errors.Is(err, surveys.ErrNotFound) {
			writeJSONError(w, h.deps.Logger, http.StatusNotFound, surveyNotFoundBody)
			return
		}
		// The store's error may echo rider data; log only a fixed cause
		// (surveys design spec §4.5).
		writeServerError(w, h.deps.Logger, 0, "create survey response", fmt.Errorf("survey %d: store write failed", survey.ID))
		return
	}
	writeJSON(w, h.deps.Logger, http.StatusCreated, responseBody(resp))
}

// amend serves POST|PUT|PATCH /api/v1/survey_responses/{responseId}: the
// same merge for all three verbs (iOS PUTs, Android POSTs, the OpenAPI says
// PATCH). Every parameter but responses is ignored.
func (h *surveysHandler) amend(w http.ResponseWriter, r *http.Request) {
	publicID := r.PathValue("responseId")
	p, ok := h.parseSurveyParams(w, r, "survey response amend")
	if !ok {
		return
	}
	raw, hasRaw := p.m["responses"].(string)
	if !hasRaw {
		writeErrors(w, h.deps.Logger, []string{surveys.ErrMalformedAnswers.Error()})
		return
	}
	answers, err := surveys.ParseAnswers(raw)
	if err != nil {
		h.deps.Logger.Info("httpapi: rejected survey response amend", "messages", 1)
		writeErrors(w, h.deps.Logger, []string{err.Error()})
		return
	}
	resp, err := h.deps.Surveys.AmendResponse(r.Context(), publicID, answers, h.deps.Now())
	if err != nil {
		switch {
		case errors.Is(err, surveys.ErrNotFound):
			writeJSONError(w, h.deps.Logger, http.StatusNotFound, responseNotFoundBody)
		case errors.Is(err, surveys.ErrTooManyAnswers):
			h.deps.Logger.Info("httpapi: rejected survey response amend", "reason", "answer cap")
			writeErrors(w, h.deps.Logger, []string{surveys.ErrTooManyAnswers.Error()})
		default:
			writeServerError(w, h.deps.Logger, 0, "amend survey response", errors.New("store write failed"))
		}
		return
	}
	writeJSON(w, h.deps.Logger, http.StatusOK, responseBody(resp))
}
