package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/OneBusAway/sidecar/internal/surveys"
)

// minimalQuestionsJSON is the smallest "questions": [...] fragment
// Definition.Validate accepts: a single label question, which needs no
// options and (per Validate's set_required_param rule) is never Required
// regardless of what the document says. See
// internal/surveys/definition_test.go's validDefinition for the fixture
// this is derived from.
const minimalQuestionsJSON = `"questions":[{"content":{"type":"label","label_text":"q"}}]`

// TestAdminStudies_CRUD pins the study family's happy paths and codes.
func TestAdminStudies_CRUD(t *testing.T) {
	t.Parallel()

	f := newFullAdminFixture(t)
	created := object(t, f.do(http.MethodPost, "/api/admin/v1/regions/1/studies",
		`{"name":"Fall 2026","description":"rider experience"}`), http.StatusCreated)
	assertKeys(t, "study", created, []string{"id", "name", "description", "created_at", "updated_at"})
	id := jsonID(t, created)

	got := object(t, f.do(http.MethodGet, fmt.Sprintf("/api/admin/v1/regions/1/studies/%d", id), ""), http.StatusOK)
	if got["name"] != "Fall 2026" {
		t.Errorf("name = %v", got["name"])
	}

	patched := object(t, f.do(http.MethodPatch, fmt.Sprintf("/api/admin/v1/regions/1/studies/%d", id),
		`{"name":"Fall 2026 (revised)"}`), http.StatusOK)
	if patched["name"] != "Fall 2026 (revised)" || patched["description"] != "rider experience" {
		t.Errorf("PATCH must merge, not replace: %v", patched)
	}

	list := array(t, f.do(http.MethodGet, "/api/admin/v1/regions/1/studies", ""), http.StatusOK)
	if len(list) != 1 {
		t.Errorf("got %d studies, want 1", len(list))
	}
	if empty := array(t, f.do(http.MethodGet, "/api/admin/v1/regions/0/studies", ""), http.StatusOK); len(empty) != 0 {
		t.Errorf("region 0 shows %d of region 1's studies", len(empty))
	}
	// A blank name is a well-formed body that fails domain validation: 422,
	// not 400 (design spec section 5, "Status codes").
	if rec := f.do(http.MethodPost, "/api/admin/v1/regions/1/studies", `{"name":"  "}`); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("blank name: status = %d, want 422", rec.Code)
	}
	if rec := f.do(http.MethodPost, "/api/admin/v1/regions/1/studies", `{`); rec.Code != http.StatusBadRequest {
		t.Errorf("malformed JSON: status = %d, want 400", rec.Code)
	}
}

// TestAdminStudies_ForeignStudyIs404 -- the loader, not a post-hoc compare.
func TestAdminStudies_ForeignStudyIs404(t *testing.T) {
	t.Parallel()

	f := newFullAdminFixture(t)
	id := jsonID(t, object(t, f.do(http.MethodPost, "/api/admin/v1/regions/1/studies", `{"name":"a"}`), http.StatusCreated))

	for _, tc := range []struct{ method, body string }{
		{http.MethodGet, ""},
		{http.MethodPatch, `{"name":"hijacked"}`},
	} {
		rec := f.do(tc.method, fmt.Sprintf("/api/admin/v1/regions/0/studies/%d", id), tc.body)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s across regions: status = %d, want 404; body = %s", tc.method, rec.Code, rec.Body.String())
		}
	}
	after := object(t, f.do(http.MethodGet, fmt.Sprintf("/api/admin/v1/regions/1/studies/%d", id), ""), http.StatusOK)
	if after["name"] != "a" {
		t.Errorf("a refused PATCH still wrote: name = %v", after["name"])
	}
}

// TestAdminSurveys_StrictDecoding. DisallowUnknownFields, exactly as the CLI
// does: a misspelled show_on_maps must not silently hide a survey.
func TestAdminSurveys_StrictDecoding(t *testing.T) {
	t.Parallel()

	f := newFullAdminFixture(t)
	studyID := jsonID(t, object(t, f.do(http.MethodPost, "/api/admin/v1/regions/1/studies", `{"name":"s"}`), http.StatusCreated))

	body := fmt.Sprintf(`{"study_id":%d,"name":"q","show_on_maps":true,%s}`, studyID, minimalQuestionsJSON)
	rec := f.do(http.MethodPost, "/api/admin/v1/regions/1/surveys", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown field: status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(bodyText(rec), "show_on_maps") {
		t.Errorf("body = %q, want it to name the unknown field", bodyText(rec))
	}
}

// TestAdminSurveys_BodyCap: 256 KB, mapped to 400 with the operator-facing
// message rather than encoding/json's Go-developer wording.
func TestAdminSurveys_BodyCap(t *testing.T) {
	t.Parallel()

	f := newFullAdminFixture(t)
	studyID := jsonID(t, object(t, f.do(http.MethodPost, "/api/admin/v1/regions/1/studies", `{"name":"s"}`), http.StatusCreated))
	huge := fmt.Sprintf(`{"study_id":%d,"name":"%s",%s}`, studyID, strings.Repeat("a", 300*1024), minimalQuestionsJSON)

	rec := f.do(http.MethodPost, "/api/admin/v1/regions/1/surveys", huge)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if got, want := bodyText(rec), `{"error":"request body too large"}`; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}

// TestAdminSurveys_ServerOwnedFieldsRejected. id, study, created_at and
// updated_at round-trip out of `survey show`, so a client that pipes show
// into create must be told, not silently obeyed (design spec section 5.2).
func TestAdminSurveys_ServerOwnedFieldsRejected(t *testing.T) {
	t.Parallel()

	f := newFullAdminFixture(t)
	studyID := jsonID(t, object(t, f.do(http.MethodPost, "/api/admin/v1/regions/1/studies", `{"name":"s"}`), http.StatusCreated))

	for _, extra := range []string{
		`"id":5`, `"study":{"id":1,"name":"x","description":""}`,
		`"created_at":"2026-01-01T00:00:00.000Z"`, `"updated_at":"2026-01-01T00:00:00.000Z"`,
	} {
		body := fmt.Sprintf(`{"study_id":%d,"name":"q",%s,%s}`, studyID, extra, minimalQuestionsJSON)
		if rec := f.do(http.MethodPost, "/api/admin/v1/regions/1/surveys", body); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status = %d, want 422; body = %s", extra, rec.Code, rec.Body.String())
		}
	}
}

// TestAdminSurveys_ForeignStudyIsNotFound. The study_id arrives in the BODY,
// so this is what CreateSurveyInRegion's join condition exists for -- a
// loader-then-compare here would be the thing a refactor drops.
func TestAdminSurveys_ForeignStudyIsNotFound(t *testing.T) {
	t.Parallel()

	f := newFullAdminFixture(t)
	studyID := jsonID(t, object(t, f.do(http.MethodPost, "/api/admin/v1/regions/0/studies", `{"name":"tampa"}`), http.StatusCreated))

	body := fmt.Sprintf(`{"study_id":%d,"name":"q",%s}`, studyID, minimalQuestionsJSON)
	if rec := f.do(http.MethodPost, "/api/admin/v1/regions/1/surveys", body); rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
	}
}

// TestAdminSurveys_RoundTrip: GET returns the same document PUT accepts.
func TestAdminSurveys_RoundTrip(t *testing.T) {
	t.Parallel()

	f := newFullAdminFixture(t)
	studyID := jsonID(t, object(t, f.do(http.MethodPost, "/api/admin/v1/regions/1/studies", `{"name":"s"}`), http.StatusCreated))
	created := object(t, f.do(http.MethodPost, "/api/admin/v1/regions/1/surveys",
		fmt.Sprintf(`{"study_id":%d,"name":"q",%s}`, studyID, minimalQuestionsJSON)), http.StatusCreated)
	id := jsonID(t, created)

	shown := object(t, f.do(http.MethodGet, fmt.Sprintf("/api/admin/v1/regions/1/surveys/%d", id), ""), http.StatusOK)
	// The GET document carries the server-owned keys; PUT must reject them,
	// so a caller edits the document rather than replaying it verbatim. This
	// asserts the shape, not a blind round trip.
	assertKeys(t, "survey", shown, []string{
		"id", "name", "available", "start_date", "end_date", "show_on_map", "show_on_stops",
		"always_visible", "allows_multiple_responses", "visible_stop_list", "visible_route_list",
		"questions", "study", "created_at", "updated_at",
	})

	updated := object(t, f.do(http.MethodPut, fmt.Sprintf("/api/admin/v1/regions/1/surveys/%d", id),
		fmt.Sprintf(`{"name":"q2",%s}`, minimalQuestionsJSON)), http.StatusOK)
	if updated["name"] != "q2" {
		t.Errorf("name = %v, want q2", updated["name"])
	}
	// PUT cannot move a survey between studies.
	if rec := f.do(http.MethodPut, fmt.Sprintf("/api/admin/v1/regions/1/surveys/%d", id),
		fmt.Sprintf(`{"study_id":%d,"name":"q3",%s}`, studyID, minimalQuestionsJSON)); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("study_id on PUT: status = %d, want 422", rec.Code)
	}
}

// TestAdminSurveys_ConflictCodes: both repository refusals are 409, carrying
// the sentinel's own text.
//
// The brief's stub says "add a response through the rider API"; this uses
// store.Surveys().CreateResponse directly instead. The rider create endpoint
// is form-encoded and throttled and has its own extensive coverage in
// surveys_test.go -- what this test needs is a response *row*, not a second
// exercise of that endpoint, and the repository call is what every other
// survey test in this package (and the CLI's own tests) already uses to get
// one.
func TestAdminSurveys_ConflictCodes(t *testing.T) {
	t.Parallel()

	f := newFullAdminFixture(t)
	studyID := jsonID(t, object(t, f.do(http.MethodPost, "/api/admin/v1/regions/1/studies", `{"name":"s"}`), http.StatusCreated))
	id := jsonID(t, object(t, f.do(http.MethodPost, "/api/admin/v1/regions/1/surveys",
		fmt.Sprintf(`{"study_id":%d,"name":"q",%s}`, studyID, minimalQuestionsJSON)), http.StatusCreated))

	ctx := context.Background()
	s, err := f.store.Surveys().GetSurvey(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Surveys().CreateResponse(ctx, surveys.NewResponse{
		SurveyID: id, PublicID: "p1", UserIdentifier: "dev-1",
		Answers: []surveys.Answer{{QuestionID: s.Questions[0].ID, Answer: "x"}},
	}, testNow); err != nil {
		t.Fatal(err)
	}

	// PUT with a different question set on a survey that has responses:
	// 409 ErrQuestionsFrozen, not the wrapped store statement.
	changed := `{"name":"q2","questions":[{"content":{"type":"text","label_text":"different"}}]}`
	rec := f.do(http.MethodPut, fmt.Sprintf("/api/admin/v1/regions/1/surveys/%d", id), changed)
	if rec.Code != http.StatusConflict {
		t.Fatalf("PUT with different questions: status = %d, want 409; body = %s", rec.Code, rec.Body.String())
	}
	if got, want := bodyText(rec), fmt.Sprintf(`{"error":%q}`, surveys.ErrQuestionsFrozen.Error()); got != want {
		t.Errorf("body = %q, want %q", got, want)
	}

	// DELETE on a survey that has responses: 409 ErrHasResponses.
	rec = f.do(http.MethodDelete, fmt.Sprintf("/api/admin/v1/regions/1/surveys/%d", id), "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("DELETE with responses: status = %d, want 409; body = %s", rec.Code, rec.Body.String())
	}
	if got, want := bodyText(rec), fmt.Sprintf(`{"error":%q}`, surveys.ErrHasResponses.Error()); got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
	if _, err := f.store.Surveys().GetSurvey(ctx, id); err != nil {
		t.Errorf("the refused DELETE removed the survey: %v", err)
	}

	// A survey with no responses DELETEs with 204.
	empty := jsonID(t, object(t, f.do(http.MethodPost, "/api/admin/v1/regions/1/surveys",
		fmt.Sprintf(`{"study_id":%d,"name":"empty"}`, studyID)), http.StatusCreated))
	rec = f.do(http.MethodDelete, fmt.Sprintf("/api/admin/v1/regions/1/surveys/%d", empty), "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE without responses: status = %d, want 204; body = %s", rec.Code, rec.Body.String())
	}
	if _, err := f.store.Surveys().GetSurvey(ctx, empty); !errors.Is(err, surveys.ErrNotFound) {
		t.Errorf("the survey still exists after a 204 delete: %v", err)
	}
}

// TestAdminSurveys_ListCarriesResponseCounts: two surveys, one with two
// responses. Asserts study_id and response_count on each list entry, and
// that region 0's list is empty.
func TestAdminSurveys_ListCarriesResponseCounts(t *testing.T) {
	t.Parallel()

	f := newFullAdminFixture(t)
	studyID := jsonID(t, object(t, f.do(http.MethodPost, "/api/admin/v1/regions/1/studies", `{"name":"s"}`), http.StatusCreated))

	noResponses := jsonID(t, object(t, f.do(http.MethodPost, "/api/admin/v1/regions/1/surveys",
		fmt.Sprintf(`{"study_id":%d,"name":"a",%s}`, studyID, minimalQuestionsJSON)), http.StatusCreated))
	twoResponses := jsonID(t, object(t, f.do(http.MethodPost, "/api/admin/v1/regions/1/surveys",
		fmt.Sprintf(`{"study_id":%d,"name":"b",%s}`, studyID, minimalQuestionsJSON)), http.StatusCreated))

	ctx := context.Background()
	s, err := f.store.Surveys().GetSurvey(ctx, twoResponses)
	if err != nil {
		t.Fatal(err)
	}
	for i, publicID := range []string{"r1", "r2"} {
		if _, err := f.store.Surveys().CreateResponse(ctx, surveys.NewResponse{
			SurveyID: twoResponses, PublicID: publicID, UserIdentifier: fmt.Sprintf("dev-%d", i),
			Answers: []surveys.Answer{{QuestionID: s.Questions[0].ID, Answer: "x"}},
		}, testNow); err != nil {
			t.Fatal(err)
		}
	}

	list := array(t, f.do(http.MethodGet, "/api/admin/v1/regions/1/surveys", ""), http.StatusOK)
	if len(list) != 2 {
		t.Fatalf("got %d surveys, want 2", len(list))
	}
	byID := make(map[int64]map[string]any, len(list))
	for _, entry := range list {
		byID[jsonID(t, entry)] = entry
	}

	if got := byID[noResponses]; got == nil || num(t, got, "response_count") != 0 || int64(num(t, got, "study_id")) != studyID {
		t.Errorf("no-response survey = %v", got)
	}
	if got := byID[twoResponses]; got == nil || num(t, got, "response_count") != 2 || int64(num(t, got, "study_id")) != studyID {
		t.Errorf("two-response survey = %v", got)
	}

	if empty := array(t, f.do(http.MethodGet, "/api/admin/v1/regions/0/surveys", ""), http.StatusOK); len(empty) != 0 {
		t.Errorf("region 0 shows %d of region 1's surveys", len(empty))
	}
}

// TestAdminSurveys_PutPreservesQuestionIDs is the Rails-side round trip
// (migration design spec section 4.3): GET, strip the server-owned survey
// keys, edit, PUT -- with question ids left in place. The edited question
// keeps its id, the new one gets a fresh id, and a foreign id is 422.
func TestAdminSurveys_PutPreservesQuestionIDs(t *testing.T) {
	t.Parallel()

	f := newFullAdminFixture(t)
	studyID := jsonID(t, object(t, f.do(http.MethodPost, "/api/admin/v1/regions/1/studies", `{"name":"s"}`), http.StatusCreated))
	create := fmt.Sprintf(`{"study_id":%d,"name":"q","questions":[{"content":{"type":"text","label_text":"one"}},{"content":{"type":"text","label_text":"two"}}]}`, studyID)
	id := jsonID(t, object(t, f.do(http.MethodPost, "/api/admin/v1/regions/1/surveys", create), http.StatusCreated))
	path := fmt.Sprintf("/api/admin/v1/regions/1/surveys/%d", id)

	shown := object(t, f.do(http.MethodGet, path, ""), http.StatusOK)
	questions, _ := shown["questions"].([]any)
	first, _ := questions[0].(map[string]any)
	second, _ := questions[1].(map[string]any)
	firstID, secondID := int64(num(t, first, "id")), int64(num(t, second, "id"))

	put := fmt.Sprintf(`{"name":"q","questions":[{"id":%d,"content":{"type":"text","label_text":"one, edited"}},{"content":{"type":"text","label_text":"three"}}]}`, firstID)
	updated := object(t, f.do(http.MethodPut, path, put), http.StatusOK)
	got, _ := updated["questions"].([]any)
	if len(got) != 2 {
		t.Fatalf("questions = %v, want 2", updated["questions"])
	}
	kept, _ := got[0].(map[string]any)
	added, _ := got[1].(map[string]any)
	if int64(num(t, kept, "id")) != firstID {
		t.Errorf("edited question id = %v, want %d", kept["id"], firstID)
	}
	if aid := int64(num(t, added, "id")); aid == firstID || aid == secondID {
		t.Errorf("added question id = %d, want a fresh id", aid)
	}

	foreign := fmt.Sprintf(`{"name":"q","questions":[{"id":%d,"content":{"type":"text","label_text":"x"}}]}`, secondID+1000)
	rec := f.do(http.MethodPut, path, foreign)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("foreign question id: status = %d, want 422; body = %s", rec.Code, rec.Body.String())
	}
	if got, want := bodyText(rec), fmt.Sprintf(`{"error":%q}`, surveys.ErrUnknownQuestion.Error()); got != want {
		t.Errorf("body = %s, want %s", got, want)
	}

	// Create refuses ids too: they are server-owned.
	withID := fmt.Sprintf(`{"study_id":%d,"name":"q","questions":[{"id":%d,"content":{"type":"text","label_text":"x"}}]}`, studyID, firstID)
	if rec := f.do(http.MethodPost, "/api/admin/v1/regions/1/surveys", withID); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("question id on create: status = %d, want 422", rec.Code)
	}
}
