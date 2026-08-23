package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/httpapi"
	"github.com/OneBusAway/sidecar/internal/ratelimit"
	"github.com/OneBusAway/sidecar/internal/store/sqlitetest"
	"github.com/OneBusAway/sidecar/internal/surveys"
)

func send(t *testing.T, h http.Handler, method, target, contentType, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), method, target, strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.RemoteAddr = "203.0.113.9:4000"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

type createdBody struct {
	SurveyResponse *struct {
		ID             *string `json:"id"`
		UserIdentifier *string `json:"user_identifier"`
		UpdatePath     *string `json:"update_path"`
	} `json:"survey_response"`
}

func decodeCreated(t *testing.T, rec *httptest.ResponseRecorder) createdBody {
	t.Helper()
	var cb createdBody
	if err := json.Unmarshal(rec.Body.Bytes(), &cb); err != nil {
		t.Fatalf("decode: %v; body = %s", err, rec.Body.String())
	}
	if cb.SurveyResponse == nil || cb.SurveyResponse.ID == nil || cb.SurveyResponse.UserIdentifier == nil || cb.SurveyResponse.UpdatePath == nil {
		t.Fatalf("survey_response keys missing (iOS requires all three): %s", rec.Body.String())
	}
	return cb
}

const answers1 = `[{"question_id":21,"question_type":"radio","question_label":"Trip?","answer":"Great"}]`

// androidForm is the exact body the Android app sends: form-encoded,
// trailing-slash path, zeroed coordinates with no stop identifier.
func androidForm(surveyID int64) string {
	v := url.Values{}
	v.Set("user_identifier", "android-uuid")
	v.Set("survey_id", fmt.Sprint(surveyID))
	v.Set("stop_latitude", "0.0")
	v.Set("stop_longitude", "0.0")
	v.Set("responses", answers1)
	return v.Encode()
}

// iosJSON is the exact body the iOS app sends: JSON with responses as a
// JSON-encoded string, no stop fields when map-originated.
func iosJSON(surveyID int64) string {
	b, _ := json.Marshal(map[string]any{"user_identifier": "ios-uuid", "survey_id": surveyID, "responses": answers1})
	return string(b)
}

func TestSurveyResponse_CreateAndroidShape(t *testing.T) {
	t.Parallel()
	h, repo, regs := surveyDeps(t, nil, nil)
	s := seedSurvey(t, repo, regs, 1, fullDefinition())
	rec := send(t, h, http.MethodPost, "/api/v1/survey_responses/", formCT, androidForm(s.ID))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	cb := decodeCreated(t, rec)
	if *cb.SurveyResponse.UserIdentifier != "android-uuid" || *cb.SurveyResponse.UpdatePath != "/api/v1/survey_responses/"+*cb.SurveyResponse.ID {
		t.Errorf("body = %s", rec.Body.String())
	}
	stored, err := repo.GetResponse(context.Background(), *cb.SurveyResponse.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.StopIdentifier != "" || stored.StopLatitude == nil || *stored.StopLatitude != 0 || stored.StopLongitude == nil {
		t.Errorf("Android zeroed coordinates without identifier must be stored as given: %+v", stored)
	}
	if len(stored.Answers) != 1 || stored.Answers[0].Answer != "Great" {
		t.Errorf("answers = %+v", stored.Answers)
	}
}

func TestSurveyResponse_CreateIOSShape(t *testing.T) {
	t.Parallel()
	h, repo, regs := surveyDeps(t, nil, nil)
	s := seedSurvey(t, repo, regs, 1, fullDefinition())
	rec := send(t, h, http.MethodPost, "/api/v1/survey_responses/?key=k&app_uid=x&app_ver=1&version=2", jsonCT, iosJSON(s.ID))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	cb := decodeCreated(t, rec)
	if len(*cb.SurveyResponse.ID) != 22 {
		t.Errorf("id = %q, want a 22-char securetoken", *cb.SurveyResponse.ID)
	}
	bare := send(t, h, http.MethodPost, "/api/v1/survey_responses", jsonCT, iosJSON(s.ID))
	if bare.Code != http.StatusCreated {
		t.Errorf("bare path status = %d", bare.Code)
	}
}

func TestSurveyResponse_CreateWithStop(t *testing.T) {
	t.Parallel()
	h, repo, regs := surveyDeps(t, nil, nil)
	s := seedSurvey(t, repo, regs, 1, fullDefinition())
	body := fmt.Sprintf(`{"user_identifier":"u","survey_id":%d,"stop_identifier":"1_570","stop_latitude":47.6,"stop_longitude":-122.3,"responses":"[]"}`, s.ID)
	rec := send(t, h, http.MethodPost, "/api/v1/survey_responses", jsonCT, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	cb := decodeCreated(t, rec)
	stored, err := repo.GetResponse(context.Background(), *cb.SurveyResponse.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.StopIdentifier != "1_570" || stored.StopLatitude == nil || *stored.StopLatitude != 47.6 || *stored.StopLongitude != -122.3 {
		t.Errorf("stored = %+v", stored)
	}
}

func TestSurveyResponse_CreateErrors(t *testing.T) {
	t.Parallel()
	h, repo, regs := surveyDeps(t, nil, nil)
	s := seedSurvey(t, repo, regs, 1, fullDefinition())
	id := fmt.Sprint(s.ID)
	tests := []struct {
		name, ct, body string
		status         int
		want           string
	}{
		{"missing survey_id", jsonCT, `{"user_identifier":"u","responses":"[]"}`, 404, `{"error":"Couldn't find Survey"}`},
		{"non-integer survey_id", formCT, "survey_id=abc&user_identifier=u&responses=[]", 404, `{"error":"Couldn't find Survey"}`},
		{"unknown survey_id", formCT, "survey_id=999999&user_identifier=u&responses=[]", 404, `{"error":"Couldn't find Survey"}`},
		{"blank user_identifier", formCT, "survey_id=" + id + "&user_identifier=%20&responses=[]", 422, `{"errors":["User identifier can't be blank"]}`},
		{"responses missing", formCT, "survey_id=" + id + "&user_identifier=u", 422, `{"errors":["responses must be a JSON-encoded array of answer objects"]}`},
		{"responses native array", jsonCT, `{"survey_id":` + id + `,"user_identifier":"u","responses":[{"question_id":1}]}`, 422, `{"errors":["responses must be a JSON-encoded array of answer objects"]}`},
		{"responses not json", formCT, "survey_id=" + id + "&user_identifier=u&responses=not-json", 422, `"responses must be a JSON-encoded array of answer objects"`},
		{"stop id without coords", formCT, "survey_id=" + id + "&user_identifier=u&stop_identifier=1_570&responses=[]", 422,
			`{"errors":["Stop latitude can't be blank","Stop longitude can't be blank"]}`},
		{"stop coords out of range", formCT, "survey_id=" + id + "&user_identifier=u&stop_identifier=1_570&stop_latitude=91&stop_longitude=-181&responses=[]", 422,
			`{"errors":["Stop latitude is invalid","Stop longitude is invalid"]}`},
		{"every message in order", formCT, "survey_id=" + id + "&user_identifier=&stop_identifier=1_570&responses=nope", 422,
			`{"errors":["User identifier can't be blank","Stop latitude can't be blank","Stop longitude can't be blank","responses must be a JSON-encoded array of answer objects"]}`},
		{"invalid json body", jsonCT, `{not json`, 422, `{"errors":["request body is invalid"]}`},
		{"too many answers", formCT, "survey_id=" + id + "&user_identifier=u&responses=" + url.QueryEscape(manyAnswers(surveys.MaxAnswers+1)), 422,
			`{"errors":["responses has too many answers"]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec := send(t, h, http.MethodPost, "/api/v1/survey_responses", tt.ct, tt.body)
			if rec.Code != tt.status || !strings.Contains(strings.TrimSpace(rec.Body.String()), tt.want) {
				t.Fatalf("status = %d body = %s; want %d containing %s", rec.Code, rec.Body.String(), tt.status, tt.want)
			}
		})
	}
	t.Run("body too large", func(t *testing.T) {
		t.Parallel()
		big := "survey_id=" + id + "&user_identifier=u&responses=" + strings.Repeat("x", 70<<10)
		rec := send(t, h, http.MethodPost, "/api/v1/survey_responses", formCT, big)
		if rec.Code != 422 || !strings.Contains(rec.Body.String(), `"request body too large"`) {
			t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
		}
	})
}

func manyAnswers(n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = fmt.Sprintf(`{"question_id":%d,"answer":"x"}`, i+1)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

func createOne(t *testing.T, h http.Handler, surveyID int64) string {
	t.Helper()
	rec := send(t, h, http.MethodPost, "/api/v1/survey_responses", jsonCT, iosJSON(surveyID))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	return *decodeCreated(t, rec).SurveyResponse.ID
}

func TestSurveyResponse_AmendAllMethods(t *testing.T) {
	t.Parallel()
	h, repo, regs := surveyDeps(t, nil, nil)
	s := seedSurvey(t, repo, regs, 1, fullDefinition())
	cases := []struct{ name, method, ct, body string }{
		{"ios PUT json", http.MethodPut, jsonCT, `{"responses":"[{\"question_id\":22,\"answer\":\"more\"}]"}`},
		// Android resends everything, including zeroed coordinates and a
		// different user_identifier; only responses may take effect.
		{"android POST form", http.MethodPost, formCT, "user_identifier=someone-else&survey_id=999&stop_latitude=0.0&stop_longitude=0.0&responses=" + url.QueryEscape(`[{"question_id":22,"answer":"more"}]`)},
		{"openapi PATCH", http.MethodPatch, jsonCT, `{"responses":"[{\"question_id\":22,\"answer\":\"more\"}]"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			id := createOne(t, h, s.ID)
			rec := send(t, h, c.method, "/api/v1/survey_responses/"+id, c.ct, c.body)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
			}
			cb := decodeCreated(t, rec)
			if *cb.SurveyResponse.ID != id || *cb.SurveyResponse.UserIdentifier != "ios-uuid" {
				t.Errorf("amend body = %s", rec.Body.String())
			}
			stored, err := repo.GetResponse(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			if stored.UserIdentifier != "ios-uuid" || stored.SurveyID != s.ID || stored.StopLatitude != nil || stored.StopIdentifier != "" {
				t.Errorf("amend changed non-responses fields: %+v", stored)
			}
			if len(stored.Answers) != 2 || stored.Answers[0].Answer != "Great" || stored.Answers[1].QuestionID != 22 {
				t.Errorf("answers = %+v, want hero merged with amend", stored.Answers)
			}
		})
	}
}

func TestSurveyResponse_UpdatePathIsHonored(t *testing.T) {
	t.Parallel()
	h, repo, regs := surveyDeps(t, nil, nil)
	s := seedSurvey(t, repo, regs, 1, fullDefinition())
	rec := send(t, h, http.MethodPost, "/api/v1/survey_responses", jsonCT, iosJSON(s.ID))
	cb := decodeCreated(t, rec)
	amend := send(t, h, http.MethodPost, *cb.SurveyResponse.UpdatePath, formCT, "responses="+url.QueryEscape(`[{"question_id":21,"answer":"Changed"}]`))
	if amend.Code != http.StatusOK {
		t.Fatalf("POST to update_path: %d %s", amend.Code, amend.Body.String())
	}
	stored, _ := repo.GetResponse(context.Background(), *cb.SurveyResponse.ID)
	if len(stored.Answers) != 1 || stored.Answers[0].Answer != "Changed" {
		t.Errorf("answers = %+v, want the replaced hero answer", stored.Answers)
	}
}

func TestSurveyResponse_AmendErrors(t *testing.T) {
	t.Parallel()
	h, repo, regs := surveyDeps(t, nil, nil)
	s := seedSurvey(t, repo, regs, 1, fullDefinition())
	id := createOne(t, h, s.ID)
	tests := []struct {
		name, path, body string
		status           int
		want             string
	}{
		{"unknown id", "/api/v1/survey_responses/nope", `{"responses":"[]"}`, 404, `{"error":"Couldn't find SurveyResponse"}`},
		{"malformed responses", "/api/v1/survey_responses/" + id, `{"responses":"nope"}`, 422, `{"errors":["responses must be a JSON-encoded array of answer objects"]}`},
		{"missing responses", "/api/v1/survey_responses/" + id, `{}`, 422, `{"errors":["responses must be a JSON-encoded array of answer objects"]}`},
		{"empty amend is ok", "/api/v1/survey_responses/" + id, `{"responses":"[]"}`, 200, `"update_path"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			rec := send(t, h, http.MethodPut, tt.path, jsonCT, tt.body)
			if rec.Code != tt.status || !strings.Contains(strings.TrimSpace(rec.Body.String()), tt.want) {
				t.Fatalf("status = %d body = %s; want %d containing %s", rec.Code, rec.Body.String(), tt.status, tt.want)
			}
		})
	}
	t.Run("cap across amends", func(t *testing.T) {
		t.Parallel()
		id := createOne(t, h, s.ID)
		fill := send(t, h, http.MethodPut, "/api/v1/survey_responses/"+id, formCT, "responses="+url.QueryEscape(manyAnswers(surveys.MaxAnswers)))
		if fill.Code != 200 {
			t.Fatalf("fill: %d %s", fill.Code, fill.Body.String())
		}
		over := send(t, h, http.MethodPut, "/api/v1/survey_responses/"+id, jsonCT, `{"responses":"[{\"question_id\":99999,\"answer\":\"x\"}]"}`)
		if over.Code != 422 || !strings.Contains(over.Body.String(), "too many answers") {
			t.Fatalf("over cap: %d %s", over.Code, over.Body.String())
		}
	})
}

// TestSurveyResponse_SharedThrottle: the bucket is one for create and
// amend (design spec 2.9); exhausting it with creates must 429 an amend.
func TestSurveyResponse_SharedThrottle(t *testing.T) {
	t.Parallel()
	h, repo, regs := surveyDeps(t, ratelimit.New(3, time.Minute), nil)
	s := seedSurvey(t, repo, regs, 1, fullDefinition())
	id := createOne(t, h, s.ID)
	createOne(t, h, s.ID)
	createOne(t, h, s.ID)
	if rec := send(t, h, http.MethodPut, "/api/v1/survey_responses/"+id, jsonCT, `{"responses":"[]"}`); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("4th write status = %d, want 429", rec.Code)
	}
	if rec := get(t, h, "/api/v1/regions/1/surveys?user_id=u"); rec.Code != http.StatusOK {
		t.Fatalf("list throttled: %d", rec.Code)
	}
}

// erroringSurveysRepo fails every write with an error echoing rider data,
// the worst case a driver could produce; the handler's log line must not
// carry it.
type erroringSurveysRepo struct{ surveys.Repository }

func (r erroringSurveysRepo) CreateResponse(_ context.Context, in surveys.NewResponse, _ time.Time) (surveys.Response, error) {
	return surveys.Response{}, fmt.Errorf("disk full while storing user=%s stop=%s answer=%s", in.UserIdentifier, in.StopIdentifier, in.Answers[0].Answer)
}

func TestSurveyResponse_LogsNeverCarryRiderData(t *testing.T) {
	t.Parallel()
	store := sqlitetest.Open(t)
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	deps := httpapi.Deps{
		Surveys: erroringSurveysRepo{store.Surveys()}, Regions: store.Regions(),
		Now: func() time.Time { return base }, Logger: logger,
	}
	h := httpapi.NewRouter(deps)
	s := seedSurvey(t, store.Surveys(), store.Regions(), 1, fullDefinition())

	const secretUser, secretStop, secretAnswer = "rider-uuid-SECRET", "stop-SECRET", "answer-SECRET"
	body := fmt.Sprintf(`{"user_identifier":%q,"survey_id":%d,"stop_identifier":%q,"stop_latitude":1,"stop_longitude":2,"responses":"[{\"question_id\":1,\"answer\":\"%s\"}]"}`,
		secretUser, s.ID, secretStop, secretAnswer)
	if rec := send(t, h, http.MethodPost, "/api/v1/survey_responses", jsonCT, body); rec.Code != http.StatusInternalServerError || rec.Body.Len() != 0 {
		t.Fatalf("store error: status = %d body = %q, want bodyless 500", rec.Code, rec.Body.String())
	}
	if rec := send(t, h, http.MethodPost, "/api/v1/survey_responses", jsonCT, fmt.Sprintf(`{"survey_id":%d,"user_identifier":%q,"responses":"nope-%s"}`, s.ID, secretUser, secretAnswer)); rec.Code != 422 {
		t.Fatalf("validation: %d", rec.Code)
	}
	logs := buf.String()
	if !strings.Contains(logs, "create survey response") {
		t.Fatalf("expected an error log line for the store failure; got: %s", logs)
	}
	if !strings.Contains(logs, "rejected survey response") {
		t.Fatalf("expected an info log line for the 422; got: %s", logs)
	}
	for _, secret := range []string{secretUser, secretStop, secretAnswer} {
		if strings.Contains(logs, secret) {
			t.Errorf("log carries rider data %q: %s", secret, logs)
		}
	}
}

func TestSurveyResponse_UnknownSurveyNoStoreWrite(t *testing.T) {
	t.Parallel()
	h, repo, regs := surveyDeps(t, nil, nil)
	s := seedSurvey(t, repo, regs, 1, fullDefinition())
	_ = s
	rec := send(t, h, http.MethodPost, "/api/v1/survey_responses", formCT, "survey_id=424242&user_identifier=u&responses=[]")
	if rec.Code != 404 {
		t.Fatalf("%d", rec.Code)
	}
	if _, err := repo.GetResponse(context.Background(), "anything"); !errors.Is(err, surveys.ErrNotFound) {
		t.Fatal(err)
	}
}
