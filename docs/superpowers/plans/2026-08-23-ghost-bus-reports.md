# Ghost Bus Reports Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement spec §8 (ghost bus reports): the rider write endpoint with dedupe and both §2.6 throttles, an asynchronous trip-details enrichment snapshot worker, and a `sidecar-admin ghostbus export` CSV command.

**Architecture:** One new domain package (`internal/ghostbus`) following the established repository pattern — domain types + `Repository` interface in the domain package, sqlc-backed adapter in `internal/store/sqlite`, conformance suite in `internal/store/storetest`, stdlib handler in `internal/httpapi`, a DB-as-queue polling scheduler modeled on `alarms.Scheduler`, wiring in `cmd/sidecar`, and a CSV export in `cmd/sidecar-admin`. One new obaapi method (`TripDetails`) that prunes the upstream response from raw JSON.

**Tech Stack:** Go 1.26 stdlib HTTP, sqlc + goose on modernc SQLite, `github.com/OneBusAway/go-sdk` v1.15.0 (via `internal/obaapi` only).

**Spec:** `docs/superpowers/specs/2026-08-23-ghost-bus-reports-design.md` (the design argues from `specification/specification.md` §8, §2.2, §2.5, §2.6 and the `createGhostBusReport` operation in `specification/openapi.yaml`; read the design first — every deliberate divergence from OBACloud is recorded there).

## Global Constraints

- `time.Now`/`time.Sleep` are banned outside `cmd/` — every package takes `Now func() time.Time` (or a `time.Time` argument) injected. Storetest derives all times from its fixed `base` instant.
- Nothing outside `internal/store/sqlite` may see a `gen.*` type; nothing outside `internal/obaapi` may import the OBA go-sdk.
- **Never log `user_identifier`, coordinates, or comments** — device pseudonyms and rider locations (spec §13). Log region ids and counts.
- Error shapes (spec §2.5): unknown region → `404 {"error": "Couldn't find Region"}` (existing `writeRegionNotFound` via `resolveRegion`); validation → `422 {"error": "Unable to save report", "messages": […]}`; duplicate → `422 {"error": "already_reported", "messages": ["User has already reported this trip"]}` (both via existing `errorWithMessages`).
- Every POST accepts query params, form bodies, and JSON bodies (spec §2.2) — `parseRequestParams` already does this with body-first precedence.
- Timestamp columns are INTEGER. `service_date`, `scheduled_arrival_at`, `predicted_arrival_at`, `prediction_last_updated_at` are epoch **milliseconds** stored as received; `created_at`, `updated_at`, `snapshot_captured_at` are epoch **seconds** via `.Unix()`. Never DATETIME (modernc writes `time.Time.String()` into DATETIME cells and ORDER BY silently sorts text).
- sqlc queries use named args (`@name`) consistently — never mix named args with bare `?` (mixing silently misnumbers every parameter at runtime).
- Wait choices `[5, 10, 15, 20, 30]` and the 1,000-char comment cap are mirrored by hand in the iOS app; do not change them.
- The oversized-body `403` applies to **JSON bodies only** (declared or streamed past 8,192 bytes) and is **bodyless**. Form bodies keep the shared 64 KB `requestBodyLimit` — iOS percent-encoding can push a legal report past 8 KB.
- Dedupe-vs-token-collision discrimination is by constraint message text: `ErrDuplicate` **only** for `ghost_bus_reports.region_id` (the dedupe index); `ghost_bus_reports.public_identifier` is a token collision → re-mint once, never `already_reported`.
- Commands: `make generate` (sqlc), `go test ./internal/... ./cmd/...` (fast loop), `make check` (full CI parity — includes `make web`; a bare `go test ./...` fails the adminui embed test). Commit after every green task.
- House comment style: comments state constraints and rationale ("why"), not narration.
- Mutation discipline (repo memory): after writing each substantive test, mutate the implementation once to confirm the assertion fires, then revert.

---

### Task 1: Domain package — types, constants, errors, haversine

**Files:**
- Create: `internal/ghostbus/ghostbus.go`
- Create: `internal/ghostbus/haversine.go`
- Test: `internal/ghostbus/ghostbus_test.go`, `internal/ghostbus/haversine_test.go`

**Interfaces:**
- Consumes: nothing new (stdlib only).
- Produces (later tasks depend on these exact names):

```go
package ghostbus

const (
	CommentMaxLen       = 1000 // runes; mirrored in GhostBusReportView.swift
	MaxSnapshotAttempts = 3    // total tries, matching OBACloud retry_on attempts: 3

	SnapshotPending     = "pending"
	SnapshotCaptured    = "captured"
	SnapshotUnavailable = "unavailable"
)

var WaitDurationChoices = []int64{5, 10, 15, 20, 30}

var (
	ErrDuplicate      = errors.New("duplicate ghost bus report") // dedupe-index hit → already_reported 422
	ErrTokenCollision = errors.New("public identifier collision") // re-mint and retry once, never already_reported
	ErrNotFound       = errors.New("ghost bus report not found")
)

type Report struct {
	ID                       int64
	RegionID                 int64
	PublicID                 string
	UserIdentifier           string
	TripIdentifier           string
	ServiceDate              int64 // epoch ms, as received (dedupe key component)
	RouteIdentifier          string
	StopIdentifier           string
	VehicleIdentifier        string
	StopSequence             *int64
	Predicted                *bool // three-state: nil = client didn't say
	ScheduleDeviationMinutes *int64
	WaitDurationMinutes      int64
	Comment                  string
	UserLatitude             *float64
	UserLongitude            *float64
	ScheduledArrivalAt       *int64 // epoch ms
	PredictedArrivalAt       *int64 // epoch ms
	PredictionLastUpdatedAt  *int64 // epoch ms
	SnapshotStatus           string
	SnapshotJSON             string // "" until captured
	SnapshotCapturedAt       *time.Time
	SnapshotAttempts         int64
	CreatedAt                time.Time
}

// NewReport is the input to Repository.Create: a Report before the store
// assigns ID, snapshot bookkeeping, and CreatedAt.
type NewReport struct {
	RegionID                 int64
	PublicID                 string
	UserIdentifier           string
	TripIdentifier           string
	ServiceDate              int64
	RouteIdentifier          string
	StopIdentifier           string
	VehicleIdentifier        string
	StopSequence             *int64
	Predicted                *bool
	ScheduleDeviationMinutes *int64
	WaitDurationMinutes      int64
	Comment                  string
	UserLatitude             *float64
	UserLongitude            *float64
	ScheduledArrivalAt       *int64
	PredictedArrivalAt       *int64
	PredictionLastUpdatedAt  *int64
}

type Repository interface {
	Create(ctx context.Context, in NewReport, now time.Time) (Report, error) // ErrDuplicate | ErrTokenCollision
	ListPendingSnapshots(ctx context.Context, limit int64) ([]Report, error) // pending AND attempts < MaxSnapshotAttempts, oldest first
	MarkSnapshotCaptured(ctx context.Context, id int64, snapshotJSON string, now time.Time) error
	MarkSnapshotUnavailable(ctx context.Context, id int64, now time.Time) error
	// RecordSnapshotFailure increments snapshot_attempts and, when the
	// increment reaches MaxSnapshotAttempts, flips snapshot_status to
	// 'unavailable' in the same UPDATE. Returns the post-increment count.
	RecordSnapshotFailure(ctx context.Context, id int64, now time.Time) (int64, error)
	ListForExport(ctx context.Context, regionID int64, sinceUnix int64) ([]Report, error) // created_at >= sinceUnix; 0 = all
}

func ValidWaitDuration(v int64) bool
func HaversineMeters(lat1, lon1, lat2, lon2 float64) float64
```

- [ ] **Step 1: Write the failing tests**

`internal/ghostbus/ghostbus_test.go`:

```go
package ghostbus

import "testing"

func TestValidWaitDuration(t *testing.T) {
	for _, v := range []int64{5, 10, 15, 20, 30} {
		if !ValidWaitDuration(v) {
			t.Errorf("ValidWaitDuration(%d) = false, want true", v)
		}
	}
	// 25 is the classic off-by-one a range check would wrongly accept;
	// 0 and negatives guard the "absent coerced to zero" path.
	for _, v := range []int64{0, -5, 1, 25, 31, 300} {
		if ValidWaitDuration(v) {
			t.Errorf("ValidWaitDuration(%d) = true, want false", v)
		}
	}
}
```

`internal/ghostbus/haversine_test.go`:

```go
package ghostbus

import "math"
import "testing"

func TestHaversineMeters(t *testing.T) {
	// Pike Place Market to Space Needle: ~1,300 m. The tolerance is loose
	// because the test pins "sane great-circle math", not a specific
	// earth-radius constant.
	got := HaversineMeters(47.6097, -122.3422, 47.6205, -122.3493)
	if got < 1200 || got > 1450 {
		t.Errorf("Seattle distance = %f, want ~1300", got)
	}
	if d := HaversineMeters(47.6, -122.3, 47.6, -122.3); d != 0 {
		t.Errorf("zero distance = %f, want 0", d)
	}
	// Antipodal-ish sanity: half the earth's circumference, ~20,000 km.
	far := HaversineMeters(0, 0, 0, 180)
	if math.Abs(far-20015000) > 100000 {
		t.Errorf("antipodal distance = %f, want ~20015000", far)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/ghostbus/`
Expected: FAIL — package does not exist / undefined: `ValidWaitDuration`, `HaversineMeters`.

- [ ] **Step 3: Write the implementation**

`internal/ghostbus/ghostbus.go` — the full contents of the **Produces** block above, with this package doc and these bodies:

```go
// Package ghostbus implements the OneBusAway sidecar spec §8 (ghost bus
// reports): rider-filed "the app promised a bus that never came" records,
// deduped per trip instance and enriched asynchronously with a trip-details
// snapshot from the region's OBA server.
//
// WaitDurationChoices and CommentMaxLen are mirrored by hand in the iOS app
// (OBAKit/Trip/TripPage/GhostBusReportView.swift: waitChoices /
// commentMaxLength); a change here needs a matching client change.
package ghostbus
```

```go
// ValidWaitDuration reports whether v is one of the §8 choices. slices is
// fine here; the list is five entries.
func ValidWaitDuration(v int64) bool {
	return slices.Contains(WaitDurationChoices, v)
}
```

`internal/ghostbus/haversine.go`:

```go
package ghostbus

import "math"

// earthRadiusM matches OBACloud's Haversine helper so exported distances
// agree between the two implementations.
const earthRadiusM = 6371000.0

// HaversineMeters is the great-circle distance between two WGS84 points.
// Used only for the CSV's vehicle_distance_from_stop_m column; callers must
// not call it with defaulted zero coordinates (0,0 is a real place).
func HaversineMeters(lat1, lon1, lat2, lon2 float64) float64 {
	toRad := func(d float64) float64 { return d * math.Pi / 180 }
	dLat := toRad(lat2 - lat1)
	dLon := toRad(lon2 - lon1)
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(toRad(lat1))*math.Cos(toRad(lat2))*math.Sin(dLon/2)*math.Sin(dLon/2)
	return 2 * earthRadiusM * math.Asin(math.Sqrt(a))
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/ghostbus/`
Expected: PASS. Mutation check: change `25` handling by widening `ValidWaitDuration` to `v >= 5 && v <= 30` — the table test must fail on 25; revert.

- [ ] **Step 5: Commit**

```bash
git add internal/ghostbus
git commit -m "feat: ghostbus domain types, wait-choice validation, haversine (spec section 8)"
```

---

### Task 2: Migration and sqlc queries

**Files:**
- Create: `internal/store/sqlite/migrations/00007_ghost_bus_reports.sql`
- Create: `internal/store/sqlite/queries/ghostbus.sql`
- Generated (via `make generate`): `internal/store/sqlite/gen/ghostbus.sql.go` and friends

**Interfaces:**
- Consumes: goose migration conventions from `00006_surveys.sql` (AUTOINCREMENT, `ON DELETE CASCADE`, `updated_at`, CHECK on closed enums, `<table>_<col>_idx` naming).
- Produces: `gen.Queries` methods `CreateGhostBusReport`, `ListPendingSnapshotReports`, `MarkGhostBusSnapshotCaptured`, `MarkGhostBusSnapshotUnavailable`, `RecordGhostBusSnapshotFailure`, `ListGhostBusReportsForExport` — used only by Task 3's adapter.

- [ ] **Step 1: Write the migration**

`internal/store/sqlite/migrations/00007_ghost_bus_reports.sql`:

```sql
-- +goose Up
-- Spec §8. service_date and the *_arrival_at / prediction_last_updated_at
-- columns are epoch MILLISECONDS stored as received (service_date is a
-- dedupe key component; a lossy transform there would change dedupe
-- behavior). created_at / updated_at / snapshot_captured_at are epoch
-- seconds. INTEGER everywhere -- never DATETIME (modernc stores
-- time.Time.String() in DATETIME cells and ORDER BY sorts text).
CREATE TABLE ghost_bus_reports (
  id                         INTEGER PRIMARY KEY AUTOINCREMENT,
  region_id                  INTEGER NOT NULL REFERENCES regions(id) ON DELETE CASCADE,
  public_identifier          TEXT    NOT NULL,
  user_identifier            TEXT    NOT NULL,
  trip_identifier            TEXT    NOT NULL,
  service_date               INTEGER NOT NULL,
  route_identifier           TEXT    NOT NULL DEFAULT '',
  stop_identifier            TEXT    NOT NULL DEFAULT '',
  vehicle_identifier         TEXT    NOT NULL DEFAULT '',
  stop_sequence              INTEGER,
  predicted                  INTEGER,
  schedule_deviation_minutes INTEGER,
  wait_duration_minutes      INTEGER NOT NULL,
  comment                    TEXT    NOT NULL DEFAULT '',
  user_latitude              REAL,
  user_longitude             REAL,
  scheduled_arrival_at       INTEGER,
  predicted_arrival_at       INTEGER,
  prediction_last_updated_at INTEGER,
  snapshot_status            TEXT    NOT NULL DEFAULT 'pending'
    CHECK (snapshot_status IN ('pending', 'captured', 'unavailable')),
  snapshot_json              TEXT    NOT NULL DEFAULT '',
  snapshot_captured_at       INTEGER,
  snapshot_attempts          INTEGER NOT NULL DEFAULT 0,
  created_at                 INTEGER NOT NULL,
  updated_at                 INTEGER NOT NULL
);

CREATE UNIQUE INDEX ghost_bus_reports_public_identifier_idx
  ON ghost_bus_reports(public_identifier);

-- The §8 dedupe key. The adapter tells this index's violation apart from
-- the public_identifier one by the columns named in the error message;
-- region_id leading keeps that message distinctive.
CREATE UNIQUE INDEX ghost_bus_reports_dedupe_idx
  ON ghost_bus_reports(region_id, user_identifier, trip_identifier, service_date);

-- The snapshot worker's poll predicate, verbatim: the attempts guard is
-- what stops a crash-stranded row from being retried forever.
CREATE INDEX ghost_bus_reports_snapshot_pending_idx ON ghost_bus_reports(id)
  WHERE snapshot_status = 'pending' AND snapshot_attempts < 3;

CREATE INDEX ghost_bus_reports_region_created_idx
  ON ghost_bus_reports(region_id, created_at);

-- +goose Down
DROP TABLE ghost_bus_reports;
```

- [ ] **Step 2: Write the queries**

`internal/store/sqlite/queries/ghostbus.sql`:

```sql
-- name: CreateGhostBusReport :one
INSERT INTO ghost_bus_reports (
  region_id, public_identifier, user_identifier, trip_identifier, service_date,
  route_identifier, stop_identifier, vehicle_identifier, stop_sequence,
  predicted, schedule_deviation_minutes, wait_duration_minutes, comment,
  user_latitude, user_longitude,
  scheduled_arrival_at, predicted_arrival_at, prediction_last_updated_at,
  created_at, updated_at
) VALUES (
  @region_id, @public_identifier, @user_identifier, @trip_identifier, @service_date,
  @route_identifier, @stop_identifier, @vehicle_identifier, @stop_sequence,
  @predicted, @schedule_deviation_minutes, @wait_duration_minutes, @comment,
  @user_latitude, @user_longitude,
  @scheduled_arrival_at, @predicted_arrival_at, @prediction_last_updated_at,
  @now, @now
)
RETURNING *;

-- name: ListPendingSnapshotReports :many
SELECT * FROM ghost_bus_reports
WHERE snapshot_status = 'pending' AND snapshot_attempts < 3
ORDER BY id
LIMIT @max_rows;

-- name: MarkGhostBusSnapshotCaptured :exec
UPDATE ghost_bus_reports
SET snapshot_json = @snapshot_json, snapshot_status = 'captured',
    snapshot_captured_at = @now, updated_at = @now
WHERE id = @id;

-- name: MarkGhostBusSnapshotUnavailable :exec
UPDATE ghost_bus_reports
SET snapshot_status = 'unavailable', updated_at = @now
WHERE id = @id;

-- The cap check and the status flip are one UPDATE deliberately: a crash
-- between "increment" and "mark unavailable" must not leave a row that is
-- both at the cap and still pending (the poll predicate would skip it, but
-- nothing would ever resolve it either).
-- name: RecordGhostBusSnapshotFailure :one
UPDATE ghost_bus_reports
SET snapshot_attempts = snapshot_attempts + 1,
    snapshot_status = CASE WHEN snapshot_attempts + 1 >= 3
                           THEN 'unavailable' ELSE snapshot_status END,
    updated_at = @now
WHERE id = @id
RETURNING snapshot_attempts;

-- name: ListGhostBusReportsForExport :many
SELECT * FROM ghost_bus_reports
WHERE region_id = @region_id AND created_at >= @since
ORDER BY id;
```

- [ ] **Step 3: Generate and verify**

Run: `make generate && go build ./...`
Expected: `gen/ghostbus.sql.go` appears; the tree still builds. Inspect the generated params struct once: nullable columns must come out as `sql.Null*` types (they will, given the schema above).

- [ ] **Step 4: Commit**

```bash
git add internal/store/sqlite/migrations/00007_ghost_bus_reports.sql internal/store/sqlite/queries/ghostbus.sql internal/store/sqlite/gen
git commit -m "feat: ghost bus reports schema and queries (spec section 8)"
```

---

### Task 3: SQLite adapter and storetest conformance suite

**Files:**
- Create: `internal/store/sqlite/ghostbus.go`
- Create: `internal/store/storetest/ghostbustest.go`
- Modify: `internal/store/sqlite/store.go` (add the `GhostBus()` accessor next to `Surveys()`)
- Test: `internal/store/sqlite/ghostbus_test.go`

**Interfaces:**
- Consumes: Task 1's `ghostbus.Repository`, `ghostbus.NewReport`, `ghostbus.ErrDuplicate`, `ghostbus.ErrTokenCollision`, `ghostbus.MaxSnapshotAttempts`; Task 2's `gen.Queries` methods; the storetest package's fixed `base` instant and its region-fixture helper (see `pushregtest.go` for the existing pattern of creating a region row before exercising a region-scoped repository).
- Produces: `(*sqlite.Store).GhostBus() ghostbus.Repository`; `storetest.RunGhostBusRepository(t *testing.T, newStore func(*testing.T) (ghostbus.Repository, regions.Repository))`.

- [ ] **Step 1: Write the conformance suite (failing)**

`internal/store/storetest/ghostbustest.go`. Subtest registry:

```go
// RunGhostBusRepository exercises a ghostbus.Repository against the
// behavioral contract every engine must satisfy.
func RunGhostBusRepository(t *testing.T, newStore func(*testing.T) (ghostbus.Repository, regions.Repository)) {
	t.Helper()
	t.Run("CreateRoundTrip", func(t *testing.T) { testGhostBusCreateRoundTrip(t, newStore) })
	t.Run("DuplicateReturnsErrDuplicate", func(t *testing.T) { testGhostBusDuplicate(t, newStore) })
	t.Run("ConcurrentDuplicateOneWins", func(t *testing.T) { testGhostBusConcurrentDuplicate(t, newStore) })
	t.Run("TokenCollisionIsNotDuplicate", func(t *testing.T) { testGhostBusTokenCollision(t, newStore) })
	t.Run("DedupeScope", func(t *testing.T) { testGhostBusDedupeScope(t, newStore) })
	t.Run("PendingSnapshotPoll", func(t *testing.T) { testGhostBusPendingPoll(t, newStore) })
	t.Run("FailureCapMarksUnavailable", func(t *testing.T) { testGhostBusFailureCap(t, newStore) })
	t.Run("CaptureRoundTrip", func(t *testing.T) { testGhostBusCaptureRoundTrip(t, newStore) })
	t.Run("ExportSinceFilter", func(t *testing.T) { testGhostBusExportSince(t, newStore) })
}
```

Core test bodies (the fixture helper mirrors the existing suites — create a region, then reports against it):

```go
func ghostBusFixture(regionID int64, publicID, user, trip string, serviceDate int64) ghostbus.NewReport {
	return ghostbus.NewReport{
		RegionID: regionID, PublicID: publicID, UserIdentifier: user,
		TripIdentifier: trip, ServiceDate: serviceDate, WaitDurationMinutes: 15,
	}
}

func testGhostBusCreateRoundTrip(t *testing.T, newStore func(*testing.T) (ghostbus.Repository, regions.Repository)) {
	repo, regionsRepo := newStore(t)
	ctx := context.Background()
	regionID := createGhostBusRegion(t, regionsRepo) // same pattern as the pushreg suite's fixture

	pred := true
	seq := int64(3)
	lat, lon := 47.6097, -122.3422
	sched := int64(1756000000000)
	in := ghostBusFixture(regionID, "tok_roundtrip_0000000001", "user-a", "1_604370", 1754809200000)
	in.RouteIdentifier, in.StopIdentifier, in.VehicleIdentifier = "1_44", "1_570", "1_4361"
	in.StopSequence, in.Predicted, in.Comment = &seq, &pred, "never showed"
	in.UserLatitude, in.UserLongitude = &lat, &lon
	in.ScheduledArrivalAt = &sched

	got, err := repo.Create(ctx, in, base)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got.ID == 0 || got.PublicID != in.PublicID || got.SnapshotStatus != ghostbus.SnapshotPending {
		t.Errorf("Create returned %+v; want assigned ID, same PublicID, pending snapshot", got)
	}
	if got.Predicted == nil || !*got.Predicted || got.StopSequence == nil || *got.StopSequence != 3 {
		t.Errorf("pointer fields did not round-trip: %+v", got)
	}
	if got.ServiceDate != 1754809200000 || got.ScheduledArrivalAt == nil || *got.ScheduledArrivalAt != sched {
		t.Errorf("epoch-ms fields did not round-trip: %+v", got)
	}
	if !got.CreatedAt.Equal(base) {
		t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, base)
	}
}

func testGhostBusDuplicate(t *testing.T, newStore func(*testing.T) (ghostbus.Repository, regions.Repository)) {
	repo, regionsRepo := newStore(t)
	ctx := context.Background()
	regionID := createGhostBusRegion(t, regionsRepo)
	first := ghostBusFixture(regionID, "tok_dup_a_00000000001", "user-a", "trip-1", 1000)
	if _, err := repo.Create(ctx, first, base); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	second := ghostBusFixture(regionID, "tok_dup_b_00000000002", "user-a", "trip-1", 1000)
	_, err := repo.Create(ctx, second, base)
	if !errors.Is(err, ghostbus.ErrDuplicate) {
		t.Fatalf("duplicate Create err = %v, want ErrDuplicate", err)
	}
}

func testGhostBusConcurrentDuplicate(t *testing.T, newStore func(*testing.T) (ghostbus.Repository, regions.Repository)) {
	repo, regionsRepo := newStore(t)
	ctx := context.Background()
	regionID := createGhostBusRegion(t, regionsRepo)
	errs := make(chan error, 2)
	for i := range 2 {
		in := ghostBusFixture(regionID, fmt.Sprintf("tok_race_%016d", i), "user-a", "trip-1", 1000)
		go func() {
			_, err := repo.Create(ctx, in, base)
			errs <- err
		}()
	}
	var okCount, dupCount int
	for range 2 {
		switch err := <-errs; {
		case err == nil:
			okCount++
		case errors.Is(err, ghostbus.ErrDuplicate):
			dupCount++
		default:
			t.Fatalf("racing Create err = %v, want nil or ErrDuplicate", err)
		}
	}
	if okCount != 1 || dupCount != 1 {
		t.Fatalf("race outcome ok=%d dup=%d, want exactly one winner and one ErrDuplicate", okCount, dupCount)
	}
}

func testGhostBusTokenCollision(t *testing.T, newStore func(*testing.T) (ghostbus.Repository, regions.Repository)) {
	repo, regionsRepo := newStore(t)
	ctx := context.Background()
	regionID := createGhostBusRegion(t, regionsRepo)
	first := ghostBusFixture(regionID, "tok_same_000000000001", "user-a", "trip-1", 1000)
	if _, err := repo.Create(ctx, first, base); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	// Different dedupe key, same public identifier: this must NOT read as
	// already_reported -- a rider's first-ever report would be rejected.
	second := ghostBusFixture(regionID, "tok_same_000000000001", "user-b", "trip-2", 2000)
	_, err := repo.Create(ctx, second, base)
	if !errors.Is(err, ghostbus.ErrTokenCollision) {
		t.Fatalf("collision Create err = %v, want ErrTokenCollision", err)
	}
}

func testGhostBusDedupeScope(t *testing.T, newStore func(*testing.T) (ghostbus.Repository, regions.Repository)) {
	repo, regionsRepo := newStore(t)
	ctx := context.Background()
	regionID := createGhostBusRegion(t, regionsRepo)
	seed := ghostBusFixture(regionID, "tok_scope_00000000001", "user-a", "trip-1", 1000)
	if _, err := repo.Create(ctx, seed, base); err != nil {
		t.Fatalf("seed Create: %v", err)
	}
	// Each varies exactly one dedupe component; all must succeed.
	variants := []ghostbus.NewReport{
		ghostBusFixture(regionID, "tok_scope_00000000002", "user-b", "trip-1", 1000),
		ghostBusFixture(regionID, "tok_scope_00000000003", "user-a", "trip-2", 1000),
		ghostBusFixture(regionID, "tok_scope_00000000004", "user-a", "trip-1", 1001),
	}
	for i, in := range variants {
		if _, err := repo.Create(ctx, in, base); err != nil {
			t.Errorf("variant %d Create err = %v, want nil", i, err)
		}
	}
}

func testGhostBusPendingPoll(t *testing.T, newStore func(*testing.T) (ghostbus.Repository, regions.Repository)) {
	repo, regionsRepo := newStore(t)
	ctx := context.Background()
	regionID := createGhostBusRegion(t, regionsRepo)
	var ids []int64
	for i := range 3 {
		in := ghostBusFixture(regionID, fmt.Sprintf("tok_poll_%013d", i), "user-a", fmt.Sprintf("trip-%d", i), 1000)
		rep, err := repo.Create(ctx, in, base)
		if err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
		ids = append(ids, rep.ID)
	}
	if err := repo.MarkSnapshotCaptured(ctx, ids[0], `{"status":{}}`, base); err != nil {
		t.Fatalf("MarkSnapshotCaptured: %v", err)
	}
	// Exhaust the second report's attempts: at the cap it must stop
	// matching the poll even though the crash-window row would still say
	// pending -- the final increment flips the status itself.
	for range ghostbus.MaxSnapshotAttempts {
		if _, err := repo.RecordSnapshotFailure(ctx, ids[1], base); err != nil {
			t.Fatalf("RecordSnapshotFailure: %v", err)
		}
	}
	pending, err := repo.ListPendingSnapshots(ctx, 10)
	if err != nil {
		t.Fatalf("ListPendingSnapshots: %v", err)
	}
	if len(pending) != 1 || pending[0].ID != ids[2] {
		t.Fatalf("pending = %+v, want exactly the untouched report %d", pending, ids[2])
	}
}

func testGhostBusFailureCap(t *testing.T, newStore func(*testing.T) (ghostbus.Repository, regions.Repository)) {
	repo, regionsRepo := newStore(t)
	ctx := context.Background()
	regionID := createGhostBusRegion(t, regionsRepo)
	rep, err := repo.Create(ctx, ghostBusFixture(regionID, "tok_cap_000000000001", "user-a", "trip-1", 1000), base)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for want := int64(1); want < ghostbus.MaxSnapshotAttempts; want++ {
		n, err := repo.RecordSnapshotFailure(ctx, rep.ID, base)
		if err != nil || n != want {
			t.Fatalf("failure %d: n=%d err=%v", want, n, err)
		}
		if p, _ := repo.ListPendingSnapshots(ctx, 10); len(p) != 1 {
			t.Fatalf("after failure %d report should still be pollable", want)
		}
	}
	n, err := repo.RecordSnapshotFailure(ctx, rep.ID, base)
	if err != nil || n != ghostbus.MaxSnapshotAttempts {
		t.Fatalf("final failure: n=%d err=%v", n, err)
	}
	if p, _ := repo.ListPendingSnapshots(ctx, 10); len(p) != 0 {
		t.Fatalf("report at the cap must not be pollable; got %+v", p)
	}
	exported, err := repo.ListForExport(ctx, regionID, 0)
	if err != nil || len(exported) != 1 {
		t.Fatalf("export: %v / %d rows", err, len(exported))
	}
	if exported[0].SnapshotStatus != ghostbus.SnapshotUnavailable {
		t.Fatalf("status after cap = %q, want unavailable", exported[0].SnapshotStatus)
	}
}

func testGhostBusCaptureRoundTrip(t *testing.T, newStore func(*testing.T) (ghostbus.Repository, regions.Repository)) {
	repo, regionsRepo := newStore(t)
	ctx := context.Background()
	regionID := createGhostBusRegion(t, regionsRepo)
	rep, err := repo.Create(ctx, ghostBusFixture(regionID, "tok_cap2_00000000001", "user-a", "trip-1", 1000), base)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	snap := `{"current_time":1,"status":{"phase":"in_progress"}}`
	capturedAt := base.Add(45 * time.Second)
	if err := repo.MarkSnapshotCaptured(ctx, rep.ID, snap, capturedAt); err != nil {
		t.Fatalf("MarkSnapshotCaptured: %v", err)
	}
	got, err := repo.ListForExport(ctx, regionID, 0)
	if err != nil || len(got) != 1 {
		t.Fatalf("export: %v / %d rows", err, len(got))
	}
	r := got[0]
	if r.SnapshotStatus != ghostbus.SnapshotCaptured || r.SnapshotJSON != snap {
		t.Errorf("captured row = %+v", r)
	}
	if r.SnapshotCapturedAt == nil || !r.SnapshotCapturedAt.Equal(capturedAt) {
		t.Errorf("SnapshotCapturedAt = %v, want %v", r.SnapshotCapturedAt, capturedAt)
	}
}

func testGhostBusExportSince(t *testing.T, newStore func(*testing.T) (ghostbus.Repository, regions.Repository)) {
	repo, regionsRepo := newStore(t)
	ctx := context.Background()
	regionID := createGhostBusRegion(t, regionsRepo)
	early := ghostBusFixture(regionID, "tok_since_0000000001", "user-a", "trip-1", 1000)
	late := ghostBusFixture(regionID, "tok_since_0000000002", "user-a", "trip-2", 1000)
	if _, err := repo.Create(ctx, early, base); err != nil {
		t.Fatalf("early Create: %v", err)
	}
	if _, err := repo.Create(ctx, late, base.Add(2*time.Hour)); err != nil {
		t.Fatalf("late Create: %v", err)
	}
	got, err := repo.ListForExport(ctx, regionID, base.Add(time.Hour).Unix())
	if err != nil {
		t.Fatalf("ListForExport: %v", err)
	}
	if len(got) != 1 || got[0].PublicID != late.PublicID {
		t.Fatalf("since filter returned %+v, want only the late report", got)
	}
	all, err := repo.ListForExport(ctx, regionID, 0)
	if err != nil || len(all) != 2 {
		t.Fatalf("since=0 returned %d rows err=%v, want 2", len(all), err)
	}
}
```

`createGhostBusRegion` follows the shared region-fixture pattern the other suites use (an upserted region row via the regions repository; copy the helper shape from `pushregtest.go`, renamed so the suites don't collide).

`internal/store/sqlite/ghostbus_test.go` mirrors `surveys_test.go`:

```go
func TestGhostBusRepositoryConformance(t *testing.T) {
	storetest.RunGhostBusRepository(t, func(t *testing.T) (ghostbus.Repository, regions.Repository) {
		s := sqlitetest.OpenMigrated(t) // the existing test-store helper used by the other adapter tests
		return s.GhostBus(), s.Regions()
	})
}
```

(Use whatever helper `surveys_test.go` actually calls to open a fresh migrated store — copy that file's setup verbatim.)

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/store/...`
Expected: FAIL — `undefined: storetest.RunGhostBusRepository` builds first, then `(*Store).GhostBus` missing.

- [ ] **Step 3: Implement the adapter**

`internal/store/sqlite/ghostbus.go`:

```go
package sqlite

// ghostBusRepo adapts gen.Queries to ghostbus.Repository. Duplicate
// discrimination is by constraint message text because modernc reports
// every unique violation as the same SQLITE_CONSTRAINT_UNIQUE code; the
// message names the violated index's columns (same pattern as alarms.go's
// V1 dedupe).
type ghostBusRepo struct{ q *gen.Queries }

func (r *ghostBusRepo) Create(ctx context.Context, in ghostbus.NewReport, now time.Time) (ghostbus.Report, error) {
	row, err := r.q.CreateGhostBusReport(ctx, gen.CreateGhostBusReportParams{
		RegionID:                 in.RegionID,
		PublicIdentifier:         in.PublicID,
		UserIdentifier:           in.UserIdentifier,
		TripIdentifier:           in.TripIdentifier,
		ServiceDate:              in.ServiceDate,
		RouteIdentifier:          in.RouteIdentifier,
		StopIdentifier:           in.StopIdentifier,
		VehicleIdentifier:        in.VehicleIdentifier,
		StopSequence:             nullFromPtr(in.StopSequence),
		Predicted:                nullBoolFromPtr(in.Predicted),
		ScheduleDeviationMinutes: nullFromPtr(in.ScheduleDeviationMinutes),
		WaitDurationMinutes:      in.WaitDurationMinutes,
		Comment:                  in.Comment,
		UserLatitude:             nullFloatFromPtr(in.UserLatitude),
		UserLongitude:            nullFloatFromPtr(in.UserLongitude),
		ScheduledArrivalAt:       nullFromPtr(in.ScheduledArrivalAt),
		PredictedArrivalAt:       nullFromPtr(in.PredictedArrivalAt),
		PredictionLastUpdatedAt:  nullFromPtr(in.PredictionLastUpdatedAt),
		Now:                      now.Unix(),
	})
	if err != nil {
		msg := err.Error()
		switch {
		case strings.Contains(msg, "ghost_bus_reports.region_id"):
			return ghostbus.Report{}, ghostbus.ErrDuplicate
		case strings.Contains(msg, "ghost_bus_reports.public_identifier"):
			return ghostbus.Report{}, ghostbus.ErrTokenCollision
		}
		return ghostbus.Report{}, err
	}
	return ghostBusFromRow(row), nil
}
```

plus the straightforward `ListPendingSnapshots` (`limit` → `MaxRows`), `MarkSnapshotCaptured`, `MarkSnapshotUnavailable`, `RecordSnapshotFailure`, `ListForExport`, and a `ghostBusFromRow(gen.GhostBusReport) ghostbus.Report` converter that maps `sql.Null*` back to pointers and `created_at`/`snapshot_captured_at` epoch seconds to `time.Time` via `time.Unix(v, 0).UTC()`. Write (or reuse, if surveys/alarms already have them) tiny `nullFromPtr`/`nullBoolFromPtr`/`nullFloatFromPtr` helpers — check `internal/store/sqlite/surveys.go` and `alarms.go` first and share rather than duplicate.

`internal/store/sqlite/store.go` addition, next to `Surveys()`:

```go
// GhostBus returns the ghost bus reports repository (spec §8).
func (s *Store) GhostBus() ghostbus.Repository {
	return &ghostBusRepo{q: s.queries}
}
```

(Match the field name the other accessors actually use for the shared `*gen.Queries`.)

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/store/...`
Expected: PASS. Mutation check: swap the two `strings.Contains` cases so a dedupe hit returns `ErrTokenCollision` — `DuplicateReturnsErrDuplicate` and `TokenCollisionIsNotDuplicate` must both fail; revert.

- [ ] **Step 5: Commit**

```bash
git add internal/store/sqlite internal/store/storetest
git commit -m "feat: ghost bus sqlite adapter with dedupe-vs-collision discrimination, conformance suite"
```

---

### Task 4: obaapi TripDetails with raw-JSON pruning

**Files:**
- Modify: `internal/obaapi/obaapi.go` (extend the `Client` interface and `client` implementation)
- Test: `internal/obaapi/obaapi_test.go` (follow the file's existing httptest fake-server pattern)

**Interfaces:**
- Consumes: existing `sdkFor`, `redact`, `statusOf`, `ErrNotFound`, `ErrNotConfigured` in `internal/obaapi/obaapi.go`.
- Produces:

```go
// TripDetailsQuery identifies the trip instance a ghost bus report named,
// plus the report's own route/stop ids used to resolve display names.
type TripDetailsQuery struct {
	TripID      string
	ServiceDate int64  // epoch ms
	RouteID     string // report's route_identifier; "" = fall back to the trip reference's routeId
	StopID      string // report's stop_identifier; "" = no stop display block
}

// TripDetails fetches trip-details for one trip instance and returns the
// pruned spec-§2.6 snapshot document. ErrNotFound on a definitive miss
// (404 or empty entry); ErrNotConfigured when the region has no API key.
TripDetails(ctx context.Context, region regions.Region, q TripDetailsQuery) (json.RawMessage, error)
```

- [ ] **Step 1: Write the failing tests**

Add to `internal/obaapi/obaapi_test.go`, using the file's existing fake-server setup. Table of cases:

```go
func TestTripDetailsPrunesRawJSON(t *testing.T) {
	// Serve a realistic trip-details document. Crucially the status block
	// OMITS lastKnownLocation and includes an extra key (nextStop) that the
	// allow-list must drop; the references resolve the trip, its route, and
	// the queried stop.
	body := `{
	  "code": 200, "currentTime": 1756000123456,
	  "data": {
	    "entry": {
	      "tripId": "1_604370",
	      "status": {
	        "predicted": true, "vehicleId": "1_4361", "phase": "in_progress",
	        "scheduleDeviation": 60, "nextStop": "1_999",
	        "position": {"lat": 47.61, "lon": -122.34}
	      }
	    },
	    "references": {
	      "trips":  [{"id": "1_604370", "routeId": "1_100044", "tripHeadsign": "Ballard"}],
	      "routes": [{"id": "1_100044", "shortName": "44", "longName": "Ballard - Montlake", "type": 3}],
	      "stops":  [{"id": "1_570", "name": "15th Ave NW & NW Market St", "lat": 47.668, "lon": -122.376}]
	    }
	  }
	}`
	client, region := newTripDetailsFake(t, http.StatusOK, body) // helper mirroring the file's existing fake pattern

	raw, err := client.TripDetails(context.Background(), region, TripDetailsQuery{
		TripID: "1_604370", ServiceDate: 1754809200000, StopID: "1_570",
	})
	if err != nil {
		t.Fatalf("TripDetails: %v", err)
	}
	var snap map[string]any
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatalf("snapshot is not JSON: %v", err)
	}
	status, _ := snap["status"].(map[string]any)
	if status["phase"] != "in_progress" || status["vehicleId"] != "1_4361" {
		t.Errorf("status block = %v", status)
	}
	// Absent means absent: keys the upstream omitted must not appear as
	// zero values (a defaulted lastKnownLocation puts the bus at Null
	// Island and poisons the CSV distance column).
	if _, ok := status["lastKnownLocation"]; ok {
		t.Error("lastKnownLocation fabricated from a zero value")
	}
	if _, ok := status["nextStop"]; ok {
		t.Error("nextStop survived the STATUS_KEYS allow-list")
	}
	display, _ := snap["display"].(map[string]any)
	if display["route_short_name"] != "44" || display["headsign"] != "Ballard" ||
		display["stop_name"] != "15th Ave NW & NW Market St" {
		t.Errorf("display block = %v", display)
	}
	if display["stop_lat"] != 47.668 {
		t.Errorf("stop_lat = %v", display["stop_lat"])
	}
	if ct, ok := snap["current_time"].(float64); !ok || int64(ct) != 1756000123456 {
		t.Errorf("current_time = %v", snap["current_time"])
	}
}

func TestTripDetailsEmptyEntryIsNotFound(t *testing.T) {
	// Upstream 200 with a null/empty entry is a definitive miss, same
	// contract as ArrivalAndDeparture's 200-with-null handling (0bb74c2).
	client, region := newTripDetailsFake(t, http.StatusOK, `{"code":200,"data":{"entry":null,"references":{}}}`)
	_, err := client.TripDetails(context.Background(), region, TripDetailsQuery{TripID: "1_x", ServiceDate: 1})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestTripDetails404IsNotFound(t *testing.T) {
	client, region := newTripDetailsFake(t, http.StatusNotFound, `{}`)
	_, err := client.TripDetails(context.Background(), region, TripDetailsQuery{TripID: "1_x", ServiceDate: 1})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestTripDetails500IsTransient(t *testing.T) {
	client, region := newTripDetailsFake(t, http.StatusInternalServerError, `{}`)
	_, err := client.TripDetails(context.Background(), region, TripDetailsQuery{TripID: "1_x", ServiceDate: 1})
	if err == nil || errors.Is(err, ErrNotFound) || errors.Is(err, ErrNotConfigured) {
		t.Fatalf("err = %v, want a transient (unclassified) error", err)
	}
}

func TestTripDetailsNoKeyIsNotConfigured(t *testing.T) {
	// Region with no key and no process default: same contract as the
	// existing calls. Reuse the file's existing no-key client fixture.
	client := New("", http.DefaultClient, slog.New(slog.DiscardHandler))
	_, err := client.TripDetails(context.Background(), regions.Region{ID: 1, OBABaseURL: "https://example.com"}, TripDetailsQuery{TripID: "1_x", ServiceDate: 1})
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("err = %v, want ErrNotConfigured", err)
	}
}
```

Match `newTripDetailsFake` to however the existing tests fabricate a server+region pair (there is one; reuse it or add a thin wrapper). Adjust the no-key test to the file's existing equivalent if one exists.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/obaapi/`
Expected: FAIL — `TripDetailsQuery`/`TripDetails` undefined.

- [ ] **Step 3: Implement**

In `internal/obaapi/obaapi.go`, add to the `Client` interface, then:

```go
// tripStatusKeys is OBACloud's STATUS_KEYS allow-list, verbatim: the
// forensic subset of a trip-details status block worth storing per report.
var tripStatusKeys = []string{
	"predicted", "vehicleId", "lastUpdateTime", "lastLocationUpdateTime",
	"scheduleDeviation", "phase", "serviceDate", "closestStop",
	"closestStopTimeOffset", "distanceAlongTrip", "totalDistanceAlongTrip",
	"lastKnownLocation", "position", "orientation", "activeTripId",
}

func (c *client) TripDetails(ctx context.Context, region regions.Region, q TripDetailsQuery) (json.RawMessage, error) {
	sdk, err := c.sdkFor(region)
	if err != nil {
		return nil, err // ErrNotConfigured
	}
	// includeSchedule stays at the server default (true) deliberately: the
	// schedule block is what pulls the queried stop into references.stops;
	// disabling it silently blanks the CSV's stop columns (design §2.7).
	resp, err := sdk.TripDetails.Get(ctx, q.TripID, oba.TripDetailGetParams{
		ServiceDate: oba.F(q.ServiceDate),
	})
	if err != nil {
		if isClientError(err) {
			return nil, ErrNotFound
		}
		return nil, redact(err)
	}
	return pruneTripSnapshot([]byte(resp.JSON.RawJSON()), q)
}

// pruneTripSnapshot builds the spec-§2.6 snapshot from the RAW response
// JSON. Raw, not the SDK's typed structs, deliberately: every typed status
// field is value-typed, so absence and zero are indistinguishable there --
// and a fabricated zero lastKnownLocation puts the vehicle at Null Island,
// which the CSV would render as kilometers of plausible garbage.
func pruneTripSnapshot(raw []byte, q TripDetailsQuery) (json.RawMessage, error) {
	var doc struct {
		CurrentTime int64 `json:"currentTime"`
		Data        struct {
			Entry      map[string]any `json:"entry"`
			References struct {
				Trips  []map[string]any `json:"trips"`
				Routes []map[string]any `json:"routes"`
				Stops  []map[string]any `json:"stops"`
			} `json:"references"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, ErrNotFound // unparseable body: definitive, not transient
	}
	if len(doc.Data.Entry) == 0 {
		return nil, ErrNotFound // 200-with-null-entry, same as ArrivalAndDeparture
	}
	status := map[string]any{}
	if entryStatus, ok := doc.Data.Entry["status"].(map[string]any); ok {
		for _, k := range tripStatusKeys {
			if v, present := entryStatus[k]; present {
				status[k] = v
			}
		}
	}
	snap := map[string]any{"current_time": doc.CurrentTime, "status": status}
	if display := tripDisplayBlock(doc.Data.References.Trips, doc.Data.References.Routes, doc.Data.References.Stops, q); len(display) > 0 {
		snap["display"] = display
	}
	return json.Marshal(snap)
}

// tripDisplayBlock resolves human-readable names out of the references:
// best-effort, absent keys simply missing (OBACloud's .compact). The DB
// stores only raw identifiers; without this the CSV could never show route
// names, headsigns, or stop coordinates.
func tripDisplayBlock(trips, routes, stops []map[string]any, q TripDetailsQuery) map[string]any {
	find := func(list []map[string]any, id string) map[string]any {
		if id == "" {
			return nil
		}
		for _, m := range list {
			if m["id"] == id {
				return m
			}
		}
		return nil
	}
	display := map[string]any{}
	trip := find(trips, q.TripID)
	routeID := q.RouteID
	if routeID == "" && trip != nil {
		routeID, _ = trip["routeId"].(string)
	}
	put := func(key string, src map[string]any, srcKey string) {
		if src == nil {
			return
		}
		if v, ok := src[srcKey]; ok && v != nil && v != "" {
			display[key] = v
		}
	}
	put("headsign", trip, "tripHeadsign")
	route := find(routes, routeID)
	put("route_short_name", route, "shortName")
	put("route_long_name", route, "longName")
	put("route_type", route, "type")
	stop := find(stops, q.StopID)
	put("stop_name", stop, "name")
	put("stop_lat", stop, "lat")
	put("stop_lon", stop, "lon")
	return display
}
```

Check `isClientError`'s actual behavior in this file first — if it treats all 4xx as client errors, keep it; the goal is 404 → `ErrNotFound`, 5xx/network → transient. Mirror exactly how `ArrivalAndDeparture` classifies, including its 200-with-null handling.

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/obaapi/`
Expected: PASS. Mutation check: make `pruneTripSnapshot` copy the whole status block instead of the allow-list — the `nextStop` assertion must fail; revert.

- [ ] **Step 5: Commit**

```bash
git add internal/obaapi
git commit -m "feat: obaapi TripDetails with raw-JSON snapshot pruning (spec section 8 enrichment)"
```

---

### Task 5: HTTP handler, Deps, and router wiring

**Files:**
- Create: `internal/httpapi/ghostbus.go`
- Modify: `internal/httpapi/router.go` (Deps fields + route registration + limiter defaults)
- Test: `internal/httpapi/ghostbus_test.go`

**Interfaces:**
- Consumes: Task 1's domain package; Task 3's repository (tests use a fake, not sqlite); existing helpers `resolveRegion`, `parseRequestParams`, `errorWithMessages`, `writeJSON`, `writeServerError`, `throttleByIP`, `coordinate`, `params.str/int64/boolish`, `errBodyTooLarge`, `maxIdentifierLen`, `securetoken.New`, `ratelimit.New`.
- Produces: Deps fields used by Task 7:

```go
// GhostBus backs the ghost bus report endpoint (spec §8). Nil means the
// route is not registered.
GhostBus ghostbus.Repository
// GhostBusIPLimiter is the §2.6 10/hour-per-IP throttle; NewRouter
// defaults it, tests inject tighter ones.
GhostBusIPLimiter *ratelimit.Limiter
// GhostBusUserLimiter is the §2.6 20/day-per-user_identifier throttle.
GhostBusUserLimiter *ratelimit.Limiter
```

- [ ] **Step 1: Write the failing contract tests**

`internal/httpapi/ghostbus_test.go`. Fake repository + table tests. The essential cases (follow the file layout of `pushregs_test.go` for router construction with test Deps):

```go
type fakeGhostBusRepo struct {
	mu       sync.Mutex
	created  []ghostbus.NewReport
	createErrs []error // popped per call; nil = success
}

func (f *fakeGhostBusRepo) Create(_ context.Context, in ghostbus.NewReport, _ time.Time) (ghostbus.Report, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
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
// ListPendingSnapshots / MarkSnapshotCaptured / MarkSnapshotUnavailable /
// RecordSnapshotFailure / ListForExport: panic("not used by handler tests").
```

Cases (each a subtest hitting `POST /api/v2/regions/1/ghost_bus_reports` on a router built with a seeded region 1, generous default limiters unless the case says otherwise):

1. **iOS-shaped form body succeeds** — pinned client contract. Body exactly as `NetworkHelpers` encodes: `user_identifier=…&trip_identifier=1_604370&service_date=1754809200000&wait_duration_minutes=15&predicted=1&stop_identifier=1_570&route_identifier=1_44&vehicle_identifier=1_4361&stop_sequence=3&scheduled_arrival_at=1754809100000&schedule_deviation_minutes=2&comment=never+showed&user_latitude=47.6&user_longitude=-122.3`, `Content-Type: application/x-www-form-urlencoded`. Expect 201, response body `{"id":"<22-char token>"}`, and the stored `NewReport` carries `Predicted != nil && *Predicted`, `ServiceDate == 1754809200000`, `*StopSequence == 3`.
2. **JSON body succeeds** — same fields as a JSON object with numeric types (`"predicted": true` as a bare bool, `"service_date": 1754809200000` as a number). Expect 201 and identical stored fields (the bare-bool path goes through `params.str`'s bool stringification).
3. **Missing requireds** — empty body → 422, `error == "Unable to save report"`, messages contain `"User identifier can't be blank"`, `"Trip identifier can't be blank"`, `"Service date can't be blank"`, `"Wait duration minutes is not included in the list"`.
4. **Non-integer service_date coerces to null then fails presence** — `service_date=2026-08-23T10:00:00Z` → 422 with `"Service date can't be blank"` (never fuzzily parsed; it is a dedupe key component).
5. **wait_duration_minutes=25 → 422** with `"Wait duration minutes is not included in the list"`.
6. **Comment over 1,000 runes → 422**; exactly 1,000 runes (use a multibyte rune to prove rune counting) → 201.
7. **Coordinate out of range** (`user_latitude=91`) and unparseable (`user_latitude=abc`) → 422 with `"User latitude must be between -90 and 90"`. Valid negative longitude passes.
8. **Duplicate maps to already_reported** — repo returns `ghostbus.ErrDuplicate` → 422 body exactly `{"error":"already_reported","messages":["User has already reported this trip"]}` (byte-compare after JSON normalization).
9. **Token collision re-mints once** — `createErrs = []error{ghostbus.ErrTokenCollision, nil}` → 201, two Create calls with **different** PublicIDs; `createErrs = [collision, collision]` → 500.
10. **Unknown region → 404** `{"error":"Couldn't find Region"}`; slug form `/regions/1-puget-sound/...` works.
11. **Oversized declared JSON → bodyless 403** — `Content-Type: application/json`, `Content-Length: 9000` (a 9,000-byte JSON body). Expect 403, empty body, repo untouched.
12. **Oversized streamed JSON → 403** — JSON body over 8,192 bytes sent without relying on the declared length being honest (httptest sets Content-Length from the buffer, so build the body > 8 KB; asserting on the parse-side `errBodyTooLarge` path needs `r.ContentLength` faked low — do this by wrapping the body in a reader and setting `req.ContentLength = 100` manually).
13. **Large form body is NOT capped at 8 KB** — a form body of ~20 KB (comment of ~1,000 four-byte runes plus padding fields under the 1,000-rune cap — build it as many distinct ignored params rather than an over-cap comment) → 201. This is the iOS emoji-comment regression guard; it must fail if someone "simplifies" the JSON-only cap away.
14. **Per-user throttle counts across encodings** — Deps with `GhostBusUserLimiter: ratelimit.New(1, 24*time.Hour)`: first request form-encoded (201), second request **JSON** with the same `user_identifier` but a different trip → 429, repo has exactly one row. This is the §2.6 JSON-bypass test.
15. **Over-length user_identifier never becomes a limiter key** — Deps with `GhostBusUserLimiter: ratelimit.New(1, 24*time.Hour)`; POST with a 300-char identifier → 422 (`"User identifier is too long (maximum is 255 characters)"`), then a valid request with a *different, valid* identifier → 201 (the limiter was not consumed; assert via a same-identifier repeat if Len() isn't exported enough — simplest: the 422 must short-circuit before Allow, so a subsequent valid post with THAT same long identifier truncated scenario is unnecessary; assert `deps.GhostBusUserLimiter.Len() == 0` after the 422, which the ratelimit package exports for tests).
16. **IP throttle** — Deps with `GhostBusIPLimiter: ratelimit.New(1, time.Hour)`: second request from the same peer → 429 regardless of identifier.
17. **Store failure → 500** — repo returns a generic error → 500, and the handler logged the operation (no assertion on log content beyond it not containing the user identifier — grep the test logger buffer for the identifier string and assert absent).

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/httpapi/ -run TestGhostBus`
Expected: FAIL — handler and Deps fields undefined.

- [ ] **Step 3: Implement the handler**

`internal/httpapi/ghostbus.go`:

```go
package httpapi

// ghostBusJSONBodyLimit is the §2.6 cap on JSON report bodies. JSON only:
// iOS sends form bodies whose percent-encoding can push a legal
// emoji-heavy comment well past 8 KB, and the padding attack this cap
// exists for (overflow a bounded throttle body-read so the parse fails
// uncounted) has no form-body analog here -- the params bag is parsed
// once, so there is no separate throttle peek to defeat.
const ghostBusJSONBodyLimit = 8192

type ghostBusHandler struct{ deps Deps }

func (h *ghostBusHandler) create(w http.ResponseWriter, r *http.Request) {
	region, ok := resolveRegion(w, r, h.deps)
	if !ok {
		return
	}
	isJSON := hasJSONContentType(r)
	if isJSON && r.ContentLength > ghostBusJSONBodyLimit {
		// A JSON body declaring more than the cap can only be padding; a
		// legitimate report is far smaller (spec §2.6). Bodyless 403 --
		// distinct in logs from the cross-site guard's 403, which carries a
		// JSON body.
		w.WriteHeader(http.StatusForbidden)
		return
	}
	limit := int64(requestBodyLimit)
	if isJSON {
		limit = ghostBusJSONBodyLimit
	}
	p, err := parseRequestParams(w, r, limit)
	if err != nil {
		if isJSON && errors.Is(err, errBodyTooLarge) {
			// Chunked or lying Content-Length: same padding, same 403.
			w.WriteHeader(http.StatusForbidden)
			return
		}
		errorWithMessages(w, h.deps.Logger, "Unable to save report", []string{err.Error()})
		return
	}

	uid, _ := p.str("user_identifier")
	if len(uid) > maxIdentifierLen {
		// Rejected before the throttle so attacker-sized strings never
		// become limiter map keys (design §2.4).
		errorWithMessages(w, h.deps.Logger, "Unable to save report",
			[]string{fmt.Sprintf("User identifier is too long (maximum is %d characters)", maxIdentifierLen)})
		return
	}
	// Blank identifiers skip the counter (no pooled nil bucket) and fail
	// presence validation below instead. The key is the identifier alone,
	// not (region, identifier): rack-attack's discriminator is the bare
	// value, so a device rotating regions shares one daily budget there
	// too.
	if uid != "" && !h.deps.GhostBusUserLimiter.Allow(uid, h.deps.Now()) {
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}

	in, msgs := ghostBusReportFromParams(region.ID, p)
	if len(msgs) > 0 {
		errorWithMessages(w, h.deps.Logger, "Unable to save report", msgs)
		return
	}

	for attempt := 0; ; attempt++ {
		if in.PublicID, err = securetoken.New(); err != nil {
			writeServerError(w, h.deps.Logger, region.ID, "ghostbus: mint token", err)
			return
		}
		_, err = h.deps.GhostBus.Create(r.Context(), in, h.deps.Now())
		switch {
		case err == nil:
			writeJSON(w, h.deps.Logger, http.StatusCreated, map[string]any{"id": in.PublicID})
			return
		case errors.Is(err, ghostbus.ErrDuplicate):
			// Validation-caught and race-caught duplicates are one path
			// here; clients treat this as a benign "got it already"
			// (spec §8 -- a 500 on the race would make the app retry
			// forever).
			errorWithMessages(w, h.deps.Logger, "already_reported",
				[]string{"User has already reported this trip"})
			return
		case errors.Is(err, ghostbus.ErrTokenCollision) && attempt == 0:
			continue // astronomically unlikely; re-mint once
		default:
			writeServerError(w, h.deps.Logger, region.ID, "ghostbus: create report", err)
			return
		}
	}
}
```

`ghostBusReportFromParams` (same file):

```go
// ghostBusReportFromParams validates and assembles the §8 create fields.
// Message strings mirror Rails full_messages for reference fidelity; no
// shipped client displays them (iOS keys on the bare 422), so exact copy
// is a courtesy, not a contract.
func ghostBusReportFromParams(regionID int64, p params) (ghostbus.NewReport, []string) {
	in := ghostbus.NewReport{RegionID: regionID}
	var msgs []string

	in.UserIdentifier, _ = p.str("user_identifier")
	if in.UserIdentifier == "" {
		msgs = append(msgs, "User identifier can't be blank")
	}
	in.TripIdentifier, _ = p.str("trip_identifier")
	switch {
	case in.TripIdentifier == "":
		msgs = append(msgs, "Trip identifier can't be blank")
	case len(in.TripIdentifier) > maxIdentifierLen:
		msgs = append(msgs, fmt.Sprintf("Trip identifier is too long (maximum is %d characters)", maxIdentifierLen))
	}
	for _, f := range []struct {
		key  string
		dst  *string
		name string
	}{
		{"route_identifier", &in.RouteIdentifier, "Route identifier"},
		{"stop_identifier", &in.StopIdentifier, "Stop identifier"},
		{"vehicle_identifier", &in.VehicleIdentifier, "Vehicle identifier"},
	} {
		*f.dst, _ = p.str(f.key)
		if len(*f.dst) > maxIdentifierLen {
			msgs = append(msgs, fmt.Sprintf("%s is too long (maximum is %d characters)", f.name, maxIdentifierLen))
		}
	}

	// Epoch-ms integers; a non-integer coerces to null (spec §8: never
	// fuzzily parsed -- service_date is a dedupe key component), and a null
	// service_date then fails presence.
	if v, ok := p.int64("service_date"); ok {
		in.ServiceDate = v
	} else {
		msgs = append(msgs, "Service date can't be blank")
	}
	in.ScheduledArrivalAt = optInt64(p, "scheduled_arrival_at")
	in.PredictedArrivalAt = optInt64(p, "predicted_arrival_at")
	in.PredictionLastUpdatedAt = optInt64(p, "prediction_last_updated_at")
	in.StopSequence = optInt64(p, "stop_sequence")
	in.ScheduleDeviationMinutes = optInt64(p, "schedule_deviation_minutes")

	if v, ok := p.int64("wait_duration_minutes"); ok && ghostbus.ValidWaitDuration(v) {
		in.WaitDurationMinutes = v
	} else {
		msgs = append(msgs, "Wait duration minutes is not included in the list")
	}

	if v, present := p.boolish("predicted"); present {
		in.Predicted = &v
	}

	in.Comment, _ = p.str("comment")
	if utf8.RuneCountInString(in.Comment) > ghostbus.CommentMaxLen {
		msgs = append(msgs, fmt.Sprintf("Comment is too long (maximum is %d characters)", ghostbus.CommentMaxLen))
	}

	var present, valid bool
	if in.UserLatitude, present, valid = coordinate(p, "user_latitude", 90); present && !valid {
		msgs = append(msgs, "User latitude must be between -90 and 90")
		in.UserLatitude = nil
	}
	if in.UserLongitude, present, valid = coordinate(p, "user_longitude", 180); present && !valid {
		msgs = append(msgs, "User longitude must be between -180 and 180")
		in.UserLongitude = nil
	}
	return in, msgs
}

func optInt64(p params, key string) *int64 {
	if v, ok := p.int64(key); ok {
		return &v
	}
	return nil
}

func hasJSONContentType(r *http.Request) bool {
	ct, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && ct == "application/json"
}
```

Router additions in `NewRouter` (guarded like every optional service) and default limiters where the other defaults live:

```go
if deps.GhostBus != nil {
	if deps.GhostBusIPLimiter == nil {
		deps.GhostBusIPLimiter = ratelimit.New(10, time.Hour) // spec §2.6
	}
	if deps.GhostBusUserLimiter == nil {
		deps.GhostBusUserLimiter = ratelimit.New(20, 24*time.Hour) // spec §2.6
	}
	gh := &ghostBusHandler{deps: deps}
	mux.HandleFunc("POST /api/v2/regions/{regionId}/ghost_bus_reports",
		throttleByIP(deps.GhostBusIPLimiter, deps, gh.create))
}
```

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/httpapi/ -run TestGhostBus`
Expected: PASS. Then `go test ./internal/httpapi/` (whole package still green). Mutation checks: (a) apply the 8 KB limit to form bodies too — case 13 must fail; (b) move the throttle above the length check — case 15 must fail; (c) return `already_reported` for `ErrTokenCollision` — case 9 must fail. Revert each.

- [ ] **Step 5: Commit**

```bash
git add internal/httpapi
git commit -m "feat: ghost bus report endpoint with dedupe 422s and section 2.6 throttles"
```

---

### Task 6: Snapshot scheduler

**Files:**
- Create: `internal/ghostbus/snapshot.go`
- Test: `internal/ghostbus/snapshot_test.go`

**Interfaces:**
- Consumes: Task 1's `Repository`; Task 4's `obaapi.TripDetailsQuery` and error sentinels; `regions.Repository`, `regions.ErrNotFound`.
- Produces (Task 7 wires this):

```go
// TripDetailsSource is the one obaapi method the scheduler needs,
// declared consumer-side so tests fake one function instead of the whole
// Client (same pattern as alarms.DepartureSource).
type TripDetailsSource interface {
	TripDetails(ctx context.Context, region regions.Region, q obaapi.TripDetailsQuery) (json.RawMessage, error)
}

type SnapshotScheduler struct {
	Repo    Repository
	Regions regions.Repository
	OBA     TripDetailsSource
	Now     func() time.Time
	Logger  *slog.Logger
}

func (s *SnapshotScheduler) CheckAll(ctx context.Context) // one polling cycle; exported for tests and wiring
func (s *SnapshotScheduler) RunLoop(ctx context.Context, interval time.Duration)

const SnapshotInterval = 30 * time.Second
const snapshotBatchSize = 100
```

- [ ] **Step 1: Write the failing tests**

`internal/ghostbus/snapshot_test.go`, with an in-package fake repository (a slice-backed `Repository` recording calls) and a fake `TripDetailsSource` returning a scripted `(json.RawMessage, error)` per trip id, plus a fake `regions.Repository` (only `Get` matters; other methods panic). Subtests:

```go
func TestSnapshotSchedulerCaptures(t *testing.T)
// One pending report; OBA returns {"current_time":1,"status":{"phase":"x"}}.
// After CheckAll: repo saw MarkSnapshotCaptured(id, thatJSON, now) and no
// failure calls.

func TestSnapshotSchedulerNotFoundIsUnavailable(t *testing.T)
// OBA returns obaapi.ErrNotFound → MarkSnapshotUnavailable, no
// RecordSnapshotFailure (a definitive miss never burns a retry).

func TestSnapshotSchedulerNoKeyIsUnavailable(t *testing.T)
// OBA returns obaapi.ErrNotConfigured → MarkSnapshotUnavailable.

func TestSnapshotSchedulerTransientCountsFailure(t *testing.T)
// OBA returns errors.New("boom") → RecordSnapshotFailure called once;
// neither Mark* called (the repository owns flipping to unavailable at the
// cap -- the scheduler never decides that).

func TestSnapshotSchedulerRegionGoneIsUnavailable(t *testing.T)
// regions.Get returns regions.ErrNotFound → MarkSnapshotUnavailable
// without calling OBA.

func TestSnapshotSchedulerRegionStoreErrorSkips(t *testing.T)
// regions.Get returns errors.New("db locked") → NOTHING recorded: a store
// hiccup is a fact about the database, not the report (same rule as the
// alarm scheduler); the report stays pending for the next cycle.

func TestSnapshotSchedulerRegionFetchedOncePerCycle(t *testing.T)
// Three pending reports in one region → the fake regions repo's Get was
// called exactly once (per-cycle cache, mirroring alarms).

func TestSnapshotSchedulerPassesReportIdentity(t *testing.T)
// The TripDetailsQuery carries the report's TripIdentifier, ServiceDate,
// RouteIdentifier, StopIdentifier verbatim.
```

Each fake `Report` comes from `ghostBusFixture`-style literals with distinct IDs; `Now` is a fixed instant.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/ghostbus/ -run TestSnapshot`
Expected: FAIL — `SnapshotScheduler` undefined.

- [ ] **Step 3: Implement**

`internal/ghostbus/snapshot.go`:

```go
package ghostbus

// The §8 enrichment worker: a DB-as-queue polling loop. The report row is
// the queue entry -- there is no durable job queue in this deployment, and
// a request-time goroutine would strand reports 'pending' forever after a
// crash between the 201 and the capture. Single-instance by construction,
// like the alarm scheduler: one loop per process, one process per
// database.
//
// Enrichment is best-effort and never touches the rider path: every
// failure direction lands on snapshot_status = 'unavailable' (definitive)
// or a bounded retry (transient), and the poll predicate's attempts guard
// means no row is retried forever.

const (
	// SnapshotInterval is the poll cadence. Enrichment is not
	// latency-sensitive; 30s keeps the trip's realtime state close to what
	// the rider saw without hammering the store.
	SnapshotInterval = 30 * time.Second
	// snapshotBatchSize bounds one cycle's work. At the §2.6 write throttles'
	// ceiling a backlog deeper than this cannot accumulate from legitimate
	// traffic.
	snapshotBatchSize = 100
)

type TripDetailsSource interface {
	TripDetails(ctx context.Context, region regions.Region, q obaapi.TripDetailsQuery) (json.RawMessage, error)
}

type SnapshotScheduler struct {
	Repo    Repository
	Regions regions.Repository
	OBA     TripDetailsSource
	Now     func() time.Time
	Logger  *slog.Logger
}

// CheckAll runs one polling cycle: claim up to snapshotBatchSize pending
// reports and try to capture each. Sequential deliberately -- the batch is
// small, the upstream deserves politeness, and nothing downstream waits on
// this loop.
func (s *SnapshotScheduler) CheckAll(ctx context.Context) {
	pending, err := s.Repo.ListPendingSnapshots(ctx, snapshotBatchSize)
	if err != nil {
		s.Logger.Error("ghostbus: list pending snapshots", "err", err)
		return
	}
	// One region fetch per region per cycle. The error is cached alongside
	// the region because "region is gone" (unavailable) and "store
	// hiccuped" (skip, retry next cycle) are different facts -- same
	// distinction the alarm scheduler draws.
	type lookup struct {
		region *regions.Region
		err    error
	}
	cache := map[int64]lookup{}
	for _, rep := range pending {
		l, ok := cache[rep.RegionID]
		if !ok {
			region, err := s.Regions.Get(ctx, rep.RegionID)
			l = lookup{err: err}
			if err == nil {
				l.region = &region
			}
			cache[rep.RegionID] = l
		}
		s.capture(ctx, rep, l.region, l.err)
	}
}

func (s *SnapshotScheduler) capture(ctx context.Context, rep Report, region *regions.Region, regionErr error) {
	if region == nil {
		if !errors.Is(regionErr, regions.ErrNotFound) {
			// Transient store failure: a fact about the database, not this
			// report. Recording anything would let one bad minute of SQLite
			// mark every pending report unavailable.
			s.Logger.Warn("ghostbus: resolve region", "region_id", rep.RegionID, "err", regionErr)
			return
		}
		s.markUnavailable(ctx, rep, "region gone")
		return
	}
	snap, err := s.OBA.TripDetails(ctx, *region, obaapi.TripDetailsQuery{
		TripID:      rep.TripIdentifier,
		ServiceDate: rep.ServiceDate,
		RouteID:     rep.RouteIdentifier,
		StopID:      rep.StopIdentifier,
	})
	switch {
	case errors.Is(err, obaapi.ErrNotFound), errors.Is(err, obaapi.ErrNotConfigured):
		// Definitive: the trip is unknown upstream, or the region has no
		// key and never will resolve. Retrying cannot help.
		s.markUnavailable(ctx, rep, "lookup definitive miss")
		return
	case err != nil:
		// Transient. The repository flips the row to unavailable when the
		// increment reaches the cap -- in the same UPDATE, so a crash here
		// cannot strand a row both capped and pending.
		if _, ferr := s.Repo.RecordSnapshotFailure(ctx, rep.ID, s.Now()); ferr != nil {
			s.Logger.Warn("ghostbus: record snapshot failure", "err", ferr)
		}
		s.Logger.Warn("ghostbus: snapshot lookup failed", "region_id", rep.RegionID, "err", err)
		return
	}
	if err := s.Repo.MarkSnapshotCaptured(ctx, rep.ID, string(snap), s.Now()); err != nil {
		s.Logger.Error("ghostbus: mark snapshot captured", "err", err)
	}
}

func (s *SnapshotScheduler) markUnavailable(ctx context.Context, rep Report, why string) {
	if err := s.Repo.MarkSnapshotUnavailable(ctx, rep.ID, s.Now()); err != nil {
		s.Logger.Warn("ghostbus: mark snapshot unavailable", "err", err)
		return
	}
	s.Logger.Info("ghostbus: snapshot unavailable", "region_id", rep.RegionID, "reason", why)
}

// RunLoop calls CheckAll every interval until ctx is done. Mirrors
// alarms.Scheduler.RunLoop's ticker shape.
func (s *SnapshotScheduler) RunLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.CheckAll(ctx)
		}
	}
}
```


- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/ghostbus/`
Expected: PASS. Mutation check: make the transient branch call `MarkSnapshotUnavailable` instead of `RecordSnapshotFailure` — `TestSnapshotSchedulerTransientCountsFailure` must fail; revert.

- [ ] **Step 5: Commit**

```bash
git add internal/ghostbus
git commit -m "feat: ghost bus snapshot scheduler -- DB-as-queue enrichment loop"
```

---

### Task 7: cmd/sidecar wiring

**Files:**
- Modify: `cmd/sidecar/main.go`

**Interfaces:**
- Consumes: Task 3's `store.GhostBus()`, Task 5's Deps fields, Task 6's `SnapshotScheduler` / `SnapshotInterval`; existing `obaClient`, `store`, `logger`, `ctx` in main.

- [ ] **Step 1: Wire the repository and the loop**

Where the other Deps are assembled, add `GhostBus: store.GhostBus(),` (limiters stay nil — `NewRouter` defaults them). Next to the alarm scheduler's startup:

```go
snapSched := &ghostbus.SnapshotScheduler{
	Repo:    store.GhostBus(),
	Regions: store.Regions(),
	OBA:     obaClient,
	Now:     time.Now,
	Logger:  logger,
}
go snapSched.RunLoop(ctx, ghostbus.SnapshotInterval)
```

Match main's existing guard style: the alarm scheduler starts unconditionally with degraded behavior when transports are missing; the snapshot loop likewise always runs — a region with no key resolves to `unavailable` per report, which is the designed behavior, not a boot error. If main gates the OBA client's construction, mirror whatever condition makes `obaClient` non-nil.

- [ ] **Step 2: Build and smoke-test**

Run: `go build ./... && go test ./cmd/...`
Expected: green. Then a manual smoke test:

```sh
go build -o bin/sidecar ./cmd/sidecar && go build -o bin/sidecar-admin ./cmd/sidecar-admin
tmpdb=$(mktemp -d)/smoke.db
./bin/sidecar-admin --db "$tmpdb" region sync
./bin/sidecar --db "$tmpdb" &
sleep 1
curl -si -X POST 'http://localhost:8080/api/v2/regions/1/ghost_bus_reports' \
  -d 'user_identifier=smoke-1&trip_identifier=1_604370&service_date=1754809200000&wait_duration_minutes=15'
# expect: 201 {"id":"..."}; repeat the same command → 422 {"error":"already_reported",...}
kill %1
```

- [ ] **Step 3: Commit**

```bash
git add cmd/sidecar
git commit -m "feat: wire ghost bus reports endpoint and snapshot loop into cmd/sidecar"
```

---

### Task 8: `sidecar-admin ghostbus export` CSV

**Files:**
- Create: `cmd/sidecar-admin/ghostbus.go`
- Modify: `cmd/sidecar-admin/main.go` or `commands.go` (wherever the command switch lives — add the `ghostbus` case and extend the "missing command" message)
- Test: `cmd/sidecar-admin/ghostbus_test.go`

**Interfaces:**
- Consumes: Task 3's `store.GhostBus().ListForExport`, `store.Regions().Get`; Task 1's `HaversineMeters`; existing `csvCell` (formula-injection guard in `surveys.go` — same package, reuse, do not duplicate) and the CLI's `run()` in-process test harness.
- Produces: `sidecar-admin ghostbus export --region N [--since RFC3339]`.

- [ ] **Step 1: Write the failing test**

`cmd/sidecar-admin/ghostbus_test.go`, using the in-process `run()` harness the other CLI tests use (temp db, seeded via the store directly). Seed one region (with `Timezone: "America/Los_Angeles"` via `region set`-equivalent store call) and three reports:

- **R1 "full"**: every optional field set; `SnapshotStatus: captured` with snapshot JSON `{"current_time":1,"status":{"phase":"in_progress","lastKnownLocation":{"lat":47.61,"lon":-122.34}},"display":{"route_short_name":"44","headsign":"=HYPERLINK(evil)","stop_name":"Stop A","stop_lat":47.668,"stop_lon":-122.376,"route_type":3}}`; `Comment: "=1+1"`; `ScheduledArrivalAt`/`PredictionLastUpdatedAt` set 30 minutes apart (ms). `Predicted: true`.
- **R2 "bare"**: only required fields; snapshot still `pending`; same trip as R1's `(trip_identifier, service_date)` — so both rows carry `trip_report_count == 2`.
- **R3 "zeroless"**: `captured` snapshot whose status has `position` but NO `lastKnownLocation` and whose display has NO stop coordinates → `vehicle_distance_from_stop_m` must be **blank** (Null-Island guard), while `vehicle_last_lat/lon` come from `position`.

Assertions on the parsed CSV (use `encoding/csv` reader on the captured stdout):

```go
// Header row is exactly the design-§2.8 column list, in order.
wantHeader := []string{
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
```

- R1: `predicted == "true"`; `comment == "'=1+1"`; `headsign == "'=HYPERLINK(evil)"` (injection guard on snapshot-sourced cells too); `prediction_staleness_minutes == "30"` (ms divisor — this assertion is the 1000× regression guard); `service_date` equals the **date** in America/Los_Angeles for the seeded ms value (compute the expected string in the test from the same zone, e.g. `"2026-08-10"`); `vehicle_distance_from_stop_m` parses as a float within ±20% of a hand-computed haversine; `trip_report_count == "2"`.
- R2: `predicted == ""`; blank snapshot columns; blank staleness; `trip_report_count == "2"`.
- R3: `vehicle_distance_from_stop_m == ""`, `vehicle_last_lat == "47.61"`.
- `--since` set between R1/R2's `created_at` and R3's: only R3 exported.
- Unknown `--region 999`: `run()` returns an error mentioning the region.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./cmd/sidecar-admin/ -run TestGhostBus`
Expected: FAIL — unknown command `ghostbus`.

- [ ] **Step 3: Implement**

`cmd/sidecar-admin/ghostbus.go`:

```go
// ghostBusCmd dispatches the ghostbus subcommands (export only, this
// slice). The CSV is the agency-facing read surface -- there is
// deliberately no rider-facing read API (spec §8).
func ghostBusCmd(ctx context.Context, stdout io.Writer, store *sqlite.Store, args []string) error {
	if len(args) == 0 || args[0] != "export" {
		return errors.New("usage: ghostbus export --region N [--since RFC3339]")
	}
	fs := flag.NewFlagSet("ghostbus export", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	regionID := fs.Int64("region", 0, "region id to export")
	since := fs.String("since", "", "only reports created at or after this RFC 3339 instant")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *regionID == 0 && *since == "" && fs.NFlag() == 0 {
		return errors.New("usage: ghostbus export --region N [--since RFC3339]")
	}
	region, err := store.Regions().Get(ctx, *regionID)
	if err != nil {
		return fmt.Errorf("region %d: %w", *regionID, err)
	}
	var sinceUnix int64
	if *since != "" {
		t, err := time.Parse(time.RFC3339, *since)
		if err != nil {
			return fmt.Errorf("--since must be RFC 3339 with an explicit UTC offset: %w", err)
		}
		sinceUnix = t.Unix()
	}
	reports, err := store.GhostBus().ListForExport(ctx, *regionID, sinceUnix)
	if err != nil {
		return fmt.Errorf("list reports: %w", err)
	}
	return writeGhostBusCSV(stdout, region, reports)
}
```

`writeGhostBusCSV` streams rows with `encoding/csv`, computing:

```go
// ghostBusSnapshot is the subset of the stored snapshot document the CSV
// renders. Pointer fields because absent-vs-zero matters: a defaulted zero
// coordinate is Null Island, and a distance computed from it is garbage.
type ghostBusSnapshot struct {
	Status struct {
		Phase             string     `json:"phase"`
		LastKnownLocation *csvLatLon `json:"lastKnownLocation"`
		Position          *csvLatLon `json:"position"`
	} `json:"status"`
	Display struct {
		RouteShortName string   `json:"route_short_name"`
		Headsign       string   `json:"headsign"`
		StopName       string   `json:"stop_name"`
		StopLat        *float64 `json:"stop_lat"`
		StopLon        *float64 `json:"stop_lon"`
	} `json:"display"`
}
type csvLatLon struct {
	Lat *float64 `json:"lat"`
	Lon *float64 `json:"lon"`
}
```

- Vehicle position: `LastKnownLocation` falling back to `Position` (reference: `status["lastKnownLocation"] || status["position"]`); distance via `ghostbus.HaversineMeters` only when all four coordinates are non-nil, else blank.
- `trip_report_count`: one pre-pass `map[[2]any]int` keyed on `(TripIdentifier, ServiceDate)` over the exported slice.
- Time cells: `msToUTC(ms *int64)` → RFC 3339 UTC or ""; `reported_at_local` via `time.LoadLocation(region.Timezone)` (an unloadable zone falls back to UTC with a warning to stderr — the known unconfigured-region caveat); `service_date` → local **date** `.Format("2006-01-02")`.
- Staleness: `(scheduledArrivalAt - predictionLastUpdatedAt) / 60000`, `math.Round`ed, both required else blank. **The divisor is 60,000 — these are milliseconds.**
- `predicted`: `true`/`false`/"" from the `*bool`.
- Every string cell — including snapshot-derived route names, headsigns, phases, and the raw `snapshot_json` — through `csvCell`.

Add the `case "ghostbus":` to the command switch and extend the missing-command error text to `region, alert, study, survey, ghostbus, migrate, or user`.

- [ ] **Step 4: Run to verify pass**

Run: `go test ./cmd/sidecar-admin/`
Expected: PASS. Mutation check: change the staleness divisor to 60 — the R1 staleness assertion must fail with a value 1000× too large; revert.

- [ ] **Step 5: Commit**

```bash
git add cmd/sidecar-admin
git commit -m "feat: sidecar-admin ghostbus export -- vendor-facing CSV with snapshot columns"
```

---

### Task 9: README, full check, wrap-up

**Files:**
- Modify: `README.md` (new "Ghost bus reports" section after Surveys)

- [ ] **Step 1: Document the feature**

Add a README section covering: the endpoint and its ratified quirks (any-422-reads-as-duplicate on iOS, `already_reported` code, bodyless 403 for oversized JSON and why form bodies are exempt), the two throttles and their §2.6 numbers (and that the per-IP throttle keys on the TCP peer like push registrations — same reverse-proxy caveat), the snapshot worker (30s cadence, 3 tries, `pending`/`captured`/`unavailable`, uses the region's OBA key), and the `ghostbus export` CSV with a note that `--since` requires an explicit UTC offset. Follow the tone and structure of the existing Surveys section.

- [ ] **Step 2: Full verification**

Run: `make check`
Expected: green across fmt-check, vet, lint, test, test-tz, test-race. `test-race` matters here: the concurrent-duplicate storetest and the handler's limiter access run under the race detector.

- [ ] **Step 3: Commit**

```bash
git add README.md
git commit -m "docs: ghost bus reports endpoint, throttles, snapshot worker, CSV export"
```

- [ ] **Step 4: Spec conformance sweep**

Re-read `docs/superpowers/specs/2026-08-23-ghost-bus-reports-design.md` §2.2–§2.8 top to bottom against the shipped code; each numbered decision should map to a test that pins it. Anything unpinned gets a test now, not a comment.
