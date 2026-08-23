# Surveys and Survey Responses — Design

**Date:** 2026-08-22
**Status:** Draft
**Implements:** [specification.md](../../../specification/specification.md) §7, and the
`listSurveys`, `createSurveyResponse`, `amendSurveyResponse{,Post,Put}` operations in
[openapi.yaml](../../../specification/openapi.yaml).

## 1. Scope

The rider-facing survey service plus the minimum authoring surface that makes it
useful:

```
GET  /api/v1/regions/{regionId}/surveys[.json]?user_id=…   → SurveyList JSON
POST /api/v1/survey_responses[/]                            → 201 SurveyResponseCreated
POST|PUT|PATCH /api/v1/survey_responses/{responseId}        → 200 SurveyResponseCreated
```

and `sidecar-admin study …` / `sidecar-admin survey …` commands to author surveys and
export their responses, following the `alert` command family.

The contract was verified against three repositories, not just the spec prose: the
reference implementation (`../obacloud`), the iOS client (`../ios`), and the Android
client (`../android`). Several load-bearing requirements below exist only in the
clients. They are called out as **client contract** and each one is pinned by a test
(§10).

### Out of scope

- **Admin API and SPA pages for surveys.** The CLI is the authoring surface in this
  slice, exactly as alerts shipped first. The domain package and repository are
  designed so the admin API is a thin layer over them in a follow-up.
- **Ghost bus reports (§8).** The next slice; it reuses the params bag and rate limiter
  and nothing from here.
- **Server-side "already answered" filtering.** `user_id` is required on the list
  endpoint and otherwise unused, matching the reference. Both apps track completion
  locally and neither asks the server.
- **Enforcing `allows_multiple_responses`.** A passthrough flag in the reference too;
  the apps apply it client-side.
- **Validating answers against the survey's questions.** The reference stores whatever
  `question_id`s the client sends. Clients can be ahead of or behind the authored
  questions, and the answer carries its own denormalized label for export.
- **Async enrichment, analytics, dashboards.** Export is a CSV; everything else is
  agency tooling outside the sidecar.

## 2. Decisions

### 2.1 Routes mirror what shipped clients actually call, not only the spec paths

**Client contract.** Both apps fetch `…/surveys.json` (the Rails format suffix), and
both POST the create to `/api/v1/survey_responses/` with a trailing slash. Neither uses
the returned `update_path`: each rebuilds `/api/v1/survey_responses/{id}` itself. iOS
amends with `PUT` and a JSON body; Android amends with `POST` and a form body. `PATCH`
is in the OpenAPI and is accepted, but no shipped client sends it.

So the router registers, with Go 1.22 mux patterns:

```
GET   /api/v1/regions/{regionId}/surveys
GET   /api/v1/regions/{regionId}/surveys.json
POST  /api/v1/survey_responses
POST  /api/v1/survey_responses/{$}          -- exact trailing slash
POST  /api/v1/survey_responses/{responseId}
PUT   /api/v1/survey_responses/{responseId}
PATCH /api/v1/survey_responses/{responseId}
```

`{$}` and `{responseId}` do not conflict: `{$}` matches only the empty trailing segment
and a wildcard needs a non-empty one. Without the `{$}` registration the Android and
iOS create call would 404, since `http.ServeMux` does not strip trailing slashes on
write methods.

iOS appends `key`, `app_uid`, `app_ver`, and `version=2` query items (inherited from
its OBA REST config) to every survey request. They are ignored.

### 2.2 `update_path` is still emitted, and still correct

Both clients ignore it, but the spec requires it and a future client might honor it.
The emitted value is `/api/v1/survey_responses/{publicID}` — the same path the clients
rebuild, so a client that *does* honor it lands in the same place.

### 2.3 The list payload is shaped for the strict client, the iOS app

**Client contract.** iOS decodes with synthesized `Codable` and hard-requires, on every
survey: `id`, `name`, `created_at`, `updated_at`, `study` (with `id` and `name`),
`questions` (array, may be empty), and on every question: `id`, `position`,
`required`, `content.type`, `content.label_text`. One missing key fails the decode of
the *entire* response — every survey in the region disappears. An unrecognized
`content.type` does the same. Dates must be RFC 3339 with an explicit `Z`/offset and
exactly zero or three fractional digits.

Android is the lenient one (`ignoreUnknownKeys`, `coerceInputValues`) but treats a
`null` boolean as `false`, so `show_on_map: null` hides a survey from the map.

Therefore:

- Every key in the OpenAPI `Survey` and `SurveyQuestion` schemas is always present.
  Booleans are always literal `true`/`false`. `visible_stop_list` / `visible_route_list`
  are an array or literal `null`, never omitted.
- `created_at` / `updated_at` / `start_date` / `end_date` are formatted as
  `2006-01-02T15:04:05.000Z` in UTC. Three fractional digits are always emitted, even
  though the stored precision is seconds, because `.000Z` is what the reference emits
  and what the iOS fixture decodes.
- `content.type` is constrained to the five known values by a `CHECK` constraint *and*
  by `Content.Validate()` at authoring time. A sixth type is a client release, not a
  server change.
- Per-type key emission follows the reference jbuilder exactly: `options` only for
  `radio`/`checkbox`; `url`, `survey_provider`, `embedded_data_fields`, and
  `sdk_configuration_values` only for `external_survey`; `text`/`label` carry just
  `type` and `label_text`. The iOS decoder tolerates extra keys, so this is fidelity,
  not necessity — but a test pins it so the wire shape does not drift.
- `required` is forced to `false` for `label` and `external_survey` questions at
  authoring time (the reference's `set_required_param`). The wire never says a
  display-only question is required.

### 2.4 Server-side window filtering is load-bearing

**Client contract.** Android performs no `start_date`/`end_date` filtering at all and
iOS only filters what it has already received. A survey the server returns after its
window closed is shown. The list query is therefore:

```
available = TRUE
AND (start_time IS NULL OR start_time <= @now)
AND (end_time   IS NULL OR end_time   >= @now)
ORDER BY id ASC
```

The reference's predicate (`now BETWEEN start AND end OR start IS NULL OR end IS NULL`)
treats a half-set window as "always active". Ours never sees one: authoring rejects a
survey with only one of the two dates, so the simpler predicate is equivalent on every
reachable row. Both bounds are inclusive, matching `BETWEEN`.

`now` is the injected clock (`Deps.Now`), passed into the repository as an argument.

### 2.5 `responses` is an opaque JSON-array string, and `answer` is an opaque string

The `responses` parameter is, by long-standing contract, a string containing a JSON
array (spec §7.2). It is parsed into `[]Answer`, validated structurally, and stored as
the canonical JSON array the server re-serializes — never the client's raw bytes.

**Client contract.** The two apps disagree on the checkbox `answer` format. iOS sends a
JSON-array string (`"[\"Bus\",\"Train\"]"`); Android sends Kotlin's `List.toString()`
(`"[Bus, Train]"`). The server stores `answer` verbatim as a string and never
interprets it. Export hands the agency exactly what the rider's app sent.

Structural rules, each a 422 `{"errors": ["responses must be a JSON-encoded array of
answer objects"]}` (the reference's message) when violated:

- `responses` absent, or not a string, or not valid JSON, or not an array.
- Any element that is not a JSON object.
- Any element whose `question_id` is absent or not an integral number.

Lenient rules, matching the reference's type coercion: `answer`, `question_type`, and
`question_label` are strings when present; a JSON number or boolean is stringified; a
missing or `null` one is stored as `""`. Extra keys on an element are dropped.

Duplicate `question_id`s within one request: the last one wins, so the merge below has
a single answer per question to work with.

### 2.6 Amend merges by `question_id` inside one transaction

Merge semantics (spec §7.2, reference `upsert_responses`): an incoming answer replaces
the stored answer with the same `question_id`; every other stored answer is preserved.
The stored order is kept, with replaced answers staying in place and new ones appended
— the reference puts new keys first, which no client observes and which makes the
export order jump around.

The merge runs inside `BEGIN IMMEDIATE` in the adapter, with the pure `MergeAnswers`
function from the domain package doing the work between the read and the write. Two
concurrent amends of one response (a retry racing its original, or the iOS hero PUT
racing a second PUT) must both land; a read-merge-write outside a transaction loses
one. The conformance suite pins this with the same concurrent-edit shape used for
alerts.

Amend ignores every parameter except `responses`. Android resends `user_identifier`,
`survey_id`, and the zeroed stop fields on its amend; none of them may change the row.

A response's stored answer count is capped at 500 distinct `question_id`s. The 64 KB
body limit bounds each request, but merges accumulate across amends and an
unauthenticated caller could otherwise grow one row without limit. Exceeding the cap
is a 422 with `"responses has too many answers"`.

### 2.7 Validation messages reproduce the reference's Rails phrasing

Clients display nothing from a 422 body, so these are for the operator reading logs and
for fidelity with the documented contract. Create fails with 422 `{"errors": [...]}`
listing, in this order, whichever apply:

| Condition | Message |
|---|---|
| `user_identifier` blank | `User identifier can't be blank` |
| survey has `require_stop_id` and `stop_identifier` blank | `Stop identifier can't be blank` |
| `stop_identifier` present and `stop_latitude` absent/unparseable | `Stop latitude can't be blank` |
| `stop_identifier` present and `stop_longitude` absent/unparseable | `Stop longitude can't be blank` |
| `responses` malformed (§2.5) | `responses must be a JSON-encoded array of answer objects` |

**Client contract.** Android sends `stop_latitude=0.0&stop_longitude=0.0` with *no*
`stop_identifier` on every submission. Coordinates without an identifier are accepted
and stored as given; only the reverse (identifier without coordinates) is an error.
Latitude is validated to ±90 and longitude to ±180 when present; out of range is
`Stop latitude is invalid` / `Stop longitude is invalid`.

`survey_id` absent, non-integer, or unknown is `404 {"error": "Couldn't find Survey"}`
— the reference's `Survey.find` behavior, and the spec's "unknown survey" shape.
Unknown `{responseId}` on amend is `404 {"error": "Couldn't find SurveyResponse"}`.

The list endpoint's only validation is `user_id`: blank after trimming is
`422 {"errors": ["user_id is required"]}`. The region 404 is checked first.

### 2.8 Public identifiers come from `securetoken`

The reference mints `SecureRandom.hex(10)`. Both clients treat the id as an opaque
string (iOS rejects an integer; Android tolerates one), so `securetoken.New()` — 22
URL-safe characters of 128 random bits, already used for alarms — is used unchanged.
The path segment is URL-safe by construction. Sequential ids never appear on the wire.

### 2.9 Both write endpoints are rate limited, beyond what the spec requires

Spec §2.6 lists throttles for push registrations and ghost bus reports and none for
surveys, and the reference has none. But create and amend are unauthenticated writes to
agency data with indefinite retention; a hostile client can fill the table. A fixed
window of **60 per minute per IP** across create and amend together (one bucket, like
the push registration path) is far above any real rider — the apps send one create and
at most one amend per survey — and bounds the write rate of a single source. `429`
with an empty body, as the existing `throttleByIP` produces. The list endpoint is
unthrottled: it is a read, the iOS app already self-limits to one fetch per 300 s, and
a `429` there would hide surveys from legitimate riders sharing a NAT.

### 2.10 Surveys belong to a study, and a study belongs to a region

The reference's hierarchy is organization → study → survey; a region maps to an
organization. Here a study belongs directly to a region. The `study` object on the
wire is required by iOS (§2.3), so `surveys.study_id` is `NOT NULL` and a study must
exist before its first survey. The reference auto-creates a survey per study; we do
not — an empty study is harmless and an unwanted survey is not.

### 2.11 Targeting lists are stored as JSON arrays, not CSV

The reference stores `visible_stop_list` as a comma-separated string and splits it on
serialization. GTFS ids are free-form and a comma inside one is not impossible. The
column is `TEXT` holding either `NULL` or a JSON array of strings, parsed by the
adapter. An empty list is normalized to `NULL` at write time: the reference's `blank?`
check emits `null` for an empty string, iOS treats empty and `null` alike, and Android
treats an empty list as "match nothing", which no author means.

### 2.12 Question content is one JSON column, typed in Go

`survey_questions.content` is a `TEXT` JSON document; `question_type` is duplicated into
its own `CHECK`-constrained column so the five-value invariant of §2.3 holds at the
schema level and not only in Go. The domain type `Content` owns validation
(`Validate`) and wire emission (`MarshalJSON`, per-type keys per §2.3). The adapter
parses stored content on read; content that fails to parse is a store error, never a
partially-emitted survey. This is the same pattern as alerts' enum columns: validated
in Go, constrained in SQL.

`sdk_configuration_values` is stored as the raw JSON object the author supplied and
emitted as-is; the OpenAPI's "omitted when unparseable" clause cannot arise because
authoring rejects invalid JSON. Neither client reads it.

### 2.13 Authoring is a JSON document, not a flag per field

An alert is eleven flat fields; a survey is a dozen fields plus an ordered list of
typed questions. `--option` flags repeated per question do not express that. So
`survey create` and `survey edit` take `--file <path>` (or `-` for stdin) holding a
**survey document** — the wire `Survey` shape minus the server-owned keys
(`id`, `created_at`, `updated_at`, `study`) plus `available` and `require_stop_id`:

```json
{
  "name": "Rider satisfaction",
  "available": true,
  "start_date": "2026-09-01T00:00:00-07:00",
  "end_date": "2026-09-30T23:59:00-07:00",
  "show_on_map": false,
  "show_on_stops": true,
  "always_visible": false,
  "allows_multiple_responses": false,
  "require_stop_id": false,
  "visible_stop_list": ["1_570", "1_578"],
  "visible_route_list": null,
  "questions": [
    { "required": true,
      "content": { "type": "radio", "label_text": "How was your trip?",
                   "options": ["Great", "Fine", "Bad"] } },
    { "content": { "type": "text", "label_text": "Anything else?" } }
  ]
}
```

`position` is the array index plus one; the document's order is the display order.
`survey show <id>` prints the same document with the server-owned keys added, so
`show | edit --file -` is a round trip. Dates follow the `alert create` rule: RFC 3339
with an explicit offset, or rejected.

On `edit`, questions are reconciled by `id`: a question carrying an `id` that belongs
to this survey is updated in place and keeps its id; one without an `id` is inserted;
a stored question absent from the document is deleted. Ids are what riders' stored
answers reference and what iOS uses to dedupe locally, so an edit that only fixes a
typo must not renumber the survey. An `id` that belongs to another survey or does not
exist is an error.

### 2.14 Export is long format

`survey responses <id> --csv` emits one row per **answer**, not per response:

```
response_id,user_identifier,stop_identifier,stop_latitude,stop_longitude,created_at,updated_at,question_id,question_type,question_label,answer
```

A wide format (one column per question) has to choose between the survey's current
questions and the answers' denormalized labels, and loses answers to questions that
were since deleted. Long format loses nothing and pivots in any spreadsheet. A response
with zero answers still emits one row with the answer columns empty, so abandoned
submissions are visible. Timestamps are the §2.3 format. Without `--csv` the command
prints one JSON object per line (the `Response` shape), for scripting.

### 2.15 Existing rules apply unchanged

`time.Now`/`time.Local` are banned outside `cmd/`; the handlers use `Deps.Now` and the
repository takes `now` as an argument. Timestamps are epoch-second `INTEGER` columns
(never `DATETIME` — modernc writes `time.Time.String()` into those and `ORDER BY`
sorts text). sqlc queries use named arguments only, never mixed with bare `?`.
Nothing outside `internal/store/sqlite` sees a `gen.*` type. Portable SQL only. Every
POST accepts form, query, and JSON parameters through the existing `params` bag. No
rider-supplied value is logged except counts and ids: `user_identifier` is a device
identifier and answers are agency data.

## 3. Data model

### 3.1 Migration `00006_surveys.sql`

```sql
CREATE TABLE studies (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  region_id   INTEGER NOT NULL REFERENCES regions(id) ON DELETE CASCADE,
  name        TEXT    NOT NULL,
  description TEXT    NOT NULL DEFAULT '',
  created_at  INTEGER NOT NULL,
  updated_at  INTEGER NOT NULL
);
CREATE INDEX studies_region_idx ON studies (region_id);

CREATE TABLE surveys (
  id                        INTEGER PRIMARY KEY AUTOINCREMENT,
  study_id                  INTEGER NOT NULL REFERENCES studies(id) ON DELETE CASCADE,
  name                      TEXT    NOT NULL,
  available                 BOOLEAN NOT NULL DEFAULT TRUE,
  -- Both set or both NULL; enforced at authoring (design spec 2.4).
  start_time                INTEGER,
  end_time                  INTEGER,
  show_on_map               BOOLEAN NOT NULL DEFAULT FALSE,
  show_on_stops             BOOLEAN NOT NULL DEFAULT FALSE,
  always_visible            BOOLEAN NOT NULL DEFAULT FALSE,
  allows_multiple_responses BOOLEAN NOT NULL DEFAULT FALSE,
  require_stop_id           BOOLEAN NOT NULL DEFAULT FALSE,
  -- NULL = everywhere; otherwise a JSON array of ids (design spec 2.11).
  visible_stop_list         TEXT,
  visible_route_list        TEXT,
  created_at                INTEGER NOT NULL,
  updated_at                INTEGER NOT NULL
);
CREATE INDEX surveys_study_idx ON surveys (study_id);

CREATE TABLE survey_questions (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  survey_id     INTEGER NOT NULL REFERENCES surveys(id) ON DELETE CASCADE,
  position      INTEGER NOT NULL,
  required      BOOLEAN NOT NULL DEFAULT FALSE,
  question_type TEXT    NOT NULL
    CHECK (question_type IN ('text', 'label', 'radio', 'checkbox', 'external_survey')),
  -- The full content document (design spec 2.12); question_type is
  -- duplicated out of it so the CHECK above can hold.
  content       TEXT    NOT NULL,
  created_at    INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL,
  UNIQUE (survey_id, position)
);

CREATE TABLE survey_responses (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  survey_id       INTEGER NOT NULL REFERENCES surveys(id) ON DELETE CASCADE,
  public_id       TEXT    NOT NULL UNIQUE,
  user_identifier TEXT    NOT NULL,
  stop_identifier TEXT,
  stop_latitude   REAL,
  stop_longitude  REAL,
  -- JSON array of answer objects, canonical server serialization
  -- (design spec 2.5), merged by question_id on amend (2.6).
  answers         TEXT    NOT NULL DEFAULT '[]',
  created_at      INTEGER NOT NULL,
  updated_at      INTEGER NOT NULL
);
CREATE INDEX survey_responses_survey_idx ON survey_responses (survey_id);
CREATE INDEX survey_responses_user_idx   ON survey_responses (user_identifier);
```

The `UNIQUE (survey_id, position)` constraint means an edit that reorders questions
must write positions in two passes (negate, then assign) or delete-and-reinsert the
moved ones; the adapter does the former inside the edit transaction. `stop_latitude` /
`stop_longitude` are `REAL` (Postgres: `DOUBLE PRECISION`) because the client sends
doubles and the values are only ever echoed to an export.

`TestMigrateDeclaresTimeColumnsAsInteger` is a table of tables; the four new ones are
added to it (`studies`, `surveys` with `start_time`/`end_time`, `survey_questions`,
`survey_responses`).

### 3.2 Queries (`queries/surveys.sql`)

Studies: `CreateStudy`, `GetStudy`, `ListStudiesByRegion`, `UpdateStudy`, `DeleteStudy`,
`CountSurveysInStudy`.

Surveys: `CreateSurvey`, `GetSurvey`, `GetSurveyRegion` (joins study → region, for the
response path), `ListSurveysByRegion` (all, for the CLI), `ListActiveSurveysByRegion`
(§2.4 predicate, `@now`), `UpdateSurvey`, `SetSurveyAvailable`, `DeleteSurvey`,
`CountResponsesForSurvey`.

Questions: `ListQuestionsBySurvey` (`ORDER BY position ASC, id ASC`),
`ListQuestionsBySurveyIDs` (one query for the list endpoint, `WHERE survey_id IN
(sqlc.slice('ids'))`), `InsertQuestion`, `UpdateQuestion`, `DeleteQuestion`,
`ShiftQuestionPositionsNegative`.

Responses: `CreateResponse`, `GetResponseByPublicID`, `UpdateResponseAnswers`,
`ListResponsesBySurvey` (`ORDER BY created_at ASC, id ASC`).

`sqlc.slice` expands to `IN (?, ?, …)` on SQLite and MySQL only; a Postgres adapter
writes the same query as `= ANY(@ids)`. That is the one query that differs per engine,
and it lives in the engine's own `queries/` directory by design (alerts spec §2.7).

### 3.3 Domain types (`internal/surveys`)

```go
type Study struct {
    ID, RegionID            int64
    Name, Description       string
    CreatedAt, UpdatedAt    time.Time
}

type Survey struct {
    ID, StudyID             int64
    Name                    string
    Available               bool
    StartTime, EndTime      *time.Time   // both nil or both set
    ShowOnMap, ShowOnStops  bool
    AlwaysVisible           bool
    AllowsMultipleResponses bool
    RequireStopID           bool
    VisibleStopList         []string     // nil = everywhere; never empty
    VisibleRouteList        []string
    Questions               []Question   // position order
    Study                   Study        // populated on every read
    CreatedAt, UpdatedAt    time.Time
}

type Question struct {
    ID       int64
    Position int64
    Required bool
    Content  Content
}

type Content struct {
    Type                   string   // one of the five; see TypeText etc.
    LabelText              string
    Options                []string // radio, checkbox
    URL                    string   // external_survey
    SurveyProvider         string   // external_survey
    EmbeddedDataFields     []string // external_survey
    SDKConfigurationValues json.RawMessage // external_survey; a JSON object or nil
}

type Answer struct {
    QuestionID    int64  `json:"question_id"`
    QuestionType  string `json:"question_type"`
    QuestionLabel string `json:"question_label"`
    Answer        string `json:"answer"`
}

type Response struct {
    ID, SurveyID                 int64
    PublicID, UserIdentifier     string
    StopIdentifier               string   // "" = absent
    StopLatitude, StopLongitude  *float64 // nil = absent
    Answers                      []Answer
    CreatedAt, UpdatedAt         time.Time
}
```

Pure functions, all unit-tested without a store:

- `(Content) Validate() error` — type in the allow-list; `label_text` non-blank;
  `options` non-empty with no blank entries for `radio`/`checkbox` and absent
  otherwise; `url` an absolute `http(s)` URL for `external_survey` and absent
  otherwise; `sdk_configuration_values` a JSON object or absent. (iOS rejects an
  external survey URL without scheme and host at open time — validating here means
  the author finds out, not the rider.)
- `(Content) MarshalJSON` — the per-type key emission of §2.3.
- `ValidateWindow(start, end *time.Time) error` — both or neither; `end > start`.
- `NormalizeList([]string) []string` — trims, drops blanks, returns nil for empty.
- `ParseAnswers(raw string) ([]Answer, error)` — §2.5, returning `ErrMalformedAnswers`.
- `MergeAnswers(stored, incoming []Answer) []Answer` — §2.6.
- `FormatTime(time.Time) string` — the `.000Z` wire format.

The repository:

```go
type Repository interface {
    CreateStudy(ctx, NewStudy, now) (Study, error)
    GetStudy(ctx, id) (Study, error)                          // ErrNotFound
    ListStudies(ctx, regionID) ([]Study, error)
    UpdateStudy(ctx, id, StudyPatch, now) (Study, error)
    DeleteStudy(ctx, id) error                                // ErrNotFound; ErrStudyNotEmpty

    CreateSurvey(ctx, studyID, SurveyDefinition, now) (Survey, error)
    GetSurvey(ctx, id) (Survey, error)                        // with Questions and Study
    ListSurveys(ctx, regionID) ([]Survey, error)              // authoring; every survey
    ListActiveSurveys(ctx, regionID, now) ([]Survey, error)   // rider list; spec 7.1
    UpdateSurvey(ctx, id, SurveyDefinition, now) (Survey, error) // question reconcile, 2.13
    SetSurveyAvailable(ctx, id, available bool, now) error
    DeleteSurvey(ctx, id) error
    CountResponses(ctx, surveyID) (int64, error)

    CreateResponse(ctx, NewResponse, now) (Response, error)   // ErrNotFound for survey
    GetResponse(ctx, publicID) (Response, error)              // ErrNotFound
    AmendResponse(ctx, publicID, incoming []Answer, now) (Response, error) // ErrNotFound, ErrTooManyAnswers
    ListResponses(ctx, surveyID) ([]Response, error)
}
```

`SurveyDefinition` is the authoring document of §2.13 as a Go struct (questions carry
an optional `ID`). `CreateSurvey`, `UpdateSurvey`, and `AmendResponse` each run in one
`BEGIN IMMEDIATE` transaction in the SQLite adapter. `GetSurvey` / list calls populate
`Study` and `Questions` with at most three queries per call (surveys, questions by
survey ids, studies), never one per survey.

## 4. HTTP

### 4.1 Router and Deps

```go
// Surveys backs the survey list and survey response endpoints (spec §7).
// Nil means those routes are not registered.
Surveys surveys.Repository
// SurveyLimiter is the per-IP throttle on survey response writes (design
// spec 2.9). NewRouter defaults it (60/minute); tests inject tighter ones.
SurveyLimiter *ratelimit.Limiter
```

When `Surveys` is set, `Regions` and `Now` are required, checked at boot with the
`missingDeps` panic the other blocks use. The seven routes of §2.1 are registered; the
five write routes are wrapped in `throttleByIP(deps.SurveyLimiter, …)`.

### 4.2 `GET …/surveys[.json]`

1. `resolveRegion` — 404 `{"error": "Couldn't find Region"}` on an unknown or malformed
   segment.
2. `user_id` from the query string, trimmed; blank → 422 `{"errors": ["user_id is required"]}`.
3. `ListActiveSurveys(region.ID, deps.Now())`; a store error is the standard 500.
4. Emit `{"surveys": [...], "region": {"id": region.ID, "name": region.Name}}`, with
   each survey rendered per §2.3. `surveys` is `[]`, never `null`, when empty.

Response `Content-Type` is `application/json` — iOS rejects any other on this GET.

### 4.3 `POST /api/v1/survey_responses[/]`

1. `parseRequestParams` with `requestBodyLimit`; a body error is 422 `{"errors": [msg]}`.
2. `survey_id` via `params.int64`; absent or unparseable → 404 `Couldn't find Survey`
   without a store read. Otherwise `GetSurvey`; `ErrNotFound` → the same 404.
3. Validate per §2.7, collecting every message; any → 422.
4. `ParseAnswers`; error → 422.
5. `securetoken.New()`; `CreateResponse`. → 201 `{"survey_response": {"id", "user_identifier", "update_path"}}`.

### 4.4 `POST|PUT|PATCH /api/v1/survey_responses/{responseId}`

One handler for all three methods. Parse params; `responses` per §2.5 → 422 on error;
`AmendResponse(publicID, answers, now)`; `ErrNotFound` → 404 `Couldn't find
SurveyResponse`; `ErrTooManyAnswers` → 422; otherwise 200 with the same body shape as
create. An empty `responses` array is a valid no-op amend that still returns 200 and
bumps `updated_at`.

### 4.5 Error handling summary

| Condition | Status | Body |
|---|---|---|
| Unknown region on list | 404 | `{"error": "Couldn't find Region"}` |
| Blank `user_id` | 422 | `{"errors": ["user_id is required"]}` |
| Unknown/absent `survey_id` | 404 | `{"error": "Couldn't find Survey"}` |
| Create validation | 422 | `{"errors": [...]}` per §2.7 |
| Malformed `responses` | 422 | `{"errors": ["responses must be a JSON-encoded array of answer objects"]}` |
| Answer cap exceeded | 422 | `{"errors": ["responses has too many answers"]}` |
| Unknown `{responseId}` | 404 | `{"error": "Couldn't find SurveyResponse"}` |
| Over the write throttle | 429 | empty |
| Body over 64 KB | 422 | `{"errors": ["request body too large"]}` |
| Store failure | 500 | `{"error": "internal server error"}` (existing helper) |

Writes use a new `writeErrors(w, logger, msgs)` helper for the `{"errors": [...]}`
shape; `errorWithMessages` stays for the `{"error", "messages"}` shape.

## 5. `sidecar-admin`

New command families, dispatched beside `region`, `alert`, `migrate`, and `user`:

```
study  create    --region N --name S [--description S]      → "created study <id>"
study  list      --region N
study  edit      <id> [--name S] [--description S]
study  delete    <id>                                        → refuses while surveys exist

survey create    --study N --file <path|->                   → "created survey <id>"
survey list      --region N                                   → id, study, name, available, window, responses
survey show      <id>                                         → the document of 2.13, with id/timestamps/study
survey edit      <id> --file <path|->                         → reconcile per 2.13
survey activate  <id>  /  survey deactivate <id>
survey delete    <id> [--force]                               → refuses while responses exist unless --force
survey responses <id> [--csv]                                 → 2.14
```

The document's `start_date` / `end_date` go through `parseInstant` with the study's
region, so the naive-datetime rejection and the helpful timezone message carry over.
`survey create`/`edit` validate the entire document — window, every question's
`Content.Validate()`, `visible_*` normalization — before the first repository call, so a
rejected document leaves nothing behind (the `alertCreate` rule).

`survey list` prints `responses` as a count, which is the one number an agency asks
for daily.

## 6. Configuration

None. The limiter default is a constant; the routes register whenever the store is
open, which is always in `cmd/sidecar`. `README.md` gets a "Surveys" section
documenting the endpoints, the document format, and the CLI, mirroring the service
alerts section.

## 7. Packages

| Path | Responsibility |
|---|---|
| `internal/surveys/surveys.go` | Domain types, `Repository`, errors |
| `internal/surveys/content.go` | `Content` validation and wire marshalling |
| `internal/surveys/answers.go` | `ParseAnswers`, `MergeAnswers`, the answer cap |
| `internal/surveys/window.go` | `ValidateWindow`, `NormalizeList`, `FormatTime` |
| `internal/store/sqlite/migrations/00006_surveys.sql` | Schema |
| `internal/store/sqlite/queries/surveys.sql` | sqlc queries |
| `internal/store/sqlite/surveys.go` | Adapter; transactions for create/edit/amend |
| `internal/store/storetest/surveytest.go` | Conformance suite |
| `internal/httpapi/surveys.go` | List, create, amend handlers; `writeErrors` |
| `internal/httpapi/router.go` | Deps and routes |
| `cmd/sidecar-admin/studies.go`, `surveys.go` | CLI command families |
| `cmd/sidecar/main.go` | `Deps.Surveys = store.Surveys()` |

## 8. Dependencies

None new. `encoding/csv` from the standard library for export.

## 9. Build order

1. `internal/surveys` — pure functions and types, fully tested without a store.
2. Migration, queries, `make generate`, adapter, conformance suite.
3. HTTP handlers and router wiring.
4. CLI command families.
5. `cmd/sidecar` wiring, README.

## 10. Testing strategy

Test-driven, dependency order, and — per the lesson recorded after the alarms branch —
every test is **mutated before it is trusted**: flip the condition it claims to pin and
watch it fail on the intended assertion.

**1 — Domain.** `Content.Validate` table: each type's required and forbidden keys, the
blank-option case, the schemeless URL, the non-object `sdk_configuration_values`.
`Content.MarshalJSON` table: the exact key set per type, compared as decoded
`map[string]any` (never a golden string — `encoding/json` key order is stable but the
point is the key set, not the bytes). `ParseAnswers`: the Android checkbox string
`"[Bus, Train]"` and the iOS one both survive verbatim; numeric `answer` stringified;
missing `answer` becomes `""`; non-integral `question_id` rejected; a native JSON array
(not a string) rejected; duplicate ids last-wins. `MergeAnswers`: replace in place,
append new, preserve order, stored unchanged when incoming is empty. `ValidateWindow`:
half-set rejected, `end <= start` rejected. `NormalizeList`: blanks dropped, empty →
nil. `FormatTime`: `.000Z` with a non-UTC input.

**2 — Store.** Conformance suite: study/survey/question round trip including every
boolean and both list columns as nil and populated; active filter at both inclusive
boundaries, `available = false` excluded, unscheduled always included, other regions
excluded, ordering by id; `UpdateSurvey` reconciliation — id kept on update, insert
without id, delete when absent, reorder without a UNIQUE violation, foreign-survey id
rejected; `DeleteStudy` refuses when non-empty; `CountResponses`; `CreateResponse` on
an unknown survey → `ErrNotFound`; `AmendResponse` merge and `ErrNotFound`; the cap;
**two concurrent `AmendResponse` calls on one row both land**; cascade survey → questions
and responses; `end_time > 2^31` round trip; store content that fails to parse surfaces
as an error from `GetSurvey`.

**3 — HTTP.** `httptest` against a real store. Every **client contract** line has a
test named for it:

- `.json` suffix and bare path both 200; trailing-slash and bare create both 201.
- Create via form body (the Android shape, including `stop_latitude=0.0` with no
  identifier) and via JSON body (the iOS shape, `responses` as a string).
- Amend via `PUT`+JSON, `POST`+form, and `PATCH`; the amend ignores `user_identifier`
  and `survey_id`.
- The list payload decodes into a struct that mirrors iOS's required-key set, with
  every required key asserted present and every boolean asserted literal; `study`
  present; `visible_*` literal `null` when unset; dates match
  `^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z$`.
- The per-type content key sets on the wire.
- Every row of the §4.5 table, including region-404-before-user_id ordering.
- The throttle: `429` after the injected limit; the list endpoint never throttled.
- The iOS fixture `surveys_missing_optional_fields.json`'s *shape* is reproduced by
  authoring an equivalent survey — the assertion is that our output decodes the same
  way the fixture does, not byte equality.
- No log line contains `user_identifier` or an answer (the alarms branch's
  token-leak test pattern).

**4 — CLI.** Temporary database through the `run` seam: study create → survey create
from a document → appears in the rider list; `show | edit --file -` round trip keeps
question ids; a document with a half-set window, a blank option, and a schemeless URL
each rejected with nothing persisted; `delete` refusal and `--force`; `responses --csv`
emits one row per answer with the header above and a response with no answers still
emits a row; `study delete` refusal.

**5 — Wiring.** `cmd/sidecar` boot test extends to assert the survey routes are
registered (the existing router panic-guard test covers the missing-deps case).
