# Sidecar

[OneBusAway](https://onebusaway.org) Sidecar server reference implementation written in Golang. 

## About

OneBusAway *sidecar services*: the region-scoped HTTP APIs that the OneBusAway mobile apps use for features the core OneBusAway REST API server does not provide — service alerts, tripdeparture alarms, iOS Live Activities, rider surveys, ghost bus reports, push notification registration, weather, vehicle search, and donations.

## Specification

See [specification.md](specification/specification.md) for the complete specification of the Sidecar server and [openapi.yaml](specification/openapi.yaml) for the OpenAPI spec.

## Reference Implementation

The reference implementation is a full implementation of the sidecar services spec in Golang. 

### Caveats

The sidecar server requires a few other services to function properly: a job queue, a database (probably PostgreSQL), and a functioning instance of the [gorush](https://github.com/appleboy/gorush) push notification server to actually send push notifications.

## Service alerts

The sidecar serves a per-region GTFS-realtime service alerts feed, authored through a
companion CLI that writes the same database directly.

### Endpoints

- `GET /api/v1/regions/{regionId}/alerts` — the feed as binary protobuf
  (`application/octet-stream`), the format apps consume.
- `GET /api/v1/regions/{regionId}/alerts.pbtext` — the same feed rendered as protobuf
  JSON (`text/plain`), for debugging in a browser or with `curl`.

`{regionId}` accepts either a bare id (`1`) or an id-prefixed slug (`1-puget-sound`), and
`0` is a real region (Tampa Bay), not "unset". Pass `?test=1` (any non-blank value) to
include alerts authored with `--test`; omit it to see what riders see.

A translation (`alert translate`) is served only while it still describes the English it
was made from. Every language in an alert's admin JSON carries `"stale": true|false`: a
language is reported stale when any of its translated fields no longer matches the
current English text, while the feed withholds each stale field individually -- so a
review UI can show what riders will not see and offer retranslation.

### Authoring alerts with `sidecar-admin`

`sidecar-admin` is the CLI for creating and publishing alerts. It never talks to the
server over HTTP — it opens the same SQLite database file directly, so run it against a
copy of (or the same file as) whatever `--db`/`SIDECAR_DB` the server uses.

The steps below are sequential, not independent examples: `region sync` populates the
regions table that `region set` updates, and `region set` must run before the first
`alert create` in a fresh database — the regions directory carries no agency id, so
`alert create` has nothing to fall back on and refuses to guess.

```sh
go build -o bin/sidecar-admin ./cmd/sidecar-admin

# Pull the regions directory (id, name, base URLs) into the database.
./bin/sidecar-admin --db ./sidecar.db region sync

# Configure the two locally-managed fields the directory doesn't carry: the
# default agency id stamped onto alerts that don't specify one, and the
# timezone `alert create`/`alert edit` interpret naive-looking times against
# (an explicit UTC offset is still required; the timezone is only used to
# report a helpful error). Required before the first `alert create` below.
./bin/sidecar-admin --db ./sidecar.db region set --id 1 \
  --agency-id 1 --timezone America/Los_Angeles

# Author, then publish, an alert. --start/--end always require an explicit
# RFC 3339 offset. `alert create` prints `created alert <id>`; publish that
# id -- it's only 1 on a fresh database.
id=$(./bin/sidecar-admin --db ./sidecar.db alert create --region 1 \
  --header "Route 44 detoured" --start 2026-08-15T14:00:00-07:00 \
  --cause CONSTRUCTION --effect DETOUR | awk '{print $3}')
echo "created alert $id"
./bin/sidecar-admin --db ./sidecar.db alert publish "$id"

# Alerts stay drafts -- absent from the feed -- until published.
./bin/sidecar-admin --db ./sidecar.db alert list --region 1
```

Full command surface:

```
sidecar-admin region  list
                       set --id N [--agency-id ID] [--timezone TZ] [--oba-api-key KEY]
                       sync
sidecar-admin alert   create --region N --header TEXT --start RFC3339
                              [--description TEXT] [--url URL] [--end RFC3339]
                              [--agency-id ID] [--cause C] [--effect E]
                              [--severity S] [--test]
                       list [--region N] [--all]
                       show ID
                       edit ID [--header TEXT] [--description TEXT] [--url URL]
                               [--start RFC3339] [--end RFC3339 | --no-end]
                               [--agency-id ID] [--cause C] [--effect E]
                               [--severity S] [--test | --no-test]
                       publish ID | unpublish ID | delete ID
                       translate ID --language es [--header TEXT] [--description TEXT]
                       push ID [--audience all|test]   # queue a push notification
                       pushes ID                       # that alert's pushes
sidecar-admin study   create --region N --name S [--description S]
                       list --region N
sidecar-admin survey  create --study N --file <path|->
                       list --region N
                       show ID
                       edit ID --file <path|->
                       delete ID
                       responses ID        # long-format CSV, one row per answer
sidecar-admin ghostbus export --region N [--since RFC3339]   # CSV, one row per report
sidecar-admin key     create --region N --name S [--scope push]  # prints the raw key once
                       list   --region N | --minted-by-principal N
                       revoke --region N --id N
sidecar-admin principal create --name S                        # prints the raw key once
                       list
                       revoke --id N [--keep-keys]
sidecar-admin user    create --username NAME [--password-stdin]
                       passwd --username NAME [--password-stdin]
                       list
                       delete --username NAME [--force]
sidecar-admin import  --file <path|-> [--dry-run]   # an OBACloud region export
sidecar-admin sequence show | bump --min N          # id headroom before a migration
sidecar-admin migrate  up | status
```

### Sending alerts as push notifications

A published alert can also be sent to the region's registered devices as a push
notification. A push is a record, not a request: enqueueing one inserts a row and
returns immediately, and the server's dispatcher performs the fan-out in the
background, paging through the audience so a large send survives a restart. Both
trigger surfaces — the admin API (and the SPA's push card) and `sidecar-admin` —
go through the same preconditions, so they cannot drift.

A push is accepted only when the alert is **published**, no other push for that
alert is still `queued` or `sending`, and the audience is not empty. Several
*completed* pushes per alert are fine: sending to test devices, checking the
result, then sending to everyone is the intended workflow.

**Audiences.** `all` is every registration in the alert's region; `test` is only
those registered with `test_device`. An alert authored with `--test` can only
ever reach test devices — the server forces the `test` audience whatever was
asked for, the same rule that keeps test alerts out of the rider-facing feed
unless `?test=1` is passed.

**Copy** is derived from the alert mechanically and snapshotted into the push row
when it is enqueued, so editing the alert mid-send does not change what the rest
of the audience receives, and the record shows what was actually sent. The
English title is the header and the body is the description; when the description
is blank the title is empty and the header becomes the body, because a push with
no body is invisible on the lock screen. Titles are clamped to 48 runes and
bodies to 120, truncated on a rune boundary with a trailing `…`. A translation is
used only while it is fresh — the same staleness rule the feed applies — and the
field it does not cover falls back to English. Each device's locale is normalized
against the languages in the snapshot; anything that does not match gets English.

#### Admin API

```text
POST   /api/admin/v1/regions/{regionId}/alerts/{id}/pushes            {"audience":"all"|"test","messages":{...}?}
GET    /api/admin/v1/regions/{regionId}/alerts/{id}/pushes
DELETE /api/admin/v1/regions/{regionId}/alerts/{id}/pushes/{pushId}
GET    /api/admin/v1/regions/{regionId}/alerts/{id}/push_audience
```

Authenticated like the rest of `/api/admin/v1` -- a session cookie or a bearer
credential (see *Region API keys and service principals* below) -- except that
`POST` and `DELETE` take an operator **or a region key minted with the `push`
scope**: an ordinary region key must not be able to deliver attacker-controlled
text as a push to every device in the region, so sending and canceling answer
it with `403` even though it can read everything else about the region's
alerts, while OBACloud's key for a migrated region carries the scope on purpose.
`POST` answers `202` with the queued push and wakes the dispatcher immediately;
`GET …/pushes` answers `200` with that alert's pushes, newest first; `DELETE`
cancels one and answers `204`; `GET …/push_audience` answers `200` with the
`all` and `test` device counts (split by platform) and a `forced_test` flag for
a test alert. `POST` may carry `messages` in the same per-language
`{"en": {"title","body"}, …}` shape the response emits; when present it is
stored as the push's copy snapshot verbatim instead of being derived from the
alert, and it must include `en`, use trimmed lowercase language tags, have a
non-blank body in every language, and respect the same 48-rune title and
120-rune body caps the derivation applies. Errors are `{"error": "..."}`:
`400` for a malformed body, an audience that is neither `all` nor `test`, or
`messages` that break one of those rules, `404` for an unknown alert, an
unknown push, or a `pushId` belonging to a different alert, and `409` for each
precondition (unpublished alert, a push already in flight, empty audience) and
for canceling a push that has already finished.

**These four routes are registered only when `SIDECAR_GORUSH_URL` is set.**
Without a transport the admin UI reports that push notifications are not
configured on this server, rather than letting an operator queue a send that
could only fail. The feedback accounting below keeps working either way.

#### Other admin route families

`/api/admin/v1` covers seven more region-scoped route families beyond alerts
and pushes above. Each one registers only when its backing repository is
wired into `Deps`, exactly what `GET /api/admin/v1/regions/{regionId}`
reports back as `"features"`, so a `404` on one of these can be told apart
from "not enabled on this deployment" by checking that list first:

```text
GET    /api/admin/v1/regions/{regionId}/studies                     studies: also POST, GET .../{id}, PATCH .../{id}
GET    /api/admin/v1/regions/{regionId}/surveys                     surveys: also POST, GET/PUT/DELETE .../{id}
GET    /api/admin/v1/regions/{regionId}/surveys/{id}/responses      survey responses, JSON (also .../responses.csv); one by public id at regions/{regionId}/survey_responses/{publicId}
GET    /api/admin/v1/regions/{regionId}/ghost_bus_reports           ghost bus reports, JSON (also .../ghost_bus_reports.csv, .../{publicId})
GET    /api/admin/v1/regions/{regionId}/alarms                      alarms, read-only: also GET .../{id}
GET    /api/admin/v1/regions/{regionId}/push_registrations/count    push registration audience counts, aggregate only
GET    /api/admin/v1/regions/{regionId}/api_keys                    region API keys: list with scopes (also POST {name, scopes?} to mint, DELETE .../{keyId} to revoke)
```

Studies and surveys are the CRUD family behind `sidecar-admin study`/`survey`
below, gated on `Deps.Surveys` (feature `"surveys"`, covering both). Survey
responses are read-only, reachable through the survey's own routes above.
Ghost bus reports are read-only -- reports are rider-submitted, so there is
no admin write route -- gated on `Deps.GhostBus` (feature
`"ghost_bus_reports"`). Alarms are read-only and omit `token` and
`user_push_id` from the JSON, since those are push credentials, not admin UI
data, gated on `Deps.Alarms` (feature `"alarms"`). Push registration counts
are aggregate only, never a token listing, gated on `Deps.PushRegs` (feature
`"push_registrations"`). Region API keys are covered in their own section
below, gated on `Deps.APIKeys` (feature `"api_keys"`, which also gates
bearer authentication itself).

#### Sending from `sidecar-admin`

```sh
./bin/sidecar-admin --db ./sidecar.db alert push 3 --audience test
./bin/sidecar-admin --db ./sidecar.db alert pushes 3
```

`alert push` prints the id of the row it queued. The CLI never talks to the
running server, so nothing is sent until the dispatcher's next tick — within
about 15 seconds. A sidecar started without a gorush URL still runs the
dispatcher, and marks each push it claims `failed` with `no push transport
configured`, so a CLI-queued push never sits `queued` forever.

#### Reading the counts

- `device_count` is the audience size measured when the send started. It is 0
  while the push is still queued.
- `submitted_count` is how many tokens gorush accepted. That is the only positive
  state there is: nothing in the chain confirms end delivery.
- `failed_count` is how many were later reported undeliverable. It is never
  subtracted from `submitted_count` — a device can be in both, which is more
  honest than claiming to know the outcome.

gorush runs asynchronously in both `compose.yaml` and `render.yaml` (it forces
`core.sync` off unless its queue engine is local), so a batch returns `success:
ok` with an empty `logs` array and every failure arrives later on the feedback
webhook. Each batch is stamped with a `notif_id` of `alertpush:<push id>`, which
is what lets a bounce be counted against the push that caused it. Under
`core.sync` the rejections come back inline instead and are counted at send time;
both paths feed the same counter, and a webhook gorush replays is deduplicated
rather than double-counted.

Failures are stored as a SHA-256 hash of the token and a reason, never the token
itself: the hash is only a deduplication key, nothing reads it back, and a
plaintext copy would outlive the registration's retention window. Those rows
cascade-delete with their push, and the pushes cascade with the alert.

#### Resuming, retrying, canceling

The dispatcher ticks every 15 seconds and commits a cursor after each page of
500 registrations, so a crash mid-send resumes at the last committed page and
re-sends at most that one page. A row left `sending` and untouched for 15 minutes
is treated as stuck and reclaimed by the next cycle. (A restart does not wait
that out: the server's first cycle after boot adopts every in-flight row at
once, so a deploy mid-send resumes in seconds.) The stuck clock is also what paces
retries: a transport error leaves the push `sending` with its attempt counted and
its cursor where it was, and after five *consecutive* failures the push is marked
`failed`. A page that succeeds resets the counter, so a long send is not killed
by five scattered errors over its lifetime. Store read errors are not counted as
attempts; they say nothing about the transport. A store *write* failure after a
page has gone out — the cursor commit that records it — does count, because
otherwise a store that never accepts that write would re-send the same page on
every reclaim forever.

Canceling a `queued` push stops it before it starts. Canceling one mid-send stops
it at the next page boundary — batches already handed to gorush are on their way
and cannot be recalled. Canceling an already-finished push is a `409`. The
dispatcher also cancels a push on its own if the alert was deleted or unpublished
between enqueue and send.

## Surveys

The sidecar serves spec §7: a per-region list of active rider surveys and the
survey response endpoints the apps submit to.

### Endpoints

- `GET /api/v1/regions/{regionId}/surveys?user_id=<device uuid>` (also
  `…/surveys.json`, which both shipped apps call) — surveys that are available
  and inside their window. `user_id` is required.
- `POST /api/v1/survey_responses` (also with a trailing slash) — creates a
  response; `responses` is a JSON-array-encoded *string*. Returns
  `survey_response.id` and `update_path`.
- `POST|PUT|PATCH /api/v1/survey_responses/{id}` — merges more answers into a
  response by `question_id`.

The write endpoints share one throttle of 60 requests per minute per source
address. The throttle keys on the connection's remote address: behind a
reverse proxy that does not preserve client addresses, every rider shares one
bucket, so configure the proxy (or raise `surveyWritesPerMinute`) accordingly.

### Authoring surveys with `sidecar-admin`

Surveys belong to a study, and a study belongs to a region:

```sh
id=$(./bin/sidecar-admin --db ./sidecar.db study create --region 1 \
  --name "Rider satisfaction" --description "Fall 2026" | awk '{print $3}')

cat > survey.json <<'EOF'
{
  "name": "Rider satisfaction",
  "available": true,
  "start_date": "2026-09-01T00:00:00-07:00",
  "end_date": "2026-09-30T23:59:00-07:00",
  "show_on_map": false,
  "show_on_stops": true,
  "always_visible": false,
  "allows_multiple_responses": false,
  "visible_stop_list": ["1_570", "1_578"],
  "visible_route_list": null,
  "questions": [
    { "required": true,
      "content": { "type": "radio", "label_text": "How was your trip?",
                   "options": ["Great", "Fine", "Bad"] } },
    { "content": { "type": "text", "label_text": "Anything else?" } }
  ]
}
EOF
./bin/sidecar-admin --db ./sidecar.db survey create --study "$id" --file survey.json
```

Question `content.type` is one of `text`, `label`, `radio`, `checkbox`,
`external_survey` (the last takes `url`, `survey_provider`,
`embedded_data_fields`, `sdk_configuration_values`). Questions are displayed in
document order. Dates require an explicit UTC offset. Absent booleans are
`false`, except `available`, which defaults to `true`; an absent or empty
targeting list means "everywhere".

`survey show <id>` prints the same document (plus `id`, `study`, timestamps),
so `survey show 3 | sidecar-admin --db ./sidecar.db survey edit 3 --file -` is
a round trip. Once a survey has responses its questions are frozen — edit only
the name, dates, flags, and targeting — and it cannot be deleted.

An edit — `survey edit`, or `PUT /api/admin/v1/regions/{regionId}/surveys/{id}`
— honours the `id` on each question in the document, since the apps persist
question ids: a question that names one keeps it, one without gets a fresh id, a
stored question the document omits is deleted, and positions are renumbered from
document order. An id that belongs to another survey, or is named twice, is
refused (`422`) before any row is touched, as is any question id on a create — so
`survey show 3 | survey create --file -` errors on the ids it carries, while
`survey show 3 | survey edit 3 --file -` round-trips.

```text
sidecar-admin study   create    --region N --name S [--description S]
                      list      --region N
sidecar-admin survey  create    --study N --file <path|->
                      list      --region N
                      show      <id>
                      edit      <id> --file <path|->
                      delete    <id>
                      responses <id>        # long-format CSV, one row per answer
```

`survey responses` writes rider-sourced cells (identifiers, question type/label,
answer) with a leading apostrophe when the cell would otherwise open with a
spreadsheet formula character (`=`, `+`, `-`, `@`, a tab, or a carriage return), so
opening the export can't execute a formula a rider embedded in their answer.

## Ghost bus reports

The sidecar serves spec §8: a write-only endpoint for riders to report a bus
that the app predicted but that never arrived, plus a background worker that
enriches each report with a snapshot of the trip's state at report time.
There is deliberately no rider-facing read API -- the CSV export below is the
read surface, and reports are agency data kept indefinitely (no retention
sweep, unlike push token registrations or alarms above).

### Endpoint

- `POST /api/v2/regions/{regionId}/ghost_bus_reports` -- creates a report.
  Requires `user_identifier`, `trip_identifier`, `service_date`, and
  `wait_duration_minutes` (one of `5`, `10`, `15`, `20`, `30`); accepts
  optional route/stop/vehicle identifiers, a coordinate pair, timing fields,
  and a comment up to 1,000 runes. Accepts JSON, form-encoded, or query
  parameters, matching every other write endpoint in this repo. `{regionId}`
  accepts either a bare id (`1`) or an id-prefixed slug (`1-puget-sound`), the
  same as the alerts feed; there is no `.json` variant, since the shipped
  client doesn't send one here.
- On success: `201 {"id": "<public identifier>"}`. One report per `(region,
  user_identifier, trip_identifier, service_date)` -- a duplicate returns
  `422 {"error": "already_reported", ...}`. The shipped iOS app treats *any*
  `422` as "already reported" regardless of error code, so validation
  failures and duplicates both read as a soft failure to riders; the distinct
  `already_reported` code is still emitted for future or third-party clients
  that want to tell the two apart.
- A JSON body over 8 KB (by declared `Content-Length` or by streaming past
  the cap) is rejected with a bodyless `403`, before or without reading the
  body. This cap is JSON-only: form bodies keep the shared 64 KB request
  limit, because iOS percent-encodes comments with an allow-list, and a
  rider-legal emoji-heavy comment can legally push a form body past 8 KB.

### Abuse throttles

Two independent limiters, per spec §2.6:

- **10 reports per hour per IP**, keyed on the request's TCP peer address --
  the same reverse-proxy caveat as push registrations applies here (see the
  Deployment section below): a proxy that re-originates the TCP connection
  merges every client into one shared bucket.
- **20 reports per day per `user_identifier`**, read from whichever of
  query/form/JSON the request actually used, so the bucket always matches the
  identifier the report is filed under. A blank or missing `user_identifier`
  skips the counter and fails validation instead; an over-length identifier
  is rejected before the throttle is consulted, so it never becomes a limiter
  key.

### Snapshot enrichment

A background worker polls every 30 seconds for reports still awaiting
enrichment, fetches trip details from the region's OneBusAway REST API (its
own key, falling back to `SIDECAR_OBA_API_KEY`), and stores a pruned snapshot
of the trip's status and display fields at the time of capture. Each report
is tried up to 3 times total before it's given up on; `snapshot_status` is
one of `pending`, `captured`, or `unavailable`. Enrichment never blocks or
fails the rider's `201` -- a region with no resolvable OBA key, or a trip
details lookup that never succeeds, just leaves the report's snapshot
`unavailable`.

### Exporting reports with `sidecar-admin`

```sh
./bin/sidecar-admin --db ./sidecar.db ghostbus export --region 1 \
  --since 2026-09-01T00:00:00-07:00 > reports.csv
```

`ghostbus export` writes CSV to stdout, one row per report, with columns for
the report itself, its trip/route/stop display fields, and the snapshot
(vehicle position, distance from stop, trip phase, and the raw snapshot
JSON). `--since` is optional and, like every other CLI instant in this repo,
requires an explicit UTC offset. Rider-sourced and GTFS-derived cells go
through the same formula-injection guard as `survey responses`: a leading
apostrophe when a cell would otherwise open with `=`, `+`, `-`, `@`, a tab,
or a carriage return.

## Live Activities

iOS Lock Screen widgets showing the next departures for one bookmarked route +
headsign at one stop (spec §6). The sidecar owns the update cadence: once a
minute per subscription it fetches the stop's arrivals from the region's OBA
server, builds the §6.2 `content-state`, and pushes it through gorush as an
APNs `liveactivity` push (priority 10, topic `<bundle id>.push-type.liveactivity`,
derived from `SIDECAR_APNS_TOPIC`).

### Endpoints

```text
POST   /api/v2/regions/{regionId}/live_activities          → 201 {"url": "…/live_activities/<token>"}
DELETE /api/v2/regions/{regionId}/live_activities/{token}  → 204 | 404
```

- Registration is an **upsert on `(region, activity_id)`**: ActivityKit rotates
  push tokens and the app re-POSTs the same activity with each one. The URL is
  the same every time; every field, including `apns_sandbox`, is re-read.
- Required: `activity_id`, `push_token` (≤ 4096 chars — a sidecar addition), `stop_id`,
  `route_short_name`, `trip_headsign`. Optional trip metadata: `trip_id`,
  `service_date` (epoch ms), `vehicle_id`, `stop_sequence`.
- `apns_sandbox` follows the §2.7 allow-list. The stakes are highest here: a
  misrouted push bounces `BadDeviceToken`, which the feedback webhook treats as
  terminal and deletes the subscription.
- POST is throttled at 30/minute per TCP peer (a sidecar-specific addition —
  every distinct stop costs one upstream call per minute for eight hours);
  DELETE is not.
- Subscriptions end at 8 hours, after 3 consecutive empty/error cycles, on
  client DELETE, or on terminal APNs feedback. The first two send a best-effort
  `end` push (dismissal 15 minutes out); the last two do not.
- Without `SIDECAR_GORUSH_URL` the updater runs in store-only mode: rows expire
  and reap but nothing is pushed.

Deviations from OBACloud, all deliberate: route/headsign matching resolves
names through the response `references` like the app does; the push-token
length cap and the POST throttle are new.

## Weather and vehicle search

The sidecar also serves two rider-facing lookups that proxy and cache an upstream
source per region: current conditions for the region's map, and a fuzzy search over
its live vehicle ids.

### Endpoints

- `GET /api/v1/regions/{regionId}/weather` — current conditions plus an hourly
  forecast for the region's centroid. A `403` means "unavailable" and covers every
  failure mode alike: no Pirate Weather key configured, the region has no centroid
  yet, or the upstream provider errored or timed out. This is deliberate, not a
  placeholder -- shipped apps treat any non-200 response as "hide the weather UI"
  and have been tested against `403` specifically, so it must never be "improved"
  to a 5xx. An unknown `{regionId}` is still a plain `404`.
- `GET /api/v1/regions/{regionId}/vehicles?query=` — substring search over the
  region's live vehicle ids. `query` must be between 3 and 64 characters (runes, not
  bytes); shorter or longer queries return `[]` without an upstream call. Only the
  query is lowercased before matching -- vehicle ids are compared raw -- so a fleet
  with uppercase ids is effectively case-sensitive; this replicates the reference
  implementation's behavior and is a deliberate compatibility quirk, not a bug.
  Results are capped at 250. An upstream failure is a `502`, not an empty `200`: an
  empty list is indistinguishable from "no such vehicle", so a rider searching for a
  bus that exists would otherwise be told, confidently, that it doesn't. An unknown
  `{regionId}` is a `404`.

### Configuration

Both endpoints depend on process-wide keys, set as flags or environment variables:

```sh
# Default OneBusAway REST API key, used for any region with no key of its own.
# Without it, vehicle search returns 502 for regions with no key of their own.
export SIDECAR_OBA_API_KEY=...

# Pirate Weather API key. Without it, the weather endpoint returns 403 for
# every region.
export SIDECAR_PIRATE_WEATHER_KEY=...

# Base URL of the gorush push gateway. Without it, departure alarms are
# still created, stored, and reaped on schedule -- they just never fire:
# the alarm scheduler always runs (spec §5.3), but the fire step is skipped
# when no transport is configured.
export SIDECAR_GORUSH_URL=...

# APNs topic (the iOS app's bundle id), required alongside SIDECAR_GORUSH_URL:
# under .p8 token auth Apple rejects every push without one (MissingTopic).
# The server warns at boot if gorush is configured and this is not.
export SIDECAR_APNS_TOPIC=org.onebusaway.iphone

# Shared secret gorush must send on the feedback webhook, as either
# `Authorization: Bearer <secret>` or a bare `Authorization: <secret>`.
# Leave it unset only if your gorush cannot send a header: the webhook then
# stays open (and rate limited), and should be restricted at the proxy.
# gorush splits its header setting on ':', so the value must contain no ':'
# or whitespace; the server refuses to start otherwise.
export SIDECAR_GORUSH_WEBHOOK_SECRET=...
```

(`--oba-api-key`/`--pirate-weather-key`/`--gorush-url`/`--apns-topic`/`--gorush-webhook-secret` are the equivalent `sidecar` flags.)

A region can also carry its own OneBusAway REST API key, overriding
`SIDECAR_OBA_API_KEY` for that region alone -- set it the same way as the other
per-region fields:

```sh
# A region's own key overrides SIDECAR_OBA_API_KEY for that region. Pass an
# empty value to clear it and fall back to the process default.
./bin/sidecar-admin --db ./sidecar.db region set --id 1 --oba-api-key <key>
```

The key is write-only: `region set` accepts it but never prints it back, `region
list` reports only whether a region has "own key" or "none (may inherit server
default)", and the admin API's region endpoints report the same status word
(`region`/`default`/`none`) instead of the key's value. Passing the key as a
command-line argument puts it in shell history and in `ps` output visible to
other users on the same host; a future revision will offer a stdin form so the
key never appears on the command line at all.

Weather needs no separate coordinate configuration -- a region's centroid arrives
automatically as part of `region sync`, computed from the regions directory's
`bounds` rectangles for that region.

## Donations

`POST /api/v1/payment_intents` (spec section 11) powers the apps' in-app
donations through Stripe's PaymentSheet. The route exists only when
`SIDECAR_STRIPE_SECRET_KEY` is set; without it the apps get a 404 and hide
the donations UI (note that the iOS app decides whether to *offer*
donations per app bundle, not per region, so a deployment serving the
OneBusAway app should set this up before taking over a region). The body
is raw JSON -- the one endpoint that does not also accept form encoding --
and the app's `test_mode: "1"` routes the request to
`SIDECAR_STRIPE_TEST_SECRET_KEY`, so TestFlight builds exercise the flow
against production. Recurring donations create a monthly price under
`SIDECAR_STRIPE_RECURRING_PRODUCT_ID` (and the test-mode
`SIDECAR_STRIPE_TEST_RECURRING_PRODUCT_ID`; each is required with its key,
checked at boot); the OBACloud deployment used
`prod_OqlLl6mR66dLVQ` and `prod_P1xUtsgjEfkGgu` respectively. A new Stripe
customer is created per donation -- deliberately not matched by email,
since the route is unauthenticated and a recurring response hands the
caller that customer's id and an ephemeral key to its saved payment
methods. A Stripe failure is a `500` with an empty
body, which is what the shipped apps expect; a malformed request is a
`400`. The endpoint is throttled at 10/minute per client address.

## Admin UI

The sidecar server also serves a small admin single-page app at `/admin` for
authoring alerts through a browser instead of the CLI above. It reads and
writes the same database as `sidecar-admin` and `sidecar` itself -- there is
no separate admin database, and no web signup: the only way to create an
account is `sidecar-admin user create`.

The steps below are sequential, like the CLI quickstart above: `user create`
bootstraps the one account that can sign in, and `make build` embeds the SPA
into the binary that then serves it.

```sh
# Create the first admin user (prompts for a password; 12 char minimum).
./bin/sidecar-admin --db ./sidecar.db user create --username admin

# Build the admin SPA into the server binary, then run it.
make build
./bin/sidecar --db ./sidecar.db
# open http://localhost:8080/admin and sign in
```

`make run` does not build the SPA -- it runs `go run` directly and skips the
`web` prerequisite that `make build` has. A server started with `make run`
(or from a `go build` run before the first `make web`) serves a 503 "admin
UI not built; run make web" response at `/admin` instead of a login page.
That is expected, not a bug -- run `make web` (or `make build`, which
includes it) once first.

`/admin` itself is now a region picker rather than the alerts list: it
auto-forwards when the database has exactly one region and otherwise
remembers the last region chosen (in `localStorage`). Every other page is
region-scoped in its own URL, e.g. `/admin/regions/1/alerts/42`, so a reload
or a deep link always has a region to put in the API path. There is no
compatibility shim for the old shape -- a bookmark to `/admin/alerts/42`
from before this change shows the SPA's ordinary not-found page rather than
redirecting.

### Region API keys and service principals

The admin API above is session-authenticated when a browser is driving it,
but a server-to-server integration -- OBACloud's Rails app, most concretely
-- has no browser and no operator to log in as. `/api/admin/v1` also accepts
`Authorization: Bearer <key>` carrying one of two credentials, checked in
place of the session cookie (a request carrying both is a bearer request;
cookies are ignored entirely once an `Authorization` header is present):

- **A region API key** (`obask_<regionID>_<43 base64url chars>`, e.g.
  `obask_1_Qm9…`) is scoped to exactly one region. Almost everything an
  operator can do to that region's own resources, it can also do -- alerts,
  studies, surveys and their responses, ghost bus reports, alarms, push
  registration counts, and reading or setting that region's
  `PATCH /regions/{id}` fields, **including its OBA API key**, which
  redirects that region's sidecar-side OBA calls (ghost bus snapshots,
  vehicle search, alarms) to a key the holder controls. Two things stay off
  limits regardless of region: another region 404s, and the `…/api_keys`
  family is refused (`403`), since a region key is not one of the principal
  kinds it accepts. Sending or canceling a push notification is gated on a
  **scope**: a key minted without one answers `403` there too -- a leaked
  ordinary key must not be able to page every device in the region -- while
  a key minted with `"scopes": ["push"]` (what OBACloud holds for each
  migrated region) can send and cancel pushes for its own region. A leaked
  region key therefore reaches one region's tenant data and, through its OBA
  key, that region's own OBA traffic; a leaked **push-scoped** key can also
  deliver notifications to every device in that region. It cannot reach
  another tenant or mint or revoke anything. **The remedy for a leaked
  region key, scoped or not, is to revoke it** (below) and mint a
  replacement.
- **A service principal** (`obasp_<43 base64url chars>`) is deployment-wide
  but single-purpose: it can only mint, list, and revoke region API keys,
  through the `…/api_keys` family, and nothing else -- every other admin
  route answers it with `403`. That is a deliberate trade. A leaked service
  principal can mint itself a live key for any published region -- including
  a push-scoped one, so it can deliver notifications to every device in every
  region -- and then use it, with the region-key exposure above; revoke every
  region key in
  the deployment (a deployment-wide denial of service for whatever
  integration depends on them), and enumerate which region ids exist along
  with every key's metadata (names, creator ids, timestamps). It **cannot
  read a single alert, survey response, or ghost bus report** -- no tenant
  data is reachable through a service principal at all. Recovery does not
  require hunting for which keys are legitimate: `principal revoke` (below)
  takes every key the principal minted with it, so the
  fix is revoke the principal, mint a new one, and re-provision every region
  from scratch.

Only a hex SHA-256 hash of each key is stored, the same posture as browser
sessions; the raw value is shown exactly once, at mint time, and never
appears in a list, a log line, or an error message afterward.

`sidecar-admin` mints and revokes both kinds directly against the database
(`created_by`/`revoked_by` records `cli` for these). A service principal
mints region keys the same way OBACloud will, over HTTP:

```sh
# An operator mints the deployment-wide principal once, up front.
./bin/sidecar-admin --db ./sidecar.db principal create --name "obacloud"
# obasp_972so11ncVZAgGSH…
# id: 1  name: obacloud

# The consumer holding that principal mints a key scoped to one region by
# calling the sidecar itself -- this is the provisioning flow OBACloud's
# integration automates (see below):
curl -s -X POST https://sidecar.example.org/api/admin/v1/regions/1/api_keys \
  -H "Authorization: Bearer obasp_972so11ncVZAgGSH…" \
  -H "Content-Type: application/json" \
  -d '{"name":"obacloud rails1","scopes":["push"]}'
# {"id":1,"name":"obacloud rails1","scopes":["push"],"key":"obask_1_L_RvltB_P6G8UwZ9…", …}
```

An operator can also mint a region key directly, without a principal, for
manual testing or a deployment with no external consumer yet:

```sh
./bin/sidecar-admin --db ./sidecar.db key create --region 1 --name "manual test key"
# obask_1_WlDL9LeQxtC1KYww…
# id: 2  name: manual test key  scopes: —

./bin/sidecar-admin --db ./sidecar.db key list --region 1
# 2  manual test key    cli            2026-08-28T00:39:13Z  —  —  —  —
# 1  obacloud rails1    principal:1    2026-08-28T00:39:05Z  —  —  —  push
```

A key carries **scopes**: named capabilities on top of the ordinary
region-scoped authoring surface. `--scope push` is repeatable and is the only
one defined; `key list` renders a key's scopes in its last column, `—` for a
key with none. Existing keys have no scopes and so keep exactly the reach
they had. An unknown scope name is refused before anything is written, and
the admin API's `POST …/api_keys` answers `400` for the same reason.

```sh
./bin/sidecar-admin --db ./sidecar.db key create --region 1 --name rails --scope push
# obask_1_9nQ2sVb7pKdT0mXe…
# id: 3  name: rails  scopes: push
```

A key is never scoped to more than one region, and nothing limits a region
to one live key, so **rotation is mint, swap, revoke**: mint a new key,
update whatever holds the old one, then revoke the old one once the new one
is confirmed working. `key list`'s `last_used_at` column is what tells an
operator whether the old key is still in use before revoking it -- but it is
touched **at most once an hour** (a deliberate write-avoidance trade, not a
bug), so a value can be up to an hour stale; a key that looks idle for the
last twenty minutes may simply not have been used *and* touched yet.

Every key and principal records **who created it and who revoked it**
(`created_by_kind`/`created_by_id`, `revoked_by_kind`/`revoked_by_id` --
`operator`, `principal`, or `cli`, with the CLI's own actor carrying no id).
After a suspected principal compromise, `key list --minted-by-principal N`
is the triage query: it lists every key that principal minted, across every
region -- distinguishing them from keys an operator minted directly, which
carry `cli` and are never touched by a principal revoke -- so an operator
can see at a glance what a leaked principal could have touched, which is
also exactly the set `principal revoke` clears out by default:

```sh
./bin/sidecar-admin --db ./sidecar.db key list --minted-by-principal 1
# 1  obacloud rails1    principal:1    2026-08-28T00:39:05Z  —  —  —  push

./bin/sidecar-admin --db ./sidecar.db principal revoke --id 1
# revoked keys: 1
# revoked principal 1

./bin/sidecar-admin --db ./sidecar.db key list --region 1
# 2  manual test key    cli            2026-08-28T00:39:13Z  —  —              —     —
# 1  obacloud rails1    principal:1    2026-08-28T00:39:05Z  —  2026-08-28T00:39:21Z  cli   push
```

The manually-minted key (`2`) is untouched -- only the key the principal
itself minted (`1`) went away. `principal revoke` takes every live key the
principal minted with it and prints their ids, so the
operator on the other end knows exactly which credentials just went dead;
pass `--keep-keys` for a planned rotation of a principal whose existing keys
are known to be fine.

### OBACloud integration

OBACloud (the Rails app behind onebusawaycloud.com) is the intended consumer
of the bearer credentials above: it re-plumbs its own server-rendered admin
pages to read and write the sidecar instead of its own Postgres tables,
region by region, then flips each region's `sidecar_base_url` to a Go host.
The Rails side holds **one service principal per sidecar deployment it talks
to** and mints **one push-scoped region API key per migrated region** on
demand, storing the key alongside the region row; it keeps its own push
wizard, copywriter, and scheduling, and sends through `POST …/pushes` with
its own `messages`. Un-migrated regions keep the Rails-hosted default
`sidecar_base_url` and need no principal. That client, its provisioning
triggers, its rotation and bulk-reprovisioning rake tasks, the cutover
task, and its error mapping from sidecar status codes to Rails-side behavior
are a contract this repository documents but does not implement -- see
[the design spec, §7](docs/superpowers/specs/2026-08-26-region-api-keys-and-admin-api-design.md#7-obacloud-contract-documented-built-later)
for the sidecar-side contract and OBACloud's own
`docs/superpowers/specs/2026-08-28-sidecar-migration-design.md` for the
cutover plan.

### Deployment

Sessions rely on the request's `Host` header and TLS status to reject
cross-site writes and to mark the session cookie `Secure`. A reverse proxy
in front of sidecar must:

- Preserve the public `Host` header (nginx: `proxy_set_header Host $host;`
  -- nginx's default rewrites `Host` to the upstream address, which makes
  every admin write look cross-site and get rejected with a 403).
- Set `X-Forwarded-Proto: https` when terminating TLS, or the session
  cookie is issued without `Secure`, so it will also be sent over any
  plain-HTTP connection to the same host instead of being restricted to
  HTTPS.
- **Must not log the `Authorization` header.** Region API keys and service
  principals (see *Region API keys and service principals* above) are
  bearer credentials sent on every request a server-to-server consumer
  makes, not a one-time login. An access log that captures request headers
  turns ordinary log retention into credential retention -- anyone who can
  read the logs can read every live key that ever made a request.

Every per-IP throttle (push registrations, Live Activities, surveys, ghost
bus reports, failed bearer attempts) keys on the TCP peer address of the
request by default -- it does not read `X-Forwarded-For` or similar
headers, since those are trivially spoofable by the client they're meant to
throttle. A reverse proxy in front of sidecar must either preserve the real
client address as the TCP peer (PROXY protocol, or a transparent/passthrough
L4 proxy), or be one that *overwrites* a client-address header of its own on
every request, in which case set `SIDECAR_TRUSTED_PROXY` (`--trusted-proxy`)
to name it: `cloudflare` reads `CF-Connecting-IP`, `render` reads
`True-Client-IP`, and `header:<Name>` covers any other proxy with the same
guarantee. The header is honoured only on requests proven to have come
through that proxy: either the peer address is inside
`SIDECAR_TRUSTED_PROXY_CIDRS` (`render` defaults to the private ranges,
which is all that reaches a Render service), or the request carries
`X-Sidecar-Proxy-Secret` equal to `SIDECAR_TRUSTED_PROXY_SECRET` -- for
Cloudflare, add a Transform Rule (Modify Request Header) on the proxied
hostname that sets it. Every other request -- including one that reaches
Render's `*.onrender.com` hostname directly, where anyone could set
`CF-Connecting-IP` to a fresh value per request and mint unlimited
buckets -- keys on the peer, as with the setting off. Requests with the
proof but no header, or a value that is not an IP address, also fall back
to the peer. There is deliberately no `X-Forwarded-For` mode: its first
entry is whatever the client sent. Leaving the setting off
behind a proxy that terminates and re-originates TCP means every request
throttles against the proxy's own address -- one shared bucket for all
clients, which at any real traffic level is a 429 for nearly everyone.

gorush's feedback webhook (spec §6.5, terminal APNs failures) should be
pointed at `POST /webhooks/gorush` on this server. That endpoint deletes a
token's registrations in *every* region on a caller-supplied value, so set
`SIDECAR_GORUSH_WEBHOOK_SECRET` and configure gorush to send it. An
authenticated webhook is not rate limited -- gorush reports one failure per
dead token, and a mass uninstall arrives as a burst that a throttle would
turn into lost prune signals.

The same endpoint is where an alert push's asynchronous failures are counted:
a payload carrying a `notif_id` of `alertpush:<push id>` increments that
push's `failed_count` and records the token's hash, whether or not the
failure reason is a terminal one (see *Sending alerts as push notifications*).

Without the secret the endpoint still works, but is rate limited per client
IP as an abuse ceiling and should be restricted to the gorush host at the
proxy. A dropped prune is not lost data: the token is cleaned up by the
180-day sweep, or by the next failure gorush reports. The push accounting is
the one thing an open webhook lets an anonymous caller *write*: someone who
guesses a `notif_id` can inflate a push's failure counters. What bounds that
is the throttle and the fact that there is nothing there to corrupt beyond a
report -- the rows hold a counter and token hashes, nothing reads either back
to make a delivery decision, and both cascade away with the push and its
alert. A deployment that cares about the accuracy of its own fan-out numbers
sets the shared secret; that is what the setting is for.

#### Staging

`render.staging.yaml` is the production Blueprint with every service and
disk renamed (`sidecar-staging`, `gorush-staging`), gorush pointed at the
APNs sandbox, Sentry tagged `staging`, the Litestream replica under its
own key prefix, and `SIDECAR_REGIONS_URL` left for you to set. Point it at
a hand-maintained directory file whose regions all carry the staging host
as `sidecarBaseUrl` -- `deploy/regions-staging.example.json` is a starting
point (Davis plus a synthetic region) to upload to the regions bucket --
so TestFlight and debug builds that set the app's custom regions URL land
on staging and production devices never do. Production runs the same
Blueprint (`render.yaml`) behind the custom domain
`sidecar2.onebusaway.org`, Cloudflare-proxied, with
`SIDECAR_TRUSTED_PROXY=cloudflare` and the Transform Rule secret so per-IP
throttles key on `CF-Connecting-IP`, and with `SIDECAR_REGIONS_URL` pointed
at the directory's **`regions-v3.json`** so experimental regions are
addressable. `sidecar.onebusaway.org` keeps resolving to OBACloud's Rails
app until the last region has migrated and drained; only then does it
become a second custom domain here. Proxy the staging custom
domain through Cloudflare like production's so the trusted-proxy path and
the feed cache rule are exercised there too. Rehearse the export/import
and a Litestream restore against staging before the first region flips.

#### Migrating a region from OBACloud

`sidecar-admin import --file <export.json>` loads one region's content and
rider state from an export document (`internal/export`, format
`sidecar-export/1`): alerts with their translations, studies, surveys and
questions, survey responses, push registrations, and ghost bus reports
with their enrichment snapshots. OBACloud produces the document with
`bin/rails "sidecar:export[<region id>,<path>]"`. `--file -` reads it from
stdin, so the cutover pipes it straight over `render ssh` with no file
transfer step:

```sh
cat export.json | render ssh sidecar -- sidecar-admin --db /data/sidecar.db import --file -
```

Ids are preserved -- the feed's `Alert_<id>` entity ids and the survey and
question ids the apps persist locally must not change under riders -- and
every row is checked with the domain packages' own rules (enum names,
question content, answer shape, time windows; not the HTTP layer's length
caps) before anything is written, so a bad row rejects the whole document.
Rows that already exist under this region (same id, public id, or
region+token) are skipped and counted, which makes the migration two runs
of the same command: a bulk import the day before the region's
`sidecar_base_url` flips, and a delta from a fresh export right after --
translations added to an already migrated alert land on the delta run. An
id that already belongs to another region's content (alerts, studies,
surveys, and questions share one id sequence) is an error naming the row,
never a silent skip. `--dry-run` runs the same checks, including that the
region exists, without writing. The region itself must already be present
(`sidecar-admin region sync`); alarms and Live Activities are deliberately
not part of the document -- OBACloud keeps firing the ones it owns until
they expire.

**Id headroom.** Once the first region has migrated, this database mints
alert, study, survey, and question ids from that region's maximum upward
while OBACloud keeps minting for un-migrated regions from its own
sequences, so a later region's export can collide with content authored
here in between. Before the first cutover, run

```sh
sidecar-admin --db /data/sidecar.db sequence bump --min 1000000
sidecar-admin --db /data/sidecar.db sequence show
```

once per deployment: `bump` raises the `alerts`, `studies`, `surveys`, and
`survey_questions` sequences to at least the floor (re-running with the same
or a lower floor changes nothing), and `show` prints each current value so
the cutover runbook can verify the headroom is still there. 1,000,000 is
comfortably above any OBACloud id and its growth during the migration.

#### Backups

The image ships [Litestream](https://litestream.io). Set
`SIDECAR_BACKUP_BUCKET` and the container entrypoint (`deploy/entrypoint.sh`)
streams every committed SQLite transaction to that S3-compatible bucket
for as long as the server runs, and -- when the local database file is
missing, as on a fresh or replaced disk -- restores the latest replica
before starting. Leave the variable empty and the entrypoint execs the
server directly, as before. The other settings: `SIDECAR_BACKUP_ENDPOINT`
(required for anything but AWS; for Cloudflare R2,
`https://<account id>.r2.cloudflarestorage.com`; must be `https://` unless
`SIDECAR_BACKUP_ALLOW_INSECURE=1` says a local test store is in use),
`SIDECAR_BACKUP_REGION`
(default `auto`, which R2 wants), `SIDECAR_BACKUP_ACCESS_KEY_ID` /
`SIDECAR_BACKUP_SECRET_ACCESS_KEY`, `SIDECAR_BACKUP_PATH` (key prefix,
default `sidecar`; give staging and production different prefixes or
buckets), and `SIDECAR_BACKUP_RETENTION` (how far back a point-in-time
restore can reach, default `168h`). The config is `deploy/litestream.yml`.

To restore by hand -- to inspect a backup, or to move to a new host --
run, with the same environment:

```sh
litestream restore -config /etc/litestream.yml -o /tmp/restored.db /data/sidecar.db
litestream restore -config /etc/litestream.yml -timestamp 2026-08-28T12:00:00Z -o /tmp/before.db /data/sidecar.db
```

The entrypoint fills in `SIDECAR_BACKUP_PATH=sidecar`,
`SIDECAR_BACKUP_REGION=auto`, and `SIDECAR_BACKUP_RETENTION=168h` when
unset; export the same three before running `litestream restore` from a
shell, since `deploy/litestream.yml` expands them.

Rehearse this on staging before relying on it. Render's own daily disk
snapshots are a second, coarser line of defence; Litestream is the one
that loses seconds rather than a day. Known gap: once replication is
running, a revoked key or a changed bucket policy makes every upload fail
while `/healthz` stays green and nothing is reported -- Litestream logs it
and carries on. Until replica lag is surfaced, check the bucket's newest
object age from outside (a cron against the bucket, or a periodic
`litestream restore -o` rehearsal on staging).

#### Feed caching

Both renderings of the alerts feed answer with
`Cache-Control: public, max-age=60, stale-if-error=600`. A CDN in front of
the host (Cloudflare, when the custom domain is proxied) can therefore
serve the feed for a minute per region and keep serving the last good
copy for ten minutes when the origin is down -- which covers the restart
every deploy costs a single-instance service. Cloudflare honours
`Cache-Control` for cacheable responses only when a cache rule marks the
path eligible; add one for `/api/v1/regions/*/alerts*`. Error responses
are `no-store`, so a cached miss cannot shadow a region added later even
under the CDN's default TTL for 404s.

#### Render

`render.yaml` declares the same two services as `compose.yaml`: `sidecar` as
a web service with a persistent disk at `/data` for SQLite, and `gorush` as
a private service. Both run prebuilt images: gorush's from Docker Hub and the
sidecar's from GHCR, where `.github/workflows/image.yml` publishes
`ghcr.io/onebusaway/sidecar` on every push to `main` (tags `main` and an
immutable `sha-<short>`) and on every `vX.Y.Z` tag (`X.Y.Z`, `X.Y`,
`latest`). The Blueprint pulls `:main`. Render re-pulls only when told to:
set the repository secret `RENDER_DEPLOY_HOOK_URL` (Dashboard -> sidecar ->
Settings -> Deploy Hook) and each `main` build deploys itself; leave it
unset and deploy by hand. To roll back, set the service's image URL to the
previous build's `sha-<short>` tag and deploy; set it back to `:main` when
the fix lands. The package must be public (Packages -> sidecar -> Package
settings -> Change visibility) or the service needs a registry credential.
First deploy:

1. New → Blueprint, pick this repo. Render creates both services and the
   `sidecar-shared` env group, which holds `SIDECAR_GORUSH_WEBHOOK_SECRET`.
   That secret is auto-generated by Render, not entered by hand (Blueprint's
   `sync: false` isn't allowed inside an env group). Verify: the Blueprint
   has a two-way `fromService` reference (`sidecar`'s `SIDECAR_GORUSH_URL`
   reads `gorush`'s `hostport`, and `gorush`'s
   `GORUSH_CORE_FEEDBACK_HOOK_HOSTPORT` reads `sidecar`'s). If Render rejects
   the Blueprint for a dependency cycle, delete the
   `GORUSH_CORE_FEEDBACK_HOOK_HOSTPORT` entry from `render.yaml` and instead
   read the sidecar's internal address from Dashboard → sidecar → Connect →
   Internal.
2. Fill the `sync: false` secrets (the webhook secret is generated for you
   and is not among them): on `sidecar`, `SIDECAR_APNS_TOPIC`,
   `SIDECAR_OBA_API_KEY`, and `SIDECAR_PIRATE_WEATHER_KEY`; on `gorush`, the
   three `GORUSH_IOS_*` credential values. Leave `GORUSH_CORE_FEEDBACK_HOOK_URL`
   and `GORUSH_CORE_FEEDBACK_HEADER` blank at creation time; they are set in
   the next step once the services exist. Verify: if the generated
   `SIDECAR_GORUSH_WEBHOOK_SECRET` in group `sidecar-shared` comes up empty
   (Render does not document `generateValue` inside env groups), set it by
   hand to the output of `openssl rand -hex 32` and make sure both services
   show the same value.
3. On the `gorush` service, set the two values Blueprint cannot derive: copy
   the generated value from Dashboard → Environment Groups →
   `sidecar-shared`, then set `GORUSH_CORE_FEEDBACK_HEADER` to
   `authorization:` followed by that value, and
   `GORUSH_CORE_FEEDBACK_HOOK_URL` to `http://` followed by the sidecar's
   internal `host:port` followed by `/webhooks/gorush`. Take that `host:port`
   from the `GORUSH_CORE_FEEDBACK_HOOK_HOSTPORT` value Render populated on the
   gorush service (a staging var gorush itself never reads); if you removed
   that entry under the dependency-cycle fallback in step 1, read it instead
   from Dashboard → sidecar → Connect → Internal.
4. Wait for `/healthz` to go green, then `render ssh sidecar` and run
   `sidecar-admin region set …` and `sidecar-admin user create …`
   (`SIDECAR_DB` is already set in the image). Verify: if the sidecar
   crash-loops instead with a permission error on `/data`, Render mounted
   the disk root-owned over the image's `sidecar`-owned directory; the fix
   is a root entrypoint that `chown`s `/data` and drops to `sidecar`, or
   removing `USER sidecar` from the Dockerfile (not changed here).
5. Sign in at `https://<service>.onrender.com/admin`; confirm the session
   cookie carries `Secure` and that an admin write succeeds. This verifies
   Render preserves `Host` and sets `X-Forwarded-Proto`, which its docs do
   not state explicitly.

**Logs and error reporting.** The server writes one line per request to
stderr (method, matched route pattern with its `{placeholders}`, status,
bytes, client address, duration -- never the concrete path, the query
string, or any header, since alarm and Live Activity tokens travel in the
path and rider identifiers and bearer keys in the others), one summary
line per background cycle (`alarms: cycle`, `liveactivities: cycle`, with
row counts and elapsed time), and a `httpapi: panic` line with a stack
when a handler panics (the client gets a 500 if nothing had been written
yet; otherwise the connection is closed so a truncated body is not
mistaken for a complete one). Set `SIDECAR_LOG_FORMAT=json` for log
aggregators. Set `SIDECAR_SENTRY_DSN` to have every error-level line --
panics, failed pushes, loop failures, the store and upstream errors
handlers log before answering 5xx, and a failed boot -- reported to Sentry
as well, tagged with `SIDECAR_SENTRY_ENVIRONMENT` and, for the published
image, the git sha it was built from; Sentry's own delivery failures are
logged as `errreport: sentry` warnings. Leave the DSN unset and nothing
leaves the box. `/healthz` is logged at debug level only.

`SIDECAR_GORUSH_URL` on the sidecar service is populated from `gorush`'s
`hostport`, which Render supplies with no scheme; the server assumes
`http://` for a scheme-less value, which is correct here since Render's
private network is plain HTTP.

**Background loops and leases.** Every background loop (regions directory
sync, push registration pruning, the alert push dispatcher, the alarm
checker, the Live Activity updater, ghost bus snapshot enrichment) runs
under a named lease in the `leases` table: a process ticks a loop only
while it holds that loop's lease, renews it once a minute, and releases
every lease on a clean shutdown. A holder that dies loses its leases three
minutes later and the next process to poll takes them over (logged as
`lease: acquired` / `lease: lost to another process`). On the single
SQLite instance this is bookkeeping; it is what makes a second process --
a rolling deploy, or a future multi-instance deployment on Postgres --
safe to run against the same database without doubling every OBA lookup
and push. The alarm checker also does not re-check every alarm every
minute: after a lookup that says the fire window is still far off, the
alarm is deferred (`alarms.check_after`) halfway to that window -- three
hours out becomes 90 minutes, then 45, 22, ... -- and returns to the
once-a-minute cadence once under four minutes of slack remain (a
deferral is never shorter than two minutes nor longer than an hour), so
an alarm set hours ahead
costs a handful of lookups rather than hundreds. A clean shutdown hands
each loop to the replacement at its next poll, within a minute. An admin
"send now" that reaches a process not holding the `alert-pushes` lease
is covered by the holder's next 15-second tick.

Because the disk pins the service to one instance, deploys restart rather
than roll. Render's proxy re-originates TCP, so set
`SIDECAR_TRUSTED_PROXY=render` on the service (or `cloudflare` plus
`SIDECAR_TRUSTED_PROXY_SECRET` and the matching Cloudflare Transform Rule
when the custom domain is proxied through Cloudflare, which is then the
hop that sets the header); without it every per-IP throttle shares one
bucket.

#### Cutover runbook

The once-per-deployment sequence that stands a host up and hands OBACloud
the credentials it needs, before any region's `sidecar_base_url` flips.
Steps 2-5 are the same on staging; run them there first. Every command
below was run against the binaries in this tree, and the commented output
is what they actually print.

**1. Deploy.** Apply `render.yaml` as a Blueprint in the production
workspace from the Dashboard (New -> Blueprint; the CLI has no `blueprint`
command as of v2.5.0), which creates `sidecar`, `gorush`, and the
`sidecar-data` disk at `/data`. Fill the `sync: false` secrets and the two
hand-derived gorush values exactly as *Render*'s first-deploy list above
describes; production additionally takes the Stripe keys,
`SIDECAR_TRUSTED_PROXY_SECRET`, `SIDECAR_SENTRY_DSN`, and the Litestream
`SIDECAR_BACKUP_*` values (*Backups*). Then add `SIDECAR_REGIONS_URL` =
`https://regions.onebusaway.org/regions-v3.json` by hand: `render.yaml`
does not declare it (only `render.staging.yaml` does), and although that
URL is also the binary's compiled-in default, setting it explicitly means
a later change to that default cannot move production. Add the custom
domain `sidecar2.onebusaway.org` to the `sidecar` service; in Cloudflare,
create the proxied CNAME to the `*.onrender.com` host, the Transform Rule
that sets `X-Sidecar-Proxy-Secret` to the value of
`SIDECAR_TRUSTED_PROXY_SECRET` (*Deployment* above --
`SIDECAR_TRUSTED_PROXY=cloudflare` is already in the Blueprint), and the
cache rule for `/api/v1/regions/*/alerts*` (*Feed caching*). Then:

```sh
deploy/smoke.sh https://sidecar2.onebusaway.org
# ok   /healthz -> 200
# ok   /admin -> 200
# ok   /api/v1/regions/1/alerts.pbtext -> 200
```

The feed line reports `skip … -> 404 (no regions synced yet)` until the
directory has been pulled. The server pulls it once at startup, in the
background, so a smoke run in the first seconds after a deploy can lose
that race; re-run the script rather than reading the skip as a failure.
Repeat the whole step for staging with `render.staging.yaml`, its own
custom domain (proxied through Cloudflare too, so the trusted-proxy path
and the cache rule are exercised there), and `SIDECAR_REGIONS_URL` set to
the hand-maintained staging directory (*Staging*).

**2. Bootstrap the database on each host.**

```sh
render ssh sidecar -- sidecar-admin --db /data/sidecar.db region sync
render ssh sidecar -- sidecar-admin --db /data/sidecar.db region list
# 0	Tampa Bay	active=true	agency=	tz=UTC	centroid=27.9553,-82.5231	oba-key=none (may inherit server default)
# 1	Puget Sound	active=true	agency=	tz=UTC	centroid=47.7528,-122.4924	oba-key=none (may inherit server default)
```

`region sync` prints nothing when it succeeds -- `region list` is how you
confirm the directory landed, and it is also the fastest way to see what
the server's own startup sync already did. `--db` is spelled out on every
line rather than leaning on the image's `SIDECAR_DB`, and on staging
`region sync` needs `--regions-url <staging directory>` too: the CLI reads
`SIDECAR_REGIONS_URL` from its own environment, and a non-interactive
`ssh host command` is not guaranteed to inherit the service's, so a bare
`region sync` there can quietly pull the production directory over the
staging one. `--regions-url` is a flag on `sidecar-admin` itself, not on
`region sync`, so it has to come before the subcommand:

```sh
render ssh sidecar-staging -- sidecar-admin --db /data/sidecar.db --regions-url https://<staging directory>/regions-v3.json region sync
```

The first admin user needs a real terminal, because the password is
prompted for twice and deliberately cannot be passed as an argument:

```sh
render ssh sidecar                                  # interactive shell
sidecar-admin --db /data/sidecar.db user create --username <operator>
# Password:
# Confirm password:
# created user <operator>
```

The one-liner form (`render ssh sidecar -- sidecar-admin … user create
…`) fails with `stdin is not a terminal; use --password-stdin`.
`--password-stdin` does work, but it puts the password through the local
shell, which is what the prompt exists to avoid.

**3. Mint the service principal, one per deployment.**

```sh
render ssh sidecar -- sidecar-admin --db /data/sidecar.db principal create --name obacloud-production
# obasp_9klpGIRDdZPsCDXG59FjTN9tC6dk…
# id: 1	name: obacloud-production

render ssh sidecar-staging -- sidecar-admin --db /data/sidecar.db principal create --name obacloud-staging
```

The first line is the raw credential, stored only as a hash and never
recoverable from the database; a lost one is re-minted, not looked up.
Paste it into OBACloud's credentials under
`sidecar.principals["https://sidecar2.onebusaway.org"]`, and staging's
under its own base URL.

**4. Id-sequence headroom, before the first cutover.**

```sh
render ssh sidecar -- sidecar-admin --db /data/sidecar.db sequence bump --min 1000000
# alerts: 0 -> 1000000
# studies: 0 -> 1000000
# surveys: 0 -> 1000000
# survey_questions: 0 -> 1000000
render ssh sidecar -- sidecar-admin --db /data/sidecar.db sequence show
# alerts	1000000
# studies	1000000
# surveys	1000000
# survey_questions	1000000
```

Record the `show` output in the cutover ticket; the cutover re-checks it.
*Migrating a region from OBACloud* above explains why the floor is there
and why re-running `bump` is safe.

**5. Prove the OBACloud contract end to end.** With the principal from
step 3 in `$P`, mint and revoke a push-scoped key exactly the way OBACloud
will:

```sh
curl -s -X POST https://sidecar2.onebusaway.org/api/admin/v1/regions/1/api_keys \
  -H "Authorization: Bearer $P" -H 'Content-Type: application/json' \
  -d '{"name":"runbook check","scopes":["push"]}'
# 201 {"id":1,"name":"runbook check","scopes":["push"],"key":"obask_1_i6iK4jtmlst4Z_SLs59uHH…",
#      "created_by":{"kind":"principal","id":1},"created_at":"2026-08-29T08:45:36Z"}

curl -s -o /dev/null -w '%{http_code}\n' -X DELETE \
  https://sidecar2.onebusaway.org/api/admin/v1/regions/1/api_keys/1 \
  -H "Authorization: Bearer $P"
# 204
```

A `403` on the POST means the principal is wrong or revoked; a `400` of
`{"error":"unknown scope \"…\""}` means the scope name was not `push`.
The minted key is live until the DELETE, so do not skip it -- the
`revoked_at` and `revoked_by` that appear in `GET …/api_keys` are the
receipt.

Then rehearse the export and import against staging, before any
production cutover:

```sh
bin/rails "sidecar:export[1,export.json]"          # run in OBACloud
cat export.json | render ssh sidecar-staging -- \
  sidecar-admin --db /data/sidecar.db import --file - --dry-run
# dry run: stdin is a valid sidecar-export/1 document for region 1: 1 alerts, 0 studies, 0 survey responses, 0 push registrations, 0 ghost bus reports
```

Drop `--dry-run` to apply it. If the CLI stops to prompt on that pipeline
-- it defaults to interactive output -- pass `--confirm` and `-o text`
before the `--`, so nothing of the CLI's own can read the document out of
stdin.

### Development

Run the SPA against a live server with hot reload instead of rebuilding the
embedded copy on every change:

```sh
cd web/admin && npm run dev
```

This proxies `/api` requests to `localhost:8080` (see
`web/admin/vite.config.ts`), so start `./bin/sidecar` (or `make run`)
alongside it. There is deliberately no CORS configuration anywhere in this
repo -- every legitimate client is same-origin, either directly or through
this dev proxy, and blocking cross-origin admin API access is exactly what
the cross-site guard exists to do.

`make web` rebuilds the embedded copy that ships inside the `sidecar`
binary from `web/admin`'s current source; `make build` and the `test`/
`test-tz`/`test-race`/`check` targets all run it for you first (see the CI
note in the "## Development" section below if you're wiring up a workflow).

## Running locally with Docker

The quickest way to exercise the whole thing -- including real pushes to a
phone -- is the compose stack, which runs the sidecar beside
[gorush](https://github.com/appleboy/gorush) exactly as production does.

```sh
cp .env.example .env    # fill in the webhook secret and your APNs key
make up                 # builds the image, starts sidecar + gorush
deploy/smoke.sh         # /healthz, /admin, and the alerts feed
make admin ARGS="region set --id 1 --agency-id 1 --timezone America/Los_Angeles"
make admin ARGS="user create --username admin"
```

If you already have a `.env` from an earlier setup, merge the new keys from
`.env.example` into it instead of copying over it.

The server syncs the regions directory itself at boot, so `region sync` is
not needed; `region set` is, because the directory carries no agency id.
Point a debug build of the app at `http://<your-mac-ip>:8080`; its tokens
register with `apns_sandbox=1` and gorush routes them to the APNs sandbox
per push, so one gorush serves debug and release builds alike.

To iterate on Go code without rebuilding the image, run only gorush in
Docker and the sidecar on the host:

```sh
make up-gorush
make run
```

`.env` already points `SIDECAR_GORUSH_URL` at `http://localhost:8088`, and gorush's
feedback hook is redirected to `host.docker.internal:8080` so token prunes
still arrive. `make down` stops either arrangement; the SQLite volume
(`sidecar-data`) survives. `GORUSH_IOS_ENABLED` and `GORUSH_ANDROID_ENABLED`
in `.env` control which gorush platforms are configured; Android additionally
needs `GORUSH_ANDROID_CREDENTIAL` set to the raw FCM service-account JSON,
not a path or base64 encoding of it. Running `make up` after `make
up-gorush` re-points gorush's hook at the container, and doing the reverse
leaves a running sidecar container whose prunes go to the host -- run `make
down` between the two arrangements.

APNs under `.p8` token auth requires a topic on every push, so
`SIDECAR_APNS_TOPIC` (the iOS bundle id) must be set wherever
`SIDECAR_GORUSH_URL` is; the server warns at boot if it is not.

### Verifying a push on a real device

A complete runbook for getting an APNs push from this stack onto an iPhone.

**1. Fill in `.env`.** A base64 `.p8` key is *not enough on its own* — token
auth also needs the key id and team id, and every push needs the app's bundle
id as its topic. All five of these must be set (get the first three from
[developer.apple.com](https://developer.apple.com) → Keys / Membership):

```sh
# gorush — the APNs credential
GORUSH_IOS_KEY_BASE64=...        # base64 -i AuthKey_XXXXXXXXXX.p8 | tr -d '\n'
GORUSH_IOS_KEY_ID=XXXXXXXXXX     # 10 chars, the key's ID
GORUSH_IOS_TEAM_ID=XXXXXXXXXX    # 10 chars, your team ID
# sidecar
SIDECAR_APNS_TOPIC=org.onebusaway.iphone   # the EXACT bundle id of the build on the phone
SIDECAR_GORUSH_WEBHOOK_SECRET=...          # openssl rand -hex 32  (needed only for the prune half)
```

The topic must be the bundle id of the build actually installed on the
device. A debug build often has a different id than the App Store build
(e.g. a `.debug` suffix); the wrong one bounces every push with
`DeviceTokenNotForTopic`.

**2. Start the stack and confirm the key loaded.**

```sh
make up
deploy/smoke.sh                              # /healthz, /admin, alerts feed
curl -s localhost:8088/api/config | sed -n '/^ios:/,/^queue:/p'
```

The `ios:` block must show `enabled: true`, `key_type: p8`, and a
`[REDACTED]` `key_id`/`team_id` (gorush hides their values). If `enabled` is
false or the container is restart-looping, gorush rejected the key — check
`docker compose logs gorush`.

**3. Get the device token and your Mac's LAN address.**

```sh
ipconfig getifaddr en0        # your Mac's LAN IP; the phone must be on the same Wi-Fi
```

Point the debug build at `http://<that-ip>:8080` (a debug build allows
plain-HTTP via its ATS exception) and let it register — it will `POST` its
APNs token to `/api/v2/regions/1/push_registrations` with `apns_sandbox=1`.
Grab that 64-hex-character token from the Xcode console or the app's debug UI.

**4a. Fastest check — push straight through gorush.** This skips the
sidecar's trip lookup and proves the credential + topic + token + sandbox
routing all work. Use `"development": true` for a debug build (sandbox
token); `false` for a TestFlight/App Store build (production token):

```sh
curl -s -X POST localhost:8088/api/push -H 'Content-Type: application/json' -d '{
  "notifications": [{
    "tokens": ["<the 64-hex device token>"],
    "platform": 1,
    "topic": "org.onebusaway.iphone",
    "title": "OneBusAway",
    "message": "Sidecar test push",
    "development": true
  }]
}'
# → {"counts":1,"logs":[],"success":"ok"}   (queued, not yet delivered)
docker compose logs -f gorush                # watch the async APNs result
```

A `200`/`success: ok` only means gorush accepted the request; delivery
happens asynchronously. Watch the gorush log for the outcome (see the error
table below).

**4b. Full end-to-end through the sidecar** (exercises the real alarm path).
Register the token, then create an alarm on a live upcoming departure; the
push fires when the trip is `seconds_before` seconds from leaving:

```sh
IP=$(ipconfig getifaddr en0)
curl -s -X POST http://$IP:8080/api/v2/regions/1/push_registrations \
  -d token=<device token> -d operating_system=ios -d apns_sandbox=1

curl -s -X POST http://$IP:8080/api/v2/regions/1/alarms \
  -d user_push_id=<device token> -d operating_system=ios -d apns_sandbox=1 \
  -d stop_id=<stop> -d trip_id=<trip> -d service_date=<epoch ms> \
  -d stop_sequence=<n> -d seconds_before=60
```

See [specification.md](specification/specification.md) §5.2 for the field
semantics. `service_date` is epoch **milliseconds**. Only `user_push_id` and
`operating_system` are validated; a bogus trip still returns `201` but its
push carries a generic message and never resolves a real departure, so use a
trip that is genuinely a minute or two out.

**Reading the result.** gorush returns immediately; the real outcome is in
`docker compose logs gorush` and, for terminal failures, on the feedback
webhook (which prunes the dead token). Common APNs errors:

| gorush log message | Cause | Fix |
|---|---|---|
| `InvalidProviderToken` | `GORUSH_IOS_KEY_ID`/`TEAM_ID`/key don't match, or the host clock is skewed | recheck all three; ensure the Mac's clock is correct |
| `MissingTopic` | no topic sent | set `SIDECAR_APNS_TOPIC` (4a sends `topic` inline) |
| `DeviceTokenNotForTopic` | topic ≠ the build's bundle id | set the topic to the installed build's exact bundle id |
| `BadDeviceToken` | `development` flag doesn't match the build's environment | `true` for a debug/sandbox build, `false` for TestFlight/release |
| `TopicDisallowed` | the key isn't enabled for this topic | enable the key for the app in the Apple developer portal |

### Live Activities

The runbook above applies; a few things are specific to the Live Activity path
(spec §6) and worth checking separately before assuming the credential is bad:

- **The topic is derived, not sent by the client or configured in gorush.**
  The sidecar appends `.push-type.liveactivity` to `SIDECAR_APNS_TOPIC` itself
  on every push (gorush does not derive this suffix — a bare bundle id would
  bounce `BadTopic`). With `SIDECAR_GORUSH_URL` set but `SIDECAR_APNS_TOPIC`
  empty, the sidecar runs Live Activities in store-only mode (rows expire and
  reap, nothing is pushed) and says so in a boot warning — nothing ever shows
  up in `docker compose logs gorush` because no request was sent. Alarms
  still go out, and APNs rejects them with `MissingTopic`.
- **The push token is not the device alert token.** ActivityKit hands the app
  a separate token per Live Activity (from `Activity.pushTokenUpdates`), not
  the `UNUserNotificationCenter` token used for `push_registrations`/alarms.
  Registering the alert token by mistake bounces every push with
  `BadDeviceToken`; the feedback webhook treats that as terminal and deletes
  the subscription outright.
- **Priority must be 10**, not 5: `SendLiveActivity` always sends gorush
  `"priority": "high"` (APNs 10). At 5 an idle phone queues the push instead
  of delivering it and the Lock Screen card visibly freezes.
- **Watching it work:** register (below), then tail `docker compose logs -f
  gorush` and the sidecar's own log for `liveactivities:` lines — one cycle
  runs per minute, so expect an `update` push within about 60 seconds.
  `TopicDisallowed` in the gorush log points at the topic (wrong bundle id,
  or the key isn't enabled for this app); `BadDeviceToken` points at the
  token (alert token used instead of the ActivityKit push token, or a
  `apns_sandbox`/build mismatch).

```sh
IP=$(ipconfig getifaddr en0)
curl -s -X POST http://$IP:8080/api/v2/regions/1/live_activities \
  -d activity_id=<activitykit activity id> \
  -d push_token=<activitykit push token, NOT the device alert token> \
  -d apns_sandbox=1 \
  -d stop_id=<stop> -d route_short_name=<route> -d trip_headsign=<headsign>
# → 201 {"url": "http://.../live_activities/<token>"}

curl -s -X DELETE http://$IP:8080/api/v2/regions/1/live_activities/<token>
# → 204
```

## Development

Requires Go 1.26+, [golangci-lint](https://golangci-lint.run) 2.12+, [sqlc](https://sqlc.dev) 1.31+, and Node -- all declared in `mise.toml` (golangci-lint and sqlc as exact pins; Go and Node as major/minor selectors), so `mise install` (or `make tools`) sets up everything once mise is activated in your shell (Node is for the admin SPA in `web/admin`: `make web` and `make check` run `npm ci` there).

```sh
make tools     # install pinned dev tooling
make check     # fmt-check + vet + lint + generate-check + test + test-tz + test-race + web-check — everything CI runs
make run       # go run the server (/admin serves 503 until `make web`)
make up        # start sidecar + gorush in Docker (see "Running locally with Docker")
make help      # list all targets, including the rest of the Docker ones
```

Run `make check` before opening a pull request. CI
(`.github/workflows/ci.yml`) runs one job per `make check` target, so a green
`make check` locally means green CI. Keep any `go test` step routed through
make: the Go suite includes an embed regression test
(`internal/httpapi/adminui/adminui_test.go`) that needs a populated
`internal/httpapi/adminui/dist/`, and a bare `go test ./...` against the
empty, gitignored `dist/` fails it.

## License

This project is made available under the terms of the Apache 2.0 license. (c) Open Transit Software Foundation.