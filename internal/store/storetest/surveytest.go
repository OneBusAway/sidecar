package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/regions"
	"github.com/OneBusAway/sidecar/internal/surveys"
)

type newSurveyStoreFunc func(*testing.T) (surveys.Repository, regions.Repository)

// RunSurveyRepository exercises a surveys.Repository against the behavioral
// contract both engines must satisfy. Each subtest gets a fresh store.
func RunSurveyRepository(t *testing.T, newStore newSurveyStoreFunc) {
	t.Helper()
	t.Run("StudyCreateGetList", func(t *testing.T) { testStudyCreateGetList(t, newStore) })
	t.Run("SurveyRoundTrip", func(t *testing.T) { testSurveyRoundTrip(t, newStore) })
	t.Run("SurveyNilListsAndWindow", func(t *testing.T) { testSurveyNilListsAndWindow(t, newStore) })
	t.Run("CreateSurveyUnknownStudy", func(t *testing.T) { testCreateSurveyUnknownStudy(t, newStore) })
	t.Run("ActiveFilter", func(t *testing.T) { testActiveFilter(t, newStore) })
	t.Run("ActiveFilterRegionScoping", func(t *testing.T) { testActiveFilterRegionScoping(t, newStore) })
	t.Run("EndTimeBeyond32Bit", func(t *testing.T) { testSurveyEndTimeBeyond32Bit(t, newStore) })
	t.Run("UpdateReplacesQuestionsWhenNoResponses", func(t *testing.T) { testUpdateReplacesQuestions(t, newStore) })
	t.Run("UpdateFreezesQuestionsOnceAnswered", func(t *testing.T) { testUpdateFreezesQuestions(t, newStore) })
	t.Run("UpdateScalarsOnFrozenSurvey", func(t *testing.T) { testUpdateScalarsOnFrozen(t, newStore) })
	t.Run("DeleteRefusesWithResponses", func(t *testing.T) { testDeleteRefusesWithResponses(t, newStore) })
	t.Run("DeleteCascadesQuestions", func(t *testing.T) { testDeleteCascadesQuestions(t, newStore) })
	t.Run("ResponseCreateGet", func(t *testing.T) { testResponseCreateGet(t, newStore) })
	t.Run("ResponseCreateUnknownSurvey", func(t *testing.T) { testResponseCreateUnknownSurvey(t, newStore) })
	t.Run("AmendMergesByQuestionID", func(t *testing.T) { testAmendMerges(t, newStore) })
	t.Run("AmendNotFoundAndCap", func(t *testing.T) { testAmendNotFoundAndCap(t, newStore) })
	t.Run("ConcurrentAmendsBothLand", func(t *testing.T) { testConcurrentAmendsBothLand(t, newStore) })
	t.Run("ListResponsesOrdered", func(t *testing.T) { testListResponsesOrdered(t, newStore) })
	t.Run("CountResponses", func(t *testing.T) { testCountResponses(t, newStore) })
}

func surveyDef(name string) surveys.Definition {
	return surveys.Definition{
		Name: name, Available: true, ShowOnStops: true,
		VisibleStopList: []string{"1_570", "1_578"},
		Questions: []surveys.QuestionDefinition{
			{Required: true, Content: surveys.Content{Type: "radio", LabelText: "Trip?", Options: []string{"Good", "Bad"}}},
			{Content: surveys.Content{Type: "external_survey", LabelText: "More", URL: "https://e.org/s",
				SurveyProvider: "qualtrics", EmbeddedDataFields: []string{"user_id"},
				SDKConfigurationValues: json.RawMessage(`{"a":1}`)}},
		},
	}
}

// seedStudy puts a region and a study in place; every survey subtest needs
// both before it can write a survey.
func seedStudy(t *testing.T, repo surveys.Repository, regs regions.Repository, regionID int64) surveys.Study {
	t.Helper()
	putStoretestRegion(t, regs, regionID)
	st, err := repo.CreateStudy(context.Background(), regionID, "Study", "desc", base)
	if err != nil {
		t.Fatalf("CreateStudy: %v", err)
	}
	return st
}

func mustCreateSurvey(t *testing.T, repo surveys.Repository, studyID int64, def surveys.Definition) surveys.Survey {
	t.Helper()
	if err := def.Validate(); err != nil {
		t.Fatalf("definition: %v", err)
	}
	s, err := repo.CreateSurvey(context.Background(), studyID, def, base)
	if err != nil {
		t.Fatalf("CreateSurvey: %v", err)
	}
	return s
}

func testStudyCreateGetList(t *testing.T, newStore newSurveyStoreFunc) {
	t.Parallel()
	repo, regs := newStore(t)
	ctx := context.Background()
	putStoretestRegion(t, regs, 1)
	putStoretestRegion(t, regs, 2)
	a, err := repo.CreateStudy(ctx, 1, "A", "", base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = repo.CreateStudy(ctx, 2, "Other region", "", base); err != nil {
		t.Fatal(err)
	}
	b, err := repo.CreateStudy(ctx, 1, "B", "about b", base.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	got, err := repo.GetStudy(ctx, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := surveys.Study{ID: b.ID, RegionID: 1, Name: "B", Description: "about b", CreatedAt: base.Add(time.Hour), UpdatedAt: base.Add(time.Hour)}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("GetStudy = %+v, want %+v", got, want)
	}
	list, err := repo.ListStudies(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != a.ID || list[1].ID != b.ID {
		t.Fatalf("ListStudies(1) = %+v, want [A, B] by id", list)
	}
	if _, err := repo.GetStudy(ctx, 999); !errors.Is(err, surveys.ErrNotFound) {
		t.Fatalf("GetStudy(999) err = %v, want ErrNotFound", err)
	}
}

func testSurveyRoundTrip(t *testing.T, newStore newSurveyStoreFunc) {
	t.Parallel()
	repo, regs := newStore(t)
	st := seedStudy(t, repo, regs, 1)
	def := surveyDef("Rider satisfaction")
	start, end := base.Add(24*time.Hour), base.Add(48*time.Hour)
	def.StartTime, def.EndTime = &start, &end
	def.ShowOnMap, def.AlwaysVisible, def.AllowsMultipleResponses = true, true, true
	def.VisibleRouteList = []string{"1_100"}
	created := mustCreateSurvey(t, repo, st.ID, def)

	got, err := repo.GetSurvey(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "Rider satisfaction" || !got.Available || !got.ShowOnMap || !got.ShowOnStops ||
		!got.AlwaysVisible || !got.AllowsMultipleResponses {
		t.Errorf("scalars = %+v", got)
	}
	if got.StartTime == nil || !got.StartTime.Equal(start) || got.EndTime == nil || !got.EndTime.Equal(end) {
		t.Errorf("window = %v..%v, want %v..%v", got.StartTime, got.EndTime, start, end)
	}
	if !reflect.DeepEqual(got.VisibleStopList, []string{"1_570", "1_578"}) || !reflect.DeepEqual(got.VisibleRouteList, []string{"1_100"}) {
		t.Errorf("lists = %v / %v", got.VisibleStopList, got.VisibleRouteList)
	}
	if got.Study.ID != st.ID || got.Study.Name != "Study" || got.Study.Description != "desc" {
		t.Errorf("Study = %+v, want the seeded study", got.Study)
	}
	if len(got.Questions) != 2 {
		t.Fatalf("Questions = %+v, want 2", got.Questions)
	}
	q0, q1 := got.Questions[0], got.Questions[1]
	if q0.Position != 1 || !q0.Required || !surveys.ContentEqual(q0.Content, def.Questions[0].Content) {
		t.Errorf("q0 = %+v", q0)
	}
	if q1.Position != 2 || q1.Required || !surveys.ContentEqual(q1.Content, def.Questions[1].Content) {
		t.Errorf("q1 = %+v (Required must be false for external_survey)", q1)
	}
	if q0.ID == 0 || q1.ID == 0 || q0.ID == q1.ID {
		t.Errorf("question ids = %d, %d", q0.ID, q1.ID)
	}
	if !got.CreatedAt.Equal(base) || got.CreatedAt.Location() != time.UTC {
		t.Errorf("CreatedAt = %v (%v)", got.CreatedAt, got.CreatedAt.Location())
	}
	if _, err := repo.GetSurvey(context.Background(), 999); !errors.Is(err, surveys.ErrNotFound) {
		t.Fatalf("GetSurvey(999) err = %v", err)
	}
}

func testSurveyNilListsAndWindow(t *testing.T, newStore newSurveyStoreFunc) {
	t.Parallel()
	repo, regs := newStore(t)
	st := seedStudy(t, repo, regs, 1)
	def := surveys.Definition{Name: "Minimal", Available: true}
	created := mustCreateSurvey(t, repo, st.ID, def)
	got, err := repo.GetSurvey(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.VisibleStopList != nil || got.VisibleRouteList != nil {
		t.Errorf("lists = %#v / %#v, want nil", got.VisibleStopList, got.VisibleRouteList)
	}
	if got.StartTime != nil || got.EndTime != nil {
		t.Errorf("window = %v..%v, want nil", got.StartTime, got.EndTime)
	}
	if got.Questions == nil || len(got.Questions) != 0 {
		t.Errorf("Questions = %#v, want empty non-nil", got.Questions)
	}
}

func testCreateSurveyUnknownStudy(t *testing.T, newStore newSurveyStoreFunc) {
	t.Parallel()
	repo, _ := newStore(t)
	def := surveyDef("x")
	if err := def.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CreateSurvey(context.Background(), 999, def, base); !errors.Is(err, surveys.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// testActiveFilter pins the spec 7.1 window (design spec 2.4): inclusive at
// both bounds, unscheduled always active, unavailable never, ordered by id.
func testActiveFilter(t *testing.T, newStore newSurveyStoreFunc) {
	t.Parallel()
	repo, regs := newStore(t)
	st := seedStudy(t, repo, regs, 1)
	start, end := base.Add(time.Hour), base.Add(2*time.Hour)
	windowed := surveys.Definition{Name: "windowed", Available: true, StartTime: &start, EndTime: &end}
	always := surveys.Definition{Name: "always", Available: true}
	off := surveys.Definition{Name: "off", Available: false}
	w := mustCreateSurvey(t, repo, st.ID, windowed)
	a := mustCreateSurvey(t, repo, st.ID, always)
	mustCreateSurvey(t, repo, st.ID, off)

	ids := func(now time.Time) []int64 {
		t.Helper()
		list, err := repo.ListActiveSurveys(context.Background(), 1, now)
		if err != nil {
			t.Fatal(err)
		}
		out := make([]int64, len(list))
		for i, s := range list {
			out[i] = s.ID
			if s.Study.ID != st.ID {
				t.Errorf("survey %d missing Study", s.ID)
			}
		}
		return out
	}
	cases := []struct {
		name string
		now  time.Time
		want []int64
	}{
		{"before start", start.Add(-time.Second), []int64{a.ID}},
		{"at start", start, []int64{w.ID, a.ID}},
		{"inside", start.Add(time.Minute), []int64{w.ID, a.ID}},
		{"at end", end, []int64{w.ID, a.ID}},
		{"after end", end.Add(time.Second), []int64{a.ID}},
	}
	for _, c := range cases {
		if got := ids(c.now); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: ids = %v, want %v", c.name, got, c.want)
		}
	}
	all, err := repo.ListSurveys(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Errorf("ListSurveys = %d surveys, want 3 (authoring sees everything)", len(all))
	}
}

func testActiveFilterRegionScoping(t *testing.T, newStore newSurveyStoreFunc) {
	t.Parallel()
	repo, regs := newStore(t)
	st1 := seedStudy(t, repo, regs, 1)
	st2 := seedStudy(t, repo, regs, 2)
	mustCreateSurvey(t, repo, st1.ID, surveys.Definition{Name: "r1", Available: true})
	mustCreateSurvey(t, repo, st2.ID, surveys.Definition{Name: "r2", Available: true})
	list, err := repo.ListActiveSurveys(context.Background(), 2, base)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Name != "r2" {
		t.Fatalf("region 2 list = %+v", list)
	}
	empty, err := repo.ListActiveSurveys(context.Background(), 3, base)
	if err != nil {
		t.Fatal(err)
	}
	if empty == nil || len(empty) != 0 {
		t.Fatalf("unknown region list = %#v, want empty non-nil", empty)
	}
}

func testSurveyEndTimeBeyond32Bit(t *testing.T, newStore newSurveyStoreFunc) {
	t.Parallel()
	repo, regs := newStore(t)
	st := seedStudy(t, repo, regs, 1)
	start := base
	end := time.Unix(1<<31+1000, 0).UTC()
	s := mustCreateSurvey(t, repo, st.ID, surveys.Definition{Name: "far", Available: true, StartTime: &start, EndTime: &end})
	got, err := repo.GetSurvey(context.Background(), s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.EndTime == nil || !got.EndTime.Equal(end) {
		t.Fatalf("EndTime = %v, want %v (a 32-bit column would have truncated)", got.EndTime, end)
	}
}

func testUpdateReplacesQuestions(t *testing.T, newStore newSurveyStoreFunc) {
	t.Parallel()
	repo, regs := newStore(t)
	st := seedStudy(t, repo, regs, 1)
	s := mustCreateSurvey(t, repo, st.ID, surveyDef("v1"))
	def := surveys.Definition{Name: "v2", Available: false, Questions: []surveys.QuestionDefinition{
		{Content: surveys.Content{Type: "text", LabelText: "Only"}},
	}}
	if err := def.Validate(); err != nil {
		t.Fatal(err)
	}
	got, err := repo.UpdateSurvey(context.Background(), s.ID, def, base.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "v2" || got.Available || got.ShowOnStops || got.VisibleStopList != nil {
		t.Errorf("scalars not rewritten: %+v", got)
	}
	if len(got.Questions) != 1 || got.Questions[0].Position != 1 || got.Questions[0].Content.LabelText != "Only" {
		t.Errorf("Questions = %+v, want the one replacement question", got.Questions)
	}
	if !got.UpdatedAt.Equal(base.Add(time.Hour)) || !got.CreatedAt.Equal(base) {
		t.Errorf("timestamps = %v / %v", got.CreatedAt, got.UpdatedAt)
	}
	if _, err := repo.UpdateSurvey(context.Background(), 999, def, base); !errors.Is(err, surveys.ErrNotFound) {
		t.Fatalf("UpdateSurvey(999) err = %v", err)
	}
}

func answerFor(id int64, text string) surveys.Answer {
	return surveys.Answer{QuestionID: id, QuestionType: "text", QuestionLabel: "q", Answer: text}
}

func mustCreateResponse(t *testing.T, repo surveys.Repository, surveyID int64, publicID string, answers ...surveys.Answer) surveys.Response {
	t.Helper()
	r, err := repo.CreateResponse(context.Background(), surveys.NewResponse{
		SurveyID: surveyID, PublicID: publicID, UserIdentifier: "device-1", Answers: answers,
	}, base)
	if err != nil {
		t.Fatalf("CreateResponse: %v", err)
	}
	return r
}

//nolint:unparam // newStore is unused only because t.Skip below short-circuits the body; Task 6 removes the skip, making it used again
func testUpdateFreezesQuestions(t *testing.T, newStore newSurveyStoreFunc) {
	t.Skip("Task 6")
	t.Parallel()
	repo, regs := newStore(t)
	st := seedStudy(t, repo, regs, 1)
	s := mustCreateSurvey(t, repo, st.ID, surveyDef("v1"))
	mustCreateResponse(t, repo, s.ID, "resp-1", answerFor(s.Questions[0].ID, "Good"))
	def := surveyDef("v1")
	def.Questions[0].Content.Options = []string{"Good", "Meh"}
	if err := def.Validate(); err != nil {
		t.Fatal(err)
	}
	_, err := repo.UpdateSurvey(context.Background(), s.ID, def, base.Add(time.Hour))
	if !errors.Is(err, surveys.ErrQuestionsFrozen) {
		t.Fatalf("err = %v, want ErrQuestionsFrozen", err)
	}
	got, err := repo.GetSurvey(context.Background(), s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Questions[0].ID != s.Questions[0].ID || !reflect.DeepEqual(got.Questions[0].Content.Options, []string{"Good", "Bad"}) {
		t.Fatalf("frozen survey changed: %+v", got.Questions[0])
	}
}

//nolint:unparam // newStore is unused only because t.Skip below short-circuits the body; Task 6 removes the skip, making it used again
func testUpdateScalarsOnFrozen(t *testing.T, newStore newSurveyStoreFunc) {
	t.Skip("Task 6")
	t.Parallel()
	repo, regs := newStore(t)
	st := seedStudy(t, repo, regs, 1)
	s := mustCreateSurvey(t, repo, st.ID, surveyDef("v1"))
	mustCreateResponse(t, repo, s.ID, "resp-1", answerFor(s.Questions[0].ID, "Good"))
	def := surveyDef("renamed")
	def.Available = false
	if err := def.Validate(); err != nil {
		t.Fatal(err)
	}
	got, err := repo.UpdateSurvey(context.Background(), s.ID, def, base.Add(time.Hour))
	if err != nil {
		t.Fatalf("scalar-only edit of a frozen survey: %v", err)
	}
	if got.Name != "renamed" || got.Available {
		t.Errorf("scalars = %+v", got)
	}
	if got.Questions[0].ID != s.Questions[0].ID || got.Questions[1].ID != s.Questions[1].ID {
		t.Errorf("question ids changed on a scalar-only edit: %+v vs %+v", got.Questions, s.Questions)
	}
}

//nolint:unparam // newStore is unused only because t.Skip below short-circuits the body; Task 6 removes the skip, making it used again
func testDeleteRefusesWithResponses(t *testing.T, newStore newSurveyStoreFunc) {
	t.Skip("Task 6")
	t.Parallel()
	repo, regs := newStore(t)
	st := seedStudy(t, repo, regs, 1)
	s := mustCreateSurvey(t, repo, st.ID, surveyDef("v1"))
	mustCreateResponse(t, repo, s.ID, "resp-1")
	if err := repo.DeleteSurvey(context.Background(), s.ID); !errors.Is(err, surveys.ErrHasResponses) {
		t.Fatalf("err = %v, want ErrHasResponses", err)
	}
	if _, err := repo.GetSurvey(context.Background(), s.ID); err != nil {
		t.Fatalf("survey gone after refused delete: %v", err)
	}
	if err := repo.DeleteSurvey(context.Background(), 999); !errors.Is(err, surveys.ErrNotFound) {
		t.Fatalf("DeleteSurvey(999) err = %v", err)
	}
}

func testDeleteCascadesQuestions(t *testing.T, newStore newSurveyStoreFunc) {
	t.Parallel()
	repo, regs := newStore(t)
	st := seedStudy(t, repo, regs, 1)
	s := mustCreateSurvey(t, repo, st.ID, surveyDef("v1"))
	other := mustCreateSurvey(t, repo, st.ID, surveyDef("other"))
	if err := repo.DeleteSurvey(context.Background(), s.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.GetSurvey(context.Background(), s.ID); !errors.Is(err, surveys.ErrNotFound) {
		t.Fatalf("after delete err = %v", err)
	}
	got, err := repo.GetSurvey(context.Background(), other.ID)
	if err != nil || len(got.Questions) != 2 {
		t.Fatalf("sibling survey affected: %+v, %v", got, err)
	}
}

//nolint:unparam // newStore is unused only because t.Skip below short-circuits the body; Task 6 removes the skip, making it used again
func testCountResponses(t *testing.T, newStore newSurveyStoreFunc) {
	t.Skip("Task 6")
	t.Parallel()
	repo, regs := newStore(t)
	st := seedStudy(t, repo, regs, 1)
	s := mustCreateSurvey(t, repo, st.ID, surveyDef("v1"))
	n, err := repo.CountResponses(context.Background(), s.ID)
	if err != nil || n != 0 {
		t.Fatalf("count = %d, %v", n, err)
	}
	mustCreateResponse(t, repo, s.ID, "a")
	mustCreateResponse(t, repo, s.ID, "b")
	if n, err = repo.CountResponses(context.Background(), s.ID); err != nil || n != 2 {
		t.Fatalf("count = %d, %v, want 2", n, err)
	}
}

func testResponseCreateGet(t *testing.T, _ newSurveyStoreFunc)           { t.Skip("Task 6") }
func testResponseCreateUnknownSurvey(t *testing.T, _ newSurveyStoreFunc) { t.Skip("Task 6") }
func testAmendMerges(t *testing.T, _ newSurveyStoreFunc)                 { t.Skip("Task 6") }
func testAmendNotFoundAndCap(t *testing.T, _ newSurveyStoreFunc)         { t.Skip("Task 6") }
func testConcurrentAmendsBothLand(t *testing.T, _ newSurveyStoreFunc)    { t.Skip("Task 6") }
func testListResponsesOrdered(t *testing.T, _ newSurveyStoreFunc)        { t.Skip("Task 6") }
