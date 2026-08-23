package httpapi_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/httpapi"
	"github.com/OneBusAway/sidecar/internal/ratelimit"
	"github.com/OneBusAway/sidecar/internal/regions"
	"github.com/OneBusAway/sidecar/internal/store/sqlitetest"
	"github.com/OneBusAway/sidecar/internal/surveys"
)

// surveyDeps builds a router over a real store with the fixed clock. The
// limiter is generous by default; throttle tests pass their own.
func surveyDeps(t *testing.T, limiter *ratelimit.Limiter, logger *slog.Logger) (http.Handler, surveys.Repository, regions.Repository) {
	t.Helper()
	store := sqlitetest.Open(t)
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	deps := httpapi.Deps{
		Surveys:       store.Surveys(),
		SurveyLimiter: limiter,
		Regions:       store.Regions(),
		Now:           func() time.Time { return base },
		Logger:        logger,
	}
	return httpapi.NewRouter(deps), store.Surveys(), store.Regions()
}

func seedSurvey(t *testing.T, repo surveys.Repository, regs regions.Repository, regionID int64, def surveys.Definition) surveys.Survey {
	t.Helper()
	putRegion(t, regs, regionID)
	st, err := repo.CreateStudy(context.Background(), regionID, "World Cup", "", base)
	if err != nil {
		t.Fatal(err)
	}
	if err = def.Validate(); err != nil {
		t.Fatal(err)
	}
	s, err := repo.CreateSurvey(context.Background(), st.ID, def, base)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func fullDefinition() surveys.Definition {
	start, end := base.Add(-time.Hour), base.Add(time.Hour)
	return surveys.Definition{
		Name: "Rider satisfaction", Available: true, StartTime: &start, EndTime: &end,
		ShowOnMap: true, ShowOnStops: true, AlwaysVisible: true, AllowsMultipleResponses: false,
		VisibleStopList: []string{"1_570"},
		Questions: []surveys.QuestionDefinition{
			{Required: true, Content: surveys.Content{Type: "radio", LabelText: "Trip?", Options: []string{"Good", "Bad"}}},
			{Content: surveys.Content{Type: "text", LabelText: "More?"}},
			{Content: surveys.Content{Type: "label", LabelText: "Thanks"}},
			{Content: surveys.Content{Type: "checkbox", LabelText: "Modes", Options: []string{"Bus", "Train"}}},
			{Content: surveys.Content{Type: "external_survey", LabelText: "Go", URL: "https://e.org/s",
				SurveyProvider: "qualtrics", EmbeddedDataFields: []string{"user_id"},
				SDKConfigurationValues: json.RawMessage(`{"a":1}`)}},
		},
	}
}

// iosSurveyList mirrors the iOS app's StudyResponse/Survey/SurveyQuestion
// Codable types: every pointer field here is one iOS hard-requires, so a
// nil after decoding is exactly the failure that would blank the region's
// survey list in the app (design spec 2.3).
type iosSurveyList struct {
	Surveys *[]iosSurvey `json:"surveys"`
	Region  *struct {
		ID   *int64  `json:"id"`
		Name *string `json:"name"`
	} `json:"region"`
}

type iosSurvey struct {
	ID                      *int64          `json:"id"`
	Name                    *string         `json:"name"`
	CreatedAt               *string         `json:"created_at"`
	UpdatedAt               *string         `json:"updated_at"`
	StartDate               json.RawMessage `json:"start_date"`
	EndDate                 json.RawMessage `json:"end_date"`
	ShowOnMap               *bool           `json:"show_on_map"`
	ShowOnStops             *bool           `json:"show_on_stops"`
	AlwaysVisible           *bool           `json:"always_visible"`
	AllowsMultipleResponses *bool           `json:"allows_multiple_responses"`
	VisibleStopList         json.RawMessage `json:"visible_stop_list"`
	VisibleRouteList        json.RawMessage `json:"visible_route_list"`
	Study                   *struct {
		ID          *int64          `json:"id"`
		Name        *string         `json:"name"`
		Description json.RawMessage `json:"description"`
	} `json:"study"`
	Questions *[]struct {
		ID       *int64 `json:"id"`
		Position *int64 `json:"position"`
		Required *bool  `json:"required"`
		Content  *struct {
			Type      *string `json:"type"`
			LabelText *string `json:"label_text"`
		} `json:"content"`
	} `json:"questions"`
}

var wireDateRe = regexp.MustCompile(`^"\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z"$`)

func TestSurveyList_IOSRequiredKeys(t *testing.T) {
	t.Parallel()
	h, repo, regs := surveyDeps(t, nil, nil)
	seedSurvey(t, repo, regs, 1, fullDefinition())

	for _, path := range []string{"/api/v1/regions/1/surveys?user_id=u1", "/api/v1/regions/1/surveys.json?user_id=u1"} {
		rec := doGet(t, h, path)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, body = %s", path, rec.Code, rec.Body.String())
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
			t.Fatalf("%s: Content-Type = %q, want application/json (iOS rejects any other)", path, ct)
		}
		var list iosSurveyList
		if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
			t.Fatalf("%s: decode: %v", path, err)
		}
		if list.Surveys == nil || list.Region == nil || list.Region.ID == nil || list.Region.Name == nil {
			t.Fatalf("%s: top-level keys missing: %s", path, rec.Body.String())
		}
		if *list.Region.ID != 1 || *list.Region.Name != "Test Region" {
			t.Errorf("region = %d %q", *list.Region.ID, *list.Region.Name)
		}
		if len(*list.Surveys) != 1 {
			t.Fatalf("surveys = %d, want 1", len(*list.Surveys))
		}
		s := (*list.Surveys)[0]
		for name, p := range map[string]any{
			"id": s.ID, "name": s.Name, "created_at": s.CreatedAt, "updated_at": s.UpdatedAt,
			"show_on_map": s.ShowOnMap, "show_on_stops": s.ShowOnStops, "always_visible": s.AlwaysVisible,
			"allows_multiple_responses": s.AllowsMultipleResponses, "study": s.Study, "questions": s.Questions,
		} {
			if isNilPtr(p) {
				t.Errorf("%s: required key %q missing", path, name)
			}
		}
		if s.Study != nil && (s.Study.ID == nil || s.Study.Name == nil || string(s.Study.Description) != `""`) {
			t.Errorf("study = %+v; description must be \"\" not null", s.Study)
		}
		// CreatedAt decodes into a *string (no surrounding quotes), unlike
		// StartDate/EndDate's json.RawMessage (raw wire bytes, quotes
		// included) -- quote it before matching the shared regex.
		if !wireDateRe.MatchString(fmt.Sprintf("%q", *s.CreatedAt)) || !wireDateRe.Match(s.StartDate) || !wireDateRe.Match(s.EndDate) {
			t.Errorf("dates = %s / %s / %s, want .000Z format", *s.CreatedAt, s.StartDate, s.EndDate)
		}
		if string(s.VisibleRouteList) != "null" || string(s.VisibleStopList) != `["1_570"]` {
			t.Errorf("lists = %s / %s", s.VisibleStopList, s.VisibleRouteList)
		}
		if !*s.ShowOnMap || !*s.AlwaysVisible || *s.AllowsMultipleResponses {
			t.Errorf("booleans = %v %v %v", *s.ShowOnMap, *s.AlwaysVisible, *s.AllowsMultipleResponses)
		}
		if len(*s.Questions) != 5 {
			t.Fatalf("questions = %d", len(*s.Questions))
		}
		for i, q := range *s.Questions {
			if q.ID == nil || q.Position == nil || q.Required == nil || q.Content == nil || q.Content.Type == nil || q.Content.LabelText == nil {
				t.Errorf("question %d missing a required key: %+v", i, q)
			}
			if q.Position != nil && *q.Position != int64(i+1) {
				t.Errorf("question %d position = %d", i, *q.Position)
			}
		}
		if q := (*s.Questions)[4]; q.Required != nil && *q.Required {
			t.Error("external_survey question emitted required=true")
		}
	}
}

func isNilPtr(v any) bool {
	switch p := v.(type) {
	case *int64:
		return p == nil
	case *string:
		return p == nil
	case *bool:
		return p == nil
	case nil:
		return true
	}
	b, _ := json.Marshal(v)
	return string(b) == "null"
}

func TestSurveyList_PerTypeContentKeys(t *testing.T) {
	t.Parallel()
	h, repo, regs := surveyDeps(t, nil, nil)
	seedSurvey(t, repo, regs, 1, fullDefinition())
	rec := doGet(t, h, "/api/v1/regions/1/surveys?user_id=u1")
	var body struct {
		Surveys []struct {
			Questions []struct {
				Content map[string]json.RawMessage `json:"content"`
			} `json:"questions"`
		} `json:"surveys"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"label_text", "options", "type"},
		{"label_text", "type"},
		{"label_text", "type"},
		{"label_text", "options", "type"},
		{"embedded_data_fields", "label_text", "sdk_configuration_values", "survey_provider", "type", "url"},
	}
	for i, q := range body.Surveys[0].Questions {
		got := make([]string, 0, len(q.Content))
		for k := range q.Content {
			got = append(got, k)
		}
		sort.Strings(got)
		if strings.Join(got, ",") != strings.Join(want[i], ",") {
			t.Errorf("question %d keys = %v, want %v", i, got, want[i])
		}
	}
	if sdk := body.Surveys[0].Questions[4].Content["sdk_configuration_values"]; string(sdk) != `{"a":1}` {
		t.Errorf("sdk_configuration_values = %s", sdk)
	}
}

func TestSurveyList_UnsetWindowAndEmptyList(t *testing.T) {
	t.Parallel()
	h, repo, regs := surveyDeps(t, nil, nil)
	seedSurvey(t, repo, regs, 1, surveys.Definition{Name: "Minimal", Available: true})
	putRegion(t, regs, 2)
	rec := doGet(t, h, "/api/v1/regions/1/surveys?user_id=u1")
	var body struct {
		Surveys []struct {
			StartDate json.RawMessage `json:"start_date"`
			EndDate   json.RawMessage `json:"end_date"`
			Questions json.RawMessage `json:"questions"`
		} `json:"surveys"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	s := body.Surveys[0]
	if string(s.StartDate) != "null" || string(s.EndDate) != "null" {
		t.Errorf("unset window = %s / %s, want literal null", s.StartDate, s.EndDate)
	}
	if string(s.Questions) != "[]" {
		t.Errorf("questions = %s, want []", s.Questions)
	}
	empty := doGet(t, h, "/api/v1/regions/2/surveys?user_id=u1")
	if empty.Code != http.StatusOK || !strings.Contains(empty.Body.String(), `"surveys":[]`) {
		t.Errorf("region with no surveys: %d %s", empty.Code, empty.Body.String())
	}
}

func TestSurveyList_Errors(t *testing.T) {
	t.Parallel()
	h, repo, regs := surveyDeps(t, nil, nil)
	seedSurvey(t, repo, regs, 1, fullDefinition())
	tests := []struct {
		name, path string
		status     int
		body       string
	}{
		{"unknown region", "/api/v1/regions/42/surveys?user_id=u1", 404, `{"error":"Couldn't find Region"}`},
		{"unknown region before user_id", "/api/v1/regions/42/surveys", 404, `{"error":"Couldn't find Region"}`},
		{"missing user_id", "/api/v1/regions/1/surveys", 422, `{"errors":["user_id is required"]}`},
		{"blank user_id", "/api/v1/regions/1/surveys?user_id=%20", 422, `{"errors":["user_id is required"]}`},
		{"slug region ok", "/api/v1/regions/1-test-region/surveys?user_id=u1", 200, `"name":"Rider satisfaction"`},
		{"ios default query items ignored", "/api/v1/regions/1/surveys.json?user_id=u1&key=k&app_uid=x&app_ver=1&version=2", 200, `"surveys"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec := doGet(t, h, tt.path)
			if rec.Code != tt.status || !strings.Contains(strings.TrimSpace(rec.Body.String()), tt.body) {
				t.Fatalf("status = %d body = %s; want %d containing %s", rec.Code, rec.Body.String(), tt.status, tt.body)
			}
		})
	}
}

// TestSurveyList_WindowFilterIsServerSide: Android does no date filtering,
// so an expired survey returned here would be shown to riders.
func TestSurveyList_WindowFilterIsServerSide(t *testing.T) {
	t.Parallel()
	h, repo, regs := surveyDeps(t, nil, nil)
	start, end := base.Add(-48*time.Hour), base.Add(-24*time.Hour)
	seedSurvey(t, repo, regs, 1, surveys.Definition{Name: "Expired", Available: true, StartTime: &start, EndTime: &end})
	rec := doGet(t, h, "/api/v1/regions/1/surveys?user_id=u1")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"surveys":[]`) {
		t.Fatalf("expired survey leaked: %d %s", rec.Code, rec.Body.String())
	}
}

func TestSurveyRoutesRequireDeps(t *testing.T) {
	t.Parallel()
	store := sqlitetest.Open(t)
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("NewRouter did not panic with Deps.Surveys set and Now/Regions missing")
		}
		msg := fmt.Sprint(r)
		for _, want := range []string{"Now", "Regions"} {
			if !strings.Contains(msg, want) {
				t.Errorf("panic message %q missing %q", msg, want)
			}
		}
	}()
	httpapi.NewRouter(httpapi.Deps{Surveys: store.Surveys()})
}

// TestSurveyRoutes_Registered pins the seven patterns of design spec 2.1 by
// method+path: a missing pattern is a 404 (or 405) here, not a 501 from
// the not-yet-implemented handler.
func TestSurveyRoutes_Registered(t *testing.T) {
	t.Parallel()
	h, _, regs := surveyDeps(t, nil, nil)
	putRegion(t, regs, 1)
	for _, rt := range []struct{ method, path string }{
		{"GET", "/api/v1/regions/1/surveys?user_id=u"},
		{"GET", "/api/v1/regions/1/surveys.json?user_id=u"},
		{"POST", "/api/v1/survey_responses"},
		{"POST", "/api/v1/survey_responses/"},
		{"POST", "/api/v1/survey_responses/abc"},
		{"PUT", "/api/v1/survey_responses/abc"},
		{"PATCH", "/api/v1/survey_responses/abc"},
	} {
		req := httptest.NewRequestWithContext(context.Background(), rt.method, rt.path, strings.NewReader(""))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusNotFound && strings.TrimSpace(rec.Body.String()) == "404 page not found" ||
			rec.Code == http.StatusMethodNotAllowed {
			t.Errorf("%s %s not routed: %d %s", rt.method, rt.path, rec.Code, rec.Body.String())
		}
	}
}
