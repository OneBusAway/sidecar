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

```sh
go build -o bin/sidecar-admin ./cmd/sidecar-admin

# Pull the regions directory (id, name, base URLs) into the database.
./bin/sidecar-admin --db ./sidecar.db region sync

# Configure the two locally-managed fields the directory doesn't carry: the
# default agency id stamped onto alerts that don't specify one, and the
# timezone `alert create`/`alert edit` interpret naive-looking times against
# (an explicit UTC offset is still required; the timezone is only used to
# report a helpful error).
./bin/sidecar-admin --db ./sidecar.db region set --id 1 \
  --agency-id 1 --timezone America/Los_Angeles

# Author, then publish, an alert. --start/--end always require an explicit
# RFC 3339 offset.
./bin/sidecar-admin --db ./sidecar.db alert create --region 1 \
  --header "Route 44 detoured" --start 2026-08-15T14:00:00-07:00 \
  --cause CONSTRUCTION --effect DETOUR
./bin/sidecar-admin --db ./sidecar.db alert publish 1

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
sidecar-admin migrate  up | status
```

## Development

Requires Go 1.26+ (`mise install` will set it up) and [golangci-lint](https://golangci-lint.run) 2.12+.

```sh
make tools   # install pinned dev tooling
make check   # fmt-check + vet + lint + test + test-tz + test-race — everything CI runs
make run     # build and run the server
make help    # list all targets
```

Run `make check` before opening a pull request.

## License

This project is made available under the terms of the Apache 2.0 license. (c) Open Transit Software Foundation.