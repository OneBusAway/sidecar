package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/ghostbus"
)

func gbInt64Ptr(v int64) *int64 { return &v }

// seedReport creates a ghost bus report directly through the repository,
// not the rider POST route: that route's CreatedAt is always h.deps.Now(),
// and these tests need reports at controlled instants (for ?since) and with
// controlled fields (for the epoch-millisecond and snapshot tests). label
// becomes the report's trip identifier and part of its public id, so each
// call names a distinct trip within a region.
func (f *adminFixture) seedReport(t *testing.T, regionID int64, label string, createdAt time.Time) ghostbus.Report {
	t.Helper()
	return f.seedReportWith(t, regionID, label, createdAt, func(*ghostbus.NewReport) {})
}

// seedReportWith is seedReport with a hook to set fields beyond the
// minimum: the epoch-millisecond fields, the comment, and so on.
func (f *adminFixture) seedReportWith(
	t *testing.T, regionID int64, label string, createdAt time.Time, mutate func(*ghostbus.NewReport),
) ghostbus.Report {
	t.Helper()
	in := ghostbus.NewReport{
		RegionID:            regionID,
		PublicID:            "tok_" + label,
		UserIdentifier:      "user-" + label,
		TripIdentifier:      "trip-" + label,
		ServiceDate:         createdAt.UnixMilli(),
		WaitDurationMinutes: 15,
	}
	mutate(&in)
	rep, err := f.store.GhostBus().Create(context.Background(), in, createdAt)
	if err != nil {
		t.Fatalf("seed ghost bus report %q: %v", label, err)
	}
	return rep
}

// TestAdminGhostBus_ListAndSince. `since` is optional (absent means all) and
// must carry an explicit UTC offset; a naive datetime is a 400, never
// interpreted in server-local time.
func TestAdminGhostBus_ListAndSince(t *testing.T) {
	t.Parallel()

	f := newFullAdminFixture(t)
	f.seedReport(t, regionPuget, "old", testNow.Add(-48*time.Hour))
	f.seedReport(t, regionPuget, "new", testNow)
	f.seedReport(t, regionTampa, "other-region", testNow)

	all := array(t, f.do(http.MethodGet, "/api/admin/v1/regions/1/ghost_bus_reports", ""), http.StatusOK)
	if len(all) != 2 {
		t.Fatalf("got %d reports, want 2 (region 0's must not appear)", len(all))
	}
	since := testNow.Add(-time.Hour).UTC().Format(time.RFC3339)
	recent := array(t, f.do(http.MethodGet,
		"/api/admin/v1/regions/1/ghost_bus_reports?since="+url.QueryEscape(since), ""), http.StatusOK)
	if len(recent) != 1 {
		t.Errorf("got %d reports since %s, want 1", len(recent), since)
	}
	for _, bad := range []string{"2026-08-27T00:00:00", "yesterday", "1756252800"} {
		if rec := f.do(http.MethodGet, "/api/admin/v1/regions/1/ghost_bus_reports?since="+url.QueryEscape(bad), ""); rec.Code != http.StatusBadRequest {
			t.Errorf("since=%q: status = %d, want 400", bad, rec.Code)
		}
	}
}

// TestAdminGhostBus_EpochMillisecondFieldsPassThrough. service_date and the
// three arrival timestamps are OBA identifiers and dedupe keys, not
// instants: they cross the wire as the integers they arrived as, and
// reformatting one as RFC 3339 would break the dedupe key (design spec
// section 5).
func TestAdminGhostBus_EpochMillisecondFieldsPassThrough(t *testing.T) {
	t.Parallel()

	f := newFullAdminFixture(t)
	const (
		serviceDate           = int64(1754809200000)
		scheduledArrivalAt    = int64(1754812800123)
		predictedArrivalAt    = int64(1754812845456)
		predictionLastUpdated = int64(1754812700789)
	)
	rep := f.seedReportWith(t, regionPuget, "epoch", testNow, func(in *ghostbus.NewReport) {
		in.ServiceDate = serviceDate
		in.ScheduledArrivalAt = gbInt64Ptr(scheduledArrivalAt)
		in.PredictedArrivalAt = gbInt64Ptr(predictedArrivalAt)
		in.PredictionLastUpdatedAt = gbInt64Ptr(predictionLastUpdated)
	})

	want := map[string]int64{
		"service_date":               serviceDate,
		"scheduled_arrival_at":       scheduledArrivalAt,
		"predicted_arrival_at":       predictedArrivalAt,
		"prediction_last_updated_at": predictionLastUpdated,
	}

	checkFields := func(t *testing.T, got map[string]any) {
		t.Helper()
		for field, wantVal := range want {
			// num() asserts the JSON value decodes as a float64; a handler
			// that mistakenly ran one of these through formatInstant would
			// emit a JSON string here instead, and this assertion would
			// fail with a type error rather than a value mismatch.
			if gotVal := int64(num(t, got, field)); gotVal != wantVal {
				t.Errorf("%s = %v, want the integer %d unchanged", field, got[field], wantVal)
			}
		}
	}

	t.Run("list", func(t *testing.T) {
		list := array(t, f.do(http.MethodGet, "/api/admin/v1/regions/1/ghost_bus_reports", ""), http.StatusOK)
		if len(list) != 1 {
			t.Fatalf("got %d reports, want 1", len(list))
		}
		checkFields(t, list[0])
	})

	t.Run("detail", func(t *testing.T) {
		got := object(t, f.do(http.MethodGet, "/api/admin/v1/regions/1/ghost_bus_reports/"+rep.PublicID, ""), http.StatusOK)
		checkFields(t, got)
	})
}

// TestAdminGhostBus_DetailCarriesTheRawSnapshot.
func TestAdminGhostBus_DetailCarriesTheRawSnapshot(t *testing.T) {
	t.Parallel()

	f := newFullAdminFixture(t)
	const snapshotDoc = `{"status":{"phase":"in_progress","lastKnownLocation":{"lat":47.61,"lon":-122.34}},"display":{"route_short_name":"44"}}`
	rep := f.seedReportWith(t, regionPuget, "snap", testNow, func(*ghostbus.NewReport) {})
	if err := f.store.GhostBus().MarkSnapshotCaptured(context.Background(), rep.ID, snapshotDoc, testNow); err != nil {
		t.Fatalf("mark captured: %v", err)
	}

	got := object(t, f.do(http.MethodGet, "/api/admin/v1/regions/1/ghost_bus_reports/"+rep.PublicID, ""), http.StatusOK)
	if v := str(t, got, "snapshot_status"); v != ghostbus.SnapshotCaptured {
		t.Errorf("snapshot_status = %q, want %q", v, ghostbus.SnapshotCaptured)
	}

	gotSnap, ok := got["snapshot_json"].(map[string]any)
	if !ok {
		t.Fatalf("snapshot_json = %v (%T), want a decoded object", got["snapshot_json"], got["snapshot_json"])
	}
	var wantSnap map[string]any
	if err := json.Unmarshal([]byte(snapshotDoc), &wantSnap); err != nil {
		t.Fatalf("decode want snapshot: %v", err)
	}
	if !reflect.DeepEqual(gotSnap, wantSnap) {
		t.Errorf("snapshot_json = %#v, want exactly the captured document %#v", gotSnap, wantSnap)
	}

	// A report in another region is 404: the region path segment is a
	// fence, not a filter, and GetByPublicID's own region_id condition is
	// what makes that true here.
	if rec := f.do(http.MethodGet, "/api/admin/v1/regions/0/ghost_bus_reports/"+rep.PublicID, ""); rec.Code != http.StatusNotFound {
		t.Errorf("foreign region: status = %d, want 404; body = %s", rec.Code, rec.Body.String())
	}

	// A pending report (no captured snapshot yet) must answer null, not an
	// empty object or an error.
	pending := f.seedReport(t, regionPuget, "pending", testNow)
	gotPending := object(t, f.do(http.MethodGet, "/api/admin/v1/regions/1/ghost_bus_reports/"+pending.PublicID, ""), http.StatusOK)
	if gotPending["snapshot_json"] != nil {
		t.Errorf("pending report snapshot_json = %v, want null", gotPending["snapshot_json"])
	}
}

// TestAdminGhostBus_CSVContract mirrors the survey CSV contract.
func TestAdminGhostBus_CSVContract(t *testing.T) {
	t.Parallel()

	f := newFullAdminFixture(t)
	f.seedReportWith(t, regionPuget, "csv", testNow, func(in *ghostbus.NewReport) {
		in.Comment = "=cmd|' /C calc'!A0"
	})

	rec := f.do(http.MethodGet, "/api/admin/v1/regions/1/ghost_bus_reports.csv", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	for header, want := range map[string]string{
		"Content-Type":           "text/csv",
		"X-Content-Type-Options": "nosniff",
		"Cache-Control":          "no-store",
		"Content-Disposition":    `attachment; filename="ghost-bus-reports-1.csv"`,
	} {
		if got := rec.Header().Get(header); !strings.HasPrefix(got, want) {
			t.Errorf("%s = %q, want a value starting with %q", header, got, want)
		}
	}
	// The guarded cell has no comma, quote, or newline of its own, so
	// encoding/csv leaves it unquoted -- the literal apostrophe-prefixed
	// text appears verbatim in the body, with no wrapping double quote to
	// look for (Task 9 learned this the hard way).
	if !strings.Contains(rec.Body.String(), "'=cmd") {
		t.Errorf("rider comment not defused:\n%s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "\n=cmd") || strings.Contains(rec.Body.String(), ",=cmd") {
		t.Errorf("rider comment reached the export unguarded:\n%s", rec.Body.String())
	}
}
