# Alert Push Fan-out — Design

**Date:** 2026-08-25
**Status:** Reviewed
**Implements:** [specification.md](../../../specification/specification.md) §4 "What gets
pushed" (with §3 localization, §6.5 feedback, §12 loop table row 3 and the resumable-cursor
requirement, §13 retention).

## 1. Scope

The last required item on the conformance checklist: sending a service alert to the region's
registered devices as a push notification. Today `internal/pushreg` collects registrations and
`internal/alerts` authors alerts; nothing joins them. This feature adds:

- a new domain package `internal/alertpush` — the send record, its repository, the copy
  builder, and the `Dispatcher` background loop that performs the fan-out;
- an audience query on `pushreg.Repository`, keyed for resumable paging;
- a batch send on the push transport that returns gorush's synchronous rejections;
- `notif_id` correlation in the gorush feedback webhook so asynchronous bounces are
  reconciled into per-push accounting;
- an admin API (four routes), an SPA card on the alert detail page, and two
  `sidecar-admin alert` subcommands as the trigger surface.

The design is grounded in the reference implementation (`../obacloud`:
`AlertPushSendJob`, `AlertPushQueuerWorker`, `AlertPush`, `AlertPushBatch`,
`Webhooks::GorushController#record_alert_failure`, `Gorush#push`) and the iOS client
(`../ios/OBAKit/PushNotifications/PushService.swift`). Deliberate divergences are called out
inline.

### Out of scope

- **AI copywriting** (OBACloud's `Alerts::PushCopywriter`). Copy is derived mechanically from
  the alert's text (§2.4); an operator wanting different push copy edits the alert.
- **Scheduled sends** (OBACloud's `at_alert_start` / `send_at`). A push goes out when the
  operator asks for it. Scheduling is a thin addition later (one column, one predicate).
- **Delivery dashboards.** Counts are exposed on the push record; no time-series.
- **Donation prompts** and any push type other than service alerts.
- **Per-token success accounting.** Only failures are recorded per token (the transport never
  confirms end delivery, so "submitted" is the only positive state, and it is a counter).

## 2. Decisions

### 2.1 A push is a record, not a request

`POST …/alerts/{id}/pushes` inserts an `alert_pushes` row with status `queued` and returns
`202`. The `Dispatcher` (§2.6) performs the send in the background. The HTTP request never
blocks on gorush: a 100k-device audience is hundreds of gorush calls, far beyond the server's
15s `WriteTimeout`, and a send that dies with its request would violate §12's at-least-once
rule. The CLI inserts the same row; the server's loop picks it up.

Status lifecycle: `queued → sending → sent | failed | canceled`. `queued → canceled` and
`sending → canceled` are the operator's exits; `sending → failed` is the dispatcher's, after
retries are exhausted (§2.7). Terminal states never change.

### 2.2 Preconditions and audience

A push is accepted only when:

- the alert is **published** (feed and push are two views of the same authored content, §3;
  OBACloud cancels a send whose alert is a draft). `409` otherwise.
- no other push for that alert is `queued` or `sending`. `409` otherwise. A partial unique
  index (`alert_pushes_inflight_idx`, §3) makes this race-free: two concurrent sends both pass
  the read check but only one insert succeeds; the adapter maps the constraint failure to
  `alertpush.ErrInFlight`. Multiple *completed*
  pushes per alert are allowed — re-sending to test devices, then to everyone, is the normal
  verification workflow. (Divergence: OBACloud allows exactly one push per alert.)
- the audience is non-empty. `422` otherwise, so an operator learns immediately that nobody
  would receive it rather than finding a `sent` record with zero devices.

**Audience** is `all` or `test`. A **test alert** (`is_test`) can only ever go to test devices:
the server forces `test` regardless of the request, matching the feed's rule that riders never
see a test alert. A non-test alert defaults to `all`; the request may choose `test` to preview
on admin-registered devices. `test` selects `push_registrations.test_device = TRUE` in the
alert's region; `all` selects every registration in the region.

### 2.3 Audience paging on `push_registrations.id`

`pushreg.Registration` gains an `ID int64` (the table already has one), and
`pushreg.Repository` gains:

```go
// ListAudience returns up to limit registrations in regionID with id > afterID,
// ascending by id; testOnly restricts to test devices.
ListAudience(ctx, regionID int64, testOnly bool, afterID int64, limit int) ([]Registration, error)
// CountAudience is the size of the same set, split by platform.
CountAudience(ctx, regionID int64, testOnly bool) (AudienceCount, error)
```

`AudienceCount{Total, IOS, Android int64}`. Ordering by id (not `last_seen_at`) is what makes
the cursor stable: ids are monotonic, so a registration that arrives mid-send has a higher id
and is either included (if the send has not passed it) or not, but never causes a token to be
visited twice or skipped. A registration deleted mid-send (feedback webhook) is simply absent
from the next page.

Page size: 500 (`alertpush.BatchSize`), OBACloud's value.

### 2.4 Copy and localization

Copy is snapshotted into the row at creation (`messages` column, JSON
`{"en": {"title","body"}, "es": {...}}`; English is always present under `en`, which can
never collide with a translation because `alert_translations` forbids `language = 'en'`). Snapshotting means an edit
to the alert during a send does not change what riders receive mid-audience, and the record
shows what was actually sent.

`alertpush.BuildMessages(a alerts.Alert) map[string]Message` is pure:

- English `title` = `HeaderText`, `body` = `DescriptionText`. When the description is blank,
  `title` is `""` and `body` is the header — the header is what the rider must read, and an
  empty-bodied notification is invisible.
- For every language with at least one **non-stale** translation (`SourceSHA256` equals
  `alerts.SourceHash` of the current English field text — the same rule as the feed), the
  translated field is used and the other field falls back to English. Stale translations are
  withheld exactly as the feed withholds them (§3).
- Each `title` is clamped to 48 runes and each `body` to 120 runes, truncating with a trailing
  `…` (rune-aware, never mid-code-point). These are OBACloud's `TITLE_LIMIT` / `BODY_LIMIT`;
  APNs' payload cap is far above this, so the limits exist for readability on the lock screen,
  not for the transport.

At fan-out time each registration's locale is resolved with the existing
`pushreg.NormalizeLocale(reg.Locale, catalog)` where `catalog` is the message map's keys
other than `en`. `""` (no match) means English, so `Messages.For(locale)` maps `""` to `en`.
This is the consumer that package's doc comment promised.

The push carries **no `data` payload.** The iOS app displays the message as an alert when
there is no custom key (`PushService.notificationReceivedHandler`); OBACloud sends none.
Priority stays `high` — gorush's only alternative is `normal`, and the existing `Send` path
already hard-codes `high`; a service alert is time-sensitive enough that this needs no
separate knob.

### 2.5 Transport: `push.BatchSender`

```go
type Rejection struct{ Token, Reason string }
type BatchResult struct{ Rejected []Rejection }
type BatchSender interface {
    SendBatch(ctx context.Context, n Notification, notifID string) (BatchResult, error)
}
```

`Notification` is reused unchanged (it already carries `[]Tokens`). `notifID` is stamped on
the gorush request as `notif_id`; `Gorush.SendBatch` parses the response body
(`{"success","counts","logs":[{"type","platform","token","message","error"}]}`) and returns
each `logs` entry as a `Rejection`. In gorush's default async mode `logs` is empty and failures
arrive via the webhook; in `core.sync` mode inline rejections (bad token, oversized payload)
appear here and *never* hit the webhook. Both paths are reconciled (§2.8). A non-2xx or
transport error is returned as `error`, as `Send` does. The error string never includes the
body (gorush echoes tokens in error bodies — same rule as `post`).

`Send` (alarms) is untouched; `Gorush` implements both.

### 2.6 The `Dispatcher`

`alertpush.Dispatcher{Repo, Alerts, PushRegs, Sender push.BatchSender, Now, Logger}` with
`RunLoop(ctx, interval)` following the alarm scheduler's ticker shape, plus `Wake()`: a
non-blocking send on a 1-buffered channel the loop also selects on, so an admin-API send
starts within milliseconds rather than at the next tick. The ticker (every 15 seconds) is the
at-least-once safety net: it catches CLI-created rows (the CLI has no handle on the server)
and rows orphaned by a crash.

One cycle (`RunOnce(ctx)`):

1. `Repo.Claim(ctx, now, stuckBefore)` atomically moves every `queued` row, and every
   `sending` row whose `updated_at < stuckBefore` (now − 15 minutes, OBACloud's
   `STUCK_AFTER`), to `sending` with `started_at`/`updated_at = now`, returning the claimed
   rows. A single `UPDATE … RETURNING` — two overlapping cycles cannot both claim a row.
2. For each claimed push, sequentially (pushes are rare; concurrency buys nothing and would
   interleave two audiences' gorush calls): `send(ctx, push)`.

`send` loop:

```
if device_count == 0: device_count = CountAudience(...).Total; persist
loop:
  page = ListAudience(region, testOnly, batch_cursor, BatchSize)
  if empty: break
  group page by (platform, NormalizeLocale(locale, catalog), apns_sandbox)
  for each group:
    result, err = Sender.SendBatch(ctx, Notification{tokens, platform, sandbox, copy[locale]}, notifID(push.ID))
    if err: return err               // retried per §2.7; cursor not advanced
    for each rejection: Repo.RecordFailure(push.ID, token, reason)  (§2.8)
    submitted += len(group) - len(result.Rejected)
  ok = Repo.AdvanceCursor(push.ID, prevCursor=batch_cursor, newCursor=page[last].ID, submitted, now)
  if !ok: log "push no longer ours (advanced elsewhere or canceled); yielding"; return nil
MarkCompleted(sent, completed_at = now)   // conditional on status = 'sending'
```

`AdvanceCursor` is one conditional `UPDATE … WHERE id = ? AND batch_cursor = ? AND status =
'sending'` that also adds to `submitted_count` and stamps `updated_at`; it reports whether a
row matched. A `false` therefore means either "another worker advanced it" or "the operator
canceled it" — both mean stop, so no separate status read is needed between pages. The final
`sent` transition is likewise conditional on `status = 'sending'` (`MarkCompleted`), so a
cancel that lands during the last page wins. This is the §12 cursor: a crash between pages resumes at the last committed
cursor, re-sending at most one page. A group that errors mid-page re-sends the groups already
pushed in that page on retry — a bounded duplicate, preferred over losing the rest of the
audience (OBACloud's stated trade-off).

`notifID(pushID)` is `"alertpush:<id>"`. One id per push, not per batch: gorush does not require
uniqueness, and the webhook only needs to find the push (§2.8).

**No transport configured** (`Sender == nil`): the dispatcher still runs — CLI-created rows
must not sit `queued` forever — and immediately marks each claimed push `failed` with
`last_error = "no push transport configured (--gorush-url)"`. The admin API routes are only
registered when a transport exists (§2.9), so the SPA shows "not configured" rather than
letting an operator queue a push that will fail.

### 2.7 Failure handling

- **Transport error** (`SendBatch` returned `error`): `Repo.RecordAttempt(push.ID, err, now)`
  increments `attempts`, stores `last_error`, and — when `attempts >= 5` (`MaxAttempts`) —
  moves the row to `failed` with `completed_at` (via `MarkCompleted`). Otherwise the row stays `sending`; the next
  cycle sees it as stuck only after 15 minutes. That is deliberately slow: a gorush outage
  should not be hammered every 15 seconds, and OBACloud's polynomial backoff lands in the same
  range by the third attempt. The push resumes from its cursor.
- **Store error** while sending: logged; the row stays `sending` and is reclaimed as stuck.
  Store failures are not counted as attempts — they say nothing about the transport.
- **Alert deleted or unpublished** between creation and send: `Alerts.Get` returns
  `ErrNotFound` (cascade) or an unpublished alert → the push is marked `canceled` (via
  `MarkCompleted`) with `last_error` naming the cause. Copy was snapshotted, so a deleted alert's row is only
  reached via the cascade in practice; the check exists for unpublish.

### 2.8 Reconciling failures

`alert_push_failures(push_id, token, reason, created_at, UNIQUE(push_id, token))`.
`Repo.RecordFailure(ctx, pushID, token, reason, now) (bool, error)` inserts with `ON CONFLICT DO
NOTHING` and increments `failed_count` **only when a row was inserted**, so gorush replaying
feedback (it can) does not double-count. Two sources feed it:

1. synchronous `Rejection`s from `SendBatch` (§2.6);
2. the feedback webhook: when the payload carries `notif_id` with the `alertpush:` prefix and
   a token, `RecordFailure` is called for that push. The existing terminal-reason registration
   delete is unchanged and runs regardless. Feedback without a recognized `notif_id` behaves
   exactly as today. `gorushFeedback` gains a `NotifID string \`json:"notif_id"\`` field.

A token is stored in `alert_push_failures` because it is the dedup key; the table is
cascade-deleted with its push and its push with its alert, and the API never returns tokens
(§2.9 returns only counts and reasons).

`submitted_count` is *not* decremented on a webhook failure. `submitted` means "accepted by
gorush"; `failed` means "later reported undeliverable". A device can be in both; the SPA
shows both numbers, which is more honest than pretending to know end delivery.

### 2.9 Admin API

All in the `adminRoutes` table, session-required, cross-site guarded. Registered only when
`Deps.AlertPushes != nil`; `main` sets it only when gorush is configured. The handlers also
need `Deps.PushRegs` and `Deps.Alerts` (boot-time `missingDeps` panic, matching the other
blocks) and use `Deps.AlertPushWaker` (interface `{ Wake() }`, optional — nil means rely on
the ticker) after every insert.

```
POST   /api/admin/v1/alerts/{id}/pushes            {"audience":"all"|"test"}   → 202 pushJSON
GET    /api/admin/v1/alerts/{id}/pushes                                       → 200 [pushJSON] newest first
DELETE /api/admin/v1/alerts/{id}/pushes/{pushId}                              → 204 | 409 already terminal | 404
GET    /api/admin/v1/alerts/{id}/push_audience                                → 200 audienceJSON
```

`pushJSON`:

```json
{"id":7,"alert_id":3,"region_id":1,"audience":"all","status":"sending",
 "device_count":1200,"submitted_count":500,"failed_count":2,"attempts":1,
 "last_error":"","messages":{"en":{"title":"…","body":"…"},"es":{"title":"…","body":"…"}},
 "failure_reasons":[{"reason":"Unregistered","count":2}],
 "created_at":"…","started_at":null,"completed_at":null}
```

`audienceJSON`: `{"all":{"total":N,"ios":N,"android":N},"test":{…},"forced_test":bool}` —
`forced_test` is true for a test alert so the SPA can hide the audience choice. Timestamps are
RFC 3339 UTC as elsewhere. `failure_reasons` is grouped in SQL (`GROUP BY reason`), top 10 by
count.

Errors follow §2.5 of the spec (`{"error": "..."}`) with the codes in §2.2. `pushId` not
belonging to `{id}` is `404`.

### 2.10 Admin SPA

The alert detail page (`web/admin/src/routes/alerts/[id]/+page.svelte`) gains a
**Push notification** card below the publish controls:

- Loads `/push_audience` and `/pushes` in the page's `load` alongside the alert. A `404` from
  `/push_audience` (routes not registered) renders "Push notifications are not configured on
  this server" and nothing else.
- For an unpublished alert the card says "Publish the alert to send it as a push" and disables
  the button.
- Audience radio (`Everyone (N devices) / Test devices (M)`) hidden when `forced_test`; the
  Send button shows the count and asks `confirm()` (the same pattern as delete).
- History table: status badge, audience, sent/failed/of N, started, last error, and a Cancel
  button on `queued`/`sending` rows.
- While any push is `queued`/`sending` the page polls `/pushes` every 3 seconds
  (`setInterval` cleared on destroy and when nothing is in flight).

Pure logic in `web/admin/src/lib/pushes.ts` with vitest coverage: `audienceOptions(audience,
alert)`, `isInFlight(push)`, `pushProgressLabel(push)`, `sendConfirmMessage(...)`.
Types in `lib/types.ts`.

### 2.11 CLI

```
sidecar-admin alert push <id> [--audience all|test]   # inserts a queued push; prints its id
sidecar-admin alert pushes <id>                       # lists that alert's pushes with status/counts
```

`alert push` applies the same preconditions as the API (published, none in flight, non-empty
audience, test alert forces `test`) through a shared `alertpush.Request` validator so the two
surfaces cannot drift. It prints `queued push N; the sidecar server sends it within 15 seconds
of its next dispatcher tick` because the CLI never talks to the running server. Both commands
take `--db` like the rest.

### 2.12 Wiring

`cmd/sidecar/main.go`: build `alertpush.Dispatcher` after the gorush client (sender nil when
unconfigured, §2.6), `go d.RunLoop(ctx, alertPushInterval)` (15s), and set
`Deps.AlertPushes = store.AlertPushes()` + `Deps.AlertPushWaker = d` only when `gorushURL != ""`.
`Store.AlertPushes()` accessor in the sqlite adapter. The webhook block in `NewRouter` passes
`Deps.AlertPushes` through regardless of transport (a webhook can arrive after a restart that
dropped the transport flag).

## 3. Data model

Migration `00009_alert_pushes.sql`:

```sql
CREATE TABLE alert_pushes (
  id               INTEGER PRIMARY KEY AUTOINCREMENT,
  alert_id         INTEGER NOT NULL REFERENCES alerts(id) ON DELETE CASCADE,
  region_id        INTEGER NOT NULL REFERENCES regions(id) ON DELETE CASCADE,
  audience         TEXT    NOT NULL CHECK (audience IN ('all', 'test')),
  status           TEXT    NOT NULL CHECK (status IN ('queued','sending','sent','failed','canceled')),
  messages         TEXT    NOT NULL,            -- JSON, §2.4
  batch_cursor     INTEGER NOT NULL DEFAULT 0,  -- last push_registrations.id processed
  device_count     INTEGER NOT NULL DEFAULT 0,
  submitted_count  INTEGER NOT NULL DEFAULT 0,
  failed_count     INTEGER NOT NULL DEFAULT 0,
  attempts         INTEGER NOT NULL DEFAULT 0,
  last_error       TEXT    NOT NULL DEFAULT '',
  started_at       INTEGER,
  completed_at     INTEGER,
  created_at       INTEGER NOT NULL,
  updated_at       INTEGER NOT NULL
);
CREATE INDEX alert_pushes_alert_idx  ON alert_pushes (alert_id, id);
CREATE INDEX alert_pushes_status_idx ON alert_pushes (status, updated_at);
-- At most one queued/sending push per alert (§2.2).
CREATE UNIQUE INDEX alert_pushes_inflight_idx ON alert_pushes (alert_id)
  WHERE status IN ('queued', 'sending');

CREATE TABLE alert_push_failures (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  push_id    INTEGER NOT NULL REFERENCES alert_pushes(id) ON DELETE CASCADE,
  token      TEXT    NOT NULL,
  reason     TEXT    NOT NULL,
  created_at INTEGER NOT NULL,
  UNIQUE (push_id, token)
);

-- The audience query pages by id within a region (spec §12 cursor).
CREATE INDEX push_registrations_audience_idx ON push_registrations (region_id, id);
```

Every timestamp is INTEGER epoch seconds (repo invariant). Region id is denormalized from the
alert so the audience query and the webhook never need a join to `alerts`.

`alertpush.Repository`:

```go
Create(ctx, in NewPush, now) (Push, error)
Get(ctx, id) (Push, error)                       // includes FailureReasons
ListByAlert(ctx, alertID) ([]Push, error)        // newest first
InFlightForAlert(ctx, alertID) (bool, error)
Claim(ctx, now, stuckBefore time.Time) ([]Push, error)
SetDeviceCount(ctx, id, n, now) error
AdvanceCursor(ctx, id, prevCursor, newCursor int64, submitted int64, now) (bool, error)
RecordFailure(ctx, id int64, token, reason string, now) (bool, error)
RecordAttempt(ctx, id, errMsg string, now) (attempts int64, error)
MarkCompleted(ctx, id, status Status, lastError string, now) (bool, error) // sent|failed|canceled, only while sending; stamps completed_at
Cancel(ctx, id, now) error                       // queued|sending → canceled; ErrNotFound / ErrTerminal
```

Queries live in `queries/alertpushes.sql` (sqlc; every parameter `sqlc.arg`, per the
placeholder invariant). `Claim`'s `UPDATE … RETURNING` and `RecordFailure`'s two-statement
insert-then-increment run in one write transaction.

## 4. Testing

- **storetest**: `RunAlertPushRepository` (create/get round trip, in-flight detection, claim
  is exclusive and reclaims stuck rows, `AdvanceCursor` refuses a moved cursor and a
  non-sending status, `RecordFailure` dedups and counts once, cascade on alert delete) and
  `ListAudience`/`CountAudience` cases added to `RunPushRegistrationRepository` (paging by id,
  region scoping, test-only filter, platform split). Instants derive from `base`.
- **alertpush** unit tests with a fake `BatchSender` recording every call: grouping keys
  (platform × normalized locale × sandbox), English fallback for unknown locales, stale
  translations withheld, clamping, cursor advance per page, resume after a mid-send error
  re-sends only the failed page, cancel between pages stops, stuck reclaim, sync rejections
  counted, nil sender fails the push, `MaxAttempts` marks failed. Each test is mutated-to-fail
  once before commit (repo rule).
- **push**: `SendBatch` posts `notif_id`, parses `logs` into rejections, tolerates an empty or
  non-JSON body, and never embeds the body in an error.
- **httpapi**: each route in the table (auth-wrapped assertion already enumerates the table),
  preconditions → 409/422, forced test audience, cancel on terminal → 409, `pushId` from
  another alert → 404, `Wake` called after create; feedback webhook with `notif_id` records the
  failure and still deletes on terminal reasons; replay does not double count.
- **cmd/sidecar-admin**: `alert push` / `alert pushes` happy path and each precondition.
- **web/admin**: vitest for `lib/pushes.ts`.
- `make check` (including `test-tz`) green.

## 5. Documentation

README: a "Sending alerts as push notifications" subsection under *Service alerts* (admin
routes, CLI, the copy rules, the async/sync gorush note, what the counts mean), and the new
CLI subcommands in the `sidecar-admin` table. `.env.example` needs no new keys.
