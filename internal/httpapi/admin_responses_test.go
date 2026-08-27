package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/OneBusAway/sidecar/internal/surveys"
)

// seedSurveyWithResponses creates a study, a survey with one text question,
// and one response per supplied answer string -- each response's single
// answer is that string. It goes through f.store.Surveys() directly, not
// the admin API, so a bug in the routes under test here cannot also corrupt
// the fixture that is supposed to expose it.
func (f *adminFixture) seedSurveyWithResponses(t *testing.T, regionID int64, answers []string) int64 {
	t.Helper()
	ctx := context.Background()

	st, err := f.store.Surveys().CreateStudy(ctx, regionID, "responses fixture", "", testNow)
	if err != nil {
		t.Fatalf("create study: %v", err)
	}
	def := surveys.Definition{
		Name: "responses fixture survey",
		Questions: []surveys.QuestionDefinition{
			{Content: surveys.Content{Type: surveys.TypeText, LabelText: "How was your trip?"}},
		},
	}
	sv, err := f.store.Surveys().CreateSurvey(ctx, st.ID, def, testNow)
	if err != nil {
		t.Fatalf("create survey: %v", err)
	}
	for i, answer := range answers {
		if _, err := f.store.Surveys().CreateResponse(ctx, surveys.NewResponse{
			SurveyID: sv.ID, PublicID: fmt.Sprintf("resp-%d-%d", sv.ID, i), UserIdentifier: fmt.Sprintf("dev-%d", i),
			Answers: []surveys.Answer{{
				QuestionID: sv.Questions[0].ID, QuestionType: surveys.TypeText,
				QuestionLabel: "How was your trip?", Answer: answer,
			}},
		}, testNow); err != nil {
			t.Fatalf("create response %d: %v", i, err)
		}
	}
	return sv.ID
}

// TestAdminResponses_CSVContract pins every header the export must carry.
// Content-Disposition is server-generated and fixed: a filename derived
// from a survey name would put rider-influenced text in a header.
func TestAdminResponses_CSVContract(t *testing.T) {
	t.Parallel()

	f := newFullAdminFixture(t)
	surveyID := f.seedSurveyWithResponses(t, regionPuget, []string{"=cmd|' /C calc'!A0", "fine"})

	rec := f.do(http.MethodGet, fmt.Sprintf("/api/admin/v1/regions/1/surveys/%d/responses.csv", surveyID), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	for header, want := range map[string]string{
		"Content-Type":           "text/csv",
		"X-Content-Type-Options": "nosniff",
		"Cache-Control":          "no-store",
		"Content-Disposition":    fmt.Sprintf(`attachment; filename="survey-%d-responses.csv"`, surveyID),
	} {
		if got := rec.Header().Get(header); !strings.HasPrefix(got, want) {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	// The formula guard survives the move out of the CLI. The guarded cell
	// has no comma, quote, or newline of its own, so encoding/csv leaves it
	// unquoted -- the literal apostrophe-prefixed text appears verbatim in
	// the body, with no wrapping double quote to look for.
	if !strings.Contains(rec.Body.String(), "'=cmd") {
		t.Errorf("rider answer not defused:\n%s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), ",fine\n") && !strings.Contains(rec.Body.String(), ",fine\r\n") {
		t.Errorf("benign answer missing or altered:\n%s", rec.Body.String())
	}
}

// TestAdminResponses_ReachedThroughAnotherRegionsSurveyIs404 is the case the
// single joined query exists for: /regions/A/survey_responses/{id-of-B}.
func TestAdminResponses_ReachedThroughAnotherRegionsSurveyIs404(t *testing.T) {
	t.Parallel()

	f := newFullAdminFixture(t)
	surveyID := f.seedSurveyWithResponses(t, regionPuget, []string{"a"})
	list := array(t, f.do(http.MethodGet, fmt.Sprintf("/api/admin/v1/regions/1/surveys/%d/responses", surveyID), ""), http.StatusOK)
	if len(list) != 1 {
		t.Fatalf("got %d responses, want 1", len(list))
	}
	publicID := str(t, list[0], "public_id")

	if rec := f.do(http.MethodGet, "/api/admin/v1/regions/0/survey_responses/"+publicID, ""); rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
	}
	if rec := f.do(http.MethodGet, "/api/admin/v1/regions/1/survey_responses/"+publicID, ""); rec.Code != http.StatusOK {
		t.Errorf("own region: status = %d, want 200", rec.Code)
	}
	// Listing responses loads and checks the SURVEY first, so a foreign
	// survey id is a 404 rather than an empty array.
	if rec := f.do(http.MethodGet, fmt.Sprintf("/api/admin/v1/regions/0/surveys/%d/responses", surveyID), ""); rec.Code != http.StatusNotFound {
		t.Errorf("foreign survey responses: status = %d, want 404", rec.Code)
	}
	if rec := f.do(http.MethodGet, fmt.Sprintf("/api/admin/v1/regions/0/surveys/%d/responses.csv", surveyID), ""); rec.Code != http.StatusNotFound {
		t.Errorf("foreign survey CSV: status = %d, want 404", rec.Code)
	}
}

// TestAdminResponses_JSONShape pins the exact field set of both the list and
// single-response views, and that instants use surveys.FormatTime's
// three-fraction-digit wire shape rather than formatInstant's plain RFC
// 3339 (see toAdminStudyJSON's comment in admin_surveys.go for why the
// whole survey family shares that convention).
func TestAdminResponses_JSONShape(t *testing.T) {
	t.Parallel()

	f := newFullAdminFixture(t)
	surveyID := f.seedSurveyWithResponses(t, regionPuget, []string{"Great trip"})

	list := array(t, f.do(http.MethodGet, fmt.Sprintf("/api/admin/v1/regions/1/surveys/%d/responses", surveyID), ""), http.StatusOK)
	if len(list) != 1 {
		t.Fatalf("got %d responses, want 1", len(list))
	}
	got := list[0]
	assertKeys(t, "response", got, []string{
		"id", "survey_id", "public_id", "user_identifier", "stop_identifier",
		"stop_latitude", "stop_longitude", "answers", "created_at", "updated_at",
	})
	if int64(num(t, got, "survey_id")) != surveyID {
		t.Errorf("survey_id = %v, want %d", got["survey_id"], surveyID)
	}

	answers, ok := got["answers"].([]any)
	if !ok || len(answers) != 1 {
		t.Fatalf("answers = %v, want a one-element array", got["answers"])
	}
	answer, ok := answers[0].(map[string]any)
	if !ok {
		t.Fatalf("answers[0] = %v, want an object", answers[0])
	}
	assertKeys(t, "answer", answer, []string{"question_id", "question_type", "question_label", "answer"})
	if answer["answer"] != "Great trip" {
		t.Errorf("answer = %v, want %q", answer["answer"], "Great trip")
	}

	for _, field := range []string{"created_at", "updated_at"} {
		v := str(t, got, field)
		if !strings.HasSuffix(v, ".000Z") {
			t.Errorf("%s = %q, want the surveys.FormatTime .000Z shape", field, v)
		}
	}

	// The single-response route returns the same shape.
	publicID := str(t, got, "public_id")
	single := object(t, f.do(http.MethodGet, "/api/admin/v1/regions/1/survey_responses/"+publicID, ""), http.StatusOK)
	assertKeys(t, "single response", single, []string{
		"id", "survey_id", "public_id", "user_identifier", "stop_identifier",
		"stop_latitude", "stop_longitude", "answers", "created_at", "updated_at",
	})
	if single["public_id"] != publicID {
		t.Errorf("public_id = %v, want %q", single["public_id"], publicID)
	}
}

// TestAdminResponses_UnknownPublicIDIs404 pins the ordinary "does not exist
// at all" case, distinct from the foreign-region case above.
func TestAdminResponses_UnknownPublicIDIs404(t *testing.T) {
	t.Parallel()

	f := newFullAdminFixture(t)
	rec := f.do(http.MethodGet, "/api/admin/v1/regions/1/survey_responses/does-not-exist", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
	}
}
