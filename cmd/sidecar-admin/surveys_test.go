package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/surveys"
)

const surveyDoc = `{
  "name": "Rider satisfaction",
  "start_date": "2026-09-01T00:00:00-07:00",
  "end_date": "2026-09-30T23:59:00-07:00",
  "show_on_stops": true,
  "visible_stop_list": ["1_570", " 1_578 "],
  "questions": [
    {"required": true, "content": {"type": "radio", "label_text": "How was your trip?", "options": ["Great", "Bad"]}},
    {"content": {"type": "label", "label_text": "Thanks"}}
  ]
}`

func writeDoc(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "survey.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func createStudy(t *testing.T, dbPath string) int64 {
	t.Helper()
	stdout, _, err := cli(t, dbPath, "study", "create", "--region", "1", "--name", "World Cup", "--description", "From ST")
	if err != nil {
		t.Fatalf("study create: %v", err)
	}
	var id int64
	if _, err := parseLine(stdout, "created study %d", &id); err != nil {
		t.Fatalf("parse %q: %v", stdout, err)
	}
	return id
}

func createSurvey(t *testing.T, dbPath string, studyID int64, doc string) int64 {
	t.Helper()
	stdout, _, err := cli(t, dbPath, "survey", "create", "--study", itoa(studyID), "--file", writeDoc(t, doc))
	if err != nil {
		t.Fatalf("survey create: %v", err)
	}
	var id int64
	if _, err := parseLine(stdout, "created survey %d", &id); err != nil {
		t.Fatalf("parse %q: %v", stdout, err)
	}
	return id
}

func TestStudyCreateAndList(t *testing.T) {
	t.Parallel()
	dbPath, store := newDB(t)
	seedRegion(t, store.Regions(), 1)
	id := createStudy(t, dbPath)
	stdout, _, err := cli(t, dbPath, "study", "list", "--region", "1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, itoa(id)) || !strings.Contains(stdout, "World Cup") {
		t.Errorf("study list = %q", stdout)
	}
	if _, _, err := cli(t, dbPath, "study", "create", "--region", "1", "--name", " "); err == nil || !strings.Contains(err.Error(), "name") {
		t.Errorf("blank name: err = %v", err)
	}
	if _, _, err := cli(t, dbPath, "study", "create", "--region", "9", "--name", "x"); err == nil {
		t.Error("unknown region accepted")
	}
}

func TestSurveyCreateAppearsInRiderList(t *testing.T) {
	t.Parallel()
	dbPath, store := newDB(t)
	seedRegion(t, store.Regions(), 1)
	st := createStudy(t, dbPath)
	id := createSurvey(t, dbPath, st, surveyDoc)

	got, err := store.Surveys().GetSurvey(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "Rider satisfaction" || !got.Available || !got.ShowOnStops || got.ShowOnMap {
		t.Errorf("survey = %+v", got)
	}
	if got.StartTime == nil || got.StartTime.UTC().Format("2006-01-02T15:04:05Z") != "2026-09-01T07:00:00Z" {
		t.Errorf("StartTime = %v, want 07:00Z (explicit -07:00 offset honored)", got.StartTime)
	}
	if len(got.VisibleStopList) != 2 || got.VisibleStopList[1] != "1_578" {
		t.Errorf("VisibleStopList = %v, want trimmed", got.VisibleStopList)
	}
	if len(got.Questions) != 2 || got.Questions[1].Required {
		t.Errorf("questions = %+v", got.Questions)
	}
	stdout, _, err := cli(t, dbPath, "survey", "list", "--region", "1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "responses=0") || !strings.Contains(stdout, "available=true") {
		t.Errorf("survey list = %q", stdout)
	}
}

func TestSurveyCreateRejectsBadDocuments(t *testing.T) {
	t.Parallel()
	dbPath, store := newDB(t)
	seedRegion(t, store.Regions(), 1)
	st := createStudy(t, dbPath)
	tests := []struct{ name, doc, wantErr string }{
		{"half window", `{"name":"x","start_date":"2026-09-01T00:00:00-07:00"}`, "both start_date and end_date"},
		{"naive date", `{"name":"x","start_date":"2026-09-01T00:00:00","end_date":"2026-09-02T00:00:00"}`, "explicit offset"},
		{"blank option", `{"name":"x","questions":[{"content":{"type":"radio","label_text":"q","options":["a",""]}}]}`, "blank option"},
		{"schemeless url", `{"name":"x","questions":[{"content":{"type":"external_survey","label_text":"q","url":"example.org"}}]}`, "absolute http(s)"},
		{"unknown key", `{"name":"x","require_stop_id":true}`, "unknown field"},
		{"not json", `{`, "parse"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, _, err := cli(t, dbPath, "survey", "create", "--study", itoa(st), "--file", writeDoc(t, tt.doc))
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
	list, err := store.Surveys().ListSurveys(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("rejected documents persisted %d surveys", len(list))
	}
}

func TestSurveyShowEditRoundTrip(t *testing.T) {
	t.Parallel()
	dbPath, store := newDB(t)
	seedRegion(t, store.Regions(), 1)
	st := createStudy(t, dbPath)
	id := createSurvey(t, dbPath, st, surveyDoc)

	shown, _, err := cli(t, dbPath, "survey", "show", itoa(id))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err = json.Unmarshal([]byte(shown), &doc); err != nil {
		t.Fatalf("show is not JSON: %v\n%s", err, shown)
	}
	for _, k := range []string{"id", "study", "created_at", "updated_at", "available", "questions"} {
		if _, ok := doc[k]; !ok {
			t.Errorf("show output missing %q", k)
		}
	}
	before, err := store.Surveys().GetSurvey(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	// Feed show's output straight back through edit via stdin: an identity
	// round trip must leave the survey unchanged.
	stdout, _, err := cliStdin(t, strings.NewReader(shown), dbPath, "survey", "edit", itoa(id), "--file", "-")
	if err != nil {
		t.Fatalf("edit from show output: %v (%s)", err, stdout)
	}
	roundTripped, err := store.Surveys().GetSurvey(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	assertSurveyRoundTrips(t, before, roundTripped)

	edited := strings.Replace(shown, `"Rider satisfaction"`, `"Renamed"`, 1)
	if _, _, err = cliStdin(t, strings.NewReader(edited), dbPath, "survey", "edit", itoa(id), "--file", "-"); err != nil {
		t.Fatal(err)
	}
	after, err := store.Surveys().GetSurvey(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if after.Name != "Renamed" || len(after.Questions) != 2 {
		t.Errorf("after edit = %+v", after)
	}
}

// assertSurveyRoundTrips checks that after -- produced by feeding show's own
// output straight back through edit -- is unchanged from before on every
// field the authoring document carries. Question ids may legitimately
// differ even on an identity edit, since edit replaces the question set
// wholesale (design spec 2.13) rather than diffing it; identity is asserted
// on question content and required-ness, not database ids.
func assertSurveyRoundTrips(t *testing.T, before, after surveys.Survey) {
	t.Helper()
	if after.Name != before.Name || after.Available != before.Available ||
		after.ShowOnMap != before.ShowOnMap || after.ShowOnStops != before.ShowOnStops ||
		after.AlwaysVisible != before.AlwaysVisible || after.AllowsMultipleResponses != before.AllowsMultipleResponses {
		t.Errorf("round trip changed scalars: before = %+v, after = %+v", before, after)
	}
	if before.StartTime == nil || after.StartTime == nil || !after.StartTime.Equal(*before.StartTime) {
		t.Errorf("StartTime = %v, want %v", after.StartTime, before.StartTime)
	}
	if before.EndTime == nil || after.EndTime == nil || !after.EndTime.Equal(*before.EndTime) {
		t.Errorf("EndTime = %v, want %v", after.EndTime, before.EndTime)
	}
	if !reflect.DeepEqual(after.VisibleStopList, before.VisibleStopList) {
		t.Errorf("VisibleStopList = %v, want %v", after.VisibleStopList, before.VisibleStopList)
	}
	if !reflect.DeepEqual(after.VisibleRouteList, before.VisibleRouteList) {
		t.Errorf("VisibleRouteList = %v, want %v", after.VisibleRouteList, before.VisibleRouteList)
	}
	if len(after.Questions) != len(before.Questions) {
		t.Fatalf("Questions = %d, want %d", len(after.Questions), len(before.Questions))
	}
	for i := range before.Questions {
		if after.Questions[i].Required != before.Questions[i].Required ||
			!surveys.ContentEqual(after.Questions[i].Content, before.Questions[i].Content) {
			t.Errorf("question %d = %+v, want %+v", i, after.Questions[i], before.Questions[i])
		}
	}
}

func TestSurveyEditFrozenAndDelete(t *testing.T) {
	t.Parallel()
	dbPath, store := newDB(t)
	seedRegion(t, store.Regions(), 1)
	st := createStudy(t, dbPath)
	id := createSurvey(t, dbPath, st, surveyDoc)
	s, _ := store.Surveys().GetSurvey(context.Background(), id)
	if _, err := store.Surveys().CreateResponse(context.Background(), surveys.NewResponse{
		SurveyID: id, PublicID: "p1", UserIdentifier: "d", Answers: []surveys.Answer{{QuestionID: s.Questions[0].ID, Answer: "Great"}},
	}, s.CreatedAt); err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(surveyDoc, `"Great", "Bad"`, `"Great", "Meh"`, 1)
	_, _, err := cli(t, dbPath, "survey", "edit", itoa(id), "--file", writeDoc(t, changed))
	if err == nil || !strings.Contains(err.Error(), "frozen") {
		t.Fatalf("edit of answered survey's questions: err = %v, want frozen refusal", err)
	}
	renamed := strings.Replace(surveyDoc, `"Rider satisfaction"`, `"Renamed"`, 1)
	if _, _, err = cli(t, dbPath, "survey", "edit", itoa(id), "--file", writeDoc(t, renamed)); err != nil {
		t.Fatalf("scalar edit of answered survey: %v", err)
	}
	_, _, err = cli(t, dbPath, "survey", "delete", itoa(id))
	if err == nil || !strings.Contains(err.Error(), "1 response") {
		t.Fatalf("delete with responses: err = %v, want refusal naming the count", err)
	}
	empty := createSurvey(t, dbPath, st, `{"name":"empty"}`)
	if _, _, err := cli(t, dbPath, "survey", "delete", itoa(empty)); err != nil {
		t.Fatalf("delete without responses: %v", err)
	}
	if _, _, err := cli(t, dbPath, "survey", "delete", itoa(empty)); err == nil {
		t.Fatal("second delete succeeded")
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func parseLine(s, format string, id *int64) (int, error) {
	return fmt.Sscanf(strings.TrimSpace(s), format, id)
}

func TestSurveyResponsesCSV(t *testing.T) {
	t.Parallel()
	dbPath, store := newDB(t)
	seedRegion(t, store.Regions(), 1)
	st := createStudy(t, dbPath)
	id := createSurvey(t, dbPath, st, surveyDoc)
	s, _ := store.Surveys().GetSurvey(context.Background(), id)
	lat, lon := 47.6, -122.3
	ctx := context.Background()
	if _, err := store.Surveys().CreateResponse(ctx, surveys.NewResponse{
		SurveyID: id, PublicID: "two-answers", UserIdentifier: "dev-1", StopIdentifier: "1_570", StopLatitude: &lat, StopLongitude: &lon,
		Answers: []surveys.Answer{
			{QuestionID: s.Questions[0].ID, QuestionType: "radio", QuestionLabel: "How was your trip?", Answer: "Great"},
			{QuestionID: 77, QuestionType: "checkbox", QuestionLabel: "Modes", Answer: "[Bus, Train]"},
		},
	}, s.CreatedAt); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Surveys().CreateResponse(ctx, surveys.NewResponse{SurveyID: id, PublicID: "abandoned", UserIdentifier: "dev-2"}, s.CreatedAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	stdout, _, err := cli(t, dbPath, "survey", "responses", itoa(id))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(strings.NewReader(stdout)).ReadAll()
	if err != nil {
		t.Fatalf("not CSV: %v\n%s", err, stdout)
	}
	wantHeader := "response_id,user_identifier,stop_identifier,stop_latitude,stop_longitude,created_at,updated_at,question_id,question_type,question_label,answer"
	if got := strings.Join(rows[0], ","); got != wantHeader {
		t.Fatalf("header = %s", got)
	}
	if len(rows) != 4 {
		t.Fatalf("rows = %d (%v), want header + 2 answers + 1 abandoned", len(rows), rows)
	}
	if rows[1][0] != "two-answers" || rows[1][2] != "1_570" || rows[1][3] != "47.6" || rows[1][10] != "Great" {
		t.Errorf("row 1 = %v", rows[1])
	}
	if rows[2][7] != "77" || rows[2][10] != "[Bus, Train]" {
		t.Errorf("row 2 = %v", rows[2])
	}
	if rows[3][0] != "abandoned" || rows[3][2] != "" || rows[3][3] != "" || rows[3][7] != "" || rows[3][10] != "" {
		t.Errorf("abandoned row = %v, want empty stop and answer cells", rows[3])
	}
	if !strings.HasSuffix(rows[1][5], "Z") || len(rows[1][5]) != len("2026-01-01T00:00:00.000Z") {
		t.Errorf("created_at = %q, want wire format", rows[1][5])
	}
	if _, _, err := cli(t, dbPath, "survey", "responses", "999"); err == nil {
		t.Error("unknown survey accepted")
	}
}

// TestSurveyResponsesCSV_NeutralizesFormulas pins finding 3: a rider-sourced
// cell that opens with a formula-trigger character (=, +, -, @, tab, CR)
// must not be handed to a spreadsheet as a live formula on open. A plain
// cell must pass through unchanged.
func TestSurveyResponsesCSV_NeutralizesFormulas(t *testing.T) {
	t.Parallel()
	dbPath, store := newDB(t)
	seedRegion(t, store.Regions(), 1)
	st := createStudy(t, dbPath)
	id := createSurvey(t, dbPath, st, surveyDoc)
	s, _ := store.Surveys().GetSurvey(context.Background(), id)
	ctx := context.Background()
	if _, err := store.Surveys().CreateResponse(ctx, surveys.NewResponse{
		SurveyID: id, PublicID: "formula-row", UserIdentifier: "@evil",
		Answers: []surveys.Answer{
			{QuestionID: s.Questions[0].ID, QuestionType: "radio", QuestionLabel: "How was your trip?", Answer: "=1+1"},
		},
	}, s.CreatedAt); err != nil {
		t.Fatal(err)
	}
	stdout, _, err := cli(t, dbPath, "survey", "responses", itoa(id))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := csv.NewReader(strings.NewReader(stdout)).ReadAll()
	if err != nil {
		t.Fatalf("not CSV: %v\n%s", err, stdout)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d (%v), want header + 1 answer", len(rows), rows)
	}
	if got := rows[1][1]; got != "'@evil" {
		t.Errorf("user_identifier cell = %q, want the apostrophe-guarded value", got)
	}
	if got := rows[1][10]; got != "'=1+1" {
		t.Errorf("answer cell = %q, want the apostrophe-guarded value", got)
	}
	// A plain answer (no formula-trigger prefix) must survive untouched.
	if _, err := store.Surveys().AmendResponse(ctx, "formula-row", []surveys.Answer{
		{QuestionID: 999, QuestionType: "text", QuestionLabel: "Plain", Answer: "just text"},
	}, s.CreatedAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	stdout, _, err = cli(t, dbPath, "survey", "responses", itoa(id))
	if err != nil {
		t.Fatal(err)
	}
	rows, err = csv.NewReader(strings.NewReader(stdout)).ReadAll()
	if err != nil {
		t.Fatalf("not CSV: %v\n%s", err, stdout)
	}
	found := false
	for _, row := range rows[1:] {
		if row[7] == "999" {
			found = true
			if row[10] != "just text" {
				t.Errorf("plain answer cell = %q, want unchanged", row[10])
			}
		}
	}
	if !found {
		t.Fatal("amended answer row not found")
	}
}
