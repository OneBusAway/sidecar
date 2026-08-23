# Push Notification Registrations and Alarms Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement spec §4 (push notification registrations) and §5 (departure alarms, V1+V2, with the minute-cadence firing loop) for the Go sidecar.

**Architecture:** Three new domain packages (`pushreg`, `alarms`, `push`) plus two utility packages (`securetoken`, `ratelimit`), following the existing repository pattern: domain types + `Repository` interface in the domain package, sqlc-backed adapter in `internal/store/sqlite`, conformance suite in `internal/store/storetest`, stdlib handlers in `internal/httpapi`, background loops started from `cmd/sidecar`. Push transport is the spec-blessed gorush gateway behind a `push.Sender` interface (one HTTP POST, zero new Go dependencies); its async feedback webhook prunes dead alert tokens.

**Tech Stack:** Go 1.26 stdlib HTTP, sqlc + goose on modernc SQLite, `github.com/OneBusAway/go-sdk` (via `internal/obaapi` only), gorush over plain HTTP JSON.

**Spec:** `specification/specification.md` §2 (conventions), §4, §5, §12, §13; `specification/openapi.yaml` (paths `/api/v{1,2}/regions/{regionId}/alarms*`, `/api/v2/regions/{regionId}/push_registrations`, webhook `alarmPush`, schemas `AlarmCreateRequestV1/V2`, `PushRegistrationRequest`, `ApnsSandboxParam`, `ErrorWithMessages`).

## Global Constraints

- `time.Now`/`time.Sleep` are banned outside `cmd/` — every package takes `Now func() time.Time` (or a `time.Time` argument) injected. Storetest derives all times from a fixed `base` instant.
- Nothing outside `internal/store/sqlite` may see a `gen.*` type; nothing outside `internal/obaapi` may import the OBA go-sdk.
- **Never log a push token or `user_push_id`** — they are device-addressable secrets (spec §13). Log counts, region ids, and platforms instead. Upstream errors from obaapi are already redacted; keep gorush errors to status codes only (a gorush error body can echo tokens).
- Error shapes (spec §2.5): unknown region → `404 {"error": "Couldn't find Region"}` (use existing `writeRegionNotFound`); validation → `422 {"error": "<summary>", "messages": [...]}`; DELETE → bodyless `204` only **after** the row is actually gone, bodyless `404` for an unknown token, `5xx` if the delete fails.
- Every POST accepts both `application/x-www-form-urlencoded`/query params **and** `application/json` bodies (spec §2.2).
- `apns_sandbox` parsing is a strict allow-list (spec §2.7): truthy `1|t|true|on` (case-insensitive, trimmed); falsy `0|f|false|off|empty|absent`; **anything else falsy + logged**. Ignored/cleared for Android.
- Region path segments accept `{int}` or `{int}-slug` (existing `ParseRegionSegment`).
- Secure tokens: 22-char URL-safe base64 of 128 random bits (spec §2.4). Never expose sequential ids.
- Timestamp columns are INTEGER (epoch seconds via `.Unix()`), like the existing schema — never DATETIME (modernc writes `time.Time.String()` into DATETIME cells and ORDER BY silently sorts text). `service_date` stays epoch **milliseconds** as received.
- sqlc queries use named args (`@name` / `sqlc.arg`) consistently — never mix named args with bare `?` (mixing silently misnumbers every parameter at runtime).
- Commands: `make generate` (sqlc), `go test ./internal/... ./cmd/...` (fast loop), `make check` (full CI parity). Commit after every green task.
- Follow house comment style: comments state constraints and rationale ("why"), not narration.

## File Structure

| File | Responsibility |
|---|---|
| `internal/securetoken/securetoken.go` | Unguessable resource tokens (§2.4) |
| `internal/ratelimit/ratelimit.go` | Fixed-window per-key counter for the §2.6 throttle |
| `internal/pushreg/pushreg.go` | Registration domain type, `Repository` interface, `ErrNotFound` |
| `internal/pushreg/locale.go` | `NormalizeLocale` (§4 locale normalization, pure function) |
| `internal/pushreg/prune.go` | `RunPruneLoop` (180-day reaper, §4 retention / §12) |
| `internal/alarms/alarms.go` | Alarm domain types, `Repository`, message composition, push data payload |
| `internal/alarms/scheduler.go` | Minute-cadence firing loop (§5.3) |
| `internal/push/push.go` | `Notification`, `Sender` interface, `IsTerminal` |
| `internal/push/gorush.go` | gorush HTTP transport |
| `internal/obaapi/obaapi.go` | + `ArrivalAndDeparture` method, `ErrNotFound` |
| `internal/store/sqlite/migrations/00004_push_registrations.sql` | push_registrations table |
| `internal/store/sqlite/migrations/00005_alarms.sql` | alarms table + V1 dedupe index |
| `internal/store/sqlite/queries/pushregs.sql`, `queries/alarms.sql` | sqlc queries |
| `internal/store/sqlite/pushregs.go`, `sqlite/alarms.go` | adapters mapping gen.* ↔ domain |
| `internal/store/storetest/pushregtest.go`, `storetest/alarmtest.go` | conformance suites |
| `internal/httpapi/params.go` | dual-encoding params bag + `parseAPNSSandbox` |
| `internal/httpapi/pushregs.go` | POST/DELETE push_registrations + throttle middleware |
| `internal/httpapi/alarms.go` | POST/DELETE alarms V1+V2 |
| `internal/httpapi/feedback.go` | gorush failed-push webhook receiver |
| `internal/httpapi/router.go` | route + Deps wiring |
| `cmd/sidecar/main.go` | flags, sender construction, background loops |
| `README.md` | new env vars, gorush + proxy deployment notes |

**Explicitly out of scope** (note in the PR description): alert-push fan-out (§4 "What gets pushed") — deferred to a follow-up hooked on the existing admin publish action (`POST /api/admin/v1/alerts/{id}/publish`); the audience registry, locale normalization function, and per-environment grouping data built here are its prerequisites. Live Activities (§6), surveys (§7), ghost bus reports (§8) are separate spec sections. V1 alarms are implemented as the spec-sanctioned "thin alias of V2 with the dedupe kept" — no OneSignal transport.

---

### Task 1: Secure token generator

**Files:**
- Create: `internal/securetoken/securetoken.go`
- Test: `internal/securetoken/securetoken_test.go`

**Interfaces:**
- Produces: `securetoken.New() (string, error)` — 22-char URL-safe base64 of 128 random bits. Consumed by the alarm create handler (Task 11) and, later, Live Activities.

- [ ] **Step 1: Write the failing test**

```go
package securetoken_test

import (
	"regexp"
	"testing"

	"github.com/OneBusAway/sidecar/internal/securetoken"
)

func TestNew(t *testing.T) {
	t.Parallel()

	urlSafe := regexp.MustCompile(`^[A-Za-z0-9_-]{22}$`)
	seen := make(map[string]bool)
	for range 1000 {
		tok, err := securetoken.New()
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if !urlSafe.MatchString(tok) {
			t.Fatalf("New() = %q; want 22 URL-safe base64 chars", tok)
		}
		if seen[tok] {
			t.Fatalf("New() repeated token %q", tok)
		}
		seen[tok] = true
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/securetoken/`
Expected: FAIL — package does not exist.

- [ ] **Step 3: Write minimal implementation**

```go
// Package securetoken mints the unguessable, URL-safe tokens that publicly
// address alarms and (later) Live Activities: 22 characters of raw URL-safe
// base64 encoding 128 random bits (spec §2.4). Possession of the token is
// the ownership proof for the resource, so the only requirements are
// crypto-strength randomness and URL safety.
package securetoken

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
)

// New returns a fresh 22-character token.
func New() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("securetoken: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/securetoken/`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/securetoken
git commit -m "feat: add securetoken package for spec §2.4 resource tokens"
```

---

### Task 2: Fixed-window rate limiter

**Files:**
- Create: `internal/ratelimit/ratelimit.go`
- Test: `internal/ratelimit/ratelimit_test.go`

**Interfaces:**
- Produces: `ratelimit.New(limit int, window time.Duration) *Limiter` and `(*Limiter).Allow(key string, now time.Time) bool`. Clockless: `now` is passed per call, keeping the package inside the time.Now ban. Safe for concurrent use. Consumed by the push-registration throttle (Task 6).

- [ ] **Step 1: Write the failing test**

```go
package ratelimit_test

import (
	"strconv"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/ratelimit"
)

var base = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func TestAllowEnforcesLimitPerWindow(t *testing.T) {
	t.Parallel()
	l := ratelimit.New(3, time.Minute)

	for i := range 3 {
		if !l.Allow("1.2.3.4", base) {
			t.Fatalf("request %d: denied inside limit", i)
		}
	}
	if l.Allow("1.2.3.4", base.Add(59*time.Second)) {
		t.Fatal("4th request in window allowed")
	}
	// Other keys have their own budget.
	if !l.Allow("5.6.7.8", base) {
		t.Fatal("different key denied")
	}
	// A new window resets the count.
	if !l.Allow("1.2.3.4", base.Add(time.Minute)) {
		t.Fatal("request in next window denied")
	}
}

func TestSweepBoundsMemory(t *testing.T) {
	t.Parallel()
	l := ratelimit.New(1, time.Minute)
	for i := range 4096 {
		l.Allow("key-"+strconv.Itoa(i), base)
	}
	// Two windows later every earlier bucket is stale; the next Allow
	// sweeps them. The sweep is time-gated to once per window so a flood of
	// distinct keys can never turn Allow into a per-request full-map scan.
	l.Allow("fresh", base.Add(2*time.Minute))
	if got := l.Len(); got > 1 {
		t.Fatalf("Len() = %d after sweep; want <= 1", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/ratelimit/`
Expected: FAIL — package does not exist.

- [ ] **Step 3: Write minimal implementation**

```go
// Package ratelimit is a fixed-window request counter for the spec §2.6
// throttles. Fixed windows (not sliding) match the reference implementation's
// rack-attack behavior, and the worst-case burst (2x limit straddling a
// window edge) is acceptable for an abuse brake. The limiter is clockless --
// callers pass now -- so it stays inside the repo-wide time.Now ban and
// tests are deterministic.
package ratelimit

import (
	"sync"
	"time"
)

type bucket struct {
	start time.Time
	count int
}

// Limiter counts requests per key in fixed windows. Safe for concurrent use.
type Limiter struct {
	limit  int
	window time.Duration

	mu        sync.Mutex
	buckets   map[string]*bucket
	lastSweep time.Time
}

// New builds a Limiter allowing limit requests per key per window.
func New(limit int, window time.Duration) *Limiter {
	return &Limiter{limit: limit, window: window, buckets: make(map[string]*bucket)}
}

// Allow reports whether key may make a request at now, counting it if so.
// A denied request is not counted -- the window budget measures successful
// admissions, so a hammering client does not extend its own lockout.
func (l *Limiter) Allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	// Sweep expired buckets at most once per window: unauthenticated
	// endpoints see arbitrary client IPs, so the map must not grow without
	// bound -- but a per-call scan would hand a flood of distinct IPs an
	// O(n) amplifier on every request. Time-gated, each window pays for one
	// scan; a bucket lives at most two windows past its last use.
	if now.Sub(l.lastSweep) >= l.window {
		for k, b := range l.buckets {
			if now.Sub(b.start) >= l.window {
				delete(l.buckets, k)
			}
		}
		l.lastSweep = now
	}

	b := l.buckets[key]
	if b == nil || now.Sub(b.start) >= l.window {
		l.buckets[key] = &bucket{start: now, count: 1}
		return true
	}
	if b.count >= l.limit {
		return false
	}
	b.count++
	return true
}

// Len reports the number of live buckets; test-only observability.
func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/ratelimit/`
Expected: PASS. Also run `go test -race ./internal/ratelimit/`.

- [ ] **Step 5: Commit**

```bash
git add internal/ratelimit
git commit -m "feat: add fixed-window rate limiter for spec §2.6 throttles"
```

---

### Task 3: pushreg domain package

**Files:**
- Create: `internal/pushreg/pushreg.go`, `internal/pushreg/locale.go`
- Test: `internal/pushreg/locale_test.go`

**Interfaces:**
- Produces (consumed by Tasks 4, 6, 11, 13, 14):

```go
package pushreg

var ErrNotFound = errors.New("push registration not found")

// OS values are the only two the API admits.
const (
	OSIOS     = "ios"
	OSAndroid = "android"
)

type Registration struct {
	RegionID        int64
	Token           string
	OperatingSystem string // OSIOS | OSAndroid
	Locale          string // raw BCP-47 tag as reported; "" = none
	APNSSandbox     bool
	TestDevice      bool
	Description     string
	LastSeenAt      time.Time
	CreatedAt       time.Time
}

// Upsert carries one registration write. Pointer fields implement the §4
// sticky semantics: nil = keep the stored value; non-nil = overwrite.
// OperatingSystem and APNSSandbox are deliberately non-sticky (always
// written): each registration states its own build's platform and APNs
// environment, and absent apns_sandbox means production (§2.7).
type Upsert struct {
	RegionID        int64
	Token           string
	OperatingSystem string
	APNSSandbox     bool
	Locale          *string
	TestDevice      *bool
	Description     *string
}

type Repository interface {
	// Upsert inserts or refreshes the (region, token) row, always updating
	// last_seen_at. Must be atomic under concurrent first registration.
	Upsert(ctx context.Context, in Upsert, now time.Time) error
	Get(ctx context.Context, regionID int64, token string) (Registration, error)
	// Delete removes one region's registration; ErrNotFound if absent.
	Delete(ctx context.Context, regionID int64, token string) error
	// DeleteByToken removes the token everywhere (terminal APNs feedback is
	// not region-scoped); returns rows removed.
	DeleteByToken(ctx context.Context, token string) (int64, error)
	// Prune deletes rows whose last_seen_at is before cutoff; returns count.
	Prune(ctx context.Context, cutoff time.Time) (int64, error)
}

// NormalizeLocale maps a reported BCP-47 tag onto catalog (§4): exact
// case-insensitive match, then known aliases, then bare primary subtag,
// else "" (English copy). Returns the catalog's own spelling.
func NormalizeLocale(tag string, catalog []string) string
```

**Design note recorded in the package doc:** registrations store the *raw* reported locale; `NormalizeLocale` is applied at fan-out time against whatever translation catalog then exists. The spec describes registration-time normalization, but the catalog in this implementation (languages present in `alert_translations`) is mutable, and normalizing against a snapshot would strand rows when translations are added. Storing raw + normalizing late is strictly more faithful to rider intent; the function itself matches the spec's algorithm exactly.

- [ ] **Step 1: Write the failing test**

```go
package pushreg_test

import (
	"testing"

	"github.com/OneBusAway/sidecar/internal/pushreg"
)

func TestNormalizeLocale(t *testing.T) {
	t.Parallel()

	catalog := []string{"es", "zh-Hans", "zh-Hant", "tl", "pt", "fr-CA"}
	tests := []struct {
		tag, want string
	}{
		{"es", "es"},
		{"ES", "es"},          // exact match is case-insensitive
		{"fr-ca", "fr-CA"},    // returns the catalog's spelling
		{"zh-CN", "zh-Hans"},  // alias
		{"zh-SG", "zh-Hans"},  // alias
		{"zh-TW", "zh-Hant"},  // alias
		{"zh-HK", "zh-Hant"},  // alias
		{"fil", "tl"},         // alias
		{"pt-BR", "pt"},       // alias
		{"es-MX", "es"},       // bare primary subtag
		{"fr-FR", ""},         // primary subtag fr not in catalog (only fr-CA)
		{"de", ""},            // no match at all -> English copy
		{"", ""},
		{"  es  ", "es"},      // trimmed
	}
	for _, tt := range tests {
		if got := pushreg.NormalizeLocale(tt.tag, catalog); got != tt.want {
			t.Errorf("NormalizeLocale(%q) = %q; want %q", tt.tag, got, tt.want)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/pushreg/`
Expected: FAIL — package does not exist.

- [ ] **Step 3: Write the implementation**

`pushreg.go` holds the types and interface exactly as in the Interfaces block (plus a package doc comment explaining the registry's role as the alert-push audience and the raw-locale design note). `locale.go`:

```go
package pushreg

import "strings"

// localeAliases are the spec §4 tag aliases, applied after an exact catalog
// match fails and before falling back to the bare primary subtag.
var localeAliases = map[string]string{
	"zh-cn": "zh-Hans", "zh-sg": "zh-Hans",
	"zh-tw": "zh-Hant", "zh-hk": "zh-Hant",
	"fil": "tl", "pt-br": "pt",
}

// NormalizeLocale maps a reported BCP-47 tag onto catalog: exact
// case-insensitive match, then aliases, then bare primary subtag, else ""
// (meaning English copy). The returned value is the catalog's own spelling,
// so callers can key translation lookups on it directly.
func NormalizeLocale(tag string, catalog []string) string {
	tag = strings.TrimSpace(tag)
	if tag == "" {
		return ""
	}
	match := func(want string) string {
		for _, c := range catalog {
			if strings.EqualFold(c, want) {
				return c
			}
		}
		return ""
	}
	if m := match(tag); m != "" {
		return m
	}
	if alias, ok := localeAliases[strings.ToLower(tag)]; ok {
		if m := match(alias); m != "" {
			return m
		}
	}
	if primary, _, found := strings.Cut(tag, "-"); found {
		if m := match(primary); m != "" {
			return m
		}
	}
	return ""
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/pushreg/` — PASS. `go vet ./internal/pushreg/` clean.

- [ ] **Step 5: Commit**

```bash
git add internal/pushreg
git commit -m "feat: add pushreg domain package with locale normalization"
```

---

### Task 4: push_registrations storage (migration + sqlc + adapter + conformance)

**Files:**
- Create: `internal/store/sqlite/migrations/00004_push_registrations.sql`
- Create: `internal/store/sqlite/queries/pushregs.sql`
- Create: `internal/store/sqlite/pushregs.go`
- Create: `internal/store/storetest/pushregtest.go`
- Modify: `internal/store/sqlite/store.go` (add `PushRegs()` accessor)
- Test: `internal/store/sqlite/store_test.go` (add conformance hookup)

**Interfaces:**
- Consumes: `pushreg.Repository`, `pushreg.Upsert`, `pushreg.Registration`, `pushreg.ErrNotFound` (Task 3); `regions.Repository` for fixtures.
- Produces: `(*sqlite.Store).PushRegs() pushreg.Repository`; `storetest.RunPushRegistrationRepository(t *testing.T, newStore func(*testing.T) (pushreg.Repository, regions.Repository))`.

- [ ] **Step 1: Write the migration**

```sql
-- +goose Up
CREATE TABLE push_registrations (
  id               INTEGER PRIMARY KEY AUTOINCREMENT,
  region_id        INTEGER NOT NULL REFERENCES regions(id) ON DELETE CASCADE,
  token            TEXT    NOT NULL,
  operating_system TEXT    NOT NULL CHECK (operating_system IN ('ios', 'android')),
  apns_sandbox     BOOLEAN NOT NULL DEFAULT FALSE,
  locale           TEXT    NOT NULL DEFAULT '',
  test_device      BOOLEAN NOT NULL DEFAULT FALSE,
  description      TEXT    NOT NULL DEFAULT '',
  last_seen_at     INTEGER NOT NULL,
  created_at       INTEGER NOT NULL,
  updated_at       INTEGER NOT NULL,
  UNIQUE (region_id, token)
);

-- The daily reaper scans by staleness alone (spec §4: 180 days unseen).
CREATE INDEX push_registrations_prune_idx ON push_registrations (last_seen_at);

-- +goose Down
DROP TABLE push_registrations;
```

- [ ] **Step 2: Write the sqlc queries**

`queries/pushregs.sql` — all named args, no bare `?`:

```sql
-- name: UpsertPushRegistration :exec
-- Sticky semantics (spec §4): locale, test_device, and description are only
-- overwritten when the request carried an actual value (the @set_* flags);
-- operating_system, apns_sandbox, and last_seen_at are always rewritten.
-- ON CONFLICT DO UPDATE is a single atomic statement, which is what
-- satisfies the concurrent-first-registration requirement without an
-- application-level retry.
INSERT INTO push_registrations (
  region_id, token, operating_system, apns_sandbox,
  locale, test_device, description,
  last_seen_at, created_at, updated_at
) VALUES (
  @region_id, @token, @operating_system, @apns_sandbox,
  @locale, @test_device, @description,
  @now, @now, @now
)
ON CONFLICT (region_id, token) DO UPDATE SET
  operating_system = excluded.operating_system,
  apns_sandbox     = excluded.apns_sandbox,
  locale           = CASE WHEN CAST(@set_locale AS BOOLEAN)      THEN excluded.locale      ELSE push_registrations.locale      END,
  test_device      = CASE WHEN CAST(@set_test_device AS BOOLEAN) THEN excluded.test_device ELSE push_registrations.test_device END,
  description      = CASE WHEN CAST(@set_description AS BOOLEAN) THEN excluded.description ELSE push_registrations.description END,
  last_seen_at     = excluded.last_seen_at,
  updated_at       = excluded.updated_at;

-- name: GetPushRegistration :one
SELECT * FROM push_registrations
WHERE region_id = @region_id AND token = @token;

-- name: DeletePushRegistration :execrows
DELETE FROM push_registrations
WHERE region_id = @region_id AND token = @token;

-- name: DeletePushRegistrationsByToken :execrows
DELETE FROM push_registrations WHERE token = @token;

-- name: PrunePushRegistrations :execrows
DELETE FROM push_registrations WHERE last_seen_at < @cutoff;
```

The `CAST(... AS BOOLEAN)` wrappers exist for sqlc: a bare named arg inside `CASE WHEN` on the SQLite engine can infer as `interface{}`; the cast pins the generated param to `bool`. If `make generate` still emits `interface{}` for any of them, adapt in the adapter rather than fighting the generator.

Run: `make generate` — gen code appears; `go build ./...` still green.

- [ ] **Step 3: Write the conformance suite (failing)**

`storetest/pushregtest.go`, mirroring `RunAlertRepository`'s shape (`t.Helper`, one subtest per behavior). **Reuse the package-level `base` instant already declared in `storetest.go` — redeclaring it is a compile error.** Behaviors to cover — each is a named subtest with full assertions:

- `UpsertInsertsAndGetRoundTrips`: upsert with all pointers set (`locale "es-MX"`, `test_device true`, `description "Aaron's iPhone"`), Get returns every field including `LastSeenAt.Equal(base)`.
- `ReRegistrationRefreshesLastSeen`: second upsert at `base.Add(time.Hour)` with all pointers nil → `LastSeenAt` advances, locale/test_device/description unchanged (sticky).
- `NilPointersKeepStoredValues` / `ExplicitFalseDemotesAndClearsDescription`: upsert `TestDevice: ptr(false), Description: ptr("")` → both cleared.
- `OperatingSystemAndSandboxAlwaysOverwritten`: re-upsert flips `ios/sandbox=true` → `android/sandbox=false`; stored row follows.
- `LocaleOverwrittenOnlyWhenSet`: `Locale: ptr("fr")` overwrites; nil keeps.
- `DescriptionOverwrittenOnlyWhenSet`: `Description: ptr("new")` with `TestDevice: nil` overwrites the stored description and keeps the stored test_device; nil keeps. (Each sticky field carries its own flag; the merged-row invariants — a test device must have a description, non-test rows carry none — are the **handler's** job, not the store's.)
- `DeleteReportsNotFound`: Delete unknown → `pushreg.ErrNotFound`; known → nil, then Get → `ErrNotFound`.
- `DeleteByTokenSpansRegions`: same token registered in regions 1 and 2 → `DeleteByToken` returns 2, both gone.
- `PruneRemovesOnlyStale`: rows at `base` and `base.Add(24h)`; `Prune(cutoff base.Add(1h))` → returns 1, fresh row survives.
- `RegionScoping`: same token in two regions are independent rows.
- `ConcurrentFirstRegistrationRaces`: 8 goroutines upsert the same new (region, token) concurrently; all return nil error; exactly one row exists.

Helper `ptr[T any](v T) *T` local to the file.

- [ ] **Step 4: Hook conformance into sqlite tests and run to verify it fails**

In `store_test.go` add:

```go
func TestPushRegistrationConformance(t *testing.T) {
	t.Parallel()
	storetest.RunPushRegistrationRepository(t, func(t *testing.T) (pushreg.Repository, regions.Repository) {
		s := sqlitetest.Open(t)
		return s.PushRegs(), s.Regions()
	})
}
```

Run: `go test ./internal/store/...` — FAIL (`PushRegs` undefined).

- [ ] **Step 5: Write the adapter**

`sqlite/pushregs.go`: `pushRegRepo{q *gen.Queries}`; `Store.PushRegs() pushreg.Repository`. Mapping rules:

- `Upsert`: `SetLocale: in.Locale != nil`, `Locale: deref(in.Locale, "")`, and the same independent flag/value pattern for `test_device` (`SetTestDevice`) and `description` (`SetDescription`) — the three sticky fields are orthogonal at this layer. `now.Unix()` for all three timestamps.
- `Get`: map row → `pushreg.Registration`, `time.Unix(row.LastSeenAt, 0).UTC()`; `sql.ErrNoRows` → `pushreg.ErrNotFound`.
- `Delete`: `:execrows` == 0 → `pushreg.ErrNotFound`.
- `DeleteByToken`, `Prune`: return the row counts directly.

- [ ] **Step 6: Run tests to verify pass**

Run: `go test ./internal/store/...` and `go test -race ./internal/store/...`
Expected: PASS, including the concurrency subtest.

- [ ] **Step 7: Commit**

```bash
git add internal/store specification 2>/dev/null; git add internal/store
git commit -m "feat: push_registrations storage with sticky upsert and conformance suite"
```

---

### Task 5: Dual-encoding params bag and apns_sandbox parser

**Files:**
- Create: `internal/httpapi/params.go`
- Test: `internal/httpapi/params_test.go`

**Interfaces:**
- Produces (consumed by Tasks 6 and 11) — unexported, tested via the export pattern the package already uses for `ParseRegionSegment` (add a `params_test.go` in-package, i.e. `package httpapi`, since these are internals):

```go
// parseRequestParams merges query-string, form-body, and JSON-body
// parameters into one bag, Rails-style (spec §2.2). JSON null values are
// dropped (spec §4: null counts as absent). Body reads are capped.
func parseRequestParams(w http.ResponseWriter, r *http.Request, maxBytes int64) (params, error)

type params struct{ m map[string]any }

// str returns the trimmed string form of the value: JSON strings verbatim,
// bools as "true"/"false", numbers in decimal. ok=false when absent.
func (p params) str(key string) (val string, ok bool)

// int64 parses the value as an integer; a non-numeric value is (0, false),
// never a fuzzy parse.
func (p params) int64(key string) (int64, bool)

// boolish applies the §2.7 allow-list to the value: truthy 1/t/true/on,
// falsy 0/f/false/off/"". present=false when the key is absent, the value
// is unrecognized, or it is blank -- so sticky fields keep stored state on
// garbage rather than flipping.
func (p params) boolish(key string) (val, present bool)

// parseAPNSSandbox reads apns_sandbox with the §2.7 strict allow-list:
// anything unrecognized is production (false) and logged.
func parseAPNSSandbox(p params, logger *slog.Logger) bool
```

- [ ] **Step 1: Write the failing tests**

`params_test.go` (`package httpapi`), table-driven:

- JSON body `{"token":"abc","n":7,"flag":true,"nil":null}` with `Content-Type: application/json` → `str("token")=("abc",true)`, `int64("n")=(7,true)`, `str("flag")=("true",true)`, `str("nil")=("",false)` (null dropped).
- Form body `token=abc&n=7` with form content type → same reads.
- Query params merge: `?a=1` + JSON body `{"b":2}` → both present; body wins on collision (`?a=1` + body `a=2` → `int64("a")=2`).
- **DELETE with a form body** `token=abc` and no query → `str("token")=("abc",true)`. (net/http's `ParseForm` never reads a DELETE body, which is why the implementation reads the body itself — spec §4's opt-out accepts "query or body parameter".)
- DELETE with `?token=abc` and an **empty body**, once with no Content-Type and once with `application/json` → token present; an empty body is an empty param set for both branches, never a decode error.
- `int64` on `"12x"`, `"2026-08-20"`, `""` → `(0,false)` (no fuzzy parsing).
- JSON float `{"n": 5.0}` → `int64("n")=(5,true)`; `{"n": 5.5}` → `(0,false)`.
- Body larger than maxBytes → error.
- Malformed JSON → error.
- `boolish`: `"1"/"t"/"TRUE"/"on "` → (true,true); `"0"/"f"/"false"/"off"` → (false,true); `""`, `"yes"`, absent → (_, false).
- `parseAPNSSandbox`: same allow-list; `"yes"` → false **and** a `Warn` line captured via `slog.New(slog.NewTextHandler(&buf, nil))`; JSON boolean `true` → true.

- [ ] **Step 2: Run to verify fail**

Run: `go test ./internal/httpapi/ -run 'TestParams|TestParseAPNSSandbox'`
Expected: FAIL — functions undefined.

- [ ] **Step 3: Implement**

```go
package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"mime"
	"net/http"
	"strconv"
	"strings"
)

type params struct{ m map[string]any }

func parseRequestParams(w http.ResponseWriter, r *http.Request, maxBytes int64) (params, error) {
	m := make(map[string]any)
	for k, vs := range r.URL.Query() {
		if len(vs) > 0 {
			m[k] = vs[0]
		}
	}
	// The body is read explicitly rather than via r.ParseForm: net/http only
	// parses form bodies for POST/PUT/PATCH, but spec §4's opt-out DELETE
	// carries its token as "query or body parameter".
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return params{}, errors.New("request body too large")
		}
		return params{}, fmt.Errorf("read request body: %w", err)
	}
	if len(body) == 0 {
		// An empty body is an empty param set, whatever the content type --
		// a DELETE with only ?token= must not fail a JSON decode.
		return params{m: m}, nil
	}
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if ct == "application/json" {
		var decoded map[string]any
		if err := json.Unmarshal(body, &decoded); err != nil {
			return params{}, fmt.Errorf("invalid JSON body: %w", err)
		}
		for k, v := range decoded {
			if v == nil {
				continue // JSON null counts as absent (spec §4)
			}
			m[k] = v
		}
		return params{m: m}, nil
	}
	form, err := url.ParseQuery(string(body))
	if err != nil {
		return params{}, fmt.Errorf("invalid form body: %w", err)
	}
	for k, vs := range form {
		if len(vs) > 0 {
			m[k] = vs[0]
		}
	}
	return params{m: m}, nil
}

func (p params) str(key string) (string, bool) {
	v, ok := p.m[key]
	if !ok {
		return "", false
	}
	switch t := v.(type) {
	case string:
		return strings.TrimSpace(t), true
	case bool:
		return strconv.FormatBool(t), true
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64), true
	default:
		return "", false
	}
}

func (p params) int64(key string) (int64, bool) {
	v, ok := p.m[key]
	if !ok {
		return 0, false
	}
	switch t := v.(type) {
	case float64:
		if t != math.Trunc(t) {
			return 0, false
		}
		return int64(t), true
	case string:
		n, err := strconv.ParseInt(strings.TrimSpace(t), 10, 64)
		if err != nil {
			return 0, false
		}
		return n, true
	default:
		return 0, false
	}
}

func (p params) boolish(key string) (val, present bool) {
	s, ok := p.str(key)
	if !ok {
		return false, false
	}
	switch strings.ToLower(s) {
	case "1", "t", "true", "on":
		return true, true
	case "0", "f", "false", "off":
		return false, true
	default:
		return false, false
	}
}

// parseAPNSSandbox applies the §2.7 allow-list. The two failure directions
// are asymmetric -- a production token misrouted to the sandbox bounces in
// front of a rider -- so anything unrecognized is production, logged rather
// than guessed.
func parseAPNSSandbox(p params, logger *slog.Logger) bool {
	raw, ok := p.str("apns_sandbox")
	if !ok || raw == "" {
		return false
	}
	switch strings.ToLower(raw) {
	case "1", "t", "true", "on":
		return true
	case "0", "f", "false", "off":
		return false
	default:
		logger.Warn("httpapi: unrecognized apns_sandbox value treated as production", "value", raw)
		return false
	}
}
```

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/httpapi/`
Expected: PASS (existing suite still green).

- [ ] **Step 5: Commit**

```bash
git add internal/httpapi/params.go internal/httpapi/params_test.go
git commit -m "feat: dual-encoding request params bag and strict apns_sandbox parsing"
```

---

### Task 6: Push registration endpoints + throttle

**Files:**
- Create: `internal/httpapi/pushregs.go`
- Modify: `internal/httpapi/router.go` (Deps fields + routes)
- Test: `internal/httpapi/pushregs_test.go`

**Interfaces:**
- Consumes: `pushreg.Repository` (Task 3/4), `ratelimit.Limiter` (Task 2), `params` (Task 5), existing `resolveRegion`, `writeJSON`, `writeServerError`.
- Produces: Deps fields `PushRegs pushreg.Repository` and `PushLimiter *ratelimit.Limiter`; routes `POST|DELETE /api/v2/regions/{regionId}/push_registrations`; helper `clientIP(r *http.Request) string` and middleware `throttleByIP` (reused nowhere else yet, but the ghost-bus endpoint will want them). Also `errorWithMessages(w, logger, summary string, msgs []string)` writing the §2.5 `{"error": ..., "messages": [...]}` 422, and `sanitizeToken(err error, token string) error` (strips a token value from an error before logging) — both reused by Task 11.

- [ ] **Step 1: Write the failing tests**

`pushregs_test.go` (`package httpapi_test`), extending `newTestServer` via a new variant `newPushTestServer(t)` that opens `sqlitetest.Open(t)` and fills `Deps{PushRegs: store.PushRegs(), PushLimiter: ratelimit.New(30, time.Minute), Regions: ..., Now: func() time.Time { return base }, Logger: discard}`. Tests:

- `TestRegister_MinimalIOS`: form-encoded `token=tok1&operating_system=ios` → 204, empty body; `Get` shows sandbox=false, locale "".
- `TestRegister_JSONBody`: JSON `{"token":"tok1","operating_system":"android","locale":"es-MX","apns_sandbox":"true"}` → 204; stored locale "es-MX"; **sandbox false** (Android clears it).
- `TestRegister_SandboxIOS`: `apns_sandbox=true` + ios → stored sandbox true; `apns_sandbox=yes` → false.
- `TestRegister_StickyOnRePost`: register with locale+test_device+description, re-POST with only token+os → stored locale/test_device/description survive, `apns_sandbox` reset to false.
- `TestRegister_ExplicitFalseDemotes`: re-POST `test_device=false` → test_device false, description "".
- `TestRegister_TestDeviceRequiresDescription`: **first** registration with `test_device=true` and no description → 422 `{"error":"Unable to register device","messages":["Description can't be blank"]}`.
- `TestRegister_TestDeviceRePostKeepsStoredDescription`: register `test_device=true&description=Aaron's iPhone`; re-POST `test_device=true` with no description → 204, stored description survives (spec §4: sticky fields are overwritten only by an actual value — the invariant "a test device has a description" is checked against the *merged* row, so a routine re-POST must not 422).
- `TestRegister_DescriptionAloneUpdatesTestDevice`: stored test device; POST with only `description=New name` (no test_device param) → 204, description updated, still a test device.
- `TestRegister_DescriptionIgnoredForNonTestDevice`: non-test row; POST with `description=X` and no test_device → 204, stored description stays "" (openapi: description is "cleared for non-test devices").
- `TestRegister_Validation`: missing token → 422 messages `["Token can't be blank"]`; token of 4097 chars → `["Token is too long (maximum is 4096 characters)"]`; missing os → `["Operating system can't be blank"]`; `operating_system=windows` → `["Operating system is not included in the list"]`; description of 256 chars with test_device=true → `["Description is too long (maximum is 255 characters)"]`.
- `TestRegister_UnknownRegion`: region 99 → 404 `{"error":"Couldn't find Region"}`.
- `TestUnregister`: DELETE `?token=tok1` → 204 empty; second DELETE → 404 empty body; DELETE with token in form body (no query) → 204.
- `TestThrottle`: Deps with `ratelimit.New(2, time.Minute)`; three POSTs from one `RemoteAddr` → third is 429; **a DELETE now also 429s** (shared path bucket); a different `RemoteAddr` still 204s. Set `req.RemoteAddr = "1.2.3.4:5555"` explicitly.
- `TestRegister_TokenNeverLogged`: use a Deps whose PushRegs stub errors on Upsert with an error message that **contains the token value** (the worst-case store error), capture logs into a buffer → assert the token string does not appear anywhere in the log output and `[token]` does (the handler sanitizes before logging). This can actually fail — a stub error without the token would only test the handler's own attrs.

- [ ] **Step 2: Run to verify fail**

Run: `go test ./internal/httpapi/ -run 'TestRegister|TestUnregister|TestThrottle'`
Expected: FAIL — routes not registered (404s).

- [ ] **Step 3: Implement handler + wiring**

`pushregs.go`:

```go
package httpapi

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"

	"github.com/OneBusAway/sidecar/internal/pushreg"
	"github.com/OneBusAway/sidecar/internal/ratelimit"
)

// pushRegsBodyLimit caps registration bodies. Tokens are at most 4096 chars;
// 64 KB leaves room for every documented field several times over while
// denying the free memory amplifier an unbounded read would hand an
// unauthenticated caller (spec §2.6).
const pushRegsBodyLimit = 64 << 10

const (
	maxTokenLen       = 4096
	maxDescriptionLen = 255
)

type pushRegsHandler struct{ deps Deps }

// errorWithMessages writes the §2.5 {"error", "messages"} 422 shape shared
// by push registrations and alarms.
func errorWithMessages(w http.ResponseWriter, logger *slog.Logger, summary string, msgs []string) {
	writeJSON(w, logger, http.StatusUnprocessableEntity, map[string]any{
		"error": summary, "messages": msgs,
	})
}

func (h *pushRegsHandler) register(w http.ResponseWriter, r *http.Request) {
	region, ok := resolveRegion(w, r, h.deps)
	if !ok {
		return
	}
	p, err := parseRequestParams(w, r, pushRegsBodyLimit)
	if err != nil {
		errorWithMessages(w, h.deps.Logger, "Unable to register device", []string{err.Error()})
		return
	}

	var msgs []string
	token, _ := p.str("token")
	switch {
	case token == "":
		msgs = append(msgs, "Token can't be blank")
	case len(token) > maxTokenLen:
		msgs = append(msgs, fmt.Sprintf("Token is too long (maximum is %d characters)", maxTokenLen))
	}
	os, osPresent := p.str("operating_system")
	switch {
	case !osPresent || os == "":
		msgs = append(msgs, "Operating system can't be blank")
	case os != pushreg.OSIOS && os != pushreg.OSAndroid:
		msgs = append(msgs, "Operating system is not included in the list")
	}

	up := pushreg.Upsert{RegionID: region.ID, Token: token, OperatingSystem: os}
	if os == pushreg.OSIOS {
		up.APNSSandbox = parseAPNSSandbox(p, h.deps.Logger)
	}
	// Sticky fields (spec §4): only an actual value overwrites; a blank
	// value on a routine launch-time re-POST keeps the stored one. The
	// test-device invariants ("a test device must be traceable to a human",
	// "cleared for non-test devices") hold on the *merged* row -- a re-POST
	// of test_device=true without a description keeps the stored
	// description rather than 422ing -- so the stored row is read first.
	// The read-then-upsert race is benign: both writers carry full values.
	var stored pushreg.Registration
	if token != "" {
		var err error
		stored, err = h.deps.PushRegs.Get(r.Context(), region.ID, token)
		if err != nil && !errors.Is(err, pushreg.ErrNotFound) {
			writeServerError(w, h.deps.Logger, region.ID, "get push registration", sanitizeToken(err, token))
			return
		}
	}
	if locale, ok := p.str("locale"); ok && locale != "" {
		up.Locale = &locale
	}
	testDevice, testDevicePresent := p.boolish("test_device")
	desc, descPresent := p.str("description")
	if desc == "" {
		descPresent = false // blank counts as absent, like locale
	}
	effectiveTest := stored.TestDevice
	if testDevicePresent {
		effectiveTest = testDevice
	}
	switch {
	case effectiveTest:
		merged := stored.Description
		if descPresent {
			merged = desc
		}
		switch {
		case merged == "":
			msgs = append(msgs, "Description can't be blank")
		case len(merged) > maxDescriptionLen:
			msgs = append(msgs, fmt.Sprintf("Description is too long (maximum is %d characters)", maxDescriptionLen))
		default:
			if testDevicePresent {
				up.TestDevice = &testDevice
			}
			if descPresent {
				up.Description = &desc
			}
		}
	case testDevicePresent:
		// An explicit false demotes and clears (spec §4).
		cleared := ""
		up.TestDevice = &testDevice
		up.Description = &cleared
	default:
		// Non-test row, no test_device in the request: a stray description
		// is deliberately ignored -- non-test rows carry none.
	}
	if len(msgs) > 0 {
		errorWithMessages(w, h.deps.Logger, "Unable to register device", msgs)
		return
	}

	if err := h.deps.PushRegs.Upsert(r.Context(), up, h.deps.Now()); err != nil {
		writeServerError(w, h.deps.Logger, region.ID, "upsert push registration", sanitizeToken(err, token))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// sanitizeToken makes a store error safe to log for a request that carried
// token: any occurrence of the token value in the error text is replaced.
// Driver errors do not normally echo bound values, but "normally" is not a
// guarantee the repo's no-token-logging rule can rest on.
func sanitizeToken(err error, token string) error {
	if err == nil || token == "" || !strings.Contains(err.Error(), token) {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), token, "[token]"))
}

func (h *pushRegsHandler) unregister(w http.ResponseWriter, r *http.Request) {
	region, ok := resolveRegion(w, r, h.deps)
	if !ok {
		return
	}
	p, err := parseRequestParams(w, r, pushRegsBodyLimit)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	token, _ := p.str("token")
	if token == "" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if err := h.deps.PushRegs.Delete(r.Context(), region.ID, token); err != nil {
		if errors.Is(err, pushreg.ErrNotFound) {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		// A failed delete must never masquerade as a 204 (spec §2.5).
		writeServerError(w, h.deps.Logger, region.ID, "delete push registration", sanitizeToken(err, token))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// clientIP is the throttle key: the connection's remote host. Behind a
// reverse proxy every request shares the proxy's address, so deployments
// must preserve client addresses at the proxy layer (see README); trusting
// X-Forwarded-For here would let any client spoof its own bucket.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// throttleByIP applies the shared path-scoped bucket (spec §2.6: DELETEs
// share the POST bucket). Denials are an empty 429.
func throttleByIP(l *ratelimit.Limiter, deps Deps, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !l.Allow(clientIP(r), deps.Now()) {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		next(w, r)
	}
}
```

`router.go` changes:

```go
// In Deps:
	// PushRegs backs the push registration endpoints and the V2 alarm
	// side-effect upsert. Nil means those routes are not registered.
	PushRegs pushreg.Repository
	// PushLimiter is the §2.6 throttle for the push_registrations path.
	// NewRouter defaults it (30/minute per IP); tests inject tighter ones.
	PushLimiter *ratelimit.Limiter

// In NewRouter, beside the other rider-facing registrations:
	if deps.PushRegs != nil {
		// The handlers deref Regions (resolveRegion) and Now on the first
		// request; fail at boot, matching the Auth block's precedent.
		if deps.Now == nil || deps.Regions == nil {
			panic("httpapi: Deps.Now and Deps.Regions required when Deps.PushRegs is set")
		}
		if deps.PushLimiter == nil {
			deps.PushLimiter = ratelimit.New(30, time.Minute)
		}
		ph := &pushRegsHandler{deps: deps}
		mux.HandleFunc("POST /api/v2/regions/{regionId}/push_registrations",
			throttleByIP(deps.PushLimiter, deps, ph.register))
		mux.HandleFunc("DELETE /api/v2/regions/{regionId}/push_registrations",
			throttleByIP(deps.PushLimiter, deps, ph.unregister))
	}
```

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/httpapi/` then `go test ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/httpapi
git commit -m "feat: push registration endpoints with sticky upsert and IP throttle (spec §4)"
```

---

### Task 7: alarms domain package

**Files:**
- Create: `internal/alarms/alarms.go`
- Test: `internal/alarms/alarms_test.go`

**Interfaces:**
- Produces (consumed by Tasks 8, 11, 12):

```go
package alarms

var (
	ErrNotFound  = errors.New("alarm not found")
	// ErrDuplicate reports a V1 create that lost the dedupe race to a
	// concurrent identical registration; callers re-fetch the winner.
	ErrDuplicate = errors.New("duplicate v1 alarm")
)

const DefaultSecondsBefore = 600 // spec §5.2

type Alarm struct {
	ID              int64
	RegionID        int64
	Token           string
	APIVersion      int // 1 | 2
	UserPushID      string
	OperatingSystem string // "ios" | "android"
	APNSSandbox     bool
	// Trip-identity fields are client obligations, not server-enforced
	// (spec §5.2): zero values mean the client omitted them and the push
	// payload carries null there.
	StopID       string
	TripID       string
	ServiceDate  int64  // epoch ms; 0 = omitted
	VehicleID    string // "" = omitted
	StopSequence *int64 // nil = omitted; 0 is a real value (first stop)
	SecondsBefore int64
	Message       string // composed at creation; what eventually gets pushed
	FailureCount  int64  // consecutive failed OBA lookups (spec §5.3)
	CreatedAt     time.Time
}

type NewAlarm struct {
	RegionID        int64
	Token           string
	APIVersion      int
	UserPushID      string
	OperatingSystem string
	APNSSandbox     bool
	StopID, TripID  string
	ServiceDate     int64
	VehicleID       string
	StopSequence    *int64
	SecondsBefore   int64
	Message         string
}

// V1Key is the §5.1 idempotency key for legacy clients.
type V1Key struct {
	RegionID    int64
	UserPushID  string
	TripID      string
	StopID      string
	ServiceDate int64
}

type Repository interface {
	Create(ctx context.Context, in NewAlarm, now time.Time) (Alarm, error) // ErrDuplicate on V1 race
	FindV1(ctx context.Context, key V1Key) (Alarm, error)                  // ErrNotFound
	Delete(ctx context.Context, regionID int64, token string) error        // ErrNotFound; 204 contract
	DeleteByID(ctx context.Context, id int64) error
	List(ctx context.Context) ([]Alarm, error) // scheduler sweep, all regions
	RecordFailure(ctx context.Context, id int64) (int64, error) // ++failure_count, returns streak
	ResetFailures(ctx context.Context, id int64) error
}

// NormalizeSecondsBefore applies the §5.2 rule: absent, non-numeric, or
// <= 0 becomes the 600-second default.
func NormalizeSecondsBefore(v int64, ok bool) int64

// ComposeMessage is the creation-time human message: "The 44 to Ballard
// leaves in 10 minutes". Minutes derive from secondsBefore because that is
// the lead time the push actually fires at.
func ComposeMessage(routeShortName, headsign string, secondsBefore int64) string

// GenericMessage is the §5.2 fallback: "The bus leaves in 10 minutes".
func GenericMessage(secondsBefore int64) string

// PushData is the §5.4 wire contract the apps deep-link from. Exact key
// set and nesting; omitted trip fields are JSON null, never "".
func (a Alarm) PushData() map[string]any

type Decision int

const (
	Wait Decision = iota
	Fire
	Expire // departure already passed: delete without pushing (spec §5.3)
)

// Decide implements the §5.3 firing rules given seconds until departure.
func Decide(secondsUntilDeparture, secondsBefore int64) Decision
```

- [ ] **Step 1: Write the failing tests**

```go
package alarms_test

import (
	"encoding/json"
	"testing"

	"github.com/OneBusAway/sidecar/internal/alarms"
)

func TestNormalizeSecondsBefore(t *testing.T) {
	t.Parallel()
	tests := []struct {
		v    int64
		ok   bool
		want int64
	}{
		{300, true, 300},
		{0, true, 600},
		{-5, true, 600},
		{0, false, 600}, // absent or non-numeric
	}
	for _, tt := range tests {
		if got := alarms.NormalizeSecondsBefore(tt.v, tt.ok); got != tt.want {
			t.Errorf("NormalizeSecondsBefore(%d, %v) = %d; want %d", tt.v, tt.ok, got, tt.want)
		}
	}
}

func TestMessages(t *testing.T) {
	t.Parallel()
	if got, want := alarms.ComposeMessage("44", "Ballard", 600), "The 44 to Ballard leaves in 10 minutes"; got != want {
		t.Errorf("ComposeMessage = %q; want %q", got, want)
	}
	if got, want := alarms.ComposeMessage("E Line", "Aurora Village", 60), "The E Line to Aurora Village leaves in 1 minute"; got != want {
		t.Errorf("ComposeMessage = %q; want %q", got, want)
	}
	// Sub-minute lead times still say 1 minute, not 0.
	if got, want := alarms.GenericMessage(30), "The bus leaves in 1 minute"; got != want {
		t.Errorf("GenericMessage = %q; want %q", got, want)
	}
	if got, want := alarms.GenericMessage(600), "The bus leaves in 10 minutes"; got != want {
		t.Errorf("GenericMessage = %q; want %q", got, want)
	}
}

func TestPushData(t *testing.T) {
	t.Parallel()
	seq := int64(3)
	full := alarms.Alarm{RegionID: 1, StopID: "1_570", TripID: "1_604370",
		ServiceDate: 1754809200000, VehicleID: "1_4361", StopSequence: &seq}
	b, err := json.Marshal(full.PushData())
	if err != nil {
		t.Fatal(err)
	}
	want := `{"arrival_and_departure":{"region_id":1,"service_date":1754809200000,` +
		`"stop_id":"1_570","stop_sequence":3,"trip_id":"1_604370","vehicle_id":"1_4361"}}`
	if string(b) != want {
		t.Errorf("PushData = %s\nwant     %s", b, want)
	}

	// Omitted fields are null, not "" or 0 (spec §5.2: "null trip fields").
	empty := alarms.Alarm{RegionID: 2}
	b, _ = json.Marshal(empty.PushData())
	want = `{"arrival_and_departure":{"region_id":2,"service_date":null,` +
		`"stop_id":null,"stop_sequence":null,"trip_id":null,"vehicle_id":null}}`
	if string(b) != want {
		t.Errorf("PushData(empty) = %s\nwant            %s", b, want)
	}
}

func TestDecide(t *testing.T) {
	t.Parallel()
	tests := []struct {
		until, before int64
		want          alarms.Decision
	}{
		{700, 600, alarms.Wait},
		{601, 600, alarms.Wait},
		{600, 600, alarms.Fire}, // boundary: not yet only when until > before
		{1, 600, alarms.Fire},
		{0, 600, alarms.Fire}, // leaving right now is still worth the push
		{-1, 600, alarms.Expire},
	}
	for _, tt := range tests {
		if got := alarms.Decide(tt.until, tt.before); got != tt.want {
			t.Errorf("Decide(%d, %d) = %v; want %v", tt.until, tt.before, got, tt.want)
		}
	}
}
```

- [ ] **Step 2: Run to verify fail**

Run: `go test ./internal/alarms/`
Expected: FAIL — package does not exist.

- [ ] **Step 3: Implement**

Types exactly as the Interfaces block, plus:

```go
func NormalizeSecondsBefore(v int64, ok bool) int64 {
	if !ok || v <= 0 {
		return DefaultSecondsBefore
	}
	return v
}

// minutesPhrase renders the lead time the way the apps' copy expects,
// clamping to at least one minute so a short lead never reads "0 minutes".
func minutesPhrase(secondsBefore int64) string {
	m := secondsBefore / 60
	if m < 1 {
		m = 1
	}
	if m == 1 {
		return "1 minute"
	}
	return fmt.Sprintf("%d minutes", m)
}

func ComposeMessage(routeShortName, headsign string, secondsBefore int64) string {
	return fmt.Sprintf("The %s to %s leaves in %s", routeShortName, headsign, minutesPhrase(secondsBefore))
}

func GenericMessage(secondsBefore int64) string {
	return fmt.Sprintf("The bus leaves in %s", minutesPhrase(secondsBefore))
}

func (a Alarm) PushData() map[string]any {
	nullableStr := func(s string) any {
		if s == "" {
			return nil
		}
		return s
	}
	ad := map[string]any{
		"region_id": a.RegionID,
		"stop_id":   nullableStr(a.StopID),
		"trip_id":   nullableStr(a.TripID),
		"vehicle_id": nullableStr(a.VehicleID),
	}
	if a.ServiceDate != 0 {
		ad["service_date"] = a.ServiceDate
	} else {
		ad["service_date"] = nil
	}
	if a.StopSequence != nil {
		ad["stop_sequence"] = *a.StopSequence
	} else {
		ad["stop_sequence"] = nil
	}
	return map[string]any{"arrival_and_departure": ad}
}

func Decide(secondsUntilDeparture, secondsBefore int64) Decision {
	switch {
	case secondsUntilDeparture > secondsBefore:
		return Wait
	case secondsUntilDeparture < 0:
		return Expire
	default:
		return Fire
	}
}
```

Package doc explains: an alarm is one-shot server-owned timing (§5 intro); `region_id` in `PushData` is the **public** region identifier, deliberately not an internal row id (§5.4 reference quirk).

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/alarms/` — PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/alarms
git commit -m "feat: alarms domain types, messages, push payload, and firing decision"
```

---

### Task 8: alarms storage (migration + sqlc + adapter + conformance)

**Files:**
- Create: `internal/store/sqlite/migrations/00005_alarms.sql`
- Create: `internal/store/sqlite/queries/alarms.sql`
- Create: `internal/store/sqlite/alarms.go`
- Create: `internal/store/storetest/alarmtest.go`
- Modify: `internal/store/sqlite/store.go` (add `Alarms()` accessor)
- Test: `internal/store/sqlite/store_test.go` (conformance hookup)

**Interfaces:**
- Consumes: `alarms.Repository` and types (Task 7).
- Produces: `(*sqlite.Store).Alarms() alarms.Repository`; `storetest.RunAlarmRepository(t, newStore func(*testing.T) (alarms.Repository, regions.Repository))`.

- [ ] **Step 1: Write the migration**

```sql
-- +goose Up
CREATE TABLE alarms (
  id               INTEGER PRIMARY KEY AUTOINCREMENT,
  region_id        INTEGER NOT NULL REFERENCES regions(id) ON DELETE CASCADE,
  -- The public address (spec §2.4). Globally unique, not per-region: tokens
  -- are 128 random bits, and a global uniqueness constraint means a token
  -- can never resolve to two riders' alarms.
  token            TEXT    NOT NULL UNIQUE,
  api_version      INTEGER NOT NULL CHECK (api_version IN (1, 2)),
  user_push_id     TEXT    NOT NULL,
  operating_system TEXT    NOT NULL CHECK (operating_system IN ('ios', 'android')),
  apns_sandbox     BOOLEAN NOT NULL DEFAULT FALSE,
  -- Trip-identity fields are stored as the client sent them, unvalidated
  -- (spec §5.2). '' / 0 mean omitted; stop_sequence needs NULL because 0 is
  -- a real value (the trip's first stop).
  stop_id          TEXT    NOT NULL DEFAULT '',
  trip_id          TEXT    NOT NULL DEFAULT '',
  service_date     INTEGER NOT NULL DEFAULT 0,
  vehicle_id       TEXT    NOT NULL DEFAULT '',
  stop_sequence    INTEGER,
  seconds_before   INTEGER NOT NULL,
  message          TEXT    NOT NULL,
  failure_count    INTEGER NOT NULL DEFAULT 0,
  created_at       INTEGER NOT NULL,
  updated_at       INTEGER NOT NULL
);

-- V1 idempotency (spec §5.1): re-POSTs of the same registration return the
-- existing alarm. UNIQUE (not just an index) so the concurrent-duplicate
-- race surfaces as a constraint violation the adapter maps to ErrDuplicate
-- instead of quietly minting a second alarm that fires twice.
CREATE UNIQUE INDEX alarms_v1_dedupe_idx
  ON alarms (region_id, user_push_id, trip_id, stop_id, service_date)
  WHERE api_version = 1;

-- +goose Down
DROP TABLE alarms;
```

- [ ] **Step 2: Write the sqlc queries**

```sql
-- name: CreateAlarm :one
INSERT INTO alarms (
  region_id, token, api_version, user_push_id, operating_system, apns_sandbox,
  stop_id, trip_id, service_date, vehicle_id, stop_sequence,
  seconds_before, message, created_at, updated_at
) VALUES (
  @region_id, @token, @api_version, @user_push_id, @operating_system, @apns_sandbox,
  @stop_id, @trip_id, @service_date, @vehicle_id, @stop_sequence,
  @seconds_before, @message, @now, @now
)
RETURNING *;

-- name: FindV1Alarm :one
SELECT * FROM alarms
WHERE api_version = 1 AND region_id = @region_id AND user_push_id = @user_push_id
  AND trip_id = @trip_id AND stop_id = @stop_id AND service_date = @service_date;

-- name: DeleteAlarmByToken :execrows
DELETE FROM alarms WHERE region_id = @region_id AND token = @token;

-- name: DeleteAlarmByID :execrows
DELETE FROM alarms WHERE id = @id;

-- name: ListAlarms :many
SELECT * FROM alarms ORDER BY id;

-- name: RecordAlarmFailure :one
UPDATE alarms SET failure_count = failure_count + 1, updated_at = @now
WHERE id = @id
RETURNING failure_count;

-- name: ResetAlarmFailures :exec
UPDATE alarms SET failure_count = 0, updated_at = @now
WHERE id = @id AND failure_count <> 0;
```

Run `make generate`; `go build ./...`.

- [ ] **Step 3: Write the conformance suite (failing)**

`storetest/alarmtest.go` subtests:

- `CreateGetRoundTrip`: create with every field set (incl. `StopSequence: ptr(int64(0))` — zero must survive as 0, not nil) and `FindV1`... for V2 rows use `List` to read back; assert all fields, token echoed, `FailureCount == 0`.
- `StopSequenceZeroDistinctFromAbsent`: one alarm with `StopSequence: ptr(int64(0))`, one with nil; `List` returns them distinguishably.
- `V1FindMatchesExactKey`: FindV1 hits only on all five key fields; changing any one → `alarms.ErrNotFound`.
- `V1DuplicateInsertReturnsErrDuplicate`: two Creates with identical V1 key → second returns `alarms.ErrDuplicate`.
- `V2NeverDeduplicates`: two identical V2 Creates → two rows.
- `DeleteByTokenReports204Contract`: Delete on known → nil then really gone (List empty); unknown → `ErrNotFound`.
- `FailureCounterIncrementsAndResets`: RecordFailure ×3 returns 1,2,3; ResetFailures → next RecordFailure returns 1.
- `ServiceDateBeyond32Bit`: epoch-ms values round-trip (int64).
- `RegionCascade`: delete... regions repo has no delete; instead assert region_id scoping on Delete (token in region 1 not deletable via region 2).

- [ ] **Step 4: Hook into `store_test.go`, run to verify fail**

```go
func TestAlarmConformance(t *testing.T) {
	t.Parallel()
	storetest.RunAlarmRepository(t, func(t *testing.T) (alarms.Repository, regions.Repository) {
		s := sqlitetest.Open(t)
		return s.Alarms(), s.Regions()
	})
}
```

Run: `go test ./internal/store/...` — FAIL (`Alarms` undefined).

- [ ] **Step 5: Write the adapter**

`sqlite/alarms.go`: `alarmRepo{q *gen.Queries}`. Mapping notes:

- `StopSequence *int64` ↔ `sql.NullInt64`.
- `Create` maps a SQLite unique-constraint error on `alarms_v1_dedupe_idx` to `alarms.ErrDuplicate`. Follow the package's actual precedent (`sqlite/auth.go` matches the constraint message string): match `strings.Contains(err.Error(), "UNIQUE constraint failed: alarms.region_id")` — the dedupe index's message names its column list, which distinguishes it from the `token` UNIQUE constraint ("UNIQUE constraint failed: alarms.token", an effectively-impossible collision that must stay a 500). Keep `in.APIVersion == 1` as a belt-and-suspenders guard.
- `Delete`/`DeleteByID`: `:execrows` 0 → `alarms.ErrNotFound` (Delete only; DeleteByID treats 0 rows as success — the scheduler may race a rider's cancel, and the row being gone is the goal).
- Timestamps `.Unix()`; `CreatedAt` read back via `time.Unix(...,0).UTC()`.

- [ ] **Step 6: Run to verify pass**

Run: `go test ./internal/store/...` and `-race`. PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/store
git commit -m "feat: alarms storage with V1 dedupe constraint and conformance suite"
```

---

### Task 9: obaapi ArrivalAndDeparture

**Files:**
- Modify: `internal/obaapi/obaapi.go`
- Modify: `internal/vehicles/vehicles_test.go` / `service_test.go` (extend any fake `obaapi.Client` with the new method)
- Modify: `internal/httpapi/vehicles_test.go` — its `fakeOBA` also implements `obaapi.Client` and breaks compilation without the one-line stub
- Test: `internal/obaapi/obaapi_test.go`

**Interfaces:**
- Produces (consumed by Tasks 11 and 12):

```go
// DepartureQuery keys one arrival-and-departure-for-stop lookup (§5.3).
type DepartureQuery struct {
	StopID       string
	TripID       string
	ServiceDate  int64 // epoch ms
	VehicleID    string // "" = omit
	StopSequence *int64 // nil = omit
}

// Departure is the slice of the OBA response alarms need.
type Departure struct {
	RouteShortName         string
	TripHeadsign           string
	ScheduledDepartureTime int64 // epoch ms
	PredictedDepartureTime int64 // epoch ms; 0 = no realtime
	Predicted              bool
}

// ErrNotFound means the OBA server answered but knows nothing about this
// trip/stop/date combination -- the alarm-reaper's "trip aged out" signal,
// distinct from transient transport failures which must NOT count toward
// the 3-strike streak (spec §5.3).
var ErrNotFound = errors.New("obaapi: arrival-and-departure not found")

// Added to the Client interface:
	ArrivalAndDeparture(ctx context.Context, region regions.Region, q DepartureQuery) (Departure, error)
```

- [ ] **Step 1: Write the failing tests**

Extend the existing `httptest`-based fake OBA server pattern in `obaapi_test.go` with a handler for `GET /api/where/arrival-and-departure-for-stop/{stopID}.json`:

- `TestArrivalAndDeparture_MapsEntry`: fake returns an entry with `routeShortName: "44"`, `tripHeadsign: "Ballard"`, predicted+scheduled times → all mapped; query string carries `tripId`, `serviceDate`, `vehicleId`, `stopSequence` exactly as passed.
- `TestArrivalAndDeparture_RouteShortNameFromReferences`: entry has empty `routeShortName` but `routeId: "1_100224"`; references carry that route with `shortName: "44"` → Departure.RouteShortName "44"; a references route with only `longName` falls back to it.
- `TestArrivalAndDeparture_404IsErrNotFound`: upstream 404 → `errors.Is(err, obaapi.ErrNotFound)`.
- `TestArrivalAndDeparture_5xxIsNotErrNotFound`: upstream 500 → error, **not** ErrNotFound, and redacted (no URL/key in `err.Error()` — assert `!strings.Contains(err.Error(), "test_key")`).
- `TestArrivalAndDeparture_OmitsOptionalParams`: nil StopSequence / empty VehicleID → those query params absent from the request the fake observed.
- `TestArrivalAndDeparture_WithoutKeyMakesNoRequest`: no key → `ErrNotConfigured`, zero requests observed (same as the existing Fleet test).

- [ ] **Step 2: Run to verify fail**

Run: `go test ./internal/obaapi/` — FAIL (method undefined).

- [ ] **Step 3: Implement**

```go
func (c *client) ArrivalAndDeparture(ctx context.Context, region regions.Region, q DepartureQuery) (Departure, error) {
	key := region.OBAAPIKey
	if key == "" {
		key = c.defaultKey
	}
	if key == "" {
		return Departure{}, ErrNotConfigured
	}
	sdk := oba.NewClient(
		option.WithBaseURL(region.OBABaseURL),
		option.WithAPIKey(key),
		option.WithHTTPClient(c.http),
		option.WithRequestTimeout(perRequestTimeout),
		option.WithMaxRetries(maxRetries),
	)

	params := oba.ArrivalAndDepartureGetParams{
		TripID:      oba.F(q.TripID),
		ServiceDate: oba.F(q.ServiceDate),
	}
	if q.VehicleID != "" {
		params.VehicleID = oba.F(q.VehicleID)
	}
	if q.StopSequence != nil {
		params.StopSequence = oba.F(*q.StopSequence)
	}

	resp, err := sdk.ArrivalAndDeparture.Get(ctx, q.StopID, params)
	if err != nil {
		// A 404 is the upstream's "no such trip at this stop/date" -- the
		// signal §5.3's reaper counts. Everything else stays a redacted
		// transient error the caller must not count.
		if statusOf(err) == http.StatusNotFound {
			return Departure{}, ErrNotFound
		}
		return Departure{}, fmt.Errorf("obaapi: arrival-and-departure in region %d: %w", region.ID, redact(err))
	}

	entry := resp.Data.Entry
	if entry.TripID == "" {
		// A 200 with an empty entry is the same "nothing here" as a 404.
		return Departure{}, ErrNotFound
	}

	shortName := entry.RouteShortName
	if shortName == "" {
		for _, route := range resp.Data.References.Routes {
			if route.ID == entry.RouteID {
				shortName = route.ShortName
				if shortName == "" {
					shortName = route.LongName
				}
				break
			}
		}
	}
	return Departure{
		RouteShortName:         shortName,
		TripHeadsign:           entry.TripHeadsign,
		ScheduledDepartureTime: entry.ScheduledDepartureTime,
		PredictedDepartureTime: entry.PredictedDepartureTime,
		Predicted:              entry.Predicted,
	}, nil
}
```

(Verify the exact `shared.ReferencesRoute` field names — `ID`, `ShortName`, `LongName` — against the SDK source at implementation time and adjust; the test in Step 1 pins the behavior either way.) Add the method to the `Client` interface and to **every** test fake of it — `internal/vehicles` and `internal/httpapi/vehicles_test.go` both define one (a one-line stub returning `Departure{}, nil`).

- [ ] **Step 4: Run to verify pass**

Run: `go test ./...` — PASS. (Not just the two packages: interface changes break fakes in packages this task doesn't otherwise touch, and the commit must leave the whole tree green.)

- [ ] **Step 5: Commit**

```bash
git add internal/obaapi internal/vehicles
git commit -m "feat: obaapi arrival-and-departure lookup with not-found sentinel"
```

---

### Task 10: push transport (Sender + gorush)

**Files:**
- Create: `internal/push/push.go`, `internal/push/gorush.go`
- Test: `internal/push/gorush_test.go`

**Interfaces:**
- Produces (consumed by Tasks 12, 13, 14):

```go
package push

// Platform uses gorush's wire codes so the adapter needs no translation
// table; they are stable public API of that project.
type Platform int

const (
	PlatformIOS     Platform = 1
	PlatformAndroid Platform = 2
)

// Notification is one push to one or more device tokens. Data is the
// structured payload the app parses (§5.4); it must marshal to JSON.
type Notification struct {
	Tokens   []string
	Platform Platform
	Sandbox  bool // APNs sandbox routing; meaningless for Android
	Title    string
	Message  string
	Data     map[string]any
}

// Sender delivers notifications. Implementations must be safe for
// concurrent use. Send returning nil means the transport accepted the
// notification, not that the device received it -- delivery failures come
// back asynchronously (§6.5) via the feedback webhook.
type Sender interface {
	Send(ctx context.Context, n Notification) error
}

func NewGorush(baseURL string, httpClient *http.Client, logger *slog.Logger) *Gorush

// IsTerminal reports whether an APNs failure reason means the token is
// dead (spec §4/§6.5): Unregistered, BadDeviceToken, DeviceTokenNotForTopic,
// matched by substring. ExpiredProviderToken is about our JWT and is
// deliberately not terminal.
func IsTerminal(reason string) bool
```

- [ ] **Step 1: Write the failing tests**

- `TestGorushSendPostsExpectedJSON`: httptest server capturing the request; `Send` an iOS sandbox notification with data → asserts method POST, path `/api/push`, and body decodes to:

```json
{"notifications":[{"tokens":["tok1"],"platform":1,"title":"OneBusAway",
  "message":"The 44 to Ballard leaves in 10 minutes","priority":"high",
  "development":true,"data":{"arrival_and_departure":{"region_id":1}}}]}
```

- `TestGorushAndroidOmitsDevelopment`: Platform 2, Sandbox true (garbage in) → body has `"platform":2` and **no** `development` key (sandbox is APNs-only, spec §2.7).
- `TestGorushProductionOmitsDevelopment`: iOS Sandbox false → no `development` key.
- `TestGorushNon2xxIsError`: server returns 400 with a body echoing the token → `Send` returns an error whose message contains the status code and **not** the token.
- `TestIsTerminal`: `"Unregistered"`, `"apns: BadDeviceToken"`, `"DeviceTokenNotForTopic"` → true; `"ExpiredProviderToken"`, `"InternalServerError"`, `""` → false.

- [ ] **Step 2: Run to verify fail**

Run: `go test ./internal/push/` — FAIL.

- [ ] **Step 3: Implement**

`gorush.go`:

```go
// gorushNotification is the subset of gorush's request schema this sidecar
// uses (POST /api/push). Priority is always "high": alarm pushes are
// time-sensitive by definition, and gorush maps it to APNs priority 10 /
// FCM high so an idle phone does not hold the wake-the-rider push.
type gorushNotification struct {
	Tokens      []string       `json:"tokens"`
	Platform    int            `json:"platform"`
	Title       string         `json:"title,omitempty"`
	Message     string         `json:"message"`
	Priority    string         `json:"priority"`
	Development bool           `json:"development,omitempty"`
	Data        map[string]any `json:"data,omitempty"`
}

type Gorush struct {
	pushURL string
	http    *http.Client
	logger  *slog.Logger
}

func NewGorush(baseURL string, httpClient *http.Client, logger *slog.Logger) *Gorush {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Gorush{
		pushURL: strings.TrimRight(baseURL, "/") + "/api/push",
		http:    httpx.NoRedirectClient(httpClient),
		logger:  logger,
	}
}

func (g *Gorush) Send(ctx context.Context, n Notification) error {
	gn := gorushNotification{
		Tokens:   n.Tokens,
		Platform: int(n.Platform),
		Title:    n.Title,
		Message:  n.Message,
		Priority: "high",
		Data:     n.Data,
	}
	if n.Platform == PlatformIOS {
		gn.Development = n.Sandbox
	}
	body, err := json.Marshal(map[string]any{"notifications": []gorushNotification{gn}})
	if err != nil {
		return fmt.Errorf("push: marshal notification: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.pushURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("push: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := g.http.Do(req)
	if err != nil {
		// The transport error can embed the gateway URL; that is
		// operator-configured and not secret, but tokens never appear in it,
		// so it is safe to wrap as-is.
		return fmt.Errorf("push: gorush request: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// Never include the response body: gorush error bodies echo the
		// notification, tokens included.
		return fmt.Errorf("push: gorush returned status %d", resp.StatusCode)
	}
	return nil
}
```

`push.go` holds the types plus:

```go
// terminalReasons are exactly the spec's list (§6.5). ExpiredToken (also
// never-retry per Apple) is deliberately excluded to stay spec-faithful;
// tokens it would have caught die at the 180-day prune instead.
var terminalReasons = []string{"Unregistered", "BadDeviceToken", "DeviceTokenNotForTopic"}

func IsTerminal(reason string) bool {
	for _, t := range terminalReasons {
		if strings.Contains(reason, t) {
			return true
		}
	}
	return false
}
```

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/push/` — PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/push
git commit -m "feat: push.Sender interface with gorush transport"
```

---

### Task 11: Alarm create/delete endpoints (V1 + V2)

**Files:**
- Create: `internal/httpapi/alarms.go`
- Modify: `internal/httpapi/router.go` (Deps: `Alarms alarms.Repository`, `OBA obaapi.Client`; routes)
- Test: `internal/httpapi/alarms_api_test.go`

**Interfaces:**
- Consumes: `alarms.Repository` (Tasks 7/8), `pushreg.Repository` (Deps.PushRegs, Task 6), `obaapi.Client.ArrivalAndDeparture` (Task 9), `securetoken.New` (Task 1), `params` + `parseAPNSSandbox` (Task 5), `errorWithMessages` (Task 6).
- Produces: routes `POST /api/v{1,2}/regions/{regionId}/alarms`, `DELETE /api/v{1,2}/regions/{regionId}/alarms/{alarmToken}`.

- [ ] **Step 1: Write the failing tests**

Test server: `sqlitetest.Open`, Deps with `Alarms`, `PushRegs`, `Regions`, and a fake `obaapi.Client` (local stub type implementing `Fleet` panic + `ArrivalAndDeparture` returning a configurable `(Departure, error)`). Seed a region whose `SidecarBaseURL` is `https://sidecar.example.org`. Tests:

- `TestCreateV2_ComposedMessage`: full form body → 201 `{"url":"https://sidecar.example.org/api/v2/regions/1/alarms/<22-char-token>"}`; stored alarm has message `"The 44 to Ballard leaves in 10 minutes"`, `APIVersion 2`, sandbox parsed, `SecondsBefore` as sent.
- `TestCreateV2_JSONBody`: same via JSON, incl. numeric `service_date` and `stop_sequence: 0` → stored `StopSequence` = ptr(0).
- `TestCreateV2_LookupFailureDegradesToGeneric`: fake OBA errors → still 201, message `"The bus leaves in 10 minutes"`.
- `TestCreateV2_MissingTripFieldsStillCreated`: only `user_push_id` + `operating_system` → 201, generic message, no OBA call made (assert via stub counter — don't hit upstream with an unkeyable query).
- `TestCreateV2_SecondsBeforeDefaults`: `seconds_before=0`, `=-3`, `=abc`, absent → all stored as 600.
- `TestCreateV2_Validation`: missing `user_push_id` → 422 `{"error":"Unable to register alarm","messages":["Push identifier can't be blank"]}` (the openapi V1 example's phrasing, used for both versions; messages are human-readable only, `error` is what clients key on); missing os → `"Operating system can't be blank"`; `operating_system=windows` → `"Operating system is not included in the list"`.
- `TestCreateV2_SideEffectUpsertsPushRegistration`: after 201, `PushRegs.Get(region, user_push_id)` exists with the alarm's OS, sandbox **false** even though the alarm was sandbox=true (spec §5.2's documented wart), locale "".
- `TestCreateV2_SideEffectRefreshesExisting`: pre-register token with locale "es"; create alarm with same token → locale still "es", `LastSeenAt` advanced.
- `TestCreateV1_DefaultsOSToIOS`: no `operating_system` → 201, stored ios; `operating_system=garbage` → still ios (V1 treats invalid as absent, spec §5.2).
- `TestCreateV1_Dedupe`: two identical POSTs → both 201 with the **same** url; second with a different `seconds_before` → still the original url and stored `SecondsBefore` unchanged ("without applying changed fields"); exactly one row.
- `TestCreateV1_NoSideEffect`: V1 create → `PushRegs.Get` → `ErrNotFound` (side effect is V2-only, spec §5.2).
- `TestCreateV1_NoSandbox`: `apns_sandbox=true` on V1 → stored sandbox false (param unsupported on V1, spec §5.1).
- `TestDelete`: create then DELETE `/api/v2/regions/1/alarms/{token}` → 204 empty; repeat → 404 empty; DELETE via slug path `/api/v2/regions/1-puget-sound/alarms/{token}` → works; V1 delete path deletes a V2-created alarm (tokens are version-agnostic).
- `TestCreate_UnknownRegion`: 404 region contract.
- `TestCreate_FallbackURLWhenNoSidecarBaseURL`: region with empty `SidecarBaseURL`, request `Host: sidecar.local` → url starts `https://sidecar.local/`.

- [ ] **Step 2: Run to verify fail**

Run: `go test ./internal/httpapi/ -run 'TestCreate|TestDelete'` — FAIL (404s).

- [ ] **Step 3: Implement**

`alarms.go` sketch:

```go
const alarmsBodyLimit = 64 << 10

type alarmsHandler struct{ deps Deps }

func (h *alarmsHandler) create(version int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		region, ok := resolveRegion(w, r, h.deps)
		if !ok {
			return
		}
		p, err := parseRequestParams(w, r, alarmsBodyLimit)
		if err != nil {
			errorWithMessages(w, h.deps.Logger, "Unable to register alarm", []string{err.Error()})
			return
		}

		var msgs []string
		userPushID, _ := p.str("user_push_id")
		if userPushID == "" {
			msgs = append(msgs, "Push identifier can't be blank")
		}
		os, _ := p.str("operating_system")
		if version == 2 {
			switch {
			case os == "":
				msgs = append(msgs, "Operating system can't be blank")
			case os != pushreg.OSIOS && os != pushreg.OSAndroid:
				msgs = append(msgs, "Operating system is not included in the list")
			}
		} else if os != pushreg.OSIOS && os != pushreg.OSAndroid {
			// V1 treats an invalid value like an absent one (spec §5.2).
			os = pushreg.OSIOS
		}
		if len(msgs) > 0 {
			errorWithMessages(w, h.deps.Logger, "Unable to register alarm", msgs)
			return
		}

		sandbox := false
		if version == 2 && os == pushreg.OSIOS {
			sandbox = parseAPNSSandbox(p, h.deps.Logger)
		}

		stopID, _ := p.str("stop_id")
		tripID, _ := p.str("trip_id")
		serviceDate, _ := p.int64("service_date") // non-numeric -> 0 = omitted
		vehicleID, _ := p.str("vehicle_id")
		var stopSeq *int64
		if v, ok := p.int64("stop_sequence"); ok {
			stopSeq = &v
		}
		sb, sbOK := p.int64("seconds_before")
		secondsBefore := alarms.NormalizeSecondsBefore(sb, sbOK)

		if version == 1 {
			key := alarms.V1Key{RegionID: region.ID, UserPushID: userPushID,
				TripID: tripID, StopID: stopID, ServiceDate: serviceDate}
			if existing, err := h.deps.Alarms.FindV1(r.Context(), key); err == nil {
				// Idempotent re-POST: hand back the existing alarm untouched
				// (spec §5.1 -- legacy clients re-POST aggressively).
				writeJSON(w, h.deps.Logger, http.StatusCreated,
					map[string]string{"url": alarmURL(region, r, version, existing.Token)})
				return
			} else if !errors.Is(err, alarms.ErrNotFound) {
				writeServerError(w, h.deps.Logger, region.ID, "find v1 alarm", sanitizeToken(err, userPushID))
				return
			}
		}

		message := h.composeMessage(r.Context(), region, stopID, tripID, serviceDate, vehicleID, stopSeq, secondsBefore)

		token, err := securetoken.New()
		if err != nil {
			writeServerError(w, h.deps.Logger, region.ID, "mint alarm token", err)
			return
		}
		created, err := h.deps.Alarms.Create(r.Context(), alarms.NewAlarm{
			RegionID: region.ID, Token: token, APIVersion: version,
			UserPushID: userPushID, OperatingSystem: os, APNSSandbox: sandbox,
			StopID: stopID, TripID: tripID, ServiceDate: serviceDate,
			VehicleID: vehicleID, StopSequence: stopSeq,
			SecondsBefore: secondsBefore, Message: message,
		}, h.deps.Now())
		if err != nil {
			if version == 1 && errors.Is(err, alarms.ErrDuplicate) {
				// Lost the race to a concurrent identical registration; the
				// winner is the alarm this client asked for.
				key := alarms.V1Key{RegionID: region.ID, UserPushID: userPushID,
					TripID: tripID, StopID: stopID, ServiceDate: serviceDate}
				if existing, ferr := h.deps.Alarms.FindV1(r.Context(), key); ferr == nil {
					writeJSON(w, h.deps.Logger, http.StatusCreated,
						map[string]string{"url": alarmURL(region, r, version, existing.Token)})
					return
				}
			}
			writeServerError(w, h.deps.Logger, region.ID, "create alarm", sanitizeToken(err, userPushID))
			return
		}

		if version == 2 {
			// Every V2 alarm creation also refreshes the alert-push audience
			// (spec §5.2): OS and last_seen_at only, no locale, and -- the
			// documented reference wart -- no apns_sandbox propagation.
			if err := h.deps.PushRegs.Upsert(r.Context(), pushreg.Upsert{
				RegionID: region.ID, Token: userPushID, OperatingSystem: os,
			}, h.deps.Now()); err != nil {
				// The alarm exists and its 201 must stand; the registry miss
				// only costs alert-push reach.
				h.deps.Logger.Warn("httpapi: alarm side-effect registration failed",
					"region_id", region.ID, "err", err)
			}
		}

		writeJSON(w, h.deps.Logger, http.StatusCreated,
			map[string]string{"url": alarmURL(region, r, version, created.Token)})
	}
}

// composeMessage resolves the arrival at creation time (spec §5.2). Any
// failure -- missing trip identity, unconfigured key, upstream error --
// degrades to the generic message on both versions: V1 here is the
// spec-sanctioned thin alias of V2, so V1's historical unstructured 500 is
// deliberately not reproduced.
func (h *alarmsHandler) composeMessage(ctx context.Context, region regions.Region,
	stopID, tripID string, serviceDate int64, vehicleID string, stopSeq *int64,
	secondsBefore int64) string {

	if h.deps.OBA == nil || stopID == "" || tripID == "" || serviceDate == 0 {
		return alarms.GenericMessage(secondsBefore)
	}
	dep, err := h.deps.OBA.ArrivalAndDeparture(ctx, region, obaapi.DepartureQuery{
		StopID: stopID, TripID: tripID, ServiceDate: serviceDate,
		VehicleID: vehicleID, StopSequence: stopSeq,
	})
	if err != nil || dep.RouteShortName == "" || dep.TripHeadsign == "" {
		return alarms.GenericMessage(secondsBefore)
	}
	return alarms.ComposeMessage(dep.RouteShortName, dep.TripHeadsign, secondsBefore)
}

// alarmURL builds the §2.4 creation-response URL. The region's directory
// sidecarBaseUrl wins; a region without one falls back to this request's
// Host over https (the only scheme the apps will talk to us on).
func alarmURL(region regions.Region, r *http.Request, version int, token string) string {
	base := strings.TrimRight(region.SidecarBaseURL, "/")
	if base == "" {
		base = "https://" + r.Host
	}
	return fmt.Sprintf("%s/api/v%d/regions/%d/alarms/%s", base, version, region.ID, token)
}

func (h *alarmsHandler) delete(w http.ResponseWriter, r *http.Request) {
	region, ok := resolveRegion(w, r, h.deps)
	if !ok {
		return
	}
	err := h.deps.Alarms.Delete(r.Context(), region.ID, r.PathValue("alarmToken"))
	switch {
	case errors.Is(err, alarms.ErrNotFound):
		w.WriteHeader(http.StatusNotFound)
	case err != nil:
		// 204 is a binding "it's cancelled" (spec §2.5); a failed delete
		// must surface as a 5xx, never a false positive.
		writeServerError(w, h.deps.Logger, region.ID, "delete alarm", err)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}
```

Router wiring (inside `NewRouter`):

```go
	if deps.Alarms != nil {
		// The V2 side-effect upsert (spec §5.2) needs the registry, and the
		// handlers deref Regions and Now; failing at boot beats a nil deref
		// on the first alarm.
		if deps.PushRegs == nil || deps.Now == nil || deps.Regions == nil {
			panic("httpapi: Deps.PushRegs, Deps.Now, and Deps.Regions required when Deps.Alarms is set")
		}
		ah := &alarmsHandler{deps: deps}
		mux.HandleFunc("POST /api/v1/regions/{regionId}/alarms", ah.create(1))
		mux.HandleFunc("POST /api/v2/regions/{regionId}/alarms", ah.create(2))
		mux.HandleFunc("DELETE /api/v1/regions/{regionId}/alarms/{alarmToken}", ah.delete)
		mux.HandleFunc("DELETE /api/v2/regions/{regionId}/alarms/{alarmToken}", ah.delete)
	}
```

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/httpapi/` then `go test ./...` — PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/httpapi
git commit -m "feat: alarm registration and cancellation endpoints, V1 and V2 (spec §5.1-§5.2)"
```

---

### Task 12: Alarm firing scheduler

**Files:**
- Create: `internal/alarms/scheduler.go`
- Test: `internal/alarms/scheduler_test.go`

**Interfaces:**
- Consumes: `alarms.Repository`, `regions.Repository`, `push.Sender`, `obaapi` Departure types.
- Produces (consumed by Task 14):

```go
// DepartureSource is the one obaapi method the scheduler needs, declared
// consumer-side so tests fake three lines instead of the whole Client.
type DepartureSource interface {
	ArrivalAndDeparture(ctx context.Context, region regions.Region, q obaapi.DepartureQuery) (obaapi.Departure, error)
}

type Scheduler struct {
	Repo    Repository
	Regions regions.Repository
	OBA     DepartureSource
	// Sender may be nil (store-only mode: no push transport configured).
	// The lifecycle bookkeeping -- expiry, reaping -- still runs; only the
	// fire step is skipped, leaving the alarm to expire.
	Sender push.Sender
	Now    func() time.Time
	Logger *slog.Logger
}

// CheckAll runs one §5.3 cycle over every pending alarm. Exported so tests
// (and Task 14's loop) drive cycles without a ticker.
func (s *Scheduler) CheckAll(ctx context.Context)

// RunLoop calls CheckAll every interval until ctx is done (§5.3: once per
// minute). Mirrors regions.RunSyncLoop's shape.
func (s *Scheduler) RunLoop(ctx context.Context, interval time.Duration)

const maxLookupFailures = 3
```

- [ ] **Step 1: Write the failing tests**

Fakes in `scheduler_test.go`: `fakeAlarmRepo` (in-memory map implementing `Repository`), `fakeRegions` (fixed map), `fakeOBA` (func field), `fakeSender` (records `[]push.Notification`, injectable error). Fixed `base` time. Tests, each building one alarm and calling `CheckAll` once unless noted:

- `TestFiresAndDeletes`: predicted departure at `base + 300s`, `seconds_before 600` → sender got exactly one notification: token, platform iOS (from `operating_system`), `Sandbox` from alarm, `Title "OneBusAway"`, `Message` = stored message, `Data` = `alarm.PushData()`; alarm deleted.
- `TestUsesPredictedOverScheduled`: predicted `base+300s`, scheduled `base+3000s` → fires (predicted wins). `TestFallsBackToScheduled`: predicted 0/Predicted false, scheduled `base+300s` → fires.
- `TestWaitsWhenFar`: departure `base+700s`, before 600 → no send, alarm survives, and a pre-set `FailureCount 2` was reset to 0 (spec §5.3 step 3).
- `TestExpiredDeletesWithoutPush`: departure `base-60s` → deleted, zero sends (spec §5.3 step 4).
- `TestNotFoundCountsAndReapsAtThree`: fakeOBA returns `obaapi.ErrNotFound`; cycle 1 and 2 → alarm survives with count 1,2; cycle 3 → deleted, zero sends.
- `TestTransientErrorDoesNotCount`: fakeOBA returns a plain error → alarm survives, `FailureCount` unchanged (spec §5.3: transient errors don't count).
- `TestSuccessResetsStreak`: count 2, then a successful too-early lookup → count 0.
- `TestSendFailureKeepsAlarm`: sender errors → alarm survives (retries next cycle; delete only after the push send returns, spec §12 at-least-once).
- `TestNilSenderLeavesAlarm`: `Sender: nil` (store-only mode, no push transport configured), fire-window alarm → no panic, alarm survives; an expired alarm is still deleted and a 3-strike streak still reaps — the scheduler's lifecycle bookkeeping must run even when nothing can be pushed, or an unconfigured deployment grows the table without bound (spec §13 gives alarms a bounded lifetime).
- `TestMissingRegionCountsAsFailure`: alarm whose region the repo doesn't know → treated like a failed lookup (counts toward the streak).
- `TestAndroidPlatform`: `operating_system "android"` → `Platform == push.PlatformAndroid`.
- `TestRunLoopStopsOnContextCancel`: `RunLoop` with 1ms interval and a canceled-after-10ms context returns; no goroutine leak (just assert it returns).

- [ ] **Step 2: Run to verify fail**

Run: `go test ./internal/alarms/` — FAIL (Scheduler undefined).

- [ ] **Step 3: Implement**

```go
// checkConcurrency bounds parallel OBA lookups per cycle; alarms across
// riders are independent, but the upstream deserves the same politeness as
// the vehicle fan-out.
const checkConcurrency = 8

func (s *Scheduler) CheckAll(ctx context.Context) {
	pending, err := s.Repo.List(ctx)
	if err != nil {
		s.Logger.Error("alarms: list pending", "err", err)
		return
	}
	// One region fetch per region per cycle, not per alarm.
	regionCache := make(map[int64]*regions.Region)
	var mu sync.Mutex
	regionFor := func(id int64) *regions.Region {
		mu.Lock()
		defer mu.Unlock()
		if r, ok := regionCache[id]; ok {
			return r
		}
		region, err := s.Regions.Get(ctx, id)
		if err != nil {
			regionCache[id] = nil
			return nil
		}
		regionCache[id] = &region
		return &region
	}

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(checkConcurrency)
	for _, alarm := range pending {
		g.Go(func() error {
			s.check(gctx, alarm, regionFor(alarm.RegionID))
			return nil
		})
	}
	_ = g.Wait()
}

func (s *Scheduler) check(ctx context.Context, alarm Alarm, region *regions.Region) {
	if region == nil {
		// An alarm whose region vanished can never resolve; let the streak
		// reap it rather than re-checking forever.
		s.countFailure(ctx, alarm)
		return
	}
	dep, err := s.OBA.ArrivalAndDeparture(ctx, *region, obaapi.DepartureQuery{
		StopID: alarm.StopID, TripID: alarm.TripID, ServiceDate: alarm.ServiceDate,
		VehicleID: alarm.VehicleID, StopSequence: alarm.StopSequence,
	})
	switch {
	case errors.Is(err, obaapi.ErrNotFound), errors.Is(err, obaapi.ErrNotConfigured):
		// Trip aged out (or the region has no key and never will resolve):
		// both count toward the §5.3 reaping streak.
		s.countFailure(ctx, alarm)
		return
	case err != nil:
		// Transient upstream/network failure: deliberately uncounted.
		s.Logger.Warn("alarms: lookup failed", "region_id", alarm.RegionID, "err", err)
		return
	}

	if alarm.FailureCount > 0 {
		if err := s.Repo.ResetFailures(ctx, alarm.ID); err != nil {
			s.Logger.Warn("alarms: reset failures", "err", err)
		}
	}

	departureMs := dep.ScheduledDepartureTime
	if dep.Predicted && dep.PredictedDepartureTime > 0 {
		departureMs = dep.PredictedDepartureTime
	}
	until := departureMs/1000 - s.Now().Unix()

	switch Decide(until, alarm.SecondsBefore) {
	case Wait:
		return
	case Expire:
		// The bus already left; waking the rider is worse than silence.
		if err := s.Repo.DeleteByID(ctx, alarm.ID); err != nil {
			s.Logger.Warn("alarms: delete expired", "err", err)
		}
	case Fire:
		if s.Sender == nil {
			// Store-only mode (no push transport configured): leave the
			// alarm; the Expire branch bounds its lifetime, and the boot
			// warning already told the operator pushes cannot happen.
			return
		}
		platform := push.PlatformIOS
		if alarm.OperatingSystem == pushreg.OSAndroid {
			platform = push.PlatformAndroid
		}
		err := s.Sender.Send(ctx, push.Notification{
			Tokens: []string{alarm.UserPushID}, Platform: platform,
			Sandbox: alarm.APNSSandbox, Title: "OneBusAway",
			Message: alarm.Message, Data: alarm.PushData(),
		})
		if err != nil {
			// Keep the alarm: it retries next cycle until the departure
			// passes. At-least-once beats losing the wake-up (spec §12).
			s.Logger.Error("alarms: push send failed", "region_id", alarm.RegionID, "err", err)
			return
		}
		// Delete only after the send returned: a crash in the gap re-fires,
		// which is the accepted duplicate (spec §12).
		if err := s.Repo.DeleteByID(ctx, alarm.ID); err != nil {
			s.Logger.Error("alarms: delete fired alarm", "err", err)
		}
	}
}

func (s *Scheduler) countFailure(ctx context.Context, alarm Alarm) {
	streak, err := s.Repo.RecordFailure(ctx, alarm.ID)
	if err != nil {
		s.Logger.Warn("alarms: record failure", "err", err)
		return
	}
	if streak >= maxLookupFailures {
		if err := s.Repo.DeleteByID(ctx, alarm.ID); err != nil {
			s.Logger.Warn("alarms: reap unresolvable", "err", err)
			return
		}
		s.Logger.Info("alarms: reaped unresolvable alarm", "region_id", alarm.RegionID, "failures", streak)
	}
}

func (s *Scheduler) RunLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.CheckAll(ctx)
		}
	}
}
```

(`time.NewTicker` here matches the allowance `regions.RunSyncLoop` already relies on — tickers are not `time.Now`; confirm against that file's lint treatment and mirror it exactly.)

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/alarms/` and `-race` — PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/alarms
git commit -m "feat: minute-cadence alarm firing scheduler (spec §5.3-§5.4)"
```

---

### Task 13: gorush feedback webhook

**Files:**
- Create: `internal/httpapi/feedback.go`
- Modify: `internal/httpapi/router.go`
- Test: `internal/httpapi/feedback_test.go`

**Interfaces:**
- Consumes: `push.IsTerminal` (Task 10), `Deps.PushRegs.DeleteByToken` (Task 4).
- Produces: route `POST /webhooks/gorush` (registered when `Deps.PushRegs != nil`). Spec §14 leaves webhook paths transport-specific; §6.5/§4 define the required behavior: a terminal APNs reason deletes the alert-token registration so reach counts stay honest.

- [ ] **Step 1: Write the failing tests**

- `TestFeedbackTerminalDeletesRegistration`: seed registration; POST `{"type":"failed-push","platform":"ios","token":"tok1","message":"...","error":"Unregistered"}` → 200; registration gone (from every region carrying it).
- `TestFeedbackTransientKeepsRegistration`: `"error":"ExpiredProviderToken"` → 200, registration survives.
- `TestFeedbackUnknownTokenIsOK`: terminal error for an unregistered token → 200 (feedback races opt-outs; nothing to do is success).
- `TestFeedbackMalformedBodyIs400`: garbage body → 400.
- `TestFeedbackNeverLogsToken`: capture logs; the token string does not appear.

- [ ] **Step 2: Run to verify fail**

Run: `go test ./internal/httpapi/ -run TestFeedback` — FAIL.

- [ ] **Step 3: Implement**

```go
// gorushFeedback is gorush's failed-push webhook payload. Only token and
// error matter here; the rest is logged context.
type gorushFeedback struct {
	Type     string `json:"type"`
	Platform string `json:"platform"`
	Token    string `json:"token"`
	Error    string `json:"error"`
}

type feedbackHandler struct{ deps Deps }

// receive consumes async delivery feedback (spec §6.5). Deleting a
// registration requires only knowing its token -- exactly the power the
// public opt-out DELETE already grants -- so this endpoint being
// unauthenticated adds no new capability.
func (h *feedbackHandler) receive(w http.ResponseWriter, r *http.Request) {
	var fb gorushFeedback
	if err := decodeJSON(w, r, 64<<10, &fb); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if fb.Token == "" || !push.IsTerminal(fb.Error) {
		w.WriteHeader(http.StatusOK)
		return
	}
	n, err := h.deps.PushRegs.DeleteByToken(r.Context(), fb.Token)
	if err != nil {
		h.deps.Logger.Error("httpapi: delete registration from feedback", "err", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if n > 0 {
		h.deps.Logger.Info("httpapi: pruned dead push token",
			"platform", fb.Platform, "reason", fb.Error, "registrations", n)
	}
	w.WriteHeader(http.StatusOK)
}
```

Router (inside the existing `deps.PushRegs != nil` block): `mux.HandleFunc("POST /webhooks/gorush", (&feedbackHandler{deps: deps}).receive)` — deliberately outside the throttle (gorush is our own infrastructure, and throttling it would drop prune signals).

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/httpapi/` — PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/httpapi
git commit -m "feat: gorush feedback webhook prunes terminally-dead push tokens (spec §6.5)"
```

---

### Task 14: Prune loop, cmd wiring, README

**Files:**
- Create: `internal/pushreg/prune.go`, `internal/pushreg/prune_test.go`
- Modify: `cmd/sidecar/main.go`, `cmd/sidecar/main_test.go`
- Modify: `README.md`

**Interfaces:**
- Consumes: everything above.
- Produces: `pushreg.RunPruneLoop(ctx context.Context, repo Repository, interval, maxAge time.Duration, now func() time.Time, logger *slog.Logger)`; flags `--gorush-url` / `SIDECAR_GORUSH_URL`.

- [ ] **Step 1: Write the failing prune-loop test**

`prune_test.go`: fake repo recording `Prune` calls; `RunPruneLoop(ctx, repo, 5*time.Millisecond, 180*24*time.Hour, func() time.Time { return base }, logger)` in a goroutine; after ~25ms cancel ctx; assert ≥1 call, each with cutoff exactly `base.Add(-180 * 24 * time.Hour)`, and that the function returned. Also assert a Prune error is logged and the loop continues (second call still happens).

- [ ] **Step 2: Run to verify fail**

Run: `go test ./internal/pushreg/` — FAIL.

- [ ] **Step 3: Implement prune loop**

```go
// RunPruneLoop deletes registrations unseen for maxAge, every interval,
// until ctx is done (spec §4: 180 days; §12: daily). Mirrors
// regions.RunSyncLoop: an immediate first pass, then the ticker, so a
// long-stopped deployment catches up at boot instead of a day later.
func RunPruneLoop(ctx context.Context, repo Repository, interval, maxAge time.Duration,
	now func() time.Time, logger *slog.Logger) {

	prune := func() {
		n, err := repo.Prune(ctx, now().Add(-maxAge))
		if err != nil {
			logger.Error("pushreg: prune", "err", err)
			return
		}
		if n > 0 {
			logger.Info("pushreg: pruned stale registrations", "count", n)
		}
	}
	prune()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			prune()
		}
	}
}
```

(Match `regions.RunSyncLoop`'s actual shape — read it first and mirror its structure, including whether it runs an immediate first pass.)

- [ ] **Step 4: Wire cmd/sidecar**

Constants:

```go
	alarmCheckInterval  = time.Minute        // spec §5.3
	pushRegPruneEvery   = 24 * time.Hour     // spec §12
	pushRegMaxAge       = 180 * 24 * time.Hour // spec §4
```

Flag beside the others: `gorushURL := fs.String("gorush-url", envOrDefault("SIDECAR_GORUSH_URL", ""), "base URL of the gorush push gateway; without it alarms are stored but never fire")`.

`buildDeps` gains parameters/fields: `PushRegs: store.PushRegs()`, `Alarms: store.Alarms()`, `OBA: obaClient` (hoist the existing `obaapi.New` result into a variable shared with vehicles). In `run`, after the sync loop:

```go
	go pushreg.RunPruneLoop(ctx, store.PushRegs(), pushRegPruneEvery, pushRegMaxAge, time.Now, logger)

	// The scheduler always runs, even with no push transport: its Expire
	// branch and 3-strike reaping are what bound the alarms table (spec
	// §13); only the fire step needs a sender.
	var sender push.Sender
	if *gorushURL == "" {
		logger.Warn("no --gorush-url/SIDECAR_GORUSH_URL set; departure alarms will be stored and reaped but never fire")
	} else {
		sender = push.NewGorush(*gorushURL, http.DefaultClient, logger)
	}
	sched := &alarms.Scheduler{
		Repo:    store.Alarms(),
		Regions: store.Regions(),
		OBA:     deps.OBA,
		Sender:  sender,
		Now:     time.Now,
		Logger:  logger,
	}
	go sched.RunLoop(ctx, alarmCheckInterval)
```

(`deps` here is the `buildDeps` result — hoist it into a variable in `run` instead of constructing it inline in the `ServerConfig` literal, so the scheduler shares the same `obaapi.Client`.)

`main_test.go`: extend the existing `buildDeps` test to assert `PushRegs`, `Alarms`, and `OBA` are non-nil (the loud-panic contract in the router depends on it).

- [ ] **Step 5: README**

Add to the configuration table: `SIDECAR_GORUSH_URL` (gorush base URL; alarms don't fire without it). Add a deployment note: the push-registration throttle keys on the TCP peer address, so a reverse proxy must be deployed in a mode that preserves it (or accept per-proxy limiting); gorush's feedback webhook should be pointed at `POST /webhooks/gorush`.

- [ ] **Step 6: Run everything**

Run: `go test ./...`, then `make check` (needs the SPA build; run it once at the end).
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/pushreg cmd/sidecar README.md
git commit -m "feat: wire alarm scheduler, gorush sender, and registration pruning into cmd/sidecar"
```

---

## Self-Review

**Spec coverage (§4):** upsert keyed (region, token) with sticky fields → Tasks 4+6; `apns_sandbox` §2.7 parsing → Task 5; validation incl. token 4096 / description-required → Task 6; opt-out DELETE → Task 6; 30/min/IP shared-bucket throttle → Tasks 2+6; locale normalization algorithm → Task 3 (applied at fan-out; judgment call documented); 180-day retention → Tasks 4+14; terminal-feedback pruning → Tasks 10+13; concurrent-first-registration race → Task 4 (atomic upsert + race test). Alert fan-out is explicitly deferred (needs out-of-scope authoring trigger) — flagged for the PR description.

**Spec coverage (§5):** V1+V2 endpoints with exact wire differences (OS default, no sandbox on V1, dedupe incl. race, no V1 side effect) → Tasks 8+11; `seconds_before` 600 default → Tasks 7+11; creation-time message with degrade-to-generic → Tasks 9+11; loose validation (only user_push_id/OS enforced) → Task 11; V2 side-effect registration upsert without sandbox/locale → Task 11; secure-token URLs honoring `sidecarBaseUrl` and slug region segments → Tasks 1+11; firing loop (minute cadence, predicted-else-scheduled, fire-once-then-delete, expire-silently, 3-strike reaping, transient errors uncounted, at-least-once ordering) → Task 12; §5.4 payload key set with nulls and public region id → Task 7; cancellation 204/404 → Tasks 8+11.

**Judgment calls surfaced for review** (also to be listed in the PR): V1 as thin alias (spec-sanctioned; V1 lookup failure degrades instead of 500ing), raw-locale storage with fan-out-time normalization, blank-string sticky fields treated as absent, test-device/description invariants validated against the merged row (a re-POST without description keeps the stored one instead of 422ing), `ErrNotConfigured` counting toward the alarm reaping streak, `ExpiredToken` excluded from terminal reasons (spec's list kept verbatim), gorush as the only bundled transport, scheduler running in store-only mode when no transport is configured (bounds table growth; fire step skipped).

**Type consistency:** `pushreg.Upsert` pointer semantics used identically in Tasks 4, 6, 11; `alarms.NewAlarm`/`V1Key` field names match between Tasks 7, 8, 11; `push.Notification` fields match between Tasks 10 and 12; `obaapi.DepartureQuery`/`Departure` match between Tasks 9, 11, 12; `Deps` field names (`PushRegs`, `PushLimiter`, `Alarms`, `OBA`) consistent across Tasks 6, 11, 13, 14.
