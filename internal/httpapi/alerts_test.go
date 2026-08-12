package httpapi_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/MobilityData/gtfs-realtime-bindings/golang/gtfs"
	"github.com/google/go-cmp/cmp"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/testing/protocmp"

	"github.com/OneBusAway/sidecar/internal/alerts"
	"github.com/OneBusAway/sidecar/internal/httpapi"
	"github.com/OneBusAway/sidecar/internal/regions"
	"github.com/OneBusAway/sidecar/internal/store/sqlite"
)

// base is the fixed instant every subtest builds its timestamps from, and
// the value Deps.Now returns -- this package must never read the wall clock.
var base = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func TestParseRegionSegment(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"1", 1, true},
		{"1-puget-sound", 1, true},
		{"0", 0, true},                       // Tampa Bay: zero is a real region
		{"007", 7, true},                     // leading zeros
		{"92233720368547758081-x", 0, false}, // overflows int64
		{"abc", 0, false},
		{"-1", 0, false},
		{"+1", 0, false},
		{"", 0, false},
	}
	for _, tt := range tests {
		got, ok := httpapi.ParseRegionSegment(tt.in)
		if got != tt.want || ok != tt.ok {
			t.Errorf("ParseRegionSegment(%q) = (%d, %v), want (%d, %v)", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}

// newTestServer builds a router over a freshly migrated SQLite store and
// hands back the router plus both repositories, so tests can seed fixtures
// directly through the same interfaces the handlers use.
func newTestServer(t *testing.T) (http.Handler, alerts.Repository, regions.Repository) {
	t.Helper()

	store, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	deps := httpapi.Deps{
		Alerts:  store.Alerts(),
		Regions: store.Regions(),
		Now:     func() time.Time { return base },
		Logger:  slog.New(slog.DiscardHandler),
	}
	return httpapi.NewRouter(deps), store.Alerts(), store.Regions()
}

func putRegion(t *testing.T, repo regions.Repository, id int64) {
	t.Helper()
	if err := repo.UpsertFromDirectory(context.Background(), []regions.Region{{
		ID:         id,
		Name:       "Test Region",
		OBABaseURL: "https://example.org/",
		Active:     true,
	}}, base); err != nil {
		t.Fatalf("UpsertFromDirectory(%d): %v", id, err)
	}
}

// publishAlert creates and publishes a minimal valid alert for regionID and
// returns its id.
func publishAlert(t *testing.T, repo alerts.Repository, regionID int64, header string, isTest bool) int64 {
	t.Helper()
	ctx := context.Background()
	a, err := repo.Create(ctx, alerts.NewAlert{
		RegionID: regionID, AgencyID: "40", HeaderText: header,
		Cause: "UNKNOWN_CAUSE", Effect: "UNKNOWN_EFFECT", Severity: "WARNING",
		StartTime: base, IsTest: isTest,
	}, base)
	if err != nil {
		t.Fatalf("Create(%q): %v", header, err)
	}
	if err := repo.SetPublished(ctx, a.ID, true, base); err != nil {
		t.Fatalf("SetPublished(%q): %v", header, err)
	}
	return a.ID
}

func doGet(t *testing.T, h http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, target, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func entityIDs(msg *gtfs.FeedMessage) []string {
	ids := make([]string, len(msg.GetEntity()))
	for i, e := range msg.GetEntity() {
		ids[i] = e.GetId()
	}
	return ids
}

func TestFeedBinary_KnownRegion(t *testing.T) {
	t.Parallel()
	h, alertRepo, regionRepo := newTestServer(t)
	putRegion(t, regionRepo, 1)
	id := publishAlert(t, alertRepo, 1, "Route 44 detoured", false)

	rec := doGet(t, h, "/api/v1/regions/1/alerts")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Errorf("Content-Type = %q, want application/octet-stream", ct)
	}

	var msg gtfs.FeedMessage
	if err := proto.Unmarshal(rec.Body.Bytes(), &msg); err != nil {
		t.Fatalf("proto.Unmarshal: %v", err)
	}
	want := []string{"Alert_" + itoa(id)}
	if got := entityIDs(&msg); !cmp.Equal(got, want) {
		t.Errorf("entity ids = %v, want %v", got, want)
	}
}

func TestFeedBinary_SlugRegion(t *testing.T) {
	t.Parallel()
	h, alertRepo, regionRepo := newTestServer(t)
	putRegion(t, regionRepo, 1)
	id := publishAlert(t, alertRepo, 1, "Route 44 detoured", false)

	rec := doGet(t, h, "/api/v1/regions/1-puget-sound/alerts")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	var msg gtfs.FeedMessage
	if err := proto.Unmarshal(rec.Body.Bytes(), &msg); err != nil {
		t.Fatalf("proto.Unmarshal: %v", err)
	}
	want := []string{"Alert_" + itoa(id)}
	if got := entityIDs(&msg); !cmp.Equal(got, want) {
		t.Errorf("entity ids = %v, want %v", got, want)
	}
}

func TestFeedBinary_UnknownRegion(t *testing.T) {
	t.Parallel()
	h, _, _ := newTestServer(t)

	rec := doGet(t, h, "/api/v1/regions/999/alerts")
	assertNotFound(t, rec)
}

// TestFeedBinary_MalformedSegments drives every failing row of the
// ParseRegionSegment table that is reachable as a real HTTP path through the
// handler: the design spec requires unrecognised region identifiers to be a
// normal 404, never a 500, and this is the assertion that would catch a
// handler that panics or 500s on a segment with no leading digits or one
// that overflows int64.
//
// The table's "" case is exercised directly by TestParseRegionSegment
// instead: an empty path segment can't reach the handler over real HTTP --
// stdlib's ServeMux cleans "/api/v1/regions//alerts" to
// "/api/v1/regions/alerts" and issues a 307 redirect before routing, so
// PathValue("regionId") is never actually empty in a served request.
func TestFeedBinary_MalformedSegments(t *testing.T) {
	t.Parallel()
	h, _, _ := newTestServer(t)

	for _, seg := range []string{"92233720368547758081-x", "abc", "-1", "%2B1"} {
		t.Run(seg, func(t *testing.T) {
			t.Parallel()
			rec := doGet(t, h, "/api/v1/regions/"+seg+"/alerts")
			assertNotFound(t, rec)
		})
	}
}

func TestFeedText(t *testing.T) {
	t.Parallel()
	h, alertRepo, regionRepo := newTestServer(t)
	putRegion(t, regionRepo, 1)
	publishAlert(t, alertRepo, 1, "Route 44 detoured", false)

	rec := doGet(t, h, "/api/v1/regions/1/alerts.pbtext")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/plain" {
		t.Errorf("Content-Type = %q, want text/plain", ct)
	}

	var gotMsg, wantMsg gtfs.FeedMessage
	if err := protojson.Unmarshal(rec.Body.Bytes(), &gotMsg); err != nil {
		t.Fatalf("protojson.Unmarshal: %v", err)
	}

	binRec := doGet(t, h, "/api/v1/regions/1/alerts")
	if err := proto.Unmarshal(binRec.Body.Bytes(), &wantMsg); err != nil {
		t.Fatalf("proto.Unmarshal: %v", err)
	}

	if diff := cmp.Diff(&wantMsg, &gotMsg, protocmp.Transform()); diff != "" {
		t.Errorf("pbtext message mismatch (-binary +pbtext):\n%s", diff)
	}
}

func TestFeed_UnpublishedNeverAppears(t *testing.T) {
	t.Parallel()
	h, alertRepo, regionRepo := newTestServer(t)
	putRegion(t, regionRepo, 1)

	ctx := context.Background()
	draft, err := alertRepo.Create(ctx, alerts.NewAlert{
		RegionID: 1, AgencyID: "40", HeaderText: "Draft alert",
		Cause: "UNKNOWN_CAUSE", Effect: "UNKNOWN_EFFECT", Severity: "WARNING",
		StartTime: base,
	}, base)
	if err != nil {
		t.Fatalf("Create(draft): %v", err)
	}

	rec := doGet(t, h, "/api/v1/regions/1/alerts")
	var msg gtfs.FeedMessage
	if err := proto.Unmarshal(rec.Body.Bytes(), &msg); err != nil {
		t.Fatalf("proto.Unmarshal: %v", err)
	}
	for _, id := range entityIDs(&msg) {
		if id == "Alert_"+itoa(draft.ID) {
			t.Fatalf("unpublished alert %d appeared in feed: %v", draft.ID, entityIDs(&msg))
		}
	}
}

func TestFeed_TestAlertSemantics(t *testing.T) {
	t.Parallel()
	h, alertRepo, regionRepo := newTestServer(t)
	putRegion(t, regionRepo, 1)
	normalID := publishAlert(t, alertRepo, 1, "Normal alert", false)
	testID := publishAlert(t, alertRepo, 1, "Test alert", true)

	cases := []struct {
		name       string
		query      string
		wantTest   bool
		wantNormal bool // always true, asserted explicitly for clarity
	}{
		{"absent", "", false, true},
		{"test=1", "?test=1", true, true},
		{"test=0", "?test=0", true, true},        // any non-blank value includes test alerts
		{"test=blank", "?test=%20", false, true}, // whitespace-only is blank
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := doGet(t, h, "/api/v1/regions/1/alerts"+tc.query)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
			}
			var msg gtfs.FeedMessage
			if err := proto.Unmarshal(rec.Body.Bytes(), &msg); err != nil {
				t.Fatalf("proto.Unmarshal: %v", err)
			}
			ids := entityIDs(&msg)

			hasNormal := containsString(ids, "Alert_"+itoa(normalID))
			hasTest := containsString(ids, "Alert_"+itoa(testID))

			// This is the assertion that catches a broken predicate that
			// hides every real alert when test alerts are requested: it
			// must always find the non-test alert, regardless of ?test=.
			if hasNormal != tc.wantNormal {
				t.Errorf("normal alert present = %v, want %v (ids = %v)", hasNormal, tc.wantNormal, ids)
			}
			if hasTest != tc.wantTest {
				t.Errorf("test alert present = %v, want %v (ids = %v)", hasTest, tc.wantTest, ids)
			}
		})
	}
}

func assertNotFound(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if got, want := rec.Body.String(), `{"error":"Couldn't find Region"}`; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}

func containsString(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

func itoa(n int64) string {
	return strconv.FormatInt(n, 10)
}
