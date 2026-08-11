# Service Alerts Feed — Design

**Date:** 2026-08-11
**Status:** Approved
**Implements:** [specification.md](../../../specification/specification.md) §3, and the
`getAlertsFeed` operation in [openapi.yaml](../../../specification/openapi.yaml).

## 1. Scope

Build the GTFS-realtime service alerts feed and the authoring path that feeds it:

```
GET /api/v1/regions/{regionId}/alerts        → binary FeedMessage (application/octet-stream)
GET /api/v1/regions/{regionId}/alerts.pbtext → protobuf JSON     (text/plain)
```

Alerts are authored with `sidecar-admin`, a command-line tool that writes the database
directly. Data lives in SQLite, accessed through sqlc-generated queries behind a
repository interface that a future Postgres implementation can satisfy without touching
domain, HTTP, or CLI code.

### Out of scope

- **Alert push fan-out** (spec §4). Requires push registrations and a push transport;
  a separate slice.
- **Admin or authoring UI.** `sidecar-admin` is the only authoring surface.
- **Feed caching** (ETag / `Cache-Control`). Deferred deliberately, not overlooked.
- Every other sidecar service (alarms, Live Activities, surveys, ghost bus reports,
  weather, vehicle search, donations).

## 2. Decisions

### 2.1 Regions come from a configurable directory URL

The sidecar fetches a `regions-v3.json`-shaped document from a configured URL at boot
and every 60 minutes, upserting into a `regions` table.

- A failed **refresh** logs and keeps serving the last known good rows.
- A failed **first fetch** boots on whatever the table already holds.
- The feed never goes dark because the directory is unreachable, and `sidecar-admin`
  reads regions offline.

The real directory (verified 2026-08-11 against
`https://regions.onebusaway.org/regions-v3.json`) supplies `id`, `regionName`,
`obaBaseUrl`, `sidecarBaseUrl`, `language`, and `active`. It supplies **no timezone and
no GTFS agency id**; both are managed locally (§2.2, §2.4).

**Region id 0 is real** — Tampa Bay. Any `if regionID == 0 { … not found }` shortcut is
a bug. Absence is signalled by an explicit lookup miss, never by a zero value.

### 2.2 The `regions` table has two kinds of columns

| Directory-sourced — refresh overwrites | Locally-managed — refresh must **not** touch |
|---|---|
| `region_name`, `oba_base_url`, `sidecar_base_url`, `language`, `active` | `default_agency_id`, `timezone` |

The sync is a **partial upsert**: `ON CONFLICT (id) DO UPDATE SET` names only
directory-sourced columns. A full-row upsert would wipe `default_agency_id` on the first
hourly refresh, after which every alert emits an empty `agency_id`.

### 2.3 Every timestamp is epoch seconds in an INTEGER column

Never `DATETIME`, never `TEXT`. This is a schema invariant.

**Why (measured, not assumed).** With a `DATETIME` column, `modernc.org/sqlite` writes
Go's `time.Time.String()` into the cell — `"2026-08-15 14:00:00 -0700 PDT"` — and
`ORDER BY` then sorts that *text* rather than the instant. Demonstrated: an alert at
2026-08-15 14:00 −07:00 (21:00 UTC) sorted as **older** than one at 16:00 UTC, inverting
spec §3's "order by start time descending". The same rows stored as `INTEGER` epoch
seconds sorted correctly, and round-tripped identically under `TZ=UTC` and
`TZ=Asia/Kathmandu`.

The bug is invisible in a single-timezone deployment. It appears when a second region in
another zone is added, or at a DST boundary when the stored offset changes.

Consequences:

- **`time.Local` is banned below the entrypoints.** `BuildFeed` takes an injected `now`;
  only `main` and CLI argument parsing consult the wall clock.
- **`--start` / `--end` require an explicit UTC offset** (RFC 3339). A naive datetime is
  rejected with an error naming the region's configured timezone — never silently
  interpreted in the server's local zone.
- **`start + 8h` is absolute arithmetic on instants**, so DST cannot affect it. Comment
  this at the call site so it is not "fixed" into wall-clock math later.
- **A `make test-tz` target runs the suite twice**, `TZ=UTC` and `TZ=Asia/Kathmandu`
  (+05:45), and is added to `make check`. A 45-minute offset breaks anything that leaked
  local time. (This repo has no CI workflow yet; wiring one up is a separate task, and
  the target is what it would call.)

### 2.4 `agency_id` resolves at author time

The `EntitySelector` needs an `agency_id` the region's OBA server uses, and a region such
as Puget Sound serves several agencies. `alert create` takes `--agency-id`, falling back
to the region's `default_agency_id`, and stores the **resolved** value on the alert row.

- `BuildFeed` stays a pure function of alerts alone — no region lookup during render.
- Changing a region's default later does not retroactively rewrite published alerts.
- If neither is available, `create` **fails**. Emitting an `EntitySelector` with an empty
  `agency_id` would be silently wrong in riders' apps.

### 2.5 Alerts have an explicit draft → published lifecycle

`create` produces a draft. `publish` makes it visible to the feed; `unpublish` withdraws
it without deleting. Spec §3 requires drafts be invisible to the feed; an explicit
transition makes that filter testable from both sides.

### 2.6 Translations carry the hash of the English they were made from

`alert_translations` stores `source_sha256`, the SHA-256 of the English source text at
the moment the translation was recorded. At render, a translation is emitted only when
that hash equals the SHA-256 of the *current* English text for the same field. This is
spec §3's requirement that stale translations be withheld so riders fall back to accurate
English.

Language tags are normalised to lowercase in Go before storage — never with SQL
collation, which differs across engines. `url` is English-only.

### 2.7 Postgres portability: identical types, one interface, one conformance suite

Generating the same schema for both engines with sqlc produces **structurally identical**
models and method signatures:

```
SQLITE                          POSTGRES
StartTime int64                 StartTime int64
EndTime   sql.NullInt64         EndTime   sql.NullInt64
Published bool                  Published bool
CreatedAt int64                 CreatedAt int64
```

That alignment is *caused by* §2.3. Had the schema used `DATETIME` / `TIMESTAMPTZ`, the
two engines would yield `time.Time` versus `pgtype.Timestamptz`. Two conditions preserve
it:

1. Epoch-integer storage for all timestamps.
2. `sql_package: "database/sql"` for both engines — **not** pgx, whose `pgtype.Text`
   diverges from `sql.NullString`.

Structural identity is not type identity: `sqlite.Alert` and `postgres.Alert` remain
distinct Go types. So the boundary is a repository interface over domain types, and:

- **Per-engine directories from day one** — `internal/store/sqlite/{migrations,queries}/`.
  Adding `postgres/` is additive rather than a restructure.
- **One shared conformance suite** in `internal/store/storetest`, parameterised over
  implementations. It runs against SQLite today; when Postgres arrives the same tests
  prove behavioural equivalence. The interface promises portability — this suite
  delivers it.
- **Portable SQL only**: `ON CONFLICT … DO UPDATE` (both engines), no `INSERT OR REPLACE`,
  no rowid tricks, no `strftime` logic. Hashing and tag normalisation happen in Go.
- **No `ORDER BY` on a nullable column** — SQLite and Postgres disagree on NULL placement,
  and SQLite only gained `NULLS FIRST/LAST` in 3.30.
- **`ORDER BY start_time DESC, id DESC`** — never `start_time` alone. Ties are otherwise
  ordered arbitrarily and differently per engine, so the 20-row cap would silently select
  different alerts.

### 2.8 `sidecar-admin` writes the database directly

No IPC. The CLI needs no running server, shares one store implementation with the server,
and its command layer is testable as a library. SQLite runs in WAL mode with a 5-second
busy timeout, so concurrent server reads and CLI writes coexist.

## 3. Data model

Timestamps are epoch seconds (§2.3). Booleans are declared `BOOLEAN`; sqlc maps them to
Go `bool` on both engines.

```sql
CREATE TABLE regions (
  id                INTEGER PRIMARY KEY,        -- public region id; 0 is valid
  region_name       TEXT    NOT NULL,
  oba_base_url      TEXT    NOT NULL,
  sidecar_base_url  TEXT,
  language          TEXT,
  active            BOOLEAN NOT NULL DEFAULT TRUE,
  -- locally managed; the directory refresh must not overwrite these
  default_agency_id TEXT,
  timezone          TEXT    NOT NULL DEFAULT 'UTC',
  synced_at         INTEGER NOT NULL,
  created_at        INTEGER NOT NULL,
  updated_at        INTEGER NOT NULL
);

CREATE TABLE alerts (
  id               INTEGER PRIMARY KEY AUTOINCREMENT,
  region_id        INTEGER NOT NULL REFERENCES regions(id) ON DELETE CASCADE,
  agency_id        TEXT    NOT NULL,            -- resolved at author time (§2.4)
  header_text      TEXT    NOT NULL,            -- English
  description_text TEXT    NOT NULL DEFAULT '', -- English
  url              TEXT,                        -- English only
  cause            TEXT    NOT NULL DEFAULT 'UNKNOWN_CAUSE',
  effect           TEXT    NOT NULL DEFAULT 'UNKNOWN_EFFECT',
  severity_level   TEXT    NOT NULL DEFAULT 'UNKNOWN_SEVERITY',
  start_time       INTEGER NOT NULL,
  end_time         INTEGER,                     -- NULL → start + 8h at render
  published        BOOLEAN NOT NULL DEFAULT FALSE,
  is_test          BOOLEAN NOT NULL DEFAULT FALSE,
  created_at       INTEGER NOT NULL,
  updated_at       INTEGER NOT NULL
);

CREATE INDEX alerts_feed_idx ON alerts (region_id, published, start_time DESC, id DESC);

CREATE TABLE alert_translations (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  alert_id      INTEGER NOT NULL REFERENCES alerts(id) ON DELETE CASCADE,
  language      TEXT    NOT NULL CHECK (language <> 'en'),  -- lowercase BCP-47
  field         TEXT    NOT NULL CHECK (field IN ('header', 'description')),
  text          TEXT    NOT NULL,
  source_sha256 TEXT    NOT NULL,               -- §2.6
  created_at    INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL,
  UNIQUE (alert_id, language, field)
);
```

English is not a row in `alert_translations` — it lives on the alert itself and is always
emitted first, so a `language` of `'en'` is rejected by the schema rather than by
convention.

`AUTOINCREMENT` on `alerts` is load-bearing rather than stylistic: feed entity ids are
`Alert_<id>` and must be stable per alert. Plain SQLite rowids are reused after deletion,
which would let a new alert inherit a deleted alert's entity id and collide with client
caches. Postgres `GENERATED ALWAYS AS IDENTITY` gives the same guarantee.

Migrations are goose SQL files embedded via `embed.FS`, under
`internal/store/sqlite/migrations/`.

## 4. Feed contract

### 4.1 Query split

SQL selects; Go renders.

- **SQL** filters by region, `published = TRUE`, and the test flag; orders
  `start_time DESC, id DESC`; caps at 20.
- **`BuildFeed`** renders those rows to protobuf. It applies no filtering, ordering, or
  capping.

Translations load in one additional query scoped by a subquery repeating the feed
predicate — two queries total, no N+1, no dependency on `sqlc.slice()` support.

The feed does **not** filter by active window. Spec §13 states apps hide out-of-window
alerts client-side using `active_period`, so all 20 newest published alerts are returned
regardless of whether they are currently active.

### 4.2 Builder

```go
func BuildFeed(alerts []Alert, opts FeedOptions) *gtfs.FeedMessage
// FeedOptions{ Now time.Time; DefaultDuration time.Duration /* 8h */ }
```

Rendering rules:

| Field | Value |
|---|---|
| `header.gtfs_realtime_version` | `"1.0"` |
| `header.incrementality` | `FULL_DATASET` |
| `header.timestamp` | `opts.Now` as epoch seconds |
| `entity.id` | `Alert_<id>` — stable per alert across requests |
| `active_period[0].start` | `start_time` |
| `active_period[0].end` | `end_time`, or `start_time + 8h` when NULL |
| `informed_entity[0].agency_id` | the alert's resolved `agency_id` |
| `cause` / `effect` / `severity_level` | stored enum name → protobuf enum |
| `header_text` / `description_text` | English first, then fresh translations sorted by tag |
| `url` | English only |

Enum names are stored as TEXT (`"CONSTRUCTION"`, not `6`) — portable, legible in
`alert list`, and validated at author time. Absent values use the GTFS-RT defaults
`UNKNOWN_CAUSE`, `UNKNOWN_EFFECT`, `UNKNOWN_SEVERITY`.

Translations sort by language tag rather than following map iteration order, so wire
output is byte-stable across runs.

An empty result set still yields a valid `FeedMessage` with a populated header and no
entities — spec §15 requires the endpoint to conform even when it always returns empty.

### 4.3 HTTP

```
GET /api/v1/regions/{regionId}/alerts          → 200 application/octet-stream
GET /api/v1/regions/{regionId}/alerts.pbtext   → 200 text/plain
                                                 404 application/json
```

- **Region segment**: parse the leading integer and ignore any suffix, per spec §2.4 —
  `1-puget-sound` resolves to region 1. Shipped clients replay server-generated
  id-prefixed slugs verbatim.
- **Unknown region**: `404 {"error": "Couldn't find Region"}`.
- **`?test=`**: test alerts are included when the parameter is present with **any
  non-blank value**. Note `?test=0` therefore *includes* them — this follows spec §3's
  "any non-blank value" and a conventional boolean parse would get it wrong.
- Routing uses the stdlib `net/http.ServeMux` path patterns available since Go 1.22.

## 5. Packages

```
cmd/sidecar/            HTTP server binary
cmd/sidecar-admin/      CLI binary
internal/
  alerts/               Alert, Translation, BuildFeed, AlertRepository   ← pure, no I/O
  regions/              Region, RegionRepository, directory client + syncer
  store/
    sqlite/             migrations/ queries/ gen/ + repository implementation
    storetest/          shared conformance suite (§2.7)
  httpapi/              routing, handlers, error shapes
  config/               flags and environment
```

`alerts` and `httpapi` never import generated code; the sqlite adapter is the only
package that does.

## 6. `sidecar-admin`

```
region  list
        set --id N [--agency-id ID] [--timezone TZ]
        sync                                    # force a directory fetch

alert   create --region N --header TEXT --start RFC3339
               [--description TEXT] [--url URL] [--end RFC3339] [--agency-id ID]
               [--cause C] [--effect E] [--severity S] [--test]
        list [--region N] [--all]
        show ID
        edit ID [--header TEXT] [--description TEXT] [--url URL]
                [--start RFC3339] [--end RFC3339] [--agency-id ID]
                [--cause C] [--effect E] [--severity S] [--test | --no-test]
        publish ID | unpublish ID | delete ID
        translate ID --language es [--header TEXT] [--description TEXT]

migrate up | status
```

`region set` updates an existing row only; it never creates one. Regions come from the
directory (§2.1), so an unknown `--id` is an error rather than an implicit insert.

Every `alert edit` flag is optional and patches only what is passed — distinct from
`create`, where `--region`, `--header`, and `--start` are required. Omitting a flag
leaves the stored value untouched; clearing an optional field uses an explicit empty
value (`--url ""`).

The database path comes from `--db` or `SIDECAR_DB`, defaulting to `./sidecar.db`.
`alert list` renders times in the region's configured timezone with the zone name, plus
UTC.

Both binaries follow the existing `run(io.Writer, []string) error` seam in
`cmd/sidecar/main.go`, so command behaviour is testable without a subprocess.

## 7. Error handling

| Condition | Behaviour |
|---|---|
| Unknown region (HTTP) | `404 {"error": "Couldn't find Region"}` |
| Directory refresh fails | Log; keep serving the last known good rows |
| Directory fetch fails at boot | Log; serve whatever the table holds |
| Naive `--start` / `--end` | CLI error naming the region's timezone; no write |
| No `--agency-id` and no region default | CLI error; no write (§2.4) |
| Unknown enum name | CLI error listing valid values; no write |
| Store failure during render | `500`, empty body; error logged with the region id |

## 8. Testing strategy

Test-driven throughout, built in dependency order so each layer is fully tested before
the next exists.

**1 — Feed builder.** Pure; no database, no HTTP. The richest logic with the cheapest
tests, so it comes first: entity id format, `+8h` fallback versus explicit end, enum
defaults, English-first ordering, stale translation withheld and fresh translation
emitted, empty input producing a valid header-only `FeedMessage`.

**2 — Store and migrations.** Against a temporary SQLite file with the real migrations.
Written as the shared conformance suite from the start: CRUD, publish/unpublish
visibility, test filtering, ordering including the `id DESC` tie-break, the 20-row cap,
and the partial upsert preserving `default_agency_id` and `timezone`.

**3 — Region directory.** An `httptest` server returning a fixture captured from the real
directory: parsing, upsert, refresh-preserves-local-columns, and fetch-failure-serves-stale.

**4 — HTTP handlers.** `httptest` against a real store: both encodings and their content
types, `?test=` handling including the `?test=0` case, the 404 body, and the
`1-puget-sound` slug segment.

**5 — CLI.** Temporary database via the `run` seam: a create → publish → appears-in-feed
round trip, and rejection of a naive `--start`.

### Wire assertions never use golden files

`protojson` deliberately randomises whitespace between fields, and protobuf binary field
order is not guaranteed stable either. Every wire assertion unmarshals and compares with
`protocmp.Transform()` from `google.golang.org/protobuf/testing/protocmp`. A golden-string
comparison passes locally and flakes in CI.

### Commands

Two new Makefile targets:

- `make test-tz` — runs `go test ./...` under `TZ=UTC` and again under
  `TZ=Asia/Kathmandu`, and is folded into `make check`.
- `make generate` — runs `sqlc generate`, and a `make generate-check` verifying the
  committed output is current, so a hand-edited or stale generated file fails the build.

## 9. Dependencies

All verified available on 2026-08-11:

| Module | Version | Purpose |
|---|---|---|
| `github.com/MobilityData/gtfs-realtime-bindings/golang/gtfs` | v1.0.0 | GTFS-RT protobuf types |
| `google.golang.org/protobuf` | v1.36.12 | Marshalling, `protojson`, `protocmp` |
| `modernc.org/sqlite` | v1.56.0 | Pure-Go SQLite driver — no cgo, simple CI |
| `github.com/pressly/goose/v3` | v3.27.3 | Migrations |
| `github.com/google/go-cmp` | latest | Test comparison |

`sqlc` is a build-time tool, not a module dependency; generated code is committed and
kept honest by `make generate-check` (§8).
The bindings module pins protobuf v1.26.0 in its own `go.mod`; upgrading to v1.36.12 was
verified to build and marshal correctly in both encodings.
