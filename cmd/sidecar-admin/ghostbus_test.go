package main

import (
	"context"
	"encoding/csv"
	"math"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/ghostbus"
	"github.com/OneBusAway/sidecar/internal/regions"
)

func boolPtr(b bool) *bool        { return &b }
func int64Ptr(v int64) *int64     { return &v }
func floatPtr(v float64) *float64 { return &v }

// seedGhostBusRegion inserts a directory-sourced region row and then sets its
// timezone through the same repository path `region set` uses -- ghost bus
// export needs a real IANA zone to render reported_at_local/service_date.
func seedGhostBusRegion(t *testing.T, repo regions.Repository, id int64, tz string) {
	t.Helper()
	seedRegion(t, repo, id)
	if err := repo.SetLocalFields(context.Background(), id, regions.LocalFields{Timezone: tz}, time.Now()); err != nil {
		t.Fatalf("SetLocalFields(%d): %v", id, err)
	}
}

// wantGhostBusHeader is the design §2.8 export column list, in order. It is
// a hardcoded literal independent of ghostbus.go's ghostBusHeader var, so a
// column reordered or dropped in the implementation is actually caught here
// rather than the assertion trivially agreeing with itself.
var wantGhostBusHeader = []string{
	"public_identifier", "reported_at_utc", "reported_at_local", "service_date",
	"route_id", "route_short_name", "headsign", "trip_id", "trip_report_count",
	"stop_id", "stop_name", "stop_sequence", "vehicle_id",
	"predicted", "wait_duration_minutes", "comment",
	"scheduled_arrival_utc", "predicted_arrival_utc", "schedule_deviation_minutes",
	"prediction_last_updated_utc", "prediction_staleness_minutes",
	"user_latitude", "user_longitude",
	"snapshot_status", "snapshot_captured_at_utc",
	"vehicle_last_lat", "vehicle_last_lon", "vehicle_distance_from_stop_m",
	"snapshot_trip_phase", "snapshot_json",
}

// ghostBusCol looks up column name in row using wantGhostBusHeader's order.
// Callers only reach this after asserting rows[0] equals wantGhostBusHeader,
// so the index lookup is safe.
func ghostBusCol(t *testing.T, row []string, name string) string {
	t.Helper()
	for i, h := range wantGhostBusHeader {
		if h == name {
			return row[i]
		}
	}
	t.Fatalf("unknown column %q", name)
	return ""
}

func TestGhostBusExportCSV(t *testing.T) {
	t.Parallel()
	dbPath, store := newDB(t)
	seedGhostBusRegion(t, store.Regions(), 1, "America/Los_Angeles")

	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}

	// service_date is an independent epoch-ms field (the rider's service
	// day), distinct from created_at -- pin its local rendering from the
	// same zone the CLI resolves through region.Timezone. 03:00 UTC is
	// 20:00 the PREVIOUS day in America/Los_Angeles (UTC-7 in August), so
	// the UTC and local calendar dates genuinely disagree here -- unlike a
	// noon-UTC instant, which lands on the same date in both zones and so
	// would pass this assertion even if ghostBusRow rendered service_date
	// in UTC instead of the region's local zone.
	serviceDateTime := time.Date(2026, 8, 10, 3, 0, 0, 0, time.UTC)
	serviceDateMs := serviceDateTime.UnixMilli()
	wantServiceDate := serviceDateTime.In(loc).Format("2006-01-02")

	predictionLastUpdated := time.Date(2026, 8, 10, 11, 0, 0, 0, time.UTC).UnixMilli()
	scheduledArrival := predictionLastUpdated + 30*60000 // 30 minutes later, in ms

	createdEarly := time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC)
	createdLate := createdEarly.Add(2 * time.Hour)

	ctx := context.Background()

	// R1 "full": every optional field set, captured snapshot with
	// formula-injection payloads in headsign and comment.
	r1Snapshot := `{"current_time":1,"status":{"phase":"in_progress","lastKnownLocation":{"lat":47.61,"lon":-122.34}},"display":{"route_short_name":"44","headsign":"=HYPERLINK(evil)","stop_name":"Stop A","stop_lat":47.668,"stop_lon":-122.376,"route_type":3}}`
	r1, err := store.GhostBus().Create(ctx, ghostbus.NewReport{
		RegionID:                 1,
		PublicID:                 "tok_full_000000000001",
		UserIdentifier:           "user-a",
		TripIdentifier:           "trip-common",
		ServiceDate:              serviceDateMs,
		RouteIdentifier:          "1_44",
		StopIdentifier:           "1_570",
		VehicleIdentifier:        "1_4361",
		StopSequence:             int64Ptr(3),
		Predicted:                boolPtr(true),
		ScheduleDeviationMinutes: int64Ptr(5),
		WaitDurationMinutes:      15,
		Comment:                  "=1+1",
		UserLatitude:             floatPtr(47.6),
		UserLongitude:            floatPtr(-122.3),
		ScheduledArrivalAt:       int64Ptr(scheduledArrival),
		PredictedArrivalAt:       int64Ptr(scheduledArrival),
		PredictionLastUpdatedAt:  int64Ptr(predictionLastUpdated),
	}, createdEarly)
	if err != nil {
		t.Fatalf("create R1: %v", err)
	}
	if err = store.GhostBus().MarkSnapshotCaptured(ctx, r1.ID, r1Snapshot, createdEarly.Add(time.Minute)); err != nil {
		t.Fatalf("mark R1 captured: %v", err)
	}

	// R2 "bare": only required fields, same (trip, service_date) as R1 so
	// trip_report_count == 2 for both. Snapshot stays pending.
	if _, err = store.GhostBus().Create(ctx, ghostbus.NewReport{
		RegionID:            1,
		PublicID:            "tok_bare_000000000001",
		UserIdentifier:      "user-b",
		TripIdentifier:      "trip-common",
		ServiceDate:         serviceDateMs,
		WaitDurationMinutes: 15,
	}, createdEarly); err != nil {
		t.Fatalf("create R2: %v", err)
	}

	// R3 "zeroless": captured snapshot whose status has "position" but no
	// "lastKnownLocation", and whose display carries no stop coordinates --
	// the Null Island guard must blank the distance while still rendering
	// vehicle_last_lat/lon from position.
	r3Snapshot := `{"status":{"phase":"scheduled","position":{"lat":47.61,"lon":-122.34}},"display":{"route_short_name":"10","headsign":"Downtown","stop_name":"Stop B"}}`
	r3, err := store.GhostBus().Create(ctx, ghostbus.NewReport{
		RegionID:            1,
		PublicID:            "tok_zero_000000000001",
		UserIdentifier:      "user-c",
		TripIdentifier:      "trip-zeroless",
		ServiceDate:         serviceDateMs,
		WaitDurationMinutes: 10,
	}, createdLate)
	if err != nil {
		t.Fatalf("create R3: %v", err)
	}
	if err = store.GhostBus().MarkSnapshotCaptured(ctx, r3.ID, r3Snapshot, createdLate.Add(time.Minute)); err != nil {
		t.Fatalf("mark R3 captured: %v", err)
	}

	stdout, _, err := cli(t, dbPath, "ghostbus", "export", "--region", "1")
	if err != nil {
		t.Fatalf("export: %v", err)
	}

	rows, err := csv.NewReader(strings.NewReader(stdout)).ReadAll()
	if err != nil {
		t.Fatalf("parse csv: %v (stdout=%q)", err, stdout)
	}
	if len(rows) != 4 {
		t.Fatalf("rows = %d, want 4 (header + 3 reports); stdout=%q", len(rows), stdout)
	}
	if !reflect.DeepEqual(rows[0], wantGhostBusHeader) {
		t.Fatalf("header = %v, want %v", rows[0], wantGhostBusHeader)
	}

	byPublicID := map[string][]string{}
	for _, row := range rows[1:] {
		byPublicID[row[0]] = row
	}

	r1Row, ok := byPublicID["tok_full_000000000001"]
	if !ok {
		t.Fatalf("R1 row missing; rows=%v", rows)
	}
	if got := ghostBusCol(t, r1Row, "predicted"); got != "true" {
		t.Errorf("R1 predicted = %q, want true", got)
	}
	if got := ghostBusCol(t, r1Row, "comment"); got != "'=1+1" {
		t.Errorf("R1 comment = %q, want '=1+1 (formula-injection guard)", got)
	}
	if got := ghostBusCol(t, r1Row, "headsign"); got != "'=HYPERLINK(evil)" {
		t.Errorf("R1 headsign = %q, want '=HYPERLINK(evil) (injection guard on snapshot-sourced cells too)", got)
	}
	if got := ghostBusCol(t, r1Row, "prediction_staleness_minutes"); got != "30" {
		t.Errorf("R1 prediction_staleness_minutes = %q, want 30 (ms divisor regression guard)", got)
	}
	// reported_at_local must render in the region's zone (America/Los_Angeles,
	// -07:00 in August), not UTC -- computed here via the same loc the CLI
	// resolves through region.Timezone rather than a hardcoded literal, but
	// it still differs from the UTC rendering of the same instant.
	wantReportedAtLocal := createdEarly.In(loc).Format(time.RFC3339)
	if got := ghostBusCol(t, r1Row, "reported_at_local"); got != wantReportedAtLocal {
		t.Errorf("R1 reported_at_local = %q, want %q", got, wantReportedAtLocal)
	}
	if got := ghostBusCol(t, r1Row, "service_date"); got != wantServiceDate {
		t.Errorf("R1 service_date = %q, want %q", got, wantServiceDate)
	}
	if got := ghostBusCol(t, r1Row, "trip_report_count"); got != "2" {
		t.Errorf("R1 trip_report_count = %q, want 2", got)
	}
	gotDist, err := strconv.ParseFloat(ghostBusCol(t, r1Row, "vehicle_distance_from_stop_m"), 64)
	if err != nil {
		t.Fatalf("R1 vehicle_distance_from_stop_m parse: %v", err)
	}
	wantDist := ghostbus.HaversineMeters(47.61, -122.34, 47.668, -122.376)
	if math.Abs(gotDist-wantDist)/wantDist > 0.2 {
		t.Errorf("R1 vehicle_distance_from_stop_m = %v, want within 20%% of %v", gotDist, wantDist)
	}

	r2Row, ok := byPublicID["tok_bare_000000000001"]
	if !ok {
		t.Fatalf("R2 row missing; rows=%v", rows)
	}
	if got := ghostBusCol(t, r2Row, "predicted"); got != "" {
		t.Errorf("R2 predicted = %q, want blank", got)
	}
	if got := ghostBusCol(t, r2Row, "snapshot_status"); got != ghostbus.SnapshotPending {
		t.Errorf("R2 snapshot_status = %q, want %q", got, ghostbus.SnapshotPending)
	}
	if got := ghostBusCol(t, r2Row, "prediction_staleness_minutes"); got != "" {
		t.Errorf("R2 prediction_staleness_minutes = %q, want blank", got)
	}
	if got := ghostBusCol(t, r2Row, "route_short_name"); got != "" {
		t.Errorf("R2 route_short_name = %q, want blank (no snapshot)", got)
	}
	if got := ghostBusCol(t, r2Row, "trip_report_count"); got != "2" {
		t.Errorf("R2 trip_report_count = %q, want 2", got)
	}

	r3Row, ok := byPublicID["tok_zero_000000000001"]
	if !ok {
		t.Fatalf("R3 row missing; rows=%v", rows)
	}
	if got := ghostBusCol(t, r3Row, "vehicle_distance_from_stop_m"); got != "" {
		t.Errorf("R3 vehicle_distance_from_stop_m = %q, want blank (Null Island guard)", got)
	}
	if got := ghostBusCol(t, r3Row, "vehicle_last_lat"); got != "47.61" {
		t.Errorf("R3 vehicle_last_lat = %q, want 47.61", got)
	}

	// --since between R1/R2's created_at and R3's: only R3 exported.
	since := createdEarly.Add(time.Hour).Format(time.RFC3339)
	stdoutSince, _, err := cli(t, dbPath, "ghostbus", "export", "--region", "1", "--since", since)
	if err != nil {
		t.Fatalf("export --since: %v", err)
	}
	rowsSince, err := csv.NewReader(strings.NewReader(stdoutSince)).ReadAll()
	if err != nil {
		t.Fatalf("parse csv: %v", err)
	}
	if len(rowsSince) != 2 || rowsSince[1][0] != "tok_zero_000000000001" {
		t.Fatalf("--since rows = %v, want header + R3 only", rowsSince)
	}

	// Unknown region: run() must return an error mentioning the region.
	if _, _, err := cli(t, dbPath, "ghostbus", "export", "--region", "999"); err == nil || !strings.Contains(err.Error(), "region") {
		t.Errorf("unknown region err = %v, want an error mentioning region", err)
	}
}

// TestGhostBusExportCSV_MalformedSnapshotBlanksDerivedColumns pins
// ghostBusRow's degrade-to-blank behavior on a malformed-but-partially-
// parseable snapshot document. The stored document is syntactically valid
// JSON but has the wrong type for status.position (a number where an
// object is expected), so encoding/json's decoder populates display and
// status.phase/lastKnownLocation *before* it reaches the type error on
// position -- json.Unmarshal returns an error, but (unlike a fully
// destination-untouched failure) the destination struct is left partially
// filled in. ghostBusRow must discard that partial result wholesale: every
// derived column blank, not a mix of "some snapshot fields happened to
// decode before the error." The raw snapshot_json cell still carries the
// stored string through csvCell, since that column exists precisely so a
// malformed document doesn't vanish from the export.
func TestGhostBusExportCSV_MalformedSnapshotBlanksDerivedColumns(t *testing.T) {
	t.Parallel()
	dbPath, store := newDB(t)
	seedGhostBusRegion(t, store.Regions(), 1, "America/Los_Angeles")
	ctx := context.Background()

	// Valid JSON syntax; "status.position" is a number, not an object, so
	// json.Unmarshal fails there -- but only after display and
	// status.phase/lastKnownLocation have already been decoded into the
	// target struct.
	const malformed = `{"display":{"route_short_name":"44","headsign":"Downtown Local","stop_name":"Main St","stop_lat":47.6,"stop_lon":-122.3},"status":{"phase":"in_progress","lastKnownLocation":{"lat":47.61,"lon":-122.34},"position":123}}`

	rep, err := store.GhostBus().Create(ctx, ghostbus.NewReport{
		RegionID:            1,
		PublicID:            "tok_malf_000000000001",
		UserIdentifier:      "user-m",
		TripIdentifier:      "trip-malformed",
		ServiceDate:         time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC).UnixMilli(),
		WaitDurationMinutes: 10,
	}, time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err = store.GhostBus().MarkSnapshotCaptured(ctx, rep.ID, malformed, time.Date(2026, 8, 10, 9, 1, 0, 0, time.UTC)); err != nil {
		t.Fatalf("mark captured: %v", err)
	}

	stdout, _, err := cli(t, dbPath, "ghostbus", "export", "--region", "1")
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	rows, err := csv.NewReader(strings.NewReader(stdout)).ReadAll()
	if err != nil {
		t.Fatalf("parse csv: %v (stdout=%q)", err, stdout)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (header + 1 report); stdout=%q", len(rows), stdout)
	}
	row := rows[1]

	for _, col := range []string{
		"route_short_name", "headsign", "stop_name",
		"snapshot_trip_phase", "vehicle_last_lat", "vehicle_last_lon",
		"vehicle_distance_from_stop_m",
	} {
		if got := ghostBusCol(t, row, col); got != "" {
			t.Errorf("%s = %q, want blank (malformed snapshot must not leak a partial decode)", col, got)
		}
	}
	if got := ghostBusCol(t, row, "snapshot_json"); got != malformed {
		t.Errorf("snapshot_json = %q, want the raw stored document %q", got, malformed)
	}
	if got := ghostBusCol(t, row, "snapshot_status"); got != ghostbus.SnapshotCaptured {
		t.Errorf("snapshot_status = %q, want %q", got, ghostbus.SnapshotCaptured)
	}
}

// TestGhostBusExportRejectsNaiveSince pins design §2.8/§5: --since is an
// instant, and (like every other CLI timestamp in this repo) a naive
// datetime with no UTC offset must be rejected rather than silently
// interpreted in some default zone.
func TestGhostBusExportRejectsNaiveSince(t *testing.T) {
	t.Parallel()
	dbPath, store := newDB(t)
	seedGhostBusRegion(t, store.Regions(), 1, "America/Los_Angeles")

	_, _, err := cli(t, dbPath, "ghostbus", "export", "--region", "1", "--since", "2026-09-01T00:00:00")
	if err == nil || !strings.Contains(err.Error(), "explicit UTC offset") {
		t.Fatalf("naive --since err = %v, want an explicit UTC offset error", err)
	}
}

// TestGhostBusExportMissingRegionFlag pins that a missing (or explicit
// zero) --region is a usage error, not a "region 0: not found" store
// lookup -- --since alone must not satisfy the flag-parsing check that
// used to let this fall through to the store.
func TestGhostBusExportMissingRegionFlag(t *testing.T) {
	t.Parallel()
	dbPath, store := newDB(t)
	seedGhostBusRegion(t, store.Regions(), 1, "America/Los_Angeles")

	const wantUsage = "usage: ghostbus export --region N [--since RFC3339]"

	if _, _, err := cli(t, dbPath, "ghostbus", "export"); err == nil || err.Error() != wantUsage {
		t.Errorf("no flags err = %v, want %q", err, wantUsage)
	}
	if _, _, err := cli(t, dbPath, "ghostbus", "export", "--since", "2026-09-01T00:00:00Z"); err == nil || err.Error() != wantUsage {
		t.Errorf("--since with no --region err = %v, want %q", err, wantUsage)
	}
	if _, _, err := cli(t, dbPath, "ghostbus", "export", "--region", "0"); err == nil || err.Error() != wantUsage {
		t.Errorf("--region 0 err = %v, want %q", err, wantUsage)
	}

	// A nonzero, nonexistent region must still surface the store's
	// not-found error, not the usage message.
	if _, _, err := cli(t, dbPath, "ghostbus", "export", "--region", "999"); err == nil || !strings.Contains(err.Error(), "region") {
		t.Errorf("unknown region err = %v, want an error mentioning region", err)
	}
}
