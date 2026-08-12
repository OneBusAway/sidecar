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
                       set --id N [--agency-id ID] [--timezone TZ]
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
sidecar-admin user    create --username NAME [--password-stdin]
                       passwd --username NAME [--password-stdin]
                       list
                       delete --username NAME [--force]
sidecar-admin migrate  up | status
```

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
  cookie is issued without `Secure` and browsers will not send it back over
  HTTPS.

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
`test-tz`/`test-race`/`check` targets all run it for you first. A CI
workflow that runs bare `go test ./...` (or otherwise skips `make web`)
will fail the embed regression test in
`internal/httpapi/adminui/adminui_test.go`, which needs a populated
`dist/`: route CI through `make check`, or run `make web` before any Go
test step.

## Development

Requires Go 1.26+ (`mise install` will set it up), [golangci-lint](https://golangci-lint.run) 2.12+, and Node (for the admin SPA in `web/admin`: `make web` and `make check` run `npm ci` there).

```sh
make tools   # install pinned dev tooling
make check   # fmt-check + vet + lint + test + test-tz + test-race — everything CI runs
make run     # build and run the server
make help    # list all targets
```

Run `make check` before opening a pull request.

## License

This project is made available under the terms of the Apache 2.0 license. (c) Open Transit Software Foundation.