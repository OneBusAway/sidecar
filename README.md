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
sidecar-admin user    create --username NAME [--password-stdin]
                       passwd --username NAME [--password-stdin]
                       list
                       delete --username NAME [--force]
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

```
POST   /api/admin/v1/alerts/{id}/pushes            {"audience":"all"|"test"}
GET    /api/admin/v1/alerts/{id}/pushes
DELETE /api/admin/v1/alerts/{id}/pushes/{pushId}
GET    /api/admin/v1/alerts/{id}/push_audience
```

Session-authenticated admin routes like the rest of `/api/admin/v1`. `POST`
answers `202` with the queued push and wakes the dispatcher immediately; `GET
…/pushes` answers `200` with that alert's pushes, newest first; `DELETE` cancels
one and answers `204`; `GET …/push_audience` answers `200` with the `all` and
`test` device counts (split by platform) and a `forced_test` flag for a test
alert. Errors are `{"error": "..."}`: `400` for a malformed body or an audience
that is neither `all` nor `test`, `404` for an unknown alert, an unknown push, or
a `pushId` belonging to a different alert, and `409` for each precondition
(unpublished alert, a push already in flight, empty audience) and for canceling a
push that has already finished.

**These four routes are registered only when `SIDECAR_GORUSH_URL` is set.**
Without a transport the admin UI reports that push notifications are not
configured on this server, rather than letting an operator queue a send that
could only fail. The feedback accounting below keeps working either way.

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

The push registration throttle (spec §2.6, 30/minute) keys on the TCP peer
address of the request -- it does not read `X-Forwarded-For` or similar
headers, since those are trivially spoofable by the client they're meant to
throttle. A reverse proxy in front of sidecar must be deployed in a mode
that preserves the real client address as the TCP peer (for example, PROXY
protocol, or a transparent/passthrough L4 proxy); terminating and
re-originating the TCP connection instead means every request throttles
against the proxy's own address, either merging every client into one
shared bucket or (worse) rate-limiting all of them together.

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

#### Render

`render.yaml` declares the same two services as `compose.yaml`: `sidecar` as
a web service with a persistent disk at `/data` for SQLite, and `gorush` as
a private service. First deploy:

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

`SIDECAR_GORUSH_URL` on the sidecar service is populated from `gorush`'s
`hostport`, which Render supplies with no scheme; the server assumes
`http://` for a scheme-less value, which is correct here since Render's
private network is plain HTTP.

Because the disk pins the service to one instance, deploys restart rather
than roll. Render's proxy re-originates TCP, so every per-IP throttle in
this server shares one bucket there; that is a known limitation, not fixed
here.

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