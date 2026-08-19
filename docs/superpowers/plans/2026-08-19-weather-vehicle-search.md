# Weather + Vehicle Search Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship the sidecar's two upstream-proxy endpoints — regional weather and fuzzy vehicle-id search — together with the OBA REST client, TTL cache, and region columns that alarms and Live Activities will inherit.

**Architecture:** Two sibling domain packages (`internal/weather`, `internal/vehicles`) hold the behavioral contracts as pure functions; they share `internal/obaapi` (a thin wrapper over the official OneBusAway Go SDK) and `internal/cache` (TTL + singleflight, injected clock). `internal/httpapi` stays a thin mapping layer that resolves the region and picks a status code. Three new region columns — nullable `latitude`/`longitude` derived from the directory's `bounds`, and a locally-managed `oba_api_key` — are surfaced through the CLI, admin API, and SPA.

**Tech Stack:** Go 1.26, `github.com/OneBusAway/go-sdk` v1.15.0, `golang.org/x/sync` (singleflight + errgroup), SQLite via `modernc.org/sqlite` with goose migrations and sqlc-generated queries, SvelteKit admin SPA.

**Spec:** [`docs/superpowers/specs/2026-08-19-weather-vehicle-search-design.md`](../specs/2026-08-19-weather-vehicle-search-design.md)

## Global Constraints

These apply to **every** task. They are not repeated in each one.

- **`time.Now` and `time.Local` are banned outside `cmd/`**, enforced by the `forbidigo` linter. Inject `now func() time.Time`. Tests and `cmd/` are exempt.
- **Nothing outside `internal/obaapi` may import `github.com/OneBusAway/go-sdk`.** Same rule already in force for `gen.*` outside `internal/store/sqlite`.
- **Neither API key may appear in any log line, any JSON response, or any error string.** Both travel in URLs (Pirate Weather as a path segment, the OBA key as a query parameter via `option.WithAPIKey`), and `*url.Error` embeds the full URL. Rewrap upstream errors at the package boundary.
- **All SQL uses bare `?` placeholders.** Never mix `sqlc.arg()` with `?` in one statement — it compiles, lints, and diffs cleanly while misbinding every argument at runtime.
- **Timestamps are epoch seconds in INTEGER columns.** Never a SQLite `DATETIME`.
- **Run `make check` before every commit.** It runs `fmt-check vet lint test test-tz test-race web-check`. For Go-only tasks `make test` is enough during the red/green loop, but the commit step runs `make check`.
- **Region id `0` is real** (Tampa Bay). Never treat `0` as "unset".
- Response codes are fixed by the design's §11 table. Weather failures are `403`, never `5xx`. Vehicle failures are `502`.

---

### Task 1: Region centroid from directory bounds

`regions-v3.json` carries no per-region `lat`/`lon` — only `bounds`, an array of rectangles. This task adds the domain type and the pure computation. Nothing is persisted yet (Task 2 adds the columns).

**Files:**
- Modify: `internal/regions/region.go`
- Modify: `internal/regions/directory.go`
- Test: `internal/regions/centroid_test.go` (create)
- Test: `internal/regions/directory_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `regions.LatLon{Lat, Lon float64}`; `regions.Region.Centroid *LatLon`; unexported `computeCentroid(bounds []directoryBound) *LatLon` in `internal/regions`.

- [ ] **Step 1: Write the failing test**

Create `internal/regions/centroid_test.go`:

```go
package regions

import "testing"

func TestComputeCentroid(t *testing.T) {
	tests := []struct {
		name   string
		bounds []directoryBound
		want   *LatLon
	}{
		{
			name:   "no bounds yields nil",
			bounds: nil,
			want:   nil,
		},
		{
			name:   "single rectangle is its own center",
			bounds: []directoryBound{{Lat: 38.5449, Lon: -121.7444, LatSpan: 0.1, LonSpan: 0.1}},
			want:   &LatLon{Lat: 38.5449, Lon: -121.7444},
		},
		{
			// Splitting one rectangle into four quadrants of equal area must
			// not move the centroid. This is the property that rules out the
			// unweighted mean, and the reason weighting is by area.
			name: "split invariance",
			bounds: []directoryBound{
				{Lat: 9.95, Lon: 19.95, LatSpan: 0.1, LonSpan: 0.1},
				{Lat: 9.95, Lon: 20.05, LatSpan: 0.1, LonSpan: 0.1},
				{Lat: 10.05, Lon: 19.95, LatSpan: 0.1, LonSpan: 0.1},
				{Lat: 10.05, Lon: 20.05, LatSpan: 0.1, LonSpan: 0.1},
			},
			want: &LatLon{Lat: 10, Lon: 20},
		},
		{
			// A large rectangle beside a tiny one is dominated by the large
			// one. An unweighted mean would sit halfway between them.
			name: "area weighting dominates",
			bounds: []directoryBound{
				{Lat: 0, Lon: 0, LatSpan: 10, LonSpan: 10},
				{Lat: 50, Lon: 50, LatSpan: 0.01, LonSpan: 0.01},
			},
			want: &LatLon{Lat: 0.0499999, Lon: 0.0499999},
		},
		{
			// Every span zero: fall back to the unweighted mean rather than
			// dividing by zero.
			name: "zero spans fall back to unweighted mean",
			bounds: []directoryBound{
				{Lat: 10, Lon: 20},
				{Lat: 20, Lon: 40},
			},
			want: &LatLon{Lat: 15, Lon: 30},
		},
		{
			name:   "out of range result is nil",
			bounds: []directoryBound{{Lat: 91, Lon: 0, LatSpan: 1, LonSpan: 1}},
			want:   nil,
		},
		{
			name:   "out of range longitude is nil",
			bounds: []directoryBound{{Lat: 0, Lon: 181, LatSpan: 1, LonSpan: 1}},
			want:   nil,
		},
		{
			// Negative spans are nonsense from upstream; clamping them to zero
			// weight keeps them from subtracting area.
			name: "negative spans contribute no weight",
			bounds: []directoryBound{
				{Lat: 10, Lon: 20, LatSpan: 2, LonSpan: 2},
				{Lat: 80, Lon: 170, LatSpan: -5, LonSpan: -5},
			},
			want: &LatLon{Lat: 10, Lon: 20},
		},
	}

	const epsilon = 1e-4
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := computeCentroid(tt.bounds)
			if tt.want == nil {
				if got != nil {
					t.Fatalf("computeCentroid = %+v, want nil", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("computeCentroid = nil, want %+v", tt.want)
			}
			if diff := got.Lat - tt.want.Lat; diff > epsilon || diff < -epsilon {
				t.Errorf("Lat = %v, want %v", got.Lat, tt.want.Lat)
			}
			if diff := got.Lon - tt.want.Lon; diff > epsilon || diff < -epsilon {
				t.Errorf("Lon = %v, want %v", got.Lon, tt.want.Lon)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/regions/ -run TestComputeCentroid -v`
Expected: FAIL — `undefined: directoryBound`, `undefined: LatLon`, `undefined: computeCentroid`.

- [ ] **Step 3: Add the domain type**

In `internal/regions/region.go`, add above `Region`:

```go
// LatLon is a geographic point. It exists so a region's centroid is a pair or
// is absent -- a half-set centroid is not representable.
type LatLon struct {
	Lat float64
	Lon float64
}
```

And add to the `Region` struct, after `Active`:

```go
	// Centroid is the region's area-weighted center, computed from the
	// directory's bounds rectangles. It is nil until a sync supplies usable
	// bounds: 0,0 is a real coordinate in the Gulf of Guinea, so "unset" must
	// not be spelled as a zero value. Weather is unavailable for a region
	// whose centroid is nil.
	Centroid *LatLon
```

- [ ] **Step 4: Add bounds parsing and the computation**

In `internal/regions/directory.go`, add the bound type next to `directoryEntry`:

```go
// directoryBound is one rectangle of a region's coverage. lat/lon is the
// rectangle's center, not a corner. Spans are pointers-free because a missing
// span legitimately means zero area, unlike a missing center.
type directoryBound struct {
	Lat     float64 `json:"lat"`
	Lon     float64 `json:"lon"`
	LatSpan float64 `json:"latSpan"`
	LonSpan float64 `json:"lonSpan"`
}
```

Add the field to `directoryEntry`, after `Active`:

```go
	Bounds         []directoryBound `json:"bounds"`
```

Add the computation at the bottom of the file:

```go
// computeCentroid reduces a region's bounds rectangles to a single point: the
// area-weighted mean of the rectangle centers.
//
// Area weighting is what makes the result invariant to how the bounds were
// split -- cutting one rectangle into four quadrants leaves the centroid
// exactly where it was. The two obvious alternatives both fail on real data:
// the union bounding box's center is dragged into the mountains by one small
// outlying rectangle, and the unweighted mean moves whenever an agency
// re-describes the same coverage with more rectangles.
//
// It returns nil rather than a zero value when there is nothing usable, since
// 0,0 is a real coordinate.
func computeCentroid(bounds []directoryBound) *LatLon {
	if len(bounds) == 0 {
		return nil
	}

	var sumLat, sumLon, sumWeight float64
	for _, b := range bounds {
		// A negative span is nonsense from upstream; clamp to zero weight
		// rather than letting it subtract area from its neighbours.
		w := math.Max(b.LatSpan, 0) * math.Max(b.LonSpan, 0)
		sumLat += b.Lat * w
		sumLon += b.Lon * w
		sumWeight += w
	}

	var lat, lon float64
	if sumWeight > 0 {
		lat, lon = sumLat/sumWeight, sumLon/sumWeight
	} else {
		// Every rectangle has zero area -- a region described as points.
		// The unweighted mean is the only meaningful answer available.
		for _, b := range bounds {
			lat += b.Lat
			lon += b.Lon
		}
		lat /= float64(len(bounds))
		lon /= float64(len(bounds))
	}

	if math.IsNaN(lat) || math.IsNaN(lon) ||
		lat < -90 || lat > 90 || lon < -180 || lon > 180 {
		return nil
	}
	return &LatLon{Lat: lat, Lon: lon}
}
```

Add `"math"` to the file's imports.

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/regions/ -run TestComputeCentroid -v`
Expected: PASS, all subtests.

- [ ] **Step 6: Wire the centroid into validate, with a failing test first**

Append to `internal/regions/directory_test.go`:

```go
// A region whose bounds are unusable must still be kept. Dropping the entry
// would take its alerts feed down too, which is far worse than a missing
// weather card.
func TestFetchKeepsRegionWithUnusableBounds(t *testing.T) {
	body := `{"data":{"list":[
		{"id":1,"regionName":"No Bounds","obaBaseUrl":"https://example.org/","active":true},
		{"id":2,"regionName":"Good","obaBaseUrl":"https://example.org/","active":true,
		 "bounds":[{"lat":47.6,"lon":-122.3,"latSpan":0.2,"lonSpan":0.2}]}
	]}}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, body)
	}))
	defer srv.Close()

	got, err := NewClient(srv.URL, nil, Options{}).Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d regions, want 2 (a bad centroid must not drop the region)", len(got))
	}
	if got[0].Centroid != nil {
		t.Errorf("region 1 Centroid = %+v, want nil", got[0].Centroid)
	}
	if got[1].Centroid == nil {
		t.Fatal("region 2 Centroid = nil, want a point")
	}
	if got[1].Centroid.Lat != 47.6 {
		t.Errorf("region 2 Lat = %v, want 47.6", got[1].Centroid.Lat)
	}
}
```

Match the existing file's `NewClient`/`Options` call style — read the top of `directory_test.go` first and copy the exact constructor invocation the other tests use.

- [ ] **Step 7: Run it to verify it fails**

Run: `go test ./internal/regions/ -run TestFetchKeepsRegionWithUnusableBounds -v`
Expected: FAIL — `Centroid` is nil for region 2 because `validate` does not set it.

- [ ] **Step 8: Set Centroid in validate**

In `internal/regions/directory.go`, in `validate`'s returned `Region` literal, add:

```go
		Centroid:       computeCentroid(e.Bounds),
```

- [ ] **Step 9: Run the whole regions package**

Run: `go test ./internal/regions/ -v`
Expected: PASS, including every pre-existing test.

- [ ] **Step 10: Commit**

```bash
make check
git add internal/regions/
git commit -m "Compute region centroids from the directory's bounds rectangles"
```

---

### Task 2: Region centroid and API key columns

Persists what Task 1 computes, adds the locally-managed `oba_api_key`, and replaces `SetLocalFields`'s three positional strings with a struct.

**Files:**
- Create: `internal/store/sqlite/migrations/00003_region_centroid_and_api_key.sql`
- Modify: `internal/store/sqlite/queries/regions.sql`
- Modify: `internal/store/sqlite/store.go`
- Modify: `internal/regions/region.go`
- Modify: `internal/store/storetest/storetest.go`
- Modify: `cmd/sidecar-admin/commands.go` (call-site update only)
- Modify: `internal/httpapi/admin_regions.go` (call-site update only)

**Interfaces:**
- Consumes: `regions.LatLon`, `regions.Region.Centroid` (Task 1).
- Produces: `regions.LocalFields{DefaultAgencyID, Timezone, OBAAPIKey string}`; `regions.Region.OBAAPIKey string`; `Repository.SetLocalFields(ctx, id int64, in LocalFields, now time.Time) error`.

- [ ] **Step 1: Write the failing conformance tests**

In `internal/store/storetest/storetest.go`, register three subtests inside `RunAlertRepository`, after `PartialUpsertPreservesLocalFields`:

```go
	t.Run("CentroidRoundTrip", func(t *testing.T) { testCentroidRoundTrip(t, newStore) })
	t.Run("CentroidRejectsHalfSet", func(t *testing.T) { testCentroidRejectsHalfSet(t, newStore) })
	t.Run("SetLocalFieldsWritesAllThree", func(t *testing.T) { testSetLocalFieldsWritesAllThree(t, newStore) })
```

Append the implementations to the same file:

```go
// testCentroidRoundTrip pins the three states a centroid can be in. The 0,0
// case is the point of the nullable column: it is a real coordinate in the
// Gulf of Guinea, and must survive as a value rather than reading back as
// "unset".
func testCentroidRoundTrip(t *testing.T, newStore newStoreFunc) {
	_, repo := newStore(t)
	ctx := context.Background()

	in := []regions.Region{
		{ID: 1, Name: "Has Centroid", OBABaseURL: "https://a.example/", Active: true,
			Centroid: &regions.LatLon{Lat: 47.75, Lon: -122.49}},
		{ID: 2, Name: "No Centroid", OBABaseURL: "https://b.example/", Active: true},
		{ID: 3, Name: "Null Island", OBABaseURL: "https://c.example/", Active: true,
			Centroid: &regions.LatLon{Lat: 0, Lon: 0}},
	}
	if err := repo.UpsertFromDirectory(ctx, in, base); err != nil {
		t.Fatalf("UpsertFromDirectory: %v", err)
	}

	got1, err := repo.Get(ctx, 1)
	if err != nil {
		t.Fatalf("Get(1): %v", err)
	}
	if got1.Centroid == nil {
		t.Fatal("region 1 Centroid = nil, want a point")
	}
	if got1.Centroid.Lat != 47.75 || got1.Centroid.Lon != -122.49 {
		t.Errorf("region 1 Centroid = %+v, want {47.75 -122.49}", *got1.Centroid)
	}

	got2, err := repo.Get(ctx, 2)
	if err != nil {
		t.Fatalf("Get(2): %v", err)
	}
	if got2.Centroid != nil {
		t.Errorf("region 2 Centroid = %+v, want nil", *got2.Centroid)
	}

	got3, err := repo.Get(ctx, 3)
	if err != nil {
		t.Fatalf("Get(3): %v", err)
	}
	if got3.Centroid == nil {
		t.Fatal("region 3 Centroid = nil, want 0,0 -- Null Island is a real coordinate")
	}
	if got3.Centroid.Lat != 0 || got3.Centroid.Lon != 0 {
		t.Errorf("region 3 Centroid = %+v, want {0 0}", *got3.Centroid)
	}
}

// testCentroidRejectsHalfSet proves the invariant lives in the schema, not
// only in the Go type. A Postgres adapter expresses this as a CHECK; both
// must refuse the same row.
func testCentroidRejectsHalfSet(t *testing.T, newStore newStoreFunc) {
	_, repo := newStore(t)
	ctx := context.Background()

	if err := repo.UpsertFromDirectory(ctx, []regions.Region{
		{ID: 1, Name: "Whole", OBABaseURL: "https://a.example/", Active: true,
			Centroid: &regions.LatLon{Lat: 47.75, Lon: -122.49}},
	}, base); err != nil {
		t.Fatalf("UpsertFromDirectory: %v", err)
	}

	if err := writeHalfSetCentroid(ctx, repo, 1); err == nil {
		t.Fatal("writing latitude without longitude succeeded, want a constraint failure")
	}
}

// testSetLocalFieldsWritesAllThree pins that the three locally-managed fields
// travel together and that a directory refresh leaves every one of them alone.
func testSetLocalFieldsWritesAllThree(t *testing.T, newStore newStoreFunc) {
	_, repo := newStore(t)
	ctx := context.Background()

	dir := []regions.Region{{ID: 1, Name: "R", OBABaseURL: "https://a.example/", Active: true}}
	if err := repo.UpsertFromDirectory(ctx, dir, base); err != nil {
		t.Fatalf("UpsertFromDirectory: %v", err)
	}

	want := regions.LocalFields{DefaultAgencyID: "40", Timezone: "America/Los_Angeles", OBAAPIKey: "secret-key"}
	if err := repo.SetLocalFields(ctx, 1, want, base); err != nil {
		t.Fatalf("SetLocalFields: %v", err)
	}

	// A refresh must not disturb any of them.
	if err := repo.UpsertFromDirectory(ctx, dir, base.Add(time.Hour)); err != nil {
		t.Fatalf("second UpsertFromDirectory: %v", err)
	}

	got, err := repo.Get(ctx, 1)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.DefaultAgencyID != want.DefaultAgencyID {
		t.Errorf("DefaultAgencyID = %q, want %q", got.DefaultAgencyID, want.DefaultAgencyID)
	}
	if got.Timezone != want.Timezone {
		t.Errorf("Timezone = %q, want %q", got.Timezone, want.Timezone)
	}
	if got.OBAAPIKey != want.OBAAPIKey {
		t.Errorf("OBAAPIKey = %q, want %q", got.OBAAPIKey, want.OBAAPIKey)
	}
}
```

`writeHalfSetCentroid` cannot go through the repository interface — the interface has no way to express an invalid row, which is the point. Add it as an engine-specific hook. In `internal/store/storetest/storetest.go`:

```go
// HalfSetCentroidWriter is implemented by adapters that can attempt an
// invalid half-set centroid write, so the conformance suite can prove the
// storage engine rejects it. An adapter that does not implement it skips
// that subtest rather than silently passing.
type HalfSetCentroidWriter interface {
	WriteHalfSetCentroidForTest(ctx context.Context, id int64) error
}

func writeHalfSetCentroid(ctx context.Context, repo regions.Repository, id int64) error {
	w, ok := repo.(HalfSetCentroidWriter)
	if !ok {
		return errors.New("adapter does not implement HalfSetCentroidWriter")
	}
	return w.WriteHalfSetCentroidForTest(ctx, id)
}
```

Also extend the existing `testPartialUpsertPreservesLocalFields` to assert `OBAAPIKey` survives the refresh, alongside the two fields it already checks.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/store/... -run 'Centroid|SetLocalFields' -v`
Expected: FAIL — `regions.LocalFields` undefined, `Region.OBAAPIKey` undefined, `SetLocalFields` arity mismatch.

- [ ] **Step 3: Write the migration**

Create `internal/store/sqlite/migrations/00003_region_centroid_and_api_key.sql`:

```sql
-- +goose Up
ALTER TABLE regions ADD COLUMN latitude    REAL;
ALTER TABLE regions ADD COLUMN longitude   REAL;
ALTER TABLE regions ADD COLUMN oba_api_key TEXT NOT NULL DEFAULT '';

-- Latitude and longitude are a pair or they are absent. The Go type makes a
-- half-set centroid unrepresentable; without these triggers the schema does
-- not, and a future writer could persist one.
--
-- The goose annotations are required, not decorative: goose splits a migration
-- on semicolons, and a trigger body contains one. Without them each trigger is
-- submitted as two fragments and the migration fails.
-- +goose StatementBegin
CREATE TRIGGER regions_centroid_paired_insert
AFTER INSERT ON regions
WHEN (NEW.latitude IS NULL) <> (NEW.longitude IS NULL)
BEGIN SELECT RAISE(ABORT, 'latitude and longitude must be set together'); END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER regions_centroid_paired_update
AFTER UPDATE OF latitude, longitude ON regions
WHEN (NEW.latitude IS NULL) <> (NEW.longitude IS NULL)
BEGIN SELECT RAISE(ABORT, 'latitude and longitude must be set together'); END;
-- +goose StatementEnd

-- +goose Down
-- The triggers must go first: SQLite refuses to DROP a column that a live
-- trigger still references.
DROP TRIGGER regions_centroid_paired_update;
DROP TRIGGER regions_centroid_paired_insert;
ALTER TABLE regions DROP COLUMN oba_api_key;
ALTER TABLE regions DROP COLUMN longitude;
ALTER TABLE regions DROP COLUMN latitude;
```

- [ ] **Step 4: Update the queries**

In `internal/store/sqlite/queries/regions.sql`, replace the two write queries. Keep every placeholder a bare `?`.

```sql
-- name: UpsertRegionFromDirectory :exec
-- Partial upsert: default_agency_id, timezone, and oba_api_key are locally
-- managed and must survive every refresh. A full-row upsert would wipe them
-- hourly, after which alerts emit an empty agency_id and vehicle search loses
-- its key.
INSERT INTO regions (
  id, region_name, oba_base_url, sidecar_base_url, language, active,
  latitude, longitude, synced_at, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (id) DO UPDATE SET
  region_name      = excluded.region_name,
  oba_base_url     = excluded.oba_base_url,
  sidecar_base_url = excluded.sidecar_base_url,
  language         = excluded.language,
  active           = excluded.active,
  latitude         = excluded.latitude,
  longitude        = excluded.longitude,
  synced_at        = excluded.synced_at,
  updated_at       = excluded.updated_at;

-- name: SetRegionLocalFields :exec
UPDATE regions
SET default_agency_id = ?, timezone = ?, oba_api_key = ?, updated_at = ?
WHERE id = ?;
```

- [ ] **Step 5: Regenerate sqlc and read the result**

Run: `make generate`

Then **read** `internal/store/sqlite/gen/regions.sql.go` and confirm the params struct field order matches the SQL argument order. This is the step that catches a placeholder misbinding, which no test would report as a binding error — only as wrong data.

Expected new fields on `gen.Region`: `Latitude sql.NullFloat64`, `Longitude sql.NullFloat64`, `ObaApiKey string`.

- [ ] **Step 6: Add the domain type and interface change**

In `internal/regions/region.go`, add to `Region` after `Timezone`:

```go
	// OBAAPIKey is this region's key for its OBA REST API server. Empty means
	// "inherit the process default"; it is never echoed back by any surface.
	OBAAPIKey string
```

Add above `Repository`:

```go
// LocalFields carries the region columns the directory does not supply. It is
// a struct rather than three positional strings because three adjacent string
// parameters is the shape that silently swaps two of them.
type LocalFields struct {
	DefaultAgencyID string
	Timezone        string
	OBAAPIKey       string
}
```

Change the interface method to:

```go
	SetLocalFields(ctx context.Context, id int64, in LocalFields, now time.Time) error
```

- [ ] **Step 7: Update the sqlite adapter**

In `internal/store/sqlite/store.go`:

`regionFromRow` gains the centroid and key. A half-null row maps to nil rather than trusting the trigger, because a Postgres adapter will express the invariant as a `CHECK` and both must behave identically:

```go
func regionFromRow(r gen.Region) regions.Region {
	out := regions.Region{
		ID:              r.ID,
		Name:            r.RegionName,
		OBABaseURL:      r.ObaBaseUrl,
		SidecarBaseURL:  r.SidecarBaseUrl,
		Language:        r.Language,
		Active:          r.Active,
		DefaultAgencyID: r.DefaultAgencyID,
		Timezone:        r.Timezone,
		OBAAPIKey:       r.ObaApiKey,
	}
	if r.Latitude.Valid && r.Longitude.Valid {
		out.Centroid = &regions.LatLon{Lat: r.Latitude.Float64, Lon: r.Longitude.Float64}
	}
	return out
}
```

`UpsertFromDirectory`'s params literal gains the two columns:

```go
		lat := sql.NullFloat64{}
		lon := sql.NullFloat64{}
		if reg.Centroid != nil {
			lat = sql.NullFloat64{Float64: reg.Centroid.Lat, Valid: true}
			lon = sql.NullFloat64{Float64: reg.Centroid.Lon, Valid: true}
		}
```

then `Latitude: lat, Longitude: lon,` inside `gen.UpsertRegionFromDirectoryParams`.

`SetLocalFields` takes the struct:

```go
func (r *regionRepo) SetLocalFields(ctx context.Context, id int64, in regions.LocalFields, now time.Time) error {
	if err := r.q.SetRegionLocalFields(ctx, gen.SetRegionLocalFieldsParams{
		DefaultAgencyID: in.DefaultAgencyID,
		Timezone:        in.Timezone,
		ObaApiKey:       in.OBAAPIKey,
		UpdatedAt:       now.Unix(),
		ID:              id,
	}); err != nil {
		return fmt.Errorf("sqlite: set local fields for region %d: %w", id, err)
	}
	return nil
}
```

Add the conformance hook:

```go
// WriteHalfSetCentroidForTest deliberately writes an invalid half-set
// centroid so the conformance suite can prove the engine rejects it. It
// bypasses the generated queries because no legitimate query can express
// this row -- which is exactly the property under test.
func (r *regionRepo) WriteHalfSetCentroidForTest(ctx context.Context, id int64) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE regions SET latitude = ?, longitude = NULL WHERE id = ?`, 47.5, id)
	return err
}
```

- [ ] **Step 8: Update the two call sites so the tree compiles**

In `cmd/sidecar-admin/commands.go`, `regionSet` currently ends with
`repo.SetLocalFields(ctx, *id, newAgencyID, newTimezone, now)`. Change to:

```go
	if err := repo.SetLocalFields(ctx, *id, regions.LocalFields{
		DefaultAgencyID: newAgencyID,
		Timezone:        newTimezone,
		OBAAPIKey:       current.OBAAPIKey,
	}, now); err != nil {
```

In `internal/httpapi/admin_regions.go`, find the `SetLocalFields` call in `patch` and pass a `regions.LocalFields` built the same way, carrying `current.OBAAPIKey` through unchanged. **No new flag or JSON field yet** — Task 8 adds those. Carrying the current value through is what keeps this task from silently wiping keys.

Also update the two `SetLocalFields` calls in `newAdminFixture` in `internal/httpapi/admin_alerts_test.go`, which still pass three positional strings:

```go
	if err := store.Regions().SetLocalFields(ctx, regionTampa, regions.LocalFields{
		DefaultAgencyID: "HART", Timezone: "America/New_York",
	}, testNow); err != nil {
		t.Fatalf("configure region %d: %v", regionTampa, err)
	}
	if err := store.Regions().SetLocalFields(ctx, regionPuget, regions.LocalFields{
		DefaultAgencyID: "1", Timezone: "America/Los_Angeles",
	}, testNow); err != nil {
		t.Fatalf("configure region %d: %v", regionPuget, err)
	}
```

Run `grep -rn "SetLocalFields" --include=*.go .` and fix every remaining call site the same way; the compiler will find them, but the grep tells you the size of the change before you start.

- [ ] **Step 9: Run the tests**

Run: `go test ./internal/... ./cmd/...`
Expected: PASS, including the three new conformance subtests and the extended `PartialUpsertPreservesLocalFields`.

- [ ] **Step 10: Commit**

```bash
make check
git add internal/store/ internal/regions/ cmd/sidecar-admin/ internal/httpapi/
git commit -m "Persist region centroids and a per-region OBA API key"
```

---

### Task 3: `internal/cache`

TTL memoization with singleflight, an injected clock, and two bounds. No dependency on anything else in this plan.

**Files:**
- Create: `internal/cache/cache.go`
- Test: `internal/cache/cache_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `cache.New[V any](ttl time.Duration, maxEntries int, budget time.Duration, now func() time.Time) *cache.Cache[V]`; `(*cache.Cache[V]).Get(ctx context.Context, key string, fetch func(context.Context) (V, error)) (V, error)`.

- [ ] **Step 1: Write the failing tests**

Create `internal/cache/cache_test.go`:

```go
package cache

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock is the injected clock; the package may not call time.Now.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func TestHitWithinTTL(t *testing.T) {
	clk := newClock()
	c := New[int](time.Minute, 8, time.Second, clk.Now)
	var calls atomic.Int64

	fetch := func(context.Context) (int, error) { calls.Add(1); return 42, nil }

	for i := 0; i < 3; i++ {
		got, err := c.Get(context.Background(), "k", fetch)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if got != 42 {
			t.Fatalf("Get = %d, want 42", got)
		}
	}
	if calls.Load() != 1 {
		t.Errorf("fetch called %d times, want 1", calls.Load())
	}
}

func TestMissAfterTTL(t *testing.T) {
	clk := newClock()
	c := New[int](time.Minute, 8, time.Second, clk.Now)
	var calls atomic.Int64
	fetch := func(context.Context) (int, error) { calls.Add(1); return 1, nil }

	if _, err := c.Get(context.Background(), "k", fetch); err != nil {
		t.Fatalf("Get: %v", err)
	}
	clk.Advance(time.Minute + time.Nanosecond)
	if _, err := c.Get(context.Background(), "k", fetch); err != nil {
		t.Fatalf("Get after expiry: %v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("fetch called %d times, want 2", calls.Load())
	}
}

// A cached failure turns a five-second upstream blip into a thirty-minute
// outage. Errors must never be stored.
func TestErrorsAreNotCached(t *testing.T) {
	clk := newClock()
	c := New[int](time.Minute, 8, time.Second, clk.Now)
	var calls atomic.Int64
	boom := errors.New("boom")
	fetch := func(context.Context) (int, error) { calls.Add(1); return 0, boom }

	for i := 0; i < 2; i++ {
		if _, err := c.Get(context.Background(), "k", fetch); !errors.Is(err, boom) {
			t.Fatalf("Get err = %v, want boom", err)
		}
	}
	if calls.Load() != 2 {
		t.Errorf("fetch called %d times, want 2", calls.Load())
	}
}

// A burst of keystrokes on a cold cache must cost one upstream call.
func TestSingleflightCollapsesConcurrentGets(t *testing.T) {
	clk := newClock()
	c := New[int](time.Minute, 8, time.Second, clk.Now)

	var calls atomic.Int64
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	fetch := func(context.Context) (int, error) {
		calls.Add(1)
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		return 7, nil
	}

	const n = 20
	var wg sync.WaitGroup
	results := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			v, err := c.Get(context.Background(), "k", fetch)
			if err != nil {
				t.Errorf("Get: %v", err)
				return
			}
			results[i] = v
		}(i)
	}

	<-entered
	close(release)
	wg.Wait()

	if calls.Load() != 1 {
		t.Errorf("fetch called %d times, want 1", calls.Load())
	}
	for i, v := range results {
		if v != 7 {
			t.Errorf("results[%d] = %d, want 7", i, v)
		}
	}
}

// The critical case: a cancelled caller must stop waiting, and the shared
// fetch must nevertheless finish and be cached for everyone else. This fails
// if singleflight.Do is used instead of DoChan, and it fails if the fetch
// context is not detached from the caller's.
func TestCancelledCallerDoesNotKillSharedFetch(t *testing.T) {
	clk := newClock()
	c := New[int](time.Minute, 8, time.Second, clk.Now)

	release := make(chan struct{})
	fetchCtxErr := make(chan error, 1)
	entered := make(chan struct{})
	fetch := func(ctx context.Context) (int, error) {
		close(entered)
		<-release
		fetchCtxErr <- ctx.Err()
		return 9, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := c.Get(ctx, "k", fetch)
		done <- err
	}()

	<-entered
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Get err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Get did not return after its caller was cancelled")
	}

	close(release)
	if err := <-fetchCtxErr; err != nil {
		t.Errorf("fetch context err = %v, want nil (the fetch must outlive its first caller)", err)
	}

	// The value the abandoned fetch produced must be cached.
	var calls atomic.Int64
	got, err := c.Get(context.Background(), "k", func(context.Context) (int, error) {
		calls.Add(1)
		return 0, errors.New("should not be called")
	})
	if err != nil {
		t.Fatalf("Get after cancellation: %v", err)
	}
	if got != 9 {
		t.Errorf("Get = %d, want 9", got)
	}
	if calls.Load() != 0 {
		t.Error("the abandoned fetch's value was not cached")
	}
}

// The fetch's budget is the cache's, measured from fetch start, not inherited
// from any caller.
func TestFetchBudgetApplies(t *testing.T) {
	clk := newClock()
	c := New[int](time.Minute, 8, 50*time.Millisecond, clk.Now)

	_, err := c.Get(context.Background(), "k", func(ctx context.Context) (int, error) {
		<-ctx.Done()
		return 0, ctx.Err()
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Get err = %v, want DeadlineExceeded", err)
	}
}

// The query cache is keyed by attacker-controlled input, so unbounded growth
// is a memory exhaustion vector on an unauthenticated endpoint.
func TestEvictionPrefersExpiredThenOldest(t *testing.T) {
	clk := newClock()
	c := New[int](time.Minute, 2, time.Second, clk.Now)
	ctx := context.Background()
	val := func(v int) func(context.Context) (int, error) {
		return func(context.Context) (int, error) { return v, nil }
	}

	if _, err := c.Get(ctx, "a", val(1)); err != nil {
		t.Fatal(err)
	}
	clk.Advance(time.Second)
	if _, err := c.Get(ctx, "b", val(2)); err != nil {
		t.Fatal(err)
	}
	// Inserting a third entry at capacity must drop "a", the oldest.
	clk.Advance(time.Second)
	if _, err := c.Get(ctx, "c", val(3)); err != nil {
		t.Fatal(err)
	}

	if c.Len() != 2 {
		t.Fatalf("Len = %d, want 2", c.Len())
	}
	var refetched atomic.Int64
	if _, err := c.Get(ctx, "a", func(context.Context) (int, error) {
		refetched.Add(1)
		return 1, nil
	}); err != nil {
		t.Fatal(err)
	}
	if refetched.Load() != 1 {
		t.Error(`"a" was not evicted; eviction must drop the oldest entry`)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/cache/ -v`
Expected: FAIL — the package does not exist.

- [ ] **Step 3: Write the implementation**

Create `internal/cache/cache.go`:

```go
// Package cache memoizes one value per key for a fixed TTL, collapsing
// concurrent misses into a single fetch. It exists so that N riders searching
// the same stop, or N instances of the same keystroke, cost one upstream call
// rather than N.
//
// It reads no clock of its own: the design bans time.Now outside cmd/, and a
// cache whose expiry cannot be advanced by a test is a cache with untestable
// expiry.
package cache

import (
	"container/list"
	"context"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// Cache memoizes one value per key for a fixed TTL. It is safe for concurrent
// use.
//
// There is deliberately no Set. A caller that could Set could store a value
// whose age the cache never measured, and the entire contract here is "this
// value is at most ttl old".
type Cache[V any] struct {
	ttl        time.Duration
	maxEntries int
	budget     time.Duration
	now        func() time.Time

	group singleflight.Group

	mu    sync.Mutex
	items map[string]*list.Element
	// order is oldest-first, so eviction reads from the front.
	order *list.List
}

type entry[V any] struct {
	key     string
	value   V
	expires time.Time
}

// New builds a cache holding at most maxEntries values, each valid for ttl.
// budget bounds a single fetch; see Get.
func New[V any](ttl time.Duration, maxEntries int, budget time.Duration, now func() time.Time) *Cache[V] {
	if maxEntries < 1 {
		maxEntries = 1
	}
	return &Cache[V]{
		ttl:        ttl,
		maxEntries: maxEntries,
		budget:     budget,
		now:        now,
		items:      make(map[string]*list.Element, maxEntries),
		order:      list.New(),
	}
}

// Get returns the cached value for key, or calls fetch and stores the result.
//
// Concurrent Gets for the same key while a fetch is in flight share that one
// fetch. Errors are returned to every waiter and are never cached: caching a
// failure would turn a brief upstream blip into an outage lasting a full ttl.
//
// Context handling is the subtle part, because two requirements pull against
// each other. A caller whose request is cancelled must stop waiting; the
// shared fetch must not die with that caller, since other waiters still need
// its result. So the fetch runs on a context detached from every caller
// (context.WithoutCancel) carrying a fresh budget measured from fetch start,
// while Get itself selects on the caller's ctx.Done(). That also forces
// DoChan rather than Do -- Do blocks uninterruptibly, so a cancelled caller
// could not stop waiting at all.
func (c *Cache[V]) Get(ctx context.Context, key string, fetch func(context.Context) (V, error)) (V, error) {
	if v, ok := c.lookup(key); ok {
		return v, nil
	}

	ch := c.group.DoChan(key, func() (any, error) {
		fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.budget)
		defer cancel()

		v, err := fetch(fetchCtx)
		if err != nil {
			return nil, err
		}
		c.store(key, v)
		return v, nil
	})

	var zero V
	select {
	case <-ctx.Done():
		return zero, ctx.Err()
	case res := <-ch:
		if res.Err != nil {
			return zero, res.Err
		}
		v, ok := res.Val.(V)
		if !ok {
			return zero, nil
		}
		return v, nil
	}
}

// Len reports how many entries are held. It exists for tests asserting the
// entry cap.
func (c *Cache[V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}

func (c *Cache[V]) lookup(key string) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	var zero V
	el, ok := c.items[key]
	if !ok {
		return zero, false
	}
	e, ok := el.Value.(*entry[V])
	if !ok {
		return zero, false
	}
	if !c.now().Before(e.expires) {
		c.removeLocked(el)
		return zero, false
	}
	return e.value, true
}

func (c *Cache[V]) store(key string, v V) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if el, ok := c.items[key]; ok {
		c.removeLocked(el)
	}
	c.evictLocked()
	c.items[key] = c.order.PushBack(&entry[V]{
		key:     key,
		value:   v,
		expires: c.now().Add(c.ttl),
	})
}

// evictLocked makes room for one insert: expired entries first, then the
// oldest. Preferring expired entries keeps a burst of distinct keys from
// throwing away live values while dead ones sit in the map.
func (c *Cache[V]) evictLocked() {
	now := c.now()
	for el := c.order.Front(); el != nil && c.order.Len() >= c.maxEntries; {
		next := el.Next()
		if e, ok := el.Value.(*entry[V]); ok && !now.Before(e.expires) {
			c.removeLocked(el)
		}
		el = next
	}
	for c.order.Len() >= c.maxEntries {
		front := c.order.Front()
		if front == nil {
			return
		}
		c.removeLocked(front)
	}
}

func (c *Cache[V]) removeLocked(el *list.Element) {
	if e, ok := el.Value.(*entry[V]); ok {
		delete(c.items, e.key)
	}
	c.order.Remove(el)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/cache/ -race -v`
Expected: PASS, all eight tests, race detector clean.

- [ ] **Step 5: Promote x/sync to a direct dependency**

Run: `go get golang.org/x/sync@v0.22.0 && make tidy`
Confirm `golang.org/x/sync` moved out of the `// indirect` block in `go.mod`.

- [ ] **Step 6: Commit**

```bash
make check
git add internal/cache/ go.mod go.sum
git commit -m "Add a TTL cache with singleflight and bounded entry count"
```

---

### Task 4: `internal/obaapi`

The OBA REST API client. The only package permitted to import the SDK.

**Files:**
- Create: `internal/obaapi/obaapi.go`
- Test: `internal/obaapi/obaapi_test.go`

**Interfaces:**
- Consumes: `regions.Region` (`OBABaseURL`, `OBAAPIKey`) from Task 2.
- Produces: `obaapi.Agency{ID, Name string}`; `obaapi.Vehicle{AgencyID, AgencyName, VehicleID string}`; `obaapi.Client` interface with `Fleet(ctx context.Context, region regions.Region) ([]Vehicle, error)`; `obaapi.New(defaultKey string, httpClient *http.Client, logger *slog.Logger) Client`; `obaapi.ErrNotConfigured`.

- [ ] **Step 1: Add the SDK dependency**

Run: `go get github.com/OneBusAway/go-sdk@v1.15.0 && make tidy`

- [ ] **Step 2: Write the failing tests**

Create `internal/obaapi/obaapi_test.go`. The fake OBA server returns the real wire shapes, so the SDK's own decoding is exercised:

```go
package obaapi

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/OneBusAway/sidecar/internal/regions"
)

const sentinelKey = "SENTINEL-API-KEY-do-not-log"

// obaServer stands in for a region's OBA REST API server. vehicleStatus lets
// a test make one agency's call fail with a specific code.
type obaServer struct {
	*httptest.Server
	agencyCalls   atomic.Int64
	vehicleStatus map[string]int
}

func newOBAServer(t *testing.T, agencies []struct{ ID, Name string }, vehicles map[string][]string) *obaServer {
	t.Helper()
	s := &obaServer{vehicleStatus: map[string]int{}}
	mux := http.NewServeMux()

	mux.HandleFunc("/api/where/agencies-with-coverage.json", func(w http.ResponseWriter, r *http.Request) {
		s.agencyCalls.Add(1)
		list := []map[string]any{}
		refs := []map[string]any{}
		for _, a := range agencies {
			list = append(list, map[string]any{
				"agencyId": a.ID, "lat": 47.6, "lon": -122.3, "latSpan": 0.1, "lonSpan": 0.1,
			})
			refs = append(refs, map[string]any{"id": a.ID, "name": a.Name})
		}
		writeOBA(w, map[string]any{
			"list":       list,
			"references": map[string]any{"agencies": refs, "routes": []any{}, "situations": []any{}, "stops": []any{}, "stopTimes": []any{}, "trips": []any{}},
		})
	})

	mux.HandleFunc("/api/where/vehicles-for-agency/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/where/vehicles-for-agency/"), ".json")
		if code, ok := s.vehicleStatus[id]; ok {
			w.WriteHeader(code)
			return
		}
		list := []map[string]any{}
		for _, v := range vehicles[id] {
			list = append(list, map[string]any{
				"vehicleId": v, "lastUpdateTime": 0, "lastLocationUpdateTime": 0,
			})
		}
		writeOBA(w, map[string]any{
			"list":       list,
			"references": map[string]any{"agencies": []any{}, "routes": []any{}, "situations": []any{}, "stops": []any{}, "stopTimes": []any{}, "trips": []any{}},
		})
	})

	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func writeOBA(w http.ResponseWriter, data map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"code": 200, "version": 2, "currentTime": 0, "text": "OK", "data": data,
	})
}

func testRegion(baseURL, key string) regions.Region {
	return regions.Region{ID: 1, Name: "Test", OBABaseURL: baseURL, OBAAPIKey: key, Active: true}
}

func TestFleetResolvesAgencyNamesFromReferences(t *testing.T) {
	srv := newOBAServer(t,
		[]struct{ ID, Name string }{{"1", "Metro Transit"}, {"3", "Community Transit"}},
		map[string][]string{"1": {"1_4361", "1_4362"}, "3": {"3_99"}},
	)

	got, err := New("", srv.Client(), slog.New(slog.DiscardHandler)).
		Fleet(context.Background(), testRegion(srv.URL, sentinelKey))
	if err != nil {
		t.Fatalf("Fleet: %v", err)
	}

	want := []Vehicle{
		{AgencyID: "1", AgencyName: "Metro Transit", VehicleID: "1_4361"},
		{AgencyID: "1", AgencyName: "Metro Transit", VehicleID: "1_4362"},
		{AgencyID: "3", AgencyName: "Community Transit", VehicleID: "3_99"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d vehicles, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("vehicle %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// Parallel completion order is not deterministic, so the result must be
// reassembled by index rather than by arrival.
func TestFleetOrderIsDeterministic(t *testing.T) {
	agencies := []struct{ ID, Name string }{}
	vehicles := map[string][]string{}
	for _, id := range []string{"1", "2", "3", "4", "5", "6"} {
		agencies = append(agencies, struct{ ID, Name string }{id, "Agency " + id})
		vehicles[id] = []string{id + "_a", id + "_b"}
	}
	srv := newOBAServer(t, agencies, vehicles)
	c := New("", srv.Client(), slog.New(slog.DiscardHandler))

	first, err := c.Fleet(context.Background(), testRegion(srv.URL, sentinelKey))
	if err != nil {
		t.Fatalf("Fleet: %v", err)
	}
	for i := 0; i < 5; i++ {
		got, err := c.Fleet(context.Background(), testRegion(srv.URL, sentinelKey))
		if err != nil {
			t.Fatalf("Fleet: %v", err)
		}
		for j := range first {
			if got[j] != first[j] {
				t.Fatalf("run %d vehicle %d = %+v, want %+v", i, j, got[j], first[j])
			}
		}
	}
}

// An agency with no realtime feed answers 4xx forever. Failing the whole
// fetch would brick vehicle search for the region permanently.
func TestFleetTolerates4xxFromOneAgency(t *testing.T) {
	srv := newOBAServer(t,
		[]struct{ ID, Name string }{{"1", "Metro"}, {"2", "NoRealtime"}},
		map[string][]string{"1": {"1_1"}},
	)
	srv.vehicleStatus["2"] = http.StatusNotFound

	got, err := New("", srv.Client(), slog.New(slog.DiscardHandler)).
		Fleet(context.Background(), testRegion(srv.URL, sentinelKey))
	if err != nil {
		t.Fatalf("Fleet: %v", err)
	}
	if len(got) != 1 || got[0].VehicleID != "1_1" {
		t.Errorf("Fleet = %+v, want just 1_1", got)
	}
}

// A 5xx is a real failure: caching a fleet with an agency silently missing
// tells every rider on its routes that their bus does not exist.
func TestFleetFailsOn5xxFromOneAgency(t *testing.T) {
	srv := newOBAServer(t,
		[]struct{ ID, Name string }{{"1", "Metro"}, {"2", "Broken"}},
		map[string][]string{"1": {"1_1"}, "2": {"2_2"}},
	)
	srv.vehicleStatus["2"] = http.StatusInternalServerError

	if _, err := New("", srv.Client(), slog.New(slog.DiscardHandler)).
		Fleet(context.Background(), testRegion(srv.URL, sentinelKey)); err == nil {
		t.Fatal("Fleet succeeded, want an error when an agency returns 500")
	}
}

func TestFleetWithoutKeyMakesNoRequest(t *testing.T) {
	srv := newOBAServer(t, []struct{ ID, Name string }{{"1", "Metro"}}, map[string][]string{"1": {"1_1"}})

	_, err := New("", srv.Client(), slog.New(slog.DiscardHandler)).
		Fleet(context.Background(), testRegion(srv.URL, ""))
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("Fleet err = %v, want ErrNotConfigured", err)
	}
	if srv.agencyCalls.Load() != 0 {
		t.Errorf("made %d requests, want 0", srv.agencyCalls.Load())
	}
}

func TestRegionKeyOverridesDefault(t *testing.T) {
	var seen atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Store(r.URL.Query().Get("key"))
		writeOBA(w, map[string]any{
			"list":       []any{},
			"references": map[string]any{"agencies": []any{}, "routes": []any{}, "situations": []any{}, "stops": []any{}, "stopTimes": []any{}, "trips": []any{}},
		})
	}))
	defer srv.Close()

	_, _ = New("process-default", srv.Client(), slog.New(slog.DiscardHandler)).
		Fleet(context.Background(), testRegion(srv.URL, "region-key"))
	if got, _ := seen.Load().(string); got != "region-key" {
		t.Errorf("key = %q, want region-key", got)
	}

	_, _ = New("process-default", srv.Client(), slog.New(slog.DiscardHandler)).
		Fleet(context.Background(), testRegion(srv.URL, ""))
	if got, _ := seen.Load().(string); got != "process-default" {
		t.Errorf("key = %q, want process-default", got)
	}
}

// The SDK puts the key in the query string, and *url.Error embeds the full
// URL. An error logged verbatim would write the secret to disk.
func TestErrorsDoNotLeakTheKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Close() // closed immediately: every request is a transport failure

	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))

	_, err := New("", srv.Client(), logger).
		Fleet(context.Background(), testRegion(srv.URL, sentinelKey))
	if err == nil {
		t.Fatal("Fleet succeeded against a closed server, want an error")
	}
	if strings.Contains(err.Error(), sentinelKey) {
		t.Errorf("error text leaks the API key: %v", err)
	}
	if strings.Contains(logs.String(), sentinelKey) {
		t.Errorf("log output leaks the API key: %s", logs.String())
	}
}
```

Add `"errors"` to the test imports.

- [ ] **Step 3: Run tests to verify they fail**

Run: `go test ./internal/obaapi/ -v`
Expected: FAIL — package does not exist.

- [ ] **Step 4: Write the implementation**

Create `internal/obaapi/obaapi.go`:

```go
// Package obaapi is the sidecar's client for the OneBusAway REST API.
//
// It is the only package permitted to import github.com/OneBusAway/go-sdk.
// The SDK's response types are generated, deeply nested, and carry per-field
// JSON metadata on every struct; letting them reach the domain packages would
// make those untestable without the SDK and pin the sidecar to the SDK's type
// layout. Everything crossing this boundary is a flat local type.
package obaapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	oba "github.com/OneBusAway/go-sdk"
	"github.com/OneBusAway/go-sdk/option"
	"golang.org/x/sync/errgroup"

	"github.com/OneBusAway/sidecar/internal/regions"
)

// ErrNotConfigured means neither the region nor the process supplied an API
// key, so no request was attempted.
var ErrNotConfigured = errors.New("obaapi: region has no API key")

// Agency is one transit agency with coverage in a region.
type Agency struct {
	ID   string
	Name string
}

// Vehicle is one vehicle currently reported by an agency's realtime feed.
type Vehicle struct {
	AgencyID   string
	AgencyName string
	VehicleID  string
}

// Client reads the OBA REST API for one region at a time. Implementations
// must be safe for concurrent use.
type Client interface {
	// Fleet returns every vehicle currently reported across every agency with
	// coverage in the region, in agencies-with-coverage order then each
	// agency's own response order.
	Fleet(ctx context.Context, region regions.Region) ([]Vehicle, error)
}

const (
	// perRequestTimeout bounds one HTTP attempt. The SDK documents its
	// request timeout as per-attempt, spanning neither retries nor the
	// surrounding context, which is why retries are disabled below -- with
	// retries on, one logical call is two attempts plus backoff and no
	// timeout arithmetic holds.
	perRequestTimeout = 4 * time.Second

	// maxRetries is zero deliberately; see perRequestTimeout.
	maxRetries = 0

	// agencyConcurrency bounds the vehicles-for-agency fan-out. Twelve keeps
	// every region that exists today to a single round while staying polite
	// to the upstream server.
	agencyConcurrency = 12
)

type client struct {
	defaultKey string
	http       *http.Client
	logger     *slog.Logger
}

// New builds a Client. defaultKey is the process-wide fallback used for any
// region that carries no key of its own; it may be empty.
func New(defaultKey string, httpClient *http.Client, logger *slog.Logger) Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &client{defaultKey: defaultKey, http: httpClient, logger: logger}
}

func (c *client) Fleet(ctx context.Context, region regions.Region) ([]Vehicle, error) {
	key := region.OBAAPIKey
	if key == "" {
		key = c.defaultKey
	}
	if key == "" {
		return nil, ErrNotConfigured
	}

	sdk := oba.NewClient(
		option.WithBaseURL(region.OBABaseURL),
		option.WithAPIKey(key),
		option.WithHTTPClient(c.http),
		option.WithRequestTimeout(perRequestTimeout),
		option.WithMaxRetries(maxRetries),
	)

	agencies, err := c.agencies(ctx, sdk)
	if err != nil {
		return nil, err
	}

	// Per-agency results are collected into a slice indexed by agency
	// position, not appended as they arrive: parallel completion order is not
	// deterministic and the response must be.
	perAgency := make([][]Vehicle, len(agencies))

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(agencyConcurrency)
	for i, agency := range agencies {
		g.Go(func() error {
			list, err := sdk.VehiclesForAgency.List(gctx, agency.ID, oba.VehiclesForAgencyListParams{})
			if err != nil {
				// An agency listed in agencies-with-coverage but with no
				// realtime feed answers 4xx forever. Failing the whole fetch
				// would take the region's vehicle search down permanently and
				// re-hammer the upstream on every miss. Anything else -- a
				// 5xx, a timeout, a transport error -- is a real failure:
				// caching a fleet with an agency silently missing tells every
				// rider on its routes that their bus does not exist.
				if isClientError(err) {
					c.logger.Warn("obaapi: agency has no vehicle feed",
						"region_id", region.ID, "agency_id", agency.ID, "status", statusOf(err))
					return nil
				}
				return fmt.Errorf("obaapi: vehicles for agency %s in region %d: %w",
					agency.ID, region.ID, redact(err))
			}
			out := make([]Vehicle, 0, len(list.Data.List))
			for _, v := range list.Data.List {
				out = append(out, Vehicle{
					AgencyID:   agency.ID,
					AgencyName: agency.Name,
					VehicleID:  v.VehicleID,
				})
			}
			perAgency[i] = out
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}

	total := 0
	for _, vs := range perAgency {
		total += len(vs)
	}
	fleet := make([]Vehicle, 0, total)
	for _, vs := range perAgency {
		fleet = append(fleet, vs...)
	}
	return fleet, nil
}

// agencies fetches the region's agencies. Names live in the response's
// references block rather than on the list entries (which carry only ids and
// bounding boxes), so one call yields both.
func (c *client) agencies(ctx context.Context, sdk *oba.Client) ([]Agency, error) {
	resp, err := sdk.AgenciesWithCoverage.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("obaapi: agencies with coverage: %w", redact(err))
	}

	names := make(map[string]string, len(resp.Data.References.Agencies))
	for _, a := range resp.Data.References.Agencies {
		names[a.ID] = a.Name
	}

	out := make([]Agency, 0, len(resp.Data.List))
	for _, a := range resp.Data.List {
		out = append(out, Agency{ID: a.AgencyID, Name: names[a.AgencyID]})
	}
	return out, nil
}
```

Add `"time"` to the imports.

Then create the error helpers in the same file. Inspect the SDK's error type first — run
`grep -rn "type Error struct" $(go env GOMODCACHE)/github.com/\!one\!bus\!away/go-sdk@v1.15.0/*.go`
and use its actual status field:

```go
// redact strips any URL-bearing text from an upstream error. Both API keys
// travel in the URL -- Pirate Weather's as a path segment, the OBA key as a
// query parameter -- and *url.Error embeds the full URL in its message. An
// error logged verbatim writes the secret to disk, undoing the care taken to
// keep it out of every JSON response.
func redact(err error) error {
	if err == nil {
		return nil
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return fmt.Errorf("%s request failed: %w", urlErr.Op, urlErr.Err)
	}
	if code := statusOf(err); code != 0 {
		return fmt.Errorf("upstream returned status %d", code)
	}
	return errors.New("upstream request failed")
}

// statusOf reports the HTTP status an SDK error carries, or 0.
func statusOf(err error) int {
	var apiErr *oba.Error
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode
	}
	return 0
}

// isClientError reports whether err is a 4xx from the upstream.
func isClientError(err error) bool {
	code := statusOf(err)
	return code >= 400 && code < 500
}
```

Add `"net/url"` to the imports. If the SDK's error type or status field differs from `oba.Error`/`StatusCode`, adjust these three helpers — the tests pin the *behavior*, not the field name.

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/obaapi/ -race -v`
Expected: PASS, all seven tests.

If `TestFleetResolvesAgencyNamesFromReferences` fails on the request path, print `r.URL.Path` in the fake server and correct the mux patterns to whatever the SDK actually requests.

- [ ] **Step 6: Commit**

```bash
make check
git add internal/obaapi/ go.mod go.sum
git commit -m "Add the OBA REST API client over the official Go SDK"
```

---

### Task 5: Vehicle search endpoint

**Files:**
- Create: `internal/vehicles/vehicles.go`
- Create: `internal/vehicles/vehicles_test.go`
- Create: `internal/httpapi/vehicles.go`
- Create: `internal/httpapi/vehicles_test.go`
- Modify: `internal/httpapi/router.go`
- Modify: `cmd/sidecar/main.go`

**Interfaces:**
- Consumes: `obaapi.Client`, `obaapi.Vehicle`, `obaapi.ErrNotConfigured` (Task 4); `cache.New`/`Get` (Task 3); `regions.Region` (Task 2).
- Produces: `vehicles.Match{ID, Name, VehicleID string}` with JSON tags `id`, `name`, `vehicle_id`; `vehicles.Normalize(query string) (string, bool)`; `vehicles.Filter(fleet []obaapi.Vehicle, q string) []Match`; `vehicles.Service` with `Search(ctx context.Context, region regions.Region, rawQuery string) ([]Match, error)`; `httpapi.Deps.Vehicles *vehicles.Service`.

- [ ] **Step 1: Write the failing pure-logic tests**

Create `internal/vehicles/vehicles_test.go`:

```go
package vehicles

import (
	"strings"
	"testing"

	"github.com/OneBusAway/sidecar/internal/obaapi"
)

func TestNormalize(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantOK  bool
	}{
		{"trims and lowers", "  ABC  ", "abc", true},
		{"two characters is too short", "ab", "", false},
		{"empty is too short", "", "", false},
		{"three characters is enough", "abc", "abc", true},
		{"whitespace only is too short", "     ", "", false},
		// Rails counts characters, not bytes. Two CJK characters are six
		// bytes; a byte-length check would let them through into a
		// full-fleet scan.
		{"two CJK characters are two runes, not six bytes", "公車", "", false},
		{"three CJK characters pass", "公車站", "公車站", true},
		// The upper bound keeps an attacker from filling the query cache with
		// megabyte-long keys. No vehicle id approaches 64 characters.
		{"over 64 runes is rejected", strings.Repeat("a", 65), "", false},
		{"exactly 64 runes is accepted", strings.Repeat("a", 64), strings.Repeat("a", 64), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := Normalize(tt.in)
			if ok != tt.wantOK {
				t.Fatalf("Normalize(%q) ok = %v, want %v", tt.in, ok, tt.wantOK)
			}
			if got != tt.want {
				t.Errorf("Normalize(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestFilter(t *testing.T) {
	fleet := []obaapi.Vehicle{
		{AgencyID: "1", AgencyName: "Metro", VehicleID: "1_4361"},
		{AgencyID: "1", AgencyName: "Metro", VehicleID: "1_4362"},
		{AgencyID: "3", AgencyName: "CT", VehicleID: "3_ABC123"},
	}

	t.Run("substring match", func(t *testing.T) {
		got := Filter(fleet, "436")
		if len(got) != 2 {
			t.Fatalf("got %d matches, want 2: %+v", len(got), got)
		}
		if got[0].VehicleID != "1_4361" || got[0].ID != "1" || got[0].Name != "Metro" {
			t.Errorf("first match = %+v", got[0])
		}
	})

	// DELIBERATE, DO NOT "FIX". Spec §10 requires lowering the query only and
	// matching against raw ids. True case-insensitivity would match here, and
	// would diverge from every shipped client on fleets with uppercase ids.
	t.Run("lowered query does not match an uppercase fleet id", func(t *testing.T) {
		if got := Filter(fleet, "abc"); len(got) != 0 {
			t.Errorf("Filter(%q) = %+v, want no matches", "abc", got)
		}
	})

	t.Run("uppercase fleet id matches when the raw case is given", func(t *testing.T) {
		// Normalize would have lowered this, which is exactly why such a
		// fleet is unsearchable -- the bug being preserved.
		if got := Filter(fleet, "ABC"); len(got) != 1 {
			t.Errorf("Filter(%q) = %+v, want 1 match", "ABC", got)
		}
	})

	t.Run("no match returns empty, never nil", func(t *testing.T) {
		got := Filter(fleet, "zzz")
		if got == nil {
			t.Fatal("Filter returned nil; it must return an empty slice so the JSON is [] not null")
		}
		if len(got) != 0 {
			t.Errorf("got %d matches, want 0", len(got))
		}
	})

	t.Run("truncates at the cap preserving fleet order", func(t *testing.T) {
		big := make([]obaapi.Vehicle, 0, MaxResults+50)
		for i := 0; i < MaxResults+50; i++ {
			big = append(big, obaapi.Vehicle{AgencyID: "1", AgencyName: "Metro", VehicleID: "1_999"})
		}
		got := Filter(big, "999")
		if len(got) != MaxResults {
			t.Fatalf("got %d matches, want the cap of %d", len(got), MaxResults)
		}
	})
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/vehicles/ -v`
Expected: FAIL — package does not exist.

- [ ] **Step 3: Write the pure logic**

Create `internal/vehicles/vehicles.go`:

```go
// Package vehicles implements the fuzzy vehicle-id search the "find my bus"
// UI calls. The matching rule is a deliberate port of the reference
// implementation's, quirks included; see Filter.
package vehicles

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/OneBusAway/sidecar/internal/cache"
	"github.com/OneBusAway/sidecar/internal/obaapi"
	"github.com/OneBusAway/sidecar/internal/regions"
)

const (
	// MinQueryRunes is spec §10: shorter queries return an empty list without
	// touching the upstream, so a search box firing per keystroke cannot
	// trigger a full-fleet scan on the first character.
	MinQueryRunes = 3

	// MaxQueryRunes bounds the cache key. The query cache is keyed by
	// attacker-controlled input on an unauthenticated endpoint, so capping
	// the entry count alone would still permit 4096 megabyte-long keys. No
	// real vehicle id approaches 64 characters.
	MaxQueryRunes = 64

	// MaxResults caps the response. A three-character query against a large
	// numeric fleet can match thousands of vehicles; this is a deliberate
	// divergence from the reference, which returns everything.
	MaxResults = 250
)

// Match is one search result, shaped as the apps expect it.
type Match struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	VehicleID string `json:"vehicle_id"`
}

// Normalize applies the query rules: trim, lowercase, and bounds-check by
// rune count. It reports false when the query must yield an empty result
// without any upstream call.
//
// Rune count, not byte length: the reference is Rails, whose String#length
// counts characters, and two CJK characters are six bytes.
func Normalize(raw string) (string, bool) {
	q := strings.ToLower(strings.TrimSpace(raw))
	n := utf8.RuneCountInString(q)
	if n < MinQueryRunes || n > MaxQueryRunes {
		return "", false
	}
	return q, true
}

// Filter selects the fleet entries whose id contains q, preserving fleet
// order and truncating at MaxResults.
//
// Only the query has been lowered; the vehicle ids are matched raw. This is
// required by spec §10 and is not an oversight: implementing true
// case-insensitivity would make this server disagree with every shipped
// client on any fleet with uppercase ids.
func Filter(fleet []obaapi.Vehicle, q string) []Match {
	out := make([]Match, 0, 16)
	for _, v := range fleet {
		if !strings.Contains(v.VehicleID, q) {
			continue
		}
		out = append(out, Match{ID: v.AgencyID, Name: v.AgencyName, VehicleID: v.VehicleID})
		if len(out) == MaxResults {
			break
		}
	}
	return out
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/vehicles/ -v`
Expected: PASS.

- [ ] **Step 5: Add the caching service**

Append to `internal/vehicles/vehicles.go`:

```go
// Service answers searches, caching both the region's fleet and each query's
// results. The two caches serve different purposes: the fleet cache stops N
// searches in a region from costing N full-fleet fetches, and the query cache
// stops a search box firing per keystroke from re-filtering the fleet.
type Service struct {
	oba    obaapi.Client
	fleet  *cache.Cache[[]obaapi.Vehicle]
	result *cache.Cache[[]Match]
	logger *slog.Logger
}

// NewService wires a Service. The caches are constructed by the caller so
// their TTLs, caps, and clock stay configuration rather than constants buried
// here.
func NewService(oba obaapi.Client, fleet *cache.Cache[[]obaapi.Vehicle], result *cache.Cache[[]Match], logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{oba: oba, fleet: fleet, result: result, logger: logger}
}

// Search returns the matches for rawQuery in region. A query that fails
// Normalize returns an empty slice and no error, without any upstream call.
func (s *Service) Search(ctx context.Context, region regions.Region, rawQuery string) ([]Match, error) {
	q, ok := Normalize(rawQuery)
	if !ok {
		return []Match{}, nil
	}

	key := strconv.FormatInt(region.ID, 10) + "|" + q
	return s.result.Get(ctx, key, func(ctx context.Context) ([]Match, error) {
		fleet, err := s.fleet.Get(ctx, strconv.FormatInt(region.ID, 10),
			func(ctx context.Context) ([]obaapi.Vehicle, error) {
				return s.oba.Fleet(ctx, region)
			})
		if err != nil {
			return nil, fmt.Errorf("vehicles: fleet for region %d: %w", region.ID, err)
		}
		matches := Filter(fleet, q)
		if len(matches) == MaxResults {
			s.logger.Warn("vehicles: results truncated",
				"region_id", region.ID, "cap", MaxResults)
		}
		return matches, nil
	})
}
```

Add `"log/slog"` to the imports.

- [ ] **Step 6: Write the failing handler tests**

Create `internal/httpapi/vehicles_test.go`. Follow the construction style of the existing `alerts_test.go` for building `Deps` and a test router — read it first and match it.

```go
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/cache"
	"github.com/OneBusAway/sidecar/internal/obaapi"
	"github.com/OneBusAway/sidecar/internal/regions"
	"github.com/OneBusAway/sidecar/internal/vehicles"
)

// fakeOBA is an obaapi.Client that returns a canned fleet or a canned error.
type fakeOBA struct {
	fleet []obaapi.Vehicle
	err   error
	calls int
}

func (f *fakeOBA) Fleet(context.Context, regions.Region) ([]obaapi.Vehicle, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.fleet, nil
}

// newTestRegions opens a migrated store and seeds it with the given regions.
// There is no shared helper for this in the package -- the existing tests each
// call sqlitetest.Open and seed inline -- so this one is defined here and
// reused by both the vehicles and weather handler tests.
func newTestRegions(t *testing.T, regs ...regions.Region) regions.Repository {
	t.Helper()
	store := sqlitetest.Open(t)
	if err := store.Regions().UpsertFromDirectory(context.Background(), regs, testNow); err != nil {
		t.Fatalf("seed regions: %v", err)
	}
	// UpsertFromDirectory deliberately ignores the locally-managed columns, so
	// any region needing an API key gets it through SetLocalFields.
	for _, r := range regs {
		if r.OBAAPIKey == "" {
			continue
		}
		if err := store.Regions().SetLocalFields(context.Background(), r.ID, regions.LocalFields{
			DefaultAgencyID: r.DefaultAgencyID, Timezone: r.Timezone, OBAAPIKey: r.OBAAPIKey,
		}, testNow); err != nil {
			t.Fatalf("set key for region %d: %v", r.ID, err)
		}
	}
	return store.Regions()
}

func newVehiclesTestServer(t *testing.T, oba obaapi.Client, regs regions.Repository) http.Handler {
	t.Helper()
	now := func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }
	svc := vehicles.NewService(oba,
		cache.New[[]obaapi.Vehicle](30*time.Minute, 8, 12*time.Second, now),
		cache.New[[]vehicles.Match](5*time.Minute, 64, 13*time.Second, now),
		slog.New(slog.DiscardHandler),
	)
	return NewRouter(Deps{
		Regions:  regs,
		Vehicles: svc,
		Now:      now,
		Logger:   slog.New(slog.DiscardHandler),
	})
}

func TestVehiclesUnknownRegionIs404(t *testing.T) {
	regs := newTestRegions(t, regions.Region{ID: 1, Name: "R", OBABaseURL: "https://x/", OBAAPIKey: "k", Active: true})
	srv := newVehiclesTestServer(t, &fakeOBA{}, regs)

	for _, seg := range []string{"99", "nope", "99999999999999999999999"} {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/regions/"+seg+"/vehicles?query=abc", nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("segment %q: status = %d, want 404", seg, rec.Code)
		}
		if got := rec.Body.String(); got != notFoundBody {
			t.Errorf("segment %q: body = %q, want %q", seg, got, notFoundBody)
		}
	}
}

func TestVehiclesShortQueryReturnsEmptyArrayWithoutUpstream(t *testing.T) {
	regs := newTestRegions(t, regions.Region{ID: 1, Name: "R", OBABaseURL: "https://x/", OBAAPIKey: "k", Active: true})
	oba := &fakeOBA{fleet: []obaapi.Vehicle{{AgencyID: "1", AgencyName: "M", VehicleID: "1_4361"}}}
	srv := newVehiclesTestServer(t, oba, regs)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/regions/1/vehicles?query=ab", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != "[]\n" && got != "[]" {
		t.Errorf("body = %q, want an empty JSON array", got)
	}
	if oba.calls != 0 {
		t.Errorf("made %d upstream calls, want 0", oba.calls)
	}
}

// A no-match search must serialize as [] and never as null: the schema says
// array, and a client decoding into an array chokes on null.
func TestVehiclesNoMatchIsEmptyArrayNotNull(t *testing.T) {
	regs := newTestRegions(t, regions.Region{ID: 1, Name: "R", OBABaseURL: "https://x/", OBAAPIKey: "k", Active: true})
	srv := newVehiclesTestServer(t, &fakeOBA{fleet: []obaapi.Vehicle{
		{AgencyID: "1", AgencyName: "M", VehicleID: "1_4361"},
	}}, regs)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/regions/1/vehicles?query=zzz", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got == "null\n" || got == "null" {
		t.Fatal("body = null, want []")
	}
	var out []vehicles.Match
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("got %d matches, want 0", len(out))
	}
}

func TestVehiclesSuccessShape(t *testing.T) {
	regs := newTestRegions(t, regions.Region{ID: 1, Name: "R", OBABaseURL: "https://x/", OBAAPIKey: "k", Active: true})
	srv := newVehiclesTestServer(t, &fakeOBA{fleet: []obaapi.Vehicle{
		{AgencyID: "1", AgencyName: "Metro Transit", VehicleID: "1_4361"},
	}}, regs)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/regions/1/vehicles?query=436", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	// The JSON key set is the wire contract. Compare against a literal rather
	// than a golden file; these names come from openapi.yaml's VehicleMatch
	// schema (required: id, name, vehicle_id).
	var raw []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(raw) != 1 {
		t.Fatalf("got %d matches, want 1", len(raw))
	}
	wantKeys := map[string]bool{"id": true, "name": true, "vehicle_id": true}
	for k := range raw[0] {
		if !wantKeys[k] {
			t.Errorf("unexpected JSON key %q", k)
		}
		delete(wantKeys, k)
	}
	for k := range wantKeys {
		t.Errorf("missing JSON key %q", k)
	}
}

func TestVehiclesUpstreamFailureIs502(t *testing.T) {
	regs := newTestRegions(t, regions.Region{ID: 1, Name: "R", OBABaseURL: "https://x/", OBAAPIKey: "k", Active: true})
	srv := newVehiclesTestServer(t, &fakeOBA{err: errors.New("upstream down")}, regs)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/regions/1/vehicles?query=436", nil))

	// 200 [] would be indistinguishable from "no such bus", telling a rider
	// their existing bus does not exist.
	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
}

func TestVehiclesUnconfiguredKeyIs502(t *testing.T) {
	regs := newTestRegions(t, regions.Region{ID: 1, Name: "R", OBABaseURL: "https://x/", Active: true})
	srv := newVehiclesTestServer(t, &fakeOBA{err: obaapi.ErrNotConfigured}, regs)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/regions/1/vehicles?query=436", nil))

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", rec.Code)
	}
}
```

`testNow` and `discardLogger()` already exist in `middleware_test.go`; reuse them rather than defining new ones. Add `"github.com/OneBusAway/sidecar/internal/store/sqlitetest"` to the imports.

- [ ] **Step 7: Run tests to verify they fail**

Run: `go test ./internal/httpapi/ -run TestVehicles -v`
Expected: FAIL — `Deps.Vehicles` undefined, no route registered.

- [ ] **Step 8: Write the handler**

Create `internal/httpapi/vehicles.go`:

```go
package httpapi

import (
	"errors"
	"net/http"

	"github.com/OneBusAway/sidecar/internal/regions"
)

// vehiclesHandler serves the fuzzy vehicle-id search.
type vehiclesHandler struct {
	deps Deps
}

// search serves GET /api/v1/regions/{regionId}/vehicles.
func (h *vehiclesHandler) search(w http.ResponseWriter, r *http.Request) {
	id, parsed := ParseRegionSegment(r.PathValue("regionId"))
	if !parsed {
		h.writeNotFound(w)
		return
	}

	ctx := r.Context()
	region, err := h.deps.Regions.Get(ctx, id)
	if err != nil {
		if errors.Is(err, regions.ErrNotFound) {
			h.writeNotFound(w)
			return
		}
		h.deps.Logger.Error("httpapi: get region", "region_id", id, "err", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	matches, err := h.deps.Vehicles.Search(ctx, region, r.URL.Query().Get("query"))
	if err != nil {
		// 502, not an empty 200: an empty list is indistinguishable from "no
		// such vehicle", so a rider searching for a bus that exists would be
		// told, confidently, that it does not.
		h.deps.Logger.Error("httpapi: vehicle search", "region_id", id, "err", err)
		w.WriteHeader(http.StatusBadGateway)
		return
	}

	writeJSON(w, h.deps.Logger, http.StatusOK, matches)
}

// writeNotFound writes the exact 404 contract for an unrecognised region.
func (h *vehiclesHandler) writeNotFound(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	if _, err := w.Write([]byte(notFoundBody)); err != nil {
		h.deps.Logger.Warn("httpapi: write 404 body", "err", err)
	}
}
```

- [ ] **Step 9: Register the route**

In `internal/httpapi/router.go`, add to `Deps`:

```go
	// Vehicles backs the vehicle search endpoint. Nil means the route is not
	// registered, which is how a feed-only deployment (or a feed-only test)
	// avoids having to supply one.
	Vehicles *vehicles.Service
```

And in `NewRouter`, after the alerts routes:

```go
	if deps.Vehicles != nil {
		vh := &vehiclesHandler{deps: deps}
		mux.HandleFunc("GET /api/v1/regions/{regionId}/vehicles", vh.search)
	}
```

Import `"github.com/OneBusAway/sidecar/internal/vehicles"`.

- [ ] **Step 10: Run tests to verify they pass**

Run: `go test ./internal/httpapi/ -run TestVehicles -race -v`
Expected: PASS, all six tests.

- [ ] **Step 11: Wire it into the binary**

In `cmd/sidecar/main.go`, add the flag beside the existing ones:

```go
	obaAPIKey := fs.String("oba-api-key", envOrDefault("SIDECAR_OBA_API_KEY", ""),
		"default OneBusAway REST API key, used for regions with no key of their own")
```

Add the constants near the existing ones:

```go
	// Cache sizing for the upstream proxies. The TTLs come from the spec
	// (fleet 30 minutes, per-query results 5 minutes); the budgets sit under
	// the server's 15s WriteTimeout, and the query budget exceeds the fleet
	// budget because a cold query fetch nests a fleet fetch inside it.
	fleetTTL       = 30 * time.Minute
	fleetEntries   = 256
	fleetBudget    = 12 * time.Second
	queryTTL       = 5 * time.Minute
	queryEntries   = 4096
	queryBudget    = 13 * time.Second
```

In the dependency-building function, construct the service:

```go
	if *obaAPIKey == "" {
		logger.Warn("no --oba-api-key/SIDECAR_OBA_API_KEY set; " +
			"vehicle search returns 502 for regions with no key of their own")
	}
	obaClient := obaapi.New(*obaAPIKey, http.DefaultClient, logger)
	vehicleSvc := vehicles.NewService(
		obaClient,
		cache.New[[]obaapi.Vehicle](fleetTTL, fleetEntries, fleetBudget, time.Now),
		cache.New[[]vehicles.Match](queryTTL, queryEntries, queryBudget, time.Now),
		logger,
	)
```

and set `Vehicles: vehicleSvc` on the `httpapi.Deps` literal. `time.Now` is legal here — `cmd/` is exempt from the ban, and this is the one place the clock is read and injected downward.

If `cmd/sidecar/main_test.go` has a `TestBuildDeps_*` pattern, add a test asserting `Deps.Vehicles` is non-nil and that the warning fires when the key is absent.

- [ ] **Step 12: Run everything**

Run: `make check`
Expected: PASS.

- [ ] **Step 13: Commit**

```bash
git add internal/vehicles/ internal/httpapi/ cmd/sidecar/
git commit -m "Add the vehicle search endpoint"
```

---

### Task 6: `internal/weather`

Provider interface, Pirate Weather implementation, and the pure mapping to the response shape.

**Files:**
- Create: `internal/weather/weather.go`
- Create: `internal/weather/pirate.go`
- Create: `internal/weather/weather_test.go`
- Create: `internal/weather/testdata/pirate.json`

**Interfaces:**
- Consumes: `regions.LatLon` (Task 1); `cache` (Task 3).
- Produces: `weather.Conditions` (one forecast point) with JSON tags `icon`, `summary`, `temperature`, `temperature_feels_like`, `precip_per_hour`, `precip_probability`, `wind_speed`, `time`; `weather.Snapshot{Units, TodaySummary string; Current Conditions; Hourly []Conditions; RetrievedAt time.Time}`; `weather.Provider` interface with `Fetch(ctx, at regions.LatLon) (Snapshot, error)`; `weather.NewPirateWeather(key string, httpClient *http.Client, now func() time.Time) Provider`.

- [ ] **Step 1: Capture a provider fixture**

Create `internal/weather/testdata/pirate.json` with a trimmed but structurally faithful Pirate Weather response:

```json
{
  "latitude": 47.75,
  "longitude": -122.49,
  "timezone": "America/Los_Angeles",
  "currently": {
    "time": 1767980400,
    "summary": "Light Rain",
    "icon": "rain",
    "precipIntensity": 0.0213,
    "precipProbability": 0.72,
    "temperature": 48.31,
    "apparentTemperature": 44.02,
    "windSpeed": 9.14
  },
  "hourly": {
    "summary": "Rain until evening.",
    "icon": "rain",
    "data": [
      {
        "time": 1767980400,
        "summary": "Light Rain",
        "icon": "rain",
        "precipIntensity": 0.0213,
        "precipProbability": 0.72,
        "temperature": 48.31,
        "apparentTemperature": 44.02,
        "windSpeed": 9.14
      },
      {
        "time": 1767984000,
        "summary": "Cloudy",
        "icon": "some-icon-we-have-never-seen",
        "precipIntensity": 0,
        "precipProbability": 0.1,
        "temperature": 47.02,
        "apparentTemperature": 43.5,
        "windSpeed": 7.2
      }
    ]
  },
  "daily": {
    "summary": "Rain throughout the week.",
    "icon": "rain",
    "data": [
      { "time": 1767945600, "summary": "Rain until evening.", "icon": "rain" },
      { "time": 1768032000, "summary": "Clear throughout the day.", "icon": "clear-day" }
    ]
  },
  "flags": { "units": "us" }
}
```

- [ ] **Step 2: Write the failing tests**

Create `internal/weather/weather_test.go`:

```go
package weather

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/regions"
)

const sentinelKey = "SENTINEL-WEATHER-KEY-do-not-log"

func fixedNow() time.Time { return time.Date(2026, 1, 9, 15, 0, 0, 0, time.UTC) }

func TestPirateWeatherMapping(t *testing.T) {
	body, err := os.ReadFile("testdata/pirate.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	p := newPirateWeatherWithBase(srv.URL, sentinelKey, srv.Client(), fixedNow)
	got, err := p.Fetch(context.Background(), regions.LatLon{Lat: 47.75, Lon: -122.49})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	if !strings.Contains(gotPath, sentinelKey) {
		t.Errorf("request path %q does not carry the key", gotPath)
	}
	if !strings.Contains(gotPath, "47.75,-122.49") {
		t.Errorf("request path %q does not carry the coordinate", gotPath)
	}
	if !strings.Contains(gotQuery, "units=us") {
		t.Errorf("query %q missing units=us", gotQuery)
	}

	if got.Units != "us" {
		t.Errorf("Units = %q, want us", got.Units)
	}
	// today_summary is the DAY's summary (daily.data[0].summary), not the
	// week's (daily.summary). Getting this wrong shows riders next Thursday's
	// weather with today's temperature.
	if got.TodaySummary != "Rain until evening." {
		t.Errorf("TodaySummary = %q, want %q", got.TodaySummary, "Rain until evening.")
	}
	if !got.RetrievedAt.Equal(fixedNow()) {
		t.Errorf("RetrievedAt = %v, want %v", got.RetrievedAt, fixedNow())
	}

	want := Conditions{
		Icon: "rain", Summary: "Light Rain",
		Temperature: 48.31, TemperatureFeelsLike: 44.02,
		PrecipPerHour: 0.0213, PrecipProbability: 0.72,
		WindSpeed: 9.14, Time: 1767980400,
	}
	if got.Current != want {
		t.Errorf("Current = %+v, want %+v", got.Current, want)
	}

	if len(got.Hourly) != 2 {
		t.Fatalf("got %d hourly entries, want 2", len(got.Hourly))
	}
	if got.Hourly[0] != want {
		t.Errorf("Hourly[0] = %+v, want %+v", got.Hourly[0], want)
	}
	// An icon we have never seen must pass through untouched: the vocabulary
	// belongs to the provider, and mapping it to a fallback would hide a new
	// condition from riders.
	if got.Hourly[1].Icon != "some-icon-we-have-never-seen" {
		t.Errorf("Hourly[1].Icon = %q, want the raw provider value", got.Hourly[1].Icon)
	}
}

func TestPirateWeatherNon200IsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	p := newPirateWeatherWithBase(srv.URL, sentinelKey, srv.Client(), fixedNow)
	if _, err := p.Fetch(context.Background(), regions.LatLon{Lat: 1, Lon: 2}); err == nil {
		t.Fatal("Fetch succeeded on a 429, want an error")
	}
}

// The key is a path segment, and *url.Error embeds the whole URL. An error
// returned verbatim writes the secret wherever the caller logs it.
func TestPirateWeatherErrorsDoNotLeakTheKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Close()

	p := newPirateWeatherWithBase(srv.URL, sentinelKey, srv.Client(), fixedNow)
	_, err := p.Fetch(context.Background(), regions.LatLon{Lat: 1, Lon: 2})
	if err == nil {
		t.Fatal("Fetch succeeded against a closed server, want an error")
	}
	if strings.Contains(err.Error(), sentinelKey) {
		t.Errorf("error leaks the key: %v", err)
	}
}
```

- [ ] **Step 3: Run tests to verify they fail**

Run: `go test ./internal/weather/ -v`
Expected: FAIL — package does not exist.

- [ ] **Step 4: Write the types**

Create `internal/weather/weather.go`:

```go
// Package weather proxies a weather provider into the normalized shape the
// apps render on the stop screen. The response vocabulary is Dark Sky's,
// which the apps were built against; provider values pass through rather than
// being remapped.
package weather

import (
	"context"
	"time"

	"github.com/OneBusAway/sidecar/internal/regions"
)

// Conditions is one forecast point, current or hourly.
type Conditions struct {
	Icon                 string  `json:"icon"`
	Summary              string  `json:"summary"`
	Temperature          float64 `json:"temperature"`
	TemperatureFeelsLike float64 `json:"temperature_feels_like"`
	PrecipPerHour        float64 `json:"precip_per_hour"`
	PrecipProbability    float64 `json:"precip_probability"`
	WindSpeed            float64 `json:"wind_speed"`
	Time                 int64   `json:"time"`
}

// Snapshot is everything a provider supplies for one coordinate. It carries
// no region identity: the region-specific envelope is assembled per request,
// so a renamed region is correct immediately and two regions sharing a
// centroid share one upstream call.
type Snapshot struct {
	Units        string
	TodaySummary string
	Current      Conditions
	Hourly       []Conditions

	// RetrievedAt is stamped when the provider call returned, and travels
	// with the cached value rather than being recomputed on a cache hit --
	// its whole purpose is to let a client see the data is 29 minutes old.
	RetrievedAt time.Time
}

// Provider fetches current conditions for a coordinate.
type Provider interface {
	Fetch(ctx context.Context, at regions.LatLon) (Snapshot, error)
}
```

- [ ] **Step 5: Write the Pirate Weather implementation**

Create `internal/weather/pirate.go`:

```go
package weather

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/OneBusAway/sidecar/internal/regions"
)

// pirateBaseURL is the production host. Tests substitute an httptest server
// through newPirateWeatherWithBase.
const pirateBaseURL = "https://api.pirateweather.net"

// maxBody caps the provider response. It is an untrusted upstream and the
// read happens before anything has been validated.
const maxBody = 4 << 20

type pirateWeather struct {
	base string
	key  string
	http *http.Client
	now  func() time.Time
}

// NewPirateWeather builds a Provider backed by Pirate Weather, a Dark
// Sky-compatible API.
func NewPirateWeather(key string, httpClient *http.Client, now func() time.Time) Provider {
	return newPirateWeatherWithBase(pirateBaseURL, key, httpClient, now)
}

func newPirateWeatherWithBase(base, key string, httpClient *http.Client, now func() time.Time) *pirateWeather {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &pirateWeather{base: base, key: key, http: httpClient, now: now}
}

// pirateResponse is the subset of the provider's payload this sidecar uses.
type pirateResponse struct {
	Currently piratePoint `json:"currently"`
	Hourly    struct {
		Data []piratePoint `json:"data"`
	} `json:"hourly"`
	Daily struct {
		Data []struct {
			Summary string `json:"summary"`
		} `json:"data"`
	} `json:"daily"`
	Flags struct {
		Units string `json:"units"`
	} `json:"flags"`
}

type piratePoint struct {
	Time                int64   `json:"time"`
	Summary             string  `json:"summary"`
	Icon                string  `json:"icon"`
	PrecipIntensity     float64 `json:"precipIntensity"`
	PrecipProbability   float64 `json:"precipProbability"`
	Temperature         float64 `json:"temperature"`
	ApparentTemperature float64 `json:"apparentTemperature"`
	WindSpeed           float64 `json:"windSpeed"`
}

func (p piratePoint) conditions() Conditions {
	return Conditions{
		Icon:                 p.Icon,
		Summary:              p.Summary,
		Temperature:          p.Temperature,
		TemperatureFeelsLike: p.ApparentTemperature,
		PrecipPerHour:        p.PrecipIntensity,
		PrecipProbability:    p.PrecipProbability,
		WindSpeed:            p.WindSpeed,
		Time:                 p.Time,
	}
}

// requestedUnits is echoed into the response so the units field always
// describes the numbers it accompanies.
const requestedUnits = "us"

func (w *pirateWeather) Fetch(ctx context.Context, at regions.LatLon) (Snapshot, error) {
	coord := strconv.FormatFloat(at.Lat, 'f', -1, 64) + "," + strconv.FormatFloat(at.Lon, 'f', -1, 64)
	endpoint := w.base + "/forecast/" + url.PathEscape(w.key) + "/" + coord +
		"?units=" + requestedUnits + "&exclude=minutely,alerts"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return Snapshot{}, errors.New("weather: build request failed")
	}

	resp, err := w.http.Do(req)
	if err != nil {
		return Snapshot{}, redact(err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		// The status, never the URL: the key is a path segment.
		return Snapshot{}, fmt.Errorf("weather: provider returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return Snapshot{}, errors.New("weather: reading provider response failed")
	}

	var out pirateResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return Snapshot{}, errors.New("weather: decoding provider response failed")
	}

	snap := Snapshot{
		Units:       requestedUnits,
		Current:     out.Currently.conditions(),
		Hourly:      make([]Conditions, 0, len(out.Hourly.Data)),
		RetrievedAt: w.now(),
	}
	// The DAY's summary, not the week's: daily.summary describes the whole
	// forecast period, which is not what the stop screen shows.
	if len(out.Daily.Data) > 0 {
		snap.TodaySummary = out.Daily.Data[0].Summary
	}
	for _, h := range out.Hourly.Data {
		snap.Hourly = append(snap.Hourly, h.conditions())
	}
	return snap, nil
}

// redact strips the URL from a transport error. The API key is a path
// segment, and *url.Error embeds the full URL in its message.
func redact(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return fmt.Errorf("weather: %s request failed: %w", urlErr.Op, urlErr.Err)
	}
	return errors.New("weather: provider request failed")
}
```

- [ ] **Step 6: Run tests to verify they pass**

Run: `go test ./internal/weather/ -v`
Expected: PASS, all three tests.

- [ ] **Step 7: Commit**

```bash
make check
git add internal/weather/
git commit -m "Add the Pirate Weather provider and forecast normalization"
```

---

### Task 7: Weather endpoint

**Files:**
- Create: `internal/httpapi/weather.go`
- Create: `internal/httpapi/weather_test.go`
- Modify: `internal/httpapi/router.go`
- Modify: `cmd/sidecar/main.go`

**Interfaces:**
- Consumes: `weather.Provider`, `weather.Snapshot`, `weather.Conditions` (Task 6); `cache` (Task 3); `regions.Region.Centroid` (Tasks 1-2).
- Produces: `httpapi.Deps.Weather *weather.Service`; `weather.NewService(p Provider, c *cache.Cache[Snapshot], logger *slog.Logger) *Service` with `Snapshot(ctx context.Context, at regions.LatLon) (Snapshot, error)`.

- [ ] **Step 1: Add the caching service**

Append to `internal/weather/weather.go`:

```go
// Service caches provider snapshots by coordinate. Two regions sharing a
// centroid therefore share one upstream call, and a renamed region needs no
// cache invalidation because the cached value holds no region identity.
type Service struct {
	provider Provider
	cache    *cache.Cache[Snapshot]
	logger   *slog.Logger
}

// NewService wires a Service. A nil provider means no key was configured, and
// Snapshot then reports ErrNoProvider without any network call.
func NewService(provider Provider, c *cache.Cache[Snapshot], logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{provider: provider, cache: c, logger: logger}
}

// ErrNoProvider means the process has no weather provider key configured.
var ErrNoProvider = errors.New("weather: no provider configured")

// Snapshot returns cached conditions for a coordinate, fetching on a miss.
func (s *Service) Snapshot(ctx context.Context, at regions.LatLon) (Snapshot, error) {
	if s.provider == nil {
		return Snapshot{}, ErrNoProvider
	}
	// Four decimals is roughly 11 metres -- far finer than any weather
	// gradient, and enough that two regions with the same centroid share a
	// cache entry.
	key := strconv.FormatFloat(at.Lat, 'f', 4, 64) + "," + strconv.FormatFloat(at.Lon, 'f', 4, 64)
	return s.cache.Get(ctx, key, func(ctx context.Context) (Snapshot, error) {
		return s.provider.Fetch(ctx, at)
	})
}
```

Add `"errors"`, `"log/slog"`, `"strconv"`, and `"github.com/OneBusAway/sidecar/internal/cache"` to that file's imports.

- [ ] **Step 2: Write the failing handler tests**

Create `internal/httpapi/weather_test.go`:

```go
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/cache"
	"github.com/OneBusAway/sidecar/internal/regions"
	"github.com/OneBusAway/sidecar/internal/weather"
)

type fakeProvider struct {
	snap  weather.Snapshot
	err   error
	calls int
}

func (f *fakeProvider) Fetch(context.Context, regions.LatLon) (weather.Snapshot, error) {
	f.calls++
	if f.err != nil {
		return weather.Snapshot{}, f.err
	}
	return f.snap, nil
}

func sampleSnapshot(at time.Time) weather.Snapshot {
	return weather.Snapshot{
		Units:        "us",
		TodaySummary: "Rain until evening.",
		Current: weather.Conditions{
			Icon: "rain", Summary: "Light Rain", Temperature: 48.31,
			TemperatureFeelsLike: 44.02, PrecipPerHour: 0.0213,
			PrecipProbability: 0.72, WindSpeed: 9.14, Time: 1767980400,
		},
		Hourly:      []weather.Conditions{{Icon: "rain", Time: 1767980400}},
		RetrievedAt: at,
	}
}

func newWeatherTestServer(t *testing.T, p weather.Provider, regs regions.Repository) http.Handler {
	t.Helper()
	now := func() time.Time { return time.Date(2026, 1, 9, 15, 0, 0, 0, time.UTC) }
	svc := weather.NewService(p, cache.New[weather.Snapshot](30*time.Minute, 8, 5*time.Second, now), slog.New(slog.DiscardHandler))
	return NewRouter(Deps{
		Regions: regs,
		Weather: svc,
		Now:     now,
		Logger:  slog.New(slog.DiscardHandler),
	})
}

func weatherRegion() regions.Region {
	return regions.Region{
		ID: 1, Name: "Puget Sound", OBABaseURL: "https://x/", Active: true,
		Centroid: &regions.LatLon{Lat: 47.75, Lon: -122.49},
	}
}

func TestWeatherUnknownRegionIs404(t *testing.T) {
	regs := newTestRegions(t, weatherRegion())
	srv := newWeatherTestServer(t, &fakeProvider{}, regs)

	for _, seg := range []string{"99", "nope", "99999999999999999999999"} {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/regions/"+seg+"/weather", nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("segment %q: status = %d, want 404", seg, rec.Code)
		}
	}
}

// 403, not 404: telling the app the region does not exist is a different and
// false claim, and shipped apps read any non-200 as "hide the weather UI".
func TestWeatherNilCentroidIs403(t *testing.T) {
	regs := newTestRegions(t, regions.Region{ID: 1, Name: "Unsynced", OBABaseURL: "https://x/", Active: true})
	p := &fakeProvider{snap: sampleSnapshot(time.Now())}
	srv := newWeatherTestServer(t, p, regs)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/regions/1/weather", nil))

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
	if p.calls != 0 {
		t.Errorf("made %d provider calls, want 0", p.calls)
	}
}

func TestWeatherProviderErrorIs403(t *testing.T) {
	regs := newTestRegions(t, weatherRegion())
	srv := newWeatherTestServer(t, &fakeProvider{err: errors.New("upstream down")}, regs)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/regions/1/weather", nil))

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (never 5xx: apps are tested against 403)", rec.Code)
	}
}

func TestWeatherSuccessShape(t *testing.T) {
	fetchedAt := time.Date(2026, 1, 9, 14, 31, 0, 0, time.UTC)
	regs := newTestRegions(t, weatherRegion())
	srv := newWeatherTestServer(t, &fakeProvider{snap: sampleSnapshot(fetchedAt)}, regs)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/regions/1/weather", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Key set from openapi.yaml's WeatherForecast schema. A literal rather
	// than a golden file: a golden diff reads as something to accept, whereas
	// a missing key here reads as a broken contract.
	want := []string{
		"latitude", "longitude", "region_identifier", "region_name",
		"retrieved_at", "units", "today_summary", "current_forecast", "hourly_forecast",
	}
	for _, k := range want {
		if _, ok := raw[k]; !ok {
			t.Errorf("missing JSON key %q", k)
		}
	}
	if len(raw) != len(want) {
		t.Errorf("got %d keys, want %d: %v", len(raw), len(want), raw)
	}

	if raw["region_identifier"] != float64(1) {
		t.Errorf("region_identifier = %v, want 1", raw["region_identifier"])
	}
	if raw["region_name"] != "Puget Sound" {
		t.Errorf("region_name = %v, want Puget Sound", raw["region_name"])
	}
	if raw["latitude"] != 47.75 {
		t.Errorf("latitude = %v, want 47.75", raw["latitude"])
	}

	// The OpenAPI schema says string/date-time. An epoch integer would pass
	// every other assertion here and violate the contract.
	got, ok := raw["retrieved_at"].(string)
	if !ok {
		t.Fatalf("retrieved_at = %T, want an RFC 3339 string", raw["retrieved_at"])
	}
	if _, err := time.Parse(time.RFC3339, got); err != nil {
		t.Errorf("retrieved_at %q is not RFC 3339: %v", got, err)
	}
}

// retrieved_at describes when the data was fetched, not when it was served.
// Recomputing it on a cache hit would tell a client 29-minute-old data is
// fresh.
func TestWeatherRetrievedAtIsStableAcrossCacheHits(t *testing.T) {
	fetchedAt := time.Date(2026, 1, 9, 14, 31, 0, 0, time.UTC)
	regs := newTestRegions(t, weatherRegion())
	p := &fakeProvider{snap: sampleSnapshot(fetchedAt)}
	srv := newWeatherTestServer(t, p, regs)

	read := func() string {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/regions/1/weather", nil))
		var raw map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
			t.Fatalf("decode: %v", err)
		}
		s, _ := raw["retrieved_at"].(string)
		return s
	}

	first, second := read(), read()
	if first != second {
		t.Errorf("retrieved_at changed across a cache hit: %q then %q", first, second)
	}
	if p.calls != 1 {
		t.Errorf("made %d provider calls, want 1", p.calls)
	}
}
```

- [ ] **Step 3: Run tests to verify they fail**

Run: `go test ./internal/httpapi/ -run TestWeather -v`
Expected: FAIL — `Deps.Weather` undefined.

- [ ] **Step 4: Write the handler**

Create `internal/httpapi/weather.go`:

```go
package httpapi

import (
	"errors"
	"net/http"
	"time"

	"github.com/OneBusAway/sidecar/internal/regions"
	"github.com/OneBusAway/sidecar/internal/weather"
)

// forecastJSON is the wire shape from openapi.yaml's WeatherForecast schema.
// The region fields are assembled per request rather than cached, so a
// renamed region is correct immediately.
type forecastJSON struct {
	Latitude         float64              `json:"latitude"`
	Longitude        float64              `json:"longitude"`
	RegionIdentifier int64                `json:"region_identifier"`
	RegionName       string               `json:"region_name"`
	RetrievedAt      string               `json:"retrieved_at"`
	Units            string               `json:"units"`
	TodaySummary     string               `json:"today_summary"`
	CurrentForecast  weather.Conditions   `json:"current_forecast"`
	HourlyForecast   []weather.Conditions `json:"hourly_forecast"`
}

// weatherHandler serves the regional forecast.
type weatherHandler struct {
	deps Deps
}

// forecast serves GET /api/v1/regions/{regionId}/weather.
//
// Every failure that is not an unknown region is a 403, per spec §9: shipped
// apps treat any non-200 as "hide the weather UI", and 403 is the code they
// have been tested against. A 404 would tell the app the region does not
// exist, which is a different and false claim.
func (h *weatherHandler) forecast(w http.ResponseWriter, r *http.Request) {
	id, parsed := ParseRegionSegment(r.PathValue("regionId"))
	if !parsed {
		h.writeNotFound(w)
		return
	}

	ctx := r.Context()
	region, err := h.deps.Regions.Get(ctx, id)
	if err != nil {
		if errors.Is(err, regions.ErrNotFound) {
			h.writeNotFound(w)
			return
		}
		h.deps.Logger.Error("httpapi: get region", "region_id", id, "err", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	if region.Centroid == nil {
		h.deps.Logger.Error("httpapi: weather unavailable, region has no centroid", "region_id", id)
		h.writeUnavailable(w)
		return
	}

	snap, err := h.deps.Weather.Snapshot(ctx, *region.Centroid)
	if err != nil {
		h.deps.Logger.Error("httpapi: weather fetch", "region_id", id, "err", err)
		h.writeUnavailable(w)
		return
	}

	writeJSON(w, h.deps.Logger, http.StatusOK, forecastJSON{
		Latitude:         region.Centroid.Lat,
		Longitude:        region.Centroid.Lon,
		RegionIdentifier: region.ID,
		RegionName:       region.Name,
		RetrievedAt:      snap.RetrievedAt.UTC().Format(time.RFC3339),
		Units:            snap.Units,
		TodaySummary:     snap.TodaySummary,
		CurrentForecast:  snap.Current,
		HourlyForecast:   snap.Hourly,
	})
}

// writeUnavailable is the 403 contract. The body is ignored by clients; an
// empty object is valid JSON for one that decodes before checking status.
func (h *weatherHandler) writeUnavailable(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	if _, err := w.Write([]byte("{}")); err != nil {
		h.deps.Logger.Warn("httpapi: write 403 body", "err", err)
	}
}

func (h *weatherHandler) writeNotFound(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	if _, err := w.Write([]byte(notFoundBody)); err != nil {
		h.deps.Logger.Warn("httpapi: write 404 body", "err", err)
	}
}
```

- [ ] **Step 5: Register the route**

In `internal/httpapi/router.go`, add to `Deps`:

```go
	// Weather backs the forecast endpoint. Nil means the route is not
	// registered.
	Weather *weather.Service
```

And in `NewRouter`:

```go
	if deps.Weather != nil {
		wh := &weatherHandler{deps: deps}
		mux.HandleFunc("GET /api/v1/regions/{regionId}/weather", wh.forecast)
	}
```

- [ ] **Step 6: Run tests to verify they pass**

Run: `go test ./internal/httpapi/ -run TestWeather -race -v`
Expected: PASS, all five tests.

- [ ] **Step 7: Wire it into the binary**

In `cmd/sidecar/main.go`:

```go
	pirateKey := fs.String("pirate-weather-key", envOrDefault("SIDECAR_PIRATE_WEATHER_KEY", ""),
		"Pirate Weather API key; without it the weather endpoint returns 403")
```

Constants:

```go
	weatherTTL     = 30 * time.Minute
	weatherEntries = 256
	weatherBudget  = 5 * time.Second
```

Construction — a nil provider is the "not configured" signal, which `weather.Service` turns into `ErrNoProvider` and the handler into a `403`:

```go
	var provider weather.Provider
	if *pirateKey == "" {
		logger.Warn("no --pirate-weather-key/SIDECAR_PIRATE_WEATHER_KEY set; the weather endpoint returns 403")
	} else {
		provider = weather.NewPirateWeather(*pirateKey, http.DefaultClient, time.Now)
	}
	weatherSvc := weather.NewService(provider,
		cache.New[weather.Snapshot](weatherTTL, weatherEntries, weatherBudget, time.Now), logger)
```

and set `Weather: weatherSvc` on the `httpapi.Deps` literal.

- [ ] **Step 8: Run everything**

Run: `make check`
Expected: PASS.

- [ ] **Step 9: Commit**

```bash
git add internal/httpapi/ internal/weather/ cmd/sidecar/
git commit -m "Add the regional weather endpoint"
```

---

### Task 8: Admin surfaces for the API key and centroid

**Files:**
- Modify: `cmd/sidecar-admin/commands.go`
- Modify: `cmd/sidecar-admin/commands_test.go`
- Modify: `internal/httpapi/admin_regions.go`
- Modify: `internal/httpapi/admin_regions_test.go`

**Interfaces:**
- Consumes: `regions.LocalFields`, `regions.Region.OBAAPIKey`, `regions.Region.Centroid` (Tasks 1-2).
- Produces: `regionJSON` gains `latitude *float64`, `longitude *float64`, `oba_api_key string` (a status word, never the key); `patchRegionRequest` gains `OBAAPIKey *string`.

- [ ] **Step 1: Write the failing admin API tests**

These use the existing `adminFixture` from `admin_alerts_test.go` (`newAdminFixture`, `f.do`, `array`, `str`, `assertKeys`, and the `regionTampa`/`regionPuget`/`regionBare` ids).

First extend the existing key-set list at the top of `admin_regions_test.go`, since `assertKeys` asserts an exact set:

```go
var regionJSONFields = []string{
	"id", "name", "oba_base_url", "sidecar_base_url", "language", "active",
	"default_agency_id", "timezone", "latitude", "longitude", "oba_api_key",
}
```

Then append:

```go
// The key must never leave the server. Asserting against the raw response
// bytes rather than a decoded struct means a field added later without a tag
// change still fails this test.
func TestAdminRegions_NeverEchoesTheKey(t *testing.T) {
	t.Parallel()

	const secret = "SENTINEL-OBA-KEY-do-not-echo"
	f := newAdminFixture(t)
	if err := f.store.Regions().SetLocalFields(context.Background(), regionPuget, regions.LocalFields{
		DefaultAgencyID: "1", Timezone: "America/Los_Angeles", OBAAPIKey: secret,
	}, testNow); err != nil {
		t.Fatalf("set key: %v", err)
	}

	rec := f.do(http.MethodGet, "/api/admin/v1/regions", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if bytes.Contains(rec.Body.Bytes(), []byte(secret)) {
		t.Fatalf("region listing leaks the API key: %s", rec.Body.String())
	}
}

// A plain boolean would report false for a region whose vehicle search works
// perfectly via the process default -- the reading an operator would act on
// wrongly. Three states, three distinguishable words.
func TestAdminRegions_KeyStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		regionKey     string
		defaultKeySet bool
		want          string
	}{
		{"region carries its own", "abc", false, "region"},
		{"region carries its own even with a default", "abc", true, "region"},
		{"inherits the process default", "", true, "default"},
		{"nothing configured anywhere", "", false, "none"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			store := sqlitetest.Open(t)
			ctx := context.Background()
			if err := store.Regions().UpsertFromDirectory(ctx, []regions.Region{
				{ID: regionPuget, Name: "Puget Sound", OBABaseURL: "https://puget.example/", Active: true},
			}, testNow); err != nil {
				t.Fatalf("seed: %v", err)
			}
			if err := store.Regions().SetLocalFields(ctx, regionPuget, regions.LocalFields{
				DefaultAgencyID: "1", Timezone: "America/Los_Angeles", OBAAPIKey: tt.regionKey,
			}, testNow); err != nil {
				t.Fatalf("set key: %v", err)
			}
			if _, err := store.Auth().CreateUser(ctx, "admin", testHash(), testNow); err != nil {
				t.Fatalf("create user: %v", err)
			}

			handler := NewRouter(Deps{
				Alerts: store.Alerts(), Regions: store.Regions(), Auth: store.Auth(),
				Now: func() time.Time { return testNow }, Logger: discardLogger(),
				Sleep: func(time.Duration) {}, OBADefaultKeySet: tt.defaultKeySet,
			})
			f := &adminFixture{handler: handler, store: store, cookie: adminLogin(t, handler)}

			got := array(t, f.do(http.MethodGet, "/api/admin/v1/regions", ""), http.StatusOK)
			region := regionByID(t, got, regionPuget)
			if v := str(t, region, "oba_api_key"); v != tt.want {
				t.Errorf("oba_api_key = %q, want %q", v, tt.want)
			}
		})
	}
}

// 0,0 is a real coordinate in the Gulf of Guinea, so an unsynced region must
// serialize as null rather than as a point off the coast of Africa.
func TestAdminRegions_CentroidIsNullWhenUnsynced(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	if err := f.store.Regions().UpsertFromDirectory(context.Background(), []regions.Region{
		{ID: regionPuget, Name: "Puget Sound", OBABaseURL: "https://puget.example/", Active: true,
			Centroid: &regions.LatLon{Lat: 47.75, Lon: -122.49}},
	}, testNow); err != nil {
		t.Fatalf("seed centroid: %v", err)
	}

	got := array(t, f.do(http.MethodGet, "/api/admin/v1/regions", ""), http.StatusOK)

	bare := regionByID(t, got, regionBare)
	if bare["latitude"] != nil {
		t.Errorf("unsynced latitude = %v, want null", bare["latitude"])
	}
	if bare["longitude"] != nil {
		t.Errorf("unsynced longitude = %v, want null", bare["longitude"])
	}

	puget := regionByID(t, got, regionPuget)
	if puget["latitude"] != 47.75 {
		t.Errorf("latitude = %v, want 47.75", puget["latitude"])
	}
	if puget["longitude"] != -122.49 {
		t.Errorf("longitude = %v, want -122.49", puget["longitude"])
	}
}

// Omission means unchanged. Anything else silently wipes the key on every
// unrelated edit an operator makes.
func TestAdminRegions_PatchOmittedKeyLeavesItIntact(t *testing.T) {
	t.Parallel()

	const secret = "keep-me"
	f := newAdminFixture(t)
	ctx := context.Background()
	if err := f.store.Regions().SetLocalFields(ctx, regionPuget, regions.LocalFields{
		DefaultAgencyID: "1", Timezone: "America/Los_Angeles", OBAAPIKey: secret,
	}, testNow); err != nil {
		t.Fatalf("set key: %v", err)
	}

	rec := f.do(http.MethodPatch, "/api/admin/v1/regions/1", `{"default_agency_id":"99"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	got, err := f.store.Regions().Get(ctx, regionPuget)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.OBAAPIKey != secret {
		t.Errorf("OBAAPIKey = %q, want %q -- an unrelated PATCH wiped the key", got.OBAAPIKey, secret)
	}
	if got.DefaultAgencyID != "99" {
		t.Errorf("DefaultAgencyID = %q, want 99", got.DefaultAgencyID)
	}
}

func TestAdminRegions_PatchEmptyKeyClearsIt(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	ctx := context.Background()
	if err := f.store.Regions().SetLocalFields(ctx, regionPuget, regions.LocalFields{
		DefaultAgencyID: "1", Timezone: "America/Los_Angeles", OBAAPIKey: "clear-me",
	}, testNow); err != nil {
		t.Fatalf("set key: %v", err)
	}

	rec := f.do(http.MethodPatch, "/api/admin/v1/regions/1", `{"oba_api_key":""}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	got, err := f.store.Regions().Get(ctx, regionPuget)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.OBAAPIKey != "" {
		t.Errorf("OBAAPIKey = %q, want empty", got.OBAAPIKey)
	}
}

// The guard's message is the kind that goes stale for a year. Pin it.
func TestAdminRegions_PatchEmptyBodyNamesAllThreeFields(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	rec := f.do(http.MethodPatch, "/api/admin/v1/regions/1", `{}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "oba_api_key") {
		t.Errorf("error message does not mention oba_api_key: %s", rec.Body.String())
	}
}
```

Add `"bytes"` and `"strings"` to the file's imports, plus `"github.com/OneBusAway/sidecar/internal/store/sqlitetest"` if it is not already there.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/httpapi/ -run 'AdminRegions|PatchRegion' -v`
Expected: FAIL.

- [ ] **Step 3: Update the admin handler**

In `internal/httpapi/admin_regions.go`:

```go
// Key status words for regionJSON.OBAAPIKey. The key itself is never sent:
// a value echoed into a JSON response lands in the SPA's memory, the
// browser's devtools, and any HAR file attached to a bug report, and no admin
// workflow needs to read one back.
const (
	keyStatusRegion  = "region"  // this region carries its own key
	keyStatusDefault = "default" // empty column, process default present
	keyStatusNone    = "none"    // nothing configured; calls will fail
)

type regionJSON struct {
	ID              int64    `json:"id"`
	Name            string   `json:"name"`
	OBABaseURL      string   `json:"oba_base_url"`
	SidecarBaseURL  string   `json:"sidecar_base_url"`
	Language        string   `json:"language"`
	Active          bool     `json:"active"`
	DefaultAgencyID string   `json:"default_agency_id"`
	Timezone        string   `json:"timezone"`
	Latitude        *float64 `json:"latitude"`
	Longitude       *float64 `json:"longitude"`
	OBAAPIKey       string   `json:"oba_api_key"`
}

type patchRegionRequest struct {
	DefaultAgencyID *string `json:"default_agency_id"`
	Timezone        *string `json:"timezone"`
	// OBAAPIKey is write-only. A nil pointer means unchanged; a pointer to
	// "" clears the key and restores the process-default fallback.
	OBAAPIKey *string `json:"oba_api_key"`
}
```

`toRegionJSON` becomes a method or takes the default-key flag:

```go
func toRegionJSON(reg regions.Region, defaultKeySet bool) regionJSON {
	out := regionJSON{
		ID:              reg.ID,
		Name:            reg.Name,
		OBABaseURL:      reg.OBABaseURL,
		SidecarBaseURL:  reg.SidecarBaseURL,
		Language:        reg.Language,
		Active:          reg.Active,
		DefaultAgencyID: reg.DefaultAgencyID,
		Timezone:        reg.Timezone,
		OBAAPIKey:       keyStatusNone,
	}
	if reg.Centroid != nil {
		lat, lon := reg.Centroid.Lat, reg.Centroid.Lon
		out.Latitude, out.Longitude = &lat, &lon
	}
	switch {
	case reg.OBAAPIKey != "":
		out.OBAAPIKey = keyStatusRegion
	case defaultKeySet:
		out.OBAAPIKey = keyStatusDefault
	}
	return out
}
```

Add `OBADefaultKeySet bool` to `httpapi.Deps` (set from `*obaAPIKey != ""` in `cmd/sidecar/main.go`) and pass `h.deps.OBADefaultKeySet` at both call sites.

In `patch`, extend the guard and the merge. Update the error string too — it is the kind of message that goes stale for a year:

```go
	if req.DefaultAgencyID == nil && req.Timezone == nil && req.OBAAPIKey == nil {
		writeJSONError(w, h.deps.Logger, http.StatusBadRequest,
			"provide default_agency_id, timezone, and/or oba_api_key")
		return
	}
```

and in the merge, `newKey := current.OBAAPIKey; if req.OBAAPIKey != nil { newKey = *req.OBAAPIKey }`, passing it in `regions.LocalFields`.

- [ ] **Step 4: Run the admin API tests**

Run: `go test ./internal/httpapi/ -run 'AdminRegions|PatchRegion' -v`
Expected: PASS.

- [ ] **Step 5: Write the failing CLI tests**

Append to `cmd/sidecar-admin/commands_test.go`, using the file's existing `cli(t, dbPath, …)` helper:

```go
// The CLI prints a status word, never the key: `region list` output routinely
// ends up pasted into issues and chat.
func TestRegionSet_OBAAPIKeyIsNeverPrinted(t *testing.T) {
	t.Parallel()
	dbPath, store := newDB(t)
	seedRegion(t, store.Regions(), 1)

	const secret = "SENTINEL-CLI-KEY"
	if _, _, err := cli(t, dbPath, "region", "set", "--id", "1", "--oba-api-key", secret); err != nil {
		t.Fatalf("region set: %v", err)
	}

	stdout, _, err := cli(t, dbPath, "region", "list")
	if err != nil {
		t.Fatalf("region list: %v", err)
	}
	if strings.Contains(stdout, secret) {
		t.Fatalf("region list printed the key: %q", stdout)
	}
	if !strings.Contains(stdout, "oba-key=configured") {
		t.Errorf("region list = %q, want it to report the key as configured", stdout)
	}

	got, err := store.Regions().Get(context.Background(), 1)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.OBAAPIKey != secret {
		t.Errorf("stored OBAAPIKey = %q, want %q", got.OBAAPIKey, secret)
	}
}

// An explicit empty value clears the key and restores the process-default
// fallback. This is why the flag is read through visitedFlags rather than by
// testing for the empty string.
func TestRegionSet_EmptyOBAAPIKeyClears(t *testing.T) {
	t.Parallel()
	dbPath, store := newDB(t)
	seedRegion(t, store.Regions(), 1)

	if _, _, err := cli(t, dbPath, "region", "set", "--id", "1", "--oba-api-key", "clear-me"); err != nil {
		t.Fatalf("region set: %v", err)
	}
	if _, _, err := cli(t, dbPath, "region", "set", "--id", "1", "--oba-api-key", ""); err != nil {
		t.Fatalf("region set (clear): %v", err)
	}

	got, err := store.Regions().Get(context.Background(), 1)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.OBAAPIKey != "" {
		t.Errorf("OBAAPIKey = %q, want empty after an explicit clear", got.OBAAPIKey)
	}

	stdout, _, err := cli(t, dbPath, "region", "list")
	if err != nil {
		t.Fatalf("region list: %v", err)
	}
	if !strings.Contains(stdout, "oba-key=not configured") {
		t.Errorf("region list = %q, want it to report the key as not configured", stdout)
	}
}

// An omitted flag leaves the key alone. Without this, setting an agency id
// silently destroys the region's vehicle search.
func TestRegionSet_PreservesKeyWhenFlagOmitted(t *testing.T) {
	t.Parallel()
	dbPath, store := newDB(t)
	seedRegion(t, store.Regions(), 1)

	const secret = "keep-me"
	if _, _, err := cli(t, dbPath, "region", "set", "--id", "1", "--oba-api-key", secret); err != nil {
		t.Fatalf("region set: %v", err)
	}
	if _, _, err := cli(t, dbPath, "region", "set", "--id", "1", "--agency-id", "40"); err != nil {
		t.Fatalf("region set (agency): %v", err)
	}

	got, err := store.Regions().Get(context.Background(), 1)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.OBAAPIKey != secret {
		t.Errorf("OBAAPIKey = %q, want %q -- an unrelated edit wiped the key", got.OBAAPIKey, secret)
	}
	if got.DefaultAgencyID != "40" {
		t.Errorf("DefaultAgencyID = %q, want 40", got.DefaultAgencyID)
	}
}

// The centroid comes from the directory, so a region seeded without bounds
// shows an em dash rather than 0,0.
func TestRegionList_ShowsCentroid(t *testing.T) {
	t.Parallel()
	dbPath, store := newDB(t)
	if err := store.Regions().UpsertFromDirectory(context.Background(), []regions.Region{
		{ID: 1, Name: "Puget Sound", OBABaseURL: "https://puget.example/", Active: true,
			Centroid: &regions.LatLon{Lat: 47.7528, Lon: -122.4924}},
		{ID: 2, Name: "Unsynced", OBABaseURL: "https://bare.example/", Active: true},
	}, testNow); err != nil {
		t.Fatalf("seed: %v", err)
	}

	stdout, _, err := cli(t, dbPath, "region", "list")
	if err != nil {
		t.Fatalf("region list: %v", err)
	}
	if !strings.Contains(stdout, "centroid=47.7528,-122.4924") {
		t.Errorf("region list = %q, want region 1's centroid", stdout)
	}
	if !strings.Contains(stdout, "centroid=\u2014") {
		t.Errorf("region list = %q, want an em dash for the unsynced region", stdout)
	}
}
```

Use whatever fixed instant this file already uses in place of `testNow` if the name differs.

- [ ] **Step 6: Run to verify they fail**

Run: `go test ./cmd/sidecar-admin/ -run TestRegionSet -v`
Expected: FAIL — unknown flag `--oba-api-key`.

- [ ] **Step 7: Update the CLI**

In `regionSet`, add the flag and merge it exactly the way `--agency-id` is merged, using `visitedFlags` so `--oba-api-key ""` clears rather than being read as absent:

```go
	obaKey := fs.String("oba-api-key", "", "OneBusAway REST API key for this region (empty clears it)")
```

```go
	newKey := current.OBAAPIKey
	if seen["oba-api-key"] {
		newKey = *obaKey
	}
```

and pass it in the `regions.LocalFields` literal.

In `regionList`, add the centroid and the key status. Print a status word, never the key:

```go
	for _, r := range list {
		centroid := "—"
		if r.Centroid != nil {
			centroid = fmt.Sprintf("%.4f,%.4f", r.Centroid.Lat, r.Centroid.Lon)
		}
		key := "not configured"
		if r.OBAAPIKey != "" {
			key = "configured"
		}
		fmt.Fprintf(stdout, "%d\t%s\tactive=%t\tagency=%s\ttz=%s\tcentroid=%s\toba-key=%s\n",
			r.ID, r.Name, r.Active, r.DefaultAgencyID, r.Timezone, centroid, key)
	}
```

- [ ] **Step 8: Run everything**

Run: `make check`
Expected: PASS.

- [ ] **Step 9: Commit**

```bash
git add cmd/sidecar-admin/ internal/httpapi/ cmd/sidecar/
git commit -m "Surface the region API key and centroid in the CLI and admin API"
```

---

### Task 9: SPA regions screen

The SPA's vitest project has no DOM, which is why `$lib/regions.ts` already holds the regions screen's view logic as pure functions with `+page.svelte` as thin markup. This task follows that split rather than fighting it: every rule with a failure mode goes into `regions.ts` and is tested there.

**Files:**
- Modify: `web/admin/src/lib/types.ts`
- Modify: `web/admin/src/lib/regions.ts`
- Modify: `web/admin/src/lib/regions.test.ts`
- Modify: `web/admin/src/routes/regions/+page.svelte`

**Interfaces:**
- Consumes: the `regionJSON` shape from Task 8 — `latitude: number | null`, `longitude: number | null`, `oba_api_key: 'region' | 'default' | 'none'`.
- Produces: `buildRegionPatch(defaultAgencyID, timezone, obaAPIKey?)`; `formatCentroid(region)`; `describeKeyStatus(status)`.

- [ ] **Step 1: Write the failing tests**

Append to `web/admin/src/lib/regions.test.ts`, matching the file's existing `describe`/`it` style:

```ts
describe('buildRegionPatch with an API key', () => {
	// Omission means unchanged. If an untouched key field sent '', every
	// unrelated edit an operator makes would silently wipe the region's key.
	it('omits oba_api_key entirely when undefined', () => {
		const payload = buildRegionPatch('1', 'America/Los_Angeles', undefined);
		expect('oba_api_key' in payload).toBe(false);
	});

	it('sends an empty oba_api_key when the operator clears it', () => {
		const payload = buildRegionPatch('1', 'America/Los_Angeles', '');
		expect(payload.oba_api_key).toBe('');
	});

	it('sends a trimmed key when one is entered', () => {
		const payload = buildRegionPatch('1', 'America/Los_Angeles', '  abc  ');
		expect(payload.oba_api_key).toBe('abc');
	});
});

describe('formatCentroid', () => {
	it('renders a point to four decimals', () => {
		expect(formatCentroid({ latitude: 47.752812, longitude: -122.492431 })).toBe(
			'47.7528, -122.4924',
		);
	});

	// 0,0 is a real coordinate in the Gulf of Guinea, so null must render as
	// "unknown" and 0 must render as 0 -- never the other way round.
	it('renders null as an em dash', () => {
		expect(formatCentroid({ latitude: null, longitude: null })).toBe('—');
	});

	it('renders Null Island as a real point', () => {
		expect(formatCentroid({ latitude: 0, longitude: 0 })).toBe('0.0000, 0.0000');
	});

	it('treats a half-set centroid as absent', () => {
		expect(formatCentroid({ latitude: 47.75, longitude: null })).toBe('—');
	});
});

describe('describeKeyStatus', () => {
	// Three states, not a boolean: a region whose vehicle search works fine
	// through the server default must not read as "not configured".
	it('distinguishes all three states', () => {
		expect(describeKeyStatus('region')).toBe('Configured for this region');
		expect(describeKeyStatus('default')).toBe('Using the server default');
		expect(describeKeyStatus('none')).toBe('Not configured — vehicle search unavailable');
	});

	it('falls back rather than rendering undefined for an unknown value', () => {
		expect(describeKeyStatus('something-new' as KeyStatus)).toBe('Unknown');
	});
});
```

Add `formatCentroid`, `describeKeyStatus`, and `KeyStatus` to the file's existing import from `./regions`.

- [ ] **Step 2: Run to verify they fail**

Run: `cd web/admin && npx vitest run src/lib/regions.test.ts`
Expected: FAIL — the three symbols do not exist.

- [ ] **Step 3: Extend the Region type**

In `web/admin/src/lib/types.ts`, add to the `Region` interface:

```ts
	/** null until a directory sync supplies usable bounds. 0 is a real value. */
	latitude: number | null;
	/** null until a directory sync supplies usable bounds. 0 is a real value. */
	longitude: number | null;
	/**
	 * Key status, never the key -- the server never sends one back.
	 * 'region'  this region carries its own
	 * 'default' empty here, but the server has a process-wide default
	 * 'none'    nothing configured; vehicle search will fail
	 */
	oba_api_key: KeyStatus;
```

and above it:

```ts
export type KeyStatus = 'region' | 'default' | 'none';
```

- [ ] **Step 4: Implement the pure functions**

In `web/admin/src/lib/regions.ts`, extend the payload interface and `buildRegionPatch`:

```ts
export interface RegionPatchPayload {
	default_agency_id?: string;
	timezone?: string;
	oba_api_key?: string;
}
```

```ts
/**
 * ...existing doc comment...
 *
 * obaAPIKey is sent only when it is defined, and `undefined` is the normal
 * state: the server never sends a key back, so an untouched field has no
 * value to resend. Sending '' for an untouched field would clear the region's
 * key on every unrelated edit. An explicit '' -- from the clear action --
 * is a deliberate clear and IS sent.
 */
export function buildRegionPatch(
	defaultAgencyID: string,
	timezone: string,
	obaAPIKey?: string,
): RegionPatchPayload {
	const payload: RegionPatchPayload = {
		default_agency_id: defaultAgencyID.trim(),
	};
	const tz = timezone.trim();
	if (tz !== '') payload.timezone = tz;
	if (obaAPIKey !== undefined) payload.oba_api_key = obaAPIKey.trim();
	return payload;
}

/**
 * formatCentroid renders a region's centroid for display.
 *
 * A null coordinate means the directory has not supplied usable bounds yet.
 * It must not render as 0, and 0 must not render as absent: 0,0 is a real
 * coordinate in the Gulf of Guinea, the same reason region id 0 is a real
 * region.
 */
export function formatCentroid(region: {
	latitude: number | null;
	longitude: number | null;
}): string {
	const { latitude, longitude } = region;
	if (latitude === null || longitude === null) return '—';
	return `${latitude.toFixed(4)}, ${longitude.toFixed(4)}`;
}

/**
 * describeKeyStatus turns the server's key status into operator-facing text.
 *
 * Three states rather than a boolean: a region with no key of its own but a
 * server default configured has working vehicle search, and reporting that as
 * "not configured" would send an operator chasing a problem that isn't there.
 */
export function describeKeyStatus(status: KeyStatus): string {
	switch (status) {
		case 'region':
			return 'Configured for this region';
		case 'default':
			return 'Using the server default';
		case 'none':
			return 'Not configured — vehicle search unavailable';
		default:
			return 'Unknown';
	}
}
```

Import `KeyStatus` from `./types`.

- [ ] **Step 5: Run to verify they pass**

Run: `cd web/admin && npx vitest run src/lib/regions.test.ts`
Expected: PASS.

- [ ] **Step 6: Update the markup**

In `web/admin/src/routes/regions/+page.svelte`, following the file's existing per-row state pattern:

- A read-only centroid cell rendering `formatCentroid(region)`.
- An `<input type="password">` for the key, bound to per-row state initialized to `undefined` and always rendered empty — the server never sends a value. Set the state on input, so an untouched row stays `undefined`.
- Status text from `describeKeyStatus(region.oba_api_key)` beside the input.
- A "Clear key" button that sets the row's key state to `''`.
- Pass the row's key state as the third argument to `buildRegionPatch`.

- [ ] **Step 7: Run the frontend checks**

Run: `make web-check`
Expected: PASS — svelte-check, prettier, eslint, and vitest all clean.

- [ ] **Step 8: Commit**

```bash
make check
git add web/admin/
git commit -m "Add the region centroid and OBA API key to the admin SPA"
```

---

### Task 10: Documentation

**Files:**
- Modify: `README.md`

**Interfaces:**
- Consumes: everything above.
- Produces: nothing.

- [ ] **Step 1: Document the two endpoints**

Add a section after the existing "Service alerts" section, matching its voice and depth:

- `GET /api/v1/regions/{regionId}/weather` — current conditions and hourly forecast for the region's centroid. `403` means unavailable (no provider key, no centroid yet, or the provider failed); apps hide the weather UI on any non-200. Note that `403` is deliberate and must not be "improved" to a 5xx.
- `GET /api/v1/regions/{regionId}/vehicles?query=` — substring search over the region's vehicle ids. Queries under 3 or over 64 characters return `[]`. Matching lowercases the *query* only, so fleets with uppercase ids are effectively case-sensitive; this is a deliberate compatibility quirk. Results cap at 250.

- [ ] **Step 2: Document configuration**

```sh
# Both are optional. Without them the corresponding endpoint degrades:
# weather returns 403 for every region; vehicle search returns 502 for any
# region that carries no key of its own.
export SIDECAR_OBA_API_KEY=...
export SIDECAR_PIRATE_WEATHER_KEY=...
```

Plus the per-region override, in the same sequential style as the existing `region set` example:

```sh
# A region's own key overrides SIDECAR_OBA_API_KEY. Pass an empty value to
# clear it and fall back to the process default.
./bin/sidecar-admin --db ./sidecar.db region set --id 1 --oba-api-key <key>
```

Note that centroids arrive automatically with `region sync` — they are computed from the directory's `bounds`, so weather needs no manual coordinate configuration.

- [ ] **Step 3: Commit**

```bash
make check
git add README.md
git commit -m "Document the weather and vehicle search endpoints"
```
