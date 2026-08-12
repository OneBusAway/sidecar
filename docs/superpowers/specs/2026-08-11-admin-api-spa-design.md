# Admin API + SPA — Design

**Date:** 2026-08-11
**Status:** Approved
**Builds on:** [2026-08-11-service-alerts-feed-design.md](2026-08-11-service-alerts-feed-design.md),
whose invariants (epoch-integer timestamps, repository interfaces, the `time.Now` /
`time.Local` ban below `cmd/`, region id 0 is real) remain binding here.

## 1. Scope

Add an authenticated JSON admin API and a browser UI for everything `sidecar-admin`
can do today:

- **Authentication only, no authorization.** Any logged-in user can do everything.
  Roles are a later column, not a later rearchitecture (§3.1).
- **Admin API** under `/api/admin/v1/`: session management, alert CRUD +
  publish/unpublish + translations, region listing and per-region settings.
- **SPA** built with SvelteKit, served by the sidecar itself at `/admin`, embedded
  in the Go binary.
- **Bootstrap** via `sidecar-admin user create` — the first user is created from the
  CLI against the shared database; there is no web-based signup, ever.

### Out of scope

- Authorization, roles, permissions.
- Web-based user management (create/delete users stays CLI-only in v1).
- Password reset flows, email, MFA, OAuth/SSO.
- Audit logging of admin actions.
- Rate limiting beyond the fixed failed-login delay (§4.4).
- Feed caching, and every other sidecar service not already implemented.

## 2. Decisions

### 2.1 Sessions are DB-backed opaque tokens, not JWTs

A `sessions` table holds the SHA-256 of a random 256-bit token; the browser holds the
token in an `HttpOnly` cookie. Chosen over JWT because:

- **Logout and user deletion are real revocations.** A JWT cannot be un-issued
  without a denylist — which is a sessions table with extra steps.
- **No signing-key management**, rotation, or algorithm-confusion surface.
- The cost — one indexed SQLite read per admin request — is nothing at this scale,
  and the statelessness JWTs buy is worthless for a single-binary SQLite server.

Storing the **hash** of the token means a leaked database copy (backup, misplaced
`.db` file) cannot be replayed into a live session.

### 2.2 The SPA is same-origin and embedded

SvelteKit with `adapter-static` produces a fully static client-side app, embedded via
`go:embed` and served by the sidecar at `/admin`. Same origin as the API, so:

- Plain `HttpOnly` cookies work. No CORS configuration exists anywhere in the design,
  and adding any is a red flag in review.
- Deployment stays a single binary.

In development the Vite dev server proxies `/api` to the running Go server (§6.4), so
frontend work gets hot reload without CORS either.

### 2.3 Node joins the toolchain; built assets are never committed

`mise` gains Node (current LTS) beside Go. `make web` runs the SvelteKit build into
the embed directory; `make build` and `make check` depend on it. `dist/` output is
gitignored.

`go:embed` needs the directory present even when Node hasn't run, so the embed
directory contains a committed `.gitkeep` and uses an `all:`-prefixed pattern. `all:`
is load-bearing twice over: plain patterns exclude files beginning with `.` **or
`_`** — which drops both `.gitkeep` and SvelteKit's entire `_app/` output tree. A
build without `all:` succeeds and then 404s every asset. When `index.html` is absent — a Go-only
build — `/admin` returns `503 admin UI not built; run make web`, loudly, instead of a
blank page or a panic at startup. A binary built by `make build` always has the UI.

### 2.4 The API and CLI share the repositories, not just the database

Every admin API handler calls the existing `alerts.Repository` and
`regions.Repository` — the same interfaces `sidecar-admin` uses. Domain invariants
(start-before-end, valid enums, agency-id resolution) already live at the repository
layer, so the API cannot drift from the CLI, and a future Postgres store slots in
under both unchanged. The new `auth.Repository` follows the identical pattern,
including a conformance suite in `storetest`.

### 2.5 All existing time rules apply unchanged

`internal/auth` and the new handlers take an injected `Now func() time.Time`;
forbidigo continues to ban `time.Now`/`time.Local` outside `cmd/`. Session expiry
comparisons, cookie `Expires`, and JSON timestamps all flow from the injected clock.
New columns are epoch-second `INTEGER`, never `DATETIME` (the measured
`modernc.org/sqlite` ordering inversion in the alerts spec §2.3 applies verbatim).

## 3. Data model

One goose migration adds two tables:

```sql
CREATE TABLE users (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  username      TEXT NOT NULL UNIQUE,
  -- PHC-formatted argon2id string: $argon2id$v=19$m=...,t=...,p=...$salt$hash
  -- Self-describing so parameters can be raised per-row without a migration.
  password_hash TEXT NOT NULL,
  created_at    INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL
);

CREATE TABLE sessions (
  -- Hex SHA-256 of the opaque token. The raw token never touches the database.
  token_hash  TEXT PRIMARY KEY,
  user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_at  INTEGER NOT NULL,
  expires_at  INTEGER NOT NULL
);

CREATE INDEX sessions_user_idx ON sessions (user_id);
CREATE INDEX sessions_expires_idx ON sessions (expires_at);
```

Deleting a user cascades to their sessions: deletion is immediate lockout.

### 3.1 Why a users table and not a singleton

Multiple operators get their own credentials (no shared password in a wiki), and
authorization later is `ALTER TABLE users ADD COLUMN role` — not a migration away
from a one-row model. The cost over a singleton is one CLI noun.

### 3.2 `auth.Repository`

```go
type Repository interface {
    CreateUser(ctx context.Context, username, passwordHash string, now time.Time) (User, error)
    GetUserByUsername(ctx context.Context, username string) (User, error)
    ListUsers(ctx context.Context) ([]User, error)
    DeleteUser(ctx context.Context, username string) error
    UpdatePassword(ctx context.Context, username, passwordHash string, now time.Time) error

    CreateSession(ctx context.Context, tokenHash string, userID int64, now, expiresAt time.Time) error
    // GetSession returns ErrNotFound for unknown OR expired tokens; expiry is
    // evaluated against the passed now, never the database clock. When it
    // observes an expired row it DELETES it before returning ErrNotFound --
    // a deliberate write inside a read path, part of the interface contract
    // (storetest asserts it), so every implementation including Postgres
    // must do the same.
    GetSession(ctx context.Context, tokenHash string, now time.Time) (Session, error)
    DeleteSession(ctx context.Context, tokenHash string) error
    // DeleteUserSessions revokes every session for a user; user passwd calls
    // it so a password change locks out whoever held the old password.
    DeleteUserSessions(ctx context.Context, userID int64) (int64, error)
    DeleteExpiredSessions(ctx context.Context, now time.Time) (int64, error)
}
```

`ErrNotFound` follows the `alerts.ErrNotFound` convention. Duplicate usernames
surface as `auth.ErrUsernameTaken`, mapped from the UNIQUE violation inside the
store, never by a racy pre-check SELECT.

**Username policy:** usernames are normalized in Go — trimmed of surrounding
whitespace and lowercased — before every store and every lookup, so `Admin` and
`admin` are one account on SQLite and Postgres alike. Normalization happens in
`internal/auth`, never via SQL collation (the same rule the alerts spec applies to
language tags). After trimming, a username must be 1–64 characters with no
whitespace.

## 4. Authentication mechanics

### 4.1 Passwords

- **argon2id** via `golang.org/x/crypto/argon2`, encoded in PHC string format.
- Parameters (OWASP current guidance): memory 19 MiB (`m=19456`), iterations `t=2`,
  parallelism `p=1`, 16-byte random salt, 32-byte key.
- Verification parses the stored PHC string and hashes with the *stored* row's
  parameters, so raising defaults later never breaks existing users.
- Minimum password length **12 characters** (bytes of UTF-8), maximum 512 (argon2
  has no 72-byte truncation like bcrypt, but unbounded input is a free CPU
  amplifier). Enforced in `internal/auth`, shared by CLI and any future surface.

### 4.2 Tokens and cookies

- Token: 32 bytes from `crypto/rand`, base64url-encoded, ~43 chars. Stored as hex
  SHA-256 only (§2.1).
- Cookie: name `sidecar_session`, value the raw token; `HttpOnly`; `SameSite=Lax`;
  `Path=/`; `Secure` when the request is TLS (`r.TLS != nil`) or arrived via
  `X-Forwarded-Proto: https` from a reverse proxy; `Max-Age` matching the session
  lifetime.
- Lifetime: **30 days, absolute** (no sliding renewal in v1 — renewal is a re-login).
- Logout deletes the session row *and* clears the cookie (`Max-Age=-1`).
- Every successful login also runs `DeleteExpiredSessions` — lazy garbage collection
  with no background goroutine to manage.

### 4.3 Login flow (`POST /api/admin/v1/session`)

1. Decode `{"username": ..., "password": ...}` (request body cap: 8 KB, matching the
   normative spec's §2.6 discipline).
2. Look up the user. **If absent, verify the password anyway against a fixed dummy
   PHC hash** so response timing does not reveal username existence.
3. On mismatch or unknown user: sleep a constant **500 ms**, log a warning with the
   username and remote address, return `401 {"error": "invalid credentials"}` — one
   message for both failure kinds.
4. On success: mint token, insert session, set cookie, return
   `200 {"username": ...}`.

The fixed delay is a brake on online guessing, not a substitute for rate limiting —
which is explicitly out of scope and listed in §9. The delay is an injected field
beside the injected clock (`FailDelay time.Duration`, production 500 ms, tests 0)
so the table-driven failure tests in §8 don't sleep half a second per row; a test
asserts the delay is actually applied on the failure path.

### 4.4 Middleware

- `RequireSession` wraps every `/api/admin/v1/*` route except `POST /session` and
  `DELETE /session`. Logout is deliberately outside it so it stays idempotent —
  §5's endpoint table gives it a 204 with no 401 variant, unlike `GET /session`.
  It is not an authentication hole: the handler deletes only the row whose raw
  256-bit token the caller presented, never reads the authenticated user, and
  returns the same empty 204 whether or not the token existed, so it cannot be
  used to probe token validity. Forced-logout CSRF stays closed by the
  cross-site guard below and independently by `SameSite=Lax`. The rest:
  extracts the cookie, hashes, `GetSession` with injected now. Missing, unknown, or
  expired → `401 {"error": "authentication required"}` (expired rows are deleted by
  `GetSession` itself — §3.2's contract, so the middleware needs no
  expired-vs-unknown distinction). The authenticated `User` lands in the request
  context.
- **Cross-site write protection** on every non-GET `/api/admin/v1/*` route
  **including `POST /session`** — login CSRF (logging a victim into an attacker's
  account) is cheap to close and the login handler sits outside `RequireSession`,
  so the check must not be coupled to that middleware. Rule: if `Sec-Fetch-Site` is
  present it must be `same-origin` or `none`; otherwise, if `Origin` is present its
  host must equal the request `Host`. Anything else →
  `403 {"error": "cross-site request rejected"}`. With `SameSite=Lax` this is
  defense in depth, not the only line; a token dance adds nothing for a same-origin
  SPA.
- **Reverse-proxy requirement:** the `Origin`-vs-`Host` fallback assumes the proxy
  preserves the public `Host` header (nginx: `proxy_set_header Host $host` — its
  default rewrites `Host` to the upstream address, which would 403 every admin
  write). This deployment requirement is documented beside the
  `X-Forwarded-Proto` trust note in the README, and an httpapi test covers the
  rewritten-`Host` failure mode so the error is at least legible.

### 4.5 What a 401 means to the SPA

The fetch wrapper treats any 401 as "session gone" and routes to `/admin/login`
(preserving the intended destination). `GET /session` exists so the SPA can answer
"am I logged in?" on boot without a sacrificial data request.

## 5. Admin API surface

All JSON, all under `/api/admin/v1`. Errors are `{"error": "human-readable message"}`
with conventional status codes (400 validation, 401 unauthenticated, 403 cross-site,
404 unknown id, 409 conflict, 500 with details logged not leaked). Timestamps in
responses are RFC 3339 UTC (`2026-08-15T21:00:00Z`); timestamps in requests require
an explicit UTC offset, exactly like the CLI, and a naive datetime is a 400 naming
the region's configured timezone.

```
POST   /session                            login  {username, password} → 200 {username}
DELETE /session                            logout → 204
GET    /session                            whoami → 200 {username} | 401

GET    /alerts?region=N                    list; drafts and test alerts included —
                                           this is the authoring view, not the feed.
                                           region absent → all regions; non-integer
                                           → 400; unknown id → 200 [] (it is a
                                           filter, not a resource lookup — and
                                           region 0 is real, never "unset")
GET    /alerts/{id}                        → alert with translations
POST   /alerts                             create draft → 201, Location header
PATCH  /alerts/{id}                        partial update, CLI edit semantics
DELETE /alerts/{id}                        → 204
POST   /alerts/{id}/publish                → 200 alert
POST   /alerts/{id}/unpublish              → 200 alert
PUT    /alerts/{id}/translations/{lang}    upsert {header?, description?} → 200
DELETE /alerts/{id}/translations/{lang}    → 204

GET    /regions                            list incl. default_agency_id, timezone
PATCH  /regions/{id}                       set {default_agency_id?, timezone?} → 200
```

Alert JSON (response shape; request shapes are subsets):

```json
{
  "id": 1, "region_id": 1, "agency_id": "1",
  "header": "Route 44 detoured", "description": "", "url": "",
  "cause": "CONSTRUCTION", "effect": "DETOUR", "severity": "WARNING",
  "start_time": "2026-08-15T21:00:00Z", "end_time": null,
  "published": true, "is_test": false,
  "created_at": "2026-08-11T20:00:00Z", "updated_at": "2026-08-11T20:05:00Z",
  "translations": [{"language": "es", "header": "...", "description": "..."}]
}
```

`POST /alerts` resolves `agency_id` at author time exactly as `alert create` does
(explicit value, else the region's default, else 400 with the same
set-a-default guidance the CLI prints). The feed endpoints are untouched and remain
unauthenticated.

## 6. SPA

### 6.1 Stack

- **SvelteKit** (Svelte 5), TypeScript, `@sveltejs/adapter-static`, scaffolded with
  `npx sv create`.
- `export const ssr = false` at the root layout and a `fallback: 'index.html'` in the
  adapter config: a pure client-rendered SPA, which is what serving from `go:embed`
  requires — there is no Node server in production. **No route may set
  `prerender = true`** — the SvelteKit docs warn an `index.html` fallback conflicts
  with prerendering, and adapter-static's *generic* recipe (root-layout
  `prerender = true`) is exactly what not to copy here; the single-page-app recipe
  governs.
- No component library, no CSS framework in v1. Hand-written CSS; the surface is
  five screens.
- Project lives in `web/admin/`.

### 6.2 Routes

| Browser URL | `src/routes/` directory | Screen |
|---|---|---|
| `/admin/login` | `login/` | username/password form |
| `/admin` | `+page.svelte` (root) | alerts list: region filter, published/draft/test badges |
| `/admin/alerts/new` | `alerts/new/` | create form |
| `/admin/alerts/[id]` | `alerts/[id]/` | edit + translations + publish/unpublish/delete |
| `/admin/regions` | `regions/` | region list; edit default agency id and timezone inline |

SvelteKit's `paths.base` is set to `/admin` so the static build's asset URLs and
client router agree with where the Go server mounts it. Route directories are
expressed **without** the base — `src/routes/admin/login/` would produce
`/admin/admin/login`, the same doubled-prefix failure mode commit `ca99e10` fixed
in the CLI. In-app links and navigations use `resolve()` from `$app/paths` (the
current API; bare `base` concatenation is deprecated), never hand-written
`/admin/...` strings.

### 6.3 API client

One thin typed wrapper (`src/lib/api.ts`): JSON in/out, non-2xx → typed error with
the server's `error` string, 401 → redirect to login with `redirectTo` preserved.
Datetime inputs are the one nontrivial UI problem: the form takes a local
datetime + an explicit region-timezone-aware offset display, and always submits
RFC 3339 with offset. That mapping logic lives in a plain `.ts` module with vitest
coverage — it is exactly where the CLI's timezone bugs would reappear in the browser.

### 6.4 Development workflow

`vite.config.ts` proxies `/api` to `http://localhost:8080` (the Go server), so
`npm run dev` gives hot reload against real data with same-origin cookies intact.
Leave `changeOrigin` **unset**: it rewrites `Host` to `localhost:8080` while the
browser's `Origin` stays the Vite origin, so §4.4's Origin check would 403 every
write in dev. The proxy does not interact with `paths.base` — API paths are
root-relative, outside `/admin`. Production build: `npm run build` → output copied
into the embed dir by `make web`.

### 6.5 Serving from Go

- `GET /admin` and `GET /admin/{path...}`: serve the embedded file if it exists,
  else `index.html` (client-side routing fallback), else the 503 of §2.3.
  **Exception: paths under `/admin/_app/` never fall back** — a missing asset (a
  stale tab requesting a chunk from a previous deploy) gets a clean 404, not
  `index.html` served as JavaScript.
- Content-hashed assets — **`/admin/_app/immutable/` only** — get
  `Cache-Control: public, max-age=31536000, immutable`. Everything else, including
  `index.html` and un-hashed files directly under `_app/` (`version.json`,
  `env.js`), gets `no-cache`: those exist to be re-fetched, and a year-long cache
  on them silently breaks any later use of SvelteKit's version polling or dynamic
  public env. Stale-HTML-fresh-assets is the classic SPA deploy bug.
- The SPA and its assets are served **unauthenticated** (the login page lives there);
  everything sensitive is behind the API.

## 7. `sidecar-admin user`

```
sidecar-admin user  create --username NAME   [--password-stdin]
                    passwd --username NAME   [--password-stdin]
                    list
                    delete --username NAME
```

- Interactive path prompts twice with echo off (`golang.org/x/term`); mismatch or
  under-12-chars re-prompts, three strikes and exit 1.
- `user passwd` calls `DeleteUserSessions` after updating the hash: a password
  change is the compromised-credential recovery path, and it must log out whoever
  held the old password — sessions surviving a password change for 30 days would
  defeat the point.
- `--password-stdin` reads the password from stdin (trailing newline trimmed) for
  scripting; there is deliberately **no `--password` flag** — passwords do not
  belong in shell history or `ps` output.
- `user delete` of the last remaining user requires `--force`, and warns that the
  admin UI becomes unreachable until a new `user create`. Deleting a nonexistent
  user is an error, not a silent success (established sidecar-admin convention).
- Bootstrap is not a special mode: it is the first `user create` against the shared
  database, documented in the README quickstart.

## 8. Testing

- **storetest**: conformance subtests for `auth.Repository` — round-trips, unique
  username → `ErrUsernameTaken`, `GetSession` expiry against injected clock (a
  session expiring one second *after* `now` is alive; *at* `now` is dead), the
  expired row is **gone** after `GetSession` observes it (§3.2's delete-on-read
  contract), `DeleteUserSessions` revokes all and only that user's sessions,
  cascade on user delete, `DeleteExpiredSessions` count, and an assertion that the
  stored `token_hash` never equals the raw token.
- **internal/auth**: hash/verify round-trip, wrong password, PHC parse of foreign
  parameters, tampered hash, min/max length enforcement, token uniqueness.
- **httpapi**: table-driven handler tests — login success/failure sets/omits
  cookies correctly; 401 on missing/garbage/expired cookie; cross-site middleware
  403s on hostile `Origin`/`Sec-Fetch-Site` (including on `POST /session`), passes
  same-origin, and produces a legible 403 on the rewritten-proxy-`Host` case
  (§4.4); full alert CRUD through the API including the 400 for naive datetimes;
  regions PATCH; SPA serving including the fallback-to-index path, the
  no-fallback 404 under `_app/`, the 503-when-unbuilt path, and the cache-header
  scoping of §6.5.
- **CLI**: `user` subcommand tests driving the stdin paths.
- **SPA**: vitest on the datetime mapping module and API client error paths;
  `svelte-check`, eslint, prettier — all wired into `make check` behind a
  `web-check` target.
- **test-tz discipline**: the Go suite continues to run under `TZ=UTC` and
  `TZ=Asia/Kathmandu`; session expiry and datetime parsing must be indifferent.

## 9. Security posture (explicit non-goals included)

Threats addressed: credential stuffing timing oracles (§4.3), session theft via DB
leak (§2.1), CSRF (§4.4), XSS-based token theft (`HttpOnly`; Svelte escapes
interpolations by default — `{@html ...}` is banned in this codebase), slowloris
(existing server timeouts), oversized bodies (8 KB cap on auth endpoints, 64 KB on
alert/region writes).

Accepted risks, deliberately: no per-IP rate limiting (fixed 500 ms failure delay
only — an internet-exposed deployment should sit behind a proxy that rate-limits),
no MFA, no password breach checking, no audit trail, sessions are not bound to IP or
user agent. Each is a straightforward later addition; none changes this schema.

## 10. Build & repo wiring

- `mise.toml`: add current Node LTS.
- `Makefile`: `web` (npm ci + build + copy into embed dir), `web-check`
  (svelte-check, eslint, prettier check, vitest), `build` depends on `web`,
  `check` gains `web-check`. Existing Go targets unchanged.
- `.gitignore`: `web/admin/node_modules/`, `web/admin/.svelte-kit/`, embed `dist/`
  contents (except `.gitkeep`).
- README: admin UI section — bootstrap a user, log in, and the same
  sequential-steps discipline the alerts quickstart learned the hard way.
