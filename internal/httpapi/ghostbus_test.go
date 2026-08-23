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
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/ghostbus"
	"github.com/OneBusAway/sidecar/internal/httpapi"
	"github.com/OneBusAway/sidecar/internal/ratelimit"
	"github.com/OneBusAway/sidecar/internal/regions"
	"github.com/OneBusAway/sidecar/internal/store/sqlitetest"
)

// fakeGhostBusRepo is the fake ghostbus.Repository the brief specifies.
// attempts records every Create call regardless of outcome (needed to
// assert on the token-collision re-mint, which never reaches "created");
// created records only the calls that actually succeeded.
type fakeGhostBusRepo struct {
	mu         sync.Mutex
	created    []ghostbus.NewReport
	attempts   []ghostbus.NewReport
	createErrs []error // popped per call; nil = success
}

func (f *fakeGhostBusRepo) Create(_ context.Context, in ghostbus.NewReport, _ time.Time) (ghostbus.Report, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts = append(f.attempts, in)
	if len(f.createErrs) > 0 {
		err := f.createErrs[0]
		f.createErrs = f.createErrs[1:]
		if err != nil {
			return ghostbus.Report{}, err
		}
	}
	f.created = append(f.created, in)
	return ghostbus.Report{ID: int64(len(f.created)), PublicID: in.PublicID}, nil
}

func (f *fakeGhostBusRepo) ListPendingSnapshots(context.Context, int64) ([]ghostbus.Report, error) {
	panic("not used by handler tests")
}
func (f *fakeGhostBusRepo) MarkSnapshotCaptured(context.Context, int64, string, time.Time) error {
	panic("not used by handler tests")
}
func (f *fakeGhostBusRepo) MarkSnapshotUnavailable(context.Context, int64, time.Time) error {
	panic("not used by handler tests")
}
func (f *fakeGhostBusRepo) RecordSnapshotFailure(context.Context, int64, time.Time) (int64, error) {
	panic("not used by handler tests")
}
func (f *fakeGhostBusRepo) ListForExport(context.Context, int64, int64) ([]ghostbus.Report, error) {
	panic("not used by handler tests")
}

// newGhostBusTestServer builds a router with the ghost bus route registered
// over a real (sqlite) regions store and the given fake repo. Nil limiters
// and logger get generous/discard defaults, matching surveyDeps' shape.
func newGhostBusTestServer(t *testing.T, repo *fakeGhostBusRepo, ipLimiter, userLimiter *ratelimit.Limiter, logger *slog.Logger) (http.Handler, regions.Repository) {
	t.Helper()
	store := sqlitetest.Open(t)
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	if ipLimiter == nil {
		ipLimiter = ratelimit.New(1000, time.Hour)
	}
	if userLimiter == nil {
		userLimiter = ratelimit.New(1000, 24*time.Hour)
	}
	deps := httpapi.Deps{
		GhostBus:            repo,
		GhostBusIPLimiter:   ipLimiter,
		GhostBusUserLimiter: userLimiter,
		Regions:             store.Regions(),
		Now:                 func() time.Time { return base },
		Logger:              logger,
	}
	return httpapi.NewRouter(deps), store.Regions()
}

// ghostBusRequest posts body to the ghost bus endpoint for region 1 (or the
// path given via target) and returns the recorded response. mutate, if
// non-nil, runs after standard setup so a case can override RemoteAddr,
// Content-Length, etc.
func ghostBusRequest(t *testing.T, h http.Handler, target, contentType, body string, mutate func(*http.Request)) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, target, strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if mutate != nil {
		mutate(req)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func ghostBusPost(t *testing.T, h http.Handler, contentType, body string) *httptest.ResponseRecorder {
	t.Helper()
	return ghostBusRequest(t, h, "/api/v2/regions/1/ghost_bus_reports", contentType, body, nil)
}

// validGhostBusFields is the field set every "should succeed" case starts
// from; individual subtests copy and mutate it.
func validGhostBusFields() map[string]string {
	return map[string]string{
		"user_identifier":            "device-abc-1",
		"trip_identifier":            "1_604370",
		"service_date":               "1754809200000",
		"wait_duration_minutes":      "15",
		"predicted":                  "1",
		"stop_identifier":            "1_570",
		"route_identifier":           "1_44",
		"vehicle_identifier":         "1_4361",
		"stop_sequence":              "3",
		"scheduled_arrival_at":       "1754809100000",
		"schedule_deviation_minutes": "2",
		"comment":                    "never showed",
		"user_latitude":              "47.6",
		"user_longitude":             "-122.3",
	}
}

func formEncode(fields map[string]string) string {
	v := url.Values{}
	for k, val := range fields {
		v.Set(k, val)
	}
	return v.Encode()
}

func jsonEncode(t *testing.T, fields map[string]any) string {
	t.Helper()
	b, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("marshal json fields: %v", err)
	}
	return string(b)
}

// stringsToAny converts a validGhostBusFields()-shaped string map to
// map[string]any so JSON subtests can override individual keys with native
// types (numbers, bools) while keeping the rest as strings.
func stringsToAny(m map[string]string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

var ghostBusIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{22}$`)

func decodeGhostBusID(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode id body: %v; body = %s", err, rec.Body.String())
	}
	if !ghostBusIDRe.MatchString(body.ID) {
		t.Fatalf("id = %q, want 22-char URL-safe token", body.ID)
	}
	return body.ID
}

// --- Case 1: iOS-shaped form body succeeds (pinned client contract) ---

func TestGhostBusCreate_IOSFormBody(t *testing.T) {
	t.Parallel()
	repo := &fakeGhostBusRepo{}
	h, regs := newGhostBusTestServer(t, repo, nil, nil, nil)
	putRegion(t, regs, 1)

	body := "user_identifier=device-abc-1&trip_identifier=1_604370&service_date=1754809200000&wait_duration_minutes=15&predicted=1&stop_identifier=1_570&route_identifier=1_44&vehicle_identifier=1_4361&stop_sequence=3&scheduled_arrival_at=1754809100000&schedule_deviation_minutes=2&comment=never+showed&user_latitude=47.6&user_longitude=-122.3"
	rec := ghostBusPost(t, h, formCT, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", rec.Code, rec.Body.String())
	}
	decodeGhostBusID(t, rec)

	if len(repo.created) != 1 {
		t.Fatalf("created = %d rows, want 1", len(repo.created))
	}
	in := repo.created[0]
	if in.Predicted == nil || !*in.Predicted {
		t.Errorf("Predicted = %v, want true", in.Predicted)
	}
	if in.ServiceDate != 1754809200000 {
		t.Errorf("ServiceDate = %d, want 1754809200000", in.ServiceDate)
	}
	if in.StopSequence == nil || *in.StopSequence != 3 {
		t.Errorf("StopSequence = %v, want 3", in.StopSequence)
	}
}

// --- Case 2: JSON body succeeds with native numeric/bool types ---

func TestGhostBusCreate_JSONBody(t *testing.T) {
	t.Parallel()
	repo := &fakeGhostBusRepo{}
	h, regs := newGhostBusTestServer(t, repo, nil, nil, nil)
	putRegion(t, regs, 1)

	fields := stringsToAny(validGhostBusFields())
	fields["predicted"] = true
	fields["service_date"] = 1754809200000
	fields["stop_sequence"] = 3
	rec := ghostBusPost(t, h, jsonCT, jsonEncode(t, fields))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", rec.Code, rec.Body.String())
	}
	decodeGhostBusID(t, rec)

	if len(repo.created) != 1 {
		t.Fatalf("created = %d rows, want 1", len(repo.created))
	}
	in := repo.created[0]
	if in.Predicted == nil || !*in.Predicted {
		t.Errorf("Predicted = %v, want true", in.Predicted)
	}
	if in.ServiceDate != 1754809200000 {
		t.Errorf("ServiceDate = %d, want 1754809200000", in.ServiceDate)
	}
	if in.StopSequence == nil || *in.StopSequence != 3 {
		t.Errorf("StopSequence = %v, want 3", in.StopSequence)
	}
}

// TestGhostBusCreate_PredictedThreeStates pins the other two states of
// Predicted's three-state contract (present-true is already covered by
// cases 1/2 above): present-false must store &false, not be dropped to
// nil, and absent must store nil, not default to &false. Collapsing the
// field to a plain bool, or defaulting an absent key to &false, would pass
// every other case in this file but fail one of these two.
func TestGhostBusCreate_PredictedThreeStates(t *testing.T) {
	t.Parallel()

	t.Run("present false", func(t *testing.T) {
		t.Parallel()
		repo := &fakeGhostBusRepo{}
		h, regs := newGhostBusTestServer(t, repo, nil, nil, nil)
		putRegion(t, regs, 1)

		fields := validGhostBusFields()
		fields["predicted"] = "0"
		rec := ghostBusPost(t, h, formCT, formEncode(fields))
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201; body = %s", rec.Code, rec.Body.String())
		}
		if len(repo.created) != 1 {
			t.Fatalf("created = %d rows, want 1", len(repo.created))
		}
		in := repo.created[0]
		if in.Predicted == nil || *in.Predicted {
			t.Errorf("Predicted = %v, want non-nil false", boolPtrString(in.Predicted))
		}
	})

	t.Run("absent", func(t *testing.T) {
		t.Parallel()
		repo := &fakeGhostBusRepo{}
		h, regs := newGhostBusTestServer(t, repo, nil, nil, nil)
		putRegion(t, regs, 1)

		fields := validGhostBusFields()
		delete(fields, "predicted")
		rec := ghostBusPost(t, h, formCT, formEncode(fields))
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201; body = %s", rec.Code, rec.Body.String())
		}
		if len(repo.created) != 1 {
			t.Fatalf("created = %d rows, want 1", len(repo.created))
		}
		in := repo.created[0]
		if in.Predicted != nil {
			t.Errorf("Predicted = %v, want nil", boolPtrString(in.Predicted))
		}
	})
}

func boolPtrString(b *bool) string {
	if b == nil {
		return "<nil>"
	}
	if *b {
		return "&true"
	}
	return "&false"
}

// --- Case 3: missing requireds ---

func TestGhostBusCreate_MissingRequireds(t *testing.T) {
	t.Parallel()
	repo := &fakeGhostBusRepo{}
	h, regs := newGhostBusTestServer(t, repo, nil, nil, nil)
	putRegion(t, regs, 1)

	rec := ghostBusPost(t, h, formCT, "")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body = %s", rec.Code, rec.Body.String())
	}
	eb := decodeErrBody(t, rec)
	if eb.Error != "Unable to save report" {
		t.Errorf("error = %q, want %q", eb.Error, "Unable to save report")
	}
	for _, want := range []string{
		"User identifier can't be blank",
		"Trip identifier can't be blank",
		"Service date can't be blank",
		"Wait duration minutes is not included in the list",
	} {
		if !slicesContains(eb.Messages, want) {
			t.Errorf("messages = %v, want to contain %q", eb.Messages, want)
		}
	}
	if len(repo.attempts) != 0 {
		t.Errorf("repo touched: %d attempts", len(repo.attempts))
	}
}

func slicesContains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// --- Case 4: non-integer service_date coerces to null then fails presence ---

func TestGhostBusCreate_NonIntegerServiceDate(t *testing.T) {
	t.Parallel()
	repo := &fakeGhostBusRepo{}
	h, regs := newGhostBusTestServer(t, repo, nil, nil, nil)
	putRegion(t, regs, 1)

	fields := validGhostBusFields()
	fields["service_date"] = "2026-08-23T10:00:00Z"
	rec := ghostBusPost(t, h, formCT, formEncode(fields))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body = %s", rec.Code, rec.Body.String())
	}
	eb := decodeErrBody(t, rec)
	if !slicesContains(eb.Messages, "Service date can't be blank") {
		t.Errorf("messages = %v, want to contain %q", eb.Messages, "Service date can't be blank")
	}
}

// --- Case 5: wait_duration_minutes=25 -> 422 ---

func TestGhostBusCreate_InvalidWaitDuration(t *testing.T) {
	t.Parallel()
	repo := &fakeGhostBusRepo{}
	h, regs := newGhostBusTestServer(t, repo, nil, nil, nil)
	putRegion(t, regs, 1)

	fields := validGhostBusFields()
	fields["wait_duration_minutes"] = "25"
	rec := ghostBusPost(t, h, formCT, formEncode(fields))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body = %s", rec.Code, rec.Body.String())
	}
	eb := decodeErrBody(t, rec)
	if !slicesContains(eb.Messages, "Wait duration minutes is not included in the list") {
		t.Errorf("messages = %v, want to contain the wait-duration message", eb.Messages)
	}
}

// --- Case 6: comment length (rune counting, not byte counting) ---

func TestGhostBusCreate_CommentLength(t *testing.T) {
	t.Parallel()

	// U+96E8 ("雨") is a 3-byte, 1-rune character: 1000 of them is legal,
	// 1001 is not, and byte-counting would misjudge both.
	rune1000 := strings.Repeat("雨", 1000)
	rune1001 := strings.Repeat("雨", 1001)

	t.Run("over limit rejected", func(t *testing.T) {
		t.Parallel()
		repo := &fakeGhostBusRepo{}
		h, regs := newGhostBusTestServer(t, repo, nil, nil, nil)
		putRegion(t, regs, 1)

		fields := validGhostBusFields()
		fields["comment"] = rune1001
		rec := ghostBusPost(t, h, formCT, formEncode(fields))
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422; body = %s", rec.Code, rec.Body.String())
		}
		eb := decodeErrBody(t, rec)
		want := "Comment is too long (maximum is 1000 characters)"
		if !slicesContains(eb.Messages, want) {
			t.Errorf("messages = %v, want to contain %q", eb.Messages, want)
		}
	})

	t.Run("exactly at limit accepted", func(t *testing.T) {
		t.Parallel()
		repo := &fakeGhostBusRepo{}
		h, regs := newGhostBusTestServer(t, repo, nil, nil, nil)
		putRegion(t, regs, 1)

		fields := validGhostBusFields()
		fields["comment"] = rune1000
		rec := ghostBusPost(t, h, formCT, formEncode(fields))
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201; body = %s", rec.Code, rec.Body.String())
		}
	})
}

// --- Case 7: coordinate validation ---

func TestGhostBusCreate_Coordinates(t *testing.T) {
	t.Parallel()

	t.Run("latitude out of range", func(t *testing.T) {
		t.Parallel()
		repo := &fakeGhostBusRepo{}
		h, regs := newGhostBusTestServer(t, repo, nil, nil, nil)
		putRegion(t, regs, 1)

		fields := validGhostBusFields()
		fields["user_latitude"] = "91"
		rec := ghostBusPost(t, h, formCT, formEncode(fields))
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422; body = %s", rec.Code, rec.Body.String())
		}
		eb := decodeErrBody(t, rec)
		want := "User latitude must be between -90 and 90"
		if !slicesContains(eb.Messages, want) {
			t.Errorf("messages = %v, want to contain %q", eb.Messages, want)
		}
	})

	t.Run("latitude unparseable", func(t *testing.T) {
		t.Parallel()
		repo := &fakeGhostBusRepo{}
		h, regs := newGhostBusTestServer(t, repo, nil, nil, nil)
		putRegion(t, regs, 1)

		fields := validGhostBusFields()
		fields["user_latitude"] = "abc"
		rec := ghostBusPost(t, h, formCT, formEncode(fields))
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422; body = %s", rec.Code, rec.Body.String())
		}
		eb := decodeErrBody(t, rec)
		want := "User latitude must be between -90 and 90"
		if !slicesContains(eb.Messages, want) {
			t.Errorf("messages = %v, want to contain %q", eb.Messages, want)
		}
	})

	t.Run("valid negative longitude passes", func(t *testing.T) {
		t.Parallel()
		repo := &fakeGhostBusRepo{}
		h, regs := newGhostBusTestServer(t, repo, nil, nil, nil)
		putRegion(t, regs, 1)

		fields := validGhostBusFields()
		fields["user_longitude"] = "-122.3"
		rec := ghostBusPost(t, h, formCT, formEncode(fields))
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201; body = %s", rec.Code, rec.Body.String())
		}
		if len(repo.created) != 1 || repo.created[0].UserLongitude == nil || *repo.created[0].UserLongitude != -122.3 {
			t.Errorf("stored longitude wrong: %+v", repo.created)
		}
	})
}

// --- Case 8: duplicate maps to already_reported ---

func TestGhostBusCreate_Duplicate(t *testing.T) {
	t.Parallel()
	repo := &fakeGhostBusRepo{createErrs: []error{ghostbus.ErrDuplicate}}
	h, regs := newGhostBusTestServer(t, repo, nil, nil, nil)
	putRegion(t, regs, 1)

	rec := ghostBusPost(t, h, formCT, formEncode(validGhostBusFields()))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body = %s", rec.Code, rec.Body.String())
	}

	// Byte-compare after JSON normalization: re-marshal both sides through
	// encoding/json so key order and whitespace can't cause a false
	// mismatch, while still pinning the exact shape.
	want, err := json.Marshal(map[string]any{
		"error":    "already_reported",
		"messages": []string{"User has already reported this trip"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var gotDecoded any
	if err := json.Unmarshal(rec.Body.Bytes(), &gotDecoded); err != nil {
		t.Fatalf("decode response: %v; body = %s", err, rec.Body.String())
	}
	got, err := json.Marshal(gotDecoded)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("body = %s, want %s", got, want)
	}
}

// --- Case 9: token collision re-mints once, then twice fails as 500 ---

func TestGhostBusCreate_TokenCollisionRemintsOnce(t *testing.T) {
	t.Parallel()
	repo := &fakeGhostBusRepo{createErrs: []error{ghostbus.ErrTokenCollision, nil}}
	h, regs := newGhostBusTestServer(t, repo, nil, nil, nil)
	putRegion(t, regs, 1)

	rec := ghostBusPost(t, h, formCT, formEncode(validGhostBusFields()))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", rec.Code, rec.Body.String())
	}
	if len(repo.attempts) != 2 {
		t.Fatalf("Create called %d times, want 2", len(repo.attempts))
	}
	if repo.attempts[0].PublicID == repo.attempts[1].PublicID {
		t.Errorf("both attempts used the same PublicID %q, want different", repo.attempts[0].PublicID)
	}
	if repo.attempts[0].PublicID == "" || repo.attempts[1].PublicID == "" {
		t.Errorf("attempts = %+v, want non-empty PublicIDs", repo.attempts)
	}
}

func TestGhostBusCreate_TokenCollisionTwiceIs500(t *testing.T) {
	t.Parallel()
	repo := &fakeGhostBusRepo{createErrs: []error{ghostbus.ErrTokenCollision, ghostbus.ErrTokenCollision}}
	h, regs := newGhostBusTestServer(t, repo, nil, nil, nil)
	putRegion(t, regs, 1)

	rec := ghostBusPost(t, h, formCT, formEncode(validGhostBusFields()))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body = %s", rec.Code, rec.Body.String())
	}
	if len(repo.attempts) != 2 {
		t.Fatalf("Create called %d times, want 2", len(repo.attempts))
	}
}

// --- Case 10: unknown region -> 404 ---

func TestGhostBusCreate_UnknownRegion(t *testing.T) {
	t.Parallel()
	repo := &fakeGhostBusRepo{}
	h, regs := newGhostBusTestServer(t, repo, nil, nil, nil)
	putRegion(t, regs, 1)

	rec := ghostBusRequest(t, h, "/api/v2/regions/999/ghost_bus_reports", formCT, formEncode(validGhostBusFields()), nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != `{"error":"Couldn't find Region"}` {
		t.Errorf("body = %q, want the region-not-found body", got)
	}
}

func TestGhostBusCreate_SlugRegionWorks(t *testing.T) {
	t.Parallel()
	repo := &fakeGhostBusRepo{}
	h, regs := newGhostBusTestServer(t, repo, nil, nil, nil)
	putRegion(t, regs, 1)

	rec := ghostBusRequest(t, h, "/api/v2/regions/1-puget-sound/ghost_bus_reports", formCT, formEncode(validGhostBusFields()), nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", rec.Code, rec.Body.String())
	}
}

// --- Case 11: oversized declared JSON -> bodyless 403 ---

func TestGhostBusCreate_OversizedDeclaredJSON(t *testing.T) {
	t.Parallel()
	repo := &fakeGhostBusRepo{}
	h, regs := newGhostBusTestServer(t, repo, nil, nil, nil)
	putRegion(t, regs, 1)

	body := strings.Repeat("x", 9000)
	rec := ghostBusPost(t, h, jsonCT, body)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body = %s", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Errorf("403 body = %q, want empty", rec.Body.String())
	}
	if len(repo.attempts) != 0 {
		t.Errorf("repo touched: %d attempts", len(repo.attempts))
	}
}

// TestGhostBusCreate_LyingHighContentLength pins the pre-read
// r.ContentLength guard specifically -- not just its end-state. A
// genuinely-9000-byte body (as in TestGhostBusCreate_OversizedDeclaredJSON
// above) produces a byte-identical bodyless 403 whether that guard exists
// or parseRequestParams' MaxBytesReader alone catches it, so that case
// alone cannot tell the two apart. Here the body is short and entirely
// valid; only a declared Content-Length that lies high triggers the 403.
// Without the pre-read guard, MaxBytesReader never sees more than the real
// (small) body and the request would parse and succeed.
func TestGhostBusCreate_LyingHighContentLength(t *testing.T) {
	t.Parallel()
	repo := &fakeGhostBusRepo{}
	h, regs := newGhostBusTestServer(t, repo, nil, nil, nil)
	putRegion(t, regs, 1)

	body := jsonEncode(t, stringsToAny(validGhostBusFields()))
	rec := ghostBusRequest(t, h, "/api/v2/regions/1/ghost_bus_reports", jsonCT, body, func(r *http.Request) {
		r.ContentLength = 9000
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body = %s", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Errorf("403 body = %q, want empty", rec.Body.String())
	}
	if len(repo.attempts) != 0 {
		t.Errorf("repo touched: %d attempts", len(repo.attempts))
	}
}

// --- Case 12: oversized streamed JSON (lying Content-Length) -> 403 ---

func TestGhostBusCreate_OversizedStreamedJSON(t *testing.T) {
	t.Parallel()
	repo := &fakeGhostBusRepo{}
	h, regs := newGhostBusTestServer(t, repo, nil, nil, nil)
	putRegion(t, regs, 1)

	// A real body over the 8 KB JSON cap, but the request claims a small
	// Content-Length -- the initial "declared size" check in the handler
	// must not be the only thing enforcing the cap; parseRequestParams'
	// MaxBytesReader has to catch it too.
	body := `{"comment":"` + strings.Repeat("a", 9000) + `"}`
	rec := ghostBusRequest(t, h, "/api/v2/regions/1/ghost_bus_reports", jsonCT, body, func(r *http.Request) {
		r.ContentLength = 100
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body = %s", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Errorf("403 body = %q, want empty", rec.Body.String())
	}
	if len(repo.attempts) != 0 {
		t.Errorf("repo touched: %d attempts", len(repo.attempts))
	}
}

// --- Case 13: large form body is NOT capped at 8 KB (iOS emoji comment guard) ---

func TestGhostBusCreate_LargeFormBodyNotCapped(t *testing.T) {
	t.Parallel()
	repo := &fakeGhostBusRepo{}
	h, regs := newGhostBusTestServer(t, repo, nil, nil, nil)
	putRegion(t, regs, 1)

	fields := validGhostBusFields()
	// A legal (<=1000 rune) but multibyte-heavy comment, like an
	// emoji-laden iOS report -- percent-encoding alone can push this past
	// 8 KB.
	fields["comment"] = strings.Repeat("雨", 800)
	// Distinct ignored padding params -- not an over-cap comment -- push
	// the total body past 8 KB without touching the comment/CommentMaxLen
	// boundary this case is not about.
	for i := range 60 {
		fields[fmt.Sprintf("padding_field_%d", i)] = strings.Repeat("x", 300)
	}
	body := formEncode(fields)
	if len(body) <= ghostBusJSONBodyLimitForTest {
		t.Fatalf("test body is only %d bytes, want > %d to actually exercise the cap", len(body), ghostBusJSONBodyLimitForTest)
	}

	rec := ghostBusPost(t, h, formCT, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body = %s", rec.Code, rec.Body.String())
	}
}

// ghostBusJSONBodyLimitForTest mirrors the unexported ghostBusJSONBodyLimit
// constant in package httpapi (8192): this test package cannot see it, and
// duplicating the literal here (rather than importing it) is deliberate --
// it keeps the size assertion honest about the JSON cap value rather than
// silently tracking whatever the production constant changes to.
const ghostBusJSONBodyLimitForTest = 8192

// --- Case 14: per-user throttle counts across form AND JSON encodings ---

func TestGhostBusCreate_UserThrottleAcrossEncodings(t *testing.T) {
	t.Parallel()
	repo := &fakeGhostBusRepo{}
	userLimiter := ratelimit.New(1, 24*time.Hour)
	h, regs := newGhostBusTestServer(t, repo, nil, userLimiter, nil)
	putRegion(t, regs, 1)

	fields1 := validGhostBusFields()
	fields1["user_identifier"] = "same-user"
	fields1["trip_identifier"] = "trip-A"
	rec1 := ghostBusPost(t, h, formCT, formEncode(fields1))
	if rec1.Code != http.StatusCreated {
		t.Fatalf("request 1 (form): status = %d, want 201; body = %s", rec1.Code, rec1.Body.String())
	}

	fields2 := stringsToAny(validGhostBusFields())
	fields2["user_identifier"] = "same-user"
	fields2["trip_identifier"] = "trip-B"
	rec2 := ghostBusPost(t, h, jsonCT, jsonEncode(t, fields2))
	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("request 2 (json): status = %d, want 429; body = %s", rec2.Code, rec2.Body.String())
	}

	if len(repo.created) != 1 {
		t.Fatalf("created = %d rows, want exactly 1", len(repo.created))
	}
}

// --- Case 15: over-length user_identifier never becomes a limiter key ---

func TestGhostBusCreate_OverlongIdentifierSkipsThrottle(t *testing.T) {
	t.Parallel()
	repo := &fakeGhostBusRepo{}
	userLimiter := ratelimit.New(1, 24*time.Hour)
	h, regs := newGhostBusTestServer(t, repo, nil, userLimiter, nil)
	putRegion(t, regs, 1)

	fields := validGhostBusFields()
	fields["user_identifier"] = strings.Repeat("a", 300)
	rec := ghostBusPost(t, h, formCT, formEncode(fields))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body = %s", rec.Code, rec.Body.String())
	}
	eb := decodeErrBody(t, rec)
	want := "User identifier is too long (maximum is 255 characters)"
	if !slicesContains(eb.Messages, want) {
		t.Errorf("messages = %v, want to contain %q", eb.Messages, want)
	}
	if got := userLimiter.Len(); got != 0 {
		t.Errorf("GhostBusUserLimiter.Len() = %d after 422, want 0 (the over-length identifier must never reach Allow)", got)
	}
}

// --- Case 16: IP throttle ---

func TestGhostBusCreate_IPThrottle(t *testing.T) {
	t.Parallel()
	repo := &fakeGhostBusRepo{}
	ipLimiter := ratelimit.New(1, time.Hour)
	h, regs := newGhostBusTestServer(t, repo, ipLimiter, nil, nil)
	putRegion(t, regs, 1)

	fields1 := validGhostBusFields()
	fields1["user_identifier"] = "user-1"
	rec1 := ghostBusPost(t, h, formCT, formEncode(fields1))
	if rec1.Code != http.StatusCreated {
		t.Fatalf("request 1: status = %d, want 201; body = %s", rec1.Code, rec1.Body.String())
	}

	fields2 := validGhostBusFields()
	fields2["user_identifier"] = "user-2" // different identifier, same IP
	rec2 := ghostBusPost(t, h, formCT, formEncode(fields2))
	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("request 2: status = %d, want 429; body = %s", rec2.Code, rec2.Body.String())
	}
	if rec2.Body.Len() != 0 {
		t.Errorf("429 body = %q, want empty", rec2.Body.String())
	}
}

// --- Case 17: store failure -> 500, identifier never logged ---

func TestGhostBusCreate_StoreFailureIs500AndNeverLogsIdentifier(t *testing.T) {
	t.Parallel()
	const identifier = "super-secret-device-identifier-12345"
	repo := &fakeGhostBusRepo{createErrs: []error{errors.New("db exploded")}}

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	h, regs := newGhostBusTestServer(t, repo, nil, nil, logger)
	putRegion(t, regs, 1)

	fields := validGhostBusFields()
	fields["user_identifier"] = identifier
	rec := ghostBusPost(t, h, formCT, formEncode(fields))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body = %s", rec.Code, rec.Body.String())
	}

	logOutput := buf.String()
	if logOutput == "" {
		t.Error("expected the store failure to be logged, got nothing")
	}
	if strings.Contains(logOutput, identifier) {
		t.Errorf("log output contains the user identifier: %s", logOutput)
	}
}
