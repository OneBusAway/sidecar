package httpapi

import (
	"log/slog"
	"net/http"
	"strings"

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

func (h *surveysHandler) create(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNotImplemented) // Task 8
}

func (h *surveysHandler) amend(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNotImplemented) // Task 8
}
