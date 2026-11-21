package httpapi

import (
	"context"
	"encoding/csv"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/ghostbus"
	"github.com/OneBusAway/sidecar/internal/surveys"
)

// The SC-21 contract for the admin lists over rider rows: a caller that
// sends no paging parameters gets every row, exactly as before, as long as
// there are at most AdminListCap of them; past that it gets the first
// AdminListCap, X-Sidecar-Truncated: true, and an X-Sidecar-Next-Cursor to
// resume from. CSV exports never truncate -- they stream in pages.

func listFixture(t *testing.T, listCap int) *adminFixture {
	t.Helper()
	return newAdminFixtureWith(t, true, func(d *Deps) { d.AdminListCap = listCap })
}

func assertPage(t *testing.T, rec *httptest.ResponseRecorder, wantLen int, wantTruncated bool) string {
	t.Helper()
	got := array(t, rec, http.StatusOK)
	if len(got) != wantLen {
		t.Fatalf("page has %d rows, want %d: %s", len(got), wantLen, rec.Body.String())
	}
	truncated := rec.Header().Get(truncatedHeader) == "true"
	cursor := rec.Header().Get(nextCursorHeader)
	if truncated != wantTruncated || (cursor != "") != wantTruncated {
		t.Fatalf("truncated=%v cursor=%q, want truncated=%v and a cursor iff truncated", truncated, cursor, wantTruncated)
	}
	return cursor
}

func TestAdminAlarms_ListIsBoundedAndResumable(t *testing.T) {
	t.Parallel()
	f := listFixture(t, 2)
	for i := range 3 {
		f.seedAlarm(t, regionPuget, fmt.Sprintf("tok-%d", i), "push")
	}
	const path = "/api/admin/v1/regions/1/alarms"
	cursor := assertPage(t, f.do(http.MethodGet, path, ""), 2, true)
	assertPage(t, f.do(http.MethodGet, path+"?cursor="+url.QueryEscape(cursor), ""), 1, false)
	assertPage(t, f.do(http.MethodGet, path+"?limit=1", ""), 1, true)
	assertPage(t, f.do(http.MethodGet, path+"?limit=99", ""), 2, true) // clamped to the cap
}

func TestAdminAlarms_ListUnderTheCapIsUnchanged(t *testing.T) {
	t.Parallel()
	f := listFixture(t, 2)
	f.seedAlarm(t, regionPuget, "tok-a", "push")
	f.seedAlarm(t, regionPuget, "tok-b", "push")
	assertPage(t, f.do(http.MethodGet, "/api/admin/v1/regions/1/alarms", ""), 2, false)
}

func TestAdminLists_RejectBadPagingParams(t *testing.T) {
	t.Parallel()
	f := listFixture(t, 2)
	surveyID := f.seedSurveyWithResponses(t, regionPuget, []string{"a"})
	for _, base := range []string{
		"/api/admin/v1/regions/1/alarms",
		"/api/admin/v1/regions/1/ghost_bus_reports",
		fmt.Sprintf("/api/admin/v1/regions/1/surveys/%d/responses", surveyID),
	} {
		for _, q := range []string{"limit=0", "limit=-1", "limit=abc", "cursor=nope", "cursor=-5"} {
			if rec := f.do(http.MethodGet, base+"?"+q, ""); rec.Code != http.StatusBadRequest {
				t.Errorf("%s?%s: status = %d, want 400", base, q, rec.Code)
			}
		}
	}
}

func TestAdminResponses_ListIsBoundedAndResumable(t *testing.T) {
	t.Parallel()
	f := listFixture(t, 2)
	surveyID := f.seedSurveyWithResponses(t, regionPuget, []string{"a", "b", "c"})
	path := fmt.Sprintf("/api/admin/v1/regions/1/surveys/%d/responses", surveyID)
	cursor := assertPage(t, f.do(http.MethodGet, path, ""), 2, true)
	assertPage(t, f.do(http.MethodGet, path+"?cursor="+url.QueryEscape(cursor), ""), 1, false)
}

func TestAdminGhostBus_ListIsBoundedAndResumable(t *testing.T) {
	t.Parallel()
	f := listFixture(t, 2)
	f.seedReport(t, regionPuget, "old", testNow.Add(-48*time.Hour))
	for _, l := range []string{"a", "b", "c"} {
		f.seedReport(t, regionPuget, l, testNow)
	}
	since := url.QueryEscape(testNow.Add(-time.Hour).UTC().Format(time.RFC3339))
	path := "/api/admin/v1/regions/1/ghost_bus_reports?since=" + since
	cursor := assertPage(t, f.do(http.MethodGet, path, ""), 2, true)
	assertPage(t, f.do(http.MethodGet, path+"&cursor="+url.QueryEscape(cursor), ""), 1, false)
}

// pageRecorder records the largest page any paged read asked for, proving
// the CSV exports read in bounded batches rather than the whole set.
type pageRecorder struct {
	mu  sync.Mutex
	max int
}

func (p *pageRecorder) saw(limit int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.max = max(p.max, limit)
}

type pagedSurveys struct {
	surveys.Repository
	rec *pageRecorder
}

func (s pagedSurveys) ListResponses(context.Context, int64) ([]surveys.Response, error) {
	panic("the CSV export must page, not load every response")
}

func (s pagedSurveys) ListResponsesPage(ctx context.Context, id int64, after surveys.ResponseCursor, limit int) ([]surveys.Response, error) {
	s.rec.saw(limit)
	return s.Repository.ListResponsesPage(ctx, id, after, limit)
}

type pagedGhostBus struct {
	ghostbus.Repository
	rec *pageRecorder
}

func (g pagedGhostBus) ListForExport(context.Context, int64, int64) ([]ghostbus.Report, error) {
	panic("the CSV export must page, not load every report")
}

func (g pagedGhostBus) ListForExportPage(ctx context.Context, region, since, after int64, limit int) ([]ghostbus.Report, error) {
	g.rec.saw(limit)
	return g.Repository.ListForExportPage(ctx, region, since, after, limit)
}

func csvRows(t *testing.T, rec *httptest.ResponseRecorder) [][]string {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	rows, err := csv.NewReader(strings.NewReader(rec.Body.String())).ReadAll()
	if err != nil {
		t.Fatalf("parse csv: %v", err)
	}
	if rec.Header().Get(truncatedHeader) != "" {
		t.Errorf("a CSV export must never be truncated")
	}
	return rows
}

func TestAdminResponses_CSVStreamsEveryRowInPages(t *testing.T) {
	t.Parallel()
	rec := &pageRecorder{}
	f := newAdminFixtureWith(t, true, func(d *Deps) {
		d.AdminListCap = 2
		d.Surveys = pagedSurveys{Repository: d.Surveys, rec: rec}
	})
	surveyID := f.seedSurveyWithResponses(t, regionPuget, []string{"a", "b", "c", "d", "e"})
	rows := csvRows(t, f.do(http.MethodGet, fmt.Sprintf("/api/admin/v1/regions/1/surveys/%d/responses.csv", surveyID), ""))
	if len(rows) != 1+5 {
		t.Fatalf("csv rows = %d, want header + 5", len(rows))
	}
	if rec.max == 0 || rec.max > 3 {
		t.Fatalf("largest page = %d, want a bounded page of at most cap+1", rec.max)
	}
}

func TestAdminGhostBus_CSVStreamsEveryRowWithWholeExportTripCounts(t *testing.T) {
	t.Parallel()
	rec := &pageRecorder{}
	f := newAdminFixtureWith(t, true, func(d *Deps) {
		d.AdminListCap = 2
		d.GhostBus = pagedGhostBus{Repository: d.GhostBus, rec: rec}
	})
	// Five reports of one trip instance: the per-trip count must cover the
	// whole export, not one page.
	for i := range 5 {
		f.seedReportWith(t, regionPuget, fmt.Sprintf("r%d", i), testNow, func(in *ghostbus.NewReport) {
			in.TripIdentifier = "trip-shared"
			in.ServiceDate = testNow.UnixMilli()
		})
	}
	rows := csvRows(t, f.do(http.MethodGet, "/api/admin/v1/regions/1/ghost_bus_reports.csv", ""))
	if len(rows) != 1+5 {
		t.Fatalf("csv rows = %d, want header + 5", len(rows))
	}
	col := -1
	for i, h := range rows[0] {
		if h == "trip_report_count" {
			col = i
		}
	}
	for _, r := range rows[1:] {
		if r[col] != "5" {
			t.Fatalf("trip_report_count = %q, want 5 on every row", r[col])
		}
	}
	if rec.max == 0 || rec.max > 3 {
		t.Fatalf("largest page = %d, want a bounded page of at most cap+1", rec.max)
	}
}
