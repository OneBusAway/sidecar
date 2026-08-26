# Alert Push Fan-out Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Send a published service alert as a localized push notification to a region's registered devices, resumably and with delivery accounting, triggered from the admin API, the admin SPA, and `sidecar-admin`.

**Architecture:** A new `internal/alertpush` domain package (the `Push` record, `Repository`, pure `BuildMessages`, `Enqueuer` preconditions, and a `Dispatcher` loop shaped like `alarms.Scheduler`) joins `internal/pushreg` (new audience paging) to `internal/push` (new `BatchSender` that returns gorush's inline rejections). The gorush feedback webhook learns `notif_id` correlation. Storage is one goose migration + sqlc queries + a sqlite adapter + a `storetest` conformance suite. Four admin routes, an SPA card, two CLI subcommands.

**Tech Stack:** Go 1.26 (see `mise.toml`), sqlc 1.31.1 + goose over modernc SQLite, gorush 1.22.0, SvelteKit (Svelte 5 runes) + vitest.

**Spec:** `docs/superpowers/specs/2026-08-25-alert-push-fanout-design.md` (cite in code comments as "design spec §N"). Normative behavior: `specification/specification.md` §3, §4, §6.5, §12.

## Global Constraints

- `time.Now`/`time.Local` are banned outside `cmd/` and `_test.go` (forbidigo). `storetest` is not a test file: derive instants from its fixed `base`.
- Every timestamp column is `INTEGER` epoch **seconds** (`unixToTime`, `nullUnixToTime`, `timeToNullUnix` in `store.go`).
- sqlc: never mix `sqlc.arg()`/`@name` with bare `?` in one query; never put a parameter on the right-hand side of `ON CONFLICT … DO UPDATE SET`. Run `make generate` after touching `.sql` and commit `gen/`.
- `revive` requires doc comments on every exported identifier and package. `nolint` needs a linter name and reason.
- Nil `Deps` field ⇒ routes not registered. Errors that can embed a token are logged only via `sanitizeToken`. Never log tokens or gorush response bodies.
- Every test must be shown to fail under mutation (run the test, break the code, watch the assertion fire, restore). Timestamp assertions must pass under `make test-tz`.
- Constants (design spec): `BatchSize = 500`, `TitleLimit = 48`, `BodyLimit = 120`, `MaxAttempts = 5`, `StuckAfter = 15 * time.Minute`, dispatcher tick `15 * time.Second`, `NotifIDPrefix = "alertpush:"`, English message key `"en"`.
- Run `make check` before finishing (needs `make web` once for the adminui embed test). Commit after every task.

## File map

| File | Responsibility |
|---|---|
| `internal/push/push.go`, `gorush.go` | `Rejection`, `BatchResult`, `BatchSender`, `(*Gorush).SendBatch` (Task 1) |
| `internal/store/sqlite/migrations/00009_alert_pushes.sql` | tables + indexes (Task 2) |
| `internal/pushreg/pushreg.go`, `queries/pushregs.sql`, `sqlite/pushregs.go`, `storetest/pushregtest.go` | `Registration.ID`, `AudienceCount`, `ListAudience`, `CountAudience` (Task 2) |
| `internal/alertpush/alertpush.go` | package doc, `Status`, `Audience`, `Message`, `Messages`, `Push`, `NewPush`, `FailureReason`, `Repository`, errors, constants, `NotifID`/`ParseNotifID` (Task 3) |
| `internal/alertpush/messages.go` | `BuildMessages`, `Clamp` (Task 3) |
| `internal/store/sqlite/queries/alertpushes.sql`, `sqlite/alertpushes.go`, `sqlite/store.go`, `storetest/alertpushtest.go` | storage + conformance (Task 4) |
| `internal/alertpush/enqueue.go` | `Enqueuer` preconditions shared by API and CLI (Task 5) |
| `internal/alertpush/dispatcher.go` | `Dispatcher`, `Waker` (Task 6) |
| `internal/httpapi/admin_pushes.go`, `router.go`, `feedback.go` | admin routes, `Deps` fields, webhook `notif_id` (Task 7) |
| `cmd/sidecar/main.go` | dispatcher wiring (Task 8) |
| `cmd/sidecar-admin/commands.go` | `alert push`, `alert pushes` (Task 9) |
| `web/admin/src/lib/types.ts`, `lib/pushes.ts`, `routes/alerts/[id]/+page.ts`, `+page.svelte` | SPA card (Task 10) |
| `README.md`, `CLAUDE.md` | docs (Task 11) |

---

### Task 1: `push.BatchSender` and `Gorush.SendBatch`

**Files:**
- Modify: `internal/push/push.go` (after the `Sender` interface)
- Modify: `internal/push/gorush.go` (`gorushNotification` gets `NotifID`; new `SendBatch`; `post` learns to return the body)
- Test: `internal/push/gorush_test.go`

**Interfaces:**
- Produces:
  ```go
  type Rejection struct{ Token, Reason string }
  type BatchResult struct{ Rejected []Rejection }
  type BatchSender interface {
      SendBatch(ctx context.Context, n Notification, notifID string) (BatchResult, error)
  }
  func (g *Gorush) SendBatch(ctx context.Context, n Notification, notifID string) (BatchResult, error)
  ```

- [ ] **Step 1: Write the failing tests**

Append to `internal/push/gorush_test.go`:

```go
func TestGorushSendBatchPostsNotifIDAndParsesRejections(t *testing.T) {
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
			t.Errorf("unmarshal: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		captured = req["notifications"].([]any)[0].(map[string]any)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":"ok","counts":3,"logs":[
			{"type":"succeeded-push","platform":"ios","token":"tok-a","message":"Hi","error":""},
			{"type":"failed-push","platform":"ios","token":"tok-b","message":"Hi","error":"BadDeviceToken"},
			{"type":"failed-push","platform":"ios","token":"","message":"Hi","error":"NoToken"},
			{"type":"failed-push","platform":"ios","token":"tok-c","message":"Hi","error":"PayloadTooLarge"}]}`))
	}))
	defer server.Close()

	g := NewGorush(server.URL, "org.example.app", server.Client())
	res, err := g.SendBatch(context.Background(), Notification{
		Tokens: []string{"tok-a", "tok-b", "tok-c"}, Platform: PlatformIOS, Sandbox: true,
		Title: "Route 44", Message: "Detour this weekend",
	}, "alertpush:7")
	if err != nil {
		t.Fatalf("SendBatch: %v", err)
	}
	if got := captured["notif_id"]; got != "alertpush:7" {
		t.Errorf("notif_id = %v, want alertpush:7", got)
	}
	if got := captured["development"]; got != true {
		t.Errorf("development = %v, want true", got)
	}
	if got := captured["topic"]; got != "org.example.app" {
		t.Errorf("topic = %v, want org.example.app", got)
	}
	if _, present := captured["data"]; present {
		t.Errorf("data present in alert push; want none")
	}
	// Only failed-push entries with a token: a succeeded-push row (gorush can
	// log those too) or a token-less row must never become a rejection.
	want := []Rejection{{Token: "tok-b", Reason: "BadDeviceToken"}, {Token: "tok-c", Reason: "PayloadTooLarge"}}
	if !reflect.DeepEqual(res.Rejected, want) {
		t.Errorf("Rejected = %+v, want %+v", res.Rejected, want)
	}
}

func TestGorushSendBatchToleratesEmptyAndNonJSONBodies(t *testing.T) {
	for _, body := range []string{"", "not json", `{"success":"ok"}`} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(body))
		}))
		g := NewGorush(server.URL, "", server.Client())
		res, err := g.SendBatch(context.Background(), Notification{Tokens: []string{"t"}, Platform: PlatformAndroid, Message: "m"}, "x")
		server.Close()
		if err != nil {
			t.Errorf("body %q: SendBatch error = %v, want nil (async mode returns no logs)", body, err)
		}
		if len(res.Rejected) != 0 {
			t.Errorf("body %q: Rejected = %v, want empty", body, res.Rejected)
		}
	}
}

func TestGorushSendBatchErrorNeverEchoesBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"bad","tokens":["SECRET-TOKEN"]}`))
	}))
	defer server.Close()
	g := NewGorush(server.URL, "", server.Client())
	_, err := g.SendBatch(context.Background(), Notification{Tokens: []string{"SECRET-TOKEN"}, Platform: PlatformAndroid, Message: "m"}, "x")
	if err == nil {
		t.Fatal("SendBatch error = nil, want non-nil for 400")
	}
	if strings.Contains(err.Error(), "SECRET-TOKEN") {
		t.Errorf("error %q echoes the response body", err)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/push -run 'TestGorushSendBatch' -v`
Expected: compile error — `g.SendBatch undefined`.

- [ ] **Step 3: Implement**

In `internal/push/push.go`, after `Sender`:

```go
// Rejection is one token gorush refused synchronously (design spec §2.5):
// in core.sync mode gorush reports inline failures such as BadDeviceToken
// or an oversized payload in the response "logs", and those tokens never
// reach the feedback webhook.
type Rejection struct {
	Token  string
	Reason string
}

// BatchResult is what the transport reported inline for one batch. An empty
// Rejected is the normal case in gorush's default async mode, where every
// failure arrives later via the webhook (§6.5).
type BatchResult struct {
	Rejected []Rejection
}

// BatchSender delivers one notification to many tokens and returns the
// transport's synchronous rejections. notifID is stamped on the request so
// asynchronous feedback can be correlated back to the send (design spec
// §2.8). A nil error means the transport accepted the batch, not that any
// device received it.
type BatchSender interface {
	SendBatch(ctx context.Context, n Notification, notifID string) (BatchResult, error)
}
```

In `internal/push/gorush.go`:

1. Add to `gorushNotification`: `NotifID string \`json:"notif_id,omitempty"\`` (with a comment: gorush echoes it in the response logs and in feedback webhook payloads, which is the only way a batched failure finds its push).
2. Add the response shape and `SendBatch`:

```go
// gorushResponse is the subset of gorush's /api/push response this sidecar
// reads. Logs is populated only in core.sync mode, and may carry
// succeeded-push entries as well as failed-push ones.
type gorushResponse struct {
	Logs []struct {
		Type  string `json:"type"`
		Token string `json:"token"`
		Error string `json:"error"`
	} `json:"logs"`
}

// gorushFailedPush is the log entry type gorush uses for a rejected token.
const gorushFailedPush = "failed-push"

// SendBatch posts n to gorush with notifID stamped on the request and
// returns every inline rejection from the response logs (design spec §2.5).
// An empty, non-JSON, or log-less body is the async-mode normal case and
// yields no rejections, not an error.
func (g *Gorush) SendBatch(ctx context.Context, n Notification, notifID string) (BatchResult, error) {
	gn := gorushNotification{
		Tokens:   n.Tokens,
		Platform: int(n.Platform),
		Title:    n.Title,
		Message:  n.Message,
		Priority: "high",
		Data:     n.Data,
		NotifID:  notifID,
	}
	if n.Platform == PlatformIOS {
		gn.Development = n.Sandbox
		gn.Topic = g.apnsTopic
	}
	body, err := g.post(ctx, map[string]any{"notifications": []gorushNotification{gn}})
	if err != nil {
		return BatchResult{}, err
	}
	var resp gorushResponse
	if json.Unmarshal(body, &resp) != nil {
		return BatchResult{}, nil
	}
	var res BatchResult
	for _, l := range resp.Logs {
		if l.Type != gorushFailedPush || l.Token == "" {
			continue
		}
		res.Rejected = append(res.Rejected, Rejection{Token: l.Token, Reason: l.Error})
	}
	return res, nil
}
```

3. Change `post` to return `([]byte, error)`: read the body with `io.ReadAll(io.LimitReader(resp.Body, 1<<20))` instead of discarding it (still never included in errors); update `Send` and `SendLiveActivity` to `_, err := g.post(...)`.

- [ ] **Step 4: Run tests; mutate; restore**

Run: `go test ./internal/push -v`
Expected: PASS. Mutations: (a) drop `NotifID: notifID` — the first test must fail on `notif_id`; (b) drop the `l.Type != gorushFailedPush` filter — it must fail on `Rejected`. Restore both.

- [ ] **Step 5: Commit**

```bash
git add internal/push && git commit -m "push: BatchSender with gorush inline rejection parsing"
```

---

### Task 2: Migration + audience paging on `pushreg.Repository`

**Files:**
- Create: `internal/store/sqlite/migrations/00009_alert_pushes.sql`
- Modify: `internal/pushreg/pushreg.go` (`Registration.ID`, `AudienceCount`, two interface methods)
- Modify: `internal/store/sqlite/queries/pushregs.sql`, `internal/store/sqlite/pushregs.go`
- Modify: `internal/store/storetest/pushregtest.go`
- Modify: `internal/httpapi/feedback_test.go` (`fakePushRepo` must satisfy the wider interface)
- Regenerate: `internal/store/sqlite/gen/` (`make generate`)

**Interfaces:**
- Produces:
  ```go
  type Registration struct { ID int64; /* existing fields */ }
  type AudienceCount struct{ Total, IOS, Android int64 }
  // on Repository:
  ListAudience(ctx context.Context, regionID int64, testOnly bool, afterID int64, limit int) ([]Registration, error)
  CountAudience(ctx context.Context, regionID int64, testOnly bool) (AudienceCount, error)
  ```

- [ ] **Step 1: Migration**

```sql
-- +goose Up
-- Alert push fan-out (design spec §3). One row per send of one alert.
CREATE TABLE alert_pushes (
  id               INTEGER PRIMARY KEY AUTOINCREMENT,
  alert_id         INTEGER NOT NULL REFERENCES alerts(id) ON DELETE CASCADE,
  region_id        INTEGER NOT NULL REFERENCES regions(id) ON DELETE CASCADE,
  audience         TEXT    NOT NULL CHECK (audience IN ('all', 'test')),
  status           TEXT    NOT NULL CHECK (status IN ('queued', 'sending', 'sent', 'failed', 'canceled')),
  messages         TEXT    NOT NULL,
  batch_cursor     INTEGER NOT NULL DEFAULT 0,
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
-- At most one queued/sending push per alert (design spec §2.2).
CREATE UNIQUE INDEX alert_pushes_inflight_idx ON alert_pushes (alert_id)
  WHERE status IN ('queued', 'sending');

-- Per-token failure accounting; (push_id, token_sha256) dedups replayed
-- feedback (design spec §2.8). Only a hash is stored: nothing reads the
-- token back, and a plaintext copy would outlive the registration's
-- retention (spec §13).
CREATE TABLE alert_push_failures (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  push_id      INTEGER NOT NULL REFERENCES alert_pushes(id) ON DELETE CASCADE,
  token_sha256 TEXT    NOT NULL,
  reason       TEXT    NOT NULL,
  created_at   INTEGER NOT NULL,
  UNIQUE (push_id, token_sha256)
);

-- The audience query pages by id within a region (spec §12 cursor).
CREATE INDEX push_registrations_audience_idx ON push_registrations (region_id, id);

-- +goose Down
DROP INDEX push_registrations_audience_idx;
DROP TABLE alert_push_failures;
DROP TABLE alert_pushes;
```

- [ ] **Step 2: Write the failing conformance tests**

In `internal/store/storetest/pushregtest.go`, register two new subtests in `RunPushRegistrationRepository`:

```go
	t.Run("ListAudiencePagesByID", func(t *testing.T) { testListAudiencePagesByID(t, newStore) })
	t.Run("CountAudienceSplitsByPlatform", func(t *testing.T) { testCountAudienceSplitsByPlatform(t, newStore) })
```

and add:

```go
// testListAudiencePagesByID pins the cursor contract the alert push
// dispatcher resumes on (design spec §2.3): ascending id, strictly greater
// than afterID, region-scoped, test-only filter, and a limit.
func testListAudiencePagesByID(t *testing.T, newStore newPushRegStoreFunc) {
	repo, regionsRepo := newStore(t)
	ctx := context.Background()
	putStoretestRegion(t, regionsRepo, 1)
	putStoretestRegion(t, regionsRepo, 2)

	// Five in region 1 (the third is a test device), one in region 2.
	for i, tok := range []string{"a", "b", "c", "d", "e"} {
		up := pushreg.Upsert{RegionID: 1, Token: tok, OperatingSystem: pushreg.OSIOS}
		if i == 2 {
			up.TestDevice, up.Description = ptr(true), ptr("QA phone")
		}
		if err := repo.Upsert(ctx, up, base); err != nil {
			t.Fatalf("Upsert(%s): %v", tok, err)
		}
	}
	if err := repo.Upsert(ctx, pushreg.Upsert{RegionID: 2, Token: "z", OperatingSystem: pushreg.OSAndroid}, base); err != nil {
		t.Fatalf("Upsert(z): %v", err)
	}

	page1, err := repo.ListAudience(ctx, 1, false, 0, 2)
	if err != nil {
		t.Fatalf("ListAudience page 1: %v", err)
	}
	if got := tokensOf(page1); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("page 1 tokens = %v, want [a b]", got)
	}
	if page1[0].ID <= 0 || page1[1].ID <= page1[0].ID {
		t.Fatalf("ids not ascending/positive: %d, %d", page1[0].ID, page1[1].ID)
	}
	page2, err := repo.ListAudience(ctx, 1, false, page1[1].ID, 2)
	if err != nil {
		t.Fatalf("ListAudience page 2: %v", err)
	}
	if got := tokensOf(page2); !reflect.DeepEqual(got, []string{"c", "d"}) {
		t.Errorf("page 2 tokens = %v, want [c d]", got)
	}
	page3, err := repo.ListAudience(ctx, 1, false, page2[1].ID, 2)
	if err != nil {
		t.Fatalf("ListAudience page 3: %v", err)
	}
	if got := tokensOf(page3); !reflect.DeepEqual(got, []string{"e"}) {
		t.Errorf("page 3 tokens = %v, want [e] (region 2's row must not leak)", got)
	}
	page4, err := repo.ListAudience(ctx, 1, false, page3[0].ID, 2)
	if err != nil {
		t.Fatalf("ListAudience page 4: %v", err)
	}
	if len(page4) != 0 {
		t.Errorf("page 4 = %v, want empty", tokensOf(page4))
	}

	testOnly, err := repo.ListAudience(ctx, 1, true, 0, 10)
	if err != nil {
		t.Fatalf("ListAudience test-only: %v", err)
	}
	if got := tokensOf(testOnly); !reflect.DeepEqual(got, []string{"c"}) {
		t.Errorf("test-only tokens = %v, want [c]", got)
	}
}

func tokensOf(regs []pushreg.Registration) []string {
	out := make([]string, 0, len(regs))
	for _, r := range regs {
		out = append(out, r.Token)
	}
	return out
}

func testCountAudienceSplitsByPlatform(t *testing.T, newStore newPushRegStoreFunc) {
	repo, regionsRepo := newStore(t)
	ctx := context.Background()
	putStoretestRegion(t, regionsRepo, 1)
	putStoretestRegion(t, regionsRepo, 2)
	seed := []pushreg.Upsert{
		{RegionID: 1, Token: "i1", OperatingSystem: pushreg.OSIOS},
		{RegionID: 1, Token: "i2", OperatingSystem: pushreg.OSIOS, TestDevice: ptr(true), Description: ptr("QA")},
		{RegionID: 1, Token: "a1", OperatingSystem: pushreg.OSAndroid},
		{RegionID: 2, Token: "other", OperatingSystem: pushreg.OSAndroid},
	}
	for _, up := range seed {
		if err := repo.Upsert(ctx, up, base); err != nil {
			t.Fatalf("Upsert(%s): %v", up.Token, err)
		}
	}
	all, err := repo.CountAudience(ctx, 1, false)
	if err != nil {
		t.Fatalf("CountAudience(all): %v", err)
	}
	if want := (pushreg.AudienceCount{Total: 3, IOS: 2, Android: 1}); all != want {
		t.Errorf("all = %+v, want %+v", all, want)
	}
	test, err := repo.CountAudience(ctx, 1, true)
	if err != nil {
		t.Fatalf("CountAudience(test): %v", err)
	}
	if want := (pushreg.AudienceCount{Total: 1, IOS: 1, Android: 0}); test != want {
		t.Errorf("test = %+v, want %+v", test, want)
	}
}
```

(Add `"reflect"` to the imports.)

- [ ] **Step 3: Run to verify failure**

Run: `go test ./internal/store/sqlite -run 'TestPushRegistrationRepository' -v`
Expected: compile error — `repo.ListAudience undefined`.

- [ ] **Step 4: Implement**

`internal/pushreg/pushreg.go`: add `ID int64 // row id; the alert push cursor pages on it (design spec §2.3)` as the first field of `Registration`; add

```go
// AudienceCount is the size of one push audience split by platform
// (design spec §2.3); Total = IOS + Android.
type AudienceCount struct {
	Total   int64
	IOS     int64
	Android int64
}
```

and the two interface methods with the doc comments from the spec (§2.3).

`queries/pushregs.sql` — every parameter is `sqlc.arg`; the test predicate uses the same `(test_device = TRUE OR NOT :test_only)` shape as `FeedAlerts`:

```sql
-- name: ListPushAudience :many
-- Pages a region's registrations by id (design spec §2.3). The predicate
-- (test_device = TRUE OR NOT test_only) selects everyone when test_only is
-- false and only test devices when it is true.
SELECT * FROM push_registrations
WHERE region_id = sqlc.arg(region_id)
  AND id > sqlc.arg(after_id)
  AND (test_device = TRUE OR NOT CAST(sqlc.arg(test_only) AS BOOLEAN))
ORDER BY id
LIMIT sqlc.arg(limit);

-- name: CountPushAudience :many
SELECT operating_system, COUNT(*) AS n FROM push_registrations
WHERE region_id = sqlc.arg(region_id)
  AND (test_device = TRUE OR NOT CAST(sqlc.arg(test_only) AS BOOLEAN))
GROUP BY operating_system;
```

`sqlite/pushregs.go`: set `ID: row.ID` in `pushRegistrationFromRow`; implement

```go
func (r *pushRegRepo) ListAudience(ctx context.Context, regionID int64, testOnly bool, afterID int64, limit int) ([]pushreg.Registration, error) {
	rows, err := r.q.ListPushAudience(ctx, gen.ListPushAudienceParams{
		RegionID: regionID, AfterID: afterID, TestOnly: testOnly, Limit: int64(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("sqlite: list push audience (region %d): %w", regionID, err)
	}
	out := make([]pushreg.Registration, 0, len(rows))
	for _, row := range rows {
		out = append(out, pushRegistrationFromRow(row))
	}
	return out, nil
}

func (r *pushRegRepo) CountAudience(ctx context.Context, regionID int64, testOnly bool) (pushreg.AudienceCount, error) {
	rows, err := r.q.CountPushAudience(ctx, gen.CountPushAudienceParams{RegionID: regionID, TestOnly: testOnly})
	if err != nil {
		return pushreg.AudienceCount{}, fmt.Errorf("sqlite: count push audience (region %d): %w", regionID, err)
	}
	var c pushreg.AudienceCount
	for _, row := range rows {
		switch row.OperatingSystem {
		case pushreg.OSIOS:
			c.IOS = row.N
		case pushreg.OSAndroid:
			c.Android = row.N
		}
		c.Total += row.N
	}
	return c, nil
}
```

(If sqlc names the generated fields differently — e.g. `TestOnly` typed `interface{}` because of the CAST — adjust the Go to what `gen/` actually emits; check `gen/pushregs.sql.go` after `make generate`. If the CAST produces `interface{}`, drop the CAST and write `AND (test_device = TRUE OR sqlc.arg(test_only) = FALSE)` instead.)

`internal/httpapi/feedback_test.go`: add pass-through `ListAudience`/`CountAudience` methods to `fakePushRepo` delegating to `f.real`.

- [ ] **Step 5: Generate, run, mutate, restore**

Run: `make generate && go test ./internal/store/... ./internal/httpapi ./internal/pushreg`
Expected: PASS. Mutation: change `id > ` to `id >= ` in the query — `ListAudiencePagesByID` must fail on page 2. Restore, `make generate`.

- [ ] **Step 6: Commit**

```bash
git add internal/store internal/pushreg internal/httpapi/feedback_test.go && git commit -m "pushreg: audience paging and counts; alert_pushes migration"
```

---

### Task 3: `internal/alertpush` domain types and `BuildMessages`

**Files:**
- Create: `internal/alertpush/alertpush.go`, `internal/alertpush/messages.go`
- Test: `internal/alertpush/messages_test.go`, `internal/alertpush/alertpush_test.go`

**Interfaces:**
- Produces (exact):

```go
package alertpush

const (
	BatchSize   = 500
	TitleLimit  = 48
	BodyLimit   = 120
	MaxAttempts = 5
	StuckAfter  = 15 * time.Minute
	EnglishKey  = "en"
	NotifIDPrefix = "alertpush:"
)

type Status string
const (
	StatusQueued Status = "queued"; StatusSending Status = "sending"; StatusSent Status = "sent"
	StatusFailed Status = "failed"; StatusCanceled Status = "canceled"
)
func (s Status) Terminal() bool   // sent | failed | canceled

type Audience string
const ( AudienceAll Audience = "all"; AudienceTest Audience = "test" )
func ParseAudience(s string) (Audience, error)  // "" → AudienceAll; unknown → error

type Message struct { Title string `json:"title"`; Body string `json:"body"` }
type Messages map[string]Message
func (m Messages) For(locale string) Message   // "" or unknown → m[EnglishKey]
func (m Messages) Catalog() []string           // sorted keys other than "en"

type FailureReason struct { Reason string; Count int64 }

type Push struct {
	ID, AlertID, RegionID int64
	Audience Audience
	Status Status
	Messages Messages
	BatchCursor, DeviceCount, SubmittedCount, FailedCount, Attempts int64
	LastError string
	StartedAt, CompletedAt *time.Time
	CreatedAt, UpdatedAt time.Time
	FailureReasons []FailureReason // populated by Get only
}

type NewPush struct { AlertID, RegionID int64; Audience Audience; Messages Messages }

var ( ErrNotFound; ErrInFlight; ErrTerminal; ErrNotPublished; ErrEmptyAudience error )

type Repository interface {
	Create(ctx context.Context, in NewPush, now time.Time) (Push, error)        // ErrInFlight on the partial unique index
	Get(ctx context.Context, id int64) (Push, error)                             // with FailureReasons
	ListByAlert(ctx context.Context, alertID int64) ([]Push, error)              // newest first, FailureReasons attached
	InFlightForAlert(ctx context.Context, alertID int64) (bool, error)
	Claim(ctx context.Context, now, stuckBefore time.Time) ([]Push, error)       // ascending id
	SetDeviceCount(ctx context.Context, id, n int64, now time.Time) error
	AdvanceCursor(ctx context.Context, id, prevCursor, newCursor, submitted int64, now time.Time) (bool, error)
	RecordFailure(ctx context.Context, id int64, token, reason string, now time.Time) (bool, error)
	RecordAttempt(ctx context.Context, id int64, errMsg string, now time.Time) (int64, error)
	MarkCompleted(ctx context.Context, id int64, status Status, lastError string, now time.Time) (bool, error)
	Cancel(ctx context.Context, id int64, now time.Time) error
}

func NotifID(pushID int64) string
func ParseNotifID(s string) (int64, bool)
func BuildMessages(a alerts.Alert) Messages
func Clamp(s string, limit int) string
```

- [ ] **Step 1: Write the failing tests**

`internal/alertpush/messages_test.go`:

```go
package alertpush_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/OneBusAway/sidecar/internal/alertpush"
	"github.com/OneBusAway/sidecar/internal/alerts"
)

func TestBuildMessagesEnglishAndFreshTranslations(t *testing.T) {
	a := alerts.Alert{
		HeaderText:      "Route 44 detour",
		DescriptionText: "Buses skip 3rd Ave this weekend.",
		Translations: []alerts.Translation{
			{Language: "es", Field: alerts.FieldHeader, Text: "Desvío ruta 44", SourceSHA256: alerts.SourceHash("Route 44 detour")},
			// Stale: hash of an older English description.
			{Language: "es", Field: alerts.FieldDescription, Text: "VIEJO", SourceSHA256: alerts.SourceHash("old text")},
			// Every field stale: language must not appear at all.
			{Language: "fr", Field: alerts.FieldHeader, Text: "VIEUX", SourceSHA256: alerts.SourceHash("old")},
		},
	}
	got := alertpush.BuildMessages(a)
	want := alertpush.Messages{
		"en": {Title: "Route 44 detour", Body: "Buses skip 3rd Ave this weekend."},
		"es": {Title: "Desvío ruta 44", Body: "Buses skip 3rd Ave this weekend."},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("BuildMessages = %+v, want %+v", got, want)
	}
	if cat := got.Catalog(); !reflect.DeepEqual(cat, []string{"es"}) {
		t.Errorf("Catalog = %v, want [es]", cat)
	}
	if m := got.For(""); m != want["en"] {
		t.Errorf("For(\"\") = %+v, want English", m)
	}
	if m := got.For("de"); m != want["en"] {
		t.Errorf("For(de) = %+v, want English fallback", m)
	}
	if m := got.For("es"); m != want["es"] {
		t.Errorf("For(es) = %+v, want Spanish", m)
	}
}

func TestBuildMessagesBlankDescriptionPromotesHeaderToBody(t *testing.T) {
	got := alertpush.BuildMessages(alerts.Alert{HeaderText: "Elevator out at Westlake"})
	if want := (alertpush.Message{Title: "", Body: "Elevator out at Westlake"}); got["en"] != want {
		t.Errorf("en = %+v, want %+v", got["en"], want)
	}
}

func TestBuildMessagesClamps(t *testing.T) {
	long := strings.Repeat("é", 200) // multi-byte: a byte-based clamp would split a rune
	got := alertpush.BuildMessages(alerts.Alert{HeaderText: long, DescriptionText: long})
	if n := len([]rune(got["en"].Title)); n != alertpush.TitleLimit {
		t.Errorf("title runes = %d, want %d", n, alertpush.TitleLimit)
	}
	if !strings.HasSuffix(got["en"].Title, "…") {
		t.Errorf("title %q lacks ellipsis", got["en"].Title)
	}
	if n := len([]rune(got["en"].Body)); n != alertpush.BodyLimit {
		t.Errorf("body runes = %d, want %d", n, alertpush.BodyLimit)
	}
	if s := alertpush.Clamp("short", 48); s != "short" {
		t.Errorf("Clamp(short) = %q", s)
	}
	if s := alertpush.Clamp(strings.Repeat("x", 48), 48); s != strings.Repeat("x", 48) {
		t.Errorf("Clamp at exactly the limit must not truncate: %q", s)
	}
}
```

`internal/alertpush/alertpush_test.go`:

```go
package alertpush_test

import (
	"testing"

	"github.com/OneBusAway/sidecar/internal/alertpush"
)

func TestNotifIDRoundTrip(t *testing.T) {
	if s := alertpush.NotifID(42); s != "alertpush:42" {
		t.Errorf("NotifID(42) = %q", s)
	}
	id, ok := alertpush.ParseNotifID("alertpush:42")
	if !ok || id != 42 {
		t.Errorf("ParseNotifID = %d, %v; want 42, true", id, ok)
	}
	for _, bad := range []string{"", "alertpush:", "alertpush:x", "alarm:42", "42"} {
		if _, ok := alertpush.ParseNotifID(bad); ok {
			t.Errorf("ParseNotifID(%q) ok = true, want false", bad)
		}
	}
}

func TestParseAudience(t *testing.T) {
	cases := map[string]alertpush.Audience{"": alertpush.AudienceAll, "all": alertpush.AudienceAll, "test": alertpush.AudienceTest}
	for in, want := range cases {
		got, err := alertpush.ParseAudience(in)
		if err != nil || got != want {
			t.Errorf("ParseAudience(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := alertpush.ParseAudience("everyone"); err == nil {
		t.Error("ParseAudience(everyone) error = nil, want error")
	}
}

func TestStatusTerminal(t *testing.T) {
	for s, want := range map[alertpush.Status]bool{
		alertpush.StatusQueued: false, alertpush.StatusSending: false,
		alertpush.StatusSent: true, alertpush.StatusFailed: true, alertpush.StatusCanceled: true,
	} {
		if got := s.Terminal(); got != want {
			t.Errorf("%s.Terminal() = %v, want %v", s, got, want)
		}
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/alertpush/...`
Expected: package does not exist / undefined symbols.

- [ ] **Step 3: Implement**

`internal/alertpush/alertpush.go`:

```go
// Package alertpush joins the alert catalog to the push registry: one Push
// row per send of one alert, the Repository that persists it, the pure copy
// builder (messages.go), the Enqueuer that applies the send preconditions
// (enqueue.go), and the Dispatcher that performs the fan-out (dispatcher.go).
// Spec §4 "What gets pushed", §12 loop table row 3; design spec
// docs/superpowers/specs/2026-08-25-alert-push-fanout-design.md.
//
// Nothing here reads the clock; every method takes now.
package alertpush

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Fan-out constants (design spec §2.3, §2.4, §2.6, §2.7).
const (
	// BatchSize is the audience page size and the gorush batch ceiling.
	BatchSize = 500
	// TitleLimit and BodyLimit clamp push copy in runes.
	TitleLimit = 48
	BodyLimit  = 120
	// MaxAttempts is how many transport failures a push survives.
	MaxAttempts = 5
	// StuckAfter is how long a sending row may go untouched before the
	// dispatcher reclaims it (a crashed worker's work re-runs, spec §12).
	StuckAfter = 15 * time.Minute
	// EnglishKey is the Messages key for the alert's own text.
	EnglishKey = "en"
	// NotifIDPrefix prefixes the gorush notif_id stamped on every batch so
	// feedback can find its push (design spec §2.8).
	NotifIDPrefix = "alertpush:"
)

// Status is a push's lifecycle state: queued → sending → sent|failed|canceled.
type Status string

// The five statuses (design spec §2.1).
const (
	StatusQueued   Status = "queued"
	StatusSending  Status = "sending"
	StatusSent     Status = "sent"
	StatusFailed   Status = "failed"
	StatusCanceled Status = "canceled"
)

// Terminal reports whether s never changes again.
func (s Status) Terminal() bool {
	return s == StatusSent || s == StatusFailed || s == StatusCanceled
}

// Audience selects who receives a push (design spec §2.2).
type Audience string

// AudienceAll is every registration in the region; AudienceTest only
// admin-marked test devices.
const (
	AudienceAll  Audience = "all"
	AudienceTest Audience = "test"
)

// ParseAudience maps a request value onto an Audience; empty means all.
func ParseAudience(s string) (Audience, error) {
	switch strings.TrimSpace(s) {
	case "", string(AudienceAll):
		return AudienceAll, nil
	case string(AudienceTest):
		return AudienceTest, nil
	}
	return "", fmt.Errorf("audience must be %q or %q", AudienceAll, AudienceTest)
}

// Message is one language's push copy.
type Message struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

// Messages is the per-language copy snapshot keyed by catalog language;
// English is always present under EnglishKey.
type Messages map[string]Message

// For returns the copy for a normalized locale, falling back to English for
// "" (pushreg.NormalizeLocale's no-match value) or an unknown key.
func (m Messages) For(locale string) Message {
	if msg, ok := m[locale]; ok && locale != "" {
		return msg
	}
	return m[EnglishKey]
}

// Catalog lists the translated languages (every key but English), sorted,
// in the form pushreg.NormalizeLocale expects.
func (m Messages) Catalog() []string {
	out := make([]string, 0, len(m))
	for k := range m {
		if k != EnglishKey {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// FailureReason is one grouped row of a push's failure accounting.
type FailureReason struct {
	Reason string
	Count  int64
}

// Push is one send of one alert, as stored (design spec §3).
type Push struct {
	ID             int64
	AlertID        int64
	RegionID       int64
	Audience       Audience
	Status         Status
	Messages       Messages
	BatchCursor    int64 // last push_registrations.id processed
	DeviceCount    int64
	SubmittedCount int64
	FailedCount    int64
	Attempts       int64
	LastError      string
	StartedAt      *time.Time
	CompletedAt    *time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
	// FailureReasons is attached by Repository.Get and ListByAlert.
	FailureReasons []FailureReason
}

// NewPush is the input to Repository.Create. Audience and Messages are
// already resolved by the Enqueuer.
type NewPush struct {
	AlertID  int64
	RegionID int64
	Audience Audience
	Messages Messages
}

// Sentinel errors. The HTTP layer maps them to 404/409/409/409/422.
var (
	ErrNotFound      = errors.New("alert push not found")
	ErrInFlight      = errors.New("a push for this alert is already queued or sending")
	ErrTerminal      = errors.New("alert push already completed")
	ErrNotPublished  = errors.New("alert is not published")
	ErrEmptyAudience = errors.New("no registered devices in the audience")
)

// Repository stores alert pushes. Implementations must be safe for
// concurrent use: the dispatcher, the admin API, and the feedback webhook
// write the same rows.
type Repository interface {
	// Create inserts a queued push; ErrInFlight if one is already queued or
	// sending for the alert (partial unique index, design spec §2.2).
	Create(ctx context.Context, in NewPush, now time.Time) (Push, error)
	// Get returns one push with FailureReasons attached; ErrNotFound if absent.
	Get(ctx context.Context, id int64) (Push, error)
	// ListByAlert returns an alert's pushes newest first, FailureReasons attached.
	ListByAlert(ctx context.Context, alertID int64) ([]Push, error)
	// InFlightForAlert reports whether a queued or sending push exists.
	InFlightForAlert(ctx context.Context, alertID int64) (bool, error)
	// Claim atomically moves every queued push, and every sending push whose
	// updated_at is before stuckBefore, to sending (stamping started_at if
	// unset and updated_at = now) and returns them ascending by id.
	Claim(ctx context.Context, now, stuckBefore time.Time) ([]Push, error)
	// SetDeviceCount records the audience size at send start.
	SetDeviceCount(ctx context.Context, id, n int64, now time.Time) error
	// AdvanceCursor moves batch_cursor from prevCursor to newCursor, adds
	// submitted to submitted_count, and resets attempts/last_error (a page
	// committed, so MaxAttempts counts consecutive failures) -- only while
	// status is sending and the cursor still equals prevCursor. False means
	// another worker advanced it or the operator canceled: stop (design
	// spec §2.6).
	AdvanceCursor(ctx context.Context, id, prevCursor, newCursor, submitted int64, now time.Time) (bool, error)
	// RecordFailure stores one (push, sha256(token)) failure and increments
	// failed_count; a replay of the same token is ignored and returns false.
	// The token itself is never stored (design spec §2.8).
	RecordFailure(ctx context.Context, id int64, token, reason string, now time.Time) (bool, error)
	// RecordAttempt increments attempts, stores errMsg as last_error, stamps
	// updated_at (so the stuck clock measures from the last attempt), and
	// returns the new attempt count.
	RecordAttempt(ctx context.Context, id int64, errMsg string, now time.Time) (int64, error)
	// MarkCompleted moves a sending push to a terminal status, stamping
	// completed_at; false if the push was not sending (already canceled).
	MarkCompleted(ctx context.Context, id int64, status Status, lastError string, now time.Time) (bool, error)
	// Cancel moves a queued or sending push to canceled. ErrNotFound if
	// absent, ErrTerminal if already completed.
	Cancel(ctx context.Context, id int64, now time.Time) error
}

// NotifID is the gorush notif_id for a push.
func NotifID(pushID int64) string {
	return NotifIDPrefix + strconv.FormatInt(pushID, 10)
}

// ParseNotifID recovers the push id from a notif_id; ok is false for
// anything not minted by NotifID.
func ParseNotifID(s string) (int64, bool) {
	rest, found := strings.CutPrefix(s, NotifIDPrefix)
	if !found || rest == "" {
		return 0, false
	}
	id, err := strconv.ParseInt(rest, 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}
```

`internal/alertpush/messages.go`:

```go
package alertpush

import (
	"unicode/utf8"

	"github.com/OneBusAway/sidecar/internal/alerts"
)

// BuildMessages derives the push copy snapshot from an alert (design spec
// §2.4): English from the alert's own text, plus every language with at
// least one non-stale translation (the feed's staleness rule: the stored
// source hash must equal the hash of the current English field), each
// missing field falling back to English. A blank description promotes the
// header to the body, because an empty-bodied notification is invisible.
func BuildMessages(a alerts.Alert) Messages {
	english := Message{Title: a.HeaderText, Body: a.DescriptionText}
	if english.Body == "" {
		english = Message{Title: "", Body: a.HeaderText}
	}
	m := Messages{EnglishKey: clampMessage(english)}

	headerHash := alerts.SourceHash(a.HeaderText)
	descHash := alerts.SourceHash(a.DescriptionText)
	for _, t := range a.Translations {
		lang := alerts.NormalizeLanguage(t.Language)
		if lang == "" || lang == EnglishKey {
			continue
		}
		var fresh bool
		switch t.Field {
		case alerts.FieldHeader:
			fresh = t.SourceSHA256 == headerHash
		case alerts.FieldDescription:
			fresh = t.SourceSHA256 == descHash
		}
		if !fresh {
			continue
		}
		msg, ok := m[lang]
		if !ok {
			msg = Message{Title: a.HeaderText, Body: a.DescriptionText}
		}
		switch t.Field {
		case alerts.FieldHeader:
			msg.Title = t.Text
		case alerts.FieldDescription:
			msg.Body = t.Text
		}
		m[lang] = msg
	}
	for lang, msg := range m {
		if lang == EnglishKey {
			continue
		}
		if msg.Body == "" {
			msg = Message{Title: "", Body: msg.Title}
		}
		m[lang] = clampMessage(msg)
	}
	return m
}

func clampMessage(msg Message) Message {
	return Message{Title: Clamp(msg.Title, TitleLimit), Body: Clamp(msg.Body, BodyLimit)}
}

// Clamp truncates s to at most limit runes, replacing the tail with a single
// "…" when it had to cut. It counts runes, never bytes, so multi-byte
// text is never split mid-code-point.
func Clamp(s string, limit int) string {
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	runes := []rune(s)
	return string(runes[:limit-1]) + "…"
}
```

- [ ] **Step 4: Run, mutate, restore**

Run: `go test ./internal/alertpush/... -v`. Mutation: make the staleness check `fresh = true` — the first test must fail on `"es".Body`. Restore.

- [ ] **Step 5: Commit**

```bash
git add internal/alertpush && git commit -m "alertpush: domain types, repository contract, copy builder"
```

---

### Task 4: sqlite adapter + storetest conformance for `alertpush.Repository`

**Files:**
- Create: `internal/store/sqlite/queries/alertpushes.sql`, `internal/store/sqlite/alertpushes.go`, `internal/store/storetest/alertpushtest.go`
- Modify: `internal/store/sqlite/store.go` (accessor), `internal/store/sqlite/store_test.go` (suite wiring)
- Regenerate: `gen/`

**Interfaces:**
- Consumes: `alertpush.Repository` (Task 3), migration (Task 2).
- Produces: `func (s *Store) AlertPushes() alertpush.Repository`; `storetest.RunAlertPushRepository(t, newStore func(*testing.T) (alertpush.Repository, alerts.Repository, regions.Repository))`.

- [ ] **Step 1: Write the failing conformance suite**

`internal/store/storetest/alertpushtest.go`:

```go
package storetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/alertpush"
	"github.com/OneBusAway/sidecar/internal/alerts"
	"github.com/OneBusAway/sidecar/internal/regions"
)

type newAlertPushStoreFunc func(*testing.T) (alertpush.Repository, alerts.Repository, regions.Repository)

// RunAlertPushRepository exercises an alertpush.Repository against the
// contract the dispatcher, admin API, and feedback webhook depend on.
func RunAlertPushRepository(t *testing.T, newStore newAlertPushStoreFunc) {
	t.Helper()
	t.Run("CreateGetRoundTrip", func(t *testing.T) { testAlertPushCreateGetRoundTrip(t, newStore) })
	t.Run("CreateRejectsInFlightDuplicate", func(t *testing.T) { testAlertPushCreateRejectsInFlight(t, newStore) })
	t.Run("ListByAlertNewestFirst", func(t *testing.T) { testAlertPushListByAlert(t, newStore) })
	t.Run("ClaimTakesQueuedAndStuckOnly", func(t *testing.T) { testAlertPushClaim(t, newStore) })
	t.Run("AdvanceCursorIsConditional", func(t *testing.T) { testAlertPushAdvanceCursor(t, newStore) })
	t.Run("RecordFailureDedupsAndCounts", func(t *testing.T) { testAlertPushRecordFailure(t, newStore) })
	t.Run("RecordAttemptAndMarkCompleted", func(t *testing.T) { testAlertPushAttemptsAndCompletion(t, newStore) })
	t.Run("CancelTransitions", func(t *testing.T) { testAlertPushCancel(t, newStore) })
	t.Run("CascadesWithAlert", func(t *testing.T) { testAlertPushCascade(t, newStore) })
}

// seedPushAlert creates a region and a published alert to hang pushes on.
func seedPushAlert(t *testing.T, alertsRepo alerts.Repository, regionsRepo regions.Repository, regionID int64) alerts.Alert {
	t.Helper()
	putStoretestRegion(t, regionsRepo, regionID)
	a, err := alertsRepo.Create(context.Background(), alerts.NewAlert{
		RegionID: regionID, AgencyID: "1", HeaderText: "Route 44 detour",
		DescriptionText: "Buses skip 3rd Ave.", Cause: "CONSTRUCTION", Effect: "DETOUR",
		Severity: "WARNING", StartTime: base,
	}, base)
	if err != nil {
		t.Fatalf("create alert: %v", err)
	}
	return a
}

func newPushFor(a alerts.Alert) alertpush.NewPush {
	return alertpush.NewPush{AlertID: a.ID, RegionID: a.RegionID, Audience: alertpush.AudienceAll,
		Messages: alertpush.Messages{"en": {Title: "Route 44 detour", Body: "Buses skip 3rd Ave."}, "es": {Title: "Desvío", Body: "Buses skip 3rd Ave."}}}
}

func testAlertPushCreateGetRoundTrip(t *testing.T, newStore newAlertPushStoreFunc) {
	repo, alertsRepo, regionsRepo := newStore(t)
	ctx := context.Background()
	a := seedPushAlert(t, alertsRepo, regionsRepo, 1)

	created, err := repo.Create(ctx, newPushFor(a), base)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := repo.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != alertpush.StatusQueued || got.Audience != alertpush.AudienceAll || got.AlertID != a.ID || got.RegionID != 1 {
		t.Errorf("round trip = %+v", got)
	}
	if got.Messages["es"].Title != "Desvío" {
		t.Errorf("Messages not round-tripped: %+v", got.Messages)
	}
	if !got.CreatedAt.Equal(base) || !got.UpdatedAt.Equal(base) || got.StartedAt != nil || got.CompletedAt != nil {
		t.Errorf("timestamps = created %v updated %v started %v completed %v", got.CreatedAt, got.UpdatedAt, got.StartedAt, got.CompletedAt)
	}
	if len(got.FailureReasons) != 0 {
		t.Errorf("FailureReasons = %v, want empty", got.FailureReasons)
	}
	if _, err := repo.Get(ctx, 999); !errors.Is(err, alertpush.ErrNotFound) {
		t.Errorf("Get(999) = %v, want ErrNotFound", err)
	}
}

func testAlertPushCreateRejectsInFlight(t *testing.T, newStore newAlertPushStoreFunc) {
	repo, alertsRepo, regionsRepo := newStore(t)
	ctx := context.Background()
	a := seedPushAlert(t, alertsRepo, regionsRepo, 1)
	first, err := repo.Create(ctx, newPushFor(a), base)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	inflight, err := repo.InFlightForAlert(ctx, a.ID)
	if err != nil || !inflight {
		t.Fatalf("InFlightForAlert = %v, %v; want true", inflight, err)
	}
	if _, err := repo.Create(ctx, newPushFor(a), base); !errors.Is(err, alertpush.ErrInFlight) {
		t.Errorf("second Create = %v, want ErrInFlight", err)
	}
	if err := repo.Cancel(ctx, first.ID, base.Add(time.Minute)); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	inflight, _ = repo.InFlightForAlert(ctx, a.ID)
	if inflight {
		t.Error("InFlightForAlert after cancel = true, want false")
	}
	if _, err := repo.Create(ctx, newPushFor(a), base.Add(2*time.Minute)); err != nil {
		t.Errorf("Create after cancel: %v, want success (completed pushes do not block)", err)
	}
}

func testAlertPushListByAlert(t *testing.T, newStore newAlertPushStoreFunc) {
	repo, alertsRepo, regionsRepo := newStore(t)
	ctx := context.Background()
	a := seedPushAlert(t, alertsRepo, regionsRepo, 1)
	b := seedPushAlert(t, alertsRepo, regionsRepo, 2)
	p1, _ := repo.Create(ctx, newPushFor(a), base)
	if _, err := repo.MarkCompletedViaClaim(ctx, repo, p1.ID); err != nil { // helper below
		t.Fatal(err)
	}
	p2, _ := repo.Create(ctx, newPushFor(a), base.Add(time.Minute))
	if _, err := repo.Create(ctx, newPushFor(b), base); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.RecordFailure(ctx, p2.ID, "tok", "Unregistered", base); err != nil {
		t.Fatal(err)
	}
	list, err := repo.ListByAlert(ctx, a.ID)
	if err != nil {
		t.Fatalf("ListByAlert: %v", err)
	}
	if len(list) != 2 || list[0].ID != p2.ID || list[1].ID != p1.ID {
		t.Errorf("ListByAlert ids = %v, want [%d %d] (newest first, alert-scoped)", idsOf(list), p2.ID, p1.ID)
	}
	if len(list[0].FailureReasons) != 1 || list[0].FailureReasons[0].Reason != "Unregistered" {
		t.Errorf("ListByAlert must attach FailureReasons per row: %+v", list[0].FailureReasons)
	}
}
```

> **Note for the implementer:** `MarkCompletedViaClaim` above is a placeholder name — replace it with a local helper `completePush(t, repo, id)` that calls `Claim(ctx, base, base)` then `MarkCompleted(ctx, id, alertpush.StatusSent, "", base)` and fails the test on error. Also add `func idsOf(ps []alertpush.Push) []int64`.

Continue the file with:

```go
func testAlertPushClaim(t *testing.T, newStore newAlertPushStoreFunc) {
	repo, alertsRepo, regionsRepo := newStore(t)
	ctx := context.Background()
	a := seedPushAlert(t, alertsRepo, regionsRepo, 1)
	b := seedPushAlert(t, alertsRepo, regionsRepo, 2)
	c := seedPushAlert(t, alertsRepo, regionsRepo, 3)

	queued, _ := repo.Create(ctx, newPushFor(a), base)
	fresh, _ := repo.Create(ctx, newPushFor(b), base)
	stale, _ := repo.Create(ctx, newPushFor(c), base)

	// Put fresh and stale into sending with different updated_at stamps.
	if _, err := repo.Claim(ctx, base, base.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	// All three are now sending at updated_at = base. Re-queue `queued` by
	// canceling and recreating so we have one queued row.
	if err := repo.Cancel(ctx, queued.ID, base); err != nil {
		t.Fatal(err)
	}
	queued, _ = repo.Create(ctx, newPushFor(a), base.Add(time.Minute))
	// Touch `fresh` so it is not stuck.
	if err := repo.SetDeviceCount(ctx, fresh.ID, 1, base.Add(20*time.Minute)); err != nil {
		t.Fatal(err)
	}

	now := base.Add(21 * time.Minute)
	claimed, err := repo.Claim(ctx, now, now.Add(-alertpush.StuckAfter))
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	got := idsOf(claimed)
	if len(got) != 2 || got[0] != queued.ID || got[1] != stale.ID {
		t.Fatalf("Claim ids = %v, want [%d %d] (queued + stuck, ascending; fresh sending row untouched)", got, queued.ID, stale.ID)
	}
	for _, p := range claimed {
		if p.Status != alertpush.StatusSending || p.StartedAt == nil || !p.UpdatedAt.Equal(now) {
			t.Errorf("claimed %d = status %s started %v updated %v", p.ID, p.Status, p.StartedAt, p.UpdatedAt)
		}
	}
	// stale's original started_at (base) survives a reclaim.
	if p, _ := repo.Get(ctx, stale.ID); !p.StartedAt.Equal(base) {
		t.Errorf("reclaimed started_at = %v, want %v (preserved)", p.StartedAt, base)
	}
	// Nothing left to claim right away.
	again, _ := repo.Claim(ctx, now, now.Add(-alertpush.StuckAfter))
	if len(again) != 0 {
		t.Errorf("second Claim = %v, want empty", idsOf(again))
	}
}

func testAlertPushAdvanceCursor(t *testing.T, newStore newAlertPushStoreFunc) {
	repo, alertsRepo, regionsRepo := newStore(t)
	ctx := context.Background()
	a := seedPushAlert(t, alertsRepo, regionsRepo, 1)
	p, _ := repo.Create(ctx, newPushFor(a), base)

	// Not sending yet: refused.
	ok, err := repo.AdvanceCursor(ctx, p.ID, 0, 10, 5, base)
	if err != nil || ok {
		t.Fatalf("AdvanceCursor on queued = %v, %v; want false", ok, err)
	}
	if _, err := repo.Claim(ctx, base, base); err != nil {
		t.Fatal(err)
	}
	ok, err = repo.AdvanceCursor(ctx, p.ID, 0, 10, 5, base.Add(time.Second))
	if err != nil || !ok {
		t.Fatalf("AdvanceCursor(0→10) = %v, %v; want true", ok, err)
	}
	// Wrong previous cursor: refused, nothing changes.
	ok, err = repo.AdvanceCursor(ctx, p.ID, 0, 20, 5, base.Add(2*time.Second))
	if err != nil || ok {
		t.Fatalf("AdvanceCursor with stale prev = %v, %v; want false", ok, err)
	}
	ok, err = repo.AdvanceCursor(ctx, p.ID, 10, 20, 7, base.Add(3*time.Second))
	if err != nil || !ok {
		t.Fatalf("AdvanceCursor(10→20) = %v, %v; want true", ok, err)
	}
	got, _ := repo.Get(ctx, p.ID)
	if got.BatchCursor != 20 || got.SubmittedCount != 12 || !got.UpdatedAt.Equal(base.Add(3*time.Second)) {
		t.Errorf("after advances: cursor %d submitted %d updated %v", got.BatchCursor, got.SubmittedCount, got.UpdatedAt)
	}
	// A committed page resets the failure streak (design spec §2.6).
	if _, err := repo.RecordAttempt(ctx, p.ID, "blip", base.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}
	if ok, _ := repo.AdvanceCursor(ctx, p.ID, 20, 25, 1, base.Add(4*time.Second)); !ok {
		t.Fatal("AdvanceCursor(20→25) refused")
	}
	if got, _ = repo.Get(ctx, p.ID); got.Attempts != 0 || got.LastError != "" {
		t.Errorf("after progress: attempts %d last_error %q, want 0 and empty", got.Attempts, got.LastError)
	}
	// Canceled mid-send: refused.
	if err := repo.Cancel(ctx, p.ID, base.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	ok, _ = repo.AdvanceCursor(ctx, p.ID, 25, 30, 1, base.Add(5*time.Second))
	if ok {
		t.Error("AdvanceCursor after cancel = true, want false")
	}
}

func testAlertPushRecordFailure(t *testing.T, newStore newAlertPushStoreFunc) {
	repo, alertsRepo, regionsRepo := newStore(t)
	ctx := context.Background()
	a := seedPushAlert(t, alertsRepo, regionsRepo, 1)
	p, _ := repo.Create(ctx, newPushFor(a), base)

	for i, c := range []struct {
		token, reason string
		wantNew       bool
	}{
		{"t1", "Unregistered", true},
		{"t1", "Unregistered", false}, // replay
		{"t2", "BadDeviceToken", true},
		{"t3", "Unregistered", true},
	} {
		isNew, err := repo.RecordFailure(ctx, p.ID, c.token, c.reason, base.Add(time.Duration(i)*time.Second))
		if err != nil || isNew != c.wantNew {
			t.Errorf("RecordFailure #%d = %v, %v; want %v", i, isNew, err, c.wantNew)
		}
	}
	got, _ := repo.Get(ctx, p.ID)
	if got.FailedCount != 3 {
		t.Errorf("FailedCount = %d, want 3 (replay not double-counted)", got.FailedCount)
	}
	want := []alertpush.FailureReason{{Reason: "Unregistered", Count: 2}, {Reason: "BadDeviceToken", Count: 1}}
	if len(got.FailureReasons) != 2 || got.FailureReasons[0] != want[0] || got.FailureReasons[1] != want[1] {
		t.Errorf("FailureReasons = %v, want %v (by count desc)", got.FailureReasons, want)
	}
	if _, err := repo.RecordFailure(ctx, 999, "t", "x", base); err == nil {
		t.Error("RecordFailure(unknown push) error = nil, want error (FK)")
	}
	// The sqlite adapter test (store_test.go) additionally opens the raw
	// database and asserts SELECT token_sha256 FROM alert_push_failures never
	// equals a plaintext token -- see Task 4 step 1's TestAlertPushFailuresStoreOnlyHashes.
}

func testAlertPushAttemptsAndCompletion(t *testing.T, newStore newAlertPushStoreFunc) {
	repo, alertsRepo, regionsRepo := newStore(t)
	ctx := context.Background()
	a := seedPushAlert(t, alertsRepo, regionsRepo, 1)
	p, _ := repo.Create(ctx, newPushFor(a), base)
	if _, err := repo.Claim(ctx, base, base); err != nil {
		t.Fatal(err)
	}
	n, err := repo.RecordAttempt(ctx, p.ID, "gorush: 502", base.Add(time.Second))
	if err != nil || n != 1 {
		t.Fatalf("RecordAttempt = %d, %v; want 1", n, err)
	}
	n, _ = repo.RecordAttempt(ctx, p.ID, "gorush: 503", base.Add(2*time.Second))
	if n != 2 {
		t.Errorf("second RecordAttempt = %d, want 2", n)
	}
	got, _ := repo.Get(ctx, p.ID)
	if got.LastError != "gorush: 503" || got.Attempts != 2 || !got.UpdatedAt.Equal(base.Add(2*time.Second)) {
		t.Errorf("after attempts: %+v", got)
	}
	ok, err := repo.MarkCompleted(ctx, p.ID, alertpush.StatusSent, "", base.Add(3*time.Second))
	if err != nil || !ok {
		t.Fatalf("MarkCompleted = %v, %v; want true", ok, err)
	}
	got, _ = repo.Get(ctx, p.ID)
	if got.Status != alertpush.StatusSent || got.CompletedAt == nil || !got.CompletedAt.Equal(base.Add(3*time.Second)) || got.LastError != "" {
		t.Errorf("after MarkCompleted: %+v", got)
	}
	// Terminal rows are never re-completed.
	ok, _ = repo.MarkCompleted(ctx, p.ID, alertpush.StatusFailed, "late", base.Add(4*time.Second))
	if ok {
		t.Error("MarkCompleted on sent row = true, want false")
	}
	// A queued (not sending) row is not completable either.
	q, _ := repo.Create(ctx, newPushFor(a), base.Add(5*time.Second))
	ok, _ = repo.MarkCompleted(ctx, q.ID, alertpush.StatusFailed, "x", base.Add(6*time.Second))
	if ok {
		t.Error("MarkCompleted on queued row = true, want false")
	}
}

func testAlertPushCancel(t *testing.T, newStore newAlertPushStoreFunc) {
	repo, alertsRepo, regionsRepo := newStore(t)
	ctx := context.Background()
	a := seedPushAlert(t, alertsRepo, regionsRepo, 1)
	p, _ := repo.Create(ctx, newPushFor(a), base)
	if err := repo.Cancel(ctx, p.ID, base.Add(time.Second)); err != nil {
		t.Fatalf("Cancel queued: %v", err)
	}
	got, _ := repo.Get(ctx, p.ID)
	if got.Status != alertpush.StatusCanceled || got.CompletedAt == nil {
		t.Errorf("after cancel: %+v", got)
	}
	if err := repo.Cancel(ctx, p.ID, base.Add(2*time.Second)); !errors.Is(err, alertpush.ErrTerminal) {
		t.Errorf("Cancel twice = %v, want ErrTerminal", err)
	}
	if err := repo.Cancel(ctx, 999, base); !errors.Is(err, alertpush.ErrNotFound) {
		t.Errorf("Cancel(999) = %v, want ErrNotFound", err)
	}
	// Sending rows cancel too (the dispatcher yields at its next cursor advance).
	p2, _ := repo.Create(ctx, newPushFor(a), base.Add(3*time.Second))
	if _, err := repo.Claim(ctx, base.Add(3*time.Second), base); err != nil {
		t.Fatal(err)
	}
	if err := repo.Cancel(ctx, p2.ID, base.Add(4*time.Second)); err != nil {
		t.Errorf("Cancel sending: %v", err)
	}
}

func testAlertPushCascade(t *testing.T, newStore newAlertPushStoreFunc) {
	repo, alertsRepo, regionsRepo := newStore(t)
	ctx := context.Background()
	a := seedPushAlert(t, alertsRepo, regionsRepo, 1)
	p, _ := repo.Create(ctx, newPushFor(a), base)
	if _, err := repo.RecordFailure(ctx, p.ID, "t", "Unregistered", base); err != nil {
		t.Fatal(err)
	}
	if err := alertsRepo.Delete(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Get(ctx, p.ID); !errors.Is(err, alertpush.ErrNotFound) {
		t.Errorf("Get after alert delete = %v, want ErrNotFound (cascade)", err)
	}
}
```

Wire it in `internal/store/sqlite/store_test.go` next to the other suites, plus an adapter-only hash check (this file already opens raw `*sql.DB`s on `sqlitetest.OpenAt` paths — follow `TestMigrateCreatesAuthTables`):

```go
func TestAlertPushRepository(t *testing.T) {
	t.Parallel()
	storetest.RunAlertPushRepository(t, func(t *testing.T) (alertpush.Repository, alerts.Repository, regions.Repository) {
		store := sqlitetest.Open(t)
		return store.AlertPushes(), store.Alerts(), store.Regions()
	})
}

// TestAlertPushFailuresStoreOnlyHashes pins design spec §2.8: the failure
// table must never hold a plaintext device token.
func TestAlertPushFailuresStoreOnlyHashes(t *testing.T) {
	t.Parallel()
	path, store := sqlitetest.OpenAt(t)
	// seed region 1 + alert + push via the repositories (as in the suite),
	// RecordFailure(ctx, p.ID, "PLAINTEXT-TOKEN", "Unregistered", now),
	// then open sql.Open("sqlite", path) and:
	//   SELECT token_sha256 FROM alert_push_failures
	// assert the value != "PLAINTEXT-TOKEN" and equals
	// hex(sha256("PLAINTEXT-TOKEN")).
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/store/sqlite -run TestAlertPushRepository`
Expected: compile error — `store.AlertPushes undefined`.

- [ ] **Step 3: Queries**

`internal/store/sqlite/queries/alertpushes.sql` (every parameter `sqlc.arg`; no bare `?`):

```sql
-- name: CreateAlertPush :one
INSERT INTO alert_pushes (
  alert_id, region_id, audience, status, messages, created_at, updated_at
) VALUES (
  sqlc.arg(alert_id), sqlc.arg(region_id), sqlc.arg(audience), 'queued',
  sqlc.arg(messages), sqlc.arg(created_at), sqlc.arg(updated_at)
)
RETURNING *;

-- name: GetAlertPush :one
SELECT * FROM alert_pushes WHERE id = sqlc.arg(id);

-- name: ListAlertPushesByAlert :many
SELECT * FROM alert_pushes WHERE alert_id = sqlc.arg(alert_id) ORDER BY id DESC;

-- name: CountInFlightAlertPushes :one
SELECT COUNT(*) FROM alert_pushes
WHERE alert_id = sqlc.arg(alert_id) AND status IN ('queued', 'sending');

-- name: ClaimAlertPushes :many
-- started_at is preserved on a reclaim so the record still shows when the
-- send first began. Two named parameters carry the same instant because
-- sqlc's SQLite engine binds each sqlc.arg occurrence separately.
UPDATE alert_pushes SET
  status     = 'sending',
  started_at = COALESCE(started_at, sqlc.arg(started_now)),
  updated_at = sqlc.arg(now)
WHERE status = 'queued'
   OR (status = 'sending' AND updated_at < sqlc.arg(stuck_before))
RETURNING *;

-- name: SetAlertPushDeviceCount :execrows
UPDATE alert_pushes SET device_count = sqlc.arg(device_count), updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id);

-- name: AdvanceAlertPushCursor :execrows
-- A committed page also clears the failure streak (design spec §2.6).
UPDATE alert_pushes SET
  batch_cursor    = sqlc.arg(new_cursor),
  submitted_count = submitted_count + sqlc.arg(submitted),
  attempts        = 0,
  last_error      = '',
  updated_at      = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND batch_cursor = sqlc.arg(prev_cursor) AND status = 'sending';

-- name: InsertAlertPushFailure :execrows
-- Never rewrite this as ON CONFLICT DO UPDATE SET failed_count = ...: a
-- parameter on the right-hand side of that clause is the sqlc bug
-- documented at the top of pushregs.sql. The increment is a second
-- statement in the same transaction (alertPushRepo.RecordFailure).
INSERT OR IGNORE INTO alert_push_failures (push_id, token_sha256, reason, created_at)
VALUES (sqlc.arg(push_id), sqlc.arg(token_sha256), sqlc.arg(reason), sqlc.arg(created_at));

-- name: IncrementAlertPushFailed :execrows
UPDATE alert_pushes SET failed_count = failed_count + 1, updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id);

-- name: ListAlertPushFailureReasons :many
SELECT reason, COUNT(*) AS n FROM alert_push_failures
WHERE push_id = sqlc.arg(push_id)
GROUP BY reason ORDER BY n DESC, reason LIMIT 10;

-- name: RecordAlertPushAttempt :one
UPDATE alert_pushes SET attempts = attempts + 1, last_error = sqlc.arg(last_error), updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id)
RETURNING attempts;

-- name: CompleteAlertPush :execrows
UPDATE alert_pushes SET
  status = sqlc.arg(status), last_error = sqlc.arg(last_error),
  completed_at = sqlc.arg(completed_at), updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND status = 'sending';

-- name: CancelAlertPush :execrows
UPDATE alert_pushes SET status = 'canceled', completed_at = sqlc.arg(completed_at), updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND status IN ('queued', 'sending');
```

Run `make generate`. If sqlc rejects `UPDATE … RETURNING *` as `:many`, fall back to a `:exec` UPDATE followed by `SELECT * FROM alert_pushes WHERE status = 'sending' AND updated_at = sqlc.arg(now) ORDER BY id` inside the same write transaction (the transaction is what keeps it atomic under `_txlock=immediate`).

- [ ] **Step 4: Adapter**

`internal/store/sqlite/alertpushes.go`:

```go
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/OneBusAway/sidecar/internal/alertpush"
	"github.com/OneBusAway/sidecar/internal/store/sqlite/gen"
)

// alertPushRepo implements alertpush.Repository. Error strings never embed
// tokens (alert_push_failures stores them; errors from that table name only
// the push id).
type alertPushRepo struct {
	db *sql.DB
	q  *gen.Queries
}

func alertPushFromRow(row gen.AlertPush) (alertpush.Push, error) {
	var msgs alertpush.Messages
	if err := json.Unmarshal([]byte(row.Messages), &msgs); err != nil {
		return alertpush.Push{}, fmt.Errorf("sqlite: alert push %d: decode messages: %w", row.ID, err)
	}
	return alertpush.Push{
		ID: row.ID, AlertID: row.AlertID, RegionID: row.RegionID,
		Audience: alertpush.Audience(row.Audience), Status: alertpush.Status(row.Status),
		Messages: msgs, BatchCursor: row.BatchCursor, DeviceCount: row.DeviceCount,
		SubmittedCount: row.SubmittedCount, FailedCount: row.FailedCount, Attempts: row.Attempts,
		LastError: row.LastError, StartedAt: nullUnixToTime(row.StartedAt),
		CompletedAt: nullUnixToTime(row.CompletedAt),
		CreatedAt: unixToTime(row.CreatedAt), UpdatedAt: unixToTime(row.UpdatedAt),
	}, nil
}

func (r *alertPushRepo) Create(ctx context.Context, in alertpush.NewPush, now time.Time) (alertpush.Push, error) {
	msgs, err := json.Marshal(in.Messages)
	if err != nil {
		return alertpush.Push{}, fmt.Errorf("sqlite: encode alert push messages: %w", err)
	}
	row, err := r.q.CreateAlertPush(ctx, gen.CreateAlertPushParams{
		AlertID: in.AlertID, RegionID: in.RegionID, Audience: string(in.Audience),
		Messages: string(msgs), CreatedAt: now.Unix(), UpdatedAt: now.Unix(),
	})
	if err != nil {
		if isUniqueViolation(err, "alert_pushes.alert_id") {
			return alertpush.Push{}, fmt.Errorf("sqlite: create alert push for alert %d: %w", in.AlertID, alertpush.ErrInFlight)
		}
		return alertpush.Push{}, fmt.Errorf("sqlite: create alert push for alert %d: %w", in.AlertID, err)
	}
	return alertPushFromRow(row)
}
```

Implement the rest following the interface doc comments: `Get` (map `sql.ErrNoRows` → `ErrNotFound`, then `ListAlertPushFailureReasons` → `FailureReasons`); `ListByAlert`; `InFlightForAlert` (`count > 0`); `Claim` (`ClaimAlertPushes{StartedNow: now.Unix(), Now: now.Unix(), StuckBefore: stuckBefore.Unix()}`, convert rows, `sort.Slice` by ID); `SetDeviceCount`; `AdvanceCursor` (`rows == 1`); `RecordFailure` (compute `hex(sha256(token))` with `crypto/sha256` + `encoding/hex`; BeginTx → `q.WithTx(tx)` → `InsertAlertPushFailure{TokenSha256: hash}`; if `rows == 1` then `IncrementAlertPushFailed`; commit; return `rows == 1`; a FK failure on the insert is returned as an error naming only the push id); `ListByAlert` attaches `FailureReasons` per row (`ListAlertPushFailureReasons` for each); `RecordAttempt`; `MarkCompleted` (`CompletedAt: sql.NullInt64{Int64: now.Unix(), Valid: true}`, `rows == 1`); `Cancel` (rows == 0 → `Get`: `ErrNotFound` if absent, else `ErrTerminal`).

Check `isUniqueViolation`'s exact signature in `store.go` (it exists — grep it) and match its argument convention for a partial index (modernc reports `UNIQUE constraint failed: alert_pushes.alert_id`).

`store.go` accessor:

```go
// AlertPushes returns the alertpush.Repository backed by this store (spec §4).
func (s *Store) AlertPushes() alertpush.Repository {
	return &alertPushRepo{db: s.db, q: s.q}
}
```

- [ ] **Step 5: Run, mutate, restore**

Run: `go test ./internal/store/... && make generate-check`. Mutation: remove `AND batch_cursor = sqlc.arg(prev_cursor)` — `AdvanceCursorIsConditional` must fail. Restore, regenerate.

- [ ] **Step 6: Commit**

```bash
git add internal/store && git commit -m "store: alert push repository with conformance suite"
```

---

### Task 5: `alertpush.Enqueuer` (shared preconditions)

**Files:**
- Create: `internal/alertpush/enqueue.go`
- Test: `internal/alertpush/enqueue_test.go` (package `alertpush_test`, uses `sqlitetest.Open` — the external test package avoids an import cycle with the sqlite adapter)

**Interfaces:**
- Consumes: `alertpush.Repository`, `alerts.Repository`, `pushreg.Repository.CountAudience`.
- Produces:

```go
type Enqueuer struct {
	Repo     Repository
	Alerts   alerts.Repository
	PushRegs pushreg.Repository
}
// Enqueue applies design spec §2.2 and inserts a queued push.
func (e *Enqueuer) Enqueue(ctx context.Context, alertID int64, audience Audience, now time.Time) (Push, error)
// AudienceFor is the read-only counterpart for the SPA's reach card.
func (e *Enqueuer) AudienceFor(ctx context.Context, alertID int64) (AudienceReport, error)
type AudienceReport struct { All, Test pushreg.AudienceCount; ForcedTest bool }
```

- [ ] **Step 1: Write the failing tests**

```go
package alertpush_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/alertpush"
	"github.com/OneBusAway/sidecar/internal/alerts"
	"github.com/OneBusAway/sidecar/internal/pushreg"
	"github.com/OneBusAway/sidecar/internal/regions"
	"github.com/OneBusAway/sidecar/internal/store/sqlite"
	"github.com/OneBusAway/sidecar/internal/store/sqlitetest"
)

var base = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func ptr[T any](v T) *T { return &v }

type fixture struct {
	store *sqlite.Store
	enq   *alertpush.Enqueuer
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	store := sqlitetest.Open(t)
	ctx := context.Background()
	if err := store.Regions().UpsertFromDirectory(ctx, []regions.Region{{ID: 1, Name: "R", OBABaseURL: "https://x/", Active: true}}, base); err != nil {
		t.Fatal(err)
	}
	return fixture{store: store, enq: &alertpush.Enqueuer{Repo: store.AlertPushes(), Alerts: store.Alerts(), PushRegs: store.PushRegs()}}
}

func (f fixture) alert(t *testing.T, published, isTest bool) alerts.Alert {
	t.Helper()
	ctx := context.Background()
	a, err := f.store.Alerts().Create(ctx, alerts.NewAlert{RegionID: 1, AgencyID: "1", HeaderText: "Hdr", DescriptionText: "Desc",
		Cause: "CONSTRUCTION", Effect: "DETOUR", Severity: "WARNING", StartTime: base, IsTest: isTest}, base)
	if err != nil {
		t.Fatal(err)
	}
	if published {
		if err := f.store.Alerts().SetPublished(ctx, a.ID, true, base); err != nil {
			t.Fatal(err)
		}
	}
	return a
}

func (f fixture) register(t *testing.T, token string, test bool) {
	t.Helper()
	up := pushreg.Upsert{RegionID: 1, Token: token, OperatingSystem: pushreg.OSIOS}
	if test {
		up.TestDevice, up.Description = ptr(true), ptr("QA")
	}
	if err := f.store.PushRegs().Upsert(context.Background(), up, base); err != nil {
		t.Fatal(err)
	}
}

func TestEnqueueHappyPathSnapshotsCopy(t *testing.T) {
	f := newFixture(t)
	a := f.alert(t, true, false)
	f.register(t, "tok", false)
	p, err := f.enq.Enqueue(context.Background(), a.ID, alertpush.AudienceAll, base)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if p.Status != alertpush.StatusQueued || p.Audience != alertpush.AudienceAll || p.RegionID != 1 {
		t.Errorf("push = %+v", p)
	}
	if p.Messages["en"] != (alertpush.Message{Title: "Hdr", Body: "Desc"}) {
		t.Errorf("Messages = %+v", p.Messages)
	}
}

func TestEnqueueRejectsUnpublished(t *testing.T) {
	f := newFixture(t)
	a := f.alert(t, false, false)
	f.register(t, "tok", false)
	if _, err := f.enq.Enqueue(context.Background(), a.ID, alertpush.AudienceAll, base); !errors.Is(err, alertpush.ErrNotPublished) {
		t.Errorf("err = %v, want ErrNotPublished", err)
	}
}

func TestEnqueueRejectsUnknownAlert(t *testing.T) {
	f := newFixture(t)
	if _, err := f.enq.Enqueue(context.Background(), 999, alertpush.AudienceAll, base); !errors.Is(err, alerts.ErrNotFound) {
		t.Errorf("err = %v, want alerts.ErrNotFound", err)
	}
}

func TestEnqueueRejectsEmptyAudience(t *testing.T) {
	f := newFixture(t)
	a := f.alert(t, true, false)
	f.register(t, "tok", false) // a non-test device: the test audience is empty
	if _, err := f.enq.Enqueue(context.Background(), a.ID, alertpush.AudienceTest, base); !errors.Is(err, alertpush.ErrEmptyAudience) {
		t.Errorf("err = %v, want ErrEmptyAudience", err)
	}
}

func TestEnqueueTestAlertForcesTestAudience(t *testing.T) {
	f := newFixture(t)
	a := f.alert(t, true, true)
	f.register(t, "qa", true)
	p, err := f.enq.Enqueue(context.Background(), a.ID, alertpush.AudienceAll, base)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if p.Audience != alertpush.AudienceTest {
		t.Errorf("Audience = %s, want test (forced for a test alert)", p.Audience)
	}
	rep, err := f.enq.AudienceFor(context.Background(), a.ID)
	if err != nil || !rep.ForcedTest || rep.Test.Total != 1 {
		t.Errorf("AudienceFor = %+v, %v", rep, err)
	}
}

func TestEnqueueRejectsInFlight(t *testing.T) {
	f := newFixture(t)
	a := f.alert(t, true, false)
	f.register(t, "tok", false)
	if _, err := f.enq.Enqueue(context.Background(), a.ID, alertpush.AudienceAll, base); err != nil {
		t.Fatal(err)
	}
	if _, err := f.enq.Enqueue(context.Background(), a.ID, alertpush.AudienceAll, base); !errors.Is(err, alertpush.ErrInFlight) {
		t.Errorf("second Enqueue = %v, want ErrInFlight", err)
	}
}
```

- [ ] **Step 2: Run to verify failure** — `go test ./internal/alertpush -run TestEnqueue` → undefined `Enqueuer`.

- [ ] **Step 3: Implement `enqueue.go`**

```go
package alertpush

import (
	"context"
	"fmt"
	"time"

	"github.com/OneBusAway/sidecar/internal/alerts"
	"github.com/OneBusAway/sidecar/internal/pushreg"
)

// Enqueuer applies the send preconditions (design spec §2.2) and inserts
// the queued push. It is shared by the admin API and the CLI so the two
// trigger surfaces cannot drift.
type Enqueuer struct {
	Repo     Repository
	Alerts   alerts.Repository
	PushRegs pushreg.Repository
}

// AudienceReport is the reach preview for one alert: both audiences'
// counts, and whether the alert's test flag forces the test audience.
type AudienceReport struct {
	All        pushreg.AudienceCount
	Test       pushreg.AudienceCount
	ForcedTest bool
}

// Enqueue validates and inserts a queued push for alertID. Errors:
// alerts.ErrNotFound, ErrNotPublished, ErrInFlight, ErrEmptyAudience. A
// test alert is always sent to the test audience regardless of audience.
func (e *Enqueuer) Enqueue(ctx context.Context, alertID int64, audience Audience, now time.Time) (Push, error) {
	a, err := e.Alerts.Get(ctx, alertID)
	if err != nil {
		return Push{}, err
	}
	if !a.Published {
		return Push{}, ErrNotPublished
	}
	if a.IsTest {
		audience = AudienceTest
	}
	if audience == "" {
		audience = AudienceAll
	}
	inFlight, err := e.Repo.InFlightForAlert(ctx, alertID)
	if err != nil {
		return Push{}, err
	}
	if inFlight {
		return Push{}, ErrInFlight
	}
	count, err := e.PushRegs.CountAudience(ctx, a.RegionID, audience == AudienceTest)
	if err != nil {
		return Push{}, fmt.Errorf("alertpush: count audience: %w", err)
	}
	if count.Total == 0 {
		return Push{}, ErrEmptyAudience
	}
	return e.Repo.Create(ctx, NewPush{
		AlertID: a.ID, RegionID: a.RegionID, Audience: audience, Messages: BuildMessages(a),
	}, now)
}

// AudienceFor reports both audiences' sizes for alertID's region.
func (e *Enqueuer) AudienceFor(ctx context.Context, alertID int64) (AudienceReport, error) {
	a, err := e.Alerts.Get(ctx, alertID)
	if err != nil {
		return AudienceReport{}, err
	}
	all, err := e.PushRegs.CountAudience(ctx, a.RegionID, false)
	if err != nil {
		return AudienceReport{}, fmt.Errorf("alertpush: count audience: %w", err)
	}
	test, err := e.PushRegs.CountAudience(ctx, a.RegionID, true)
	if err != nil {
		return AudienceReport{}, fmt.Errorf("alertpush: count test audience: %w", err)
	}
	return AudienceReport{All: all, Test: test, ForcedTest: a.IsTest}, nil
}
```

- [ ] **Step 4: Run, mutate (drop the `a.IsTest` override → `ForcesTestAudience` fails), restore.**

- [ ] **Step 5: Commit** — `git add internal/alertpush && git commit -m "alertpush: Enqueuer with shared send preconditions"`

---

### Task 6: `alertpush.Dispatcher`

**Files:**
- Create: `internal/alertpush/dispatcher.go`
- Test: `internal/alertpush/dispatcher_test.go` (package `alertpush_test`; real sqlite store via `sqlitetest.Open`, fake `push.BatchSender`)

**Interfaces:**
- Consumes: `Repository` (Task 4), `pushreg.Repository.ListAudience`/`CountAudience` (Task 2), `push.BatchSender` (Task 1), `pushreg.NormalizeLocale`, `Messages.For/Catalog`.
- Produces:

```go
// Waker is what the admin API pokes after enqueueing (design spec §2.6).
type Waker interface{ Wake() }

type Dispatcher struct {
	Repo     Repository
	Alerts   alerts.Repository
	PushRegs pushreg.Repository
	Sender   push.BatchSender // nil = no transport: claimed pushes fail immediately
	Now      func() time.Time
	Logger   *slog.Logger
	wake     chan struct{} // lazily made by Wake/RunLoop via sync.Once
}
func (d *Dispatcher) Wake()
func (d *Dispatcher) RunOnce(ctx context.Context)
func (d *Dispatcher) RunLoop(ctx context.Context, interval time.Duration)
```

- [ ] **Step 1: Write the failing tests**

```go
package alertpush_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/alertpush"
	"github.com/OneBusAway/sidecar/internal/alerts"
	"github.com/OneBusAway/sidecar/internal/push"
	"github.com/OneBusAway/sidecar/internal/pushreg"
)

// fakeSender records every batch. failOn returns an error for the nth call
// (1-based) when non-zero; rejectTokens are reported as inline rejections.
type fakeSender struct {
	mu           sync.Mutex
	calls        []sentBatch
	failOn       int
	rejectTokens map[string]string
}

type sentBatch struct {
	notifID  string
	n        push.Notification
}

func (s *fakeSender) SendBatch(_ context.Context, n push.Notification, notifID string) (push.BatchResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, sentBatch{notifID: notifID, n: n})
	if s.failOn == len(s.calls) {
		return push.BatchResult{}, errors.New("gorush: 502")
	}
	var res push.BatchResult
	for _, tok := range n.Tokens {
		if reason, ok := s.rejectTokens[tok]; ok {
			res.Rejected = append(res.Rejected, push.Rejection{Token: tok, Reason: reason})
		}
	}
	return res, nil
}

func newDispatcher(f fixture, sender push.BatchSender, now *time.Time) *alertpush.Dispatcher {
	return &alertpush.Dispatcher{
		Repo: f.store.AlertPushes(), Alerts: f.store.Alerts(), PushRegs: f.store.PushRegs(),
		Sender: sender, Now: func() time.Time { return *now },
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func (f fixture) registerFull(t *testing.T, token, os, locale string, sandbox, test bool) {
	t.Helper()
	up := pushreg.Upsert{RegionID: 1, Token: token, OperatingSystem: os, APNSSandbox: sandbox, Locale: ptr(locale)}
	if test {
		up.TestDevice, up.Description = ptr(true), ptr("QA")
	}
	if err := f.store.PushRegs().Upsert(context.Background(), up, base); err != nil {
		t.Fatal(err)
	}
}

func (f fixture) translate(t *testing.T, a alerts.Alert, lang string, field alerts.Field, text string) {
	t.Helper()
	src := a.HeaderText
	if field == alerts.FieldDescription {
		src = a.DescriptionText
	}
	if err := f.store.Alerts().UpsertTranslation(context.Background(), a.ID, alerts.Translation{
		Language: lang, Field: field, Text: text, SourceSHA256: alerts.SourceHash(src)}, base); err != nil {
		t.Fatal(err)
	}
}

func TestDispatcherGroupsByPlatformLocaleAndSandbox(t *testing.T) {
	f := newFixture(t)
	a := f.alert(t, true, false)
	f.translate(t, a, "es", alerts.FieldHeader, "Título")
	f.registerFull(t, "ios-en-prod", pushreg.OSIOS, "en-US", false, false)
	f.registerFull(t, "ios-en-sandbox", pushreg.OSIOS, "", true, false)
	f.registerFull(t, "ios-es-mx", pushreg.OSIOS, "es-MX", false, false) // bare-subtag match → es
	f.registerFull(t, "android-es", pushreg.OSAndroid, "es", false, false)
	f.registerFull(t, "android-de", pushreg.OSAndroid, "de", false, false) // no catalog match → English
	p, err := f.enq.Enqueue(context.Background(), a.ID, alertpush.AudienceAll, base)
	if err != nil {
		t.Fatal(err)
	}

	sender := &fakeSender{}
	now := base.Add(time.Second)
	d := newDispatcher(f, sender, &now)
	d.RunOnce(context.Background())

	// Expect exactly four groups; order within a page is not contractual,
	// so match by content.
	type key struct {
		platform push.Platform
		sandbox  bool
		title    string
	}
	got := map[key][]string{}
	for _, c := range sender.calls {
		if c.notifID != alertpush.NotifID(p.ID) {
			t.Errorf("notif_id = %q, want %q", c.notifID, alertpush.NotifID(p.ID))
		}
		got[key{c.n.Platform, c.n.Sandbox, c.n.Title}] = c.n.Tokens
	}
	want := map[key][]string{
		{push.PlatformIOS, false, "Hdr"}:        {"ios-en-prod"},
		{push.PlatformIOS, true, "Hdr"}:         {"ios-en-sandbox"},
		{push.PlatformIOS, false, "Título"}:     {"ios-es-mx"},
		{push.PlatformAndroid, false, "Título"}: {"android-es"},
		{push.PlatformAndroid, false, "Hdr"}:    {"android-de"},
	}
	if len(got) != len(want) {
		t.Fatalf("groups = %v, want %v", got, want)
	}
	for k, toks := range want {
		if g := got[k]; len(g) != len(toks) || g[0] != toks[0] {
			t.Errorf("group %+v tokens = %v, want %v", k, g, toks)
		}
	}
	final, _ := f.store.AlertPushes().Get(context.Background(), p.ID)
	if final.Status != alertpush.StatusSent || final.DeviceCount != 5 || final.SubmittedCount != 5 || final.FailedCount != 0 {
		t.Errorf("final push = %+v", final)
	}
	if final.CompletedAt == nil || !final.CompletedAt.Equal(now) {
		t.Errorf("CompletedAt = %v, want %v", final.CompletedAt, now)
	}
}

func TestDispatcherResumesFromCursorAfterTransportError(t *testing.T) {
	f := newFixture(t)
	a := f.alert(t, true, false)
	// 3 pages of BatchSize would need 1000+ rows; instead exercise paging
	// by making every registration its own group (distinct sandbox/locale
	// combos are limited, so use BatchSize-sized pages of one platform and
	// assert on cursor progress across two RunOnce calls).
	for i := 0; i < alertpush.BatchSize+3; i++ {
		f.registerFull(t, "tok-"+itoa(i), pushreg.OSAndroid, "", false, false)
	}
	p, err := f.enq.Enqueue(context.Background(), a.ID, alertpush.AudienceAll, base)
	if err != nil {
		t.Fatal(err)
	}
	sender := &fakeSender{failOn: 2} // page 1 succeeds, page 2's (only) group fails
	now := base
	d := newDispatcher(f, sender, &now)
	d.RunOnce(context.Background())

	mid, _ := f.store.AlertPushes().Get(context.Background(), p.ID)
	if mid.Status != alertpush.StatusSending || mid.Attempts != 1 || mid.LastError != "gorush: 502" {
		t.Fatalf("after failure: %+v", mid)
	}
	if mid.SubmittedCount != alertpush.BatchSize || mid.BatchCursor == 0 {
		t.Fatalf("page 1 not committed: submitted %d cursor %d", mid.SubmittedCount, mid.BatchCursor)
	}

	// Not yet stuck: a cycle 1 minute later leaves it alone.
	now = base.Add(time.Minute)
	d.RunOnce(context.Background())
	if len(sender.calls) != 2 {
		t.Fatalf("calls after non-stuck cycle = %d, want 2", len(sender.calls))
	}

	// Stuck: reclaimed, resumes at the cursor and sends ONLY the last page.
	now = base.Add(alertpush.StuckAfter + time.Minute)
	d.RunOnce(context.Background())
	if len(sender.calls) != 3 {
		t.Fatalf("calls after reclaim = %d, want 3", len(sender.calls))
	}
	if got := len(sender.calls[2].n.Tokens); got != 3 {
		t.Errorf("resumed page size = %d, want 3 (the remainder, not the whole audience)", got)
	}
	final, _ := f.store.AlertPushes().Get(context.Background(), p.ID)
	if final.Status != alertpush.StatusSent || final.SubmittedCount != int64(alertpush.BatchSize+3) {
		t.Errorf("final = %+v", final)
	}
	if final.Attempts != 0 || final.LastError != "" {
		t.Errorf("a committed page must clear the streak: attempts %d last_error %q", final.Attempts, final.LastError)
	}
}

func TestDispatcherMarksFailedAfterMaxAttempts(t *testing.T) {
	f := newFixture(t)
	a := f.alert(t, true, false)
	f.register(t, "tok", false)
	p, _ := f.enq.Enqueue(context.Background(), a.ID, alertpush.AudienceAll, base)
	now := base
	d := newDispatcher(f, &alwaysFail{}, &now)
	for i := 0; i < alertpush.MaxAttempts; i++ {
		d.RunOnce(context.Background())
		now = now.Add(alertpush.StuckAfter + time.Minute)
	}
	final, _ := f.store.AlertPushes().Get(context.Background(), p.ID)
	if final.Status != alertpush.StatusFailed || final.Attempts != alertpush.MaxAttempts || final.CompletedAt == nil {
		t.Errorf("final = %+v", final)
	}
}

type alwaysFail struct{}

func (alwaysFail) SendBatch(context.Context, push.Notification, string) (push.BatchResult, error) {
	return push.BatchResult{}, errors.New("down")
}

func TestDispatcherCountsInlineRejections(t *testing.T) {
	f := newFixture(t)
	a := f.alert(t, true, false)
	f.register(t, "good", false)
	f.register(t, "bad", false)
	p, _ := f.enq.Enqueue(context.Background(), a.ID, alertpush.AudienceAll, base)
	now := base
	d := newDispatcher(f, &fakeSender{rejectTokens: map[string]string{"bad": "BadDeviceToken"}}, &now)
	d.RunOnce(context.Background())
	final, _ := f.store.AlertPushes().Get(context.Background(), p.ID)
	if final.SubmittedCount != 1 || final.FailedCount != 1 || final.Status != alertpush.StatusSent {
		t.Errorf("final = %+v", final)
	}
	if len(final.FailureReasons) != 1 || final.FailureReasons[0] != (alertpush.FailureReason{Reason: "BadDeviceToken", Count: 1}) {
		t.Errorf("FailureReasons = %v", final.FailureReasons)
	}
}

func TestDispatcherCanceledPushIsNotSent(t *testing.T) {
	f := newFixture(t)
	a := f.alert(t, true, false)
	f.register(t, "tok", false)
	p, _ := f.enq.Enqueue(context.Background(), a.ID, alertpush.AudienceAll, base)
	if err := f.store.AlertPushes().Cancel(context.Background(), p.ID, base); err != nil {
		t.Fatal(err)
	}
	sender := &fakeSender{}
	now := base
	newDispatcher(f, sender, &now).RunOnce(context.Background())
	if len(sender.calls) != 0 {
		t.Errorf("calls = %d, want 0 for a canceled push", len(sender.calls))
	}
}

func TestDispatcherUnpublishedAlertCancelsPush(t *testing.T) {
	f := newFixture(t)
	a := f.alert(t, true, false)
	f.register(t, "tok", false)
	p, _ := f.enq.Enqueue(context.Background(), a.ID, alertpush.AudienceAll, base)
	if err := f.store.Alerts().SetPublished(context.Background(), a.ID, false, base); err != nil {
		t.Fatal(err)
	}
	sender := &fakeSender{}
	now := base
	newDispatcher(f, sender, &now).RunOnce(context.Background())
	final, _ := f.store.AlertPushes().Get(context.Background(), p.ID)
	if final.Status != alertpush.StatusCanceled || len(sender.calls) != 0 || final.LastError == "" {
		t.Errorf("final = %+v, calls %d", final, len(sender.calls))
	}
}

func TestDispatcherNoSenderFailsPush(t *testing.T) {
	f := newFixture(t)
	a := f.alert(t, true, false)
	f.register(t, "tok", false)
	p, _ := f.enq.Enqueue(context.Background(), a.ID, alertpush.AudienceAll, base)
	now := base
	newDispatcher(f, nil, &now).RunOnce(context.Background())
	final, _ := f.store.AlertPushes().Get(context.Background(), p.ID)
	if final.Status != alertpush.StatusFailed || final.LastError == "" {
		t.Errorf("final = %+v", final)
	}
}

func TestDispatcherWakeTriggersRunWithoutTick(t *testing.T) {
	f := newFixture(t)
	a := f.alert(t, true, false)
	f.register(t, "tok", false)
	p, _ := f.enq.Enqueue(context.Background(), a.ID, alertpush.AudienceAll, base)
	sender := &fakeSender{}
	now := base
	d := newDispatcher(f, sender, &now)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { d.RunLoop(ctx, time.Hour); close(done) }()
	d.Wake()
	deadline := time.Now().Add(5 * time.Second)
	for {
		final, _ := f.store.AlertPushes().Get(context.Background(), p.ID)
		if final.Status == alertpush.StatusSent {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("push not sent after Wake; status %s", final.Status)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	// Wake before RunLoop and repeated Wakes must never block.
	d2 := newDispatcher(f, sender, &now)
	d2.Wake()
	d2.Wake()
}

func itoa(i int) string { return strconv.Itoa(i) }
```

(Add `"strconv"` to imports. `time.Now`/`time.Sleep` are allowed here: this is a `_test.go` file.)

- [ ] **Step 2: Run to verify failure** — undefined `Dispatcher`.

- [ ] **Step 3: Implement `dispatcher.go`**

```go
package alertpush

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/OneBusAway/sidecar/internal/alerts"
	"github.com/OneBusAway/sidecar/internal/push"
	"github.com/OneBusAway/sidecar/internal/pushreg"
)

// Waker is the one method the admin API needs from the Dispatcher: a
// non-blocking nudge so an enqueued push starts now rather than at the
// next tick (design spec §2.6).
type Waker interface {
	Wake()
}

// Dispatcher performs alert push fan-out (spec §4, §12 row 3): it claims
// queued (and stuck) pushes, pages each audience by registration id, groups
// every page by (platform, normalized locale, APNs environment), sends one
// gorush batch per group, and commits progress one page at a time so a
// crash resumes at the last committed cursor.
type Dispatcher struct {
	Repo     Repository
	Alerts   alerts.Repository
	PushRegs pushreg.Repository
	// Sender may be nil (no push transport configured): claimed pushes are
	// failed immediately with an explanatory last_error rather than left
	// queued forever, because the CLI can enqueue without a server.
	Sender push.BatchSender
	Now    func() time.Time
	Logger *slog.Logger

	once sync.Once
	wake chan struct{}
}

func (d *Dispatcher) wakeCh() chan struct{} {
	d.once.Do(func() { d.wake = make(chan struct{}, 1) })
	return d.wake
}

// Wake asks the loop to run a cycle now. Never blocks; a pending wake is
// coalesced with any already queued.
func (d *Dispatcher) Wake() {
	select {
	case d.wakeCh() <- struct{}{}:
	default:
	}
}

// RunLoop runs a cycle on every tick and on every Wake until ctx is done.
func (d *Dispatcher) RunLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	wake := d.wakeCh()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.RunOnce(ctx)
		case <-wake:
			d.RunOnce(ctx)
		}
	}
}

// RunOnce claims every push that is due and sends each in turn. Exported
// so tests drive cycles without a ticker.
func (d *Dispatcher) RunOnce(ctx context.Context) {
	now := d.Now()
	claimed, err := d.Repo.Claim(ctx, now, now.Add(-StuckAfter))
	if err != nil {
		d.Logger.Error("alertpush: claim", "err", err)
		return
	}
	for _, p := range claimed {
		if ctx.Err() != nil {
			return
		}
		d.send(ctx, p)
	}
}

// send runs one push from its current cursor to the end of its audience.
func (d *Dispatcher) send(ctx context.Context, p Push) {
	log := d.Logger.With("push_id", p.ID, "alert_id", p.AlertID, "region_id", p.RegionID)

	if d.Sender == nil {
		d.complete(ctx, log, p.ID, StatusFailed, "no push transport configured (--gorush-url/SIDECAR_GORUSH_URL)")
		return
	}

	a, err := d.Alerts.Get(ctx, p.AlertID)
	switch {
	case errors.Is(err, alerts.ErrNotFound):
		d.complete(ctx, log, p.ID, StatusCanceled, "alert deleted before send")
		return
	case err != nil:
		log.Warn("alertpush: load alert", "err", err) // store blip: reclaimed as stuck later
		return
	case !a.Published:
		d.complete(ctx, log, p.ID, StatusCanceled, "alert unpublished before send")
		return
	}

	testOnly := p.Audience == AudienceTest
	if p.DeviceCount == 0 {
		count, err := d.PushRegs.CountAudience(ctx, p.RegionID, testOnly)
		if err != nil {
			log.Warn("alertpush: count audience", "err", err)
			return
		}
		if err := d.Repo.SetDeviceCount(ctx, p.ID, count.Total, d.Now()); err != nil {
			log.Warn("alertpush: set device count", "err", err)
			return
		}
	}

	catalog := p.Messages.Catalog()
	cursor := p.BatchCursor
	for {
		page, err := d.PushRegs.ListAudience(ctx, p.RegionID, testOnly, cursor, BatchSize)
		if err != nil {
			log.Warn("alertpush: list audience", "err", err)
			return
		}
		if len(page) == 0 {
			break
		}
		submitted, err := d.sendPage(ctx, p, page, catalog)
		if err != nil {
			d.recordAttempt(ctx, log, p.ID, err)
			return
		}
		last := page[len(page)-1].ID
		ok, err := d.Repo.AdvanceCursor(ctx, p.ID, cursor, last, submitted, d.Now())
		if err != nil {
			log.Warn("alertpush: advance cursor", "err", err)
			return
		}
		if !ok {
			log.Info("alertpush: push no longer ours (advanced elsewhere or canceled); yielding")
			return
		}
		cursor = last
	}
	d.complete(ctx, log, p.ID, StatusSent, "")
}

// groupKey is one gorush batch's identity within a page (spec §4).
type groupKey struct {
	platform push.Platform
	locale   string
	sandbox  bool
}

// sendPage sends one audience page as one batch per (platform, locale,
// sandbox) group and returns how many tokens gorush accepted. A transport
// error aborts the page; groups already sent in it are re-sent on resume
// (a bounded duplicate, design spec §2.6).
func (d *Dispatcher) sendPage(ctx context.Context, p Push, page []pushreg.Registration, catalog []string) (int64, error) {
	groups := make(map[groupKey][]string)
	var order []groupKey // deterministic send order for logs and tests
	for _, reg := range page {
		platform := push.PlatformIOS
		if reg.OperatingSystem == pushreg.OSAndroid {
			platform = push.PlatformAndroid
		}
		k := groupKey{platform: platform, locale: pushreg.NormalizeLocale(reg.Locale, catalog), sandbox: reg.APNSSandbox && platform == push.PlatformIOS}
		if _, seen := groups[k]; !seen {
			order = append(order, k)
		}
		groups[k] = append(groups[k], reg.Token)
	}

	var submitted int64
	notifID := NotifID(p.ID)
	for _, k := range order {
		tokens := groups[k]
		msg := p.Messages.For(k.locale)
		res, err := d.Sender.SendBatch(ctx, push.Notification{
			Tokens: tokens, Platform: k.platform, Sandbox: k.sandbox,
			Title: msg.Title, Message: msg.Body,
		}, notifID)
		if err != nil {
			return submitted, fmt.Errorf("send batch (%d tokens): %w", len(tokens), err)
		}
		for _, rej := range res.Rejected {
			if _, err := d.Repo.RecordFailure(ctx, p.ID, rej.Token, rej.Reason, d.Now()); err != nil {
				d.Logger.Warn("alertpush: record inline rejection", "push_id", p.ID, "err", err)
			}
		}
		submitted += int64(len(tokens) - len(res.Rejected))
	}
	return submitted, nil
}

func (d *Dispatcher) recordAttempt(ctx context.Context, log *slog.Logger, id int64, sendErr error) {
	attempts, err := d.Repo.RecordAttempt(ctx, id, sendErr.Error(), d.Now())
	if err != nil {
		log.Error("alertpush: record attempt", "err", err)
		return
	}
	if attempts >= MaxAttempts {
		log.Error("alertpush: giving up", "attempts", attempts, "err", sendErr)
		d.complete(ctx, log, id, StatusFailed, sendErr.Error())
		return
	}
	log.Warn("alertpush: send failed; will resume from cursor", "attempts", attempts, "err", sendErr)
}

func (d *Dispatcher) complete(ctx context.Context, log *slog.Logger, id int64, status Status, lastError string) {
	ok, err := d.Repo.MarkCompleted(ctx, id, status, lastError, d.Now())
	if err != nil {
		log.Error("alertpush: mark completed", "status", status, "err", err)
		return
	}
	if ok {
		log.Info("alertpush: completed", "status", status)
	}
}
```

Note the `RecordFailure` error path in `sendPage` never logs the token — the adapter's error strings only name the push id (Task 4 comment).

- [ ] **Step 4: Run, mutate, restore**

Run: `go test ./internal/alertpush/... -race -v`. Mutations: (a) start `cursor := 0` instead of `p.BatchCursor` — `ResumesFromCursor` must fail on "resumed page size"; (b) drop the `sandbox` key component — the grouping test must fail. Restore both.

- [ ] **Step 5: Commit** — `git add internal/alertpush && git commit -m "alertpush: resumable fan-out dispatcher"`

---

### Task 7: Admin API routes + feedback webhook correlation

**Files:**
- Create: `internal/httpapi/admin_pushes.go`
- Modify: `internal/httpapi/router.go` (`Deps.AlertPushes`, `Deps.AlertPushWaker`, four `adminRoutes` entries, boot guard)
- Modify: `internal/httpapi/feedback.go` (`NotifID` field; record failure before the terminal check)
- Test: `internal/httpapi/admin_pushes_test.go`, additions to `internal/httpapi/feedback_test.go`

**Interfaces:**
- Consumes: `alertpush.Enqueuer`, `alertpush.Repository`, `alertpush.Waker`, `alertpush.ParseNotifID`.
- Produces: `Deps.AlertPushes alertpush.Repository`, `Deps.AlertPushWaker alertpush.Waker`; routes from design spec §2.9.

- [ ] **Step 1: Write the failing tests**

`internal/httpapi/admin_pushes_test.go` (package `httpapi`, reusing `newAdminFixture`; extend the fixture so `NewRouter` also receives `PushRegs: store.PushRegs()`, `AlertPushes: store.AlertPushes()`, and `AlertPushWaker: f.waker` where `f.waker` is a `*recordingWaker{calls int}` — since `PushRegs` is now set, the fixture must also supply `Now` and `Regions`, which it already does; check that no existing admin test asserts on the exact route table length or on `PushRegs` being nil, and update it if so). Helpers you need from the existing test file: `f.do(method, path, body)`-style request helper — read `admin_alerts_test.go` and reuse whatever it defines (do not duplicate). Tests:

```go
type recordingWaker struct{ calls int }

func (w *recordingWaker) Wake() { w.calls++ }

// seedPublishedAlert creates and publishes an alert in region 1 via the store.
// seedRegistration upserts a registration (test=true marks a test device).

func TestAdminCreatePushQueuesAndWakes(t *testing.T) {
	f := newAdminFixture(t)
	id := f.seedPublishedAlert(t, regionPuget, false)
	f.seedRegistration(t, regionPuget, "tok-1", false)
	res := f.do(t, http.MethodPost, fmt.Sprintf("/api/admin/v1/alerts/%d/pushes", id), `{"audience":"all"}`)
	if res.Code != http.StatusAccepted {
		t.Fatalf("status = %d body %s", res.Code, res.Body)
	}
	var got map[string]any
	_ = json.Unmarshal(res.Body.Bytes(), &got)
	if got["status"] != "queued" || got["audience"] != "all" || got["device_count"] != float64(0) {
		t.Errorf("body = %v", got)
	}
	if _, ok := got["messages"].(map[string]any)["en"]; !ok {
		t.Errorf("messages lacks en: %v", got["messages"])
	}
	if f.waker.calls != 1 {
		t.Errorf("Wake calls = %d, want 1", f.waker.calls)
	}
}

func TestAdminCreatePushPreconditions(t *testing.T) {
	f := newAdminFixture(t)
	draft := f.seedAlert(t, regionPuget, false)            // unpublished
	published := f.seedPublishedAlert(t, regionPuget, false)
	testAlert := f.seedPublishedAlert(t, regionPuget, true)
	emptyRegionAlert := f.seedPublishedAlert(t, regionTampa, false)
	f.seedRegistration(t, regionPuget, "tok-1", false)
	f.seedRegistration(t, regionPuget, "qa", true)

	cases := []struct {
		name string; alertID int64; body string; want int
	}{
		{"unpublished", draft, `{}`, http.StatusConflict},
		{"unknown alert", 9999, `{}`, http.StatusNotFound},
		{"bad audience", published, `{"audience":"everyone"}`, http.StatusBadRequest},
		{"empty audience", emptyRegionAlert, `{}`, http.StatusConflict}, // a published alert in regionTampa, which has no registrations
	}
	for _, c := range cases {
		res := f.do(t, http.MethodPost, fmt.Sprintf("/api/admin/v1/alerts/%d/pushes", c.alertID), c.body)
		if res.Code != c.want {
			t.Errorf("%s: status = %d, want %d (%s)", c.name, res.Code, c.want, res.Body)
		}
	}
	// Forced test audience.
	res := f.do(t, http.MethodPost, fmt.Sprintf("/api/admin/v1/alerts/%d/pushes", testAlert), `{"audience":"all"}`)
	if res.Code != http.StatusAccepted || !strings.Contains(res.Body.String(), `"audience":"test"`) {
		t.Errorf("test alert: status %d body %s", res.Code, res.Body)
	}
	// In flight.
	if res := f.do(t, http.MethodPost, fmt.Sprintf("/api/admin/v1/alerts/%d/pushes", published), `{}`); res.Code != http.StatusAccepted {
		t.Fatalf("first: %d", res.Code)
	}
	if res := f.do(t, http.MethodPost, fmt.Sprintf("/api/admin/v1/alerts/%d/pushes", published), `{}`); res.Code != http.StatusConflict {
		t.Errorf("second: %d, want 409", res.Code)
	}
}

func TestAdminListCancelAndAudience(t *testing.T) {
	f := newAdminFixture(t)
	id := f.seedPublishedAlert(t, regionPuget, false)
	other := f.seedPublishedAlert(t, regionPuget, false)
	f.seedRegistration(t, regionPuget, "tok-1", false)
	f.seedRegistration(t, regionPuget, "qa", true)

	aud := f.do(t, http.MethodGet, fmt.Sprintf("/api/admin/v1/alerts/%d/push_audience", id), "")
	if aud.Code != http.StatusOK || !strings.Contains(aud.Body.String(), `"all":{"total":2,"ios":2,"android":0}`) || !strings.Contains(aud.Body.String(), `"forced_test":false`) {
		t.Errorf("audience: %d %s", aud.Code, aud.Body)
	}

	created := f.do(t, http.MethodPost, fmt.Sprintf("/api/admin/v1/alerts/%d/pushes", id), `{}`)
	var p struct{ ID int64 `json:"id"` }
	_ = json.Unmarshal(created.Body.Bytes(), &p)

	list := f.do(t, http.MethodGet, fmt.Sprintf("/api/admin/v1/alerts/%d/pushes", id), "")
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), fmt.Sprintf(`"id":%d`, p.ID)) {
		t.Errorf("list: %d %s", list.Code, list.Body)
	}
	if res := f.do(t, http.MethodGet, fmt.Sprintf("/api/admin/v1/alerts/%d/pushes", other), ""); res.Body.String() != "[]\n" && res.Body.String() != "[]" {
		t.Errorf("other alert's list = %s, want []", res.Body)
	}
	// Cancel via the wrong alert → 404; via the right one → 204; again → 409.
	if res := f.do(t, http.MethodDelete, fmt.Sprintf("/api/admin/v1/alerts/%d/pushes/%d", other, p.ID), ""); res.Code != http.StatusNotFound {
		t.Errorf("cross-alert cancel: %d, want 404", res.Code)
	}
	if res := f.do(t, http.MethodDelete, fmt.Sprintf("/api/admin/v1/alerts/%d/pushes/%d", id, p.ID), ""); res.Code != http.StatusNoContent {
		t.Errorf("cancel: %d, want 204 (%s)", res.Code, res.Body)
	}
	if res := f.do(t, http.MethodDelete, fmt.Sprintf("/api/admin/v1/alerts/%d/pushes/%d", id, p.ID), ""); res.Code != http.StatusConflict {
		t.Errorf("cancel twice: %d, want 409", res.Code)
	}
}

func TestAdminPushRoutesRequireSessionAndAreAbsentWithoutWaker(t *testing.T) {
	// Build one router WITH AlertPushes but WITHOUT AlertPushWaker (no
	// transport): every one of the four routes answers 404 even when
	// logged in, and adminRoutes(deps) does not list them. Build a second
	// router with both set: an anonymous request to each route is 401 and
	// adminRoutes(deps) lists all four with requiresSession == true. Iterate
	// the four (method, path) pairs from design spec §2.9 in both halves.
}
```

Feedback tests (append to `feedback_test.go`, package `httpapi_test`; build `Deps` with `PushRegs`, `AlertPushes: store.AlertPushes()`, `Regions`, `Now`):

```go
func TestFeedbackRecordsAlertPushFailureByNotifID(t *testing.T) {
	// seed region 1, published alert, registration "tok", enqueue a push p
	// via alertpush.Enqueuer; then:
	body := fmt.Sprintf(`{"type":"failed-push","platform":"ios","token":"tok","error":"Unregistered","notif_id":%q}`, alertpush.NotifID(p.ID))
	res := feedbackRequest(t, h, body)
	if res.Code != http.StatusOK { ... }
	got, _ := store.AlertPushes().Get(ctx, p.ID)
	if got.FailedCount != 1 { t.Errorf(...) }
	// Terminal reason still deleted the registration:
	if _, err := store.PushRegs().Get(ctx, 1, "tok"); !errors.Is(err, pushreg.ErrNotFound) { ... }
	// Replay is not double counted:
	feedbackRequest(t, h, body)
	got, _ = store.AlertPushes().Get(ctx, p.ID)
	if got.FailedCount != 1 { ... }
	// Non-terminal reason counts but does not delete:
	// register "tok2"; send {"token":"tok2","error":"TooManyRequests","notif_id":...} → FailedCount 2, tok2 still registered.
	// Unknown notif_id ("alertpush:999999") → 200, nothing recorded, no 500.
}
```

- [ ] **Step 2: Run to verify failure** — compile errors for `Deps.AlertPushes`.

- [ ] **Step 3: Implement**

`router.go` — `Deps` gets:

```go
	// AlertPushes backs the feedback webhook's alert-push failure accounting
	// (design spec §2.8) and, together with AlertPushWaker, the admin
	// alert-push routes (§2.9). main always sets it; the webhook must keep
	// accounting even when no transport is configured.
	AlertPushes alertpush.Repository
	// AlertPushWaker is the dispatcher, poked after every enqueue so a send
	// starts at once rather than at the next tick. main sets it only when a
	// push transport is configured, so it doubles as the "pushes can be
	// sent" signal: the admin routes are registered only when both this and
	// AlertPushes are non-nil, and the SPA shows "not configured" instead of
	// letting an operator queue a push that can only fail.
	AlertPushWaker alertpush.Waker
```

In `adminRoutes`, after the translation routes:

```go
		{"POST /api/admin/v1/alerts/{id}/pushes", pushesAdmin.create, true},
		{"GET /api/admin/v1/alerts/{id}/pushes", pushesAdmin.list, true},
		{"DELETE /api/admin/v1/alerts/{id}/pushes/{pushId}", pushesAdmin.cancel, true},
		{"GET /api/admin/v1/alerts/{id}/push_audience", pushesAdmin.audience, true},
```

but only when `deps.AlertPushes != nil && deps.AlertPushWaker != nil` — build the slice conditionally (`routes = append(routes, …)`) so the table stays enumerable. In `NewRouter`'s `Auth` block add a guard: when both are set, `missingDeps{"Deps.PushRegs": deps.PushRegs == nil}` panics.

`admin_pushes.go`:

```go
package httpapi

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/OneBusAway/sidecar/internal/alertpush"
	"github.com/OneBusAway/sidecar/internal/alerts"
	"github.com/OneBusAway/sidecar/internal/pushreg"
)

// pushJSON is the admin wire shape of one alert push (design spec §2.9).
type pushJSON struct {
	ID             int64                 `json:"id"`
	AlertID        int64                 `json:"alert_id"`
	RegionID       int64                 `json:"region_id"`
	Audience       string                `json:"audience"`
	Status         string                `json:"status"`
	DeviceCount    int64                 `json:"device_count"`
	SubmittedCount int64                 `json:"submitted_count"`
	FailedCount    int64                 `json:"failed_count"`
	Attempts       int64                 `json:"attempts"`
	LastError      string                `json:"last_error"`
	Messages       alertpush.Messages    `json:"messages"`
	FailureReasons []failureReasonJSON   `json:"failure_reasons"`
	CreatedAt      string                `json:"created_at"`
	StartedAt      *string               `json:"started_at"`
	CompletedAt    *string               `json:"completed_at"`
}

type failureReasonJSON struct {
	Reason string `json:"reason"`
	Count  int64  `json:"count"`
}

type audienceCountJSON struct {
	Total   int64 `json:"total"`
	IOS     int64 `json:"ios"`
	Android int64 `json:"android"`
}

type audienceJSON struct {
	All        audienceCountJSON `json:"all"`
	Test       audienceCountJSON `json:"test"`
	ForcedTest bool              `json:"forced_test"`
}

type createPushRequest struct {
	Audience string `json:"audience"`
}

type adminPushesHandler struct{ deps Deps }

func (h *adminPushesHandler) enqueuer() *alertpush.Enqueuer {
	return &alertpush.Enqueuer{Repo: h.deps.AlertPushes, Alerts: h.deps.Alerts, PushRegs: h.deps.PushRegs}
}

func toPushJSON(p alertpush.Push) pushJSON { /* map fields; formatInstant for times; nil slices → empty slices so JSON is [] not null */ }
func toAudienceJSON(r alertpush.AudienceReport) audienceJSON { ... }

// create handles POST /alerts/{id}/pushes → 202.
func (h *adminPushesHandler) create(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)  // 400 on error
	var req createPushRequest
	// Empty body is allowed: decodeJSON on io.EOF → treat as {}. Check how
	// decodeJSON reports an empty body and special-case it.
	audience, err := alertpush.ParseAudience(req.Audience) // 400
	p, err := h.enqueuer().Enqueue(r.Context(), id, audience, h.deps.Now())
	if err != nil { h.enqueueError(w, err); return }
	h.deps.AlertPushWaker.Wake() // non-nil: the route only exists when it is set
	writeJSON(w, h.deps.Logger, http.StatusAccepted, toPushJSON(p))
}

// enqueueError maps Enqueuer errors: alerts.ErrNotFound → 404,
// ErrNotPublished/ErrInFlight/ErrEmptyAudience → 409, else 500.
```

`list` (`ListByAlert` → 200 array; verify the alert exists first via `h.deps.Alerts.Get` so an unknown id is 404 not `[]`), `cancel` (parse `pushId` with `strconv.ParseInt(r.PathValue("pushId"))` → 400; `Get` → 404 if absent or `p.AlertID != id`; `Cancel` → `ErrTerminal` 409, else 204), `audience` (`AudienceFor` → 404 on `alerts.ErrNotFound`, else 200).

`feedback.go`: add `NotifID string \`json:"notif_id"\`` to `gorushFeedback`; after decoding and **before** the `IsTerminal` early return (most bounces are non-terminal and every one counts — design spec §2.8):

```go
	if fb.Token != "" && h.deps.AlertPushes != nil {
		if pushID, ok := alertpush.ParseNotifID(fb.NotifID); ok {
			// Every failure counts against the push, terminal or not
			// (design spec §2.8); an unknown push id is a stale or foreign
			// notif_id and is ignored (RecordFailure's FK error is logged
			// at Info, not returned as 500 -- gorush does not retry).
			if _, err := h.deps.AlertPushes.RecordFailure(r.Context(), pushID, fb.Token, fb.Error, h.deps.Now()); err != nil {
				h.deps.Logger.Info("httpapi: alert push feedback not recorded", "push_id", pushID, "err", sanitizeToken(err, fb.Token))
			}
		}
	}
```

then the existing `if fb.Token == "" || !push.IsTerminal(fb.Error) { 200 }` logic continues unchanged.

- [ ] **Step 4: Run, mutate, restore** — `go test ./internal/httpapi/...`. Also run the existing route-table tests (`router_test.go`) to confirm the new routes are asserted as session-wrapped. Mutations: (a) swap 409→200 for `ErrInFlight` → preconditions test fails; (b) move the `RecordFailure` block below the `IsTerminal` return → the non-terminal feedback assertion fails.

- [ ] **Step 5: Commit** — `git add internal/httpapi && git commit -m "httpapi: admin alert push routes; notif_id feedback accounting"`

---

### Task 8: Wire the dispatcher in `cmd/sidecar/main.go`

**Files:**
- Modify: `cmd/sidecar/main.go`
- Test: `cmd/sidecar/main_test.go` (there is an existing `TestBuildDeps_WiresFailDelay`; add one)

- [ ] **Step 1: Failing test**

```go
func TestBuildDepsWiresAlertPushWakerOnlyWithTransport(t *testing.T) {
	store := sqlitetest.Open(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	without := buildDeps(store, logger, "", "", "", nil)
	if without.AlertPushes == nil {
		t.Error("AlertPushes must always be set (the webhook keeps accounting without a transport)")
	}
	if without.AlertPushWaker != nil {
		t.Error("AlertPushWaker set without a transport")
	}
	d := &alertpush.Dispatcher{}
	with := buildDeps(store, logger, "", "", "", d)
	if with.AlertPushes == nil || with.AlertPushWaker != alertpush.Waker(d) {
		t.Error("AlertPushes/Waker not wired with a transport")
	}
}
```

(Adjust `buildDeps`'s signature: add `waker alertpush.Waker` as the last parameter; nil = no transport. Update its existing test call sites.)

- [ ] **Step 2: Implement**

In `run`, after the gorush block and before `buildDeps`:

```go
	// Alert push fan-out (spec §4, §12 row 3). The dispatcher always runs so
	// CLI-enqueued pushes are resolved (failed with a clear reason) even
	// with no transport; only the admin routes are gated on a transport.
	var batchSender push.BatchSender
	if sender != nil {
		batchSender = sender.(*push.Gorush)
	}
	dispatcher := &alertpush.Dispatcher{
		Repo: store.AlertPushes(), Alerts: store.Alerts(), PushRegs: store.PushRegs(),
		Sender: batchSender, Now: time.Now, Logger: logger,
	}
	go dispatcher.RunLoop(ctx, alertPushInterval)
```

(Cleaner: keep `var g *push.Gorush` from the existing block and pass `g` when non-nil, avoiding the type assertion.) Add `alertPushInterval = 15 * time.Second` to the const block with a comment. In `buildDeps`, always set `AlertPushes: store.AlertPushes()`; set `AlertPushWaker: waker` (pass `dispatcher` when `batchSender != nil`, else nil). Move `buildDeps` after the dispatcher is built.

- [ ] **Step 3: `go build ./... && go test ./cmd/...`; commit** — `git commit -am "sidecar: run the alert push dispatcher"`

---

### Task 9: `sidecar-admin alert push` / `alert pushes`

**Files:**
- Modify: `cmd/sidecar-admin/commands.go` (`runAlert` switch + two functions; update the "requires a subcommand" error text)
- Test: `cmd/sidecar-admin/commands_test.go`

- [ ] **Step 1: Failing tests**

```go
func TestAlertPushEnqueuesAndLists(t *testing.T) {
	dbPath, store := newDB(t)
	seedRegion(t, store.Regions(), 1)
	ctx := context.Background()
	if err := store.PushRegs().Upsert(ctx, pushreg.Upsert{RegionID: 1, Token: "tok", OperatingSystem: pushreg.OSIOS}, time.Now()); err != nil {
		t.Fatal(err)
	}
	out, _, err := cli(t, dbPath, "alert", "create", "--region", "1", "--agency-id", "1", "--header", "Hdr", "--start", "2026-01-01T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	id := parseCreatedID(t, out)

	if _, stderr, err := cli(t, dbPath, "alert", "push", strconv.FormatInt(id, 10)); err == nil || !strings.Contains(err.Error(), "not published") {
		t.Fatalf("push unpublished: err %v stderr %s", err, stderr)
	}
	if _, _, err := cli(t, dbPath, "alert", "publish", strconv.FormatInt(id, 10)); err != nil {
		t.Fatal(err)
	}
	out, _, err = cli(t, dbPath, "alert", "push", strconv.FormatInt(id, 10))
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if !strings.HasPrefix(out, "queued push 1 for alert") || !strings.Contains(out, "audience all") {
		t.Errorf("stdout = %q", out)
	}
	if _, _, err := cli(t, dbPath, "alert", "push", strconv.FormatInt(id, 10)); err == nil || !strings.Contains(err.Error(), "already queued") {
		t.Errorf("second push err = %v, want in-flight error", err)
	}
	if _, _, err := cli(t, dbPath, "alert", "push", strconv.FormatInt(id, 10), "--audience", "test"); err == nil || !strings.Contains(err.Error(), "already queued") {
		t.Errorf("in-flight check must run before the audience check: %v", err)
	}
	out, _, err = cli(t, dbPath, "alert", "pushes", strconv.FormatInt(id, 10))
	if err != nil {
		t.Fatalf("pushes: %v", err)
	}
	if !strings.Contains(out, "queued") || !strings.Contains(out, "all") {
		t.Errorf("pushes stdout = %q", out)
	}
	if _, _, err := cli(t, dbPath, "alert", "push", "999"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("unknown alert err = %v", err)
	}
	if _, _, err := cli(t, dbPath, "alert", "push", strconv.FormatInt(id, 10), "--audience", "everyone"); err == nil {
		t.Error("bad audience accepted")
	}
}
```

- [ ] **Step 2: Implement**

In `runAlert`: `case "push": return alertPush(ctx, stdout, store, now, cmdArgs)`, `case "pushes": return alertPushes(ctx, stdout, store, cmdArgs)`; update the two subcommand-list strings.

```go
// alertPush queues a push of a published alert (design spec §2.11). The CLI
// never talks to the running server: the row is picked up by the server's
// dispatcher within one tick, which the success message says.
func alertPush(ctx context.Context, stdout io.Writer, store *sqlite.Store, now time.Time, args []string) error {
	fs := flag.NewFlagSet("alert push", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	audienceFlag := fs.String("audience", "all", "who receives it: all (every registered device) or test (admin-marked test devices); a test alert always uses test")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("alert push: %w", err)
	}
	id, err := parseAlertIDArg("alert push", fs.Args())
	if err != nil {
		return err
	}
	audience, err := alertpush.ParseAudience(*audienceFlag)
	if err != nil {
		return fmt.Errorf("alert push: %w", err)
	}
	enq := &alertpush.Enqueuer{Repo: store.AlertPushes(), Alerts: store.Alerts(), PushRegs: store.PushRegs()}
	p, err := enq.Enqueue(ctx, id, audience, now)
	if err != nil {
		return wrapAlertErr("alert push", id, err)
	}
	fmt.Fprintf(stdout, "queued push %d for alert %d (audience %s); the sidecar server sends it on its next dispatcher tick (within 15s), or marks it failed if no push transport is configured\n", p.ID, id, p.Audience)
	return nil
}

func alertPushes(ctx context.Context, stdout io.Writer, store *sqlite.Store, args []string) error {
	id, err := parseAlertIDArg("alert pushes", args)
	if err != nil {
		return err
	}
	if _, err := store.Alerts().Get(ctx, id); err != nil {
		return wrapAlertErr("alert pushes", id, err)
	}
	list, err := store.AlertPushes().ListByAlert(ctx, id)
	if err != nil {
		return fmt.Errorf("alert pushes %d: %w", id, err)
	}
	if len(list) == 0 {
		fmt.Fprintln(stdout, "no pushes")
		return nil
	}
	fmt.Fprintf(stdout, "%-6s %-9s %-8s %8s %9s %6s  %-20s  %s\n", "id", "status", "audience", "devices", "submitted", "failed", "created", "last error")
	for _, p := range list {
		fmt.Fprintf(stdout, "%-6d %-9s %-8s %8d %9d %6d  %-20s  %s\n", p.ID, p.Status, p.Audience, p.DeviceCount, p.SubmittedCount, p.FailedCount, p.CreatedAt.UTC().Format(time.RFC3339), p.LastError)
	}
	return nil
}
```

Note the flag parse before the positional id: `alert push 3 --audience test` works because Go's `flag` stops at the first non-flag — so parse with the id first: extract `args[0]` as the id when it does not start with `-`, then parse the rest. Match whatever convention `alertEdit` uses (read it) so `alert push <id> --audience test` and `alert push --audience test <id>` both work if `alertEdit` supports both; otherwise document the supported order in the flag usage.

- [ ] **Step 3: Run, mutate, restore; commit** — `git commit -am "sidecar-admin: alert push / alert pushes"`

---

### Task 10: Admin SPA push card

**Files:**
- Modify: `web/admin/src/lib/types.ts` (add `AlertPush`, `AudienceCount`, `PushAudience`)
- Create: `web/admin/src/lib/pushes.ts`, `web/admin/src/lib/pushes.test.ts`
- Modify: `web/admin/src/routes/alerts/[id]/+page.ts`, `+page.svelte`

- [ ] **Step 1: Types**

```ts
export type PushStatus = 'queued' | 'sending' | 'sent' | 'failed' | 'canceled';
export type PushAudienceKind = 'all' | 'test';
export interface PushMessage { title: string; body: string }
export interface AlertPush {
	id: number; alert_id: number; region_id: number;
	audience: PushAudienceKind; status: PushStatus;
	device_count: number; submitted_count: number; failed_count: number; attempts: number;
	last_error: string;
	messages: Record<string, PushMessage>;
	failure_reasons: { reason: string; count: number }[];
	created_at: string; started_at: string | null; completed_at: string | null;
}
export interface AudienceCount { total: number; ios: number; android: number }
export interface PushAudience { all: AudienceCount; test: AudienceCount; forced_test: boolean }
```

- [ ] **Step 2: Failing vitest for `lib/pushes.ts`**

```ts
import { describe, expect, it } from 'vitest';
import { audienceOptions, isInFlight, progressLabel, sendConfirmMessage, statusTone } from './pushes';

const audience = { all: { total: 1200, ios: 900, android: 300 }, test: { total: 3, ios: 3, android: 0 }, forced_test: false };

describe('audienceOptions', () => {
	it('offers both audiences for a normal alert', () => {
		expect(audienceOptions(audience).map((o) => o.value)).toEqual(['all', 'test']);
		expect(audienceOptions(audience)[0].label).toBe('Everyone (1,200 devices)');
	});
	it('offers only test devices when forced', () => {
		const opts = audienceOptions({ ...audience, forced_test: true });
		expect(opts).toHaveLength(1);
		expect(opts[0].value).toBe('test');
		expect(opts[0].label).toBe('Test devices (3)');
	});
});

describe('isInFlight', () => {
	it('is true for queued and sending only', () => {
		expect(isInFlight({ status: 'queued' })).toBe(true);
		expect(isInFlight({ status: 'sending' })).toBe(true);
		for (const s of ['sent', 'failed', 'canceled'] as const) expect(isInFlight({ status: s })).toBe(false);
	});
});

describe('progressLabel', () => {
	it('reports submitted/failed of devices', () => {
		expect(progressLabel({ device_count: 1200, submitted_count: 500, failed_count: 2 })).toBe('500 sent · 2 failed · of 1,200');
	});
	it('says pending while the count is unknown', () => {
		expect(progressLabel({ device_count: 0, submitted_count: 0, failed_count: 0 })).toBe('pending');
	});
});

describe('sendConfirmMessage', () => {
	it('names the audience size', () => {
		expect(sendConfirmMessage('all', audience)).toBe('Send this alert as a push notification to 1,200 devices?');
		expect(sendConfirmMessage('test', audience)).toBe('Send this alert as a push notification to 3 test devices?');
	});
});

describe('statusTone', () => {
	it('maps statuses to badge tones', () => {
		expect(statusTone('sent')).toBe('published');
		expect(statusTone('failed')).toBe('test');
		expect(statusTone('queued')).toBe('draft');
	});
});
```

- [ ] **Step 3: Implement `lib/pushes.ts`** — pure functions matching the tests exactly (`Intl.NumberFormat('en-US')` for the thousands separator; `statusTone` reuses the existing `Badge['tone']` union so the existing `.badge-*` CSS applies: sent→`published`, failed/canceled→`test`, queued/sending→`draft`).

- [ ] **Step 4: `+page.ts`** — extend the load:

```ts
const [alert, regions, pushes, audience] = await Promise.all([
	api.get<Alert>(`/alerts/${params.id}`),
	api.get<Region[]>('/regions'),
	loadPushes(params.id),
	loadAudience(params.id),
]);
return { alert, regions, pushes, audience };
```

with `loadAudience` returning `null` on an `ApiError` whose status is 404 (routes not registered) and `loadPushes` returning `[]` in the same case; any other error rethrows. Put both helpers in `lib/pushes.ts` too (they only depend on `api`), with a test that a 404 yields `null`/`[]` and a 500 rethrows (mock `api.get` with `vi.spyOn`).

- [ ] **Step 5: `+page.svelte`** — add a `<section class="card">` "Push notification" after the publish controls:

- `data.audience === null` → paragraph "Push notifications are not configured on this server (no gorush URL)."
- `progressLabel` must tolerate `submitted_count > device_count` (a resumed page's bounded duplicate) — it just prints the numbers.
- `!alert.published` → paragraph "Publish the alert to send it as a push notification." and disabled button.
- Otherwise: radio group from `audienceOptions(data.audience)` bound to `let pushAudience = $state<'all'|'test'>(...)` (default `'test'` when forced, else `'all'`); Send button (`disabled={busy || inFlight}`) calling:

```ts
async function sendPush() {
	if (!data.audience || !confirm(sendConfirmMessage(pushAudience, data.audience))) return;
	busy = true; pushError = '';
	try {
		await api.post(`/alerts/${alert.id}/pushes`, { audience: pushAudience });
		await reload();
	} catch (err) { pushError = message(err, 'could not queue the push'); }
	finally { busy = false; }
}
async function cancelPush(id: number) { /* api.del(`/alerts/${alert.id}/pushes/${id}`); await reload(); */ }
```

- History table over `data.pushes`: `#id`, badge (`statusTone`), audience, `progressLabel(p)`, `formatInstantForRegion(p.created_at, zone)`, `p.last_error`, and a Cancel button when `isInFlight(p)`.
- Polling: `$effect(() => { if (!data.pushes.some(isInFlight)) return; const t = setInterval(() => void invalidateAll(), 3000); return () => clearInterval(t); })`.

Run `npm run check && npm run lint && npm run test:unit` in `web/admin`; fix formatting with `npx prettier --write`. Then `make web` and `go test ./internal/httpapi/adminui`.

- [ ] **Step 6: Commit** — `git add web/admin && git commit -m "admin ui: alert push card"`

---

### Task 11: Docs + full check

**Files:**
- Modify: `README.md` (new "Sending alerts as push notifications" subsection under *Service alerts*, after the CLI section; add `alert push` / `alert pushes` to the CLI listing), `CLAUDE.md` (add `alertpush` to the domain package list and "alert push dispatcher" to the background loops sentence)

- [ ] **Step 1: README content** — document: the four admin routes with their status codes (202/409/422/404); audiences and the test-alert rule; copy derivation (header/description, 48/120 rune clamps, fresh translations only, locale normalization); the CLI commands and that the server sends within ~15s; what `device_count`/`submitted_count`/`failed_count` mean and that gorush's default async mode reports failures only via the webhook (`notif_id` correlation), while `core.sync` rejections are counted inline; resumability and the 15-minute stuck reclaim; cancel semantics; that the admin routes are only registered when `SIDECAR_GORUSH_URL` is set.

- [ ] **Step 2: `make check`** — everything green (`fmt-check vet lint generate-check test test-tz test-race web-check`). Fix anything it reports.

- [ ] **Step 3: Commit** — `git commit -am "docs: alert push fan-out"`

---

## Self-review

- **Spec coverage:** §2.1 (Task 4/7), §2.2 (Task 5, index in Task 2), §2.3 (Task 2), §2.4 (Task 3), §2.5 (Task 1), §2.6/§2.7 (Task 6), §2.8 (Tasks 4, 6, 7), §2.9 (Task 7), §2.10 (Task 10), §2.11 (Task 9), §2.12 (Task 8), §3 (Tasks 2, 4), §4 (each task's tests), §5 (Task 11).
- **Type consistency:** `Repository` method names in Task 3 match their use in Tasks 4–7 (`Claim`, `AdvanceCursor`, `RecordFailure`, `RecordAttempt`, `MarkCompleted`, `Cancel`, `InFlightForAlert`, `SetDeviceCount`, `ListByAlert`); `push.BatchSender.SendBatch(ctx, n, notifID)` in Tasks 1 and 6; `pushreg.ListAudience(ctx, regionID, testOnly, afterID, limit)` in Tasks 2 and 6; `Enqueuer{Repo, Alerts, PushRegs}` in Tasks 5, 7, 9.
- **Placeholders:** Task 4's `MarkCompletedViaClaim` is explicitly flagged for replacement; Task 7's `TestAdminPushRoutesRequireSessionAndAreAbsentWithoutRepo` body and the feedback test are described in prose with concrete assertions — the implementer writes them to that description.
