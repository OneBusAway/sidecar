# Local and Render Orchestration Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Run the sidecar plus a real appleboy/gorush push gateway with one `make up` locally, and deploy the identical two-service topology to Render from a committed `render.yaml`.

**Architecture:** One multi-stage `Dockerfile` produces an image with both `sidecar` and `sidecar-admin` and the embedded admin SPA. `compose.yaml` runs that image beside `appleboy/gorush:1.22.0` for local; `render.yaml` declares the same pair (web service + disk, private service). Two small Go changes make real pushes possible: an APNs topic on outgoing iOS pushes, and a `/healthz` route for Render's health check.

**Tech Stack:** Go 1.26, SvelteKit (adapter-static), Docker 29 / Compose v5, appleboy/gorush 1.22.0, Render Blueprints.

**Spec:** `docs/superpowers/specs/2026-08-23-local-and-render-orchestration-design.md`

## Global Constraints

- `time.Now`/`time.Local` are banned outside `cmd/` and `_test.go` (golangci forbidigo). Nothing in this plan needs a clock outside `cmd/`.
- Every exported identifier and package needs a doc comment (revive). `nolint` needs a specific linter and an explanation.
- gorush image pinned to `appleboy/gorush:1.22.0`. Go build image `golang:1.26`, SPA build image `node:24`.
- Ports: sidecar `8080`, gorush `8088` — identical locally and on Render. Render's private network forbids port 10000.
- Webhook secret must contain no `:` or whitespace (gorush splits `feedback_header` entries on `:`); the bare `Authorization: <secret>` form is used everywhere.
- `make check` must pass at the end of every task that touches Go. Run it as `make check`, not `go test ./...` (the adminui embed test needs `make web` first).
- Commit after each task. Branch: `docker`.
- Docker Desktop for Mac is the only tested local runtime; `host.docker.internal` is assumed to resolve to the host.

---

### Task 1: Verify gorush honors `GORUSH_CORE_FEEDBACK_HEADER` (spike, no commit unless it fails)

The spec assumes no gorush config file is needed. Prove it before anything depends on it.

**Files:** none (throwaway).

- [ ] **Step 1: Run gorush with the env var and read back its config**

```sh
docker run --rm -d --name gorush-spike -p 8088:8088 \
  -e GORUSH_CORE_FEEDBACK_HOOK_URL=http://example.invalid/webhooks/gorush \
  -e GORUSH_CORE_FEEDBACK_HEADER=authorization:spikesecret \
  appleboy/gorush:1.22.0
sleep 2
curl -s localhost:8088/api/config | grep -A2 -i feedback
docker rm -f gorush-spike
```

Expected: output contains `feedback_hook_url: http://example.invalid/webhooks/gorush` and `feedback_header:` followed by `- authorization:spikesecret`.

- [ ] **Step 2: Record the outcome**

If the header appears: proceed; no `deploy/gorush.yml` in later tasks.

If it does NOT appear: in Task 5 add a file `deploy/gorush.yml` with

```yaml
core:
  feedback_hook_url: "http://sidecar:8080/webhooks/gorush"
  feedback_header:
    - "authorization:REPLACE_ME"
```

mounted at `/home/gorush/config.yml`, and note in `.env.example` that the secret must be hand-edited into that file. Write the result as one line at the top of Task 5 before continuing.

---

### Task 2: APNs topic on iOS pushes

**Files:**
- Modify: `internal/push/gorush.go` (struct `gorushNotification`, `Gorush`, `NewGorush`, `Send`)
- Modify: `internal/push/gorush_test.go` (every `NewGorush(` call gains a topic argument)
- Modify: `cmd/sidecar/main.go` (new flag, call site at ~line 191, boot warning)
- Modify: `cmd/sidecar/main_test.go` (flag test)

**Interfaces:**
- Produces: `func NewGorush(baseURL, apnsTopic string, httpClient *http.Client) *Gorush`. Wire field `"topic"` on iOS notifications only. Flag `--apns-topic`, env `SIDECAR_APNS_TOPIC`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/push/gorush_test.go`:

```go
// TestGorushSendSetsAPNsTopicForIOSOnly pins the one field APNs token auth
// cannot live without: under .p8 auth Apple requires apns-topic on every
// request and gorush passes it through verbatim from the notification's
// "topic". Android has no such concept, so the field must be absent there
// rather than sent as an empty string.
func TestGorushSendSetsAPNsTopicForIOSOnly(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		platform  Platform
		wantTopic string
		wantSet   bool
	}{
		{"ios carries topic", PlatformIOS, "org.onebusaway.iphone", true},
		{"android omits topic", PlatformAndroid, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var captured map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Errorf("read body: %v", err)
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				var req map[string]any
				if err := json.Unmarshal(body, &req); err != nil {
					t.Errorf("unmarshal body: %v", err)
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				captured = req["notifications"].([]any)[0].(map[string]any)
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()

			g := NewGorush(server.URL, "org.onebusaway.iphone", server.Client())
			err := g.Send(context.Background(), Notification{
				Tokens: []string{"tok"}, Platform: tc.platform, Message: "hi",
			})
			if err != nil {
				t.Fatalf("Send: %v", err)
			}
			got, set := captured["topic"]
			if set != tc.wantSet {
				t.Fatalf("topic present = %v, want %v (payload %v)", set, tc.wantSet, captured)
			}
			if set && got != tc.wantTopic {
				t.Errorf("topic = %v, want %q", got, tc.wantTopic)
			}
		})
	}
}
```

Update every existing `NewGorush(x, y)` call in `gorush_test.go` (lines ~38, 171, 203, 241, 295, 298, 309, 325) to `NewGorush(x, "", y)` so the file compiles once the signature changes.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/push -run TestGorushSendSetsAPNsTopicForIOSOnly`
Expected: compile error `too many arguments in call to NewGorush`.

- [ ] **Step 3: Implement**

In `internal/push/gorush.go`:

```go
type gorushNotification struct {
	Tokens      []string       `json:"tokens"`
	Platform    int            `json:"platform"`
	Title       string         `json:"title,omitempty"`
	Message     string         `json:"message"`
	Priority    string         `json:"priority"`
	Development bool           `json:"development,omitempty"`
	// Topic is the APNs topic (the app's bundle id). Required by Apple under
	// token-based (.p8) auth -- without it every push bounces MissingTopic --
	// and gorush has no global setting for it, so it rides on each request.
	Topic string         `json:"topic,omitempty"`
	Data  map[string]any `json:"data,omitempty"`
}

// Gorush is a Sender backed by one gorush instance's HTTP push API.
type Gorush struct {
	pushURL   string
	apnsTopic string
	http      *http.Client
}

// NewGorush builds a Gorush that posts to baseURL's /api/push and stamps
// apnsTopic (the iOS app's bundle id) onto every iOS notification; an empty
// topic is sent as no field, which APNs rejects under .p8 auth, so callers
// should treat empty as misconfiguration (main warns at boot). A nil
// httpClient defaults to http.DefaultClient. If the given client has no
// Timeout set, NewGorush uses a copy with a 10-second Timeout rather than
// the caller's client as-is -- see gorushTimeout -- following
// httpx.NoRedirectClient's copy-don't-mutate rule so a shared
// http.DefaultClient is never altered out from under other callers.
func NewGorush(baseURL, apnsTopic string, httpClient *http.Client) *Gorush {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	client := httpx.NoRedirectClient(httpClient)
	if client.Timeout == 0 {
		client.Timeout = gorushTimeout
	}
	return &Gorush{
		pushURL:   strings.TrimRight(baseURL, "/") + "/api/push",
		apnsTopic: apnsTopic,
		http:      client,
	}
}
```

In `Send`, replace the existing `if n.Platform == PlatformIOS { gn.Development = n.Sandbox }` with:

```go
	if n.Platform == PlatformIOS {
		gn.Development = n.Sandbox
		gn.Topic = g.apnsTopic
	}
```

- [ ] **Step 4: Run push tests**

Run: `go test ./internal/push`
Expected: PASS.

- [ ] **Step 5: Write the failing main test**

In `cmd/sidecar/main.go` the flag does not exist yet. Append to `cmd/sidecar/main_test.go`:

```go
// TestRun_APNsTopicFlagParses pins that --apns-topic is a recognised flag:
// a typo'd or missing definition would make every deployment that sets it
// fail at boot with "flag provided but not defined".
func TestRun_APNsTopicFlagParses(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	err := run(&stdout, &stderr, []string{"--apns-topic", "org.example.app", "--help"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !strings.Contains(stdout.String(), "apns-topic") {
		t.Errorf("usage output lacks apns-topic:\n%s", stdout.String())
	}
}
```

(Add `bytes` and `strings` to the imports if not already present; check the top of the file.)

- [ ] **Step 6: Run to verify it fails**

Run: `go test ./cmd/sidecar -run TestRun_APNsTopicFlagParses`
Expected: FAIL, `flag provided but not defined: -apns-topic`.

- [ ] **Step 7: Add the flag and wire it**

In `cmd/sidecar/main.go` `run`, after the `gorushURL` flag definition:

```go
	apnsTopic := fs.String("apns-topic", envOrDefault("SIDECAR_APNS_TOPIC", ""),
		"APNs topic (the iOS app's bundle id) stamped on every iOS push; required for pushes to be accepted under .p8 token auth")
```

Replace the sender block:

```go
	var sender push.Sender
	if *gorushURL == "" {
		logger.Warn("no --gorush-url/SIDECAR_GORUSH_URL set; departure alarms will be stored and reaped but never fire")
	} else {
		if *apnsTopic == "" {
			logger.Warn("no --apns-topic/SIDECAR_APNS_TOPIC set; iOS pushes will be rejected by APNs with MissingTopic")
		}
		sender = push.NewGorush(*gorushURL, *apnsTopic, http.DefaultClient)
	}
```

- [ ] **Step 8: Run the full check**

Run: `make check`
Expected: PASS (gofmt, vet, lint, tests under both zones, race, web-check).

- [ ] **Step 9: Commit**

```sh
git add internal/push cmd/sidecar
git commit -m "feat(push): stamp APNs topic on iOS notifications; add --apns-topic/SIDECAR_APNS_TOPIC"
```

---

### Task 3: `GET /healthz`

**Files:**
- Create: `internal/httpapi/health.go`
- Create: `internal/httpapi/health_test.go`
- Modify: `internal/httpapi/router.go` (`NewRouter`, right after `mux := http.NewServeMux()`)

**Interfaces:**
- Produces: route `GET /healthz` → `200 text/plain "ok\n"`, registered unconditionally.

- [ ] **Step 1: Write the failing test**

`internal/httpapi/health_test.go`:

```go
package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestHealthz pins the liveness route Render's health check and compose's
// smoke script depend on. It must not touch the database or any
// per-feature dependency: a fresh deploy has no regions yet, and the health
// check has to pass before anyone can ssh in to create them.
func TestHealthz(t *testing.T) {
	t.Parallel()
	h := NewRouter(Deps{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != "ok\n" {
		t.Errorf("body = %q, want %q", got, "ok\n")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/httpapi -run TestHealthz`
Expected: FAIL, `status = 404, want 200`.

- [ ] **Step 3: Implement**

`internal/httpapi/health.go`:

```go
package httpapi

import "net/http"

// healthz is the liveness probe. It deliberately checks nothing: its job is
// to say "the process is up and routing", which is what a platform health
// check needs before a fresh deployment has any regions or users to query.
func healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n")) //nolint:errcheck // a failed write to a probe has no one to report to
}
```

In `router.go`, immediately after `mux := http.NewServeMux()`:

```go
	mux.HandleFunc("GET /healthz", healthz)
```

- [ ] **Step 4: Run tests and lint**

Run: `make check`
Expected: PASS.

- [ ] **Step 5: Commit**

```sh
git add internal/httpapi/health.go internal/httpapi/health_test.go internal/httpapi/router.go
git commit -m "feat(httpapi): add GET /healthz liveness route"
```

---

### Task 4: Dockerfile, .dockerignore, `make image`, smoke script

**Files:**
- Create: `Dockerfile`
- Create: `.dockerignore`
- Create: `deploy/smoke.sh`
- Modify: `Makefile` (new `image` target under "Build & run"; add `image` to `help` automatically via the `##` convention)

**Interfaces:**
- Produces: image tag `sidecar:local` with `sidecar` and `sidecar-admin` on `PATH`, `WORKDIR /data`, `ENV SIDECAR_DB=/data/sidecar.db`, `EXPOSE 8080`. Script `deploy/smoke.sh <base-url>` exits 0 when healthy.

- [ ] **Step 1: Write `.dockerignore`**

```
.git
bin/
*.db
*.db-wal
*.db-shm
coverage.out
.env
docs/
.superpowers/
.claude/
web/admin/node_modules/
web/admin/build/
web/admin/.svelte-kit/
internal/httpapi/adminui/dist/*
!internal/httpapi/adminui/dist/.gitkeep
```

- [ ] **Step 2: Write `Dockerfile`**

```dockerfile
# syntax=docker/dockerfile:1

# --- Stage 1: admin SPA ------------------------------------------------------
FROM node:24-alpine AS web
WORKDIR /src/web/admin
COPY web/admin/package.json web/admin/package-lock.json ./
RUN npm ci
COPY web/admin/ ./
RUN npm run build

# --- Stage 2: Go binaries ----------------------------------------------------
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# The SPA is embedded via //go:embed all:dist, so it must sit in the tree
# before go build. Copy from the web stage rather than trusting whatever the
# developer's local dist/ holds.
RUN rm -rf internal/httpapi/adminui/dist && mkdir -p internal/httpapi/adminui/dist
COPY --from=web /src/web/admin/build/ internal/httpapi/adminui/dist/
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/sidecar ./cmd/sidecar \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/sidecar-admin ./cmd/sidecar-admin

# --- Stage 3: runtime --------------------------------------------------------
FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata \
 && addgroup -S sidecar && adduser -S -G sidecar sidecar \
 && mkdir -p /data && chown sidecar:sidecar /data
COPY --from=build /out/sidecar /out/sidecar-admin /usr/local/bin/
USER sidecar
WORKDIR /data
ENV SIDECAR_DB=/data/sidecar.db
EXPOSE 8080
ENTRYPOINT ["sidecar"]
```

Notes for the implementer: `modernc.org/sqlite` is pure Go, so `CGO_ENABLED=0` is correct. `tzdata` is needed because `sidecar-admin region set --timezone` loads IANA zones. The `ENTRYPOINT` is the server; `sidecar-admin` is run via `docker compose exec` (Task 5), which bypasses the entrypoint.

- [ ] **Step 3: Write `deploy/smoke.sh`**

```sh
#!/bin/sh
# Smoke-test a running sidecar. Usage: deploy/smoke.sh [base-url]
# Exit 0 when /healthz and /admin both answer 200. The alerts feed is
# checked only if a region exists yet (it 404s on a fresh database).
set -eu
base="${1:-http://localhost:8080}"

check() {
  path="$1"; want="$2"
  got="$(curl -s -o /dev/null -w '%{http_code}' "$base$path")"
  if [ "$got" != "$want" ]; then
    echo "FAIL $path -> $got (want $want)" >&2
    exit 1
  fi
  echo "ok   $path -> $got"
}

check /healthz 200
check /admin 200            # proves the SPA embedded (503 means it did not)
alerts="$(curl -s -o /dev/null -w '%{http_code}' "$base/api/v1/regions/1/alerts.pbtext")"
case "$alerts" in
  200) echo "ok   /api/v1/regions/1/alerts.pbtext -> 200" ;;
  404) echo "skip /api/v1/regions/1/alerts.pbtext -> 404 (no regions synced yet)" ;;
  *)   echo "FAIL /api/v1/regions/1/alerts.pbtext -> $alerts" >&2; exit 1 ;;
esac
```

Then `chmod +x deploy/smoke.sh`.

- [ ] **Step 4: Add the Makefile target**

Under `## --- Build & run` after `build`:

```makefile
IMAGE ?= sidecar:local

.PHONY: image
image: ## Build the container image (SPA + both binaries)
	docker build -t $(IMAGE) .
```

- [ ] **Step 5: Build and verify**

```sh
make image
docker run --rm sidecar:local --help | head -3
docker run --rm --entrypoint sidecar-admin sidecar:local --help | head -3
docker run --rm -d --name sidecar-smoke -p 8080:8080 sidecar:local
sleep 2
deploy/smoke.sh http://localhost:8080
docker rm -f sidecar-smoke
```

Expected: both `--help` outputs print usage; smoke prints `ok /healthz`, `ok /admin`, and either `ok` or `skip` for the alerts line (the container syncs regions from the public directory at boot, so `ok` is likely after a few seconds).

- [ ] **Step 6: Commit**

```sh
git add Dockerfile .dockerignore deploy/smoke.sh Makefile
git commit -m "build: multi-stage Dockerfile, make image, smoke script"
```

---

### Task 5: `compose.yaml`, `.env.example`, make targets

*(If Task 1 failed, first line here: "gorush.yml fallback REQUIRED" and add the file + mount described in Task 1.)*

**Files:**
- Create: `compose.yaml`
- Modify: `.env.example` (replace wholesale)
- Modify: `Makefile` (new section `## --- Local stack`)

**Interfaces:**
- Produces: `make up`, `make up-gorush`, `make down`, `make logs`, `make admin ARGS="..."`.

- [ ] **Step 1: Write `.env.example`**

```
# Local development configuration. Copy to .env and fill in. The sidecar
# loads it at boot (real environment variables win), and docker compose
# reads it for the gorush container. Never commit .env.

# --- sidecar ---------------------------------------------------------------
# Host `make run` only; the container image sets /data/sidecar.db itself.
SIDECAR_DB=./sidecar.db
# Default OneBusAway REST API key (regions without their own key inherit it).
SIDECAR_OBA_API_KEY=
# Pirate Weather key; without it the weather endpoint returns 403.
SIDECAR_PIRATE_WEATHER_KEY=
# Host `make run` talks to the compose-exposed gorush port. compose.yaml
# overrides this to http://gorush:8088 for the containerised sidecar.
SIDECAR_GORUSH_URL=http://localhost:8088
# Shared secret gorush sends on the feedback webhook. Generate with
# `openssl rand -hex 32`. Must contain no ':' or whitespace -- gorush splits
# header entries on ':'.
SIDECAR_GORUSH_WEBHOOK_SECRET=
# The iOS app's bundle id. APNs requires it under .p8 auth.
SIDECAR_APNS_TOPIC=org.onebusaway.iphone

# --- gorush / APNs -----------------------------------------------------------
# base64 -i AuthKey_XXXXXXXXXX.p8 | tr -d '\n'
GORUSH_IOS_KEY_BASE64=
# The 10-character key id from the Apple developer portal.
GORUSH_IOS_KEY_ID=
# The 10-character team id.
GORUSH_IOS_TEAM_ID=

# --- gorush / FCM (optional) ------------------------------------------------
GORUSH_ANDROID_ENABLED=false
# FCM service-account credential (see gorush's android.credential docs).
GORUSH_ANDROID_CREDENTIAL=

# --- compose ------------------------------------------------------------------
# `full` starts both containers. `make up-gorush` ignores this.
COMPOSE_PROFILES=full
```

Before finalising `GORUSH_ANDROID_CREDENTIAL`'s comment, check gorush 1.22.0's `notify/notification_fcm.go`: `curl -s https://raw.githubusercontent.com/appleboy/gorush/v1.22.0/notify/notification_fcm.go | grep -n -i 'credential' | head`. If it expects raw JSON, say so in the comment; if it expects a base64 string, say that.

- [ ] **Step 2: Write `compose.yaml`**

```yaml
# Local stack: the sidecar beside a real gorush. `make up` starts both;
# `make up-gorush` starts only gorush so the sidecar can run on the host via
# `make run`. Ports match production (Render) exactly: 8080 and 8088.
name: sidecar

services:
  sidecar:
    profiles: [full]
    build: .
    image: sidecar:local
    ports:
      - "8080:8080"
    volumes:
      - sidecar-data:/data
    env_file: .env
    environment:
      # Inside the compose network the gateway is the gorush service, not
      # localhost; this overrides the host-oriented value in .env.
      SIDECAR_GORUSH_URL: http://gorush:8088
    depends_on:
      gorush:
        condition: service_healthy
    restart: unless-stopped

  gorush:
    image: appleboy/gorush:1.22.0
    ports:
      - "8088:8088"
    environment:
      GORUSH_CORE_PORT: "8088"
      GORUSH_IOS_ENABLED: "true"
      GORUSH_IOS_KEY_TYPE: p8
      GORUSH_IOS_KEY_BASE64: ${GORUSH_IOS_KEY_BASE64}
      GORUSH_IOS_KEY_ID: ${GORUSH_IOS_KEY_ID}
      GORUSH_IOS_TEAM_ID: ${GORUSH_IOS_TEAM_ID}
      # Production is the default route; the sidecar flips individual pushes
      # to the sandbox with the per-notification `development` flag.
      GORUSH_IOS_PRODUCTION: "true"
      GORUSH_ANDROID_ENABLED: ${GORUSH_ANDROID_ENABLED:-false}
      GORUSH_ANDROID_CREDENTIAL: ${GORUSH_ANDROID_CREDENTIAL:-}
      # Feedback webhook. FEEDBACK_HOOK_HOST is overridden by `make up-gorush`
      # so a host-run sidecar still receives prune signals.
      GORUSH_CORE_FEEDBACK_HOOK_URL: http://${FEEDBACK_HOOK_HOST:-sidecar}:8080/webhooks/gorush
      GORUSH_CORE_FEEDBACK_HEADER: authorization:${SIDECAR_GORUSH_WEBHOOK_SECRET}
    # The image ships its own HEALTHCHECK (gorush --ping); nothing to add.
    restart: unless-stopped

volumes:
  sidecar-data:
```

- [ ] **Step 3: Add Makefile targets**

New section after `## --- Build & run`:

```makefile
## --- Local stack -----------------------------------------------------------

.PHONY: up
up: ## Start sidecar + gorush in Docker (reads .env)
	docker compose up --build -d

.PHONY: up-gorush
up-gorush: ## Start only gorush; run the sidecar on the host with `make run`
	FEEDBACK_HOOK_HOST=host.docker.internal docker compose up -d gorush

.PHONY: down
down: ## Stop the local stack (data volume is kept)
	docker compose down

.PHONY: logs
logs: ## Follow local stack logs
	docker compose logs -f

.PHONY: admin
admin: ## Run sidecar-admin inside the container (make admin ARGS="region list")
	docker compose exec sidecar sidecar-admin $(ARGS)
```

- [ ] **Step 4: Verify the full stack**

```sh
cp -n .env.example .env   # then fill SIDECAR_GORUSH_WEBHOOK_SECRET and the GORUSH_IOS_* values
make up
sleep 5
docker compose ps                       # both "healthy"/"running"
deploy/smoke.sh
curl -s localhost:8088/api/config | grep -A1 feedback_header
make admin ARGS="region list"
make admin ARGS="region set --id 1 --agency-id 1 --timezone America/Los_Angeles"
make admin ARGS="user create --username admin"   # prompts for a password
```

Expected: `smoke.sh` all ok; gorush config shows `authorization:<your secret>`; `region list` prints regions (the server synced them at boot); `user create` succeeds.

- [ ] **Step 5: Verify the shortcut**

```sh
make down
make up-gorush
make run &
sleep 3
deploy/smoke.sh
curl -s localhost:8088/api/config | grep feedback_hook_url    # host.docker.internal
kill %1
make down
```

Expected: hook URL is `http://host.docker.internal:8080/webhooks/gorush`.

- [ ] **Step 6: Commit**

```sh
git add compose.yaml .env.example Makefile
git commit -m "build: docker compose stack for sidecar + gorush with host-run shortcut"
```

---

### Task 6: End-to-end push verification (manual, records results in the plan)

This task produces no code; it proves the stack does what the spec promises. Requires a real APNs key in `.env` and a device token from a debug build of the app registered against `http://<mac-ip>:8080`.

- [ ] **Step 1: Happy path**

With `make up` running and a real token (`$TOKEN`) from the app:

```sh
curl -s -X POST localhost:8080/api/v2/regions/1/push_registrations \
  -d "user_push_id=$TOKEN" -d "platform=ios" -d "apns_sandbox=1"
# then create an alarm through the app for a departure ~2 minutes out, or:
curl -s -X POST localhost:8080/api/v2/regions/1/alarms \
  -d "user_push_id=$TOKEN" -d "apns_sandbox=1" \
  -d "stop_id=1_75403" -d "trip_id=<a live trip>" -d "service_date=<ms>" \
  -d "seconds_before=60"
make logs | grep -i 'push\|gorush'
```

Expected: a push arrives on the phone; logs show gorush returning 200.

(Consult `specification/specification.md` §5.2 for the exact alarm parameter names before running; the list above is indicative.)

- [ ] **Step 2: Feedback path**

```sh
curl -s -X POST localhost:8080/api/v2/regions/1/push_registrations \
  -d "user_push_id=deadbeef" -d "platform=ios" -d "apns_sandbox=1"
# create an alarm for that token as above
make logs | grep -i 'webhooks/gorush\|BadDeviceToken\|prune'
```

Expected: gorush reports `BadDeviceToken`, the sidecar logs the prune, and the registration is gone.

- [ ] **Step 3: Record**

Append a `## Verification log` section to the bottom of this plan with date, gorush tag, and pass/fail for each step, then commit:

```sh
git add docs/superpowers/plans/2026-08-23-local-and-render-orchestration.md
git commit -m "docs: record end-to-end push verification"
```

---

### Task 7: `render.yaml`

**Files:**
- Create: `render.yaml`

**Interfaces:**
- Produces: a Blueprint with services `sidecar` (web, docker runtime, disk) and `gorush` (pserv, image runtime), env group `sidecar-shared`.

- [ ] **Step 1: Write `render.yaml`**

```yaml
# Render Blueprint: the same two services compose.yaml runs locally.
# Docs: https://render.com/docs/blueprint-spec
#
# Secrets marked `sync: false` are entered in the dashboard on first deploy.
# Ports are 8080 (sidecar) and 8088 (gorush) on the private network, same as
# local; Render forbids 10000 there.

envVarGroups:
  - name: sidecar-shared
    envVars:
      # openssl rand -hex 32 -- no ':' or whitespace.
      - key: SIDECAR_GORUSH_WEBHOOK_SECRET
        sync: false

services:
  - type: web
    name: sidecar
    runtime: docker
    dockerfilePath: ./Dockerfile
    plan: starter
    region: oregon
    healthCheckPath: /healthz
    disk:
      name: sidecar-data
      mountPath: /data
      sizeGB: 1
    envVars:
      - fromGroup: sidecar-shared
      - key: PORT
        value: "8080"
      - key: SIDECAR_DB
        value: /data/sidecar.db
      - key: SIDECAR_GORUSH_URL
        fromService:
          name: gorush
          type: pserv
          property: hostport
      - key: SIDECAR_APNS_TOPIC
        sync: false
      - key: SIDECAR_OBA_API_KEY
        sync: false
      - key: SIDECAR_PIRATE_WEATHER_KEY
        sync: false

  - type: pserv
    name: gorush
    runtime: image
    image:
      url: docker.io/appleboy/gorush:1.22.0
    plan: starter
    region: oregon
    envVars:
      - fromGroup: sidecar-shared
      - key: GORUSH_CORE_PORT
        value: "8088"
      - key: GORUSH_IOS_ENABLED
        value: "true"
      - key: GORUSH_IOS_KEY_TYPE
        value: p8
      - key: GORUSH_IOS_PRODUCTION
        value: "true"
      - key: GORUSH_IOS_KEY_BASE64
        sync: false
      - key: GORUSH_IOS_KEY_ID
        sync: false
      - key: GORUSH_IOS_TEAM_ID
        sync: false
      - key: GORUSH_CORE_FEEDBACK_HOOK_HOSTPORT
        fromService:
          name: sidecar
          type: web
          property: hostport
      # Blueprint cannot concatenate values, so the two derived settings
      # below are entered by hand on first deploy (see README):
      #   GORUSH_CORE_FEEDBACK_HOOK_URL = http://<GORUSH_CORE_FEEDBACK_HOOK_HOSTPORT>/webhooks/gorush
      #   GORUSH_CORE_FEEDBACK_HEADER   = authorization:<SIDECAR_GORUSH_WEBHOOK_SECRET>
      - key: GORUSH_CORE_FEEDBACK_HOOK_URL
        sync: false
      - key: GORUSH_CORE_FEEDBACK_HEADER
        sync: false
```

Implementer notes:
- `SIDECAR_GORUSH_URL` from `hostport` yields `gorush-xxxx:8088` **without a scheme**, and `push.NewGorush` just appends `/api/push`, producing an invalid URL. Fix it in `cmd/sidecar/main.go` with a helper, TDD:

  Test (append to `cmd/sidecar/main_test.go`):

  ```go
  // TestNormalizeGorushURL pins the scheme default that lets Render's
  // fromService hostport ("gorush-abcd:8088") be used verbatim.
  func TestNormalizeGorushURL(t *testing.T) {
  	t.Parallel()
  	cases := map[string]string{
  		"":                   "",
  		"gorush:8088":        "http://gorush:8088",
  		"http://gorush:8088": "http://gorush:8088",
  		"https://g.example":  "https://g.example",
  	}
  	for in, want := range cases {
  		if got := normalizeGorushURL(in); got != want {
  			t.Errorf("normalizeGorushURL(%q) = %q, want %q", in, got, want)
  		}
  	}
  }
  ```

  Implementation (in `main.go`, add `strings` to imports):

  ```go
  // normalizeGorushURL defaults a scheme-less gateway address to http://.
  // Render's Blueprint fromService "hostport" property hands out
  // "name-xxxx:8088" with no scheme, and the private network is plain HTTP.
  func normalizeGorushURL(s string) string {
  	if s == "" || strings.Contains(s, "://") {
  		return s
  	}
  	return "http://" + s
  }
  ```

  and in `run`, right after `fs.Parse`: `*gorushURL = normalizeGorushURL(*gorushURL)`.
- Whether `hostport` carries a scheme, and whether Blueprint accepts `fromService` on a `pserv`→`web` reference, must be checked against https://render.com/docs/blueprint-spec (`fromService` properties: `host`, `port`, `hostport`, `connectionString`). Adjust the two comment lines if the docs differ.

- [ ] **Step 2: Validate the Blueprint**

Load the render skill for the current schema: use the `render-deploy` skill's validation guidance, or `npx ctx7@latest library Render "render.yaml blueprint spec fromService hostport pserv image runtime"` then `npx ctx7@latest docs <id> "<same question>"`. Confirm every key used above exists. Fix any that do not.

- [ ] **Step 3: Run `make check` (for the Go helper) and commit**

```sh
make check
git add render.yaml cmd/sidecar
git commit -m "deploy: Render Blueprint for sidecar (web+disk) and gorush (pserv); default gorush URL scheme"
```

---

### Task 8: README and CLAUDE.md

**Files:**
- Modify: `README.md` — replace `### Deployment` (lines 370–405) and extend `## Development` (line 428); add a new `## Running locally with Docker` section before `## Development`.
- Modify: `CLAUDE.md` — Commands section.

- [ ] **Step 1: Add `## Running locally with Docker` to README** (insert before `## Development`)

```markdown
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

`.env` already points `SIDECAR_GORUSH_URL` at `localhost:8088`, and gorush's
feedback hook is redirected to `host.docker.internal:8080` so token prunes
still arrive. `make down` stops either arrangement; the SQLite volume
(`sidecar-data`) survives.

APNs under `.p8` token auth requires a topic on every push, so
`SIDECAR_APNS_TOPIC` (the iOS bundle id) must be set wherever
`SIDECAR_GORUSH_URL` is; the server warns at boot if it is not.
```

- [ ] **Step 2: Replace `### Deployment`**

Keep the existing three paragraphs on `Host`/`X-Forwarded-Proto`, the TCP-peer throttle, and the webhook secret verbatim, then append:

```markdown
#### Render

`render.yaml` declares the same two services: `sidecar` as a web service
with a persistent disk at `/data` for SQLite, and `gorush` as a private
service. First deploy:

1. New → Blueprint, pick this repo. Render creates both services and the
   `sidecar-shared` env group.
2. Fill the `sync: false` secrets: the webhook secret (`openssl rand -hex 32`,
   no `:`), `SIDECAR_APNS_TOPIC`, the API keys, and the three
   `GORUSH_IOS_*` values.
3. On the `gorush` service, set the two values Blueprint cannot derive:
   `GORUSH_CORE_FEEDBACK_HOOK_URL` = `http://` + the sidecar's internal
   host:port (Dashboard → sidecar → Connect → Internal) +
   `/webhooks/gorush`, and `GORUSH_CORE_FEEDBACK_HEADER` =
   `authorization:` + the webhook secret.
4. Wait for `/healthz` to go green, then `render ssh sidecar` and run
   `sidecar-admin region set …` and `sidecar-admin user create …`
   (`SIDECAR_DB` is already set in the image).
5. Sign in at `https://<service>.onrender.com/admin`; confirm the session
   cookie carries `Secure` and that an admin write succeeds. This verifies
   Render preserves `Host` and sets `X-Forwarded-Proto`, which its docs do
   not state explicitly.

Because the disk pins the service to one instance, deploys restart rather
than roll. Render's proxy re-originates TCP, so every per-IP throttle in
this server shares one bucket there; that is a known limitation, not fixed
here.
```

- [ ] **Step 3: Update `## Development`**

Add `make image`, `make up`, `make up-gorush`, `make down`, `make admin` lines to the command list block.

- [ ] **Step 4: Update `CLAUDE.md` Commands**

Add after the existing `go build -o bin/sidecar-admin` line:

```
make image              # docker build -t sidecar:local .
make up / down / logs   # compose stack: sidecar + gorush (needs .env)
make up-gorush          # gorush only; pair with `make run` on the host
make admin ARGS="…"     # sidecar-admin inside the container
deploy/smoke.sh [url]   # /healthz + /admin + alerts feed check
```

and to the env-var list: `SIDECAR_APNS_TOPIC`. In the Architecture section's wiring sentence, mention `render.yaml`/`compose.yaml` mirror each other on ports 8080/8088.

- [ ] **Step 5: Commit**

```sh
git add README.md CLAUDE.md
git commit -m "docs: local Docker stack and Render deployment"
```

---

## Self-review

- **Spec coverage:** §2.1 topic → Task 2; §2.2 healthz → Task 3; §3 artifacts → Tasks 4, 5, 7, 8 (`deploy/gorush.yml` conditional on Task 1); §4 compose → Task 5; §5 Render → Task 7 (+ scheme normalization discovered while planning); §6 env → Task 5; §7 verification → Tasks 1, 4, 5, 6, 7; §8 out of scope — nothing planned.
- **Placeholders:** Task 6's alarm parameters are marked "indicative — consult spec §5.2"; that is a pointer to the normative source, not a TBD. Task 7's Render schema check is a verification step with a named source.
- **Type consistency:** `NewGorush(baseURL, apnsTopic string, httpClient *http.Client)` used identically in Tasks 2 and 7's note. `healthz` handler name used in Task 3 only. `FEEDBACK_HOOK_HOST` used in both `compose.yaml` and the `up-gorush` target.
