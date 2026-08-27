package ghostbus

import (
	"encoding/csv"
	"math"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/regions"
)

func csvBoolPtr(b bool) *bool           { return &b }
func csvInt64Ptr(v int64) *int64        { return &v }
func csvFloatPtr(v float64) *float64    { return &v }
func csvTimePtr(t time.Time) *time.Time { return &t }

// wantGhostBusHeader is the design §2.8 export column list, in order. It is
// a hardcoded literal independent of csv.go's ghostBusHeader var, so a
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

// requireCell asserts one named CSV cell's exact rendering.
func requireCell(t *testing.T, row []string, col, want string) {
	t.Helper()
	if got := ghostBusCol(t, row, col); got != want {
		t.Errorf("%s = %q, want %q", col, got, want)
	}
}

// ghostBusExportFixture carries the instants the fixture reports below were
// built from, so assertions re-derive expectations from the same values
// instead of hardcoding renderings.
type ghostBusExportFixture struct {
	loc             *time.Location
	serviceDateTime time.Time
	createdEarly    time.Time
	region          regions.Region
}

// ghostBusExportReports builds the three canonical export fixtures directly
// as Report values (this package cannot import internal/store/sqlite --
// that package imports this one): R1 "full" (every optional field, a
// captured snapshot carrying formula-injection payloads), R2 "bare"
// (required fields only, pending snapshot, same trip instance as R1 so
// trip_report_count == 2), and R3 "zeroless" (captured snapshot with
// "position" but no "lastKnownLocation" and no stop coordinates -- the Null
// Island guard case).
func ghostBusExportReports(t *testing.T) (ghostBusExportFixture, []Report) {
	t.Helper()
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}

	// service_date is an independent epoch-ms field (the rider's service
	// day), distinct from created_at -- its local rendering is pinned from
	// the same zone the caller resolves through region.Timezone. 03:00 UTC
	// is 20:00 the PREVIOUS day in America/Los_Angeles (UTC-7 in August),
	// so the UTC and local calendar dates genuinely disagree here -- unlike
	// a noon-UTC instant, which lands on the same date in both zones and so
	// would pass that assertion even if reportRow rendered service_date in
	// UTC instead of the region's local zone.
	fx := ghostBusExportFixture{
		loc:             loc,
		serviceDateTime: time.Date(2026, 8, 10, 3, 0, 0, 0, time.UTC),
		createdEarly:    time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC),
		region:          regions.Region{ID: 1, Timezone: "America/Los_Angeles"},
	}
	serviceDateMs := fx.serviceDateTime.UnixMilli()
	predictionLastUpdated := time.Date(2026, 8, 10, 11, 0, 0, 0, time.UTC).UnixMilli()
	scheduledArrival := predictionLastUpdated + 30*60000 // 30 minutes later, in ms
	createdLate := fx.createdEarly.Add(2 * time.Hour)

	r1Snapshot := `{"current_time":1,"status":{"phase":"in_progress","lastKnownLocation":{"lat":47.61,"lon":-122.34}},"display":{"route_short_name":"44","headsign":"=HYPERLINK(evil)","stop_name":"Stop A","stop_lat":47.668,"stop_lon":-122.376,"route_type":3}}`
	r1 := Report{
		ID:                       1,
		RegionID:                 1,
		PublicID:                 "tok_full_000000000001",
		UserIdentifier:           "user-a",
		TripIdentifier:           "trip-common",
		ServiceDate:              serviceDateMs,
		RouteIdentifier:          "1_44",
		StopIdentifier:           "1_570",
		VehicleIdentifier:        "1_4361",
		StopSequence:             csvInt64Ptr(3),
		Predicted:                csvBoolPtr(true),
		ScheduleDeviationMinutes: csvInt64Ptr(5),
		WaitDurationMinutes:      15,
		Comment:                  "=1+1",
		UserLatitude:             csvFloatPtr(47.6),
		UserLongitude:            csvFloatPtr(-122.3),
		ScheduledArrivalAt:       csvInt64Ptr(scheduledArrival),
		PredictedArrivalAt:       csvInt64Ptr(scheduledArrival),
		PredictionLastUpdatedAt:  csvInt64Ptr(predictionLastUpdated),
		SnapshotStatus:           SnapshotCaptured,
		SnapshotJSON:             r1Snapshot,
		SnapshotCapturedAt:       csvTimePtr(fx.createdEarly.Add(time.Minute)),
		CreatedAt:                fx.createdEarly,
	}

	r2 := Report{
		ID:                  2,
		RegionID:            1,
		PublicID:            "tok_bare_000000000001",
		UserIdentifier:      "user-b",
		TripIdentifier:      "trip-common",
		ServiceDate:         serviceDateMs,
		WaitDurationMinutes: 15,
		SnapshotStatus:      SnapshotPending,
		CreatedAt:           fx.createdEarly,
	}

	r3Snapshot := `{"status":{"phase":"scheduled","position":{"lat":47.61,"lon":-122.34}},"display":{"route_short_name":"10","headsign":"Downtown","stop_name":"Stop B"}}`
	r3 := Report{
		ID:                  3,
		RegionID:            1,
		PublicID:            "tok_zero_000000000001",
		UserIdentifier:      "user-c",
		TripIdentifier:      "trip-zeroless",
		ServiceDate:         serviceDateMs,
		WaitDurationMinutes: 10,
		SnapshotStatus:      SnapshotCaptured,
		SnapshotJSON:        r3Snapshot,
		SnapshotCapturedAt:  csvTimePtr(createdLate.Add(time.Minute)),
		CreatedAt:           createdLate,
	}

	return fx, []Report{r1, r2, r3}
}

// assertGhostBusFullRow checks R1: injection guards on both rider- and
// snapshot-sourced cells, the ms staleness divisor, region-local time
// renderings, the trip-instance count, and the haversine distance.
func assertGhostBusFullRow(t *testing.T, row []string, fx ghostBusExportFixture) {
	t.Helper()
	requireCell(t, row, "predicted", "true")
	requireCell(t, row, "comment", "'=1+1")                   // formula-injection guard
	requireCell(t, row, "headsign", "'=HYPERLINK(evil)")      // guard on snapshot-sourced cells too
	requireCell(t, row, "prediction_staleness_minutes", "30") // ms divisor regression guard
	// Local renderings re-derived through the same loc the caller resolves
	// via region.Timezone; both differ from the UTC rendering (see the
	// fixture comment on the 03:00 UTC choice).
	requireCell(t, row, "reported_at_local", fx.createdEarly.In(fx.loc).Format(time.RFC3339))
	requireCell(t, row, "service_date", fx.serviceDateTime.In(fx.loc).Format("2006-01-02"))
	requireCell(t, row, "trip_report_count", "2")

	gotDist, err := strconv.ParseFloat(ghostBusCol(t, row, "vehicle_distance_from_stop_m"), 64)
	if err != nil {
		t.Fatalf("R1 vehicle_distance_from_stop_m parse: %v", err)
	}
	wantDist := HaversineMeters(47.61, -122.34, 47.668, -122.376)
	if math.Abs(gotDist-wantDist)/wantDist > 0.2 {
		t.Errorf("R1 vehicle_distance_from_stop_m = %v, want within 20%% of %v", gotDist, wantDist)
	}
}

// assertGhostBusBareRow checks R2: everything optional or snapshot-derived
// renders blank, while the trip-instance count still pairs it with R1.
func assertGhostBusBareRow(t *testing.T, row []string) {
	t.Helper()
	requireCell(t, row, "predicted", "")
	requireCell(t, row, "snapshot_status", SnapshotPending)
	requireCell(t, row, "prediction_staleness_minutes", "")
	requireCell(t, row, "route_short_name", "") // no snapshot
	requireCell(t, row, "trip_report_count", "2")
}

// assertGhostBusZerolessRow checks R3: distance blanks when stop
// coordinates are absent (Null Island guard) while the position fallback
// still renders the vehicle columns.
func assertGhostBusZerolessRow(t *testing.T, row []string) {
	t.Helper()
	requireCell(t, row, "vehicle_distance_from_stop_m", "")
	requireCell(t, row, "vehicle_last_lat", "47.61")
}

func TestWriteReportsCSV(t *testing.T) {
	t.Parallel()
	fx, reports := ghostBusExportReports(t)

	var buf strings.Builder
	if err := WriteReportsCSV(&buf, fx.region, reports); err != nil {
		t.Fatalf("WriteReportsCSV: %v", err)
	}
	rows, err := csv.NewReader(strings.NewReader(buf.String())).ReadAll()
	if err != nil {
		t.Fatalf("parse csv: %v (out=%q)", err, buf.String())
	}
	if len(rows) != 4 {
		t.Fatalf("rows = %d, want 4 (header + 3 reports); out=%q", len(rows), buf.String())
	}
	if !reflect.DeepEqual(rows[0], wantGhostBusHeader) {
		t.Fatalf("header = %v, want %v", rows[0], wantGhostBusHeader)
	}

	byPublicID := map[string][]string{}
	for _, row := range rows[1:] {
		byPublicID[row[0]] = row
	}
	for id, assert := range map[string]func(*testing.T, []string){
		"tok_full_000000000001": func(t *testing.T, row []string) { assertGhostBusFullRow(t, row, fx) },
		"tok_bare_000000000001": assertGhostBusBareRow,
		"tok_zero_000000000001": assertGhostBusZerolessRow,
	} {
		row, ok := byPublicID[id]
		if !ok {
			t.Fatalf("row %s missing; rows=%v", id, rows)
		}
		assert(t, row)
	}
}

// TestWriteReportsCSV_UnloadableTimezoneFallsBackToUTC pins the fallback a
// missing/invalid region timezone takes: the export must still succeed,
// rendering the UTC-equivalent local columns rather than failing outright.
func TestWriteReportsCSV_UnloadableTimezoneFallsBackToUTC(t *testing.T) {
	t.Parallel()
	created := time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC)
	reports := []Report{{
		PublicID:            "tok_bare_000000000001",
		TripIdentifier:      "trip-x",
		ServiceDate:         created.UnixMilli(),
		WaitDurationMinutes: 10,
		CreatedAt:           created,
	}}

	var buf strings.Builder
	if err := WriteReportsCSV(&buf, regions.Region{ID: 2, Timezone: ""}, reports); err != nil {
		t.Fatalf("WriteReportsCSV: %v", err)
	}
	rows, err := csv.NewReader(strings.NewReader(buf.String())).ReadAll()
	if err != nil {
		t.Fatalf("parse csv: %v", err)
	}
	requireCell(t, rows[1], "reported_at_local", created.UTC().Format(time.RFC3339))
}

// TestWriteReportsCSV_MalformedSnapshotBlanksDerivedColumns pins
// reportRow's degrade-to-blank behavior on a malformed-but-partially-
// parseable snapshot document. The stored document is syntactically valid
// JSON but has the wrong type for status.position (a number where an
// object is expected), so encoding/json's decoder populates display and
// status.phase/lastKnownLocation *before* it reaches the type error on
// position -- json.Unmarshal returns an error, but (unlike a fully
// destination-untouched failure) the destination struct is left partially
// filled in. reportRow must discard that partial result wholesale: every
// derived column blank, not a mix of "some snapshot fields happened to
// decode before the error." The raw snapshot_json cell still carries the
// stored string through csvsafe.Cell, since that column exists precisely
// so a malformed document doesn't vanish from the export.
func TestWriteReportsCSV_MalformedSnapshotBlanksDerivedColumns(t *testing.T) {
	t.Parallel()

	// Valid JSON syntax; "status.position" is a number, not an object, so
	// json.Unmarshal fails there -- but only after display and
	// status.phase/lastKnownLocation have already been decoded into the
	// target struct.
	const malformed = `{"display":{"route_short_name":"44","headsign":"Downtown Local","stop_name":"Main St","stop_lat":47.6,"stop_lon":-122.3},"status":{"phase":"in_progress","lastKnownLocation":{"lat":47.61,"lon":-122.34},"position":123}}`

	report := Report{
		PublicID:            "tok_malf_000000000001",
		TripIdentifier:      "trip-malformed",
		ServiceDate:         time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC).UnixMilli(),
		WaitDurationMinutes: 10,
		SnapshotStatus:      SnapshotCaptured,
		SnapshotJSON:        malformed,
		SnapshotCapturedAt:  csvTimePtr(time.Date(2026, 8, 10, 9, 1, 0, 0, time.UTC)),
		CreatedAt:           time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC),
	}

	var buf strings.Builder
	if err := WriteReportsCSV(&buf, regions.Region{ID: 1, Timezone: "America/Los_Angeles"}, []Report{report}); err != nil {
		t.Fatalf("WriteReportsCSV: %v", err)
	}
	rows, err := csv.NewReader(strings.NewReader(buf.String())).ReadAll()
	if err != nil {
		t.Fatalf("parse csv: %v (out=%q)", err, buf.String())
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (header + 1 report); out=%q", len(rows), buf.String())
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
	if got := ghostBusCol(t, row, "snapshot_status"); got != SnapshotCaptured {
		t.Errorf("snapshot_status = %q, want %q", got, SnapshotCaptured)
	}
}
