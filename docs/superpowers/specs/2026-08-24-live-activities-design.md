# iOS Live Activities — Design

**Date:** 2026-08-24
**Status:** Reviewed
**Implements:** [specification.md](../../../specification/specification.md) §6 (with §2.4
tokens, §2.7 `apns_sandbox`, §6.5 feedback, §12 background processing), and the
`createLiveActivity` / `deleteLiveActivity` operations plus the `liveActivityUpdate`
webhook in [openapi.yaml](../../../specification/openapi.yaml).

## 1. Scope

The last required item on the spec's conformance checklist (§15 item 5): a stateful
Lock Screen subscription the sidecar updates once a minute via ActivityKit pushes.

```
POST   /api/v2/regions/{regionId}/live_activities          → 201 {"url": "…/live_activities/<token>"}
DELETE /api/v2/regions/{regionId}/live_activities/{token}  → 204 | 404
```

plus a background updater following the alarm-scheduler pattern, a Live Activity push
type on the gorush transport, a new OBA client call, and a Live Activity branch in the
gorush feedback webhook.

The contract was verified against the reference implementation (`../obacloud`:
`LiveActivityContentState`, `LiveActivityChecker`, `LiveActivityRegistrar`,
`LiveActivity`, `Api::V2::LiveActivitiesController`, `Gorush#send_live_activity`,
`Webhooks::GorushController`) and the iOS client (`../ios`:
`TripAttributes.ContentState`, `ObacoAPIService.postLiveActivity/deleteLiveActivity`,
`OBAKitTests/fixtures/live_activity_content_state.json`). Android has no Live
Activities; iOS is the only client. Deliberate divergences from the reference are called
out inline.

### Out of scope

- **Admin API / SPA / CLI visibility.** Rider-facing only for this cut; alarms have no
  admin surface either. The repository interface is designed so a list endpoint is a
  thin layer later.
- **Per-push delivery accounting** (OBACloud's `PushDelivery` rows and `notif_id`
  correlation). The spec calls that "internal and not part of this contract". The
  feedback webhook correlates by token alone, exactly as it already does for
  registrations.
- **Generalizing the alarm scheduler.** The two loops differ in nearly every step
  (one-shot vs stateful, different failure semantics, per-stop caching); the shared
  scaffolding (region cache, errgroup fan-out) is duplicated deliberately.

## 2. Decisions

### 2.1 Registration is an upsert on `(region, activity_id)`

ActivityKit rotates push tokens over an activity's lifetime and iOS re-POSTs the same
`activity_id` with each new token (§6.1). `Repository.Upsert`:

- **New row:** mint a §2.4 secure token (`securetoken.New`, 22-char URL-safe base64,
  same generator as alarms), set `expires_at = now + 8h`, insert.
- **Existing row:** keep `token` and `expires_at`; rewrite `push_token`, `stop_id`,
  `route_short_name`, `trip_headsign`, the four optional trip fields, and
  `apns_sandbox`, and stamp `updated_at = now`. `consecutive_failures`,
  `last_content_state`, and `last_pushed_at` are preserved: a token rotation is not a
  reset of the subscription's health. The `apns_sandbox` re-read is deliberate and
  normative — a rotated token comes from the same build that sent the original. The
  reference logs a sandbox flip on re-registration; this port does not (the shipped
  client stamps the flag from `#if DEBUG`, fixed per build, so the condition cannot
  occur), which keeps `Upsert`'s return type a plain `LiveActivity`.
- **Race:** two concurrent first registrations both miss the SELECT; the second INSERT
  violates `UNIQUE (region_id, activity_id)`. The adapter maps that to
  `ErrDuplicate`; the handler retries `Upsert` **once**, which now takes the update
  path. Same shape as the V1 alarm dedupe race.
- The returned URL is the same for every re-registration of one activity: §6.1's upsert
  semantics imply one address per activity, and iOS's `LiveActivityRegistry`
  (`register`/`store(deleteURL:)`, and the DELETE it issues when
  `confirm()` reports the activity gone) must hit the same row whichever registration
  it last saw.

The response is `201` on both insert and update (the reference renders `:created`
unconditionally; iOS ignores the status and decodes `url`).

### 2.2 Content state is a typed struct with a pinned fixture

`ContentState{Arrivals []ArrivalInfo}` / `ArrivalInfo{DepartureTime, ScheduleStatus,
ScheduleDeviation, IsArrival}` with explicit `json` tags (`departure_time`,
`schedule_status`, `schedule_deviation`, `is_arrival`). `Arrivals` marshals as `[]`,
never `null`, when empty. The iOS fixture is copied verbatim into
`internal/liveactivities/testdata/live_activity_content_state.json` and a test
decodes it, re-encodes it, and compares against a canonical re-encoding of the
original, so any drift from the app's decoder is a test failure here, not a frozen
Lock Screen in the field.

**No timestamp or other always-changing field may ever be added** (§6.2): change
detection compares consecutive states.

### 2.3 Building the state (port of `LiveActivityContentState`)

`BuildContentState(entries []obaapi.StopArrival, routeShortName, tripHeadsign string,
now time.Time) ContentState` is a pure function:

1. Keep entries whose `RouteShortName == routeShortName && TripHeadsign == tripHeadsign`
   (exact, case-sensitive string equality). `StopArrival.RouteShortName` and
   `TripHeadsign` are resolved by `obaapi` with the same references fallback the app
   uses (`ArrivalDeparture` on iOS: entry value, else `references.routes[].shortName`
   / `references.trips[].tripHeadsign`) and that this repo's `ArrivalAndDeparture`
   already applies. **Deliberate deviation from the Rails port**, which compares the
   raw entry field: a feed that omits `routeShortName` on the entry would otherwise
   produce an empty state, three failures, and an ended activity while the app's own
   refresh shows arrivals.
2. **Collapse duplicate vehicle reports.** Group by visit identity
   `(StopID, TripID, RouteID, ServiceDate, StopSequence)`; in each group keep the entry
   with the greatest `LastUpdateTime`, ties broken by lowest response index. An entry
   whose `HasIdentity` is false (any component absent in the upstream JSON) is its own
   group and is never collapsed. Never group by `TripID` alone — loop routes and
   cross-midnight recurrences are distinct buses.
3. `IsArrival = StopSequence != 0`. Use the arrival-time pair when `IsArrival` and
   either `ScheduledArrivalTime > 0 || PredictedArrivalTime > 0`; otherwise the
   departure pair (some feeds omit arrival times at non-first stops).
4. `predicted := Predicted && predictedX > 0`. Time is `predictedX` if predicted else
   `scheduledX`, converted ms → s by integer division. `Deviation` is
   `(predictedX - scheduledX)/1000` seconds when predicted, else 0.
5. Drop entries whose chosen time (seconds) `< now.Unix()`.
6. Status: not predicted → `unknown`. Else with `m = deviation/60.0`: `m < -1.5` →
   `early`; `m < 1.5` → `on_time`; else `delayed`. Exactly −1.5 is on_time, exactly
   +1.5 is delayed (half-open on the late side, mirroring `ArrivalDeparture`).
7. Sort ascending by `DepartureTime` (stable; equal times keep response order), take
   the first 3.

### 2.4 Identity presence comes from the OBA client

The Go SDK's typed entry struct zero-fills absent fields, so `StopSequence == 0` is
ambiguous between "first stop" and "absent". The SDK exposes per-field JSON metadata
(`entry.JSON.StopSequence` etc., type `apijson.Field` with `IsNull()` — true for
missing or null — and `IsInvalid()` — present but the wrong type). `obaapi` reports
presence explicitly: `StopArrival.HasIdentity = !IsNull() && !IsInvalid()` for each of
`stopId`, `tripId`, `routeId`, `serviceDate`, `stopSequence`. Tests cover a missing
`stopSequence` and a non-numeric `serviceDate`.

### 2.4a OBA client call

```go
type StopArrivalsQuery struct {
    StopID        string
    MinutesBefore int64
    MinutesAfter  int64
}

type StopArrival struct {
    StopID, TripID, RouteID string
    ServiceDate             int64 // epoch ms
    StopSequence            int64
    HasIdentity             bool  // all five fields above present and well-typed upstream (2.4)
    LastUpdateTime          int64 // epoch ms; 0 when absent
    RouteShortName          string // entry value, else references fallback (2.3)
    TripHeadsign            string // entry value, else references fallback (2.3)
    Predicted               bool
    ScheduledArrivalTime, PredictedArrivalTime     int64 // epoch ms; 0 = absent
    ScheduledDepartureTime, PredictedDepartureTime int64 // epoch ms; 0 = absent
}

// On obaapi.Client:
ArrivalsAndDeparturesForStop(ctx context.Context, region regions.Region, q StopArrivalsQuery) ([]StopArrival, error)
```

Error mapping as `ArrivalAndDeparture`: upstream 404, a `null` body, or a nil entry →
`ErrNotFound`; no API key → `ErrNotConfigured`; anything else a redacted transient
error. An entry list that is present but empty is **not** an error — it returns
`[]StopArrival{}` and the updater's empty-state rule (2.5 step 4) handles it. Adding
the method to the `obaapi.Client` interface means the existing fakes in
`internal/httpapi/alarms_api_test.go`, `internal/httpapi/vehicles_test.go`, and
`internal/ghostbus/snapshot_test.go` gain a stub; the updater itself depends only on
its own one-method `ArrivalsSource` interface (`alarms.DepartureSource` pattern), with
`LookbackMinutes`/`LookaheadMinutes` supplied by the updater in the query.

### 2.5 Update cycle (port of `LiveActivityChecker`)

`Updater.CheckAll` runs one cycle over `Repository.List()`; `RunLoop` ticks it every
minute. Per row, in order:

1. `now.After(expires_at)` → `end(expired)`.
2. Region: resolved once per region per cycle. `regions.ErrNotFound` → count a
   failure (the activity can never resolve). Any other store error → Warn and skip:
   a store hiccup is a fact about the database, not this row.
3. Fetch arrivals through the per-stop cache (2.6). **Any** fetch error — `ErrNotFound`,
   `ErrNotConfigured`, transient network — counts a failure. This differs from the
   alarm scheduler on purpose: §6.3 step 2 says "On OBA/network error, count a
   failure", because a Live Activity that cannot be updated is worthless to the rider
   and three consecutive minutes of failure is the spec's cutoff. The comment in code
   cites §6.3 so a future reader doesn't "fix" it to match alarms.
4. Build the state. Empty `Arrivals` → count a failure and stop (night headways and
   feed gaps produce valid-but-empty responses on healthy subscriptions). Non-empty and
   `consecutive_failures > 0` → `ResetFailures`.
5. Push an `update` when `Changed(last, state)` or a keepalive is due
   (`last_pushed_at` nil, or `now - last_pushed_at >= 55s`; the reference uses a strict
   `>`, this port's `>=` is a deliberate boundary choice — the intent is to push on
   every cycle). `stale-date = now + 10m`. The push `timestamp` is
   `max(now, last_pushed_at + 1s)`: APNs drops a Live Activity push whose timestamp
   does not advance, and the guard makes that impossible even for an update and an
   end in the same second.
   On send error: Error log, leave the row untouched; the next cycle retries. On
   success: `RecordPush(id, state, now)` — `last_pushed_at` is stamped *after* the
   send returns, which is why the keepalive threshold sits under the 60s cadence.
6. Store-only mode (`Sender == nil`): skip the send, still run steps 1–4 so rows
   expire and reap; nothing is recorded as pushed.

`countFailure` increments via `RecordFailure` and, when the returned streak reaches 3,
calls `end(reason)`.

`end(reason)`: if a sender is configured, send a best-effort `end` push carrying the
last stored content state (`{"arrivals":[]}` if none) with `dismissal-date = now +
15m`; log any failure at Warn. Then **always** `DeleteByID`. Deleting regardless of
push outcome is the §6.4 requirement — a dead token must not keep the row being
re-checked forever.

Concurrency: errgroup limit 8, same as alarms. `CheckAll` never returns an error; every
failure is logged with `region_id` and never with the push token.

### 2.6 Per-stop upstream cache

N subscriptions on one stop must cost one upstream request per cycle (§6.3). The
updater owns a `*cache.Cache[[]obaapi.StopArrival]` with TTL 55s, keyed
`"<region_id>/<stop_id>"`, `maxEntries` 1024, and fetch budget
`StopFetchBudget = 6s` (above `obaapi`'s 4s per-request timeout with no retries). `cache.Get` deduplicates concurrent misses, so eight
goroutines checking one stop share a single upstream call. Errors are not cached —
the next subscription on that stop retries — which matches the reference
(`Rails.cache.fetch` does not store on raise). The cache's `now` is the updater's
injected `Now`.

### 2.7 Push transport

A second, narrow interface in `internal/push`, leaving `Sender`/`Notification` untouched
so the alarm scheduler's dependency stays minimal:

```go
type LiveActivityPush struct {
    Token         string
    Sandbox       bool
    Event         string    // "update" | "end"
    ContentState  any       // the updater passes liveactivities.ContentState (push cannot import it: cycle)
    Timestamp     time.Time // epoch seconds on the wire; must advance every push
    StaleDate     time.Time // zero = omitted (end pushes)
    DismissalDate time.Time // zero = omitted (update pushes)
}

type LiveActivitySender interface {
    SendLiveActivity(ctx context.Context, p LiveActivityPush) error
}
```

`*Gorush` implements it. Wire body (one-element `notifications` batch, same as `Send`):

```json
{"tokens":["<token>"],"platform":1,"push_type":"liveactivity","priority":"high",
 "topic":"<apnsTopic>.push-type.liveactivity","development":true,
 "event":"update","content-state":{"arrivals":[…]},"timestamp":1767980400,
 "stale-date":1767981000}
```

- `topic` is **derived** from the existing `apnsTopic` (the app bundle id) by appending
  `.push-type.liveactivity`. gorush does not derive it. No new configuration. An empty
  `apnsTopic` makes `SendLiveActivity` return an error **without sending** (unlike
  `Send`, which omits the field): a Live Activity push cannot succeed without a topic,
  and sending `.push-type.liveactivity` bare would bounce `BadTopic` every minute for
  eight hours. The updater logs it like any send failure; main's boot warning already
  covers the missing topic.
- `priority: "high"` → APNs priority 10, required (§6.6; verified on-device by the
  reference: at 5 an idle phone holds every push).
- The three date keys and `content-state` are **hyphenated**; gorush's unmarshaller
  silently drops snake_case variants and the activity never updates.
- No `title`/`message`: a Live Activity push has no alert.
- Same 10s timeout and same never-log-the-response-body rule as `Send` (bodies echo
  tokens). `Timestamp` is required; a zero value is a programming error and returns
  an error without sending.

### 2.8 Feedback

`feedbackHandler.receive` already deletes push registrations for terminal reasons,
but the router registers `POST /webhooks/gorush` only when `Deps.PushRegs` is set and
the handler dereferences `PushRegs` unconditionally. Both change: the webhook is
registered when **either** `PushRegs` or `LiveActivities` is non-nil, and each delete
runs only when its repository is set. For a terminal reason the handler deletes from
every configured table for that token — a token that bounced `Unregistered` is dead
whichever table holds it, and a token never appears in both (ActivityKit tokens are
not device alert tokens). No end push is sent (§6.4: the token is dead). The existing
store-error path (500 so gorush retries) applies to both deletes. A router test covers
the `LiveActivities`-only configuration.

### 2.9 Time and timezones

Every instant is `time.Time` in the domain and epoch seconds in SQLite. `expires_at`,
`last_pushed_at`, `stale-date`, `dismissal-date`, and the keepalive comparison are
absolute-instant arithmetic; nothing consults a location. `service_date` stays epoch
milliseconds because it is OBA data passed through, exactly as alarms store it.

## 3. Data model

Migration `internal/store/sqlite/migrations/00008_live_activities.sql`:

```sql
CREATE TABLE live_activities (
  id                   INTEGER PRIMARY KEY AUTOINCREMENT,
  region_id            INTEGER NOT NULL REFERENCES regions(id) ON DELETE CASCADE,
  token                TEXT    NOT NULL UNIQUE,   -- public address (spec 2.4)
  activity_id          TEXT    NOT NULL,          -- ActivityKit activity id
  push_token           TEXT    NOT NULL,          -- ActivityKit push token (not the alert token)
  apns_sandbox         BOOLEAN NOT NULL DEFAULT FALSE,
  stop_id              TEXT    NOT NULL,
  route_short_name     TEXT    NOT NULL,
  trip_headsign        TEXT    NOT NULL,
  trip_id              TEXT    NOT NULL DEFAULT '',
  service_date         INTEGER NOT NULL DEFAULT 0, -- epoch ms; 0 = omitted
  vehicle_id           TEXT    NOT NULL DEFAULT '',
  stop_sequence        INTEGER,                    -- NULL = omitted; 0 is a real value
  last_content_state   TEXT    NOT NULL DEFAULT '{"arrivals":[]}',
  last_pushed_at       INTEGER,                    -- epoch s; NULL = never pushed
  consecutive_failures INTEGER NOT NULL DEFAULT 0,
  expires_at           INTEGER NOT NULL,           -- epoch s
  created_at           INTEGER NOT NULL,
  updated_at           INTEGER NOT NULL
);
CREATE UNIQUE INDEX live_activities_activity_idx ON live_activities (region_id, activity_id);
CREATE INDEX live_activities_push_token_idx ON live_activities (push_token);
```

`last_content_state` is the canonical `json.Marshal` of `ContentState`; the adapter
unmarshals it on read and the domain compares structs, never strings. An unparseable
cell is treated as `{"arrivals":[]}` with a Warn log — one bad row must not fail
`List` for every subscription — and the next successful push overwrites it.

### Repository

```go
type Repository interface {
    Upsert(ctx context.Context, in NewLiveActivity, now time.Time) (LiveActivity, error) // ErrDuplicate on the first-registration race
    Delete(ctx context.Context, regionID int64, token string) error                     // ErrNotFound; 204 contract
    DeleteByID(ctx context.Context, id int64) error
    DeleteByPushToken(ctx context.Context, pushToken string) (int64, error)             // feedback; rows deleted
    List(ctx context.Context) ([]LiveActivity, error)                                   // updater sweep, all regions
    RecordFailure(ctx context.Context, id int64) (int64, error)                         // ++consecutive_failures, returns streak
    ResetFailures(ctx context.Context, id int64) error
    RecordPush(ctx context.Context, id int64, state ContentState, pushedAt time.Time) error
}
```

`NewLiveActivity` carries `RegionID, Token, ExpiresAt, ActivityID, PushToken,
APNSSandbox, StopID, RouteShortName, TripHeadsign, TripID, ServiceDate, VehicleID,
StopSequence *int64`; `Token` and `ExpiresAt` are used only on insert. The update
path touches only the columns listed in 2.1 plus `updated_at`.

Queries in `internal/store/sqlite/queries/liveactivities.sql`, generated with
`make generate`. No `sqlc.arg()`/`?` mixing.

## 4. HTTP

`Deps.LiveActivities liveactivities.Repository`; nil leaves both routes unregistered.
When set, `Regions` and `Now` are required (panic at construction, like alarms). Both
handlers resolve the region with `resolveRegion`, so the §2.4 id-prefixed slug
(`1-puget-sound`) is accepted on DELETE exactly as for alarms (tested).

**Rate limit (sidecar-specific addition).** §2.6 lists no limit and the reference has
none, but every distinct `(region, stop_id)` a registration names costs one upstream
OBA request per minute for eight hours — far more amplification per row than an alarm.
POST is throttled at 30/minute per TCP peer via `Deps.LiveActivityLimiter`
(`ratelimit.New(30, time.Minute)` default, injectable like `PushLimiter`), returning
`429`; DELETE is not throttled. Documented in README as a divergence from the
reference.

**POST** — `parseRequestParams` (form or JSON, `requestBodyLimit`). Validation, all
collected before responding, `422 {"error":"Unable to register live activity",
"messages":[…]}`:

| Field | Rule | Message |
|---|---|---|
| `activity_id` | required, non-blank | `Activity can't be blank` |
| `push_token` | required, non-blank, `len <= maxTokenLen` (the cap is a sidecar addition — the reference only checks presence — applied for the same reason as registrations: a junk token is stored and pushed to for eight hours) | `Push token can't be blank` / `Push token is too long (maximum is 4096 characters)` |
| `stop_id` | required | `Stop can't be blank` |
| `route_short_name` | required | `Route short name can't be blank` |
| `trip_headsign` | required | `Trip headsign can't be blank` |

`apns_sandbox` through `parseAPNSSandbox` (§2.7 allow-list, unrecognized logged).
Optional `trip_id`, `vehicle_id` (strings), `service_date` (int64, non-numeric → 0),
`stop_sequence` (`*int64`, absent → nil) — identical parsing to alarms.

Response `201 {"url": "<base>/api/v2/regions/<id>/live_activities/<token>"}` where
`<base>` is the region's `sidecarBaseUrl`, else `https://<Host>` — the same helper as
`alarmURL`, generalized to take the resource path segment.

**DELETE** — `204` only after the row is gone, `404` for an unknown token in that
region, `500` on store error. No push.

Both handlers wrap store errors with `sanitizeToken` for the push token and the minted
token before logging.

## 5. Packages

| Package | Adds |
|---|---|
| `internal/liveactivities` | `LiveActivity`, `NewLiveActivity`, `Repository`, `ErrNotFound`, `ErrDuplicate`, `ContentState`, `ArrivalInfo`, `BuildContentState`, `Changed`, constants, `Updater`, `ArrivalsSource` |
| `internal/obaapi` | `StopArrivalsQuery`, `StopArrival`, `Client.ArrivalsAndDeparturesForStop`; stubs on the three existing fakes |
| `internal/push` | `LiveActivityPush`, `LiveActivitySender`, `(*Gorush).SendLiveActivity` |
| `internal/store/sqlite` | migration 00008, `queries/liveactivities.sql`, `gen/`, adapter, `Store.LiveActivities()` |
| `internal/store/storetest` | `RunLiveActivityRepository` |
| `internal/httpapi` | `liveActivitiesHandler` (create/delete), `Deps.LiveActivities`, `Deps.LiveActivityLimiter`, feedback branch + webhook registration hoisted out of the `PushRegs` guard, router registration |
| `cmd/sidecar` | wiring: repo into Deps, `Updater` loop at 1 minute |
| docs | README "Live Activities" section (endpoints, sandbox warning, store-only caveat, on-device verification notes); CLAUDE.md package list |

Constants (`internal/liveactivities`): `Lifetime = 8h`, `KeepaliveInterval = 55s`,
`MaxConsecutiveFailures = 3`, `StaleAfter = 10m`, `DismissAfterEnd = 15m`,
`LookbackMinutes = 5`, `LookaheadMinutes = 120`, `MaxArrivals = 3`,
`StopCacheTTL = 55s`, `StopFetchBudget = 6s`, `checkConcurrency = 8`.

## 6. Configuration

None new. `SIDECAR_GORUSH_URL` and `SIDECAR_APNS_TOPIC` are reused; the Live Activity
topic is derived. Without a gorush URL the updater runs in store-only mode and main's
existing boot warning is extended to mention Live Activities.

## 7. Build order

1. `push`: `LiveActivityPush`, `LiveActivitySender`, `Gorush.SendLiveActivity` + body test.
2. `obaapi`: `StopArrival`, `ArrivalsAndDeparturesForStop` + tests (incl. presence
   detection and the `null` body / empty entry cases).
3. `liveactivities` domain: types, `ContentState` fixture round-trip, `BuildContentState`
   table tests, `Changed`.
4. Store: migration, queries, `make generate`, adapter, `storetest` conformance.
5. `liveactivities.Updater` with fakes.
6. `httpapi`: handlers, feedback branch, router registration + guard tests.
7. `main.go` wiring, README, CLAUDE.md.
8. `make check`, `make test-tz`.

## 8. Testing strategy

- **Fixture contract:** decode `testdata/live_activity_content_state.json`, re-encode,
  compare to a canonical re-encoding of the file; `[]` not `null` for empty.
- **`obaapi`:** references fallback for short name and headsign; `HasIdentity` false
  for a missing `stopSequence` and for a non-numeric `serviceDate`; `null` body and
  empty list cases.
- **`BuildContentState`:** route/headsign filter; collapse survivor by `LastUpdateTime`,
  tie → first in order; loop route (same trip, two sequences) both survive;
  same trip across service dates both survive; `HasIdentity=false` never collapsed;
  arrival vs departure pair and the omitted-arrival-times fallback; predicted only when
  `Predicted && >0`; past entries dropped at exactly `now` boundary (equal survives);
  sort + cap 3; `unknown` with deviation 0; thresholds at −91s (early), −90s (on_time),
  +89s (on_time), +90s (delayed).
- **`Updater`:** an advancing fake clock (a fixed `Now` would never expire the stop
  cache and would never advance `timestamp`); expired → end push then delete; three empties end, two do
  not; fetch error counts; success resets; keepalive at 54s (no push) vs 55s (push);
  changed state inside the keepalive window pushes; send failure leaves the row and
  `last_pushed_at`; end-push failure still deletes; store-only mode never sends but
  still expires; region `ErrNotFound` counts, other region errors do not; two
  subscriptions on one stop → one upstream call per cycle and a second call once the
  clock passes 55s; `timestamp` advances (and is `last_pushed_at + 1s` when the clock
  has not moved) and `stale-date`/`dismissal-date` are set only on the right event;
  a corrupt `last_content_state` is treated as empty.
- **Gorush:** exact JSON body from a real `liveactivities.ContentState` (hyphenated
  keys, derived topic, `development`, no `message`, `[]` not `null` for empty
  arrivals), non-2xx → error without body, zero `Timestamp` rejected, empty
  `apnsTopic` rejected without a request.
- **HTTP:** each 422 message; JSON and form bodies; re-POST with a new `push_token`
  returns the same URL and rewrites the token; 201 on both paths;
  DELETE 204/404 including a slug region path; POST 429 at the limit, DELETE
  unthrottled; feedback prunes registrations and live activities for the same token,
  and is registered (and works) with `LiveActivities` set and `PushRegs` nil; router
  guard when `Regions`/`Now` missing.
- **Store:** `storetest.RunLiveActivityRepository` (upsert insert vs update, race →
  `ErrDuplicate`, update preserves counters and `last_pushed_at`, delete by token
  scoped to region, `DeleteByPushToken` count,
  failure counter, `RecordPush` round-trips state and timestamp, cascade on region
  delete).
- Every test must be shown to fail under mutation; timestamp assertions must hold
  under `make test-tz`.
