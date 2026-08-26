# Region API Keys and the Region-Scoped Admin API — Design

**Date:** 2026-08-26
**Status:** Reviewed (red-teamed and security-audited; findings folded in)
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

- `internal/apikey`: **region API keys** (`obask_…`, one region each) and **service
  principals** (`obasp_…`, may mint and revoke region keys, nothing else), with a
  repository, migration, storetest suite, and `sidecar-admin key` / `principal`
  commands;
- a second authentication path on the admin API (`Authorization: Bearer`), with a
  principal model and per-route allow-lists the tests can enumerate;
- a **region segment in every region-scoped admin path**
  (`/api/admin/v1/regions/{regionId}/…`), replacing today's `?region=` filter and
  `region_id` body field, with one middleware enforcing tenancy;
- admin routes for studies, surveys, survey responses (JSON and CSV), ghost bus reports
  (JSON and CSV), alarms (read-only), push registration counts, and region API keys;
- the SPA moved to region-scoped paths and URLs;
- the Rails-side contract, **documented but not built** (§7).

### Out of scope

- Building the OBACloud integration itself (client, provisioner, UI re-plumbing). §7
  is the contract that work will implement.
- Key management in the SPA. Keys are minted by the CLI or by a service principal.
- Live Activities: nothing to author, and OBACloud has no UI for them.
- Listing push registration tokens. Only aggregate counts are exposed.
- Pagination. Per-region tables are small and the CLI already lists them whole; `?limit`
  and `?after` are reserved for a later addition and not implemented now.
- Browser-direct calls from OBACloud pages to the sidecar. Every call is
  server-to-server; there is no CORS configuration and none is planned.
- Key expiry. In a server-to-server integration an expiring credential is an outage
  timer; rotation is explicit (§2.3, §7.3).
- Deprovisioning a region. The sidecar never deletes a region (a directory sync only
  upserts), so revoking its keys is the only tenancy teardown; see §7.1.

## 2. Decisions

### 2.1 Server-to-server, per-region keys

OBACloud authenticates its own users and enforces organization → region authorization
(`User#can_view?`, `ApplicationController#set_organization`, `Current.organization`).
The sidecar never sees an OBACloud user or browser: Rails calls it with a credential
only Rails holds. That credential is scoped to **one region** so a leaked key exposes
one tenant, and so a multi-tenant sidecar does not rely solely on Rails to keep regions
apart. Rails's own authorization is the first fence; the key's region scope is the
second, independent one — and it matters for OBACloud admins too, whose organization
comes from a URL slug rather than their account.

**What a leaked region key can do** (accepted, stated once here): read and write every
resource of its region listed in §4.5, including setting that region's OBA API key
via `PATCH /regions/{id}` (which redirects the region's sidecar-side OBA calls — ghost
bus snapshots, vehicle search, alarms — to a key the holder controls). It cannot send
push notifications (push create/cancel are operator-only, §4.5), reach another region,
or mint or revoke keys. The remedy is revocation.

### 2.2 Service principals automate provisioning

A per-region key still has to get into Rails somehow. A **service principal** is a
deployment-wide credential whose only power is to mint, list, and revoke region keys.
OBACloud holds one per sidecar deployment it talks to and mints region keys on demand
(§7.2).

This is a deliberate trade: a mint-capable secret partially reintroduces the blast
radius per-region keys avoid. **What a leaked service principal can do**, stated
plainly: mint a live key for any published region (then use it, with the region-key
exposure above), revoke every region key in the deployment — a deployment-wide denial
of service for the OBACloud integration — and learn which region ids exist plus the
metadata of every key (names, creator ids, timestamps). It can read no tenant data:
no alert, no survey response, no rider report. It is accepted because the principal is
used only on the rare provisioning path (never the hot path where secrets get logged
or leak into error reports) and the damage is recoverable:

- every key records **who minted it and who revoked it** (§3), so after a leak
  `key list --minted-by-principal N` shows the keys to revoke and `key list` shows
  which revocations were the attacker's;
- because the attacker mints with the same principal Rails uses, those lists cannot
  separate attacker keys from legitimate ones, so recovery is **revoke the principal
  with its keys, mint a new principal, and bulk re-provision** (§7.2) — one command on
  each side rather than a per-region hunt.

A region key can never mint or revoke keys, so a leaked region key cannot propagate.

### 2.3 Multiple live keys per region; rotation is create → swap → revoke

Nothing limits a region to one key. Rotation is: mint a new key, update the consumer,
revoke the old one. `last_used_at` (§3) tells an operator whether an old key is still
in use before revoking it — bearing in mind it is touched at most hourly (§4.2), so it
may be up to an hour stale.

### 2.4 One admin API, three principals

There is one admin API at `/api/admin/v1`, not a parallel bearer-only namespace. The
authentication middleware yields a **principal** of one of three kinds; each route
declares which kinds it accepts (§4.4). The embedded SPA and OBACloud call the same
handlers.

### 2.5 The region is a path segment

Every region-scoped route is `/api/admin/v1/regions/{regionId}/…`. Today's admin alert
routes are cross-region (`GET /alerts?region=N`, `region_id` in the create body); they
move under the region segment and the SPA moves with them, including its own URLs
(§6.2). There is no compatibility shim: the admin API's only client is the SPA
shipped in the same binary. Existing SPA bookmarks to `/admin/alerts/{id}` break; the
SPA shows its normal not-found page for them.

The segment is what makes tenancy a single check: one middleware compares the path's
region against the principal (§4.3). Handlers never parse a region themselves. Where
a repository method can take the region as a query condition, it does (§3.2), so the
check is in SQL rather than in a Go comparison a refactor could drop.

The plural `regions` matches the rider API (`/api/v1/regions/{regionId}/alerts`).

### 2.6 What hash-only storage does and does not protect

Keys are stored as unsalted hex SHA-256 (256-bit random input makes salting and
constant-time comparison unnecessary; the lookup is an index hit on the hash). A
stolen database backup therefore yields no usable region key or principal. It still
yields everything the sidecar already stores in plaintext — each region's OBA API key,
push registration and alarm tokens, rider `user_identifier`s, ghost bus reports — so
hash-only keys narrow that threat, they do not close it.

### 2.7 Existing rules apply unchanged

No `time.Now` outside `cmd/` and tests; epoch-second INTEGER columns; RFC 3339 with an
explicit offset for every instant in JSON; ASCII-only comments in `queries/*.sql`; no
`sqlc.arg()` inside `IN (...)`; rider-sourced CSV cells pass the formula-injection guard.
`regions.Region.Active` (the directory's flag) is not consulted for admin access: an
inactive region stays authorable, as it is for operators today.

## 3. Data model

Migration `00010_api_keys.sql` (00009 is `alert_pushes`):

```sql
CREATE TABLE service_principals (
    id           INTEGER PRIMARY KEY,
    name         TEXT    NOT NULL,
    key_hash     TEXT    NOT NULL UNIQUE,   -- hex SHA-256 of the raw key
    created_at   INTEGER NOT NULL,          -- epoch seconds
    last_used_at INTEGER,                   -- epoch seconds, NULL until first use
    revoked_at   INTEGER                    -- epoch seconds, NULL while live
);

CREATE TABLE region_api_keys (
    id              INTEGER PRIMARY KEY,
    region_id       INTEGER NOT NULL REFERENCES regions(id) ON DELETE CASCADE,
    name            TEXT    NOT NULL,
    key_hash        TEXT    NOT NULL UNIQUE,
    created_by_kind TEXT    NOT NULL CHECK (created_by_kind IN ('operator', 'principal', 'cli')),
    created_by_id   INTEGER,                -- users.id or service_principals.id; NULL iff cli
    created_at      INTEGER NOT NULL,
    last_used_at    INTEGER,
    revoked_at      INTEGER,
    revoked_by_kind TEXT    CHECK (revoked_by_kind IN ('operator', 'principal', 'cli')),
    revoked_by_id   INTEGER,
    CHECK ((created_by_kind = 'cli') = (created_by_id IS NULL)),
    CHECK ((revoked_at IS NULL) = (revoked_by_kind IS NULL)),
    CHECK ((revoked_by_kind IS NULL) OR ((revoked_by_kind = 'cli') = (revoked_by_id IS NULL)))
);
CREATE INDEX region_api_keys_region ON region_api_keys(region_id);
CREATE INDEX region_api_keys_creator ON region_api_keys(created_by_kind, created_by_id);
```

Revoked rows are kept (not deleted) so `key list` shows history and so a revoked key's
hash cannot be re-minted by accident. `regions` cascade-deletes keys (the store enables
`foreign_keys`), matching alerts. `created_by_*`/`revoked_by_*` are not foreign keys: a
deleted operator or a revoked principal must not orphan the audit trail.

### 3.1 Key format

- Region key: `obask_<regionID>_<43 base64url chars>` from 32 random bytes
  (`crypto/rand`), e.g. `obask_1_Qm9…`.
- Service principal: `obasp_<43 base64url chars>`.

The prefixes are project-specific so secret scanners can classify them (and do not
misfile them as Stripe `sk_` keys) and so the middleware can dispatch to the right table
without a second lookup. The region id in the plaintext is a debugging aid only: the
hash lookup decides, and the middleware treats a row whose `region_id` disagrees with
the prefix as an invalid key.

**Parsing is pinned** because the base64url alphabet contains `_`, so about half of
all random segments contain one: `ParsePrefix` uses `strings.Cut` on the first `_`
to take the kind, then (for `obask_`) a second `strings.Cut` to take the region id,
and treats the remainder as opaque — never a `strings.Split`, never a cut on the last
`_`. The region id segment must match `^(0|[1-9][0-9]*)$`. A fixture key whose random
segment contains both `_` and `-` is in the tests (§8).

Only the hex SHA-256 of the raw key is stored (the same posture as sessions); the raw
key is printed once by the CLI or returned once by the mint route, and is never
written to a log line at any level.

### 3.2 `apikey` package

```go
package apikey

type Actor struct {
    Kind string // "operator" | "principal" | "cli"
    ID   int64  // 0 for cli
}

type RegionKey struct {
    ID         int64
    RegionID   int64
    Name       string
    KeyHash    string
    CreatedBy  Actor
    CreatedAt  time.Time
    LastUsedAt *time.Time
    RevokedAt  *time.Time
    RevokedBy  *Actor
}

type ServicePrincipal struct {
    ID         int64
    Name       string
    KeyHash    string
    CreatedAt  time.Time
    LastUsedAt *time.Time
    RevokedAt  *time.Time
}

type Repository interface {
    CreateRegionKey(ctx, regionID int64, name, keyHash string, by Actor, now time.Time) (RegionKey, error)
    // GetRegionKeyByHash returns ErrNotFound for unknown hashes and ErrRevoked for
    // a hash that matches a revoked row (so the caller can log a replay distinctly).
    GetRegionKeyByHash(ctx, keyHash string) (RegionKey, error)
    ListRegionKeys(ctx, regionID int64) ([]RegionKey, error)                 // live and revoked, newest first
    // ListRegionKeysByCreator matches Actor{Kind:"cli"} against created_by_id IS NULL;
    // the query spells the NULL case as an explicit OR, never a bare "= ?".
    ListRegionKeysByCreator(ctx, by Actor) ([]RegionKey, error)
    // RevokeRegionKey is region-scoped (WHERE id = ? AND region_id = ?): a key in
    // another region is ErrNotFound. An already-revoked key is a no-op success.
    RevokeRegionKey(ctx, regionID, id int64, by Actor, now time.Time) error
    // RevokeRegionKeysByCreator revokes every live key the actor minted, in one
    // transaction, and returns their ids.
    RevokeRegionKeysByCreator(ctx, minted Actor, by Actor, now time.Time) ([]int64, error)
    TouchRegionKey(ctx, id int64, now time.Time) error

    CreatePrincipal(ctx, name, keyHash string, now time.Time) (ServicePrincipal, error)
    GetPrincipalByHash(ctx, keyHash string) (ServicePrincipal, error)         // ErrNotFound | ErrRevoked
    ListPrincipals(ctx) ([]ServicePrincipal, error)
    RevokePrincipal(ctx, id int64, now time.Time) error
    TouchPrincipal(ctx, id int64, now time.Time) error
}
```

Both row types implement `slog.LogValuer` omitting `KeyHash`, as `regions.Region`
omits its OBA key. `NewRegionKey(regionID) (raw, hash string, err error)` and
`NewPrincipalKey()` mint and hash; `ParsePrefix(raw) (kind Kind, regionID int64, ok
bool)` is specified in §3.1. `storetest.RunAPIKeyRepository(t, newStore)` is the
conformance suite; its `newStore` returns `(apikey.Repository, regions.Repository)`
because the cascade case needs a region to delete.

**Other repository additions — every new method that addresses a resource takes the
region as a query condition**, following the "cannot be forgotten" principle:

- `alarms.Repository`: `ListByRegion(ctx, regionID int64) ([]Alarm, error)` and
  `GetInRegion(ctx, regionID, id int64) (Alarm, error)`; `List` (the scheduler's
  all-region sweep) is unchanged.
- `surveys.Repository`: `UpdateStudy(ctx, regionID, id int64, name, description
  string, now time.Time) (Study, error)`; `CreateSurveyInRegion(ctx, regionID, studyID
  int64, def Definition, now time.Time) (Survey, error)` — the study's region is a
  join condition, so a body-borne `study_id` from another region is ErrNotFound;
  `GetResponseInRegion(ctx, regionID int64, publicID string) (Response, error)` — one
  query joining `survey_responses → surveys → studies`.
- `ghostbus.Repository`: `GetByPublicID(ctx, regionID int64, publicID string) (Report,
  error)`.

Existing methods that take bare ids (`alerts.Update`, `SetPublished`, `Delete`,
translations; `GetStudy`, `GetSurvey`, `UpdateSurvey`, `DeleteSurvey`,
`ListResponses`) are left as they are; for those the loader rule in §4.3 applies and
the handler must pass the loaded resource forward rather than re-reading
`r.PathValue` after the loader ran.

## 4. Authentication and authorization

### 4.1 Principal

```go
type principalKind int
const (
    principalOperator principalKind = iota + 1 // session cookie
    principalRegionKey                         // obask_
    principalService                           // obasp_
)

type principal struct {
    kind     principalKind
    user     auth.User // operator only (whoami needs the username; replaces userFrom)
    regionID int64     // region key only
    keyID    int64     // region key and service principal
}

// canAccessRegion is the tenancy fence for every region-scoped route EXCEPT the
// key-management family (§4.3). A service principal is never granted region
// access here; its reach comes only from requireKeyAdminRegion.
func (p principal) canAccessRegion(id int64) bool {
    return p.kind == principalOperator || (p.kind == principalRegionKey && p.regionID == id)
}

func (p principal) actor() apikey.Actor // for created_by / revoked_by
```

`principal` implements `slog.LogValuer` emitting kind, username, region id, and key
id only — it embeds `auth.User`, which carries the password hash, and a future
`slog.Any("principal", p)` must not print it.

### 4.2 `requirePrincipal` (replaces `requireSession`)

1. If an `Authorization` header is present (`r.Header.Values` — more than one header
   is a 401; a present-but-empty value is a 401, not "absent"), cookies are **ignored
   entirely** and the request either authenticates by bearer or fails. The scheme is
   matched case-insensitively against exactly `Bearer` followed by one space; anything
   else, a value longer than 128 bytes (rejected before hashing), a prefix `ParsePrefix`
   cannot parse, an unknown or revoked key, or a prefix/row region mismatch →
   `401 {"error":"invalid api key"}` **immediately** — no `FailDelay`: a 256-bit
   random key is not guessable, so a delay would defend nothing while pinning a
   goroutine per garbage request. Success sets the principal and, if `last_used_at` is
   null or `now.Sub(*LastUsedAt) >= time.Hour` (injected `Now`), touches it — best
   effort; a failed touch is logged, not surfaced.
2. Otherwise the existing session-cookie path runs unchanged and yields an operator
   principal.
3. When `Deps.APIKeys` is nil, any `Authorization` header is 401: bearer auth is simply
   not configured. `main` always sets it.

**Failed attempts** are throttled per peer address by `throttleByIP` with a bucket of
their own (`bearerFailuresPerMinute`, 60/minute, defaulted by `NewRouter`, injected by
tests): the throttle is charged only on failure, so successful calls from Rails are
unmetered — this is the one unauthenticated path in the repo and it keeps the repo's
throttle-everything-unauthenticated posture. Each failure logs the parsed kind, the
prefix's region id (if any), the value's length, the peer, and — distinctly, as
`reason=revoked` — whether the hash matched a revoked row: a revoked key being
replayed is the clearest signal that a credential leaked. Never any part of the
random segment.

### 4.3 Cross-site guard, region middleware, loaders

`crossSiteGuard` wraps **every** admin route unchanged, bearer or not. It already
passes requests carrying neither `Origin` nor `Sec-Fetch-Site`, which is what a
server-side HTTP client sends, so no bypass is needed and none is added; a bearer
request with a foreign `Origin` is rejected before authentication.

`requireRegion` (all region-scoped routes except key management) parses `{regionId}`:
the segment must match `^(0|[1-9][0-9]*)$` and then `strconv.ParseInt(s, 10, 64)`;
anything else is 404. This deliberately differs from `pathID`'s 400 (an unparseable
region is "no such region", and the code must not differ between "malformed" and
"not yours") and from the rider feed's lenient `ParseRegionSegment`, which the admin
API does not reuse. It then checks `principal.canAccessRegion`, loads the
`regions.Region` (404 for unknown), and stores it in the context. "Not visible" and
"does not exist" are the same 404 so a region key cannot probe for other regions.
Handlers read the region from the context and never fetch it themselves.

`requireKeyAdminRegion` (the `…/api_keys` family only) parses and loads the region the
same way but grants access to operators and service principals without consulting
`canAccessRegion`. It is a separate function so the service principal's reach is
visibly confined to one middleware, and the route-table test (§4.4) asserts it is
applied to exactly the patterns ending in `/api_keys` or `/api_keys/{keyId}`.

**Loaders.** Every handler that addresses a resource goes through a loader that takes
the context region: `loadAlert` (and pushes through it, additionally asserting
`push.RegionID`), `loadStudy`, `loadSurvey` (via its study), `loadResponse`
(`GetResponseInRegion`), `loadReport` (`GetByPublicID`), `loadAlarm` (`GetInRegion`),
`loadKey`. Each returns 404 when the resource's region is not the context region;
this is what stops `/regions/A/alerts/{id-of-B}`. Listing responses loads and checks
the survey first. Body-borne ids (`study_id`) go through a region-scoped repository
method (§3.2), never a loader-then-compare.

A principal kind that a route does not allow gets `403 {"error":"forbidden"}` —
distinct from the guard's `"cross-site request rejected"` so tests and Rails can tell
them apart.

### 4.4 Route table

`adminRoute` gains two columns:

```go
type adminRoute struct {
    pattern string
    handler http.HandlerFunc
    allowed principalSet // nil means "no principal required" (login/logout only)
    scope   routeScope   // scopeNone | scopeRegion | scopeKeyAdmin
}
```

`registerAdminRoutes` wraps each route as `crossSiteGuard(requirePrincipal(allowed,
[requireRegion | requireKeyAdminRegion,] handler))`. Tests enumerate the table and
assert:

- every pattern except login/logout has a non-empty `allowed`;
- every pattern containing `{regionId}` has a non-`scopeNone` scope and vice versa
  (so the segment is never parsed by hand);
- `scopeKeyAdmin` iff the pattern ends in `/api_keys` or `/api_keys/{keyId}`, and
  `principalService` appears in `allowed` only on `scopeKeyAdmin` routes;
- for each principal kind, each route returns 403 iff the kind is not in `allowed`;
- the **tenancy walk**: every scoped route, called with a region-A key against
  fixtures created in region B, returns 404 (lists return only region-A rows). The
  fixture table is keyed by pattern and supplies a request **body** as well as a
  path; for every route carrying a resource id in its body (`study_id`) the walk sends
  a region-B id. A route added to `adminRoutes` without an entry fails the test.

`concreteRoute` in the tests learns the `{regionId}`, `{publicId}`, `{lang}`, and
`{keyId}` wildcards, and the pinned route count is updated.

### 4.5 Allowed principals by route family

| Family | operator | region key | service principal |
|---|---|---|---|
| `session` whoami | ✓ | — | — |
| `GET /regions` (list) | ✓ | — | — |
| `GET /regions/{id}`, `PATCH /regions/{id}` | ✓ | ✓ (own region) | — |
| alerts (CRUD, publish, translations), `GET …/pushes`, `GET …/push_audience` | ✓ | ✓ | — |
| `POST …/pushes`, `DELETE …/pushes/{pushId}` | ✓ | — | — |
| studies, surveys, responses, ghost bus, alarms, push counts | ✓ | ✓ | — |
| `…/api_keys` (mint, list, revoke) | ✓ | — | ✓ |

Sending or cancelling a push is operator-only: a leaked region key must not be able to
deliver attacker text as a notification to every device in the region, and
OBACloud's migration plan (§7.4) never sends pushes. Push reads stay available so
OBACloud can show status.

`PATCH /regions/{id}` by a region key lets OBACloud set timezone and default agency.
It can also set the region's OBA API key (write-only, never echoed, as today); the
exposure is stated in §2.1 and confined to the region the key already controls. Which
OBACloud users may reach it is a Rails-side rule (§7.3).

## 5. Admin API surface

All paths below are prefixed `/api/admin/v1`. Bodies and responses are JSON with the
existing `{"error": "…"}` envelope on failure. Instants (`created_at`, `start_time`,
…) are RFC 3339 UTC. Fields that are epoch **milliseconds** in the domain because they
are OBA identifiers or dedupe keys — `service_date`, `scheduled_arrival_at`,
`predicted_arrival_at`, `prediction_last_updated_at` — pass through as integers,
unchanged. Ids are integers except where a resource has a public id.

**Status codes.** The moved alert and region routes keep their existing codes
(validation failures are **400**, as `admin_alerts.go` does today). New families use
400 for malformed JSON, an oversized body, an unparseable path id or query parameter,
and **422** for a well-formed body that fails domain validation (bad survey
definition, blank key name, `region_id`/`study` present where forbidden). Both are
mapped by Rails (§7.3).

### 5.1 Moved routes (alerts and pushes)

```
GET    /regions/{regionId}/alerts
POST   /regions/{regionId}/alerts                       body loses region_id; Location: /api/admin/v1/regions/{regionId}/alerts/{id}
GET    /regions/{regionId}/alerts/{id}
PATCH  /regions/{regionId}/alerts/{id}
DELETE /regions/{regionId}/alerts/{id}
POST   /regions/{regionId}/alerts/{id}/publish | unpublish
PUT    /regions/{regionId}/alerts/{id}/translations/{lang}
DELETE /regions/{regionId}/alerts/{id}/translations/{lang}
POST   /regions/{regionId}/alerts/{id}/pushes           operator-only
GET    /regions/{regionId}/alerts/{id}/pushes
DELETE /regions/{regionId}/alerts/{id}/pushes/{pushId}  operator-only
GET    /regions/{regionId}/alerts/{id}/push_audience
GET    /regions/{regionId}                              new; the region row plus "features": [...]
PATCH  /regions/{regionId}                              unchanged
```

Semantics are unchanged apart from the region source; the alert push routes remain
conditional on a configured transport. A `region_id` in the create body is rejected
(400, consistent with the family) rather than ignored, so a stale client cannot
believe it targeted a region.

`GET /regions/{regionId}` adds `"features"`: the list of admin families registered in
this deployment (`"alerts"`, `"pushes"`, `"surveys"`, `"ghost_bus_reports"`,
`"alarms"`, `"push_registrations"`, `"api_keys"`), so a consumer can distinguish
"family not enabled here" from a 404 (§5.7).

### 5.2 Studies and surveys

The JSON survey document is the one `sidecar-admin survey create --file` already
accepts (surveys design spec §3). `Document` and its codec move from
`cmd/sidecar-admin` into `internal/surveys`, and **the API decodes it strictly**
(`DisallowUnknownFields`, as the CLI does today — a misspelled `show_on_maps` must not
silently hide a survey). Strict decoding is done by a new `decodeJSONStrict` in
`json.go`, beside `decodeJSON`, which applies the same `http.MaxBytesReader` cap and
the same 400 + `errBodyTooLarge` mapping; the survey cap is `maxSurveyBody` = 256 KB.
`study_id` is the only study reference; a `study`, `id`, `created_at`, or `updated_at`
in a request body is rejected (422). `PUT` cannot move a survey between studies.

```
GET    /regions/{regionId}/studies                      [{id, name, description, created_at, updated_at}]
POST   /regions/{regionId}/studies                      {name, description?} → 201
GET    /regions/{regionId}/studies/{id}
PATCH  /regions/{regionId}/studies/{id}                 {name?, description?}   (UpdateStudy is region-scoped)
GET    /regions/{regionId}/surveys                      [{…survey summary, study_id, response_count}]
POST   /regions/{regionId}/surveys                      {study_id, …definition} → 201 via CreateSurveyInRegion; foreign study → 404
GET    /regions/{regionId}/surveys/{id}                 full survey with questions and study
PUT    /regions/{regionId}/surveys/{id}                 full definition; ErrQuestionsFrozen → 409
DELETE /regions/{regionId}/surveys/{id}                 ErrHasResponses → 409; 204
GET    /regions/{regionId}/surveys/{id}/responses       [{…response}]
GET    /regions/{regionId}/surveys/{id}/responses.csv   text/csv; Content-Disposition: attachment; filename="survey-{id}-responses.csv"
GET    /regions/{regionId}/survey_responses/{publicId}
```

Validation failures carry the same messages the CLI prints. The CSV writer moves from
`cmd/sidecar-admin/surveys.go` into `internal/surveys`
(`WriteResponsesCSV(w, survey, responses)`) and keeps the formula-injection guard; the
CLI calls it. CSV responses carry `X-Content-Type-Options: nosniff` and
`Cache-Control: no-store`. Filenames are fixed and server-generated — never derived
from a name.

### 5.3 Ghost bus reports (read-only)

```
GET /regions/{regionId}/ghost_bus_reports?since=RFC3339        [{…report}]
GET /regions/{regionId}/ghost_bus_reports.csv?since=RFC3339    text/csv; filename="ghost-bus-reports-{regionId}.csv"; nosniff; no-store
GET /regions/{regionId}/ghost_bus_reports/{publicId}            one report, including snapshot JSON (raw, as captured) and snapshot status
```

`since` is optional (absent → `0`, i.e. all) and must carry an explicit UTC offset;
otherwise 400. `ListForExport(regionID, sinceUnix)` already exists. The CSV writer moves
into `internal/ghostbus`.

### 5.4 Alarms (read-only)

```
GET /regions/{regionId}/alarms        [{id, api_version, operating_system, stop_id, trip_id, service_date, vehicle_id, stop_sequence, seconds_before, message, failure_count, created_at}]
GET /regions/{regionId}/alarms/{id}
```

`token` and `user_push_id` are **omitted**: they are push credentials, not UI data.
These routes require `Deps.Alarms`, which the router already requires to come with
`PushRegs`, `Regions`, and `Now`; a test fixture enabling them supplies all of those.

### 5.5 Push registration counts

```
GET /regions/{regionId}/push_registrations/count
→ {"total": N, "ios": N, "android": N, "test": {"total": N, "ios": N, "android": N}}
```

Two `CountAudience` calls (`testOnly` false and true). No token listing.

### 5.6 Region API keys

```
POST   /regions/{regionId}/api_keys            {name} → 201 {id, name, key, created_by, created_at}; Cache-Control: no-store; no Location header
GET    /regions/{regionId}/api_keys            [{id, name, created_by: {kind, id}, created_at, last_used_at, revoked_at, revoked_by}]
DELETE /regions/{regionId}/api_keys/{keyId}    revoke; 204 (also for an already-revoked key); 404 for an unknown id or an id in another region
```

Operator or service principal only (§4.5), through `requireKeyAdminRegion`. The raw
key appears in the mint response and nowhere else: never in a URL, a `Location`
header, or a log line. `name` is passed through `stripControlChars` (the guard
`internal/regions` already uses for directory text — a compromised principal controls
this string and `key list` prints it to the terminal of the operator investigating
that compromise), then trimmed, and must be 1–100 **bytes**, else 422. `created_by`
and `revoked_by` record the calling principal via `principal.actor()`. The region must
already exist in `regions`, which is populated from OBACloud's directory export — so a
principal can mint keys only for regions OBACloud has published.

### 5.7 Registration conditions

A resource family's admin routes register only when its repository is set in `Deps`
(surveys → `Deps.Surveys`, ghost bus → `Deps.GhostBus`, alarms → `Deps.Alarms`, push
counts → `Deps.PushRegs`), keeping the nil-means-absent convention; `GET
/regions/{id}` reports the registered set as `features` (§5.1). The key-management
routes and bearer auth require `Deps.APIKeys`; `main` always sets it.

## 6. CLI and SPA

### 6.1 `sidecar-admin`

```
sidecar-admin key create --region N --name NAME             prints the raw key once, then id/name; created_by = cli
sidecar-admin key list --region N                           id, name, created by, created, last used, revoked, revoked by
sidecar-admin key list --minted-by-principal N              every key a principal minted, across regions
sidecar-admin key revoke --region N --id N
sidecar-admin principal create --name NAME                  prints obasp_… once
sidecar-admin principal list
sidecar-admin principal revoke --id N [--keep-keys]         by default ALSO revokes every live key it minted, in one
                                                            transaction, and prints their ids; --keep-keys opts out
```

`key list` prints names through the same control-character guard as §5.6 (defence
in depth: rows written before the guard, or by a future path, must not reach a
terminal unguarded). Same flag conventions as `user`, `study`, and `survey`; tests in
`cmd/sidecar-admin/keys_test.go`.

### 6.2 SPA

The SPA's own routes become region-scoped so every page has a region to put in the
API path, including on reload and deep link:

```
/admin                          region picker (auto-forwards when there is one region;
                                otherwise remembers the last choice in localStorage)
/admin/regions/[region]/alerts
/admin/regions/[region]/alerts/new
/admin/regions/[region]/alerts/[id]
/admin/regions                  unchanged (region settings)
```

Affected files: `routes/alerts/**` (moved), `routes/+page.svelte|ts` (picker),
`lib/api.ts`, `lib/alerts.ts` (`buildCreatePayload` drops `region_id`), `lib/pushes.ts`
("transport not configured" is now read from the region's `features` once, not
inferred from a per-alert 404), `lib/regions.ts`, and their tests. Old
`/admin/alerts/…` bookmarks show the SPA's not-found page.

## 7. OBACloud contract (documented; built later)

This section is the specification the Rails integration will implement. It is
recorded here so the sidecar side is designed against a concrete consumer.

### 7.1 Identity join, visibility, and teardown

The sidecar's region `id` is the id in the regions directory, which OBACloud publishes
from `Region#region_identifier` (`app/views/regions/export.json.jbuilder`,
`Regions::FileRenderer`). Every sidecar path uses that value:
`…/regions/#{region.region_identifier}/…`.

**Only published regions enter the sidecar.** The directory export includes
`Region.published` rows only, and every organization's region is created
`published: false` (`Organization#ensure_region`). After publishing, the region is
addressable in the sidecar once the directory has been regenerated and the sidecar has
synced it (hourly, and at boot) — up to about an hour. A deployment that points the
sidecar's `--regions-url` at the v2 `regions.json` additionally excludes
`experimental` regions.

**Nothing ever leaves the sidecar.** A directory sync only upserts; a region that is
later unpublished, or whose organization is destroyed, stays fully addressable with
its data and its keys. Two Rails-side rules follow:

- `region_identifier` is **immutable after first publish**: removed from
  `region_params` on update and enforced by a model validation. Otherwise an OBACloud
  admin could assign org A the identifier a destroyed org B once used and mint org A a
  key to B's rider data; the sidecar cannot detect reassignment.
- **Unpublishing a region and destroying its organization both revoke the region's
  key** (`DELETE …/api_keys/{sidecar_api_key_id}` at `sidecar_base_url`) and clear both
  credential columns, before the row changes. Revocation is the only tenancy teardown.

### 7.2 Credentials and provisioning

- Rails credentials hold `sidecar.principals`, a map from sidecar base URL to
  `obasp_…`, one entry per sidecar deployment. `Region.sidecar_base_url` **must
  validate as a key of that map**; an unknown base URL is a hard validation error and
  never falls back to a default principal. (Its current default,
  `https://dashboard.onebusawaycloud.com`, must be a real sidecar with an entry, or
  the default must change before this ships.) Principals are created by an operator
  with `sidecar-admin principal create`.
- `Region` gains `sidecar_api_key` (an `encrypts`-ed column), `sidecar_api_key_id`, and
  `sidecar_previous_api_key_id` (held until a rotation's revoke confirms). Enforced,
  not merely intended: none of the three is ever in `region_params`; a request spec
  asserts the rendered `regions.json` and `regions-v3.json` contain none of them;
  `sidecar_api_key` is in `filter_parameters`; `Region` stays un-`audited` (audit rows
  would archive plaintext copies); the key is never interpolated into a log line or a
  URL.
- `Sidecar::Provisioner.ensure_key!(region)`:
  - a no-op unless `region.published?` — an unpublished region cannot exist in the
    sidecar, so a 404 there is expected, not transient; nothing is retried;
  - **lock first, then read**: `region.with_lock` (row lock) and re-read
    `sidecar_api_key` inside it, so the after-commit trigger and the lazy trigger
    cannot both mint; the lazy trigger from `Sidecar::Client` uses `NOWAIT` semantics
    and, on contention or on a blank key, **enqueues provisioning and fails the
    current request soft** (region health warning) rather than holding a row lock
    across a 5 s HTTP call inside a Puma thread;
  - when the column is blank, `POST …/regions/{id}/api_keys` with
    `{name: "obacloud <rails host>"}` using the principal for
    `region.sidecar_base_url`, then stores key and id in the same save. If that save
    fails, it revokes the key it just minted before re-raising; if that compensating
    revoke fails too, it is logged at error level and enqueued for retry, never
    swallowed;
  - a 404 on a published region means "directory not synced yet": enqueue a retry job
    with backoff, capped at a day, and surface a region health warning if it expires.
- Triggers: the `published` false → true transition (after commit, via job), a change
  of `sidecar_base_url` on a published region, and lazily from `Sidecar::Client`.
- **Changing `sidecar_base_url`** is ordered: revoke the old key **at the old base
  URL** and clear all three columns in the same transaction, and only then provision
  against the new URL. The live key must never be presented to the new host.
- `Sidecar::Provisioner.rotate!(region)`: mint → save the new key and id and move the
  old id into `sidecar_previous_api_key_id` → revoke the previous id → clear
  `sidecar_previous_api_key_id`; same revoke-on-save-failure and retry-on-revoke-failure
  rules. Exposed to OBACloud admins as a button on the region form and callable from a
  job.
- `Sidecar::Provisioner.reprovision_all!(base_url)`: the recovery path after a
  principal compromise (§2.2): for every published region on that base URL, clear the
  three columns and run `ensure_key!` with the new principal. A rake task.
- `Sidecar::ReconcileKeysJob` (daily): for each published region, `GET
  …/api_keys` and compare live key ids against `sidecar_api_key_id` /
  `sidecar_previous_api_key_id`; revoke live keys Rails does not hold, and report any
  region whose held key is not live (that region needs `ensure_key!`).

### 7.3 Client and user authorization

`Sidecar::Client.new(region)` wraps one HTTP client with `Authorization: Bearer
#{region.sidecar_api_key}`, JSON in/out, a 5 s open/read timeout, and this error
mapping:

| sidecar | Rails |
|---|---|
| 400, 422 | `Sidecar::Invalid` carrying the error message → form errors (400 from a well-formed Rails call is also a programming error; log it) |
| 401 | `Sidecar::Unauthorized` — the key was revoked out of band; surfaced as a region health warning, with "re-provision" (clear the columns, `ensure_key!`) offered to OBACloud admins only |
| 403 | `Sidecar::Forbidden` — a route the key is not allowed to call; a programming error |
| 404 | `Sidecar::NotFound` → the controller's existing not-found handling; consult `features` on `GET /regions/{id}` (cached) first to distinguish "family not enabled" |
| 409 | `Sidecar::Conflict` carrying the message → form error |
| 5xx, timeout | `Sidecar::Unavailable` → flash and re-render; never retried on writes |

**Secrets never reach Sentry or logs.** The region key is the hot path, so: `Authorization`
is scrubbed from Sentry events and breadcrumbs (`before_send` plus header capture
disabled on the HTTP integration); `sidecar_api_key` is in `filter_parameters`; the
key is never part of an exception message or URL. `current_organization` can be nil
for an admin with an unmatched slug — the client factory guards it and renders not
found rather than raising into Sentry.

The client is obtained from `current_organization.region`. For a `Customer` that
organization comes from the account (`Current.organization`), so a customer can never
address another region's key; for an `Admin` it comes from the URL slug, and the key's
own region scope is the fence that matters.

**Which sidecar routes each OBACloud role may drive:**

| Sidecar route family | Customer | Admin |
|---|---|---|
| alerts CRUD, publish, translations, push reads | ✓ | ✓ |
| studies, surveys, responses (JSON, CSV) | ✓ | ✓ |
| ghost bus reports, alarms, push counts | ✓ | ✓ |
| `PATCH /regions/{id}` (timezone, default agency, OBA API key) | — | ✓ |
| `GET /regions/{id}` | ✓ | ✓ |

### 7.4 Migration order

Alerts first (the sidecar alert API is the most mature), then surveys and responses,
then alarms and push counts as read-only views. Each step reads from the sidecar and
stops reading the corresponding Postgres tables; dropping those tables is a later,
separate change. OBACloud never sends pushes through the sidecar.

## 8. Testing

- `storetest.RunAPIKeyRepository`: create/lookup/revoke/touch for both kinds; revoked
  rows return `ErrRevoked` from `GetByHash`; region-scoped revoke; `revoked_by`
  recorded; `ListRegionKeysByCreator` for all three kinds including `cli` (NULL id);
  `RevokeRegionKeysByCreator` atomicity and returned ids; cascade on region delete;
  ordering; the `CHECK` constraints reject mismatched actor pairs.
- `apikey`: `ParsePrefix` with a fixture whose random segment contains `_` and `-`;
  region-segment regex; `LogValue` omits hashes.
- Middleware: bearer beats cookie; malformed, duplicate, empty, over-long headers are
  401 not fall-through; revoked key 401 with `reason=revoked` in the log and no delay;
  prefix/row mismatch is 401; failure throttle charges failures only; touch at most
  hourly (fixed `Now`); nil `Deps.APIKeys` rejects bearer; the cross-site guard still
  applies to bearer requests that carry a foreign `Origin`; `principal.LogValue` omits
  the password hash.
- Route table invariants (§4.4): scope/pattern agreement, `scopeKeyAdmin` ⇔
  `/api_keys` patterns, `principalService` only on `scopeKeyAdmin`, the per-kind 403
  walk (region key refused on push create/cancel; service principal refused everywhere
  else), and the tenancy walk with bodies (`study_id` from region B → 404).
- Handler tests per new family: happy paths, 404 across regions (including a
  response reached through another region's survey), 400/409/422 mappings, strict
  survey decoding and the 256 KB cap, CSV content type, `nosniff`, `no-store`,
  filename, and formula guard, alarm JSON omits `token`/`user_push_id`, epoch-ms fields
  pass through unchanged, mint response has `no-store` and no `Location`, key `name`
  control characters stripped, raw key absent from captured logs.
- CLI tests for `key` and `principal`, including default key revocation on `principal
  revoke`, `--keep-keys`, and guarded names in `key list`.
- SPA: region picker, moved routes, `features`-driven push card,
  `api.test.ts`/`alerts.test.ts`/`pushes.test.ts` updated.
- `make check` under both `test-tz` zones. Every new test is checked to fail when the
  code under test is mutated.

## 9. Documentation

- README: "Region API keys and service principals" under the admin API section
  (format, CLI, provisioning flow, rotation, the audit columns, what each leaked
  credential can do), the updated route list, the SPA URL change, a deployment note
  that the fronting proxy must not log the `Authorization` header, and a short
  "OBACloud integration" summary pointing at §7.
- `specification/openapi.yaml` does not describe the admin API today and is left as
  is; the admin surface is documented in the README and this spec.

## 10. Build and wiring

- `cmd/sidecar/main.go`: `Deps.APIKeys = store.APIKeys()`.
- `internal/store/sqlite`: `apikeys.go`, `queries/apikeys.sql`, `make generate`.
- `internal/httpapi/json.go`: `decodeJSONStrict`.
- `Deps` gains `APIKeys apikey.Repository` and `BearerFailLimiter *ratelimit.Limiter`.
- Route registration for the new families lives in `adminRoutes`, gated per §5.7.
