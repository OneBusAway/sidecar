# Weather + Vehicle Search — Design

**Date:** 2026-08-19
**Status:** Draft
**Implements:** [specification.md](../../../specification/specification.md) §9 and §10,
and the `getWeather` / `searchVehicles` operations in
[openapi.yaml](../../../specification/openapi.yaml).

## 1. Scope

Two rider-facing, read-only, unauthenticated proxies:

```
GET /api/v1/regions/{regionId}/weather              → WeatherForecast JSON
GET /api/v1/regions/{regionId}/vehicles?query=…     → [VehicleMatch] JSON
```

Neither stores anything. Both exist to spend an upstream call the app cannot make
itself — the weather provider needs a key the app must not carry, and the vehicle
search needs a full-fleet scan no phone should do.

Their real weight is the foundations they force into existence, all of which alarms
(§5 of the sidecar spec) and Live Activities (§6) inherit:

- **An OBA REST API client**, wired per region with that region's base URL and key.
- **An in-memory TTL cache with singleflight**, which spec §6.3 will need for the
  per-stop response sharing it mandates.
- **Region centroid and API-key columns**, plus the admin surfaces that set them.

### Out of scope

- **Alarms, Live Activities, push registrations, surveys, ghost bus reports,
  donations.** Separate slices.
- **A shared or persistent cache.** Decided against in §2.4; a swap later touches one
  package.
- **Weather providers other than Pirate Weather.** The interface exists so the tests
  can fake it, not because a second implementation is planned.
- **Rate limiting.** Spec §2.6 mandates it for the abuse-prone *write* endpoints.
  These are reads whose cost is bounded by the caches; §2.4 closes the
  unbounded-growth hazards they do introduce.
- **`Cache-Control` / `ETag` on the responses.** Deferred deliberately, consistent
  with the alerts feed.

## 2. Decisions

### 2.1 The OBA REST API client is the official Go SDK

`github.com/OneBusAway/go-sdk` v1.15.0 (verified on the module proxy 2026-08-19,
published 2026-07-20). It covers everything the sidecar will ever ask of the OBA
server: `AgenciesWithCoverage` and `VehiclesForAgency` for this slice,
`ArrivalAndDeparture` and `ArrivalsAndDeparturesForLocation` for alarms and Live
Activities.

Three of its `option.RequestOption`s are exactly the seams this design needs:
`WithBaseURL` (per region), `WithAPIKey` (per region), and `WithHTTPClient`
(`httptest` in tests, so handler tests exercise the real decode path against a fake
OBA server).

**Nothing outside `internal/obaapi` may import the SDK.** The rule is the one already
in force for `gen.*` in the sqlite adapter, for the same reason: the SDK's response
types are generated, deeply nested, and carry `apijson.Field` metadata on every
struct. Letting them reach `internal/vehicles` or `internal/httpapi` would make those
packages untestable without the SDK and would pin us to its type layout forever.
`internal/obaapi` maps them to flat domain types at the boundary.

### 2.2 The OBA API key is per-region, with a server-wide default

The regions directory carries `obaBaseUrl` but no key, and the OBA REST API requires
one. A single-region operator has one key; a multi-region deployment may hold a
different key per agency.

So: a locally-managed `oba_api_key` column beside `default_agency_id` and `timezone`,
falling back to a `--oba-api-key` / `SIDECAR_OBA_API_KEY` process default when empty.
Empty-means-inherit matches how `default_agency_id` already behaves and avoids a
nullable column for a value that has no meaningful "explicitly blank".

**The key is write-only across every surface.** `GET /api/admin/v1/regions` reports
whether a key is set, never the key. `sidecar-admin region list` prints a status word.
The admin API is session-authenticated, but a key echoed into a JSON response lands in
the SPA's memory, the browser's devtools, and any HAR file an operator attaches to a
bug report. There is no admin workflow that needs to read a key back — only to know
whether one is set, and to replace it. §2.10 extends the same rule to logs.

### 2.3 The region centroid is computed from the directory's `bounds`

`regions-v3.json` carries **no per-region `lat`/`lon`**. Each entry carries `bounds`:
an array of rectangles, each `{lat, lon, latSpan, lonSpan}`, where `lat`/`lon` is that
rectangle's center. Verified 2026-08-19 against
`internal/regions/testdata/regions-v3.json`, itself captured from the live directory:
of seven entries, rectangle counts run 1, 1, 2, 2, 3, 9, 9.

The centroid is the **area-weighted mean of the rectangle centers**:

```
w_i      = max(latSpan_i, 0) × max(lonSpan_i, 0)
centroid = ( Σ w_i·lat_i / Σ w_i ,  Σ w_i·lon_i / Σ w_i )
```

Two alternatives were rejected. The **union bounding box center** is dragged a long
way by a single small outlying rectangle — it puts San Diego's centroid in the
mountains east of the city. The **unweighted mean of rectangle centers** is not
invariant to how the bounds were split: cutting one rectangle into nine moves it.
Area weighting is invariant to that split, which matters because rectangle counts vary
from 1 to 9 across regions for no reason a consumer can see.

Degenerate cases, in order:

1. `bounds` absent or empty → centroid is **nil**.
2. `Σ w_i == 0` (every span zero or negative, i.e. a region described by points) →
   fall back to the unweighted mean of the centers.
3. The computed centroid outside ±90 latitude or ±180 longitude → **nil**.

Both columns are **nullable**, which is deliberate and unlike every other column in
this schema. `0, 0` is a real coordinate in the Gulf of Guinea, so a region whose
bounds have not synced must be distinguishable from one that genuinely sits on the
equator. This is the same rule that makes region id 0 (Tampa Bay) real rather than
"unset", and it is enforced in the type rather than by convention:

```go
// Centroid is nil until a directory sync supplies usable bounds. Lat and Lon
// are a pair or they are absent; a half-set centroid is not representable.
type LatLon struct{ Lat, Lon float64 }
```

A region whose bounds are unusable is **kept**, with a nil centroid — it simply has no
weather. This is a *new* rule, not an application of the existing one: `validate` in
`directory.go` currently skips the whole entry when a field is bad. Dropping a region
because its bounds are malformed would take its alerts feed down too, which is a far
worse outcome than a missing weather card.

### 2.4 Caches are in-memory, per-process, with singleflight — and bounded twice

`internal/cache` is a TTL map with an injected clock and a
`golang.org/x/sync/singleflight` group. Its properties, each load-bearing:

**Get-or-fetch is the only operation.** There is no exported `Set`. A caller that
could `Set` could write an entry whose age the cache never measured, and the whole
contract here is "this value is at most N minutes old".

**Errors are never cached.** A failed upstream call must be retried on the next
request. Caching a failure for 30 minutes converts a five-second provider blip into a
half-hour outage.

**Entry count is capped**, because the vehicle *query* cache is keyed by arbitrary
user input on an unauthenticated endpoint.

**Key length is capped too**, at the call site (§7). Capping only the entry count
leaves an attacker free to store 4096 megabyte-long keys: `query` is bounded only by
Go's 1 MB header budget, so the count cap alone permits multiple gigabytes resident.
Both caps are needed; either alone is a hole.

Cache loss on restart is harmless. Multiple instances each keeping their own copy
multiplies upstream calls by instance count, which for a 30-minute weather TTL is a
handful of calls an hour. If that ever matters, one package changes.

No background goroutine: eviction is lazy on read plus bounded on insert, so there is
nothing to start, stop, or leak.

### 2.5 Weather failure is `403`, and that is load-bearing

Spec §9 requires `403` on upstream failure because shipped apps treat any non-200 as
"hide the weather UI" and `403` is what they have been tested against. A `500` or
`502` here would be more honest and is wrong.

**Every** failure mode collapses to that one code: provider error, provider timeout, a
region whose centroid is nil, and a process with no provider key configured. One
response code, four distinguishable log lines. The alternative — `404` for an
unconfigured region, say — would tell the app the *region* does not exist, which is a
different and false claim.

`404` is still returned for a genuinely unknown region id, per the §1.2 contract,
before any provider call happens.

### 2.6 The vehicle match rule is a deliberate bug-compatible port

Spec §10 is unusually explicit: lowercase and trim **the query**, then match it as a
substring against the **raw** vehicle ids. It then says to replicate this exactly
rather than implementing true case-insensitivity, because results diverge on fleets
with uppercase ids.

So `strings.Contains(v.VehicleID, q)` where only `q` has been lowered. A fleet id of
`ABC123` does not match the query `abc`, and a test pins that — with a comment saying
it is intentional, because it is precisely the assertion a future reader will "fix".

The minimum length check counts **runes**, not bytes (`utf8.RuneCountInString`). The
reference is Rails, whose `String#length` is characters; a two-character CJK query is
six bytes, and a byte check would let it through into a full-fleet scan.

### 2.7 Vehicle-search upstream failure is `502`

Spec §10 defines only `200` and `404`, so this is a judgment call, recorded here.

Returning `200 []` when the OBA server is down is the tempting option and the wrong
one: it is indistinguishable from "no vehicle matches that", so a rider searching for
a bus that exists is told, confidently, that it does not. `502 Bad Gateway` says the
upstream leg failed, which is true; the app's search UI degrades on any non-200 the
same way.

Missing API key and missing region configuration collapse into the same `502` with a
distinct log line, keeping the response contract to one non-spec code rather than
three.

### 2.8 A partial fleet is a failed fleet — except for a `4xx` on one agency

Vehicle search fans out one `vehicles-for-agency` call per agency in the region. A
transport error, timeout, or `5xx` on any agency fails the whole fetch: nothing is
cached and the request returns `502`. Caching a fleet with one agency silently missing
for thirty minutes would tell every rider on that agency's routes that their bus does
not exist. Spec §6.2 states the principle for the analogous case: showing a duplicate
row is cosmetic, hiding a rider's bus is not.

A **`4xx` on a single agency's call is different** and contributes zero vehicles,
logged at `Warn`. An agency listed in `agencies-with-coverage` that has no realtime
vehicle feed answers deterministically and forever; treating that as a fetch failure
would brick vehicle search for the entire region permanently, and would re-hammer the
upstream on every cache miss while doing it. A permanent `4xx` is information about
that agency, not a failure of the request.

The residual risk — an agency erroring `5xx` forever takes the region's vehicle search
down — is accepted and documented for operators, whose remedy is to raise it with the
agency. It is loud rather than silent, which is the property that matters.

### 2.9 Existing time rules apply unchanged

`time.Now` and `time.Local` remain banned outside `cmd/` by the `forbidigo` rule. The
cache takes `now func() time.Time`; the weather assembler takes the instant from
`Deps.Now`. `retrieved_at` is stamped when the provider call returns and stored in the
cache entry — **not** recomputed on a cache hit, because its entire purpose is to let
a client see that the data is twenty-nine minutes old.

### 2.10 Neither API key may reach a log line

Both keys travel in the URL. Pirate Weather takes its key as a **path segment**
(`/forecast/{key}/{lat},{lon}`), and the SDK's `WithAPIKey` appends the OBA key as a
**query parameter** (`option.WithQuery("key", …)`).

Go's HTTP transport reports failures as `*url.Error`, which embeds the full URL in its
`Error()` string. §11 requires logging every non-200 with its cause — so a plain
`err` in that log line writes the secret into the log file, undoing the care §2.2
spends keeping it out of JSON and devtools.

Both `internal/weather` and `internal/obaapi` therefore **rewrap upstream errors at
their package boundary**, discarding any URL-bearing text and reporting the operation
and status instead. A test asserts that logged output never contains the key, using a
recognizable sentinel key value.

### 2.11 Keys are stored in plaintext

Both keys sit unencrypted in the SQLite file, like the session tokens and password
hashes already there. This matches the accepted-risk posture the admin design records:
the database file is trusted, and a process that can read it can also read whatever
would decrypt it. Stated here so it is a decision rather than an oversight.

## 3. Data model

### 3.1 Migration `00003_region_centroid_and_api_key.sql`

```sql
-- +goose Up
ALTER TABLE regions ADD COLUMN latitude    REAL;
ALTER TABLE regions ADD COLUMN longitude   REAL;
ALTER TABLE regions ADD COLUMN oba_api_key TEXT NOT NULL DEFAULT '';

-- Latitude and longitude are a pair or they are absent (§2.3). The Go type
-- makes a half-set centroid unrepresentable; without this the schema does not,
-- and a future writer could persist one.
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
-- The triggers must go first: SQLite refuses to DROP a column a live trigger
-- still references (verified 2026-08-19 against modernc.org/sqlite v1.56.0).
DROP TRIGGER regions_centroid_paired_update;
DROP TRIGGER regions_centroid_paired_insert;
ALTER TABLE regions DROP COLUMN oba_api_key;
ALTER TABLE regions DROP COLUMN longitude;
ALTER TABLE regions DROP COLUMN latitude;
```

Triggers rather than a `CHECK` constraint because SQLite cannot add a `CHECK` to an
existing table via `ALTER TABLE` — the alternative is the twelve-step table rebuild,
which is far more risk for the same invariant. Behavior verified 2026-08-19: a
half-set insert fails with `constraint failed`, while a fully-paired and a fully-null
insert both succeed. The adapter additionally maps a half-null row to a nil centroid
rather than trusting the trigger, since a Postgres adapter will express this as a
`CHECK` and both must behave identically.

`oba_api_key` follows `default_agency_id`: `NOT NULL DEFAULT ''`, where empty means
"inherit the process default".

### 3.2 Query changes

`UpsertRegionFromDirectory` gains `latitude` and `longitude` as directory-derived
columns (written on insert, refreshed on conflict). `oba_api_key` is **not** in that
statement — it is locally managed and must survive every hourly refresh, exactly like
`default_agency_id` and `timezone`. The existing
`testPartialUpsertPreservesLocalFields` conformance test is extended to cover it.

`SetRegionLocalFields` gains `oba_api_key`.

> **Hazard.** Every query in this repo uses bare `?` placeholders. sqlc numbers `?`
> positionally and `sqlc.arg()` by name, and mixing the two in one statement produces
> code that compiles, lints, and diffs cleanly while binding the wrong argument to
> every parameter at runtime. Keep the new columns on bare `?`, and after
> `make generate` read the generated params struct field order against the SQL before
> writing the adapter call.

### 3.3 Domain type changes

```go
type Region struct {
    // …existing fields…

    // Centroid is nil until a directory sync supplies usable bounds (§2.3).
    Centroid *LatLon

    // Locally managed, alongside DefaultAgencyID and Timezone.
    OBAAPIKey string
}
```

`Repository.SetLocalFields` changes shape:

```go
// LocalFields carries the region columns the directory does not supply. It is
// a struct rather than three positional strings because three adjacent string
// parameters is the shape that silently swaps two of them.
type LocalFields struct {
    DefaultAgencyID string
    Timezone        string
    OBAAPIKey       string
}

SetLocalFields(ctx context.Context, id int64, in LocalFields, now time.Time) error
```

This is a breaking interface change touching the sqlite adapter, the shared
conformance suite, `sidecar-admin`, and the admin regions handler. It is worth doing
now rather than adding a third bare string.

## 4. Packages

```
internal/
  cache/      TTL + singleflight, injected clock, bounded entry count   ← pure
  obaapi/     SDK wrapper: per-region client, domain types, fan-out
  weather/    Provider interface, PirateWeather impl, normalization     ← pure mapping
  vehicles/   Fleet assembly, the match rule, result caching
  httpapi/    weather.go, vehicles.go — region resolution + status codes
```

`weather`'s normalization, `vehicles`' match rule, and the centroid computation in
`regions` are pure functions over decoded input, testable with no HTTP and no clock.
That is where the real behavioral contracts live, which is why they are not in the
handlers.

## 5. `internal/cache`

```go
// Cache memoizes one value per key for a fixed TTL. It is safe for concurrent
// use. There is no Set: a value may only enter through Get's fetch function,
// so every entry's age is known (§2.4).
type Cache[V any] struct{ … }

// New builds a cache. budget bounds a single fetch; see Get.
func New[V any](ttl time.Duration, maxEntries int, budget time.Duration, now func() time.Time) *Cache[V]

// Get returns the cached value for key, or calls fetch and stores the result.
// Concurrent Gets for the same key while a fetch is in flight share that one
// fetch. Errors are returned to every waiter and are not cached.
func (c *Cache[V]) Get(ctx context.Context, key string, fetch func(context.Context) (V, error)) (V, error)
```

**Context handling** is the subtle part, and the two requirements pull against each
other. A caller whose request is cancelled must stop waiting; the shared fetch must
*not* die with that caller, because other waiters still need it. So:

- The fetch runs under `context.WithTimeout(context.WithoutCancel(ctx), budget)`.
  `WithoutCancel` detaches it from the first caller's lifetime — and, since it
  discards the deadline as well as the cancellation, the fetch gets a **fresh
  `budget` measured from fetch start**, owned by the cache. There is no request
  deadline left to "derive" from.
- Waiting uses `singleflight.Group.DoChan`, not `Do`: `Do` blocks uninterruptibly, so
  a cancelled caller could not stop waiting. `Get` selects on the returned channel and
  `ctx.Done()`, returning `ctx.Err()` on cancellation while the fetch continues for
  everyone else.

**Eviction** on insert when at `maxEntries`: expired entries first, then the oldest
insertion. Named here because it is the difference between three plausible
implementations and three different tests.

Three instances are constructed in `cmd/sidecar` and injected:

| Cache | Key | TTL | Cap | Budget |
|---|---|---|---|---|
| Weather conditions | `lat,lon` rounded to 4 decimals | 30 min | 256 | 5s |
| Region fleet | region id | 30 min | 256 | 12s |
| Vehicle query results | `regionID\|query` | 5 min | 4096 | 13s |

The query cache's budget exceeds the fleet cache's because its fetch may block on a
cold fleet fetch nested inside it. All three sit under `http.Server`'s 15s
`WriteTimeout`.

The weather cache is keyed by coordinate rather than region id so the cached value
holds only provider data. The region-specific envelope (`region_identifier`,
`region_name`, `latitude`, `longitude`) is assembled fresh on every request, so a
renamed region is correct immediately, and two regions sharing a centroid share one
upstream call.

## 6. `internal/obaapi`

```go
type Agency struct{ ID, Name string }
type Vehicle struct{ AgencyID, AgencyName, VehicleID string }

// Client reads the OBA REST API for one region at a time. Implementations must
// be safe for concurrent use.
type Client interface {
    // Fleet returns every vehicle currently reported across every agency with
    // coverage in the region, in agency order then response order.
    Fleet(ctx context.Context, region regions.Region) ([]Vehicle, error)
}

var ErrNotConfigured = errors.New("obaapi: region has no API key")
```

`Fleet` is one method rather than two because the two calls are always made together
and the fan-out policy (§2.8) belongs behind the interface, not in every caller.
Alarms will add `ArrivalAndDeparture` to this interface later.

Implementation:

1. Resolve the key: `region.OBAAPIKey` if non-empty, else the injected process
   default; if both are empty, return `ErrNotConfigured` **without making a request**.
2. Build an SDK client with `WithBaseURL(region.OBABaseURL)`, `WithAPIKey(key)`,
   `WithHTTPClient(injected)`, `WithRequestTimeout(4s)`, `WithMaxRetries(0)`.
   Construction is per call: it allocates a handful of service structs, which is noise
   beside an HTTP round trip, and it keeps the client stateless.
3. `AgenciesWithCoverage` once. Agency **names** come from
   `Data.References.Agencies`, not from the list entries (which carry only ids and
   bounding boxes) — so one call yields both.
4. `VehiclesForAgency` per agency, in parallel through
   `golang.org/x/sync/errgroup` with `SetLimit(12)`. Results are reassembled **by
   index** into agencies-with-coverage order, because parallel completion order is not
   deterministic and the response must be.
5. Failure handling per §2.8: `4xx` on one agency yields zero vehicles and a `Warn`;
   anything else fails `Fleet`.
6. Errors are rewrapped so no key-bearing URL escapes the package (§2.10).

**Timeout budget.** `WithRequestTimeout` is documented as *per attempt*, spanning
neither retries nor the surrounding context, which is why retries are set to zero —
otherwise one logical call is two attempts plus backoff and no arithmetic here holds.
With a 4s per-attempt timeout and a concurrency limit of 12, a cold fetch costs
`4s + ceil(agencies/12) × 4s`: 8s for the ≤12-agency regions that exist today, and
still inside budget up to 24. **The 12s cache budget is the actual guarantee**; the
arithmetic only shows it is not routinely hit. A `Fleet` that exceeds it reports an
ordinary error → `502`, rather than letting the server truncate a response mid-write.

## 7. Vehicle search

Handler, in order:

1. `ParseRegionSegment` (existing helper — accepts `1` and `1-puget-sound`); failure
   → `404 {"error":"Couldn't find Region"}`.
2. `Regions.Get`; `ErrNotFound` → the same `404`.
3. `q := strings.ToLower(strings.TrimSpace(query))`, then
   `n := utf8.RuneCountInString(q)`. If `n < 3` **or `n > 64`** → `200 []`, before any
   upstream call and before touching the cache. The lower bound is spec §10; the
   upper bound keeps an attacker from filling the cache with megabyte keys (§2.4). No
   vehicle id approaches 64 characters, so the cap costs no real query.
4. Query-result cache, keyed `regionID|q`. On miss:
   a. Fleet cache, keyed by region id → `obaapi.Fleet`.
   b. Filter with `strings.Contains(v.VehicleID, q)` — **raw id, lowered query only**.
   c. Map to `[]VehicleMatch{ID: agency id, Name: agency name, VehicleID: …}`.
5. Errors → `502` with an empty body and a logged `Error` naming the region and cause.
6. Success → `200` with a JSON array. **Never `null`**: the slice is initialized with
   `make([]VehicleMatch, 0)` so a no-match response is `[]`, which is what the schema
   says and what a client decoding into an array expects.

**Result cap and ordering.** Results keep **fleet order** —
`agencies-with-coverage` order, then each agency's response order, which §6 step 4
already guarantees is deterministic — and are truncated at **250**, with a `Warn` log
when truncation occurs. There is no re-sort; the order is inherited, not imposed. The
cap is a deliberate divergence from the reference, which returns everything: a
three-character query against a large numeric fleet can match thousands of vehicles on
an unauthenticated endpoint, and a rider scrolling past 250 candidate buses is not a
workflow this feature serves.

## 8. Weather

### 8.1 Provider

```go
// Provider fetches current conditions for a coordinate. One implementation
// exists (Pirate Weather); the interface is here so the mapping and the
// handler can be tested without a network.
type Provider interface {
    Fetch(ctx context.Context, at LatLon) (Conditions, error)
}
```

`PirateWeather` calls
`https://api.pirateweather.net/forecast/{key}/{lat},{lon}?units=us&exclude=minutely,alerts`
with the request built via `http.NewRequestWithContext` (the `noctx` linter is
enabled) and the response body closed in a `defer`. Non-200 upstream is an error, and
every error is rewrapped to drop the key-bearing URL (§2.10). `hourly` and `daily` are
both retained: `today_summary` is `daily.data[0].summary` (the *day's* summary), not
`daily.summary` (the week's).

`units` is the constant `us`. It is echoed into the response's `units` field, so the
value the client sees always describes the numbers it accompanies.

### 8.2 Mapping

Pure function, no clock, no I/O:

| Response field | Source |
|---|---|
| `icon` | `icon` (Dark Sky vocabulary, passed through) |
| `summary` | `summary` |
| `temperature` | `temperature` |
| `temperature_feels_like` | `apparentTemperature` |
| `precip_per_hour` | `precipIntensity` |
| `precip_probability` | `precipProbability` |
| `wind_speed` | `windSpeed` |
| `time` | `time` (epoch seconds, passed through) |

Applied to `currently` for `current_forecast` and to each `hourly.data[]` entry for
`hourly_forecast`.

### 8.3 Handler

1. Region segment parse and lookup → `404` on miss (§1.2), before anything else.
2. No provider key configured, or `region.Centroid == nil` → `403`, logged (§2.5).
3. Conditions cache keyed by rounded coordinate → `Provider.Fetch`. Error → `403`,
   logged with the rewrapped cause.
4. Assemble the envelope from the region row and the cached conditions:
   `latitude`, `longitude` from the centroid; `region_identifier` the **public**
   region id; `region_name` the region's name; `units`; `today_summary`;
   `current_forecast`; `hourly_forecast`; and `retrieved_at` as an **RFC 3339 UTC
   string** — the OpenAPI schema says `type: string, format: date-time`, and an epoch
   integer would satisfy every test this design describes while violating the
   contract.
5. `200`.

The `403` body is `{}`. Spec §9 says any body is ignored; an empty object is valid
JSON for a client that decodes before checking status.

## 9. Admin surface

### 9.1 `sidecar-admin`

```
region set --id N [--agency-id ID] [--timezone TZ] [--oba-api-key KEY]
```

`--oba-api-key ""` clears the region's key, restoring the process-default fallback
(the existing visited-flags mechanism already distinguishes "passed empty" from "not
passed"). `region list` gains two columns: the centroid (or `—` when unsynced) and the
key's status. The key itself is never printed (§2.2).

### 9.2 Admin API

`regionJSON` gains:

```json
{ "latitude": 47.75, "longitude": -122.49, "oba_api_key": "region" }
```

`latitude` and `longitude` are `null` when the centroid is nil. The key field is a
**status enum, not the key**: `"region"` (this region has its own), `"default"` (empty
column, process default present, so calls will work), or `"none"` (empty column, no
process default, so calls will fail). A plain boolean would report `false` for a
region whose vehicle search works perfectly via the default, which is the reading an
operator would act on wrongly.

`patchRegionRequest` gains `oba_api_key *string` — write-only, `""` clears. The
existing "at least one field" guard extends to three fields, and its error string
(`"provide default_agency_id and/or timezone"` in `admin_regions.go`) must be updated
with it; it is the kind of message that stays stale for a year.

### 9.3 SPA

The regions screen gains a read-only centroid display and a `type="password"` OBA API
key input showing the §9.2 status rather than a value, with an explicit clear action.
Submitting an untouched form must not send `oba_api_key` at all — omission means
unchanged, and sending `""` would silently wipe the key.

## 10. Configuration

Two new flags on `cmd/sidecar`, each with an environment fallback matching the
existing pattern:

| Flag | Env | Absent behavior |
|---|---|---|
| `--oba-api-key` | `SIDECAR_OBA_API_KEY` | Regions with their own key still work; others return `502` |
| `--pirate-weather-key` | `SIDECAR_PIRATE_WEATHER_KEY` | Weather returns `403` for every region |

Neither is required to boot: a feed-only deployment is legitimate. Each absent key
logs once at `Warn` during startup, naming the endpoint it disables, so the operator
learns from the log rather than from a rider.

## 11. Error handling summary

| Condition | Weather | Vehicles |
|---|---|---|
| Unparseable or unknown region | `404 {"error":"Couldn't find Region"}` | same |
| Query under 3 or over 64 runes | — | `200 []` |
| No provider / API key configured | `403 {}` | `502` |
| Centroid nil | `403 {}` | — |
| Upstream error, non-200, or timeout | `403 {}` | `502` |
| Store error on region lookup | `500`, empty body | same |

Every non-200 that is not a `404` logs at `Error` with the region id and the rewrapped
cause (§2.10). No upstream error text, and no API key, ever reaches a rider or a log.

## 12. Testing strategy

Test-driven, in dependency order, each layer complete before the next exists.

**1 — Centroid computation.** Pure, table-driven, against the real captured fixture:
the 9-rectangle Puget Sound case, the 1-rectangle Davis case, split-invariance (one
rectangle versus the same area expressed as four), zero-span fallback to the
unweighted mean, empty bounds → nil, out-of-range result → nil, and a region with
unusable bounds surviving the sync with its alerts intact.

**2 — `internal/cache`.** Pure, with a fake clock. Hit before TTL; miss after; error
not cached (two calls, two fetches); singleflight proven by a fetch that blocks on a
channel while N goroutines call `Get` and exactly one fetch is observed; a cancelled
caller returning `ctx.Err()` promptly **while the shared fetch completes and its value
is cached** — the assertion that fails if `Do` is used instead of `DoChan`, or if the
fetch context is not detached; the fetch's own budget expiring independent of any
caller; eviction preferring expired entries then oldest insert.

**3 — `internal/weather` mapping.** Pure. A captured Pirate Weather fixture through
the mapper, asserting every field including the `daily.data[0].summary` source for
`today_summary` and the pass-through of an unrecognized `icon` value.

**4 — `internal/vehicles` match rule.** Table-driven, no HTTP: under 3 runes and over
64 runes (bytes vs. runes covered by a CJK case), whitespace trimmed, query lowered,
**and the uppercase-fleet-id case that must NOT match** — with a comment marking it
intentional. Truncation at 250 preserving fleet order with no re-sort.

**5 — `internal/obaapi`.** `httptest` server standing in as the OBA REST API via
`option.WithHTTPClient`, exercising the real SDK decode path: agency names resolved
from `references`; deterministic reassembly under out-of-order parallel completion;
one agency returning `404` yielding an otherwise-complete fleet; one agency returning
`500` failing the whole fetch; `ErrNotConfigured` making zero HTTP requests; the
per-region key overriding the process default; and a transport failure whose error
string contains neither the key nor the URL.

**6 — Store and migrations.** Extends the shared conformance suite in
`internal/store/storetest`, so a future Postgres adapter inherits it: nullable centroid
round-trip including the `NULL` case and a genuine `0,0`; a half-set centroid rejected;
`oba_api_key` surviving `UpsertRegionFromDirectory`; `SetLocalFields` writing all
three fields.

**7 — HTTP handlers.** `httptest` against a real store plus fake upstreams. The full
§11 table, both endpoints. Specifically: `403` not `404` for a nil centroid; `[]` not
`null` for a no-match search; the region-segment table (`1-puget-sound`, `007`,
`int64` overflow, non-numeric) all `404` and never `500`; `retrieved_at` holding
steady across a cache hit rather than advancing, and parsing as RFC 3339; and a
`slog` handler capturing every log record for the whole request, asserted not to
contain a sentinel API key.

**8 — Admin surfaces.** The key never appearing in any `GET` response body — asserted
against the **raw JSON bytes**, not the decoded struct, so a field added later without
a tag change still fails. All three `oba_api_key` status values. Omitted
`oba_api_key` leaving the stored key intact; `""` clearing it. CLI: `region list`
printing the status word and not the key.

### Wire assertions

The existing golden-file ban is specific to protobuf, whose field order is not stable.
JSON from `encoding/json` is deterministic, but these tests still decode and compare
with `go-cmp` against a literal rather than matching strings — a golden file would
make an accidental field rename look like a diff to accept.

One additional assertion per endpoint compares the **set of top-level JSON keys**
against a Go literal listing the OpenAPI schema's property names, with a comment naming
the schema (`WeatherForecast`, `VehicleMatch`) and its line. Parsing `openapi.yaml` at
test time would need a YAML dependency this design does not otherwise want; a
hand-maintained literal catches the rename that actually happens — someone editing the
Go struct — and its staleness risk is one comment away from being noticed.

## 13. Dependencies

All verified available 2026-08-19:

| Module | Version | Purpose |
|---|---|---|
| `github.com/OneBusAway/go-sdk` | v1.15.0 | OBA REST API client |
| `golang.org/x/sync` | v0.22.0 | `singleflight`, `errgroup` — already indirect, becomes direct |

No new frontend dependencies.

## 14. Build order

1. Centroid computation, migration, domain types, queries, conformance-suite
   extensions.
2. `internal/cache`.
3. `internal/obaapi` + vehicle search end to end.
4. `internal/weather` + weather endpoint end to end.
5. Admin surfaces: CLI, admin API, SPA.
6. README: the two endpoints, the two new flags, and the key-configuration workflow.

Weather does not depend on `obaapi`, so steps 3 and 4 are independent after step 2.
