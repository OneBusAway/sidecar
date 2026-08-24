# Local and Render orchestration — design

Date: 2026-08-23
Status: approved for planning

## 1. Goal

Make it trivial to run the sidecar together with a real
[gorush](https://github.com/appleboy/gorush) push gateway on a laptop, exercise the
full push path (register a token from a debug build of the iOS app → create an alarm →
push arrives on the phone → gorush feedback prunes dead tokens), and deploy the same
topology to Render with nothing changed but ports and secrets.

Decisions already made:

- Local pushes are **real**: gorush runs with the developer's APNs auth key and pushes
  to a physical phone. No fake receiver.
- The phone reaches the laptop over **plain HTTP on the LAN** (`http://<mac-ip>:8080`)
  from a debug build; no tunnel.
- Production is **Render**: sidecar as a web service with a persistent disk for SQLite,
  gorush as a private service.
- Local runs the sidecar in Docker by default (prod-shaped), with a shortcut that runs
  only gorush in Docker so the sidecar can iterate on the host via `make run`.

## 2. Artifacts

All new files live at the repo root unless noted.

| File | Purpose |
|------|---------|
| `Dockerfile` | Multi-stage: `node:24` builds `web/admin`; `golang:1.26` copies the built SPA into `internal/httpapi/adminui/dist` and builds `sidecar` and `sidecar-admin`; a minimal runtime stage (distroless or alpine) carries both binaries on `PATH`, `WORKDIR /data`, `ENV SIDECAR_DB=/data/sidecar.db`. The image never depends on the developer's local `dist/`. |
| `.dockerignore` | `bin/`, `*.db*`, `web/admin/node_modules`, `web/admin/build`, `.env`, `docs/`, `.superpowers/`, `.git/`. |
| `compose.yaml` | Services `sidecar` and `gorush` (§3). |
| `render.yaml` | Blueprint declaring the same two services for prod (§4). |
| `deploy/smoke.sh` | Curl-based check that a running image serves the embedded SPA and the alerts feed. |
| `.env.example` | Grows into the single documented variable list (§5). |
| `Makefile` | New targets `image`, `up`, `up-gorush`, `down`, `logs`, `admin`. |
| `README.md`, `CLAUDE.md` | Deployment section rewritten around compose + Render; CLAUDE.md commands list updated. |

## 3. Local topology (`compose.yaml`)

```
phone (debug build) ──http://<mac-ip>:8080──▶ sidecar ──http://gorush:8088/api/push──▶ gorush ──▶ APNs
                                                 ▲                                          │
                                                 └── POST /webhooks/gorush (Authorization: Bearer $SECRET) ◀──┘
```

**`sidecar`**
- Built from `Dockerfile`; `ports: 8080:8080`; named volume `sidecar-data:/data` so the
  database survives `down`/`up`.
- Env from `.env`: `SIDECAR_GORUSH_URL=http://gorush:8088`,
  `SIDECAR_GORUSH_WEBHOOK_SECRET`, `SIDECAR_OBA_API_KEY`, `SIDECAR_PIRATE_WEATHER_KEY`.
- `depends_on: gorush: condition: service_healthy`.
- Member of compose profile `full`.

**`gorush`**
- `image: appleboy/gorush:<pinned tag>`; `ports: 8088:8088` on the host as well, so the
  `up-gorush` shortcut and manual `curl localhost:8088` checks work.
- Env: `GORUSH_IOS_ENABLED=true`, `GORUSH_IOS_KEY_TYPE=p8`, `GORUSH_IOS_KEY_BASE64`,
  `GORUSH_IOS_KEY_ID`, `GORUSH_IOS_TEAM_ID`, `GORUSH_IOS_PRODUCTION=true`. Production is
  the global default because the sidecar already flips individual pushes to the sandbox
  via gorush's per-notification `development` flag from the persisted `apns_sandbox`
  (spec §2.7); one gorush instance therefore serves debug and release tokens.
- Optional `GORUSH_ANDROID_ENABLED` / `GORUSH_ANDROID_KEY_BASE64` for FCM.
- `GORUSH_CORE_FEEDBACK_HOOK_URL` defaults to `http://sidecar:8080/webhooks/gorush`, and
  `GORUSH_CORE_FEEDBACK_HEADER=authorization:${SIDECAR_GORUSH_WEBHOOK_SECRET}`. The
  sidecar accepts a bare `Authorization: <secret>` as well as `Bearer <secret>`, and the
  bare form has no whitespace, so the header survives viper's whitespace-split of list
  env vars and no config file is needed. **The first implementation task verifies this
  against a running gorush (`GET /api/config`)**; if gorush does not honor the env var,
  fall back to a mounted `deploy/gorush.yml` carrying `core.feedback_header`.
- `healthcheck` on gorush's health endpoint.
- Always started (no profile).

**Shortcut.** `make up-gorush` runs `docker compose up gorush` only, overriding the hook
URL to `http://host.docker.internal:8080/webhooks/gorush`. The developer then runs
`make run`; `.env` supplies `SIDECAR_GORUSH_URL=http://localhost:8088` for the host
process. `.env.example` sets `COMPOSE_PROFILES=full` so plain `make up` starts both.

**Bootstrap.** A fresh volume has no regions or users. `make admin ARGS="..."` wraps
`docker compose exec sidecar sidecar-admin --db /data/sidecar.db "$@"`. README documents
the first run as three commands: `make up`, `make admin ARGS="region sync"`,
`make admin ARGS="user create --username admin"`.

## 4. Production topology (`render.yaml`)

Same two services, same variable names.

**`sidecar`** — `type: web`, `runtime: image` built from the repo `Dockerfile`, paid
plan (disks require one), `disk` mounted at `/data`. Health check
`GET /api/v1/regions/1/alerts.pbtext`. Env: `SIDECAR_GORUSH_URL=http://gorush:8088`
(Render private-service hostname = service name), `SIDECAR_DB=/data/sidecar.db`, API
keys and webhook secret as `sync: false` dashboard secrets.

Consequence of the disk: one instance, deploys restart rather than roll. Acceptable for
this workload; stated in README.

**`gorush`** — `type: pserv` (no public URL), `image: appleboy/gorush:<tag>`, same
`GORUSH_*` env, `GORUSH_CORE_FEEDBACK_HOOK_URL=http://sidecar:10000/webhooks/gorush`
(Render's internal port). The APNs key remains a base64 env secret; no file mounts.
The webhook secret lives in one `envVarGroup` shared by both services so it cannot
drift; gorush reads it via `GORUSH_CORE_FEEDBACK_HEADER` exactly as locally, so the
private service needs no files at all.

**Proxy behavior.** Render preserves `Host` and sets `X-Forwarded-Proto: https`, which
satisfies the admin-session requirements. Render's proxy re-originates TCP, so the
per-IP throttles (push registrations, surveys, ghost bus) key on the proxy address and
merge every client into one bucket. This is a known limitation, documented, and *not*
fixed here.

**Admin CLI in prod:** `render ssh sidecar`, then `sidecar-admin --db /data/sidecar.db …`.

What differs between local and prod: the sidecar's listen port, the hook URL's port,
and where secrets come from. Nothing else.

## 5. Configuration (`.env.example`)

Grouped, each line commented with how to obtain the value:

```
# --- sidecar ---
SIDECAR_OBA_API_KEY=
SIDECAR_PIRATE_WEATHER_KEY=
SIDECAR_GORUSH_URL=http://localhost:8088      # host `make run`; compose overrides to http://gorush:8088
SIDECAR_GORUSH_WEBHOOK_SECRET=                 # openssl rand -hex 32
# --- gorush / APNs (base64 -i AuthKey_XXXXXXXXXX.p8 | tr -d '\n') ---
GORUSH_IOS_KEY_BASE64=
GORUSH_IOS_KEY_ID=
GORUSH_IOS_TEAM_ID=
# --- gorush / FCM (optional) ---
GORUSH_ANDROID_ENABLED=false
GORUSH_ANDROID_KEY_BASE64=
# --- compose ---
COMPOSE_PROFILES=full
```

`.env` stays gitignored. Real environment variables win over the file (existing
`dotenv.Load` behavior).

## 6. Verification

Infrastructure, so mostly executable checks:

- **Header env var:** start gorush with `GORUSH_CORE_FEEDBACK_HEADER=authorization:x`
  and confirm `GET /api/config` echoes it under `core.feedback_header`. Decides whether
  `deploy/gorush.yml` exists.
- **Image:** `make image` builds; `docker run --rm <img> sidecar --help` and
  `sidecar-admin --help` succeed; `deploy/smoke.sh` against a running container expects
  `200` from `/admin` (proves the SPA embedded) and from
  `/api/v1/regions/1/alerts.pbtext` after `region sync`.
- **Compose, happy path:** `make up` → `make admin ARGS="region sync"` → register a
  real device token with `curl` (or from the debug app) → create an alarm → `make logs`
  shows gorush accept it → push lands on the phone.
- **Compose, feedback path:** register a garbage token, fire an alarm, observe
  `/webhooks/gorush` receive `BadDeviceToken` and prune the registration.
- **Shortcut:** `make up-gorush` + `make run` reproduces both paths with the sidecar on
  the host.
- **Render:** `render.yaml` validated against the Blueprint schema. The actual first
  deploy needs the owner's account; the plan ships a first-deploy checklist (create
  Blueprint, set secrets, `render ssh` → `region sync` / `user create`, point gorush
  hook, verify with the same curl sequence).
- **Go tests:** `make check` still passes unchanged. No Go code changes are expected;
  if the Dockerfile forces one, it gets its own test.

## 7. Out of scope

Postgres; a fake push receiver; tunnels; trusting `X-Forwarded-For`; a CI workflow that
publishes images (Render builds from the repo); Live Activities end-to-end (same
transport, no extra orchestration).
