# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

Go reference implementation of the OneBusAway *sidecar services*: the region-scoped HTTP APIs the OneBusAway mobile apps call for things the core OBA REST API does not provide (service alerts feed, push registrations, departure alarms, surveys, ghost bus reports, weather, vehicle search) plus an admin API/SPA and a `sidecar-admin` CLI. The normative spec is `specification/specification.md` (with `specification/openapi.yaml`); README.md documents every endpoint's shipped-client quirks and the CLI. Per-feature design docs live in `docs/superpowers/specs/` and are cited from code comments as "design spec §N" — those references point at the design doc for that feature, not the main spec.

## Commands

Toolchain is configured in `mise.toml`: Go 1.26 and Node 24 are major/minor selectors (Go must track go.mod and golangci-lint's own build — a newer stdlib breaks lint's typecheck); golangci-lint (2.12.2) and sqlc (1.31.1) are exact pins. `make tools` is `mise install`; CI installs from the same file. Keep the `check` prerequisites and the matrix in `.github/workflows/ci.yml` in sync.

```sh
make check              # everything CI runs: fmt-check vet lint generate-check test test-tz test-race web-check
make test               # go test ./...  (builds the SPA first — see below)
make test-tz            # suite under TZ=UTC and TZ=Asia/Kathmandu; catches local-time leaks
make lint / lint-fix    # golangci-lint (v2 config in .golangci.yml)
make generate           # sqlc generate   (after editing queries/ or migrations/)
make generate-check     # sqlc diff — fails if committed gen/ is stale
make web                # build the SvelteKit admin SPA into internal/httpapi/adminui/dist
make build              # SPA + go build -o bin/sidecar
make run ARGS="..."     # go run ./cmd/sidecar — does NOT build the SPA; /admin serves 503 until `make web`
go build -o bin/sidecar-admin ./cmd/sidecar-admin
make image              # docker build -t sidecar:local .
make up / down / logs   # compose stack: sidecar + gorush (needs .env)
make up-gorush          # gorush only; pair with `make run` on the host
make admin ARGS="…"     # sidecar-admin inside the container
deploy/smoke.sh [url]   # /healthz + /admin + alerts feed check
```

Single test: `go test ./internal/httpapi -run 'TestName/Subtest'`. Go tests need no SPA except `internal/httpapi/adminui` (its embed assertion needs a populated `dist/`), so plain `go test ./...` fails there until you've run `make web` once.

Frontend (`web/admin`): `npm run check` (svelte-check), `npm run lint` (prettier + eslint), `npm run test:unit` (vitest), `npm run dev`.

Local config: copy `.env.example` to `.env`; `cmd/sidecar` loads it at boot and real env vars win. Keys: `SIDECAR_DB`, `SIDECAR_OBA_API_KEY`, `SIDECAR_PIRATE_WEATHER_KEY`, `SIDECAR_GORUSH_URL`, `SIDECAR_GORUSH_WEBHOOK_SECRET`, `SIDECAR_APNS_TOPIC` (each also a flag). `*.db` files are gitignored.

## Architecture

**Two binaries, one SQLite file.** `cmd/sidecar` is the HTTP server; `cmd/sidecar-admin` is the authoring CLI. The CLI never talks HTTP — it opens the same database directly. The admin SPA (`web/admin`, SvelteKit + adapter-static) is built into `internal/httpapi/adminui/dist` and embedded in the server binary (`all:` embed prefix — required so SvelteKit's `_app/` tree is included).

**Layering.** Each feature is a domain package under `internal/` (`alerts`, `regions`, `auth`, `pushreg`, `alertpush`, `alarms`, `surveys`, `ghostbus`, `weather`, `vehicles`, `liveactivities`) that defines its own `Repository` interface and domain types. `internal/store/sqlite` implements every repository over sqlc-generated code (`gen/`, from `queries/*.sql` against goose migrations in `migrations/`) and exposes them via `Store.Alerts()`, `Store.Regions()`, etc. Support packages: `internal/push` (the `Sender` interface and gorush client), `internal/obaapi` (OBA REST client), `internal/httpx` (shared HTTP-client helpers, e.g. the copy-don't-mutate `NoRedirectClient`), `internal/ratelimit`, `internal/cache`, `internal/securetoken`, `internal/dotenv`. `internal/httpapi` holds handlers; everything they need arrives through the `Deps` struct (`router.go`). Most `Deps` fields are optional: a nil repository/service means *those routes are not registered*, which is how feed-only deployments and narrow tests work. Admin routes are a table (`adminRoutes`) rather than ad-hoc `mux.Handle` calls, so tests can enumerate them and assert every one is wrapped in the cross-site guard.

**Wiring lives only in `cmd/sidecar/main.go`**: open store → `Migrate()` → build `Deps` → start background loops (regions directory sync, push-registration prune, alarm scheduler, alert push dispatcher, ghost bus snapshot enrichment, Live Activity updater) → serve. Background loops take `ctx`, a repository, an interval, and a clock. `compose.yaml` (local) and `render.yaml` (Render) mirror each other on ports 8080 (sidecar) and 8088 (gorush).

**Tests.** `internal/store/storetest` is an engine-agnostic conformance suite (`RunAlertRepository(t, newStore)` etc.) that a future Postgres adapter must pass unchanged — it must not import any adapter. `internal/store/sqlitetest.Open(t)` gives a migrated temp-file store for everything else. Handler tests build a `Deps` with only what they need and inject tighter rate limiters, a fixed `Now`, and recorder `Sleep`/`VerifyPassword` funcs.

## Invariants the linter and tests enforce

- **`time.Now` and `time.Local` are banned outside `cmd/` and `_test.go`** (forbidigo). Inject a `now func() time.Time` / pass `now time.Time` down. This includes `storetest`, which is not a test file — derive instants from its fixed `base`.
- **Every timestamp column is INTEGER epoch seconds**, never DATETIME/TEXT: modernc sqlite writes `time.Time.String()` into DATETIME cells and `ORDER BY` then sorts text. Use the `unixToTime`/`timeToNullUnix` helpers in `store.go`.
- **Every CLI/API instant requires an explicit RFC 3339 UTC offset**; naive datetimes are rejected, never interpreted in server-local time. Duration arithmetic (e.g. alarm windows) is on absolute instants — DST must not affect it.
- **sqlc + SQLite:** don't mix `sqlc.arg()` with bare `?` in one query — it compiles and diffs clean but breaks at runtime. Run `make generate` after touching `.sql` files and commit `gen/`.
- **Two more sqlc traps, same silent shape:** keep `queries/*.sql` comments ASCII-only — sqlc renumbers `sqlc.arg()` by byte offset into the statement text, so a multi-byte rune (a `§`, an em dash) in a preceding comment shifts the offsets and emits garbage SQL; cite the design spec as "spec section N" instead. And sqlc does not extract a parameter written inside an `IN (...)` list: `sqlc.arg(x) IN ('a','b')` compiles, diffs clean, and hands the literal text `sqlc.arg(x)` to the driver — spell such guards out as OR comparisons.
- **Rate limiters key on the TCP peer address**, deliberately ignoring `X-Forwarded-For`. Don't "fix" this; the README's Deployment section documents the proxy requirement.
- Several status codes are shipped-client contracts, not placeholders: weather failures are `403` (apps hide the UI on any non-200; tested against 403), ghost bus duplicates/validation are `422`, vehicle upstream failure is `502` not empty `200`. Check README before changing any response code.
- `revive` enforces doc comments on exported identifiers and packages; `nolint` directives need a specific linter and an explanation.
- Rider-sourced cells in CSV exports go through the formula-injection guard (leading apostrophe for `= + - @ \t \r`).
- When writing a test, make sure it can fail: mutate the code under test and confirm the assertion fires. Timezone-dependent assertions must hold under both `make test-tz` zones.

## Workflow notes

- `.NOTPARALLEL` is set in the Makefile on purpose (SPA embed dir and `.svelte-kit` are shared mutable trees); `make -j` gains nothing.
- SonarCloud exclusions must be kept in sync between `sonar-project.properties` and `.sonarcloud.properties` (Automatic Analysis reads the latter).
- Superpowers plans/specs for past features are in `docs/superpowers/`; new feature work has followed the pattern spec → plan → implementation there.
