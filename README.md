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
sidecar-admin study   create --region N --name S [--description S]
                       list --region N
sidecar-admin survey  create --study N --file <path|->
                       list --region N
                       show ID
                       edit ID --file <path|->
                       delete ID
                       responses ID        # long-format CSV, one row per answer
sidecar-admin user    create --username NAME [--password-stdin]
                       passwd --username NAME [--password-stdin]
                       list
                       delete --username NAME [--force]
sidecar-admin migrate  up | status
```

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
`false`; an absent or empty targeting list means "everywhere".

`survey show <id>` prints the same document (plus `id`, `study`, timestamps),
so `survey show 3 | sidecar-admin --db ./sidecar.db survey edit 3 --file -` is
a round trip. Once a survey has responses its questions are frozen — edit only
the name, dates, flags, and targeting — and it cannot be deleted.

```
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

# Shared secret gorush must send on the feedback webhook, as either
# `Authorization: Bearer <secret>` or a bare `Authorization: <secret>`.
# Leave it unset only if your gorush cannot send a header: the webhook then
# stays open (and rate limited), and should be restricted at the proxy.
export SIDECAR_GORUSH_WEBHOOK_SECRET=...
```

(`--oba-api-key`/`--pirate-weather-key`/`--gorush-url` are the equivalent `sidecar` flags.)

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

Without the secret the endpoint still works, but is rate limited per client
IP as an abuse ceiling and should be restricted to the gorush host at the
proxy. A dropped prune is not lost data: the token is cleaned up by the
180-day sweep, or by the next failure gorush reports.

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

## Development

Requires Go 1.26+ (`mise install` will set it up), [golangci-lint](https://golangci-lint.run) 2.12+, and Node (for the admin SPA in `web/admin`: `make web` and `make check` run `npm ci` there).

```sh
make tools   # install pinned dev tooling
make check   # fmt-check + vet + lint + test + test-tz + test-race — everything CI runs
make run     # build and run the server
make help    # list all targets
```

Run `make check` before opening a pull request. There is no CI workflow in
this repo yet (no `.github/`), but whoever adds one needs to route it
through `make check`, or run `make web` before any `go test` step: the Go
suite includes an embed regression test
(`internal/httpapi/adminui/adminui_test.go`) that needs a populated
`internal/httpapi/adminui/dist/`, and a bare `go test ./...` against the
empty, gitignored `dist/` fails it.

## License

This project is made available under the terms of the Apache 2.0 license. (c) Open Transit Software Foundation.