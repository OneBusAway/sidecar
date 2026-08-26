# Region API Keys and the Region-Scoped Admin API — Design

**Date:** 2026-08-26
**Status:** Reviewed (red-teamed; findings folded in)
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

### 2.2 Service principals automate provisioning

A per-region key still has to get into Rails somehow. A **service principal** is a
deployment-wide credential whose only power is to mint and revoke region keys. OBACloud
holds one per sidecar deployment it talks to and mints region keys on demand (§7.2).

This is a deliberate trade: a mint-capable secret partially reintroduces the blast
radius per-region keys avoid. It is accepted because the principal is used only on the
rare provisioning path (never the hot path where secrets get logged or leak into error
reports), region keys stay individually revocable, and the principal can read nothing —
not an alert, not a region — so its compromise yields keys, never data, until those keys
are used. To make a compromise recoverable, every key records **who minted it**
(§3): after a principal leak, `key list --minted-by-principal N` identifies exactly
the keys to revoke, and `principal revoke --with-keys` revokes them in one step.

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
region against the principal (§4.3). Handlers never parse a region themselves.

The plural `regions` matches the rider API (`/api/v1/regions/{regionId}/alerts`).

### 2.6 Existing rules apply unchanged

No `time.Now` outside `cmd/` and tests; epoch-second INTEGER columns; RFC 3339 with an
explicit offset for every instant in JSON; ASCII-only comments in `queries/*.sql`; no
`sqlc.arg()` inside `IN (...)`; rider-sourced CSV cells pass the formula-injection guard.

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
    created_by_kind TEXT    NOT NULL,       -- 'operator' | 'principal' | 'cli'
    created_by_id   INTEGER,                -- users.id or service_principals.id; NULL for cli
    created_at      INTEGER NOT NULL,
    last_used_at    INTEGER,
    revoked_at      INTEGER
);
CREATE INDEX region_api_keys_region ON region_api_keys(region_id);
CREATE INDEX region_api_keys_creator ON region_api_keys(created_by_kind, created_by_id);
```

Revoked rows are kept (not deleted) so `key list` shows history and so a revoked key's
hash cannot be re-minted by accident. `regions` cascade-deletes keys (the store enables
`foreign_keys`), matching alerts. `created_by_*` is not a foreign key: a deleted
operator or a revoked principal must not orphan the audit trail.

### 3.1 Key format

- Region key: `obask_<regionID>_<43 base64url chars>` from 32 random bytes
  (`crypto/rand`), e.g. `obask_1_Qm9…`.
- Service principal: `obasp_<43 base64url chars>`.

The prefixes are project-specific so secret scanners can classify them (and do not
misfile them as Stripe `sk_` keys) and so the middleware can dispatch to the right table
without a second lookup. The region id in the plaintext is a debugging aid only: the
hash lookup decides, and the middleware treats a row whose `region_id` disagrees with
the prefix as an invalid key. Only the hex SHA-256 of the raw key is stored (the same
posture as sessions); the raw key is printed once by the CLI or returned once by the
mint route.

### 3.2 `apikey` package

```go
package apikey

type Creator struct {
    Kind string // "operator" | "principal" | "cli"
    ID   int64  // 0 for cli
}

type RegionKey struct {
    ID         int64
    RegionID   int64
    Name       string
    KeyHash    string
    CreatedBy  Creator
    CreatedAt  time.Time
    LastUsedAt *time.Time
    RevokedAt  *time.Time
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
    CreateRegionKey(ctx, regionID int64, name, keyHash string, by Creator, now time.Time) (RegionKey, error)
    // GetRegionKeyByHash returns ErrNotFound for unknown AND revoked hashes.
    GetRegionKeyByHash(ctx, keyHash string) (RegionKey, error)
    ListRegionKeys(ctx, regionID int64) ([]RegionKey, error)                 // live and revoked, newest first
    ListRegionKeysByCreator(ctx, by Creator) ([]RegionKey, error)
    // RevokeRegionKey is region-scoped: a key in another region is ErrNotFound.
    // Revoking an already-revoked key is a no-op success.
    RevokeRegionKey(ctx, regionID, id int64, now time.Time) error
    TouchRegionKey(ctx, id int64, now time.Time) error

    CreatePrincipal(ctx, name, keyHash string, now time.Time) (ServicePrincipal, error)
    GetPrincipalByHash(ctx, keyHash string) (ServicePrincipal, error)         // ErrNotFound for unknown AND revoked
    ListPrincipals(ctx) ([]ServicePrincipal, error)
    RevokePrincipal(ctx, id int64, now time.Time) error                       // does NOT revoke keys it minted
    TouchPrincipal(ctx, id int64, now time.Time) error
}
```

Both row types implement `slog.LogValuer` omitting `KeyHash`, as `regions.Region`
omits its OBA key. `NewRegionKey(regionID) (raw, hash string, err error)` and
`NewPrincipalKey()` mint and hash; `ParsePrefix(raw) (kind Kind, regionID int64, ok
bool)` splits a bearer value for dispatch. `storetest.RunAPIKeyRepository(t, newStore)`
is the conformance suite; its `newStore` returns `(apikey.Repository,
regions.Repository)` because the cascade case needs a region to delete.

Other repository additions:

- `alarms.Repository`: `ListByRegion(ctx, regionID int64) ([]Alarm, error)` and
  `GetInRegion(ctx, regionID, id int64) (Alarm, error)`; `List` (the scheduler's
  all-region sweep) is unchanged.
- `surveys.Repository`: `UpdateStudy(ctx, id int64, name, description string, now
  time.Time) (Study, error)`; `GetResponseInRegion(ctx, regionID int64, publicID
  string) (Response, error)` — one query joining `survey_responses → surveys → studies`
  so the two-hop tenancy check is a single round trip and cannot be forgotten.
- `ghostbus.Repository`: `GetByPublicID(ctx, regionID int64, publicID string) (Report,
  error)`.

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

func (p principal) canAccessRegion(id int64) bool {
    return p.kind == principalOperator || (p.kind == principalRegionKey && p.regionID == id)
}
```

The principal is stored in the request context by `requirePrincipal` and read by
`requireRegion` and by the key-management handlers (to record the creator).

### 4.2 `requirePrincipal` (replaces `requireSession`)

1. If an `Authorization` header is present, cookies are **ignored entirely** and the
   request either authenticates by bearer or fails. `ParsePrefix` dispatches on
   `obask_`/`obasp_`; the hash is looked up; a malformed header, unknown or revoked
   key, or a prefix/row region mismatch → `401 {"error":"invalid api key"}`
   **immediately** — no `FailDelay`: a 256-bit random key is not guessable, so a
   delay would defend nothing while pinning a goroutine per garbage request. Success
   sets the principal and, if `last_used_at` is null or `now.Sub(*LastUsedAt) >=
   time.Hour` (injected `Now`), touches it — best effort; a failed touch is logged, not
   surfaced.
2. Otherwise the existing session-cookie path runs unchanged and yields an operator
   principal.
3. When `Deps.APIKeys` is nil, any `Authorization` header is 401: bearer auth is simply
   not configured. `main` always sets it.

Failed bearer attempts are logged with the parsed kind, the prefix's region id (if
any), the value's length, and the peer address — never any part of the random
segment.

### 4.3 Cross-site guard, rate limiting, region middleware

`crossSiteGuard` wraps **every** admin route unchanged, bearer or not. It already
passes requests carrying neither `Origin` nor `Sec-Fetch-Site`, which is what a
server-side HTTP client sends, so no bypass is needed and none is added.

Bearer requests are not throttled: Rails is a trusted caller, the same reasoning as the
gorush webhook secret.

`requireRegion` parses `{regionId}` with strict `strconv.ParseInt` (base 10, 64-bit;
no sign, no leading zeros — the canonical form only), returns 404 for anything else
(deliberately not `pathID`'s 400: an unparseable region is "no such region", and the
code must not differ between "malformed" and "not yours"), checks
`principal.canAccessRegion`, loads the `regions.Region` (404 for unknown), and stores
it in the context. "Not visible" and "does not exist" are the same 404 so a region key
cannot probe for other regions. Handlers read the region from the context and stop
fetching it themselves.

Resource loaders (`loadAlert`, `loadStudy`, `loadSurvey` via its study,
`loadResponse` via `GetResponseInRegion`, `loadReport` via `GetByPublicID`,
`loadAlarm` via `GetInRegion`, `loadKey`) assert `resource.RegionID == ctx region` and
return 404 otherwise; this is what stops `/regions/A/alerts/{id-of-B}`. Listing
responses loads and checks the survey first.

A principal kind that a route does not allow gets `403 {"error":"forbidden"}` —
distinct from the guard's `"cross-site request rejected"` so tests and Rails can tell
them apart.

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

`registerAdminRoutes` wraps each route as `crossSiteGuard(requirePrincipal(allowed,
[requireRegion,] handler))`. Tests enumerate the table and assert:

- every pattern except login/logout has a non-empty `allowed`;
- every `regionScoped` pattern contains `{regionId}`, and every pattern containing
  `{regionId}` is `regionScoped` (so the segment is never parsed by hand);
- for each principal kind, each route returns 403 iff the kind is not in `allowed`;
- the **tenancy walk**: every `regionScoped` route, called with a region-A key against
  fixtures created in region B, returns 404 (lists return only region-A rows). The
  walk's fixture table is keyed by pattern; a route added to `adminRoutes` without an
  entry fails the test.

`concreteRoute` in the tests learns the `{regionId}`, `{publicId}`, `{lang}`, and
`{keyId}` wildcards, and the pinned route count is updated.

### 4.5 Allowed principals by route family

| Family | operator | region key | service principal |
|---|---|---|---|
| `session` whoami | ✓ | — | — |
| `GET /regions` (list) | ✓ | — | — |
| `GET /regions/{id}`, `PATCH /regions/{id}` | ✓ | ✓ (own region) | — |
| alerts, pushes, studies, surveys, responses, ghost bus, alarms, push counts | ✓ | ✓ | — |
| `…/api_keys` (mint, list, revoke) | ✓ | — | ✓ |

`PATCH /regions/{id}` by a region key lets OBACloud set timezone and default agency.
It can also set the region's OBA API key (write-only, never echoed, as today), which
means a leaked region key could redirect that region's sidecar-side OBA calls (ghost
bus snapshots, vehicle search, alarms) to an attacker's key. Accepted: the exposure is
confined to the same region the key already controls, and the key is revocable.

## 5. Admin API surface

All paths below are prefixed `/api/admin/v1`. Bodies and responses are JSON with the
existing `{"error": "…"}` envelope on failure. Instants (`created_at`, `start_time`,
…) are RFC 3339 UTC. Fields that are epoch **milliseconds** in the domain because they
are OBA identifiers or dedupe keys — `service_date`, `scheduled_arrival_at`,
`predicted_arrival_at`, `prediction_last_updated_at` — pass through as integers,
unchanged. Ids are integers except where a resource has a public id. Validation
failures are 422; malformed JSON or an unparseable query parameter is 400.

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
POST   /regions/{regionId}/alerts/{id}/pushes
GET    /regions/{regionId}/alerts/{id}/pushes
DELETE /regions/{regionId}/alerts/{id}/pushes/{pushId}
GET    /regions/{regionId}/alerts/{id}/push_audience
GET    /regions/{regionId}                              new; the region row, same shape as the list's elements
PATCH  /regions/{regionId}                              unchanged
```

Semantics are unchanged apart from the region source; the alert push routes remain
conditional on a configured transport. A `region_id` in the create body is rejected
(422) rather than ignored, so a stale client cannot believe it targeted a region.

### 5.2 Studies and surveys

The JSON survey document is the one `sidecar-admin survey create --file` already
accepts (surveys design spec §3). `Document` and its codec move from
`cmd/sidecar-admin` into `internal/surveys`, and **the API decodes it strictly**
(`DisallowUnknownFields`, as the CLI does today — a misspelled `show_on_maps` must not
silently hide a survey), which is why this decode does not go through `decodeJSON`.
`study_id` is the only study reference; a `study`, `id`, `created_at`, or `updated_at`
in a request body is rejected (422). `PUT` cannot move a survey between studies.

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
GET    /regions/{regionId}/surveys/{id}/responses.csv   text/csv; Content-Disposition: attachment; filename="survey-{id}-responses.csv"
GET    /regions/{regionId}/survey_responses/{publicId}
```

Validation failures carry the same messages the CLI prints. The CSV writer moves from
`cmd/sidecar-admin/surveys.go` into `internal/surveys`
(`WriteResponsesCSV(w, survey, responses)`) and keeps the formula-injection guard; the
CLI calls it. Filenames are fixed and server-generated — never derived from a name.

### 5.3 Ghost bus reports (read-only)

```
GET /regions/{regionId}/ghost_bus_reports?since=RFC3339        [{…report}]
GET /regions/{regionId}/ghost_bus_reports.csv?since=RFC3339    text/csv; filename="ghost-bus-reports-{regionId}.csv"
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
POST   /regions/{regionId}/api_keys            {name} → 201 {id, name, key, created_by, created_at}   key appears here and nowhere else
GET    /regions/{regionId}/api_keys            [{id, name, created_by: {kind, id}, created_at, last_used_at, revoked_at}]
DELETE /regions/{regionId}/api_keys/{keyId}    revoke; 204 (also for an already-revoked key); 404 for an unknown id or an id in another region
```

Operator or service principal only (§4.5). `name` is trimmed and must be 1–100
characters after trimming, else 422. `created_by` records the calling principal. The
region must already exist in `regions`, which is populated from OBACloud's directory
export — so a principal can mint keys only for regions OBACloud has already published.

### 5.7 Registration conditions

A resource family's admin routes register only when its repository is set in `Deps`
(surveys → `Deps.Surveys`, ghost bus → `Deps.GhostBus`, alarms → `Deps.Alarms`, push
counts → `Deps.PushRegs`), keeping the nil-means-absent convention. The key-management
routes and bearer auth require `Deps.APIKeys`; `main` always sets it.

## 6. CLI and SPA

### 6.1 `sidecar-admin`

```
sidecar-admin key create --region N --name NAME             prints the raw key once, then id/name; created_by = cli
sidecar-admin key list --region N                           id, name, created by, created, last used, revoked
sidecar-admin key list --minted-by-principal N              every key a principal minted, across regions
sidecar-admin key revoke --region N --id N
sidecar-admin principal create --name NAME                  prints obasp_… once
sidecar-admin principal list
sidecar-admin principal revoke --id N [--with-keys]         --with-keys also revokes every key it minted
```

Same flag conventions as `user`, `study`, and `survey`; tests in
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
(a 404 on the pushes routes no longer means only "transport not configured" — the
"not configured" signal becomes the route's absence at the region level, detected once
per region rather than per alert), `lib/regions.ts`, and their tests. Old
`/admin/alerts/…` bookmarks show the SPA's not-found page.

## 7. OBACloud contract (documented; built later)

This section is the specification the Rails integration will implement. It is
recorded here so the sidecar side is designed against a concrete consumer.

### 7.1 Identity join and visibility

The sidecar's region `id` is the id in the regions directory, which OBACloud publishes
from `Region#region_identifier` (`app/views/regions/export.json.jbuilder`,
`Regions::FileRenderer`). Every sidecar path uses that value:
`…/regions/#{region.region_identifier}/…`.

**Only published regions exist in the sidecar.** The directory export includes
`Region.published` rows only, and every organization's region is created
`published: false` (`Organization#ensure_region`). After publishing, the region is
addressable in the sidecar once the directory has been regenerated and the sidecar has
synced it (hourly, and at boot) — up to about an hour. A deployment that points the
sidecar's `--regions-url` at the v2 `regions.json` additionally excludes
`experimental` regions.

### 7.2 Credentials and provisioning

- Rails credentials hold `sidecar.principals`, a map from sidecar base URL to
  `obasp_…`, one entry per sidecar deployment (`Region.sidecar_base_url` already
  allows several; note its default is `https://dashboard.onebusawaycloud.com`, so that
  host must be a real sidecar or the default must change before this ships).
  Principals are created by an operator with `sidecar-admin principal create`.
- `Region` gains `sidecar_api_key` (an `encrypts`-ed column, never rendered) and
  `sidecar_api_key_id` (the sidecar's key id, for rotation and as the idempotency
  guard).
- `Sidecar::Provisioner.ensure_key!(region)`:
  - a no-op unless `region.published?` — an unpublished region cannot exist in the
    sidecar, so a 404 there is expected, not transient; nothing is retried;
  - takes a row lock on the region so the after-commit trigger and the lazy client
    trigger cannot both mint;
  - when the column is blank, `POST …/regions/{id}/api_keys` with
    `{name: "obacloud <rails host>"}` using the principal for
    `region.sidecar_base_url`, then stores key and id in the same save. If that save
    fails, it revokes the key it just minted before re-raising, so no live key exists
    that Rails does not hold;
  - a 404 on a published region means "directory not synced yet": enqueue a retry job
    with backoff, capped at a day, and surface a region health warning if it expires.
- Triggers: the `published` false → true transition (after commit, via job), a change
  of `sidecar_base_url` on a published region, and lazily from `Sidecar::Client` on
  first use.
- `Sidecar::Provisioner.rotate!(region)`: mint → save both columns → revoke the
  previous `sidecar_api_key_id`; same revoke-on-save-failure rule. Exposed to OBACloud
  admins as a button on the region form and callable from a job.

### 7.3 Client

`Sidecar::Client.new(region)` wraps one HTTP client with `Authorization: Bearer
#{region.sidecar_api_key}`, JSON in/out, a 5 s open/read timeout, and this error
mapping:

| sidecar | Rails |
|---|---|
| 401 | `Sidecar::Unauthorized` — the key was revoked out of band; surfaced as a region health warning, with "re-provision" (clear both columns, `ensure_key!`) offered to OBACloud admins only |
| 403 | `Sidecar::Forbidden` — a route the key is not allowed to call; a programming error, reported to Sentry |
| 404 | `Sidecar::NotFound` → the controller's existing not-found handling |
| 409, 422 | `Sidecar::Invalid` carrying the error message → form errors |
| 5xx, timeout | `Sidecar::Unavailable` → flash and re-render; never retried on writes |

The client is obtained from `current_organization.region`. For a `Customer` that
organization comes from the account (`Current.organization`), so a customer can never
address another region's key; for an `Admin` it comes from the URL slug, and the key's
own region scope is the fence that matters.

### 7.4 Migration order

Alerts first (the sidecar alert API is the most mature), then surveys and responses,
then alarms and push counts as read-only views. Each step reads from the sidecar and
stops reading the corresponding Postgres tables; dropping those tables is a later,
separate change.

## 8. Testing

- `storetest.RunAPIKeyRepository`: create/lookup/revoke/touch for both kinds, revoked
  rows are invisible to `GetByHash`, region-scoped revoke, `ListRegionKeysByCreator`,
  cascade on region delete, ordering.
- Middleware: bearer beats cookie; malformed header is 401 not fall-through; revoked
  key 401 with no delay; prefix/row mismatch is 401; touch at most hourly (fixed `Now`);
  nil `Deps.APIKeys` rejects bearer; the cross-site guard still applies to bearer
  requests that carry a foreign `Origin`.
- Route table invariants and the tenancy walk (§4.4), including the per-kind 403 walk
  and `PATCH /regions/{id}` refusing a service principal.
- Handler tests per new family: happy paths, 404 across regions (including a
  response reached through another region's survey), 409/422 mappings, strict survey
  decoding, CSV content type, filename, and formula guard, alarm JSON omits
  `token`/`user_push_id`, epoch-ms fields pass through unchanged.
- CLI tests for `key` and `principal`, including `--with-keys`.
- SPA: region picker, moved routes, `api.test.ts`/`alerts.test.ts`/`pushes.test.ts`
  updated.
- `make check` under both `test-tz` zones. Every new test is checked to fail when the
  code under test is mutated.

## 9. Documentation

- README: "Region API keys and service principals" under the admin API section
  (format, CLI, provisioning flow, rotation, the audit columns), the updated route
  list, the SPA URL change, and a short "OBACloud integration" summary pointing at §7.
- `specification/openapi.yaml` does not describe the admin API today and is left as
  is; the admin surface is documented in the README and this spec.

## 10. Build and wiring

- `cmd/sidecar/main.go`: `Deps.APIKeys = store.APIKeys()`.
- `internal/store/sqlite`: `apikeys.go`, `queries/apikeys.sql`, `make generate`.
- Route registration for the new families lives in `adminRoutes`, gated per §5.7.
