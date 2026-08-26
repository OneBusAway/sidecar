# Region API Keys and the Region-Scoped Admin API — Design

**Date:** 2026-08-26
**Status:** Reviewed
**Builds on:** [Admin API + SPA](2026-08-11-admin-api-spa-design.md) (sessions, route
table, middleware), [Surveys](2026-08-22-surveys-design.md), [Ghost Bus
Reports](2026-08-23-ghost-bus-reports-design.md), [Alert Push
Fan-out](2026-08-25-alert-push-fanout-design.md).

## 1. Scope

OBACloud (`../obacloud`, Rails) already has server-rendered UIs for alerts, surveys,
survey responses, and alarms that read OBACloud's own Postgres tables. The goal is to
re-plumb those UIs so they read and write the sidecar instead, region by region. That
needs two things from the sidecar:

1. **A credential a server can hold.** The admin API today accepts only a browser
   session cookie minted by an operator login. OBACloud needs a bearer credential that
   is scoped to exactly one region and can be provisioned without a human copying
   secrets between systems.
2. **A complete authoring surface.** Surveys and ghost bus reports are reachable only
   through `sidecar-admin`; alarms and push registrations have no admin surface at all.

This design adds:

- `internal/apikey`: **region API keys** (`sk_…`, one region each) and **service
  principals** (`sp_…`, may mint and revoke region keys, nothing else), with a
  repository, migration, storetest suite, and `sidecar-admin key` / `principal`
  commands;
- a second authentication path on the admin API (`Authorization: Bearer`), with a
  principal model and per-route allow-lists the tests can enumerate;
- a **region segment in every region-scoped admin path**
  (`/api/admin/v1/regions/{regionId}/…`), replacing today's `?region=` filter and
  `region_id` body field, with one middleware enforcing tenancy;
- admin routes for studies, surveys, survey responses (JSON and CSV), ghost bus reports
  (JSON and CSV), alarms (read-only), push registration counts, and region API keys;
- the SPA updated to the new paths;
- the Rails-side contract, **documented but not built** (§7).

### Out of scope

- Building the OBACloud integration itself (client, provisioner, UI re-plumbing). §7
  is the contract that work will implement.
- Key management in the SPA. Keys are minted by the CLI or by a service principal;
  the SPA is not touched beyond the path change.
- Live Activities: nothing to author, and OBACloud has no UI for them.
- Listing push registration tokens. Only aggregate counts are exposed.
- Pagination. Per-region tables are small and the CLI already lists them whole; `?limit`
  and `?after` are reserved for a later addition and not implemented now.
- Browser-direct calls from OBACloud pages to the sidecar. Every call is
  server-to-server; there is no CORS configuration and none is planned.
- Key expiry. In a server-to-server integration an expiring credential is an outage
  timer; rotation is explicit (§2.3, §7.3).

## 2. Decisions

### 2.1 Server-to-server, per-region keys

OBACloud authenticates its own users and enforces organization → region authorization
(`User#can_view?`, `Organizations::BaseController#set_organization`). The sidecar never
sees an OBACloud user or browser: Rails calls it with a credential only Rails holds. That
credential is scoped to **one region** so a leaked key exposes one tenant, and so a
multi-tenant sidecar does not rely solely on Rails to keep regions apart. Rails's own
authorization is the first fence; the key's region scope is the second, independent
one.

### 2.2 Service principals automate provisioning

A per-region key still has to get into Rails somehow. A **service principal** is a
deployment-wide credential whose only power is to mint and revoke region keys. OBACloud
holds one per sidecar deployment it talks to and mints region keys on demand (§7.2).

This is a deliberate trade: a mint-capable secret partially reintroduces the blast
radius per-region keys avoid. It is accepted because the principal is used only on the
rare provisioning path (never the hot path where secrets get logged or leak into error
reports), region keys stay individually revocable, and the principal can read nothing —
not an alert, not a region — so its compromise yields keys, never data, until those keys
are used, and every mint is visible in `key list`.

A region key can never mint or revoke keys, so a leaked region key cannot propagate.

### 2.3 Multiple live keys per region; rotation is create → swap → revoke

Nothing limits a region to one key. Rotation is: mint a new key, update the consumer,
revoke the old one. `last_used_at` (§3) tells an operator whether an old key is still
in use before revoking it.

### 2.4 One admin API, three principals

There is one admin API at `/api/admin/v1`, not a parallel bearer-only namespace. The
authentication middleware yields a **principal** of one of three kinds; each route
declares which kinds it accepts (§4.3). The embedded SPA and OBACloud call the same
handlers.

### 2.5 The region is a path segment

Every region-scoped route is `/api/admin/v1/regions/{regionId}/…`. Today's admin alert
routes are cross-region (`GET /alerts?region=N`, `region_id` in the create body); they
move under the region segment and the SPA is updated. There is no compatibility shim:
the admin API's only client is the SPA shipped in the same binary.

The segment is what makes tenancy a single check: one middleware compares the path's
region against the principal (§4.4). Handlers never parse a region themselves.

The plural `regions` matches the rider API (`/api/v1/regions/{regionId}/alerts`).

### 2.6 Existing rules apply unchanged

No `time.Now` outside `cmd/` and tests; epoch-second INTEGER columns; RFC 3339 with an
explicit offset for every instant in JSON; ASCII-only comments in `queries/*.sql`; no
`sqlc.arg()` inside `IN (...)`; rider-sourced CSV cells pass the formula-injection guard.

## 3. Data model

Migration `00009_api_keys.sql`:

```sql
CREATE TABLE region_api_keys (
    id           INTEGER PRIMARY KEY,
    region_id    INTEGER NOT NULL REFERENCES regions(id) ON DELETE CASCADE,
    name         TEXT    NOT NULL,
    key_hash     TEXT    NOT NULL UNIQUE,   -- hex SHA-256 of the raw key
    created_at   INTEGER NOT NULL,          -- epoch seconds
    last_used_at INTEGER,                   -- epoch seconds, NULL until first use
    revoked_at   INTEGER                    -- epoch seconds, NULL while live
);
CREATE INDEX region_api_keys_region ON region_api_keys(region_id);

CREATE TABLE service_principals (
    id           INTEGER PRIMARY KEY,
    name         TEXT    NOT NULL,
    key_hash     TEXT    NOT NULL UNIQUE,
    created_at   INTEGER NOT NULL,
    last_used_at INTEGER,
    revoked_at   INTEGER
);
```

Revoked rows are kept (not deleted) so `key list` shows history and so a revoked key's
hash cannot be re-minted by accident. `regions` cascade-deletes keys, matching alerts.

### 3.1 Key format

- Region key: `sk_<regionID>_<43 base64url chars>` from 32 random bytes
  (`crypto/rand`), e.g. `sk_1_Qm9…`.
- Service principal: `sp_<43 base64url chars>`.

The `sk_`/`sp_` prefixes make keys recognisable to secret scanners and let the
middleware dispatch to the right table without a second lookup. The region id in the
plaintext is a debugging aid only: the hash lookup decides, and the middleware treats a
row whose `region_id` disagrees with the prefix as an invalid key. Only the hex SHA-256
of the raw key is stored (the same posture as sessions); the raw key is printed once by
the CLI or returned once by the mint route.

### 3.2 `apikey.Repository`

```go
package apikey

type RegionKey struct {
    ID         int64
    RegionID   int64
    Name       string
    KeyHash    string
    CreatedAt  time.Time
    LastUsedAt *time.Time
    RevokedAt  *time.Time
}

type Principal struct { // service principal row
    ID         int64
    Name       string
    KeyHash    string
    CreatedAt  time.Time
    LastUsedAt *time.Time
    RevokedAt  *time.Time
}

type Repository interface {
    CreateRegionKey(ctx, regionID int64, name, keyHash string, now time.Time) (RegionKey, error)
    // GetRegionKeyByHash returns ErrNotFound for unknown AND revoked hashes.
    GetRegionKeyByHash(ctx, keyHash string) (RegionKey, error)
    ListRegionKeys(ctx, regionID int64) ([]RegionKey, error)          // live and revoked, newest first
    RevokeRegionKey(ctx, id int64, now time.Time) error                // ErrNotFound; idempotent on an already-revoked row
    TouchRegionKey(ctx, id int64, now time.Time) error

    CreatePrincipal(ctx, name, keyHash string, now time.Time) (Principal, error)
    GetPrincipalByHash(ctx, keyHash string) (Principal, error)          // ErrNotFound for unknown AND revoked
    ListPrincipals(ctx) ([]Principal, error)
    RevokePrincipal(ctx, id int64, now time.Time) error
    TouchPrincipal(ctx, id int64, now time.Time) error
}
```

`NewRegionKey(regionID) (raw, hash string, err error)` and `NewPrincipalKey()` mint
and hash; `ParsePrefix(raw) (kind, regionID, ok)` splits a bearer value for dispatch.
`internal/store/storetest.RunAPIKeyRepository` is the conformance suite.

The alarms repository gains `ListByRegion(ctx, regionID int64) ([]Alarm, error)` and
`Get(ctx, id int64) (Alarm, error)`; `List` (the scheduler's all-region sweep) is
unchanged. Surveys gains `UpdateStudy(ctx, id int64, name, description string, now
time.Time) (Study, error)`.

## 4. Authentication and authorization

### 4.1 Principal

```go
type principalKind int
const (
    principalOperator principalKind = iota + 1 // session cookie
    principalRegionKey                         // sk_
    principalService                           // sp_
)

type principal struct {
    kind     principalKind
    regionID int64 // set only for principalRegionKey
    userID   int64 // set only for principalOperator
    keyID    int64 // set for the two bearer kinds
}

func (p principal) canAccessRegion(id int64) bool {
    return p.kind == principalOperator || (p.kind == principalRegionKey && p.regionID == id)
}
```

The principal is stored in the request context by `requirePrincipal` and read by
`requireRegion` and by the key-management handlers (to reject a region key).

### 4.2 `requirePrincipal` (replaces `requireSession`)

1. If `Authorization: Bearer <value>` is present, cookies are **ignored entirely**.
   `ParsePrefix` dispatches on `sk_`/`sp_`; the hash is looked up; unknown, revoked, or
   prefix/row mismatch → `401 {"error":"invalid api key"}` after `Deps.FailDelay`
   via `Deps.Sleep` (the same brake login uses). Success sets the principal and, if
   `last_used_at` is null or older than one hour, touches it (best effort; a failed
   touch is logged, not surfaced). A malformed header (no `Bearer`, wrong shape) is
   401 too — it never falls through to the cookie path.
2. Otherwise the existing session-cookie path runs unchanged and yields an operator
   principal.
3. When `Deps.APIKeys` is nil, any `Authorization` header is 401: bearer auth is simply
   not configured. `main` always sets it.

Failed bearer attempts are logged with the first eight characters of the value and the
peer address, never the full value.

### 4.3 Cross-site guard and rate limiting

`crossSiteGuard` is bypassed for requests carrying an `Authorization` header. A browser
cannot attach that header cross-site without a CORS preflight, and the sidecar never
answers preflights, so the CSRF class the guard defends against does not apply. The
guard still wraps every request that authenticates by cookie, including login.

Bearer requests are not throttled: Rails is a trusted caller, the same reasoning as the
gorush webhook secret. Failed bearer authentication pays `FailDelay`.

### 4.4 Route table

`adminRoute` gains two columns:

```go
type adminRoute struct {
    pattern      string
    handler      http.HandlerFunc
    allowed      principalSet // nil means "no principal required" (login/logout only)
    regionScoped bool         // pattern contains {regionId}; requireRegion is applied
}
```

`registerAdminRoutes` wraps each route as: `crossSiteGuard(requirePrincipal(allowed,
[requireRegion,] handler))`. Tests enumerate the table and assert:

- every pattern except login/logout has a non-empty `allowed`;
- every `regionScoped` pattern contains `{regionId}`, and every pattern containing
  `{regionId}` is `regionScoped` (so the segment is never parsed by hand);
- for each principal kind, each route returns 403 iff the kind is not in `allowed`;
- the **tenancy walk**: every `regionScoped` route, called with a region-A key against
  fixtures created in region B, returns 404 (lists return only region-A rows). A route
  added to the table without a fixture entry in the walk's table fails the test.

`requireRegion` parses `{regionId}`, returns 404 for a non-integer, checks
`principal.canAccessRegion`, loads the `regions.Region` (404 for unknown), and stores
it in the context. "Not visible" and "does not exist" are the same 404 so a region key
cannot probe for other regions. Handlers read the region from the context and stop
fetching it themselves.

Resource loaders (`loadAlert`, `loadSurvey`, `loadStudy`, …) assert
`resource.RegionID == ctx region` (surveys through their study) and return 404
otherwise; this is what stops `/regions/A/alerts/{id-of-B}`.

### 4.5 Allowed principals by route family

| Family | operator | region key | service principal |
|---|---|---|---|
| `session` whoami | ✓ | — | — |
| `GET /regions` (list) | ✓ | — | — |
| `GET /regions/{id}`, `PATCH /regions/{id}` | ✓ | ✓ (own region) | — |
| alerts, pushes, studies, surveys, responses, ghost bus, alarms, push counts | ✓ | ✓ | — |
| `…/api_keys` (mint, list, revoke) | ✓ | — | ✓ |

`PATCH /regions/{id}` by a region key is allowed so OBACloud can set timezone and
default agency; the OBA API key field stays write-only and never echoed, as today.

## 5. Admin API surface

All paths below are prefixed `/api/admin/v1`. Bodies and responses are JSON with the
existing `{"error": "…"}` envelope on failure; instants are RFC 3339 UTC; ids are
integers except where a resource has a public id.

### 5.1 Moved routes (alerts and pushes)

```
GET    /regions/{regionId}/alerts
POST   /regions/{regionId}/alerts                       body loses region_id
GET    /regions/{regionId}/alerts/{id}
PATCH  /regions/{regionId}/alerts/{id}
DELETE /regions/{regionId}/alerts/{id}
POST   /regions/{regionId}/alerts/{id}/publish | unpublish
PUT    /regions/{regionId}/alerts/{id}/translations/{lang}
DELETE /regions/{regionId}/alerts/{id}/translations/{lang}
POST   /regions/{regionId}/alerts/{id}/pushes
GET    /regions/{regionId}/alerts/{id}/pushes
DELETE /regions/{regionId}/alerts/{id}/pushes/{pushId}
GET    /regions/{regionId}/alerts/{id}/push_audience
GET    /regions/{regionId}                              new; the region row, same shape as the list's elements
PATCH  /regions/{regionId}                              unchanged
```

Semantics are unchanged apart from the region source; the alert push routes remain
conditional on a configured transport.

### 5.2 Studies and surveys

The JSON survey document is the one `sidecar-admin survey create --file` already
accepts (surveys design spec §3); the codec moves from `cmd/sidecar-admin` into
`internal/surveys` so the CLI and the API cannot drift.

```
GET    /regions/{regionId}/studies                      [{id, name, description, created_at, updated_at}]
POST   /regions/{regionId}/studies                      {name, description?} → 201
GET    /regions/{regionId}/studies/{id}
PATCH  /regions/{regionId}/studies/{id}                 {name?, description?}
GET    /regions/{regionId}/surveys                      [{…survey summary, study_id, response_count}]
POST   /regions/{regionId}/surveys                      {study_id, …definition} → 201; study must be in the region (404 otherwise)
GET    /regions/{regionId}/surveys/{id}                 full survey with questions and study
PUT    /regions/{regionId}/surveys/{id}                 full definition; ErrQuestionsFrozen → 409
DELETE /regions/{regionId}/surveys/{id}                 ErrHasResponses → 409; 204
GET    /regions/{regionId}/surveys/{id}/responses       [{…response}]
GET    /regions/{regionId}/surveys/{id}/responses.csv   text/csv; Content-Disposition attachment
GET    /regions/{regionId}/survey_responses/{publicId}
```

Validation failures (bad definition, unknown question type, …) are 422 with the same
messages the CLI prints. The CSV writer moves from `cmd/sidecar-admin/surveys.go` into
`internal/surveys` (`WriteResponsesCSV(w, survey, responses)`) and keeps the
formula-injection guard; the CLI calls it.

### 5.3 Ghost bus reports (read-only)

```
GET /regions/{regionId}/ghost_bus_reports?since=RFC3339        [{…report}]; since optional; an explicit UTC offset is required
GET /regions/{regionId}/ghost_bus_reports.csv?since=RFC3339    text/csv, the CLI export's columns
GET /regions/{regionId}/ghost_bus_reports/{publicId}            one report, including snapshot JSON (raw, as captured) and snapshot status
```

`ListForExport` already takes `(regionID, sinceUnix)`; a `GetByPublicID(ctx, regionID,
publicID)` is added. The CSV writer moves into `internal/ghostbus`.

### 5.4 Alarms (read-only)

```
GET /regions/{regionId}/alarms        [{id, api_version, operating_system, stop_id, trip_id, service_date, vehicle_id, stop_sequence, seconds_before, message, failure_count, created_at}]
GET /regions/{regionId}/alarms/{id}
```

`token` and `user_push_id` are **omitted**: they are push credentials, not UI data.

### 5.5 Push registration counts

```
GET /regions/{regionId}/push_registrations/count
→ {"total": N, "ios": N, "android": N, "test": {"total": N, "ios": N, "android": N}}
```

Two `CountAudience` calls (`testOnly` false and true). No token listing.

### 5.6 Region API keys

```
POST   /regions/{regionId}/api_keys        {name} → 201 {id, name, key, created_at}   key appears here and nowhere else
GET    /regions/{regionId}/api_keys        [{id, name, created_at, last_used_at, revoked_at}]
DELETE /regions/{regionId}/api_keys/{id}   revoke; 204; idempotent
```

Operator or service principal only (§4.5). `name` is required, trimmed, ≤ 100 chars.
The region must already exist in `regions`, which is populated from OBACloud's directory
export — so a principal can mint keys only for regions OBACloud has already published.

### 5.7 Registration conditions

A resource family's admin routes register only when its repository is set in `Deps`
(surveys → `Deps.Surveys`, ghost bus → `Deps.GhostBus`, alarms → `Deps.Alarms`, push
counts → `Deps.PushRegs`), keeping the nil-means-absent convention. The key-management
routes and bearer auth require `Deps.APIKeys`; `main` always sets it.

## 6. CLI and SPA

### 6.1 `sidecar-admin`

```
sidecar-admin key create --region N --name NAME       prints the raw key once, then id/name
sidecar-admin key list --region N                     id, name, created, last used, revoked
sidecar-admin key revoke --id N
sidecar-admin principal create --name NAME            prints sp_… once
sidecar-admin principal list
sidecar-admin principal revoke --id N
```

Same flag conventions as `user`, `study`, and `survey`; tests in
`cmd/sidecar-admin/keys_test.go`.

### 6.2 SPA

`web/admin/src/lib/api.ts` and the `alerts`/`pushes` modules switch to the region-segment
paths. The alerts list page, which today lists every region, gains a region selector
(defaulting to the single region when there is only one, otherwise the last choice from
`localStorage`); the alert form drops its region field in favour of the selected region.
No other SPA change.

## 7. OBACloud contract (documented; built later)

This section is the specification the Rails integration will implement. It is
recorded here so the sidecar side is designed against a concrete consumer.

### 7.1 Identity join

The sidecar's region `id` is the id in the regions directory, which OBACloud publishes
from `Region#region_identifier` (`app/views/regions/export.json.jbuilder`). Every
sidecar path uses that value: `…/regions/#{region.region_identifier}/…`. A region is
addressable in the sidecar only after the directory sync has imported it (hourly, and
at boot).

### 7.2 Credentials and provisioning

- Rails credentials hold `sidecar.principals`, a map from sidecar base URL to `sp_…`,
  one entry per sidecar deployment (`Region.sidecar_base_url` already allows several).
  Principals are created by an operator with `sidecar-admin principal create`.
- `Region` gains `sidecar_api_key`, an `encrypts`-ed column, never rendered.
- `Sidecar::Provisioner.ensure_key!(region)`: when the column is blank, `POST
  …/regions/{id}/api_keys` with `{name: "obacloud <rails host>"}` using the principal
  for `region.sidecar_base_url`, then store the returned key. Triggered by a `Region`
  after-commit when `sidecar_base_url` is set or changed, and lazily by
  `Sidecar::Client` on first use. A 404 means "not synced yet": the provisioner enqueues
  a retry job with backoff rather than failing the save.
- `Sidecar::Provisioner.rotate!(region)`: mint → save → revoke the previous key id (kept
  in a second column `sidecar_api_key_id`). Exposed to OBACloud admins as a button on
  the region form and callable from a job.

### 7.3 Client

`Sidecar::Client.new(region)` wraps one HTTP client with `Authorization: Bearer
#{region.sidecar_api_key}`, JSON in/out, a 5 s open/read timeout, and this error
mapping:

| sidecar | Rails |
|---|---|
| 401 | `Sidecar::Unauthorized` — the key was revoked out of band; surfaced as a region health warning, with "re-provision" (clear the column, `ensure_key!`) offered to OBACloud admins only |
| 404 | `Sidecar::NotFound` → the controller's existing not-found handling |
| 409, 422 | `Sidecar::Invalid` carrying the error message → form errors |
| 5xx, timeout | `Sidecar::Unavailable` → flash and re-render; never retried on writes |

The client is obtained from `current_organization.region`, so a customer can never
address another region's key; the key's own scope is the second fence.

### 7.4 Migration order

Alerts first (the sidecar alert API is the most mature), then surveys and responses,
then alarms and push counts as read-only views. Each step reads from the sidecar and
stops reading the corresponding Postgres tables; dropping those tables is a later,
separate change.

## 8. Testing

- `storetest.RunAPIKeyRepository`: create/lookup/revoke/touch for both kinds, revoked
  rows are invisible to `GetByHash`, cascade on region delete, `ListRegionKeys` ordering.
- Middleware: bearer beats cookie; malformed header is 401 not fall-through; revoked
  key 401 with delay; prefix/row mismatch is 401; touch at most hourly (fixed `Now`);
  nil `Deps.APIKeys` rejects bearer; cross-site guard bypass only with a bearer header.
- Route table invariants and the tenancy walk (§4.4), including the per-kind 403 walk.
- Handler tests per new family: happy paths, 404 across regions, 409/422 mappings,
  CSV content type and formula guard, alarm JSON omits `token`/`user_push_id`.
- CLI tests for `key` and `principal`.
- SPA: `api.test.ts` and `alerts.test.ts` updated for the new paths; a region-selector
  test.
- `make check` under both `test-tz` zones. Every new test is checked to fail when the
  code under test is mutated.

## 9. Documentation

- README: "Region API keys and service principals" under the admin API section
  (format, CLI, provisioning flow, rotation), the updated route list, and a short
  "OBACloud integration" summary pointing at §7.
- `specification/openapi.yaml` does not describe the admin API today and is left as
  is; the admin surface is documented in the README and this spec.

## 10. Build and wiring

- `cmd/sidecar/main.go`: `Deps.APIKeys = store.APIKeys()`.
- `internal/store/sqlite`: `apikeys.go`, `queries/apikeys.sql`, `make generate`.
- Route registration for the new families lives in `adminRoutes`, gated per §5.7.
