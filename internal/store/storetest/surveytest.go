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

const (
	deviceID        = "device-1"
	responseID      = "resp-1"
	errWantNotFound = "err = %v, want ErrNotFound"
	timestampsFmt   = "timestamps = %v / %v"
)

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
	t.Run("UpdateKeepsQuestionIDsWhenUnchanged", func(t *testing.T) { testUpdateKeepsQuestionIDsWhenUnchanged(t, newStore) })
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
	t.Run("UpdateStudyIsRegionScoped", func(t *testing.T) { testUpdateStudyRegionScoped(t, newStore) })
	t.Run("CreateSurveyInRegionRejectsForeignStudy", func(t *testing.T) { testCreateSurveyInRegion(t, newStore) })
	t.Run("GetResponseInRegionIsScoped", func(t *testing.T) { testGetResponseInRegion(t, newStore) })
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

// seedSurveyRegions puts regions 0 and 1 in place for the region-scoping
// subtests below. Region 0 is a real region (Tampa Bay) and deliberately
// holds the "home" data in those tests, so an id-only lookup that forgot
// its region condition would not be saved by a coincidentally-nonzero id.
func seedSurveyRegions(t *testing.T, regs regions.Repository) {
	t.Helper()
	putStoretestRegion(t, regs, 0)
	putStoretestRegion(t, regs, 1)
}

// minimalQuestionContent is the smallest surveys.Content that
// Content.Validate accepts: a plain text question needs only a type and a
// non-blank label.
func minimalQuestionContent(t *testing.T) surveys.Content {
	t.Helper()
	return surveys.Content{Type: surveys.TypeText, LabelText: "Q"}
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
		t.Fatalf("GetStudy(999) "+errWantNotFound, err)
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
	assertSurveyScalars(t, got, st, start, end)
	assertSurveyQuestions(t, got, def)
	if _, err := repo.GetSurvey(context.Background(), 999); !errors.Is(err, surveys.ErrNotFound) {
		t.Fatalf("GetSurvey(999) "+errWantNotFound, err)
	}
}

// assertSurveyScalars checks the non-question fields of a survey created
// from surveyDef with a window and every flag set.
func assertSurveyScalars(t *testing.T, got surveys.Survey, st surveys.Study, start, end time.Time) {
	t.Helper()
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
	if !got.CreatedAt.Equal(base) || got.CreatedAt.Location() != time.UTC {
		t.Errorf("CreatedAt = %v (%v)", got.CreatedAt, got.CreatedAt.Location())
	}
}

// assertSurveyQuestions checks that the stored questions match def's in
// order, content, and the per-type Required rule, with distinct ids.
func assertSurveyQuestions(t *testing.T, got surveys.Survey, def surveys.Definition) {
	t.Helper()
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
		t.Fatalf(errWantNotFound, err)
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
		t.Errorf(timestampsFmt, got.CreatedAt, got.UpdatedAt)
	}
	if _, err := repo.UpdateSurvey(context.Background(), 999, def, base); !errors.Is(err, surveys.ErrNotFound) {
		t.Fatalf("UpdateSurvey(999) err = %v", err)
	}
}

// testUpdateKeepsQuestionIDsWhenUnchanged pins finding 6: even with zero
// responses, an edit whose questions are identical to the stored set (same
// order, required, and content -- only the name differs) must not
// renumber them. Before the fix, UpdateSurvey replaced questions
// unconditionally whenever responses == 0, so this scalar-only edit would
// silently mint new question ids.
func testUpdateKeepsQuestionIDsWhenUnchanged(t *testing.T, newStore newSurveyStoreFunc) {
	t.Parallel()
	repo, regs := newStore(t)
	st := seedStudy(t, repo, regs, 1)
	s := mustCreateSurvey(t, repo, st.ID, surveyDef("v1"))
	def := surveyDef("renamed") // same questions as surveyDef("v1"); only the name changes
	if err := def.Validate(); err != nil {
		t.Fatal(err)
	}
	got, err := repo.UpdateSurvey(context.Background(), s.ID, def, base.Add(time.Hour))
	if err != nil {
		t.Fatalf("scalar-only edit with no responses: %v", err)
	}
	if got.Name != "renamed" {
		t.Errorf("Name = %q, want renamed", got.Name)
	}
	if len(got.Questions) != len(s.Questions) {
		t.Fatalf("Questions = %+v, want %d unchanged questions", got.Questions, len(s.Questions))
	}
	for i, q := range got.Questions {
		if q.ID != s.Questions[i].ID {
			t.Errorf("question %d id = %d, want unchanged id %d", i, q.ID, s.Questions[i].ID)
		}
	}
}

func answerFor(id int64, text string) surveys.Answer {
	return surveys.Answer{QuestionID: id, QuestionType: "text", QuestionLabel: "q", Answer: text}
}

func mustCreateResponse(t *testing.T, repo surveys.Repository, surveyID int64, publicID string, answers ...surveys.Answer) surveys.Response {
	t.Helper()
	r, err := repo.CreateResponse(context.Background(), surveys.NewResponse{
		SurveyID: surveyID, PublicID: publicID, UserIdentifier: deviceID, Answers: answers,
	}, base)
	if err != nil {
		t.Fatalf("CreateResponse: %v", err)
	}
	return r
}

func testUpdateFreezesQuestions(t *testing.T, newStore newSurveyStoreFunc) {
	t.Parallel()
	repo, regs := newStore(t)
	st := seedStudy(t, repo, regs, 1)
	s := mustCreateSurvey(t, repo, st.ID, surveyDef("v1"))
	mustCreateResponse(t, repo, s.ID, responseID, answerFor(s.Questions[0].ID, "Good"))
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

func testUpdateScalarsOnFrozen(t *testing.T, newStore newSurveyStoreFunc) {
	t.Parallel()
	repo, regs := newStore(t)
	st := seedStudy(t, repo, regs, 1)
	s := mustCreateSurvey(t, repo, st.ID, surveyDef("v1"))
	mustCreateResponse(t, repo, s.ID, responseID, answerFor(s.Questions[0].ID, "Good"))
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

func testDeleteRefusesWithResponses(t *testing.T, newStore newSurveyStoreFunc) {
	t.Parallel()
	repo, regs := newStore(t)
	st := seedStudy(t, repo, regs, 1)
	s := mustCreateSurvey(t, repo, st.ID, surveyDef("v1"))
	mustCreateResponse(t, repo, s.ID, responseID)
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

func testCountResponses(t *testing.T, newStore newSurveyStoreFunc) {
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

func testResponseCreateGet(t *testing.T, newStore newSurveyStoreFunc) {
	t.Parallel()
	repo, regs := newStore(t)
	st := seedStudy(t, repo, regs, 1)
	s := mustCreateSurvey(t, repo, st.ID, surveyDef("v1"))
	lat, lon := 47.6, -122.3
	in := surveys.NewResponse{
		SurveyID: s.ID, PublicID: "pub-1", UserIdentifier: deviceID,
		StopIdentifier: "1_570", StopLatitude: &lat, StopLongitude: &lon,
		Answers: []surveys.Answer{{QuestionID: s.Questions[0].ID, QuestionType: "radio", QuestionLabel: "Trip?", Answer: "[Bus, Train]"}},
	}
	created, err := repo.CreateResponse(context.Background(), in, base)
	if err != nil {
		t.Fatal(err)
	}
	got, err := repo.GetResponse(context.Background(), "pub-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != created.ID || got.SurveyID != s.ID || got.PublicID != "pub-1" || got.UserIdentifier != deviceID ||
		got.StopIdentifier != "1_570" || got.StopLatitude == nil || *got.StopLatitude != lat ||
		got.StopLongitude == nil || *got.StopLongitude != lon {
		t.Errorf("GetResponse = %+v", got)
	}
	if !reflect.DeepEqual(got.Answers, in.Answers) {
		t.Errorf("Answers = %+v, want %+v stored verbatim", got.Answers, in.Answers)
	}
	if !got.CreatedAt.Equal(base) || !got.UpdatedAt.Equal(base) {
		t.Errorf(timestampsFmt, got.CreatedAt, got.UpdatedAt)
	}
	// Absent stop fields round-trip as absent, not zero (Android sends
	// 0.0 coordinates *with no identifier*; the two cases must stay apart).
	bare := mustCreateResponse(t, repo, s.ID, "pub-2")
	gotBare, err := repo.GetResponse(context.Background(), bare.PublicID)
	if err != nil {
		t.Fatal(err)
	}
	if gotBare.StopIdentifier != "" || gotBare.StopLatitude != nil || gotBare.StopLongitude != nil {
		t.Errorf("bare response stop fields = %q %v %v, want absent", gotBare.StopIdentifier, gotBare.StopLatitude, gotBare.StopLongitude)
	}
	if gotBare.Answers == nil || len(gotBare.Answers) != 0 {
		t.Errorf("bare Answers = %#v, want empty non-nil", gotBare.Answers)
	}
	if _, err := repo.GetResponse(context.Background(), "nope"); !errors.Is(err, surveys.ErrNotFound) {
		t.Fatalf("GetResponse(nope) err = %v", err)
	}
}

func testResponseCreateUnknownSurvey(t *testing.T, newStore newSurveyStoreFunc) {
	t.Parallel()
	repo, _ := newStore(t)
	_, err := repo.CreateResponse(context.Background(), surveys.NewResponse{SurveyID: 999, PublicID: "p", UserIdentifier: "d"}, base)
	if !errors.Is(err, surveys.ErrNotFound) {
		t.Fatalf(errWantNotFound, err)
	}
}

func testAmendMerges(t *testing.T, newStore newSurveyStoreFunc) {
	t.Parallel()
	repo, regs := newStore(t)
	st := seedStudy(t, repo, regs, 1)
	s := mustCreateSurvey(t, repo, st.ID, surveyDef("v1"))
	mustCreateResponse(t, repo, s.ID, "pub-1", answerFor(1, "a"), answerFor(2, "b"))
	got, err := repo.AmendResponse(context.Background(), "pub-1", []surveys.Answer{answerFor(2, "B"), answerFor(3, "c")}, base.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	want := []surveys.Answer{answerFor(1, "a"), answerFor(2, "B"), answerFor(3, "c")}
	if !reflect.DeepEqual(got.Answers, want) {
		t.Fatalf("Answers = %+v, want %+v", got.Answers, want)
	}
	if !got.UpdatedAt.Equal(base.Add(time.Minute)) || !got.CreatedAt.Equal(base) {
		t.Errorf(timestampsFmt, got.CreatedAt, got.UpdatedAt)
	}
	// An empty amend is a no-op that still bumps updated_at (design spec 4.4).
	again, err := repo.AmendResponse(context.Background(), "pub-1", nil, base.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(again.Answers, want) || !again.UpdatedAt.Equal(base.Add(2*time.Minute)) {
		t.Errorf("empty amend = %+v", again)
	}
}

func testAmendNotFoundAndCap(t *testing.T, newStore newSurveyStoreFunc) {
	t.Parallel()
	repo, regs := newStore(t)
	st := seedStudy(t, repo, regs, 1)
	s := mustCreateSurvey(t, repo, st.ID, surveyDef("v1"))
	if _, err := repo.AmendResponse(context.Background(), "nope", []surveys.Answer{answerFor(1, "a")}, base); !errors.Is(err, surveys.ErrNotFound) {
		t.Fatalf(errWantNotFound, err)
	}
	stored := make([]surveys.Answer, surveys.MaxAnswers)
	for i := range stored {
		stored[i] = answerFor(int64(i+1), "x")
	}
	mustCreateResponse(t, repo, s.ID, "full", stored...)
	if _, err := repo.AmendResponse(context.Background(), "full", []surveys.Answer{answerFor(1, "replaced")}, base); err != nil {
		t.Fatalf("replacing within the cap: %v", err)
	}
	_, err := repo.AmendResponse(context.Background(), "full", []surveys.Answer{answerFor(surveys.MaxAnswers+1, "new")}, base)
	if !errors.Is(err, surveys.ErrTooManyAnswers) {
		t.Fatalf("err = %v, want ErrTooManyAnswers", err)
	}
}

// testConcurrentAmendsBothLand is the reason sqlite.Open sets
// _txlock=immediate (design spec 2.6, 3.2): two amends racing on one row
// must both succeed and both be visible. Mutation to trust this test:
// remove _txlock=immediate from the DSN and it fails with SQLITE_BUSY.
func testConcurrentAmendsBothLand(t *testing.T, newStore newSurveyStoreFunc) {
	t.Parallel()
	const attempts = 25
	for i := range attempts {
		repo, regs := newStore(t)
		st := seedStudy(t, repo, regs, 1)
		s := mustCreateSurvey(t, repo, st.ID, surveyDef("v1"))
		mustCreateResponse(t, repo, s.ID, "pub-1", answerFor(1, "hero"))

		var errA, errB error
		start := make(chan struct{})
		done := make(chan struct{}, 2)
		go func() {
			<-start
			_, errA = repo.AmendResponse(context.Background(), "pub-1", []surveys.Answer{answerFor(2, "a")}, base)
			done <- struct{}{}
		}()
		go func() {
			<-start
			_, errB = repo.AmendResponse(context.Background(), "pub-1", []surveys.Answer{answerFor(3, "b")}, base)
			done <- struct{}{}
		}()
		close(start)
		<-done
		<-done
		if errA != nil || errB != nil {
			t.Fatalf("attempt %d: concurrent amends must both succeed: a=%v b=%v", i, errA, errB)
		}
		got, err := repo.GetResponse(context.Background(), "pub-1")
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Answers) != 3 {
			t.Fatalf("attempt %d: answers = %+v, want hero + both amends (lost update)", i, got.Answers)
		}
	}
}

func testListResponsesOrdered(t *testing.T, newStore newSurveyStoreFunc) {
	t.Parallel()
	repo, regs := newStore(t)
	st := seedStudy(t, repo, regs, 1)
	s := mustCreateSurvey(t, repo, st.ID, surveyDef("v1"))
	other := mustCreateSurvey(t, repo, st.ID, surveyDef("other"))
	later, err := repo.CreateResponse(context.Background(), surveys.NewResponse{SurveyID: s.ID, PublicID: "later", UserIdentifier: "d"}, base.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	earlier := mustCreateResponse(t, repo, s.ID, "earlier")
	mustCreateResponse(t, repo, other.ID, "elsewhere")
	list, err := repo.ListResponses(context.Background(), s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != earlier.ID || list[1].ID != later.ID {
		t.Fatalf("ListResponses = %+v, want [earlier, later] by created_at", list)
	}
}

// testUpdateStudyRegionScoped pins design spec 3.2: the region is a query
// condition on the update itself, so a study addressed through the wrong
// region is ErrNotFound and the row is left untouched, not merely refused
// after a Go-side comparison.
func testUpdateStudyRegionScoped(t *testing.T, newStore newSurveyStoreFunc) {
	repo, regionRepo := newStore(t)
	ctx := context.Background()
	seedSurveyRegions(t, regionRepo) // reuse this file's existing seeder

	inA, err := repo.CreateStudy(ctx, 0, "A", "first", base)
	if err != nil {
		t.Fatalf("CreateStudy: %v", err)
	}
	at := base.Add(time.Hour)
	updated, err := repo.UpdateStudy(ctx, 0, inA.ID, "A2", "second", at)
	if err != nil {
		t.Fatalf("UpdateStudy: %v", err)
	}
	if updated.Name != "A2" || updated.Description != "second" {
		t.Errorf("updated = %+v, want A2/second", updated)
	}
	if !updated.UpdatedAt.Equal(at) {
		t.Errorf("UpdatedAt = %v, want %v", updated.UpdatedAt, at)
	}
	if !updated.CreatedAt.Equal(base) {
		t.Errorf("CreatedAt moved: %v, want %v", updated.CreatedAt, base)
	}

	// The same study, addressed through the wrong region, must not update
	// and must not report success.
	if _, updateErr := repo.UpdateStudy(ctx, 1, inA.ID, "hijacked", "", at); !errors.Is(updateErr, surveys.ErrNotFound) {
		t.Fatalf("cross-region UpdateStudy: err = %v, want ErrNotFound", updateErr)
	}
	after, err := repo.GetStudy(ctx, inA.ID)
	if err != nil {
		t.Fatalf("GetStudy: %v", err)
	}
	if after.Name != "A2" {
		t.Errorf("a refused update still wrote: name = %q", after.Name)
	}
}

// testCreateSurveyInRegion pins design spec 3.2 for a body-borne id: the
// study's region is a JOIN condition on the create, so a study_id from
// another region -- exactly what POST /regions/1/surveys {"study_id":
// <region 0's>} would send -- is ErrNotFound and writes nothing.
func testCreateSurveyInRegion(t *testing.T, newStore newSurveyStoreFunc) {
	repo, regionRepo := newStore(t)
	ctx := context.Background()
	seedSurveyRegions(t, regionRepo)

	studyA, err := repo.CreateStudy(ctx, 0, "A", "", base)
	if err != nil {
		t.Fatalf("CreateStudy: %v", err)
	}
	def := surveys.Definition{
		Name: "Ride quality", Available: true,
		Questions: []surveys.QuestionDefinition{{Content: minimalQuestionContent(t)}},
	}

	created, err := repo.CreateSurveyInRegion(ctx, 0, studyA.ID, def, base)
	if err != nil {
		t.Fatalf("CreateSurveyInRegion: %v", err)
	}
	if created.StudyID != studyA.ID {
		t.Errorf("StudyID = %d, want %d", created.StudyID, studyA.ID)
	}

	// A study_id from another region is ErrNotFound, decided by the join --
	// this is what stops POST /regions/1/surveys {"study_id": <region 0's>}.
	if _, createErr := repo.CreateSurveyInRegion(ctx, 1, studyA.ID, def, base); !errors.Is(createErr, surveys.ErrNotFound) {
		t.Errorf("foreign study: err = %v, want ErrNotFound", createErr)
	}
	if _, createErr := repo.CreateSurveyInRegion(ctx, 0, 99999, def, base); !errors.Is(createErr, surveys.ErrNotFound) {
		t.Errorf("unknown study: err = %v, want ErrNotFound", createErr)
	}
	list, err := repo.ListSurveys(ctx, 1)
	if err != nil {
		t.Fatalf("ListSurveys: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("region 1 gained %d surveys from a refused create", len(list))
	}
}

// testGetResponseInRegion pins design spec 3.2 for a response reached
// through its survey's study's region in a single query.
func testGetResponseInRegion(t *testing.T, newStore newSurveyStoreFunc) {
	repo, regionRepo := newStore(t)
	ctx := context.Background()
	seedSurveyRegions(t, regionRepo)

	study, err := repo.CreateStudy(ctx, 0, "A", "", base)
	if err != nil {
		t.Fatalf("CreateStudy: %v", err)
	}
	survey, err := repo.CreateSurvey(ctx, study.ID, surveys.Definition{
		Name: "s", Available: true,
		Questions: []surveys.QuestionDefinition{{Content: minimalQuestionContent(t)}},
	}, base)
	if err != nil {
		t.Fatalf("CreateSurvey: %v", err)
	}
	resp, err := repo.CreateResponse(ctx, surveys.NewResponse{
		SurveyID: survey.ID, PublicID: "pub-1", UserIdentifier: "rider-1",
	}, base)
	if err != nil {
		t.Fatalf("CreateResponse: %v", err)
	}

	got, err := repo.GetResponseInRegion(ctx, 0, "pub-1")
	if err != nil {
		t.Fatalf("GetResponseInRegion: %v", err)
	}
	if got.ID != resp.ID || got.UserIdentifier != "rider-1" {
		t.Errorf("got = %+v, want response %d", got, resp.ID)
	}
	// Reaching a response through another region's survey is the case the
	// handler tests mirror at the HTTP layer.
	if _, getErr := repo.GetResponseInRegion(ctx, 1, "pub-1"); !errors.Is(getErr, surveys.ErrNotFound) {
		t.Errorf("across regions: err = %v, want ErrNotFound", getErr)
	}
	if _, getErr := repo.GetResponseInRegion(ctx, 0, "nope"); !errors.Is(getErr, surveys.ErrNotFound) {
		t.Errorf("unknown public id: err = %v, want ErrNotFound", getErr)
	}
}
