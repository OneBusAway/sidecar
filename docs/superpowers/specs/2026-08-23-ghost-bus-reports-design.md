# Ghost Bus Reports — Design

**Date:** 2026-08-23
**Status:** Draft
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
shipped client.

### Out of scope

- **Rider-facing read API.** The spec forbids one; the CSV export is the read surface.
- **Admin API / SPA pages.** CLI first, exactly as surveys shipped. The domain
  package and repository are designed so an admin API is a thin layer later.
- **Retention/reaping.** Reports are agency data, kept indefinitely (spec §13).
- **OBACloud's Trips dashboard, search, and metrics.** Dashboard concerns; the CSV
  carries a `trip_report_count` column so grouping survives into a spreadsheet.

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

### 2.2 Validation (spec §8)

Required: `user_identifier`, `trip_identifier`, `service_date`,
`wait_duration_minutes`. Rules:

- `wait_duration_minutes` ∈ {5, 10, 15, 20, 30}, else a validation 422.
- `comment` ≤ 1,000 characters (runes, matching the app's character count).
- `user_latitude` ∈ [−90, 90], `user_longitude` ∈ [−180, 180]; each independently
  optional (the reference does not require them as a pair). An unparseable
  coordinate is a 422, not silently dropped (house precedent from surveys).
- All epoch-ms timestamp fields (`service_date`, `scheduled_arrival_at`,
  `predicted_arrival_at`, `prediction_last_updated_at`) parse as integers via the
  existing `params.int64` semantics; a non-integer value coerces to **null** — and
  for `service_date` then fails the presence check — never fuzzily parsed, because
  `service_date` participates in the dedupe key (spec §8).
- `predicted` is three-state: the §2.7 truthy set → true, the falsy set → false,
  absent/unrecognized → null. It is data, not push routing, so unrecognized values
  need no logging.
- Identifier-ish fields (`user_identifier`, `trip_identifier`, `route_identifier`,
  `stop_identifier`, `vehicle_identifier`) share the surveys' 255-char `maxIdentifierLen` cap; anything
  over is a validation 422 rather than a truncated store.

Validation failure → `422 {"error": "Unable to save report", "messages": […]}`
(spec §2.5 row 2). Unknown region → `404 {"error": "Couldn't find Region"}`.

### 2.3 Dedupe: insert, then map the constraint

One report per `(region, user_identifier, trip_identifier, service_date)`, enforced
by a unique index. There is **no pre-check SELECT**: the handler validates fields,
attempts the insert, and maps the unique-constraint violation to
`422 {"error": "already_reported", "messages": ["User has already reported this trip"]}`.
This collapses the reference's two duplicate paths (validation-caught and
race-caught) into one; the spec explicitly allows the `messages` text to differ
between implementations, and a `500` on the race path would make the app retry
forever. `service_date` is stored as the raw epoch-ms integer it arrived as, so the
dedupe key is deterministic.

### 2.4 Throttles (spec §2.6)

Two limiters, both `ratelimit.Limiter` (fixed-window, clockless):

- **10/hour per IP**, applied by the existing `throttleByIP` wrapper → bodyless 429.
- **20/day per `user_identifier`**, applied inside the handler immediately after
  `parseRequestParams` and before validation or storage. Because the handler parses
  query, form, and JSON into one params bag with body-first precedence, the throttle
  bucket automatically matches the identifier the report is filed under — the
  JSON-bypass hazard the spec warns about cannot arise from a separate body peek,
  because there is no separate body peek. A blank/absent `user_identifier` skips the
  per-user counter (no pooled nil bucket) and fails presence validation instead.

**Oversized-body 403.** The endpoint parses its body with an 8 KB cap (not the
shared 64 KB `requestBodyLimit`): a JSON request whose `Content-Length` declares
more than 8,192 bytes is rejected `403` before the body is read, and a body that
exceeds the cap while streaming (chunked, or a lying Content-Length) maps
`errBodyTooLarge` → `403` as well. A legitimate report is far smaller (the comment
caps at 1,000 chars); a body past the cap can only be padding meant to defeat the
counted read.

### 2.5 Public identifier

`securetoken.New()` — the same 22-char URL-safe generator survey responses use.
The written spec's §2.4 note ("the reference uses 20 hex chars") describes
OBACloud, not a wire requirement: iOS decodes `id` as an opaque string. House
precedent wins over byte-for-byte mimicry; what is mandatory is unguessability and
never exposing sequential ids.

### 2.6 Snapshot enrichment: DB-as-queue worker

Per spec §8 enrichment is asynchronous and never blocks the 201. Two candidate
shapes were considered:

- *Request-time goroutine* (closest analog to OBACloud's job queue): simple, but
  this repo has no durable queue, so a crash between 201 and capture strands the
  report `pending` forever with nothing to retry it.
- **DB-as-queue polling loop (chosen):** the report row itself is the queue entry.
  `RunSnapshotLoop` (pattern: `alarms.RunSchedulerLoop`) polls every **30 seconds**
  for reports with `snapshot_status = 'pending'`, oldest first, bounded batch:

  1. Region has no OBA base URL or no resolvable API key → `unavailable`.
  2. Call `obaapi.TripDetails` (`includeStatus`, `includeTrip`, the report's
     `service_date`).
  3. Definitive miss (404, empty entry, unparseable body) → `unavailable`.
  4. Transient failure (network error, 5xx, timeout) → increment
     `snapshot_attempts`; on the **3rd** failed attempt → `unavailable`.
     (OBACloud: `retry_on … attempts: 3` then `snapshot_unavailable!`.)
  5. Success → store the pruned snapshot JSON, `snapshot_status = 'captured'`,
     `snapshot_captured_at = now`.

  The loop is safe under at-least-once execution: re-capturing an already-captured
  report cannot happen (the poll filters on `pending`), and a crash mid-capture
  just re-runs the fetch.

**Snapshot shape** (mirrors `CaptureSnapshotJob#prune`): a single JSON document —

```json
{
  "current_time": 1756000000000,
  "status":  { "predicted": true, "vehicleId": "…", "lastUpdateTime": 0,
               "lastLocationUpdateTime": 0, "scheduleDeviation": 0, "phase": "…",
               "serviceDate": 0, "closestStop": "…", "closestStopTimeOffset": 0,
               "distanceAlongTrip": 0, "totalDistanceAlongTrip": 0,
               "lastKnownLocation": {"lat": 0, "lon": 0}, "position": {"lat": 0, "lon": 0},
               "orientation": 0, "activeTripId": "…" },
  "display": { "headsign": "…", "route_short_name": "…", "route_long_name": "…",
               "route_type": 3, "stop_name": "…", "stop_lat": 0, "stop_lon": 0 }
}
```

`status` is the trip-details entry's status block restricted to OBACloud's
`STATUS_KEYS`; `display` resolves the trip/route/stop out of the response
`references` (route id from the report's `route_identifier`, falling back to the
trip reference's `routeId`), keys absent when unresolvable. The DB stores only raw
identifiers; without `display` the CSV could never show route names or stop
coordinates.

### 2.7 obaapi addition

`Client.TripDetails(ctx, region, tripID string, serviceDateMs int64)` returning a
small pruned struct (status subset + resolved display fields), built on the go-sdk's
`TripDetails.Get` (verified present in v1.15.0 with `ServiceDate`, `IncludeStatus`,
`IncludeTrip` params and `References` in the response). Same rules as existing
calls: memoized per-region SDK clients, redacted errors, a "not found / empty
entry" sentinel distinct from transient errors so the worker can tell step 3 from
step 4. Nothing outside `internal/obaapi` sees a go-sdk type.

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

- `reported_at_local` renders in the region's configured timezone (with the known
  caveat that an unconfigured region defaults to UTC — follow-ups doc item 1).
- `trip_report_count` is a `GROUP BY (trip_identifier, service_date)` count over
  the exported set, carrying trip-instance grouping into the spreadsheet.
- `vehicle_distance_from_stop_m` is haversine(vehicle last position, snapshot stop
  coords); blank when either is absent. Small pure helper in the domain package.
- `prediction_staleness_minutes` = (scheduled_arrival − prediction_last_updated)/60,
  rounded; blank unless both present.
- Every string cell goes through the surveys' formula-injection guard (leading
  apostrophe for `=`, `+`, `-`, `@`, tab, CR): comments are unauthenticated rider
  input and GTFS names come from external feeds.
- No row cap: the CLI streams; OBACloud's 10,000-row cap protects a web download
  path this design does not have.

## 3. Data model

Migration `00007_ghost_bus_reports.sql`:

```sql
CREATE TABLE ghost_bus_reports (
  id                         INTEGER PRIMARY KEY,
  region_id                  INTEGER NOT NULL REFERENCES regions(id),
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
  snapshot_status            TEXT    NOT NULL DEFAULT 'pending',
  snapshot_json              TEXT,
  snapshot_captured_at       INTEGER,                    -- epoch seconds
  snapshot_attempts          INTEGER NOT NULL DEFAULT 0,
  created_at                 INTEGER NOT NULL            -- epoch seconds
);
CREATE UNIQUE INDEX idx_gbr_public_identifier ON ghost_bus_reports(public_identifier);
CREATE UNIQUE INDEX idx_gbr_dedupe
  ON ghost_bus_reports(region_id, user_identifier, trip_identifier, service_date);
CREATE INDEX idx_gbr_snapshot_pending ON ghost_bus_reports(snapshot_status)
  WHERE snapshot_status = 'pending';
CREATE INDEX idx_gbr_region_created ON ghost_bus_reports(region_id, created_at);
```

House rules apply: INTEGER epochs, never DATETIME (modernc text-sort trap); ms
fields stored as received; sqlc named args only, never mixed with bare `?`.

## 4. HTTP

- `POST /api/v2/regions/{regionId}/ghost_bus_reports` — registered only when the
  `GhostBus` repository dep is non-nil, like every optional service. Region segment
  accepts `{int}` and `{int}-slug`.
- Wrapped in `throttleByIP(deps.GhostBusIPLimiter)` (default 10/hour; tests inject
  tighter). Per-user limiter (`GhostBusUserLimiter`, default 20/day) checked in the
  handler per §2.4 above.
- Responses: `201 {"id": …}`; `403` oversized body; `404` unknown region;
  `422` in the two shapes of §2.2/§2.3; `429` bodyless; `500` on store failure with
  the real error logged (house precedent from surveys).
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
`RunSnapshotLoop` whenever the ghost bus repository and OBA client are configured.

## 7. Packages

| File | Responsibility |
|---|---|
| `internal/ghostbus/ghostbus.go` | Domain type, validation, `Repository`, `ErrDuplicate`/`ErrNotFound` |
| `internal/ghostbus/snapshot.go` | `RunSnapshotLoop` + capture/prune logic (§2.6) |
| `internal/ghostbus/haversine.go` | Distance helper for the CSV |
| `internal/obaapi/obaapi.go` | `TripDetails` addition (§2.7) |
| `internal/store/sqlite/ghostbus.go` + queries + migration 00007 | sqlc adapter |
| `internal/store/storetest/ghostbustest.go` | Conformance suite (dedupe race included) |
| `internal/httpapi/ghostbus.go` | Handler, throttles, 8 KB/403 guard |
| `cmd/sidecar-admin/ghostbus.go` | CSV export command |
| `cmd/sidecar/main.go`, `internal/httpapi/router.go` | Wiring |

Time is injected everywhere (`Now func() time.Time`); `time.Now` stays banned
outside `cmd/`.

## 8. Dependencies

No new Go modules. go-sdk v1.15.0 (already vendored) covers trip-details.

## 9. Build order

1. Domain package: types, validation, three-state `predicted`, haversine.
2. Migration + sqlc queries + sqlite adapter + storetest conformance (incl. the
   concurrent-duplicate race and the pending-poll ordering).
3. obaapi `TripDetails` with the not-found sentinel.
4. HTTP handler: contract tests first (form-encoded iOS shape, JSON shape, every
   4xx, throttle behaviors), then wiring.
5. Snapshot worker loop.
6. CLI export.
7. README + `make check`.

## 10. Testing strategy

- **Handler:** table tests over form/JSON/query encodings; iOS-shaped body
  (`predicted="1"`, ms timestamps) as a pinned client-contract test; every error
  shape byte-compared; oversized-JSON 403 both by declared length and by streamed
  overflow; per-user throttle exercised through a JSON body (the §2.6 bypass);
  denied requests not counted (house limiter semantics).
- **Store:** conformance suite runs against sqlite; duplicate insert returns
  `ErrDuplicate` both sequentially and under two goroutines (the race the
  controller must 422).
- **Worker:** fake obaapi; transient-vs-terminal paths; 3-attempt cap; injected
  clock; verifies a captured report is never re-fetched.
- **CSV:** golden-file test incl. formula-injection cells, absent-snapshot rows,
  staleness/distance math.
- **Mutation discipline:** per the repo's memory — mutate each new assertion once
  to confirm which one fires (tests that cannot fail have shipped here before).
