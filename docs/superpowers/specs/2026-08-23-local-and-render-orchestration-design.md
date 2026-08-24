# Local and Render orchestration — design

Date: 2026-08-23
Status: approved for planning (revised after validation review)

## 1. Goal

Make it trivial to run the sidecar together with a real
[gorush](https://github.com/appleboy/gorush) push gateway on a laptop, exercise the
full push path (register a token from a debug build of the iOS app → create an alarm →
push arrives on the phone → gorush feedback prunes dead tokens), and deploy the same
topology to Render with nothing changed but secrets and service addresses.

Decisions already made:

- Local pushes are **real**: gorush runs with the developer's APNs auth key and pushes
  to a physical phone. No fake receiver.
- The phone reaches the laptop over **plain HTTP on the LAN** (`http://<mac-ip>:8080`)
  from a debug build; no tunnel.
- Production is **Render**: sidecar as a web service with a persistent disk for SQLite,
  gorush as a private service.
- Local runs the sidecar in Docker by default (prod-shaped), with a shortcut that runs
  only gorush in Docker so the sidecar can iterate on the host via `make run`.

## 2. Required code changes

Validation against gorush's source and Apple's APNs docs found two gaps that make the
"push lands on the phone" goal impossible with the current code. Both are small and in
scope.

### 2.1 APNs topic (blocker)

Under token-based (`.p8`) auth, APNs *requires* the `apns-topic` header; only
certificate auth defaults it to the bundle id. gorush has no `ios.topic` setting — it
passes each request's `topic` field straight through — and the sidecar's
`gorushNotification` (`internal/push/gorush.go`) sends none. Every push would bounce
with `MissingTopic`.

Change: add `SIDECAR_APNS_TOPIC` (flag `--apns-topic`), the iOS app's bundle id.
`push.Notification` gains `Topic string`; `push.Gorush` sets it on iOS notifications
(omitted for Android). `cmd/sidecar` threads it into the alarm scheduler's sender. If the
flag is unset and a gorush URL *is* set, log a warning at boot — iOS pushes will fail.
Live Activities need the `.push-type.liveactivity` suffix and `push_type`; they are not
wired yet and stay out of scope. The existing terminal reason `DeviceTokenNotForTopic`
already covers a mismatched topic.

### 2.2 Health endpoint (important)

Render's health check must pass before the service goes live, and `render ssh` needs a
live service. The only always-200 route today is `/admin`, and `/api/v1/regions/1/...`
is 404 until the regions sync loop has fetched the directory. Add `GET /healthz` →
`200 ok`, unauthenticated, no dependencies. Registered unconditionally in `NewRouter`.

## 3. Artifacts

All new files live at the repo root unless noted.

| File | Purpose |
|------|---------|
| `Dockerfile` | Multi-stage: `node:24` builds `web/admin`; `golang:1.26` copies `web/admin/build/.` into `internal/httpapi/adminui/dist` (the `//go:embed all:dist` directive) and builds `sidecar` and `sidecar-admin`; a minimal runtime stage carries both binaries on `PATH`, `WORKDIR /data`, `ENV SIDECAR_DB=/data/sidecar.db`. The image never depends on the developer's local `dist/`. |
| `.dockerignore` | `bin/`, `*.db*`, `web/admin/node_modules`, `web/admin/build`, `.env`, `docs/`, `.superpowers/`, `.git/`. |
| `compose.yaml` | Services `sidecar` and `gorush` (§4). |
| `render.yaml` | Blueprint declaring the same two services for prod (§5). |
| `deploy/smoke.sh` | Curl-based check against a running image: `/healthz` 200, `/admin` 200 (proves the SPA embedded), `/api/v1/regions/1/alerts.pbtext` 200 once regions have synced. |
| `.env.example` | Grows into the single documented variable list (§6). |
| `Makefile` | New targets `image`, `up`, `up-gorush`, `down`, `logs`, `admin`. |
| `README.md`, `CLAUDE.md` | Deployment section rewritten around compose + Render; CLAUDE.md commands list updated. |

No `deploy/gorush.yml`: gorush v1.22.0 reads `core.feedback_header` via
`viper.GetStringSlice`, which honors `GORUSH_CORE_FEEDBACK_HEADER` from the environment
(verified in `config/config.go` at that tag). The first plan task still confirms this
empirically via `GET /api/config`; if it fails, fall back to a file mounted at
`/home/gorush/config.yml`.

## 4. Local topology (`compose.yaml`)

```
phone (debug build) ──http://<mac-ip>:8080──▶ sidecar ──http://gorush:8088/api/push──▶ gorush ──▶ APNs
                                                 ▲                                          │
                                                 └── POST /webhooks/gorush (Authorization: $SECRET) ◀──┘
```

**`sidecar`**
- Built from `Dockerfile`; `ports: 8080:8080`; named volume `sidecar-data:/data` so the
  database survives `down`/`up`.
- Env from `.env`: `SIDECAR_GORUSH_URL=http://gorush:8088` (compose sets this
  explicitly, overriding the host-oriented value in `.env`),
  `SIDECAR_GORUSH_WEBHOOK_SECRET`, `SIDECAR_APNS_TOPIC`, `SIDECAR_OBA_API_KEY`,
  `SIDECAR_PIRATE_WEATHER_KEY`.
- `depends_on: gorush: condition: service_healthy`.
- Member of compose profile `full`.

**`gorush`**
- `image: appleboy/gorush:1.22.0` (alpine-based, runs as user `gorush`, `WORKDIR
  /home/gorush`, `EXPOSE 8088`). `ports: 8088:8088` on the host as well, so the
  `up-gorush` shortcut and manual `curl localhost:8088` checks work.
- Env: `GORUSH_IOS_ENABLED=true`, `GORUSH_IOS_KEY_TYPE=p8`, `GORUSH_IOS_KEY_BASE64`,
  `GORUSH_IOS_KEY_ID`, `GORUSH_IOS_TEAM_ID`, `GORUSH_IOS_PRODUCTION=true`. Production is
  the global default because the sidecar already flips individual pushes to the sandbox
  via gorush's per-notification `development` flag from the persisted `apns_sandbox`
  (spec §2.7); one gorush instance therefore serves debug and release tokens.
- Optional FCM: `GORUSH_ANDROID_ENABLED=true` + `GORUSH_ANDROID_CREDENTIAL` (gorush's
  `android.credential`, the service-account JSON; the plan checks
  `notify/notification_fcm.go` for whether it wants raw JSON or base64 before
  documenting it).
- `GORUSH_CORE_FEEDBACK_HOOK_URL=http://sidecar:8080/webhooks/gorush` and
  `GORUSH_CORE_FEEDBACK_HEADER=authorization:${SIDECAR_GORUSH_WEBHOOK_SECRET}`. The
  sidecar accepts a bare `Authorization: <secret>` as well as `Bearer <secret>`; the bare
  form has no whitespace, so it survives viper's whitespace split of list env vars.
  gorush's `extractHeaders` splits each entry on `:` into exactly two parts, so the
  secret **must not contain `:` or whitespace** — `openssl rand -hex 32` satisfies this
  and `.env.example` says so.
- Health: the image already declares `HEALTHCHECK CMD ["/bin/gorush","--ping"]`; compose
  uses it as-is (no custom healthcheck).
- Always started (no profile).

**Shortcut.** `make up-gorush` runs `docker compose up gorush` only, overriding the hook
URL to `http://host.docker.internal:8080/webhooks/gorush` (resolves to the host on
Docker Desktop for Mac). The developer then runs `make run`; `.env` supplies
`SIDECAR_GORUSH_URL=http://localhost:8088` for the host process, which binds `:8080` on
all interfaces. `.env.example` sets `COMPOSE_PROFILES=full` so plain `make up` starts
both.

**Bootstrap.** The server syncs the regions directory itself at boot, so the feed works
without CLI intervention. The CLI is needed to set per-region local fields and to
create the admin user. `make admin ARGS="..."` wraps
`docker compose exec sidecar sidecar-admin "$@"` (the image's `SIDECAR_DB` env is
honored by `sidecar-admin`, so no `--db`). README documents the first run as:
`make up` → `make admin ARGS="region set --id 1 --agency-id 1 --timezone
America/Los_Angeles"` → `make admin ARGS="user create --username admin"`.

## 5. Production topology (`render.yaml`)

Same two services, same variable names.

**`sidecar`** — `type: web`, `runtime: docker` (`dockerfilePath: ./Dockerfile`; Render
builds the image from the repo), paid plan (disks require one), `disk: {name:
sidecar-data, mountPath: /data, sizeGB: 1}`, `healthCheckPath: /healthz`. Env:
`PORT: 8080` (the sidecar does not read `PORT`; it binds `--addr` default `:8080`, and
setting `PORT` explicitly makes Render's port detection deterministic),
`SIDECAR_DB=/data/sidecar.db`, `SIDECAR_GORUSH_URL` built from
`fromService: {name: gorush, type: pserv, property: hostport}` (private hostnames carry
a random suffix — `gorush-2j3e:8088` — and cannot be hardcoded), API keys and
`SIDECAR_APNS_TOPIC` as `sync: false` secrets, webhook secret via `fromGroup`.

Consequence of the disk: one instance, deploys restart rather than roll. Acceptable for
this workload; stated in README.

**`gorush`** — `type: pserv`, `runtime: image`, `image: {url:
docker.io/appleboy/gorush:1.22.0}` (public, no `creds`), same `GORUSH_*` env as
locally. `GORUSH_CORE_FEEDBACK_HOOK_URL` is built from `fromService: {name: sidecar,
type: web, property: hostport}` + `/webhooks/gorush`. Port 8080 is used for the
private hop (Render forbids 10000 on the private network). The APNs key remains a
base64 env secret; no files.

**Shared secret.** A top-level `envVarGroups` entry holds
`SIDECAR_GORUSH_WEBHOOK_SECRET`; *both* services reference it with `fromGroup`, and
gorush's `GORUSH_CORE_FEEDBACK_HEADER` is set from the same value so it cannot drift.
(If Blueprint cannot compose `authorization:` + a group value, the header is a second
`sync: false` secret and the README states both must match.)

**Proxy behavior.** Render terminates TLS at its load balancer and re-originates TCP.
Two consequences:
- The per-IP throttles (push registrations, surveys, ghost bus) key on the proxy
  address and merge every client into one bucket. Known limitation, documented, *not*
  fixed here.
- The admin session code needs `Host` preserved and `X-Forwarded-Proto: https`
  (`internal/httpapi/session.go`, `middleware.go`). Render docs do not state either
  explicitly, so this is *expected and verified on first deploy*: log in at `/admin`,
  confirm the session cookie carries `Secure`, and confirm an admin write succeeds.

**Admin CLI in prod:** `render ssh sidecar`, then `sidecar-admin …` (`SIDECAR_DB` is set
in the image).

What differs between local and prod: how service addresses are supplied (compose DNS
names vs. Render `fromService`) and where secrets come from. Ports are 8080/8088 in
both.

## 6. Configuration (`.env.example`)

Grouped, each line commented with how to obtain the value:

```
# --- sidecar ---
SIDECAR_DB=./sidecar.db                        # host `make run` only; the image sets /data/sidecar.db
SIDECAR_OBA_API_KEY=
SIDECAR_PIRATE_WEATHER_KEY=
SIDECAR_GORUSH_URL=http://localhost:8088      # host `make run`; compose overrides to http://gorush:8088
SIDECAR_GORUSH_WEBHOOK_SECRET=                 # openssl rand -hex 32 — no ':' or whitespace
SIDECAR_APNS_TOPIC=                            # iOS app bundle id, e.g. org.onebusaway.iphone
# --- gorush / APNs (base64 -i AuthKey_XXXXXXXXXX.p8 | tr -d '\n') ---
GORUSH_IOS_KEY_BASE64=
GORUSH_IOS_KEY_ID=
GORUSH_IOS_TEAM_ID=
# --- gorush / FCM (optional) ---
GORUSH_ANDROID_ENABLED=false
GORUSH_ANDROID_CREDENTIAL=
# --- compose ---
COMPOSE_PROFILES=full
```

`.env` stays gitignored. Real environment variables win over the file (existing
`dotenv.Load` behavior; a missing file is not an error, so the container's `/data`
workdir is fine).

## 7. Verification

- **Go:** `push` tests cover the topic field (present for iOS, absent for Android);
  `httpapi` router test covers `/healthz`; `make check` passes under both `test-tz`
  zones.
- **Header env var:** start gorush 1.22.0 with `GORUSH_CORE_FEEDBACK_HEADER=authorization:x`
  and confirm `GET /api/config` echoes it under `core.feedback_header`. Decides whether
  the `gorush.yml` fallback is needed.
- **Image:** `make image` builds; `docker run --rm <img> sidecar --help` and
  `sidecar-admin --help` succeed; `deploy/smoke.sh` passes against a running container.
- **Compose, happy path:** `make up` → register a real device token with `curl` (or from
  the debug app) → create an alarm → `make logs` shows gorush accept it → push lands on
  the phone.
- **Compose, feedback path:** register a garbage token, fire an alarm, observe
  `/webhooks/gorush` receive `BadDeviceToken` and prune the registration.
- **Shortcut:** `make up-gorush` + `make run` reproduces both paths with the sidecar on
  the host.
- **Render:** `render.yaml` validated against the Blueprint schema. The actual first
  deploy needs the owner's account; the plan ships a first-deploy checklist (create
  Blueprint, set secrets, confirm `/healthz` live, `render ssh` → `region set` /
  `user create`, verify `Secure` cookie + an admin write, then the same curl sequence
  as local).

## 8. Out of scope

Postgres; a fake push receiver; tunnels; trusting `X-Forwarded-For`; a CI workflow that
publishes images (Render builds from the repo); Live Activities end-to-end (needs the
`.push-type.liveactivity` topic suffix and `push_type`, a separate change).
