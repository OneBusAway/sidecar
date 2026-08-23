# Ghost Bus Reports — Design

**Date:** 2026-08-23
**Status:** Reviewed
**Implements:** [specification.md](../../../specification/specification.md) §8 (with the
§2.6 throttles), and the `createGhostBusReport` operation in
[openapi.yaml](../../../specification/openapi.yaml).

## 1. Scope

The rider-facing write endpoint, its two abuse throttles, an asynchronous
trip-details enrichment snapshot, and a CLI export for agencies:

```
POST /api/v2/regions/{regionId}/ghost_bus_reports   → 201 {"id": "<public identifier>"}
```

plus a `sidecar-admin ghostbus export` command producing the vendor-facing CSV, and a
background snapshot worker following the alarm-scheduler pattern.

The contract was verified against the reference implementation (`../obacloud`:
`Api::V2::GhostBusReportsController`, `GhostBusReport`,
`GhostBusReports::CaptureSnapshotJob`, `GhostBusReports::CsvExport`, and the
rack-attack ghost bus throttles) and the iOS client (`../ios`:
`ObacoAPIService.postGhostBusReport`, `GhostBusReportDraft`,
`TripPage/GhostBusReportView.swift`). Android has no ghost bus code; iOS is the only
shipped client. This document was itself reviewed against all four repositories; the
deliberate divergences from the reference are called out inline.

### Out of scope

- **Rider-facing read API.** The spec forbids one; the CSV export is the read surface.
- **Admin API / SPA pages.** CLI first, exactly as surveys shipped. The domain
  package and repository are designed so an admin API is a thin layer later.
- **Retention/reaping.** Reports are agency data, kept indefinitely (spec §13).
- **OBACloud's Trips dashboard, search, and metrics.** Dashboard concerns; the CSV
  carries a `trip_report_count` column so grouping survives into a spreadsheet.
- **A `.json` path suffix.** The reference throttle tolerates one and this repo
  registers `.json` variants where clients send them (surveys); iOS does not send
  one here, so no variant is registered. Deliberate.

## 2. Decisions

### 2.1 Client contract facts that shape the server

- iOS sends a **form-encoded** body (`NetworkHelpers.dictionary(toHTTPBodyData:)`),
  not JSON. JSON acceptance is still mandatory (spec §2.2) and is exactly the path
  the per-user throttle must not be bypassed through (§2.6).
- iOS sends `predicted` as the strings `"1"` / `"0"`, timestamps as epoch-ms
  integers, and decodes the response as `{ "id": String }` — the identifier is
  opaque to the client.
- iOS treats **any 422 as "already reported"** (`GhostBusReportView.swift:168` keys
  on the status code alone). The `already_reported` error code is still emitted per
  spec — future clients key on it — but server-side validation is a backstop, not
  UX: the app pre-enforces the wait choices and the 1,000-char comment cap.
- The wait-duration choice list `[5, 10, 15, 20, 30]` and the comment cap are
  mirrored by hand in `GhostBusReportView.swift` (`waitChoices` /
  `commentMaxLength`); changing either here requires a client change.
- iOS percent-encodes with an allow-list, so a rider-legal comment full of emoji
  can produce a form body well over 8 KB. This is why the oversized-body guard in
  §2.4 is JSON-only.

### 2.2 Validation (spec §8)

Required: `user_identifier`, `trip_identifier`, `service_date`,
`wait_duration_minutes`. Rules:

- `wait_duration_minutes` ∈ {5, 10, 15, 20, 30}, else a validation 422.
- `comment` ≤ 1,000 **runes**. This matches the reference (Rails `String#length`
  counts code points), not the app: Swift counts grapheme clusters, so a
  1,000-cluster emoji comment the app accepts can exceed 1,000 runes and 422. That
  matches OBACloud's behavior today and is accepted; the app shows its generic
  failure copy.
- `user_latitude` ∈ [−90, 90], `user_longitude` ∈ [−180, 180]; each independently
  optional (the reference does not require them as a pair). An unparseable
  coordinate is a 422, not silently dropped (house precedent from surveys).
- All epoch-ms timestamp fields (`service_date`, `scheduled_arrival_at`,
  `predicted_arrival_at`, `prediction_last_updated_at`) parse as integers via the
  existing `params.int64` semantics; a non-integer value coerces to **null** — and
  for `service_date` then fails the presence check — never fuzzily parsed, because
  `service_date` participates in the dedupe key (spec §8).
- `predicted` is three-state: `1|t|true|on` → true, `0|f|false|off` → false,
  absent/empty/unrecognized → null. (Bare JSON booleans arrive stringified by
  `params.str`, so `{"predicted": true}` works.) This is *not* the §2.7 allow-list
  — that rule is for push routing, where unrecognized must fail safe to falsy —
  and it deliberately diverges from Rails' permissive cast (which turns any
  unrecognized non-nil string into true): `predicted` is forensic data, and an
  unrecognized value recorded as null is honest where true is a guess.
- Identifier-ish fields (`user_identifier`, `trip_identifier`, `route_identifier`,
  `stop_identifier`, `vehicle_identifier`) share the surveys' 255-char
  `maxIdentifierLen` cap; anything over is a validation 422 rather than a
  truncated store. The `user_identifier` cap is checked **before** the per-user
  throttle (§2.4), so oversized attacker-chosen strings never become limiter keys.

Validation failure → `422 {"error": "Unable to save report", "messages": […]}`
(spec §2.5 row 2). Unknown region → `404 {"error": "Couldn't find Region"}`.

### 2.3 Dedupe: insert, then map the constraint

One report per `(region, user_identifier, trip_identifier, service_date)`, enforced
by a unique index. There is **no pre-check SELECT**: the handler validates fields,
attempts the insert, and maps the dedupe-index violation to
`422 {"error": "already_reported", "messages": ["User has already reported this trip"]}`.
This collapses the reference's two duplicate paths (validation-caught and
race-caught) into one; the spec explicitly allows the `messages` text to differ
between implementations, and a `500` on the race path would make the app retry
forever.

**The table has two unique indexes**, and modernc/SQLite reports both as the same
`SQLITE_CONSTRAINT_UNIQUE` code — the discriminator is the message text, which
names the violated columns. Following the house pattern
(`internal/store/sqlite/alarms.go`, `strings.Contains(err.Error(), "UNIQUE
constraint failed: alarms.region_id")`), the sqlite adapter returns `ErrDuplicate`
**only** when the message names the dedupe columns
(`ghost_bus_reports.region_id`). A `public_identifier` collision (astronomically
unlikely, but distinguishable) re-mints the token and retries once, then fails
`500` — it must never surface as `already_reported`, which would tell a rider
their first-ever report was a duplicate.

`service_date` is stored as the raw epoch-ms integer it arrived as, so the dedupe
key is deterministic. **Deliberate divergence:** OBACloud truncates to seconds
(`Time.zone.at(ms / 1000)`), so its dedupe key is coarser. iOS resends the OBA
API's own service date verbatim — stable across submissions — so the shipped
client behaves identically; a hypothetical client varying sub-second digits would
dedupe on OBACloud and not here. Accepted: raw storage keeps the house "ms as
received" rule and avoids a lossy transform in a key column.

### 2.4 Throttles (spec §2.6)

Two limiters, both `ratelimit.Limiter` (fixed-window, clockless):

- **10/hour per IP**, applied by the existing `throttleByIP` wrapper → bodyless 429.
- **20/day per `user_identifier`**, applied inside the handler after
  `parseRequestParams` **and after the `maxIdentifierLen` check** (so limiter keys
  are bounded at 255 chars), but before any other validation or storage. Because
  the handler parses query, form, and JSON into one params bag with body-first
  precedence, the throttle bucket automatically matches the identifier the report
  is filed under — the JSON-bypass hazard the spec warns about cannot arise from a
  separate body peek, because there is no separate body peek. A blank/absent
  `user_identifier` skips the per-user counter (no pooled nil bucket) and fails
  presence validation instead. Retention note: the limiter sweeps once per window,
  so a bucket can live up to two windows (~48 h); with keys length-capped and the
  IP limiter bounding arrival rate, worst-case map growth is bounded and
  acceptable for an abuse brake.

**Oversized-JSON 403 — JSON only.** Both the spec's rationale and the reference
scope the 8 KB cap to JSON bodies (`rack_attack.rb`: `media_type ==
"application/json" && content_length > 8_192`); form bodies keep the shared 64 KB
`requestBodyLimit` — iOS's percent-encoding can push a legal report past 8 KB
(§2.1), and there is no uncounted body peek here for padding to defeat. Mechanics,
since `parseRequestParams`' `MaxBytesReader` never inspects `Content-Length`: the
handler first checks `Content-Type: application/json && r.ContentLength > 8192` →
bodyless `403` without reading the body; then, for JSON, parses with
`maxBytes = 8192` and maps `errBodyTooLarge` → the same bodyless `403` (covers
chunked bodies and lying declarations). Two deliberate divergences, stated here so
they aren't "fixed" later: surveys map `errBodyTooLarge` to a 422, ghost bus uses
403 because the spec pins it; and this 403 is bodyless where the same route's
cross-site-guard 403 carries `{"error": "cross-site request rejected"}` — the
presence/absence of a body is how operators tell them apart in logs.

### 2.5 Public identifier

`securetoken.New()` — the same 22-char URL-safe generator survey responses use.
The written spec's §2.4 note ("the reference uses 20 hex chars") describes
OBACloud, not a wire requirement: iOS decodes `id` as an opaque string. House
precedent wins over byte-for-byte mimicry; what is mandatory is unguessability and
never exposing sequential ids. Known cosmetic cost, already documented for
surveys: URL-safe base64 ids can open with `-`, so about 1 in 64 exported ids
gains a leading apostrophe from the CSV injection guard.

### 2.6 Snapshot enrichment: DB-as-queue worker

Per spec §8 enrichment is asynchronous and never blocks the 201. Two candidate
shapes were considered:

- *Request-time goroutine* (closest analog to OBACloud's job queue): simple, but
  this repo has no durable queue, so a crash between 201 and capture strands the
  report `pending` forever with nothing to retry it.
- **DB-as-queue polling loop (chosen):** the report row itself is the queue entry.
  `ghostbus.Scheduler.RunLoop` (pattern: the alarms `(*Scheduler).RunLoop`, which
  is single-instance by construction — one loop per process, one process per
  database; running two sidecar processes against one database is already
  unsupported for alarms and stays unsupported here) polls every **30 seconds**
  for reports with `snapshot_status = 'pending' AND snapshot_attempts < 3`,
  oldest first, bounded batch:

  1. Region has no OBA base URL or no resolvable API key → `unavailable`.
  2. Call `obaapi.TripDetails` with the report's trip id and `service_date`.
  3. Definitive miss (404, empty entry, unparseable body) → `unavailable`.
  4. Transient failure (network error, 5xx, timeout) → increment
     `snapshot_attempts`; a report is tried **3 times total** (matching
     OBACloud's `retry_on … attempts: 3`): the increment that brings
     `snapshot_attempts` to 3 also sets `snapshot_status = 'unavailable'` in the
     same UPDATE, so no row can sit at the cap still `pending`.
  5. Success → store the pruned snapshot JSON, `snapshot_status = 'captured'`,
     `snapshot_captured_at = now`.

  The `snapshot_attempts < 3` poll predicate (mirrored in the partial index) is
  the leak guard: even if a crash lands between a failed fetch and its UPDATE, the
  row is retried at most up to the cap, and a row that somehow reaches the cap
  without a status write simply stops matching the poll — no head-of-line
  blocking, no forever-retry.

  The loop is safe under at-least-once execution: a crash mid-capture re-runs the
  fetch; re-capturing an already-captured report cannot happen because the poll
  filters on `pending`.

**Snapshot shape** (mirrors `CaptureSnapshotJob#prune`): a single JSON document —

```json
{
  "current_time": 1756000000000,
  "status":  { "predicted": true, "vehicleId": "…", "scheduleDeviation": 60,
               "phase": "in_progress",
               "lastKnownLocation": {"lat": 47.6, "lon": -122.3} },
  "display": { "headsign": "…", "route_short_name": "…", "route_long_name": "…",
               "route_type": 3, "stop_name": "…", "stop_lat": 47.6, "stop_lon": -122.3 }
}
```

`status` is the trip-details entry's status block restricted to OBACloud's
`STATUS_KEYS` (`predicted vehicleId lastUpdateTime lastLocationUpdateTime
scheduleDeviation phase serviceDate closestStop closestStopTimeOffset
distanceAlongTrip totalDistanceAlongTrip lastKnownLocation position orientation
activeTripId`); `display` resolves the trip/route/stop out of the response
`references` (route id from the report's `route_identifier`, falling back to the
trip reference's `routeId`), keys absent when unresolvable.

**Absent means absent.** The reference prunes from raw JSON, so keys the upstream
omitted are genuinely missing from the snapshot. The go-sdk's typed structs are
value-typed (absence and zero are indistinguishable — `Predicted bool`,
`LastKnownLocation` a bare struct), so the pruner MUST work from the response's
raw JSON (the SDK exposes `RawJSON()`), copying only keys actually present.
Otherwise `"predicted": false` becomes unfalsifiable and — worse — zero-valued
coordinates put the vehicle at Null Island, which the CSV's distance column would
render as thousands of km of plausible-looking garbage.

The DB stores only raw identifiers; without `display` the CSV could never show
route names or stop coordinates.

### 2.7 obaapi addition

`Client.TripDetails(ctx, region, tripID string, serviceDateMs int64)` returning
the pruned snapshot document (§2.6), built on the go-sdk's `TripDetails.Get`
(verified present in v1.15.0 with a `ServiceDate` param and `References` in the
response), pruning from `RawJSON()` per §2.6. **Leave `includeSchedule` at its
server default (true)** — the reference passes no include flags, and the schedule
block is what pulls the rider's stop into `references.stops`; disabling it as an
"optimization" silently blanks `stop_name`, `stop_lat`/`stop_lon`, and the
distance column. Same rules as existing calls: memoized per-region SDK clients,
redacted errors, and a "not found / empty entry" sentinel distinct from transient
errors so the worker can tell step 3 from step 4. Nothing outside
`internal/obaapi` sees a go-sdk type.

### 2.8 CSV export

`sidecar-admin ghostbus export --region N [--since RFC3339]` writes CSV to stdout,
modeled on `survey responses` and OBACloud's `CsvExport::HEADERS`:

```
public_identifier reported_at_utc reported_at_local service_date
route_id route_short_name headsign trip_id trip_report_count
stop_id stop_name stop_sequence vehicle_id
predicted wait_duration_minutes comment
scheduled_arrival_utc predicted_arrival_utc schedule_deviation_minutes
prediction_last_updated_utc prediction_staleness_minutes
user_latitude user_longitude
snapshot_status snapshot_captured_at_utc
vehicle_last_lat vehicle_last_lon vehicle_distance_from_stop_m
snapshot_trip_phase snapshot_json
```

Cell rendering (pinned here because the schema stores raw ms and 0/1 ints, and the
obvious dumps would regress the reference's human-readable export):

- `service_date` renders as a region-local ISO **date** (`2026-08-23`), matching
  `in_time_zone(zone).to_date.iso8601` — not the 13-digit ms integer.
- `reported_at_local` renders in the region's configured timezone (with the known
  caveat that an unconfigured region defaults to UTC — follow-ups doc item 1);
  `*_utc` columns are RFC 3339 UTC instants.
- `predicted` renders `true` / `false` / blank, never `1`/`0`.
- `trip_report_count` is a `GROUP BY (trip_identifier, service_date)` count over
  the exported set, carrying trip-instance grouping into the spreadsheet.
- `vehicle_last_lat`/`lon` come from the snapshot's `lastKnownLocation`, falling
  back to `position` (reference: `status["lastKnownLocation"] || status["position"]`).
- `vehicle_distance_from_stop_m` is haversine(vehicle last position, snapshot stop
  coords), **blank when either coordinate is absent from the snapshot** — never
  computed from defaulted zeros (see §2.6's absent-means-absent rule).
- `prediction_staleness_minutes` = (scheduled_arrival_at −
  prediction_last_updated_at) / **60,000** (both stored as epoch ms), rounded;
  blank unless both present.
- Every string cell goes through the surveys' formula-injection guard (leading
  apostrophe for `=`, `+`, `-`, `@`, tab, CR): comments are unauthenticated rider
  input and GTFS names come from external feeds.
- No row cap: the CLI streams; OBACloud's 10,000-row cap protects a web download
  path this design does not have.

## 3. Data model

Migration `00007_ghost_bus_reports.sql`, following the 00006 house conventions
(AUTOINCREMENT, `ON DELETE CASCADE`, `updated_at` everywhere, CHECK on closed
enums, `<table>_<col>_idx` naming):

```sql
CREATE TABLE ghost_bus_reports (
  id                         INTEGER PRIMARY KEY AUTOINCREMENT,
  region_id                  INTEGER NOT NULL REFERENCES regions(id) ON DELETE CASCADE,
  public_identifier          TEXT    NOT NULL,
  user_identifier            TEXT    NOT NULL,
  trip_identifier            TEXT    NOT NULL,
  service_date               INTEGER NOT NULL,          -- epoch ms, as received
  route_identifier           TEXT,
  stop_identifier            TEXT,
  vehicle_identifier         TEXT,
  stop_sequence              INTEGER,
  predicted                  INTEGER,                    -- NULL / 0 / 1
  schedule_deviation_minutes INTEGER,
  wait_duration_minutes      INTEGER NOT NULL,
  comment                    TEXT,
  user_latitude              REAL,
  user_longitude             REAL,
  scheduled_arrival_at       INTEGER,                    -- epoch ms
  predicted_arrival_at       INTEGER,                    -- epoch ms
  prediction_last_updated_at INTEGER,                    -- epoch ms
  snapshot_status            TEXT    NOT NULL DEFAULT 'pending'
    CHECK (snapshot_status IN ('pending', 'captured', 'unavailable')),
  snapshot_json              TEXT,
  snapshot_captured_at       INTEGER,                    -- epoch seconds
  snapshot_attempts          INTEGER NOT NULL DEFAULT 0,
  created_at                 INTEGER NOT NULL,           -- epoch seconds
  updated_at                 INTEGER NOT NULL            -- epoch seconds
);
CREATE UNIQUE INDEX ghost_bus_reports_public_identifier_idx
  ON ghost_bus_reports(public_identifier);
CREATE UNIQUE INDEX ghost_bus_reports_dedupe_idx
  ON ghost_bus_reports(region_id, user_identifier, trip_identifier, service_date);
CREATE INDEX ghost_bus_reports_snapshot_pending_idx ON ghost_bus_reports(id)
  WHERE snapshot_status = 'pending' AND snapshot_attempts < 3;
CREATE INDEX ghost_bus_reports_region_created_idx
  ON ghost_bus_reports(region_id, created_at);
```

House rules apply: INTEGER epochs, never DATETIME (modernc text-sort trap); ms
fields stored as received; sqlc named args only, never mixed with bare `?`.

## 4. HTTP

- `POST /api/v2/regions/{regionId}/ghost_bus_reports` — registered only when the
  `GhostBus` repository dep is non-nil, like every optional service. Region segment
  accepts `{int}` and `{int}-slug`. No `.json` variant (scope note in §1).
- Wrapped in `throttleByIP(deps.GhostBusIPLimiter)` (default 10/hour; tests inject
  tighter). Per-user limiter (`GhostBusUserLimiter`, default 20/day) checked in the
  handler per §2.4 above.
- Responses: `201 {"id": …}`; bodyless `403` oversized JSON body (§2.4, including
  how it differs from the cross-site guard's 403); `404` unknown region; `422` in
  the two shapes of §2.2/§2.3; `429` bodyless; `500` on store failure with the
  real error logged (house precedent from surveys).
- **Never log `user_identifier`** or coordinates: the identifier is a long-lived
  device pseudonym and the coordinates are a rider's location (spec §13 privacy
  posture). Log region ids and counts.

## 5. `sidecar-admin`

```
sidecar-admin ghostbus export --region N [--since RFC3339]
```

One command in this slice. `--since` filters on `created_at`; the timestamp
requires an explicit UTC offset like every other CLI instant. Exit non-zero on an
unknown region. (List/show subcommands can come with an admin API later; the CSV is
the read surface agencies asked for.)

## 6. Configuration

Nothing new. The snapshot worker uses the existing per-region OBA API keys with the
`SIDECAR_OBA_API_KEY` fallback; a region resolving to no key yields
`snapshot_status = 'unavailable'`, never a blocked report. `cmd/sidecar` starts
the snapshot loop whenever the ghost bus repository and OBA client are configured.

## 7. Packages

| File | Responsibility |
|---|---|
| `internal/ghostbus/ghostbus.go` | Domain type, validation, `Repository`, `ErrDuplicate`/`ErrNotFound` |
| `internal/ghostbus/snapshot.go` | Snapshot scheduler loop + capture logic (§2.6) |
| `internal/ghostbus/haversine.go` | Distance helper for the CSV |
| `internal/obaapi/obaapi.go` | `TripDetails` addition (§2.7) |
| `internal/store/sqlite/ghostbus.go` + queries + migration 00007 | sqlc adapter |
| `internal/store/storetest/ghostbustest.go` | Conformance suite (dedupe race included) |
| `internal/httpapi/ghostbus.go` | Handler, throttles, JSON 8 KB/403 guard |
| `cmd/sidecar-admin/ghostbus.go` | CSV export command |
| `cmd/sidecar/main.go`, `internal/httpapi/router.go` | Wiring |

Time is injected everywhere (`Now func() time.Time`); `time.Now` stays banned
outside `cmd/`.

## 8. Dependencies

No new Go modules. go-sdk v1.15.0 (already vendored) covers trip-details.

## 9. Build order

1. Domain package: types, validation, three-state `predicted`, haversine.
2. Migration + sqlc queries + sqlite adapter + storetest conformance (incl. the
   concurrent-duplicate race, the public-identifier-collision retry, and the
   pending-poll predicate).
3. obaapi `TripDetails` with raw-JSON pruning and the not-found sentinel.
4. HTTP handler: contract tests first (form-encoded iOS shape, JSON shape, every
   4xx, throttle behaviors), then wiring.
5. Snapshot worker loop.
6. CLI export.
7. README + `make check`.

## 10. Testing strategy

- **Handler:** table tests over form/JSON/query encodings; iOS-shaped body
  (`predicted="1"`, ms timestamps) as a pinned client-contract test; a >8 KB
  form-encoded body that must **succeed** (the JSON-only cap, §2.4); every error
  shape byte-compared; oversized-JSON 403 both by declared length and by streamed
  overflow; per-user throttle exercised through a JSON body (the §2.6 bypass);
  an over-length `user_identifier` 422s without consuming a throttle slot;
  denied requests not counted (house limiter semantics).
- **Store:** conformance suite runs against sqlite; duplicate insert returns
  `ErrDuplicate` both sequentially and under two goroutines (the race the
  controller must 422); a forced `public_identifier` collision does **not**
  surface as `ErrDuplicate`.
- **Worker:** fake obaapi; transient-vs-terminal paths; 3-tries-total cap with the
  final increment and `unavailable` in one UPDATE; a row at the cap stops matching
  the poll; injected clock; verifies a captured report is never re-fetched;
  absent-vs-zero snapshot keys (no Null Island coordinates).
- **CSV:** golden-file test incl. formula-injection cells, absent-snapshot rows
  (blank distance, blank staleness), the ms-based staleness divisor, `predicted`
  as `true`/`false`/blank, and `service_date` as a local ISO date.
- **Mutation discipline:** per the repo's memory — mutate each new assertion once
  to confirm which one fires (tests that cannot fail have shipped here before).
