# OBACloud Prerequisites (Migration Phase 0) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give the Go sidecar everything OBACloud's migration needs before any Rails work starts: push-scoped region keys, caller-supplied push copy, translation staleness on the wire, stdin import, id-sequence headroom, and question-id preservation on survey PUT, with the design spec and README amended to match.

**Architecture:** Every change lands in an existing layer. `internal/apikey` gains a `Scopes` domain type stored as a JSON array in a new `region_api_keys.scopes` column; the route table gains an `operatorOrPushKey` allow-list that admits a region key only when it carries the `push` scope. `alertpush.Enqueuer` accepts optional caller copy validated by the same caps the mechanical derivation observes. The survey `Definition` carries optional question ids so the SQLite adapter can re-insert kept questions under their original ids. Two CLI additions (`import --file -`, `sequence show|bump`) are plain `flag` subcommands in `cmd/sidecar-admin`.

**Tech Stack:** Go 1.26, `net/http` stdlib mux, sqlc 1.31.1 over goose migrations (SQLite via modernc), golangci-lint 2.12.2 (`make check`), SvelteKit SPA untouched.

**Spec:** `/Users/aaron/repos/onebusaway/obacloud/docs/superpowers/specs/2026-08-28-sidecar-migration-design.md` §0.2 and §2 (2.1–2.8). The sidecar-side contract it amends is `docs/superpowers/specs/2026-08-26-region-api-keys-and-admin-api-design.md` (referred to below as "the keys spec").

## Global Constraints

- Repo: `/Users/aaron/repos/onebusaway/sidecar`. Read its `CLAUDE.md` first; every rule there applies to every task.
- **`time.Now` and `time.Local` are banned outside `cmd/` and `_test.go`** (forbidigo). Handlers use `deps.Now()`; repositories take `now time.Time`.
- **Every timestamp column is INTEGER epoch seconds.** No new DATETIME/TEXT time columns.
- **sqlc traps:** never mix `sqlc.arg()` with bare `?` in one query; `queries/*.sql` comments are ASCII-only (write "spec section N", never a section sign); no parameter inside an `IN (...)` list. After touching `queries/*.sql` or `migrations/*.sql` run `make generate` and commit `gen/`.
- **`revive` requires doc comments on every exported identifier**; `nolint` needs a specific linter and a reason.
- **Admin routes live only in the `adminRoutes` table** (`internal/httpapi/router.go`); tests walk it.
- **Shipped-client status codes are contracts.** Nothing in this plan changes a rider-facing response code; the only new codes are admin-API 400/422 for malformed bodies.
- **Every new test must be able to fail:** after it passes, mutate the code under test once and confirm the assertion fires, then revert.
- Timezone-dependent assertions must hold under `TZ=UTC` and `TZ=Asia/Kathmandu` (`make test-tz`).
- Region ids: `0` is a real region (Tampa Bay). Never treat `0` as "unset".
- Design-spec citations in code comments are "design spec §N" (or "spec section N" in `.sql`), pointing at the feature's design doc, not `specification/specification.md`.
- Commit after every task with a message in the repo's existing style (`area: imperative summary`).
- Final gate for the whole plan: `make check` green. (`go test ./...` needs `make web` once for the `adminui` embed assertion.)

---

## File map

| File | Responsibility in this plan |
|---|---|
| `internal/apikey/scopes.go` (new) | `Scope`, `Scopes`, `ParseScopes`, `ErrUnknownScope` |
| `internal/apikey/apikey.go` | `RegionKey.Scopes`; `Repository.CreateRegionKey` gains `scopes` |
| `internal/store/sqlite/migrations/00013_region_api_key_scopes.sql` (new) | `scopes TEXT NOT NULL DEFAULT '[]'` |
| `internal/store/sqlite/queries/apikeys.sql` | `CreateRegionAPIKey` inserts `scopes` |
| `internal/store/sqlite/apikeys.go` | encode/decode scopes; `regionKeyFromRow` returns an error |
| `internal/store/sqlite/sequences.go` (new) | `SequenceTables`, `Sequences`, `BumpSequences` |
| `internal/store/storetest/apikeytest.go` | scopes round trip |
| `internal/httpapi/principal.go` | `principalPushKey`, `principalSet.admits`, `principal.scopes`, `operatorOrPushKey` |
| `internal/httpapi/middleware.go` | `requirePrincipal` uses `admits` |
| `internal/httpapi/bearer.go` | region-key principal carries scopes |
| `internal/httpapi/router.go` | push create/cancel → `operatorOrPushKey` |
| `internal/httpapi/admin_apikeys.go` | `scopes` in mint request, mint response, list |
| `internal/httpapi/admin_pushes.go` | optional `messages` on create; 400 mapping |
| `internal/httpapi/admin_alerts.go` | `translationJSON.Stale` |
| `internal/httpapi/admin_surveys.go` | 422 for `surveys.ErrUnknownQuestion` |
| `internal/alerts/alert.go` | `Alert.TranslationStale` |
| `internal/alertpush/messages.go` | `ValidateMessages`, `ErrInvalidMessages` |
| `internal/alertpush/enqueue.go` | `Enqueue` takes optional `Messages` |
| `internal/surveys/surveys.go`, `definition.go`, `codec.go` | `QuestionDefinition.ID`, `ErrUnknownQuestion`, `QuestionsEqual` with ids |
| `internal/store/sqlite/queries/surveys.sql`, `surveys.go` | `InsertQuestionWithID`; id-preserving replace |
| `cmd/sidecar-admin/keys.go` | `key create --scope`, `key list` scopes column |
| `cmd/sidecar-admin/import.go` | `--file -` reads stdin |
| `cmd/sidecar-admin/sequence.go` (new) | `sequence show`, `sequence bump --min N` |
| `cmd/sidecar-admin/commands.go` | dispatch for `sequence`; `runImport` gets stdin |
| `docs/superpowers/specs/2026-08-26-region-api-keys-and-admin-api-design.md`, `README.md` | amendments (§0.2 of the migration spec) |

---

### Task 1: `scopes` on region API keys — domain, migration, store, CLI

**Files:**
- Create: `internal/apikey/scopes.go`, `internal/apikey/scopes_test.go`
- Create: `internal/store/sqlite/migrations/00013_region_api_key_scopes.sql`
- Modify: `internal/apikey/apikey.go` (`RegionKey`, `LogValue`, `Repository.CreateRegionKey`)
- Modify: `internal/store/sqlite/queries/apikeys.sql` (`CreateRegionAPIKey`)
- Modify: `internal/store/sqlite/apikeys.go`
- Modify: `internal/store/storetest/apikeytest.go`
- Modify: `cmd/sidecar-admin/keys.go`, `cmd/sidecar-admin/keys_test.go`
- Modify (compile fixes, add a `nil` scopes argument): `internal/httpapi/admin_apikeys.go`, `internal/httpapi/bearer_test.go`, `internal/httpapi/admin_alerts_test.go`, `cmd/sidecar-admin/keys_test.go`

**Interfaces:**
- Produces: `apikey.Scope` (string), `apikey.ScopePush = "push"`, `apikey.Scopes []Scope` with `Has(Scope) bool` and `Strings() []string`, `apikey.ParseScopes([]string) (Scopes, error)`, `apikey.ErrUnknownScope`.
- Produces: `apikey.RegionKey.Scopes Scopes`.
- Produces: `Repository.CreateRegionKey(ctx, regionID int64, name, keyHash string, scopes Scopes, by Actor, now time.Time) (RegionKey, error)` — new `scopes` parameter between `keyHash` and `by`.
- Produces: `sidecar-admin key create --region N --name NAME [--scope push ...]`; `key list` prints a final tab column `scopes` (`push` or `—`).

- [ ] **Step 1: Write the failing domain test**

Create `internal/apikey/scopes_test.go`:

```go
package apikey_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/OneBusAway/sidecar/internal/apikey"
)

func TestParseScopes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		in      []string
		want    apikey.Scopes
		wantErr error
	}{
		{"nil is empty, never nil", nil, apikey.Scopes{}, nil},
		{"empty is empty", []string{}, apikey.Scopes{}, nil},
		{"push", []string{"push"}, apikey.Scopes{apikey.ScopePush}, nil},
		{"duplicates collapse", []string{"push", "push"}, apikey.Scopes{apikey.ScopePush}, nil},
		{"surrounding whitespace is trimmed", []string{" push "}, apikey.Scopes{apikey.ScopePush}, nil},
		{"unknown", []string{"admin"}, nil, apikey.ErrUnknownScope},
		{"case matters", []string{"Push"}, nil, apikey.ErrUnknownScope},
		{"blank", []string{""}, nil, apikey.ErrUnknownScope},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := apikey.ParseScopes(tc.in)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if tc.wantErr == nil && (got == nil || !reflect.DeepEqual(got, tc.want)) {
				t.Errorf("scopes = %#v, want %#v", got, tc.want)
			}
		})
	}
}

func TestScopes_HasAndStrings(t *testing.T) {
	t.Parallel()
	var none apikey.Scopes
	if none.Has(apikey.ScopePush) {
		t.Error("nil Scopes must not have push")
	}
	if got := none.Strings(); got == nil || len(got) != 0 {
		t.Errorf("nil Scopes.Strings() = %#v, want an empty non-nil slice", got)
	}
	s := apikey.Scopes{apikey.ScopePush}
	if !s.Has(apikey.ScopePush) {
		t.Error("Has(push) = false")
	}
	if got := s.Strings(); !reflect.DeepEqual(got, []string{"push"}) {
		t.Errorf("Strings() = %#v", got)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/apikey -run 'TestParseScopes|TestScopes_' -v`
Expected: FAIL to compile — `undefined: apikey.ParseScopes`, `apikey.Scopes`.

- [ ] **Step 3: Write the domain type**

Create `internal/apikey/scopes.go`:

```go
package apikey

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Scope is one named capability a region key carries on top of the
// ordinary region-scoped authoring surface. A key with no scopes is the
// key the keys design spec section 2.1 describes; each scope widens its
// blast radius by exactly one stated thing, so the set is closed and every
// name is spelled here.
type Scope string

// ScopePush lets a region key send and cancel alert pushes for its own
// region (POST/DELETE .../pushes). It exists for OBACloud, which drives the
// sidecar's push routes at send time (migration design spec section 2.2);
// a leaked push-scoped key can deliver notifications to every device in
// its region, and the remedy remains revocation.
const ScopePush Scope = "push"

// ErrUnknownScope is returned by ParseScopes for any name that is not a
// defined Scope. It is a client error (400 at the admin API), never a
// silently dropped value: a key minted without the scope the caller asked
// for would fail later, at send time, in a different process.
var ErrUnknownScope = errors.New("unknown scope")

// Scopes is a key's scope set: sorted, deduplicated, and never nil once it
// has been through ParseScopes, so it marshals as [] rather than null.
type Scopes []Scope

// ParseScopes validates and normalizes a list of scope names. Whitespace
// around a name is trimmed; anything else that is not exactly a defined
// scope is ErrUnknownScope. nil and empty input both yield an empty,
// non-nil set.
func ParseScopes(names []string) (Scopes, error) {
	seen := make(map[Scope]bool, len(names))
	out := Scopes{}
	for _, n := range names {
		s := Scope(strings.TrimSpace(n))
		if s != ScopePush {
			return nil, fmt.Errorf("%w %q", ErrUnknownScope, n)
		}
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

// Has reports whether want is in the set. A nil set has nothing.
func (s Scopes) Has(want Scope) bool {
	for _, got := range s {
		if got == want {
			return true
		}
	}
	return false
}

// Strings renders the set for JSON and the CLI: always a non-nil slice,
// so an empty set is [] on the wire.
func (s Scopes) Strings() []string {
	out := make([]string, 0, len(s))
	for _, sc := range s {
		out = append(out, string(sc))
	}
	return out
}
```

- [ ] **Step 4: Run the domain test to verify it passes**

Run: `go test ./internal/apikey -run 'TestParseScopes|TestScopes_' -v`
Expected: PASS.

- [ ] **Step 5: Add `Scopes` to `RegionKey` and the repository signature**

In `internal/apikey/apikey.go`:

Add the field to `RegionKey` after `KeyHash`:

```go
	KeyHash    string
	// Scopes widens what the key may do beyond the region-scoped authoring
	// surface; see Scope. Empty for every key minted before scopes existed.
	Scopes     Scopes
	CreatedBy  Actor
```

In `RegionKey.LogValue`, add after the `name` attribute:

```go
		slog.Any("scopes", k.Scopes.Strings()),
```

Change the `Repository` method signature and its comment:

```go
	// CreateRegionKey mints a key. scopes is stored as given (already
	// normalized by ParseScopes); nil is an empty set.
	CreateRegionKey(ctx context.Context, regionID int64, name, keyHash string, scopes Scopes, by Actor, now time.Time) (RegionKey, error)
```

- [ ] **Step 6: Write the migration and regenerate sqlc**

Create `internal/store/sqlite/migrations/00013_region_api_key_scopes.sql`:

```sql
-- +goose Up
-- Region key scopes (migration design spec section 2.2). A JSON array of
-- scope names; the only defined scope is "push". Existing keys get [] and
-- so keep exactly the reach they had.
ALTER TABLE region_api_keys ADD COLUMN scopes TEXT NOT NULL DEFAULT '[]';

-- +goose Down
ALTER TABLE region_api_keys DROP COLUMN scopes;
```

In `internal/store/sqlite/queries/apikeys.sql`, replace the `CreateRegionAPIKey` statement with:

```sql
-- name: CreateRegionAPIKey :one
INSERT INTO region_api_keys (
  region_id, name, key_hash, scopes, created_by_kind, created_by_id, created_at
) VALUES (
  sqlc.arg(region_id), sqlc.arg(name), sqlc.arg(key_hash), sqlc.arg(scopes),
  sqlc.arg(created_by_kind), sqlc.arg(created_by_id), sqlc.arg(created_at)
)
RETURNING *;
```

Run: `make generate && git status --short internal/store/sqlite/gen`
Expected: `gen/models.go` (`RegionApiKey.Scopes string`) and `gen/apikeys.sql.go` (`CreateRegionAPIKeyParams.Scopes`) modified.

- [ ] **Step 7: Write the failing store conformance test**

In `internal/store/storetest/apikeytest.go`, register a new subtest in `RunAPIKeyRepository` after `"CreateGetRoundTrip"`:

```go
	t.Run("ScopesRoundTrip", func(t *testing.T) { testAPIKeyScopes(t, newStore) })
```

Add the function at the end of the file:

```go
// testAPIKeyScopes pins that scopes survive every read path -- Create's
// return, GetByHash, ListRegionKeys, ListRegionKeysByCreator -- and that a
// key minted with nil scopes reads back as an empty, non-nil set. A key
// that silently lost its push scope would 403 at send time in another
// process, which is the failure the migration design spec section 2.2
// calls out.
func testAPIKeyScopes(t *testing.T, newStore newAPIKeyStoreFunc) {
	keys, regionRepo := newStore(t)
	seedAPIKeyRegions(t, regionRepo)
	ctx := context.Background()
	by := apikey.Actor{Kind: apikey.ActorOperator, ID: 9}

	push, err := keys.CreateRegionKey(ctx, 1, "push", "hash-push", apikey.Scopes{apikey.ScopePush}, by, base)
	if err != nil {
		t.Fatalf("CreateRegionKey(push): %v", err)
	}
	plain, err := keys.CreateRegionKey(ctx, 1, "plain", "hash-plain", nil, by, base)
	if err != nil {
		t.Fatalf("CreateRegionKey(plain): %v", err)
	}
	if !push.Scopes.Has(apikey.ScopePush) {
		t.Errorf("created push key scopes = %v, want push", push.Scopes)
	}
	if plain.Scopes == nil || len(plain.Scopes) != 0 {
		t.Errorf("created plain key scopes = %#v, want empty non-nil", plain.Scopes)
	}

	got, err := keys.GetRegionKeyByHash(ctx, "hash-push")
	if err != nil {
		t.Fatalf("GetRegionKeyByHash: %v", err)
	}
	if !got.Scopes.Has(apikey.ScopePush) {
		t.Errorf("GetRegionKeyByHash scopes = %v, want push", got.Scopes)
	}

	list, err := keys.ListRegionKeys(ctx, 1)
	if err != nil {
		t.Fatalf("ListRegionKeys: %v", err)
	}
	var sawPush, sawPlain bool
	for _, k := range list {
		switch k.ID {
		case push.ID:
			sawPush = k.Scopes.Has(apikey.ScopePush)
		case plain.ID:
			sawPlain = k.Scopes != nil && len(k.Scopes) == 0
		}
	}
	if !sawPush || !sawPlain {
		t.Errorf("ListRegionKeys lost scopes: %+v", list)
	}

	byCreator, err := keys.ListRegionKeysByCreator(ctx, by)
	if err != nil {
		t.Fatalf("ListRegionKeysByCreator: %v", err)
	}
	for _, k := range byCreator {
		if k.ID == push.ID && !k.Scopes.Has(apikey.ScopePush) {
			t.Errorf("ListRegionKeysByCreator lost the push scope: %+v", k)
		}
	}
}
```

Update every existing `CreateRegionKey(` call in `apikeytest.go` to pass `nil` as the new fourth argument (between the hash and the actor). Find them with:

Run: `grep -n 'CreateRegionKey(' internal/store/storetest/apikeytest.go`

- [ ] **Step 8: Run the conformance suite to verify it fails**

Run: `go test ./internal/store/sqlite -run 'TestAPIKeyRepository' -v 2>&1 | head -30`
Expected: FAIL to compile (`*apiKeyRepo` does not implement `apikey.Repository`, wrong signature).

- [ ] **Step 9: Implement scopes in the SQLite adapter**

In `internal/store/sqlite/apikeys.go`:

Add `"encoding/json"` to the imports.

Replace `regionKeyFromRow` with:

```go
// regionKeyFromRow maps a generated row onto the domain type. CreatedBy's ID
// is 0 exactly when CreatedByKind is "cli": the region_api_keys CHECK
// constraint ((created_by_kind = 'cli') = (created_by_id IS NULL))
// guarantees that, so no separate zero-value branch is needed here.
//
// It returns an error for a scopes cell that does not decode or names a
// scope this binary does not know: a key must never quietly read back with
// fewer scopes than it was minted with (migration design spec section 2.2).
func regionKeyFromRow(r gen.RegionApiKey) (apikey.RegionKey, error) {
	scopes, err := decodeScopes(r.Scopes)
	if err != nil {
		return apikey.RegionKey{}, fmt.Errorf("sqlite: region api key %d: scopes: %w", r.ID, err)
	}
	out := apikey.RegionKey{
		ID:         r.ID,
		RegionID:   r.RegionID,
		Name:       r.Name,
		KeyHash:    r.KeyHash,
		Scopes:     scopes,
		CreatedBy:  apikey.Actor{Kind: r.CreatedByKind, ID: r.CreatedByID.Int64},
		CreatedAt:  unixToTime(r.CreatedAt),
		LastUsedAt: nullUnixToTime(r.LastUsedAt),
		RevokedAt:  nullUnixToTime(r.RevokedAt),
	}
	if r.RevokedByKind.Valid {
		out.RevokedBy = &apikey.Actor{Kind: r.RevokedByKind.String, ID: r.RevokedByID.Int64}
	}
	return out, nil
}

// encodeScopes renders a scope set for the scopes column: a JSON array of
// names, [] for an empty or nil set.
func encodeScopes(s apikey.Scopes) (string, error) {
	b, err := json.Marshal(s.Strings())
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// decodeScopes is the inverse of encodeScopes, re-validated through
// ParseScopes so a hand-edited or downgraded row cannot carry a name the
// running binary does not enforce.
func decodeScopes(cell string) (apikey.Scopes, error) {
	var names []string
	if err := json.Unmarshal([]byte(cell), &names); err != nil {
		return nil, err
	}
	return apikey.ParseScopes(names)
}

// regionKeysFromRows maps a list, stopping at the first undecodable row.
func regionKeysFromRows(rows []gen.RegionApiKey) ([]apikey.RegionKey, error) {
	out := make([]apikey.RegionKey, len(rows))
	for i, row := range rows {
		k, err := regionKeyFromRow(row)
		if err != nil {
			return nil, err
		}
		out[i] = k
	}
	return out, nil
}
```

Replace `CreateRegionKey`:

```go
func (r *apiKeyRepo) CreateRegionKey(ctx context.Context, regionID int64, name, keyHash string, scopes apikey.Scopes, by apikey.Actor, now time.Time) (apikey.RegionKey, error) {
	kind, id := actorToColumns(by)
	encoded, err := encodeScopes(scopes)
	if err != nil {
		return apikey.RegionKey{}, fmt.Errorf("sqlite: create region api key for region %d: encode scopes: %w", regionID, err)
	}
	row, err := r.q.CreateRegionAPIKey(ctx, gen.CreateRegionAPIKeyParams{
		RegionID: regionID, Name: name, KeyHash: keyHash, Scopes: encoded,
		CreatedByKind: kind, CreatedByID: id, CreatedAt: now.Unix(),
	})
	if err != nil {
		return apikey.RegionKey{}, fmt.Errorf("sqlite: create region api key for region %d: %w", regionID, err)
	}
	return regionKeyFromRow(row)
}
```

In `GetRegionKeyByHash`, replace `key := regionKeyFromRow(row)` with:

```go
	key, err := regionKeyFromRow(row)
	if err != nil {
		return apikey.RegionKey{}, err
	}
```

In `ListRegionKeys`, replace the `out := make(...)` loop and return with:

```go
	return regionKeysFromRows(rows)
```

In `ListRegionKeysByCreator`, replace the trailing `out := make(...)` loop and return with:

```go
	return regionKeysFromRows(rows)
```

- [ ] **Step 10: Fix the other compile sites**

Add the new `nil` (or real) scopes argument at each call:

- `internal/httpapi/admin_apikeys.go` `create`: temporarily pass `nil` (Task 2 replaces it).
- `internal/httpapi/bearer_test.go` `mintRegionKey` and the "mismatched" key near line 336: pass `nil`.
- `internal/httpapi/admin_alerts_test.go`: `grep -n 'CreateRegionKey(' internal/httpapi/admin_alerts_test.go` and pass `nil`.
- `cmd/sidecar-admin/keys_test.go`: same.
- `cmd/sidecar-admin/keys.go` `keyCreate`: pass `nil` for now (Step 12 wires the flag).

Run: `go build ./... && go vet ./...`
Expected: clean.

- [ ] **Step 11: Run the store tests to verify they pass**

Run: `go test ./internal/store/... ./internal/apikey ./internal/httpapi 2>&1 | tail -5`
Expected: PASS (the `ScopesRoundTrip` subtest included).

- [ ] **Step 12: Write the failing CLI test**

Append to `cmd/sidecar-admin/keys_test.go`:

```go
// TestKeyCreate_ScopeFlag: --scope is repeatable, an unknown name is
// refused before anything is written, and `key list` shows the scopes in
// its last column so the existing column indexes stay put.
func TestKeyCreate_ScopeFlag(t *testing.T) {
	t.Parallel()

	path, store := keyFixture(t)
	var stdout bytes.Buffer
	if err := run(strings.NewReader(""), &stdout, io.Discard,
		[]string{"--db", path, "key", "create", "--region", "1", "--name", "rails", "--scope", "push"}); err != nil {
		t.Fatalf("key create --scope push: %v", err)
	}
	findKeyInOutput(t, stdout.String())

	keys, err := store.APIKeys().ListRegionKeys(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || !keys[0].Scopes.Has(apikey.ScopePush) {
		t.Fatalf("stored keys = %+v, want one push-scoped key", keys)
	}

	var list bytes.Buffer
	if err := run(strings.NewReader(""), &list, io.Discard, []string{"--db", path, "key", "list", "--region", "1"}); err != nil {
		t.Fatalf("key list: %v", err)
	}
	line := strings.TrimRight(list.String(), "\n")
	fields := strings.Split(line, "\t")
	if got := fields[len(fields)-1]; got != "push" {
		t.Errorf("last column = %q, want push; line = %q", got, line)
	}

	var stderr bytes.Buffer
	err = run(strings.NewReader(""), io.Discard, &stderr,
		[]string{"--db", path, "key", "create", "--region", "1", "--name", "bad", "--scope", "admin"})
	if err == nil || !errors.Is(err, apikey.ErrUnknownScope) {
		t.Fatalf("unknown scope: err = %v, want ErrUnknownScope", err)
	}
	keys, err = store.APIKeys().ListRegionKeys(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 {
		t.Errorf("a refused create wrote a key: %+v", keys)
	}
}
```

- [ ] **Step 13: Run it to verify it fails**

Run: `go test ./cmd/sidecar-admin -run TestKeyCreate_ScopeFlag -v`
Expected: FAIL — `flag provided but not defined: -scope`.

- [ ] **Step 14: Add `--scope` and the list column**

In `cmd/sidecar-admin/keys.go`, add above `runKey`:

```go
// scopeFlag collects a repeatable --scope. flag.Value rather than a
// comma-split string so `--scope push --scope other` reads the way every
// other repeatable flag in this CLI would.
type scopeFlag []string

// String implements flag.Value.
func (s *scopeFlag) String() string { return strings.Join(*s, ",") }

// Set implements flag.Value.
func (s *scopeFlag) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// formatScopes renders a key's scopes for a table cell: comma-joined, or a
// dash for none, matching the other optional columns.
func formatScopes(s apikey.Scopes) string {
	if len(s) == 0 {
		return "—"
	}
	return strings.Join(s.Strings(), ",")
}
```

In `keyCreate`, after the `name` flag:

```go
	var scopeNames scopeFlag
	fs.Var(&scopeNames, "scope", "grant a scope (repeatable); the only defined scope is push")
```

After the region existence check and before `apikey.NewRegionKey`:

```go
	scopes, err := apikey.ParseScopes(scopeNames)
	if err != nil {
		return fmt.Errorf("key create: %w", err)
	}
```

Change the `CreateRegionKey` call to pass `scopes` instead of `nil`, and change the summary line to:

```go
	fmt.Fprintf(stdout, "id: %d\tname: %s\tscopes: %s\n", created.ID, created.Name, formatScopes(created.Scopes))
```

In `keyList`, change the `Fprintf` to append the scopes column last:

```go
		fmt.Fprintf(stdout, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			k.ID, regions.StripControlChars(k.Name), formatActor(k.CreatedBy),
			formatKeyInstant(k.CreatedAt), lastUsed, revoked, revokedBy, formatScopes(k.Scopes))
```

- [ ] **Step 15: Run the CLI tests to verify they pass**

Run: `go test ./cmd/sidecar-admin -run 'TestKey|TestPrincipal' -v 2>&1 | tail -20`
Expected: PASS. If `TestKeyList_ShowsHistoryAndCreators` indexes a column by position, it still passes because the new column is last; if it compares a whole line, update its expected line to end with `\t—`.

- [ ] **Step 16: Lint, generate-check, commit**

Run: `make lint && make generate-check && go test ./... 2>&1 | tail -3`
Expected: clean, PASS.

```bash
git add internal/apikey internal/store cmd/sidecar-admin internal/httpapi
git commit -m "apikey: region key scopes (push) in the domain, store, and CLI"
```

---

### Task 2: Mint and list `scopes` over the admin API

**Files:**
- Modify: `internal/httpapi/admin_apikeys.go`
- Test: `internal/httpapi/admin_apikeys_test.go`

**Interfaces:**
- Consumes: `apikey.ParseScopes`, `apikey.ErrUnknownScope`, `RegionKey.Scopes` (Task 1).
- Produces: `POST …/api_keys` body `{name, scopes?: [string]}`; unknown scope → `400 {"error":"unknown scope \"x\""}`; mint response and each list item carry `"scopes": [...]` (always an array).

- [ ] **Step 1: Write the failing handler tests**

Append to `internal/httpapi/admin_apikeys_test.go`:

```go
// TestAPIKeys_ScopesTakeEffect is the "field actually took effect" test
// the migration design spec section 2.2 demands: decodeJSON is lenient, so
// a misspelled or ignored scopes field would mint a key without push and
// fail later at send time. The assertion chain is request -> 201 body ->
// stored row -> list body -> the key can reach the push route.
func TestAPIKeys_ScopesTakeEffect(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	rec := f.do(http.MethodPost, "/api/admin/v1/regions/1/api_keys", `{"name":"obacloud","scopes":["push"]}`)
	got := object(t, rec, http.StatusCreated)
	assertKeys(t, "api key", got, []string{"id", "name", "key", "scopes", "created_by", "created_at"})
	if scopes, _ := got["scopes"].([]any); len(scopes) != 1 || scopes[0] != "push" {
		t.Fatalf("minted scopes = %v, want [push]", got["scopes"])
	}
	raw, _ := got["key"].(string)

	keys, err := f.store.APIKeys().ListRegionKeys(context.Background(), regionPuget)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 || !keys[0].Scopes.Has(apikey.ScopePush) {
		t.Fatalf("stored scopes = %+v, want push", keys)
	}

	list := array(t, f.do(http.MethodGet, "/api/admin/v1/regions/1/api_keys", ""), http.StatusOK)
	assertKeys(t, "api key", list[0],
		[]string{"id", "name", "scopes", "created_by", "created_at", "last_used_at", "revoked_at", "revoked_by"})
	if scopes, _ := list[0]["scopes"].([]any); len(scopes) != 1 || scopes[0] != "push" {
		t.Errorf("listed scopes = %v, want [push]", list[0]["scopes"])
	}

	// The scope is honoured by the router, not just echoed: a push-scoped
	// key is not refused with 403 on the push route. (404 here: alert 1
	// does not exist in this fixture; what matters is that the allow-list
	// let the request through to the loader.)
	if rec := sendBearer(f.handler, http.MethodPost, "/api/admin/v1/regions/1/alerts/1/pushes", `{}`, "Bearer "+raw); rec.Code == http.StatusForbidden {
		t.Errorf("push-scoped key was refused on POST pushes: %s", rec.Body.String())
	}
}

// TestAPIKeys_ScopesValidation: unknown names are 400, and an absent or
// empty scopes field mints an unscoped key whose scopes marshal as [].
func TestAPIKeys_ScopesValidation(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	for _, tc := range []struct {
		name string
		body string
		want int
	}{
		{"unknown scope", `{"name":"a","scopes":["admin"]}`, http.StatusBadRequest},
		{"blank scope", `{"name":"a","scopes":[""]}`, http.StatusBadRequest},
		{"wrong type", `{"name":"a","scopes":"push"}`, http.StatusBadRequest},
		{"absent", `{"name":"a"}`, http.StatusCreated},
		{"empty", `{"name":"a","scopes":[]}`, http.StatusCreated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := f.do(http.MethodPost, "/api/admin/v1/regions/1/api_keys", tc.body)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, tc.want, rec.Body.String())
			}
			if tc.want == http.StatusCreated {
				got := object(t, rec, http.StatusCreated)
				if scopes, ok := got["scopes"].([]any); !ok || len(scopes) != 0 {
					t.Errorf("scopes = %v (%T), want []", got["scopes"], got["scopes"])
				}
			}
		})
	}
	if rec := f.do(http.MethodPost, "/api/admin/v1/regions/1/api_keys", `{"name":"a","scopes":["admin"]}`); bodyText(rec) != `{"error":"unknown scope \"admin\""}` {
		t.Errorf("400 body = %s", bodyText(rec))
	}
}
```

Add `"context"` and `"github.com/OneBusAway/sidecar/internal/apikey"` to that file's imports if absent.

Update the two existing `assertKeys` lists in this file (`TestAPIKeys_MintReturnsTheRawKeyOnce` and `TestAPIKeys_ListNeverEchoesTheKey`) to include `"scopes"` in the same positions as above.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/httpapi -run 'TestAPIKeys_Scopes|TestAPIKeys_MintReturnsTheRawKeyOnce|TestAPIKeys_ListNeverEchoesTheKey' -v 2>&1 | tail -20`
Expected: FAIL — `scopes` missing from the 201 and list bodies; unknown scope answers 201.

- [ ] **Step 3: Implement**

In `internal/httpapi/admin_apikeys.go`:

`apiKeyJSON` gains, after `Name`:

```go
	Scopes     []string   `json:"scopes"`
```

`toAPIKeyJSON` sets `Scopes: k.Scopes.Strings(),` in the literal.

`mintedKeyJSON` gains, after `Name`:

```go
	Scopes    []string  `json:"scopes"`
```

`createKeyRequest` becomes:

```go
// createKeyRequest is the POST .../api_keys body. Scopes is optional and
// validated strictly: the decoder is lenient about unknown FIELDS, but an
// unknown scope NAME is a 400, because a silently dropped scope would mint
// a key that fails at send time (migration design spec section 2.2).
type createKeyRequest struct {
	Name   string   `json:"name"`
	Scopes []string `json:"scopes"`
}
```

In `create`, after the name check and before `principalFrom`:

```go
	scopes, err := apikey.ParseScopes(req.Scopes)
	if err != nil {
		writeJSONError(w, h.deps.Logger, http.StatusBadRequest, err.Error())
		return
	}
```

Change the mint call to `h.deps.APIKeys.CreateRegionKey(r.Context(), region.ID, name, hash, scopes, p.actor(), h.deps.Now())` and add `Scopes: created.Scopes.Strings(),` to the `mintedKeyJSON` literal. (Rename the later `raw, hash, err :=` to `raw, hash, err =` if the compiler complains about `err` redeclaration.)

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/httpapi -run 'TestAPIKeys_' -v 2>&1 | tail -20`
Expected: PASS. (`TestAPIKeys_ScopesTakeEffect`'s last assertion passes only after Task 3; if it fails with 403 here, that is expected — proceed to Task 3 and re-run.)

- [ ] **Step 5: Commit**

```bash
git add internal/httpapi/admin_apikeys.go internal/httpapi/admin_apikeys_test.go
git commit -m "httpapi: mint and list region key scopes"
```

---

### Task 3: `operatorOrPushKey` on push create/cancel

**Files:**
- Modify: `internal/httpapi/principal.go`, `internal/httpapi/middleware.go`, `internal/httpapi/bearer.go`, `internal/httpapi/router.go`
- Test: `internal/httpapi/admin_alerts_test.go` (route-table tests), `internal/httpapi/bearer_test.go` (helper), `internal/httpapi/admin_pushes_test.go`

**Interfaces:**
- Consumes: `RegionKey.Scopes`, `apikey.ScopePush`.
- Produces: `principalPushKey principalKind` (an allow-list marker, never an authenticated kind); `principalSet.admits(p principal) bool`; `principal.scopes apikey.Scopes`; `operatorOrPushKey principalSet`; test helper `(f *adminFixture) mintRegionKeyWithScopes(t, regionID, scopes apikey.Scopes) string`.

- [ ] **Step 1: Write the failing route-table tests**

In `internal/httpapi/admin_alerts_test.go`, replace `TestRouteTable_OperatorOnlyRoutesAreClosedToRegionKeys` with:

```go
// TestRouteTable_PushScopeIsExactlyTheTwoPushWrites pins WHICH routes take
// the push scope and which stay operator-only, derived from the URL pattern
// rather than from the allow-list being checked (TestAdminRoutes_
// PrincipalAllowLists compares the live table against behaviour the same
// table drives, so it is self-consistent and cannot catch a widened
// allow-list on its own).
//
// Send and cancel admit an operator or a region key carrying the push scope
// -- and NOT an unscoped region key, whose blast radius must stay "one
// region's tenant data" (keys design spec section 2.1 as amended by the
// migration design spec section 0.2). No other route may take the push
// scope: "those two routes and only those".
func TestRouteTable_PushScopeIsExactlyTheTwoPushWrites(t *testing.T) {
	t.Parallel()

	pushScoped := map[string]bool{
		"POST /api/admin/v1/regions/{regionId}/alerts/{id}/pushes":            true,
		"DELETE /api/admin/v1/regions/{regionId}/alerts/{id}/pushes/{pushId}": true,
	}
	operatorOnlyPatterns := map[string]bool{
		"GET /api/admin/v1/regions": true,
	}

	f := newFullAdminFixture(t)
	seen := map[string]bool{}
	for _, rt := range adminRoutes(f.deps) {
		switch {
		case pushScoped[rt.pattern]:
			seen[rt.pattern] = true
			if rt.allowed.has(principalRegionKey) {
				t.Errorf("route %q admits an unscoped region key", rt.pattern)
			}
			if !rt.allowed.has(principalPushKey) {
				t.Errorf("route %q does not admit a push-scoped key; OBACloud cannot send", rt.pattern)
			}
			if !rt.allowed.has(principalOperator) {
				t.Errorf("route %q does not admit an operator", rt.pattern)
			}
			if rt.allowed.has(principalService) {
				t.Errorf("route %q admits a service principal; it reads no tenant data", rt.pattern)
			}
		case operatorOnlyPatterns[rt.pattern]:
			seen[rt.pattern] = true
			if rt.allowed.has(principalRegionKey) || rt.allowed.has(principalPushKey) || rt.allowed.has(principalService) {
				t.Errorf("route %q admits a non-operator; the spec makes it operator-only", rt.pattern)
			}
		default:
			if rt.allowed.has(principalPushKey) {
				t.Errorf("route %q takes the push scope; only the two push writes may", rt.pattern)
			}
		}
	}
	for pattern := range pushScoped {
		if !seen[pattern] {
			t.Errorf("route %q is no longer in the table; this test has stopped guarding it", pattern)
		}
	}
	for pattern := range operatorOnlyPatterns {
		if !seen[pattern] {
			t.Errorf("route %q is no longer in the table; this test has stopped guarding it", pattern)
		}
	}
}
```

In `TestAdminRoutes_PrincipalAllowLists` (same file), replace the `kinds` slice and loop body with a version that also walks a push-scoped key:

```go
	pushKey := f.mintRegionKeyWithScopes(t, regionPuget, apikey.Scopes{apikey.ScopePush})

	kinds := []struct {
		name    string
		header  string
		allowed func(principalSet) bool
	}{
		{"region key", "Bearer " + regionKey, func(s principalSet) bool { return s.has(principalRegionKey) }},
		{"service principal", "Bearer " + servicePrincipal, func(s principalSet) bool { return s.has(principalService) }},
		// A push-scoped key is still a region key: it reaches everything an
		// unscoped key reaches, plus the routes that name principalPushKey.
		{"push-scoped region key", "Bearer " + pushKey, func(s principalSet) bool {
			return s.has(principalRegionKey) || s.has(principalPushKey)
		}},
	}

	for _, rt := range adminRoutes(f.deps) {
		if rt.allowed == nil {
			continue // login and logout
		}
		method, target := concreteRoute(t, rt.pattern)
		for _, k := range kinds {
			rec := sendBearer(f.handler, method, target, "", k.header)
			forbidden := rec.Code == http.StatusForbidden && bodyText(rec) == `{"error":"forbidden"}`
			if allowed := k.allowed(rt.allowed); allowed == forbidden {
				t.Errorf("%s %s with %s: status = %d body = %s; allowed = %v",
					method, target, k.name, rec.Code, rec.Body.String(), allowed)
			}
		}
	}
```

Add `"github.com/OneBusAway/sidecar/internal/apikey"` to the file's imports if absent.

In `internal/httpapi/bearer_test.go`, replace `mintRegionKey` with a scoped variant plus the old name delegating to it:

```go
// mintRegionKey creates a live, unscoped region key in the fixture's store
// and returns its raw form. The raw key exists only here and in the
// Authorization header the test sends -- exactly as in production.
func (f *adminFixture) mintRegionKey(t *testing.T, regionID int64) string {
	t.Helper()
	return f.mintRegionKeyWithScopes(t, regionID, nil)
}

// mintRegionKeyWithScopes is mintRegionKey with a scope set, for the push
// routes' allow-list tests.
func (f *adminFixture) mintRegionKeyWithScopes(t *testing.T, regionID int64, scopes apikey.Scopes) string {
	t.Helper()
	raw, hash, err := apikey.NewRegionKey(regionID)
	if err != nil {
		t.Fatalf("NewRegionKey: %v", err)
	}
	_, err = f.store.APIKeys().CreateRegionKey(context.Background(), regionID, "test",
		hash, scopes, apikey.Actor{Kind: apikey.ActorCLI}, testNow)
	if err != nil {
		t.Fatalf("CreateRegionKey: %v", err)
	}
	return raw
}
```

Append an end-to-end test to `internal/httpapi/admin_pushes_test.go`:

```go
// TestAdminPushes_PushScopedKeyCanSendAndCancel is the OBACloud send path
// end to end: a push-scoped region key queues and cancels a push for its
// own region, an unscoped key is refused with 403 on both, and the scope
// widens nothing else -- key management stays 403 and another region stays
// 404.
func TestAdminPushes_PushScopedKeyCanSendAndCancel(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	id := f.seedPublishedAlert(t, regionPuget, false)
	f.seedRegistration(t, regionPuget, "tok-1", false)
	pushKey := "Bearer " + f.mintRegionKeyWithScopes(t, regionPuget, apikey.Scopes{apikey.ScopePush})
	plainKey := "Bearer " + f.mintRegionKey(t, regionPuget)

	if rec := sendBearer(f.handler, http.MethodPost, pushesPath(regionPuget, id), `{"audience":"all"}`, plainKey); rec.Code != http.StatusForbidden {
		t.Errorf("unscoped key POST pushes: status = %d, want 403", rec.Code)
	}
	got := object(t, sendBearer(f.handler, http.MethodPost, pushesPath(regionPuget, id), `{"audience":"all"}`, pushKey), http.StatusAccepted)
	pushID := jsonID(t, got)

	cancelPath := fmt.Sprintf("%s/%d", pushesPath(regionPuget, id), pushID)
	if rec := sendBearer(f.handler, http.MethodDelete, cancelPath, "", plainKey); rec.Code != http.StatusForbidden {
		t.Errorf("unscoped key DELETE push: status = %d, want 403", rec.Code)
	}
	if rec := sendBearer(f.handler, http.MethodDelete, cancelPath, "", pushKey); rec.Code != http.StatusNoContent {
		t.Errorf("push-scoped key DELETE push: status = %d, want 204; body = %s", rec.Code, rec.Body.String())
	}

	if rec := sendBearer(f.handler, http.MethodPost, "/api/admin/v1/regions/1/api_keys", `{"name":"x"}`, pushKey); rec.Code != http.StatusForbidden {
		t.Errorf("push-scoped key on key management: status = %d, want 403", rec.Code)
	}
	if rec := sendBearer(f.handler, http.MethodPost, pushesPath(regionTampa, id), `{}`, pushKey); rec.Code != http.StatusNotFound {
		t.Errorf("push-scoped key on another region: status = %d, want 404", rec.Code)
	}
}
```

Add `"github.com/OneBusAway/sidecar/internal/apikey"` to that file's imports.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/httpapi -run 'TestRouteTable_PushScope|TestAdminRoutes_PrincipalAllowLists|TestAdminPushes_PushScopedKey' 2>&1 | head -20`
Expected: FAIL to compile — `undefined: principalPushKey`.

- [ ] **Step 3: Implement the gate**

In `internal/httpapi/principal.go`:

Extend the kinds:

```go
const (
	principalOperator  principalKind = iota + 1 // session cookie
	principalRegionKey                          // obask_
	principalService                            // obasp_
	// principalPushKey is never what a request authenticates AS -- a
	// push-scoped key authenticates as principalRegionKey. It is the
	// allow-list marker for "a region key carrying apikey.ScopePush", so the
	// route table can say operatorOrPushKey and a test can ask has(). See
	// principalSet.admits.
	principalPushKey
)
```

Add a `String` case:

```go
	case principalPushKey:
		return "push_scoped_region_key"
```

Add the allow-list and the admission rule:

```go
	// operatorOrPushKey is the two push writes (send, cancel): an operator,
	// or a region key that was minted with the push scope. An unscoped
	// region key is refused, so a leaked ordinary key still cannot page
	// every device in its region (keys design spec section 2.1, amended by
	// migration design spec section 0.2).
	operatorOrPushKey = principalSet{principalOperator, principalPushKey}
```

(Update the comment above the `var (` block from "three allow-lists" to "four allow-lists".)

Add the method after `has`:

```go
// admits reports whether p may use a route guarded by s. It is has() on
// p's kind, plus the one derived admission: a region key carrying the push
// scope satisfies principalPushKey. Callers deciding access use this, not
// has(), so the scope check has exactly one home.
func (s principalSet) admits(p principal) bool {
	if s.has(p.kind) {
		return true
	}
	return p.kind == principalRegionKey && p.scopes.Has(apikey.ScopePush) && s.has(principalPushKey)
}
```

Add the field to `principal` after `keyID`:

```go
	// scopes is populated for region keys only; see apikey.Scopes.
	scopes apikey.Scopes
```

In `LogValue`, add `slog.Any("scopes", p.scopes.Strings()),`.

In `internal/httpapi/middleware.go` `requirePrincipal`, change `if !allowed.has(p.kind) {` to `if !allowed.admits(p) {`.

In `internal/httpapi/bearer.go` `authenticateRegionKey`, change the final return to:

```go
	return principal{kind: principalRegionKey, regionID: key.RegionID, keyID: key.ID, scopes: key.Scopes}, true
```

In `internal/httpapi/router.go`, update the push routes and their comment:

```go
			// Sending and cancelling a push reach every rider's device, which
			// is the one blast radius an ordinary region key must not have
			// (design spec §4.5) -- so they take an operator or a region key
			// minted with the push scope, which OBACloud holds so it can drive
			// sends from its own wizard (migration design spec §2.2). Reading
			// what was sent, and counting the audience beforehand, stay open
			// to any region key.
			routes = append(routes,
				adminRoute{"POST /api/admin/v1/regions/{regionId}/alerts/{id}/pushes", pushesAdmin.create, operatorOrPushKey, scopeRegion},
				adminRoute{"GET /api/admin/v1/regions/{regionId}/alerts/{id}/pushes", pushesAdmin.list, operatorOrKey, scopeRegion},
				adminRoute{"DELETE /api/admin/v1/regions/{regionId}/alerts/{id}/pushes/{pushId}", pushesAdmin.cancel, operatorOrPushKey, scopeRegion},
				adminRoute{"GET /api/admin/v1/regions/{regionId}/alerts/{id}/push_audience", pushesAdmin.audience, operatorOrKey, scopeRegion},
			)
```

- [ ] **Step 4: Run the httpapi package**

Run: `go test ./internal/httpapi 2>&1 | tail -20`
Expected: PASS, including the Task 2 `TestAPIKeys_ScopesTakeEffect` push-route assertion. If `bearer_test.go`'s `principalSet.has` table test (around line 514) enumerates kinds, add rows `{"operatorOrPushKey admits a push marker", operatorOrPushKey, principalPushKey, true}` and `{"operatorOrPushKey refuses a plain region key", operatorOrPushKey, principalRegionKey, false}`.

- [ ] **Step 5: Mutation check**

Temporarily change `admits` to `return true` for region keys; run `go test ./internal/httpapi -run 'TestRouteTable_PushScope|TestAdminRoutes_PrincipalAllowLists|TestAdminPushes_PushScopedKey'`; confirm FAIL; revert.

- [ ] **Step 6: Commit**

```bash
git add internal/httpapi
git commit -m "httpapi: push create/cancel admit push-scoped region keys (operatorOrPushKey)"
```

---

### Task 4: Optional `messages` on `POST …/pushes`

**Files:**
- Modify: `internal/alertpush/messages.go`, `internal/alertpush/enqueue.go`, `internal/httpapi/admin_pushes.go`, `cmd/sidecar-admin/commands.go` (`alertPush`)
- Modify (compile, add `nil`): `internal/alertpush/enqueue_test.go`, `internal/alertpush/dispatcher_test.go`, `internal/httpapi/feedback_test.go`
- Test: `internal/alertpush/messages_test.go`, `internal/httpapi/admin_pushes_test.go`

**Interfaces:**
- Produces: `alertpush.ErrInvalidMessages`; `alertpush.ValidateMessages(m Messages) error`; `(*Enqueuer).Enqueue(ctx, alertID int64, audience Audience, messages Messages, now time.Time) (Push, error)` — `messages == nil` derives copy with `BuildMessages`, non-nil is validated and stored as the snapshot.
- Produces: `POST …/pushes` body `{"audience": "...", "messages": {"en": {"title","body"}, "<lang>": {...}}}`; invalid messages → `400 {"error":"invalid push messages: ..."}`.

- [ ] **Step 1: Write the failing validation test**

Append to `internal/alertpush/messages_test.go`:

```go
func TestValidateMessages(t *testing.T) {
	t.Parallel()
	ok := alertpush.Message{Title: "Route 40", Body: "Detour until Friday"}
	for _, tc := range []struct {
		name string
		in   alertpush.Messages
		want bool
	}{
		{"english only", alertpush.Messages{"en": ok}, true},
		{"english plus a translation", alertpush.Messages{"en": ok, "es": {Title: "Ruta 40", Body: "Desvio"}}, true},
		{"empty title is fine (header promoted to body)", alertpush.Messages{"en": {Body: "Detour"}}, true},
		{"title at the cap", alertpush.Messages{"en": {Title: strings.Repeat("t", alertpush.TitleLimit), Body: "b"}}, true},
		{"body at the cap", alertpush.Messages{"en": {Body: strings.Repeat("b", alertpush.BodyLimit)}}, true},
		{"empty set", alertpush.Messages{}, false},
		{"no english", alertpush.Messages{"es": ok}, false},
		{"blank body", alertpush.Messages{"en": {Title: "t", Body: "   "}}, false},
		{"title over the cap", alertpush.Messages{"en": {Title: strings.Repeat("t", alertpush.TitleLimit+1), Body: "b"}}, false},
		{"body over the cap", alertpush.Messages{"en": {Body: strings.Repeat("b", alertpush.BodyLimit+1)}}, false},
		{"multi-byte body at the cap counts runes", alertpush.Messages{"en": {Body: strings.Repeat("中", alertpush.BodyLimit)}}, true},
		{"un-normalized language", alertpush.Messages{"en": ok, "ES": ok}, false},
		{"blank language", alertpush.Messages{"en": ok, "": ok}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := alertpush.ValidateMessages(tc.in)
			if (err == nil) != tc.want {
				t.Fatalf("err = %v, want ok=%v", err, tc.want)
			}
			if err != nil && !errors.Is(err, alertpush.ErrInvalidMessages) {
				t.Errorf("err = %v, want ErrInvalidMessages", err)
			}
		})
	}
}
```

Add `"errors"` and `"strings"` to that file's imports if absent.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/alertpush -run TestValidateMessages 2>&1 | head -5`
Expected: FAIL to compile — `undefined: alertpush.ValidateMessages`.

- [ ] **Step 3: Implement validation and the optional snapshot**

In `internal/alertpush/messages.go`, add `"errors"`, `"fmt"`, `"strings"` to the imports and append:

```go
// ErrInvalidMessages marks caller-supplied push copy that ValidateMessages
// refused. The HTTP layer maps it to 400: it is a fact about the request
// body, not the alert.
var ErrInvalidMessages = errors.New("invalid push messages")

// ValidateMessages checks copy a caller supplies in place of BuildMessages'
// derivation (migration design spec section 2.3). The rules are the ones
// the derivation's output already satisfies: English present, every key a
// normalized language tag, a non-blank body (an empty-bodied notification
// is invisible; an empty title is fine, it is what promoteHeader produces),
// and TitleLimit/BodyLimit in runes. Nothing is clamped or rewritten --
// the caller asked for this exact text, so it either fits or is refused.
func ValidateMessages(m Messages) error {
	if _, ok := m[EnglishKey]; !ok {
		return fmt.Errorf("%w: messages must include %q", ErrInvalidMessages, EnglishKey)
	}
	for lang, msg := range m {
		if lang == "" || alerts.NormalizeLanguage(lang) != lang {
			return fmt.Errorf("%w: language %q must be a trimmed, lowercase tag", ErrInvalidMessages, lang)
		}
		if strings.TrimSpace(msg.Body) == "" {
			return fmt.Errorf("%w: messages[%s].body must not be blank", ErrInvalidMessages, lang)
		}
		if n := utf8.RuneCountInString(msg.Title); n > TitleLimit {
			return fmt.Errorf("%w: messages[%s].title is %d runes, max %d", ErrInvalidMessages, lang, n, TitleLimit)
		}
		if n := utf8.RuneCountInString(msg.Body); n > BodyLimit {
			return fmt.Errorf("%w: messages[%s].body is %d runes, max %d", ErrInvalidMessages, lang, n, BodyLimit)
		}
	}
	return nil
}
```

In `internal/alertpush/enqueue.go`, replace `Enqueue`'s signature, doc comment, and the `Create` call:

```go
// Enqueue validates and inserts a queued push for alertID. Errors:
// alerts.ErrNotFound, ErrNotPublished, ErrInFlight, ErrEmptyAudience,
// ErrInvalidMessages. A test alert is always sent to the test audience
// regardless of audience. A nil messages derives the copy snapshot from the
// alert (BuildMessages); a non-nil one is the caller's own copy, validated
// by ValidateMessages and stored verbatim -- OBACloud's copywriter output
// (migration design spec section 2.3).
func (e *Enqueuer) Enqueue(ctx context.Context, alertID int64, audience Audience, messages Messages, now time.Time) (Push, error) {
	if messages != nil {
		if err := ValidateMessages(messages); err != nil {
			return Push{}, err
		}
	}
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
	if messages == nil {
		messages = BuildMessages(a)
	}
	return e.Repo.Create(ctx, NewPush{
		AlertID: a.ID, RegionID: a.RegionID, Audience: audience, Messages: messages,
	}, now)
}
```

Fix callers: pass `nil` as the new fourth argument in `cmd/sidecar-admin/commands.go` (`alertPush`), `internal/alertpush/enqueue_test.go`, `internal/alertpush/dispatcher_test.go`, `internal/httpapi/feedback_test.go`. Find them with `grep -rn '\.Enqueue(' internal cmd`.

Run: `go build ./... && go test ./internal/alertpush 2>&1 | tail -3`
Expected: PASS.

- [ ] **Step 4: Write the failing handler test**

Append to `internal/httpapi/admin_pushes_test.go`:

```go
// TestAdminCreatePush_CustomMessages: present messages are validated and
// stored as the snapshot verbatim; absent messages still derive from the
// alert (TestAdminCreatePushQueuesAndWakes covers that half).
func TestAdminCreatePush_CustomMessages(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	id := f.seedPublishedAlert(t, regionPuget, false)
	f.seedRegistration(t, regionPuget, "tok-1", false)

	body := `{"audience":"all","messages":{"en":{"title":"Custom title","body":"Custom body"},"es":{"title":"Titulo","body":"Cuerpo"}}}`
	got := object(t, f.do(http.MethodPost, pushesPath(regionPuget, id), body), http.StatusAccepted)
	messages, _ := got["messages"].(map[string]any)
	en, _ := messages["en"].(map[string]any)
	es, _ := messages["es"].(map[string]any)
	if en["title"] != "Custom title" || en["body"] != "Custom body" || es["title"] != "Titulo" || es["body"] != "Cuerpo" {
		t.Errorf("messages = %v, want the supplied copy verbatim", got["messages"])
	}

	stored, err := f.store.AlertPushes().Get(context.Background(), jsonID(t, got))
	if err != nil {
		t.Fatal(err)
	}
	if stored.Messages["en"] != (alertpush.Message{Title: "Custom title", Body: "Custom body"}) ||
		stored.Messages["es"] != (alertpush.Message{Title: "Titulo", Body: "Cuerpo"}) {
		t.Errorf("stored messages = %+v, want the supplied copy", stored.Messages)
	}
	if f.waker.calls != 1 {
		t.Errorf("Wake calls = %d, want 1", f.waker.calls)
	}
}

// TestAdminCreatePush_InvalidMessagesAre400: every ValidateMessages refusal
// is a 400 carrying the sentinel's text, and nothing is queued.
func TestAdminCreatePush_InvalidMessagesAre400(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	id := f.seedPublishedAlert(t, regionPuget, false)
	f.seedRegistration(t, regionPuget, "tok-1", false)

	for _, tc := range []struct{ name, body string }{
		{"empty object", `{"messages":{}}`},
		{"no english", `{"messages":{"es":{"title":"t","body":"b"}}}`},
		{"blank body", `{"messages":{"en":{"title":"t","body":" "}}}`},
		{"title too long", fmt.Sprintf(`{"messages":{"en":{"title":%q,"body":"b"}}}`, strings.Repeat("t", alertpush.TitleLimit+1))},
		{"body too long", fmt.Sprintf(`{"messages":{"en":{"title":"t","body":%q}}}`, strings.Repeat("b", alertpush.BodyLimit+1))},
		{"uppercase language", `{"messages":{"en":{"body":"b"},"ES":{"body":"c"}}}`},
		{"wrong shape", `{"messages":["en"]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := f.do(http.MethodPost, pushesPath(regionPuget, id), tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
			}
		})
	}
	if pushes, err := f.store.AlertPushes().ListByAlert(context.Background(), id); err != nil || len(pushes) != 0 {
		t.Errorf("a refused body queued a push: %v %+v", err, pushes)
	}
	if f.waker.calls != 0 {
		t.Errorf("Wake calls = %d, want 0", f.waker.calls)
	}
}
```

- [ ] **Step 5: Run to verify failure**

Run: `go test ./internal/httpapi -run 'TestAdminCreatePush_' 2>&1 | tail -15`
Expected: FAIL — the custom test sees derived copy ("Route 40 detour"); the invalid-messages cases answer 202.

- [ ] **Step 6: Implement the request field and mapping**

In `internal/httpapi/admin_pushes.go`:

```go
// createPushRequest is the POST /alerts/{id}/pushes body. An absent audience
// means "all", so an empty body is a complete request. Messages, when
// present, replaces the mechanical copy derivation with the caller's own
// per-language {title, body} snapshot (migration design spec §2.3); it is
// the same shape pushJSON emits. Absent (nil) derives as before; present
// but empty is a 400, since it can only mean the caller forgot the text.
type createPushRequest struct {
	Audience string             `json:"audience"`
	Messages alertpush.Messages `json:"messages"`
}
```

In `create`, change the enqueue call to:

```go
	p, err := h.enqueuer().Enqueue(r.Context(), alert.ID, audience, req.Messages, h.deps.Now())
```

In `enqueueError`, add before the sentinel loop:

```go
	if errors.Is(err, alertpush.ErrInvalidMessages) {
		// The body, not the alert, is at fault -- and the wrapped text names
		// which field, without any store framing (design spec §5).
		writeJSONError(w, h.deps.Logger, http.StatusBadRequest, err.Error())
		return
	}
```

Update `enqueueError`'s doc comment: "Every one of them is a fact about the alert or the region, not about the request body, so they are 404/409 rather than 400 -- except ErrInvalidMessages, which is the body."

- [ ] **Step 7: Run to verify passing**

Run: `go test ./internal/httpapi -run 'TestAdminCreatePush' 2>&1 | tail -5`
Expected: PASS (existing `TestAdminCreatePushQueuesAndWakes` still derives English copy).

- [ ] **Step 8: Commit**

```bash
git add internal/alertpush internal/httpapi cmd/sidecar-admin/commands.go
git commit -m "alertpush: accept caller-supplied messages on push create"
```

---

### Task 5: `stale` per translation in admin alert JSON

**Files:**
- Modify: `internal/alerts/alert.go`, `internal/httpapi/admin_alerts.go`
- Test: `internal/alerts/feed_test.go` (or a new `internal/alerts/alert_test.go`), `internal/httpapi/admin_alerts_test.go`

**Interfaces:**
- Produces: `func (a Alert) TranslationStale(t Translation) bool` in `internal/alerts`.
- Produces: `translationJSON.Stale bool json:"stale"` — true when any field of that language no longer matches the current English text.

- [ ] **Step 1: Write the failing domain test**

Create `internal/alerts/alert_test.go`:

```go
package alerts_test

import (
	"testing"

	"github.com/OneBusAway/sidecar/internal/alerts"
)

func TestAlert_TranslationStale(t *testing.T) {
	t.Parallel()
	a := alerts.Alert{HeaderText: "Header", DescriptionText: "Description"}
	for _, tc := range []struct {
		name string
		tr   alerts.Translation
		want bool
	}{
		{"fresh header", alerts.Translation{Field: alerts.FieldHeader, SourceSHA256: alerts.SourceHash("Header")}, false},
		{"fresh description", alerts.Translation{Field: alerts.FieldDescription, SourceSHA256: alerts.SourceHash("Description")}, false},
		{"stale header", alerts.Translation{Field: alerts.FieldHeader, SourceSHA256: alerts.SourceHash("Old header")}, true},
		{"stale description", alerts.Translation{Field: alerts.FieldDescription, SourceSHA256: alerts.SourceHash("Old")}, true},
		{"unknown field is never fresh", alerts.Translation{Field: "url", SourceSHA256: alerts.SourceHash("Header")}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := a.TranslationStale(tc.tr); got != tc.want {
				t.Errorf("TranslationStale = %v, want %v", got, tc.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/alerts -run TestAlert_TranslationStale 2>&1 | head -5`
Expected: FAIL to compile — `a.TranslationStale undefined`.

- [ ] **Step 3: Implement the domain method**

Append to `internal/alerts/alert.go`:

```go
// TranslationStale reports whether t no longer describes the English text
// it was translated from: the feed withholds such a translation (see the
// Translation doc comment), and the admin API surfaces the same judgement
// as "stale" so a review UI can show what riders will not see (migration
// design spec §2.4). A translation of an unknown field is never fresh.
func (a Alert) TranslationStale(t Translation) bool {
	switch t.Field {
	case FieldHeader:
		return t.SourceSHA256 != SourceHash(a.HeaderText)
	case FieldDescription:
		return t.SourceSHA256 != SourceHash(a.DescriptionText)
	default:
		return true
	}
}
```

Run: `go test ./internal/alerts -run TestAlert_TranslationStale`
Expected: PASS.

- [ ] **Step 4: Write the failing handler test**

In `internal/httpapi/admin_alerts_test.go`, change `translationJSONFields` to:

```go
	translationJSONFields = []string{"language", "header", "description", "stale"}
```

Append:

```go
// TestAdminAlerts_TranslationStaleFlag follows one translation through the
// edit cycle the Rails review UI cares about: fresh after PUT, stale after
// the English it came from changes, fresh again after retranslation. A
// language with two fields is stale when either field is.
func TestAdminAlerts_TranslationStaleFlag(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	id := f.createAlertIn(t, regionPuget, `{"header":"English header","description":"English description","start_time":"2026-08-15T14:00:00-07:00"}`)

	staleOf := func(t *testing.T, lang string) bool {
		t.Helper()
		for _, tr := range translationsOf(t, object(t, f.do(http.MethodGet, alertPath(regionPuget, id, ""), ""), http.StatusOK)) {
			if str(t, tr, "language") == lang {
				return boolean(t, tr, "stale")
			}
		}
		t.Fatalf("no %s translation", lang)
		return false
	}

	f.do(http.MethodPut, alertPath(regionPuget, id, "/translations/es"), `{"header":"Encabezado","description":"Descripcion"}`)
	if staleOf(t, "es") {
		t.Error("fresh translation reported stale")
	}

	// Only the description changes; the header translation is still fresh,
	// but the language as a whole is not.
	f.do(http.MethodPatch, alertPath(regionPuget, id, ""), `{"description":"Edited description"}`)
	if !staleOf(t, "es") {
		t.Error("translation of an edited field reported fresh")
	}

	f.do(http.MethodPut, alertPath(regionPuget, id, "/translations/es"), `{"header":"Encabezado","description":"Descripcion editada"}`)
	if staleOf(t, "es") {
		t.Error("retranslated language reported stale")
	}
}
```

If a `boolean(t, m, key) bool` helper does not exist in the httpapi tests, it does (`TestAdminAlerts_PublishUnpublish` uses it); otherwise add one beside `str`/`num`.

- [ ] **Step 5: Run to verify failure**

Run: `go test ./internal/httpapi -run 'TestAdminAlerts_TranslationStaleFlag|TestAdminAlerts_Translations' 2>&1 | tail -10`
Expected: FAIL — `stale` key missing.

- [ ] **Step 6: Implement**

In `internal/httpapi/admin_alerts.go`:

```go
// translationJSON is one language's rendering of an alert. A nil Header or
// Description means that field has no translation, which is different from a
// translation whose text is empty. Stale is true when any translated field
// no longer matches the English it was made from -- the feed withholds it,
// and a review UI should say so (migration design spec §2.4).
type translationJSON struct {
	Language    string  `json:"language"`
	Header      *string `json:"header"`
	Description *string `json:"description"`
	Stale       bool    `json:"stale"`
}
```

Change `groupTranslations` to take the alert and compute staleness:

```go
func groupTranslations(a alerts.Alert) []translationJSON {
	byLanguage := make(map[string]*translationJSON, len(a.Translations))
	for _, t := range a.Translations {
		entry, ok := byLanguage[t.Language]
		if !ok {
			entry = &translationJSON{Language: t.Language}
			byLanguage[t.Language] = entry
		}
		text := t.Text
		switch t.Field {
		case alerts.FieldHeader:
			entry.Header = &text
		case alerts.FieldDescription:
			entry.Description = &text
		}
		if a.TranslationStale(t) {
			entry.Stale = true
		}
	}

	// make, not nil: the field is always an array in the response.
	out := make([]translationJSON, 0, len(byLanguage))
	for _, entry := range byLanguage {
		out = append(out, *entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Language < out[j].Language })
	return out
}
```

In `toAlertJSON`, change `Translations: groupTranslations(a.Translations),` to `Translations: groupTranslations(a),`.

- [ ] **Step 7: Run the package**

Run: `go test ./internal/httpapi 2>&1 | tail -3`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/alerts internal/httpapi
git commit -m "httpapi: report translation staleness on admin alerts"
```

---

### Task 6: `sidecar-admin import --file -` reads stdin

**Files:**
- Modify: `cmd/sidecar-admin/import.go`, `cmd/sidecar-admin/commands.go` (the `import` dispatch line)
- Test: `cmd/sidecar-admin/import_test.go`

**Interfaces:**
- Produces: `runImport(ctx context.Context, stdin io.Reader, stdout io.Writer, store *sqlite.Store, now time.Time, args []string) error`; `--file -` reads the document from stdin and reports it as `stdin` in messages.

- [ ] **Step 1: Write the failing test**

Append to `cmd/sidecar-admin/import_test.go`:

```go
// TestImportCommand_ReadsStdin: `--file -` is the cutover runbook's shape
// (`cat export.json | render ssh sidecar -- sidecar-admin import --file -`),
// so there is no file to copy onto the host first.
func TestImportCommand_ReadsStdin(t *testing.T) {
	t.Parallel()
	dbPath, store := newDB(t)
	seedRegion(t, store.Regions(), 16)
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	doc := export.Document{
		Format: export.Format, ExportedAt: start, RegionID: 16,
		Alerts: []export.Alert{{ID: 77, AgencyID: "unitrans", HeaderText: "From stdin", StartTime: start, Published: true}},
	}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}

	stdout, _, err := cliStdin(t, bytes.NewReader(b), dbPath, "import", "--file", "-", "--dry-run")
	if err != nil || !strings.Contains(stdout, "dry run: stdin is a valid") {
		t.Fatalf("dry run from stdin: %v %q", err, stdout)
	}
	stdout, _, err = cliStdin(t, bytes.NewReader(b), dbPath, "import", "--file", "-")
	if err != nil || !strings.Contains(stdout, "imported region 16 from stdin") {
		t.Fatalf("import from stdin: %v %q", err, stdout)
	}
	if _, getErr := store.Alerts().Get(context.Background(), 77); getErr != nil {
		t.Fatalf("alert not imported: %v", getErr)
	}

	// A trailing second document on stdin is refused exactly like in a file.
	_, _, err = cliStdin(t, bytes.NewReader(append(b, []byte("\n{}")...)), dbPath, "import", "--file", "-")
	if err == nil || !strings.Contains(err.Error(), "after the document") {
		t.Fatalf("trailing content on stdin: err = %v", err)
	}
}
```

Add `"bytes"` to the file's imports.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./cmd/sidecar-admin -run TestImportCommand_ReadsStdin 2>&1 | tail -5`
Expected: FAIL — `open -: no such file or directory`.

- [ ] **Step 3: Implement**

Replace the top of `runImport` in `cmd/sidecar-admin/import.go` through the decode:

```go
// runImport loads an export document (internal/export; produced by
// OBACloud's `rake sidecar:export`) into this database. Rows that already
// exist are skipped, so the same command applied to a later export of the
// same region imports only what is new -- the "final delta" step of a
// region cutover (README, Migrating a region from OBACloud). `--file -`
// reads the document from stdin, so the cutover can pipe it over `render
// ssh` with no file transfer step (migration design spec section 2.5).
func runImport(ctx context.Context, stdin io.Reader, stdout io.Writer, store *sqlite.Store, now time.Time, args []string) error {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	file := fs.String("file", "", "path to the export document, or - for stdin (required)")
	dryRun := fs.Bool("dry-run", false, "validate the document and report what would be imported, without writing")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *file == "" {
		return errors.New("import requires --file")
	}
	var r io.Reader = stdin
	source := "stdin"
	if *file != "-" {
		f, err := os.Open(*file)
		if err != nil {
			return err
		}
		defer f.Close()
		r = f
		source = *file
	}
	var doc export.Document
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if decodeErr := dec.Decode(&doc); decodeErr != nil {
		return fmt.Errorf("import: %s: %w", source, decodeErr)
	}
	// One document per input: a second JSON value would otherwise be read
	// past silently and never imported.
	if _, trailingErr := dec.Token(); !errors.Is(trailingErr, io.EOF) {
		return fmt.Errorf("import: %s: unexpected content after the document", source)
	}
```

Then replace every remaining `*file` in the function body with `source` (the dry-run line and the `imported region %d from %s` line).

In `cmd/sidecar-admin/commands.go` `run`, change the dispatch to:

```go
	case "import":
		return runImport(ctx, stdin, stdout, store, now, cmdArgs)
```

and update `run`'s doc comment sentence "stdin feeds `user create`/`user passwd`'s --password-stdin and interactive prompt; every other command ignores it." to also name `survey create/edit --file -` and `import --file -`.

- [ ] **Step 4: Run the CLI tests**

Run: `go test ./cmd/sidecar-admin -run 'TestImport' 2>&1 | tail -5`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/sidecar-admin/import.go cmd/sidecar-admin/import_test.go cmd/sidecar-admin/commands.go
git commit -m "sidecar-admin: import --file - reads the export document from stdin"
```

---

### Task 7: `sidecar-admin sequence show|bump --min N`

**Files:**
- Create: `internal/store/sqlite/sequences.go`, `internal/store/sqlite/sequences_test.go`
- Create: `cmd/sidecar-admin/sequence.go`, `cmd/sidecar-admin/sequence_test.go`
- Modify: `cmd/sidecar-admin/commands.go` (dispatch and the two "expected ..." messages)

**Interfaces:**
- Produces: `sqlite.SequenceTables = []string{"alerts", "studies", "surveys", "survey_questions"}`; `(*Store).Sequences(ctx) (map[string]int64, error)` (0 for a table that has never had a row); `(*Store).BumpSequences(ctx, min int64) (map[string]int64, error)` returning the value each sequence had before.
- Produces: `sidecar-admin sequence show` → one `name\tseq` line per table; `sidecar-admin sequence bump --min N` → one `name: before -> after` line per table (`after == before` when already at or above N).

- [ ] **Step 1: Write the failing store test**

Create `internal/store/sqlite/sequences_test.go`:

```go
package sqlite_test

import (
	"context"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/alerts"
	"github.com/OneBusAway/sidecar/internal/regions"
	"github.com/OneBusAway/sidecar/internal/store/sqlite"
	"github.com/OneBusAway/sidecar/internal/store/sqlitetest"
)

// TestBumpSequences: the next id minted after a bump is above the floor,
// a lower bump is a no-op, and a table that has never had a row still gets
// its floor (sqlite_sequence has no row for it until then).
func TestBumpSequences(t *testing.T) {
	t.Parallel()
	store := sqlitetest.Open(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	if err := store.Regions().UpsertFromDirectory(ctx, []regions.Region{{ID: 1, Name: "R", OBABaseURL: "https://r.example/", Active: true}}, now); err != nil {
		t.Fatal(err)
	}
	newAlert := func(t *testing.T) int64 {
		t.Helper()
		a, err := store.Alerts().Create(ctx, alerts.NewAlert{RegionID: 1, AgencyID: "1", HeaderText: "h", StartTime: now}, now)
		if err != nil {
			t.Fatal(err)
		}
		return a.ID
	}
	first := newAlert(t) // 1

	before, err := store.Sequences(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if before["alerts"] != first || before["studies"] != 0 {
		t.Fatalf("Sequences before = %v", before)
	}

	const floor = 1_000_000
	prev, err := store.BumpSequences(ctx, floor)
	if err != nil {
		t.Fatalf("BumpSequences: %v", err)
	}
	if prev["alerts"] != first || prev["survey_questions"] != 0 {
		t.Errorf("previous values = %v", prev)
	}
	if got := newAlert(t); got != floor+1 {
		t.Errorf("next alert id = %d, want %d", got, floor+1)
	}

	after, err := store.Sequences(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range sqlite.SequenceTables {
		if after[name] < floor {
			t.Errorf("%s seq = %d, want >= %d", name, after[name], floor)
		}
	}

	// A lower floor changes nothing.
	if _, err := store.BumpSequences(ctx, 10); err != nil {
		t.Fatal(err)
	}
	if got := newAlert(t); got != floor+2 {
		t.Errorf("after a lower bump, next alert id = %d, want %d", got, floor+2)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/store/sqlite -run TestBumpSequences 2>&1 | head -5`
Expected: FAIL to compile — `store.Sequences undefined`.

- [ ] **Step 3: Implement the store methods**

Create `internal/store/sqlite/sequences.go`:

```go
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// SequenceTables are the AUTOINCREMENT tables whose ids an export document
// carries verbatim (internal/export): alerts, studies, surveys, and
// survey questions. After one region migrates, this database mints those
// ids from that region's maximum upward while OBACloud keeps minting for
// un-migrated regions from its own sequences, so a later region's import
// would collide with content authored here in between. BumpSequences moves
// every one of these above any id OBACloud could still hand out (migration
// design spec section 2.6).
var SequenceTables = []string{"alerts", "studies", "surveys", "survey_questions"}

// Sequences reports sqlite_sequence for every SequenceTables entry: the
// last id minted, or 0 for a table that has never had a row (SQLite creates
// the sqlite_sequence row on first insert).
//
// sqlite_sequence is not in the sqlc schema -- it is SQLite's own table --
// so this is one of the few hand-written statements in the adapter.
func (s *Store) Sequences(ctx context.Context) (map[string]int64, error) {
	out := make(map[string]int64, len(SequenceTables))
	for _, name := range SequenceTables {
		seq, _, err := readSequence(ctx, s.db, name)
		if err != nil {
			return nil, err
		}
		out[name] = seq
	}
	return out, nil
}

// BumpSequences raises every SequenceTables sequence to at least min, in
// one write transaction, and returns the value each had before. A sequence
// already at or above min is left alone, so the call is idempotent and
// can never move an id backwards.
func (s *Store) BumpSequences(ctx context.Context, min int64) (map[string]int64, error) {
	if min <= 0 {
		return nil, fmt.Errorf("sqlite: bump sequences: min must be positive, got %d", min)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("sqlite: bump sequences: begin tx: %w", err)
	}
	//nolint:errcheck // rollback after a successful commit is a documented no-op; the error is expected and safe to ignore
	defer func() { _ = tx.Rollback() }()

	before := make(map[string]int64, len(SequenceTables))
	for _, name := range SequenceTables {
		seq, found, err := readSequence(ctx, tx, name)
		if err != nil {
			return nil, err
		}
		before[name] = seq
		switch {
		case !found:
			// No row yet: sqlite_sequence has no unique index, so this is an
			// insert keyed on the row's absence, not an upsert -- a second
			// row for the same name would make SQLite's lookup ambiguous.
			if _, err := tx.ExecContext(ctx, `INSERT INTO sqlite_sequence (name, seq) VALUES (?, ?)`, name, min); err != nil {
				return nil, fmt.Errorf("sqlite: bump sequences: insert %s: %w", name, err)
			}
		case seq < min:
			if _, err := tx.ExecContext(ctx, `UPDATE sqlite_sequence SET seq = ? WHERE name = ?`, min, name); err != nil {
				return nil, fmt.Errorf("sqlite: bump sequences: update %s: %w", name, err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("sqlite: bump sequences: commit: %w", err)
	}
	return before, nil
}

// querier is the one method readSequence needs from *sql.DB and *sql.Tx.
type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// readSequence returns the sqlite_sequence value for name. found is false
// when the table has never had a row (SQLite creates the sqlite_sequence
// row on first insert); seq is 0 then.
func readSequence(ctx context.Context, q querier, name string) (seq int64, found bool, err error) {
	err = q.QueryRowContext(ctx, `SELECT seq FROM sqlite_sequence WHERE name = ?`, name).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("sqlite: read sequence %s: %w", name, err)
	}
	return seq, true, nil
}
```

`Sequences` calls `readSequence` as `seq, _, err := readSequence(ctx, s.db, name)`.

- [ ] **Step 4: Run the store test**

Run: `go test ./internal/store/sqlite -run TestBumpSequences -v 2>&1 | tail -5`
Expected: PASS.

- [ ] **Step 5: Write the failing CLI test**

Create `cmd/sidecar-admin/sequence_test.go`:

```go
package main

import (
	"context"
	"strings"
	"testing"

	"github.com/OneBusAway/sidecar/internal/store/sqlite"
)

// TestSequenceCommands: show lists every table; bump raises them to the
// floor and reports before -> after; a re-run is a no-op; the flag is
// required and must be positive.
func TestSequenceCommands(t *testing.T) {
	t.Parallel()
	dbPath, store := newDB(t)

	out, _, err := cli(t, dbPath, "sequence", "show")
	if err != nil {
		t.Fatalf("sequence show: %v", err)
	}
	for _, name := range sqlite.SequenceTables {
		if !strings.Contains(out, name+"\t0\n") {
			t.Errorf("show output lacks %q at 0:\n%s", name, out)
		}
	}

	out, _, err = cli(t, dbPath, "sequence", "bump", "--min", "1000000")
	if err != nil {
		t.Fatalf("sequence bump: %v", err)
	}
	if !strings.Contains(out, "alerts: 0 -> 1000000") {
		t.Errorf("bump output:\n%s", out)
	}
	seqs, err := store.Sequences(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range sqlite.SequenceTables {
		if seqs[name] != 1000000 {
			t.Errorf("%s = %d after bump", name, seqs[name])
		}
	}

	out, _, err = cli(t, dbPath, "sequence", "bump", "--min", "10")
	if err != nil || !strings.Contains(out, "alerts: 1000000 -> 1000000") {
		t.Errorf("lower bump: %v\n%s", err, out)
	}

	cliErrContains(t, dbPath, "requires --min", "sequence", "bump")
	cliErrContains(t, dbPath, "must be positive", "sequence", "bump", "--min", "0")
	cliErrContains(t, dbPath, "subcommand", "sequence")
	cliErrContains(t, dbPath, "unknown sequence subcommand", "sequence", "reset")
}
```

- [ ] **Step 6: Run to verify failure**

Run: `go test ./cmd/sidecar-admin -run TestSequenceCommands 2>&1 | tail -5`
Expected: FAIL — `unknown command "sequence"`.

- [ ] **Step 7: Implement the command**

Create `cmd/sidecar-admin/sequence.go`:

```go
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/OneBusAway/sidecar/internal/store/sqlite"
)

// runSequence dispatches `sidecar-admin sequence`'s subcommands: the
// id-sequence headroom tooling for migrating regions from OBACloud
// (migration design spec section 2.6; README, Migrating a region from
// OBACloud).
func runSequence(ctx context.Context, stdout io.Writer, store *sqlite.Store, args []string) error {
	if len(args) == 0 {
		return errors.New("sequence requires a subcommand: show, bump")
	}
	cmd, cmdArgs := args[0], args[1:]
	switch cmd {
	case "show":
		return sequenceShow(ctx, stdout, store, cmdArgs)
	case "bump":
		return sequenceBump(ctx, stdout, store, cmdArgs)
	default:
		return fmt.Errorf("unknown sequence subcommand %q; expected show or bump", cmd)
	}
}

// sequenceShow prints one "table<TAB>seq" line per id sequence the import
// preserves, in sqlite.SequenceTables order so the output is stable.
func sequenceShow(ctx context.Context, stdout io.Writer, store *sqlite.Store, args []string) error {
	fs := flag.NewFlagSet("sequence show", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		return err
	}
	seqs, err := store.Sequences(ctx)
	if err != nil {
		return fmt.Errorf("sequence show: %w", err)
	}
	for _, name := range sqlite.SequenceTables {
		fmt.Fprintf(stdout, "%s\t%d\n", name, seqs[name])
	}
	return nil
}

// sequenceBump raises every preserved id sequence to at least --min and
// prints before -> after per table. Re-running with the same or a lower
// floor changes nothing, so the cutover runbook can call it unconditionally.
func sequenceBump(ctx context.Context, stdout io.Writer, store *sqlite.Store, args []string) error {
	fs := flag.NewFlagSet("sequence bump", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	min := fs.Int64("min", 0, "floor for every sequence (required; the runbook uses 1000000)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !visitedFlags(fs)["min"] {
		return errors.New("sequence bump requires --min")
	}
	if *min <= 0 {
		return fmt.Errorf("sequence bump: --min must be positive, got %d", *min)
	}
	before, err := store.BumpSequences(ctx, *min)
	if err != nil {
		return fmt.Errorf("sequence bump: %w", err)
	}
	after, err := store.Sequences(ctx)
	if err != nil {
		return fmt.Errorf("sequence bump: %w", err)
	}
	for _, name := range sqlite.SequenceTables {
		fmt.Fprintf(stdout, "%s: %d -> %d\n", name, before[name], after[name])
	}
	return nil
}
```

In `cmd/sidecar-admin/commands.go` `run`:
- add `case "sequence": return runSequence(ctx, stdout, store, cmdArgs)` to the switch;
- add `sequence` to both "expected region, alert, study, survey, ghostbus, import, migrate, user, key, or principal" messages (the missing-command error and the default case): `"... import, migrate, sequence, user, key, or principal"`.

- [ ] **Step 8: Run the CLI test**

Run: `go test ./cmd/sidecar-admin -run 'TestSequenceCommands|TestRun' 2>&1 | tail -5`
Expected: PASS. If a test in `commands_test.go` pins the exact "expected …" message, update it to include `sequence`.

- [ ] **Step 9: Commit**

```bash
git add internal/store/sqlite/sequences.go internal/store/sqlite/sequences_test.go cmd/sidecar-admin/sequence.go cmd/sidecar-admin/sequence_test.go cmd/sidecar-admin/commands.go
git commit -m "sidecar-admin: sequence show / bump --min for id headroom before a migration"
```

---

### Task 8: Question ids survive `PUT /surveys/{id}`

Today `UpdateSurvey` keeps ids only when the question set is byte-identical; any change deletes and re-inserts every question with new ids. Apps persist question ids, so a document that names an id must keep it, and a question without one gets a fresh id.

**Files:**
- Modify: `internal/surveys/surveys.go` (`QuestionDefinition.ID`, `ErrUnknownQuestion`), `internal/surveys/definition.go` (`QuestionsEqual`), `internal/surveys/codec.go` (`DefinitionFromDocument`)
- Modify: `internal/store/sqlite/queries/surveys.sql` (`InsertQuestionWithID`), `internal/store/sqlite/surveys.go`
- Modify: `internal/httpapi/admin_surveys.go` (`writeSurveyStoreError`)
- Test: `internal/surveys/definition_test.go`, `internal/store/storetest/surveytest.go`, `internal/httpapi/admin_surveys_test.go`

**Interfaces:**
- Produces: `surveys.QuestionDefinition{ID *int64; Required bool; Content Content}`; `surveys.ErrUnknownQuestion` (a document names a question id that is not this survey's → HTTP 422); `QuestionsEqual(stored []Question, want []QuestionDefinition) bool` also requires each present `want[i].ID` to equal `stored[i].ID`.
- Produces: `UpdateSurvey` semantics — when the set changes and the survey has no responses: questions carrying an id keep it, questions without an id get a new one, stored questions absent from the document are deleted, positions follow document order. `CreateSurvey*` refuse a definition carrying any question id with `ErrUnknownQuestion`.

- [ ] **Step 1: Write the failing domain test**

Append to `internal/surveys/definition_test.go`:

```go
func TestQuestionsEqual_IDs(t *testing.T) {
	t.Parallel()
	content := surveys.Content{Type: surveys.TypeText, LabelText: "Q"}
	stored := []surveys.Question{{ID: 10, Position: 1, Content: content}, {ID: 11, Position: 2, Content: content}}
	id := func(n int64) *int64 { return &n }

	if !surveys.QuestionsEqual(stored, []surveys.QuestionDefinition{{Content: content}, {Content: content}}) {
		t.Error("no ids: same content must be equal")
	}
	if !surveys.QuestionsEqual(stored, []surveys.QuestionDefinition{{ID: id(10), Content: content}, {ID: id(11), Content: content}}) {
		t.Error("matching ids in order must be equal")
	}
	if surveys.QuestionsEqual(stored, []surveys.QuestionDefinition{{ID: id(11), Content: content}, {ID: id(10), Content: content}}) {
		t.Error("reordered ids are a change even with identical content")
	}
	if surveys.QuestionsEqual(stored, []surveys.QuestionDefinition{{ID: id(10), Content: content}, {Content: content}}) {
		t.Error("an id-less question in place of a stored one is a change")
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/surveys -run TestQuestionsEqual_IDs 2>&1 | head -5`
Expected: FAIL to compile — `unknown field ID in struct literal`.

- [ ] **Step 3: Implement the domain changes**

In `internal/surveys/surveys.go`:

```go
	// ErrUnknownQuestion blocks a write whose document names a question id
	// that is not this survey's (or names one twice, or names any on
	// create): ids are server-owned, and a wrong one would silently rebind
	// stored answers (migration design spec section 2.7). The HTTP layer
	// maps it to 422.
	ErrUnknownQuestion = errors.New("question id does not belong to this survey")
```

```go
// QuestionDefinition is one question in an authoring document; position is
// implied by array order (design spec §2.13). ID, when set on an edit, is
// the stored question this entry keeps: apps persist question ids, so an
// edit that changes one question must not renumber the rest. An entry
// without an id is a new question.
type QuestionDefinition struct {
	ID       *int64  `json:"id,omitempty"`
	Required bool    `json:"required"`
	Content  Content `json:"content"`
}
```

In `internal/surveys/definition.go`, replace `QuestionsEqual`:

```go
// QuestionsEqual reports whether a document's questions are, in order,
// identical to the stored set -- the test for whether an edit touches a
// frozen survey's questions (design spec §2.13). A document entry that
// names an id must name the stored question at that position; an entry
// without an id matches on content alone, which is how a document written
// before ids existed still reads as "unchanged".
func QuestionsEqual(stored []Question, want []QuestionDefinition) bool {
	if len(stored) != len(want) {
		return false
	}
	// The rule: a document that names NO ids matches on content alone; a
	// document that names ANY id must name every stored question's id at
	// its position (a mixed document is a change -- the id-less entry is a
	// new question).
	anyID := false
	for _, q := range want {
		if q.ID != nil {
			anyID = true
			break
		}
	}
	for i := range stored {
		if anyID && (want[i].ID == nil || *want[i].ID != stored[i].ID) {
			return false
		}
		if stored[i].Required != want[i].Required || !ContentEqual(stored[i].Content, want[i].Content) {
			return false
		}
	}
	return true
}
```

In `internal/surveys/codec.go` `DefinitionFromDocument`, carry the id:

```go
	for _, q := range doc.Questions {
		def.Questions = append(def.Questions, QuestionDefinition{ID: q.ID, Required: q.Required, Content: q.Content})
	}
```

Run: `go test ./internal/surveys 2>&1 | tail -3`
Expected: PASS.

- [ ] **Step 4: Write the failing store conformance test**

In `internal/store/storetest/surveytest.go`, register after `"UpdateKeepsQuestionIDsWhenUnchanged"`:

```go
	t.Run("UpdatePreservesNamedQuestionIDs", func(t *testing.T) { testUpdatePreservesNamedQuestionIDs(t, newStore) })
	t.Run("UpdateRefusesForeignQuestionID", func(t *testing.T) { testUpdateRefusesForeignQuestionID(t, newStore) })
	t.Run("CreateRefusesQuestionIDs", func(t *testing.T) { testCreateRefusesQuestionIDs(t, newStore) })
```

Add the functions:

```go
// testUpdatePreservesNamedQuestionIDs is migration design spec section
// 2.7: an edit that changes one question, adds another, drops a third, and
// names the kept ids keeps them; the new question gets a fresh id; order
// follows the document.
func testUpdatePreservesNamedQuestionIDs(t *testing.T, newStore newSurveyStoreFunc) {
	t.Parallel()
	repo, regs := newStore(t)
	st := seedStudy(t, repo, regs, 1)
	s := mustCreateSurvey(t, repo, st.ID, surveyDef("v1")) // two questions
	keep, drop := s.Questions[0].ID, s.Questions[1].ID

	def := surveyDef("v2")
	edited := def.Questions[0]
	edited.ID = &keep
	edited.Content.Options = []string{"Good", "Bad", "Ugly"}
	added := surveys.QuestionDefinition{Content: surveys.Content{Type: surveys.TypeText, LabelText: "Anything else?"}}
	def.Questions = []surveys.QuestionDefinition{added, edited}
	if err := def.Validate(); err != nil {
		t.Fatal(err)
	}
	got, err := repo.UpdateSurvey(context.Background(), s.ID, def, base.Add(time.Hour))
	if err != nil {
		t.Fatalf("UpdateSurvey: %v", err)
	}
	if len(got.Questions) != 2 {
		t.Fatalf("Questions = %+v, want 2", got.Questions)
	}
	if got.Questions[0].ID == keep || got.Questions[0].ID == drop || got.Questions[0].Position != 1 {
		t.Errorf("new question = %+v, want a fresh id at position 1", got.Questions[0])
	}
	if got.Questions[1].ID != keep || got.Questions[1].Position != 2 ||
		!reflect.DeepEqual(got.Questions[1].Content.Options, []string{"Good", "Bad", "Ugly"}) {
		t.Errorf("kept question = %+v, want id %d at position 2 with the edited options", got.Questions[1], keep)
	}
	reread, err := repo.GetSurvey(context.Background(), s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(reread.Questions, got.Questions) {
		t.Errorf("GetSurvey questions = %+v, want %+v", reread.Questions, got.Questions)
	}
}

func testUpdateRefusesForeignQuestionID(t *testing.T, newStore newSurveyStoreFunc) {
	t.Parallel()
	repo, regs := newStore(t)
	st := seedStudy(t, repo, regs, 1)
	a := mustCreateSurvey(t, repo, st.ID, surveyDef("a"))
	b := mustCreateSurvey(t, repo, st.ID, surveyDef("b"))

	for _, tc := range []struct {
		name string
		id   int64
	}{
		{"another survey's question", b.Questions[0].ID},
		{"no such question", 999999},
	} {
		t.Run(tc.name, func(t *testing.T) {
			def := surveyDef("a")
			def.Questions[0].ID = &tc.id
			if err := def.Validate(); err != nil {
				t.Fatal(err)
			}
			_, err := repo.UpdateSurvey(context.Background(), a.ID, def, base.Add(time.Hour))
			if !errors.Is(err, surveys.ErrUnknownQuestion) {
				t.Fatalf("err = %v, want ErrUnknownQuestion", err)
			}
		})
	}
	t.Run("duplicate id", func(t *testing.T) {
		def := surveyDef("a")
		id := a.Questions[0].ID
		def.Questions[0].ID, def.Questions[1].ID = &id, &id
		if err := def.Validate(); err != nil {
			t.Fatal(err)
		}
		if _, err := repo.UpdateSurvey(context.Background(), a.ID, def, base.Add(time.Hour)); !errors.Is(err, surveys.ErrUnknownQuestion) {
			t.Fatalf("err = %v, want ErrUnknownQuestion", err)
		}
	})
	got, err := repo.GetSurvey(context.Background(), a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Questions, a.Questions) {
		t.Errorf("a refused edit changed the questions: %+v", got.Questions)
	}
}

func testCreateRefusesQuestionIDs(t *testing.T, newStore newSurveyStoreFunc) {
	t.Parallel()
	repo, regs := newStore(t)
	st := seedStudy(t, repo, regs, 1)
	def := surveyDef("c")
	id := int64(5)
	def.Questions[0].ID = &id
	if err := def.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CreateSurvey(context.Background(), st.ID, def, base); !errors.Is(err, surveys.ErrUnknownQuestion) {
		t.Errorf("CreateSurvey err = %v, want ErrUnknownQuestion", err)
	}
	if _, err := repo.CreateSurveyInRegion(context.Background(), 1, st.ID, def, base); !errors.Is(err, surveys.ErrUnknownQuestion) {
		t.Errorf("CreateSurveyInRegion err = %v, want ErrUnknownQuestion", err)
	}
}
```

- [ ] **Step 5: Run to verify failure**

Run: `go test ./internal/store/sqlite -run 'TestSurveyRepository/(UpdatePreservesNamedQuestionIDs|UpdateRefusesForeignQuestionID|CreateRefusesQuestionIDs)' 2>&1 | tail -15`
Expected: FAIL — ids renumbered; foreign ids accepted; create accepts ids.

- [ ] **Step 6: Add the query and implement the adapter**

In `internal/store/sqlite/queries/surveys.sql`, after `InsertQuestion`:

```sql
-- name: InsertQuestionWithID :one
-- An edit re-inserts a kept question under its original id (migration
-- design spec section 2.7); AUTOINCREMENT still advances past it.
INSERT INTO survey_questions (id, survey_id, position, required, question_type, content, created_at, updated_at)
VALUES (@id, @survey_id, @position, @required, @question_type, @content, @now, @now)
RETURNING *;
```

Run: `make generate`

In `internal/store/sqlite/surveys.go`:

Replace `insertQuestions`:

```go
// insertQuestions writes defs in document order. An entry carrying an id
// is re-inserted under that id -- the caller has already proven it belongs
// to this survey (checkQuestionIDs) and deleted the old rows -- so a kept
// question keeps the id apps have persisted; an entry without one gets a
// fresh AUTOINCREMENT id.
func insertQuestions(ctx context.Context, q *gen.Queries, surveyID int64, defs []surveys.QuestionDefinition, now int64) error {
	for i, qd := range defs {
		content, err := json.Marshal(qd.Content)
		if err != nil {
			return fmt.Errorf("question %d: %w", i+1, err)
		}
		if qd.ID != nil {
			_, err = q.InsertQuestionWithID(ctx, gen.InsertQuestionWithIDParams{
				ID: *qd.ID, SurveyID: surveyID, Position: int64(i + 1), Required: qd.Required,
				QuestionType: qd.Content.Type, Content: string(content), Now: now,
			})
		} else {
			_, err = q.InsertQuestion(ctx, gen.InsertQuestionParams{
				SurveyID: surveyID, Position: int64(i + 1), Required: qd.Required,
				QuestionType: qd.Content.Type, Content: string(content), Now: now,
			})
		}
		if err != nil {
			return fmt.Errorf("question %d: %w", i+1, err)
		}
	}
	return nil
}

// checkQuestionIDs proves every id a document names is one of stored's and
// is named once. Anything else is surveys.ErrUnknownQuestion, decided
// before a single row is touched.
func checkQuestionIDs(stored []surveys.Question, defs []surveys.QuestionDefinition) error {
	own := make(map[int64]bool, len(stored))
	for _, q := range stored {
		own[q.ID] = true
	}
	seen := make(map[int64]bool, len(defs))
	for i, qd := range defs {
		if qd.ID == nil {
			continue
		}
		if !own[*qd.ID] || seen[*qd.ID] {
			return fmt.Errorf("question %d (id %d): %w", i+1, *qd.ID, surveys.ErrUnknownQuestion)
		}
		seen[*qd.ID] = true
	}
	return nil
}

// rejectQuestionIDsOnCreate refuses a create whose document names ids:
// there is nothing to keep yet, and honouring one would collide with, or
// steal, another survey's row.
func rejectQuestionIDsOnCreate(defs []surveys.QuestionDefinition) error {
	for i, qd := range defs {
		if qd.ID != nil {
			return fmt.Errorf("question %d (id %d): %w", i+1, *qd.ID, surveys.ErrUnknownQuestion)
		}
	}
	return nil
}
```

At the top of both `CreateSurvey` and `CreateSurveyInRegion` (before any DB call), add:

```go
	if err := rejectQuestionIDsOnCreate(def.Questions); err != nil {
		return surveys.Survey{}, fmt.Errorf("sqlite: create survey: %w", err)
	}
```

In `UpdateSurvey`, after `stored, err := loadSurvey(...)` and before `replaceQuestions := false`:

```go
	if err := checkQuestionIDs(stored.Questions, def.Questions); err != nil {
		return surveys.Survey{}, fmt.Errorf("sqlite: update survey %d: %w", id, err)
	}
```

The existing delete-all-then-`insertQuestions` path now preserves named ids; no other change is needed there. Update the comment block above `replaceQuestions` to say: "The question set is replaced (delete all, insert in document order -- a kept question is re-inserted under the id the document names, migration design spec §2.7) only when the document's questions differ from the stored set ...".

- [ ] **Step 7: Run the conformance suite**

Run: `go test ./internal/store/... 2>&1 | tail -3`
Expected: PASS.

- [ ] **Step 8: Write the failing handler test**

Append to `internal/httpapi/admin_surveys_test.go`:

```go
// TestAdminSurveys_PutPreservesQuestionIDs is the Rails-side round trip
// (migration design spec section 4.3): GET, strip the server-owned survey
// keys, edit, PUT -- with question ids left in place. The edited question
// keeps its id, the new one gets a fresh id, and a foreign id is 422.
func TestAdminSurveys_PutPreservesQuestionIDs(t *testing.T) {
	t.Parallel()

	f := newFullAdminFixture(t)
	studyID := jsonID(t, object(t, f.do(http.MethodPost, "/api/admin/v1/regions/1/studies", `{"name":"s"}`), http.StatusCreated))
	create := fmt.Sprintf(`{"study_id":%d,"name":"q","questions":[{"content":{"type":"text","label_text":"one"}},{"content":{"type":"text","label_text":"two"}}]}`, studyID)
	id := jsonID(t, object(t, f.do(http.MethodPost, "/api/admin/v1/regions/1/surveys", create), http.StatusCreated))
	path := fmt.Sprintf("/api/admin/v1/regions/1/surveys/%d", id)

	shown := object(t, f.do(http.MethodGet, path, ""), http.StatusOK)
	questions, _ := shown["questions"].([]any)
	first, _ := questions[0].(map[string]any)
	second, _ := questions[1].(map[string]any)
	firstID, secondID := int64(num(t, first, "id")), int64(num(t, second, "id"))

	put := fmt.Sprintf(`{"name":"q","questions":[{"id":%d,"content":{"type":"text","label_text":"one, edited"}},{"content":{"type":"text","label_text":"three"}}]}`, firstID)
	updated := object(t, f.do(http.MethodPut, path, put), http.StatusOK)
	got, _ := updated["questions"].([]any)
	if len(got) != 2 {
		t.Fatalf("questions = %v, want 2", updated["questions"])
	}
	kept, _ := got[0].(map[string]any)
	added, _ := got[1].(map[string]any)
	if int64(num(t, kept, "id")) != firstID {
		t.Errorf("edited question id = %v, want %d", kept["id"], firstID)
	}
	if aid := int64(num(t, added, "id")); aid == firstID || aid == secondID {
		t.Errorf("added question id = %d, want a fresh id", aid)
	}

	foreign := fmt.Sprintf(`{"name":"q","questions":[{"id":%d,"content":{"type":"text","label_text":"x"}}]}`, secondID+1000)
	rec := f.do(http.MethodPut, path, foreign)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("foreign question id: status = %d, want 422; body = %s", rec.Code, rec.Body.String())
	}
	if got, want := bodyText(rec), fmt.Sprintf(`{"error":%q}`, surveys.ErrUnknownQuestion.Error()); got != want {
		t.Errorf("body = %s, want %s", got, want)
	}

	// Create refuses ids too: they are server-owned.
	withID := fmt.Sprintf(`{"study_id":%d,"name":"q","questions":[{"id":%d,"content":{"type":"text","label_text":"x"}}]}`, studyID, firstID)
	if rec := f.do(http.MethodPost, "/api/admin/v1/regions/1/surveys", withID); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("question id on create: status = %d, want 422", rec.Code)
	}
}
```

- [ ] **Step 9: Run to verify failure**

Run: `go test ./internal/httpapi -run TestAdminSurveys_PutPreservesQuestionIDs 2>&1 | tail -10`
Expected: FAIL — the foreign id answers 500 (unmapped `ErrUnknownQuestion`), or the edited id changed.

- [ ] **Step 10: Map the sentinel**

In `internal/httpapi/admin_surveys.go` `writeSurveyStoreError`, add a case:

```go
	case errors.Is(err, surveys.ErrUnknownQuestion):
		// The document named an id that is not this survey's: a body
		// fault, reported with the sentinel's own text (design spec
		// section 5).
		writeJSONError(w, logger, http.StatusUnprocessableEntity, surveys.ErrUnknownQuestion.Error())
```

and mention `ErrUnknownQuestion` in that function's doc comment alongside the other sentinels.

- [ ] **Step 11: Run the package and the CLI**

Run: `go test ./internal/httpapi ./cmd/sidecar-admin 2>&1 | tail -3`
Expected: PASS (`survey show | survey edit --file -` round-trips ids through the same `Definition`; `TestSurveyEdit*` still pass).

- [ ] **Step 12: Commit**

```bash
git add internal/surveys internal/store internal/httpapi
git commit -m "surveys: PUT preserves question ids named by the document"
```

---

### Task 9: Spec and README amendments

Documentation only. Each edit below is an exact find/replace; keep the surrounding prose intact.

**Files:**
- Modify: `docs/superpowers/specs/2026-08-26-region-api-keys-and-admin-api-design.md`
- Modify: `README.md`

- [ ] **Step 1: Keys spec §2.1 — region-key blast radius**

Find:

```
via `PATCH /regions/{id}` (which redirects the region's sidecar-side OBA calls — ghost
bus snapshots, vehicle search, alarms — to a key the holder controls). It cannot send
push notifications (push create/cancel are operator-only, §4.5), reach another region,
or mint or revoke keys. The remedy is revocation.
```

Replace with:

```
via `PATCH /regions/{id}` (which redirects the region's sidecar-side OBA calls — ghost
bus snapshots, vehicle search, alarms — to a key the holder controls). It cannot reach
another region or mint or revoke keys. Whether it can send push notifications depends
on its **scopes** (§3, §5.6): an unscoped key cannot (push create/cancel are
`operatorOrPushKey`, §4.5); a key minted with the `push` scope — which OBACloud holds
for every migrated region — can deliver notifications to every device in the region.
The remedy in either case is revocation.
```

- [ ] **Step 2: Keys spec §2.2 — principal blast radius**

Find:

```
radius per-region keys avoid. **What a leaked service principal can do**, stated
plainly: mint a live key for any published region (then use it, with the region-key
exposure above), revoke every region key in the deployment — a deployment-wide denial
```

Replace with:

```
radius per-region keys avoid. **What a leaked service principal can do**, stated
plainly: mint a live key for any published region — including a push-scoped one, so it
can deliver notifications to every device in every region (then use it, with the
region-key exposure above), revoke every region key in the deployment — a deployment-wide denial
```

- [ ] **Step 3: Keys spec §3 — data model**

Find the line `    key_hash        TEXT    NOT NULL UNIQUE,` inside the `region_api_keys` CREATE TABLE and add after it:

```
    scopes          TEXT    NOT NULL DEFAULT '[]',  -- JSON array; the only scope is "push" (00013)
```

After the sentence ending `must not orphan the audit trail.` (end of §3's prose, before `### 3.1`), add a paragraph:

```
`scopes` (migration `00013_region_api_key_scopes.sql`) is a JSON array of scope names.
The only defined scope is `push`, which admits the key to `POST …/pushes` and
`DELETE …/pushes/{pushId}` (§4.5). Unknown names are refused at mint time, never
stored.
```

- [ ] **Step 4: Keys spec §4.5 — allowed principals**

Replace the table row:

```
| `POST …/pushes`, `DELETE …/pushes/{pushId}` | ✓ | — | — |
```

with:

```
| `POST …/pushes`, `DELETE …/pushes/{pushId}` | ✓ | ✓ only with the `push` scope | — |
```

Replace the paragraph:

```
Sending or cancelling a push is operator-only: a leaked region key must not be able to
deliver attacker text as a notification to every device in the region, and
OBACloud's migration plan (§7.4) never sends pushes. Push reads stay available so
OBACloud can show status.
```

with:

```
Sending or cancelling a push takes an operator or a region key carrying the `push`
scope (`operatorOrPushKey`): an ordinary leaked region key must not be able to deliver
attacker text as a notification to every device in the region, while OBACloud — which
keeps its push wizard and drives the sidecar's push routes at send time (migration
design §2.2) — holds a push-scoped key per migrated region and accepts that exposure.
Push reads stay available to every region key.
```

- [ ] **Step 5: Keys spec §5 route list and §5.6**

Replace the two annotated lines:

```
POST   /regions/{regionId}/alerts/{id}/pushes           operator-only
```
→
```
POST   /regions/{regionId}/alerts/{id}/pushes           operator or push-scoped key; body {audience?, messages?}
```
and
```
DELETE /regions/{regionId}/alerts/{id}/pushes/{pushId}  operator-only
```
→
```
DELETE /regions/{regionId}/alerts/{id}/pushes/{pushId}  operator or push-scoped key
```

In §5.6 replace:

```
POST   /regions/{regionId}/api_keys            {name} → 201 {id, name, key, created_by, created_at}; Cache-Control: no-store; no Location header
GET    /regions/{regionId}/api_keys            [{id, name, created_by: {kind, id}, created_at, last_used_at, revoked_at, revoked_by}]
```

with:

```
POST   /regions/{regionId}/api_keys            {name, scopes?: ["push"]} → 201 {id, name, scopes, key, created_by, created_at}; Cache-Control: no-store; no Location header; unknown scope → 400
GET    /regions/{regionId}/api_keys            [{id, name, scopes, created_by: {kind, id}, created_at, last_used_at, revoked_at, revoked_by}]
```

In §6.1 replace:

```
sidecar-admin key create --region N --name NAME             prints the raw key once, then id/name; created_by = cli
sidecar-admin key list --region N                           id, name, created by, created, last used, revoked, revoked by
```

with:

```
sidecar-admin key create --region N --name NAME [--scope push]   prints the raw key once, then id/name/scopes; created_by = cli
sidecar-admin key list --region N                           id, name, created by, created, last used, revoked, revoked by, scopes
```

- [ ] **Step 6: Keys spec §7.2 and §7.4**

In §7.2 replace:

```
  `obasp_…`, one entry per sidecar deployment. `Region.sidecar_base_url` **must
  validate as a key of that map**; an unknown base URL is a hard validation error and
  never falls back to a default principal. (Its current default,
  `https://dashboard.onebusawaycloud.com`, must be a real sidecar with an entry, or
  the default must change before this ships.) Principals are created by an operator
  with `sidecar-admin principal create`.
```

with:

```
  `obasp_…`, one entry per sidecar deployment. `Region.sidecar_base_url` **must
  validate as a key of that map once the region is migrated** (`sidecar_migrated_at`
  set — migration design §3.2); an un-migrated region keeps the Rails-hosted default
  (`https://sidecar.onebusaway.org`) with no principal, and an unknown base URL on a
  migrated region is a hard validation error that never falls back to a default
  principal. Principals are created by an operator with `sidecar-admin principal
  create`. Keys are minted with `scopes: ["push"]` (§5.6) so the wizard can send.
```

In §7.4 replace `OBACloud never sends pushes through the sidecar.` with:

```
OBACloud keeps its push wizard, copywriter, scheduling, and test pushes, and sends
through the sidecar's push routes with a push-scoped key and its own `messages`
(migration design §2.2–2.3).
```

- [ ] **Step 7: README — push section**

In `README.md` under "Sending alerts as push notifications", replace the route line:

```
POST   /api/admin/v1/regions/{regionId}/alerts/{id}/pushes            {"audience":"all"|"test"}
```

with:

```
POST   /api/admin/v1/regions/{regionId}/alerts/{id}/pushes            {"audience":"all"|"test","messages":{...}?}
```

Replace the paragraph beginning `Authenticated like the rest of ` and ending `has already finished.` with:

```
Authenticated like the rest of `/api/admin/v1` -- a session cookie or a bearer
credential (see *Region API keys and service principals* below) -- except that
`POST` and `DELETE` take an operator **or a region key minted with the `push`
scope**: an ordinary region key must not be able to deliver attacker-controlled
text as a push to every device in the region, so sending and canceling answer
it with `403` even though it can read everything else about the region's
alerts, while OBACloud's key for a migrated region carries the scope on purpose.
`POST` answers `202` with the queued push and wakes the dispatcher immediately;
`GET …/pushes` answers `200` with that alert's pushes, newest first; `DELETE`
cancels one and answers `204`; `GET …/push_audience` answers `200` with the
`all` and `test` device counts (split by platform) and a `forced_test` flag for
a test alert. `POST` may carry `messages` in the same per-language
`{"en": {"title","body"}, …}` shape the response emits; when present it is
stored as the push's copy snapshot verbatim instead of being derived from the
alert, and it must include `en`, use trimmed lowercase language tags, have a
non-blank body in every language, and respect the same 48-rune title and
120-rune body caps the derivation applies. Errors are `{"error": "..."}`:
`400` for a malformed body, an audience that is neither `all` nor `test`, or
`messages` that break one of those rules, `404` for an unknown alert, an
unknown push, or a `pushId` belonging to a different alert, and `409` for each
precondition (unpublished alert, a push already in flight, empty audience) and
for canceling a push that has already finished.
```

Under "#### Other admin route families", replace the `api_keys` line:

```
GET    /api/admin/v1/regions/{regionId}/api_keys                    region API keys: list (also POST to mint, DELETE .../{keyId} to revoke)
```

with:

```
GET    /api/admin/v1/regions/{regionId}/api_keys                    region API keys: list with scopes (also POST {name, scopes?} to mint, DELETE .../{keyId} to revoke)
```

- [ ] **Step 8: README — alerts admin JSON `stale`**

Find the README's description of the admin alert JSON `translations` array (search `translations` under the "Service alerts" admin API text; if the README does not describe the per-translation fields, add this sentence at the end of the paragraph that introduces `PUT …/translations/{lang}`):

```
Every translation in an alert's admin JSON carries `"stale": true|false`: true
when the English it was made from has since changed, which is exactly when the
feed withholds it -- so a review UI can show what riders will not see and offer
retranslation.
```

- [ ] **Step 9: README — region keys and service principals**

In the first bullet ("**A region API key** ..."), replace:

```
  family is refused (`403`), since a region key is not one of the principal
  kinds it accepts. And one thing stays off limits even *inside* its own
  region: sending or canceling a push notification is operator-only and
  answers a region key with `403` too -- a leaked key must not be able to
  page every device in the region. A leaked region key therefore reaches
  one region's tenant data and, through its OBA key, that region's own OBA
  traffic -- but it cannot reach another tenant, send a push, or mint or
  revoke anything. **The remedy for a leaked region key is to revoke it**
  (below) and mint a replacement.
```

with:

```
  family is refused (`403`), since a region key is not one of the principal
  kinds it accepts. Sending or canceling a push notification is gated on a
  **scope**: a key minted without one answers `403` there too -- a leaked
  ordinary key must not be able to page every device in the region -- while
  a key minted with `"scopes": ["push"]` (what OBACloud holds for each
  migrated region) can send and cancel pushes for its own region. A leaked
  region key therefore reaches one region's tenant data and, through its OBA
  key, that region's own OBA traffic; a leaked **push-scoped** key can also
  deliver notifications to every device in that region. It cannot reach
  another tenant or mint or revoke anything. **The remedy for a leaked
  region key, scoped or not, is to revoke it** (below) and mint a
  replacement.
```

In the second bullet, replace:

```
  route answers it with `403`. That is a deliberate trade. A leaked service
  principal can mint itself a live key for any published region (and then
  use it, with the region-key exposure above), revoke every region key in
```

with:

```
  route answers it with `403`. That is a deliberate trade. A leaked service
  principal can mint itself a live key for any published region -- including
  a push-scoped one, so it can deliver notifications to every device in every
  region -- (and then use it, with the region-key exposure above), revoke every region key in
```

Replace the curl example's body and comment:

```
  -d '{"name":"obacloud rails1"}'
# {"id":1,"name":"obacloud rails1","key":"obask_1_L_RvltB_P6G8UwZ9…", …}
```

with:

```
  -d '{"name":"obacloud rails1","scopes":["push"]}'
# {"id":1,"name":"obacloud rails1","scopes":["push"],"key":"obask_1_L_RvltB_P6G8UwZ9…", …}
```

Replace the manual-key example block:

```
./bin/sidecar-admin --db ./sidecar.db key create --region 1 --name "manual test key"
# obask_1_WlDL9LeQxtC1KYww…
# id: 2  name: manual test key

./bin/sidecar-admin --db ./sidecar.db key list --region 1
# 2  manual test key    cli            2026-08-28T00:39:13Z  —  —  —
# 1  obacloud rails1    principal:1    2026-08-28T00:39:05Z  —  —  —
```

with:

```
./bin/sidecar-admin --db ./sidecar.db key create --region 1 --name "manual test key"
# obask_1_WlDL9LeQxtC1KYww…
# id: 2  name: manual test key  scopes: —

./bin/sidecar-admin --db ./sidecar.db key create --region 1 --name "send-capable" --scope push
# obask_1_9hQ2…
# id: 3  name: send-capable  scopes: push

./bin/sidecar-admin --db ./sidecar.db key list --region 1
# 3  send-capable       cli            2026-08-28T00:40:02Z  —  —  —  push
# 2  manual test key    cli            2026-08-28T00:39:13Z  —  —  —  —
# 1  obacloud rails1    principal:1    2026-08-28T00:39:05Z  —  —  —  push
```

Add a sentence after "`--scope push` ..." context, immediately after that block:

```
`--scope` is repeatable and `push` is the only scope; an unknown name is refused
before anything is written, and the admin API's `POST …/api_keys` answers `400`
for the same reason. `key list` prints the scopes in its last column.
```

- [ ] **Step 10: README — OBACloud integration**

Replace the paragraph body of "### OBACloud integration" (from `OBACloud (the Rails app` to the closing spec link sentence) with:

```
OBACloud (the Rails app behind onebusawaycloud.com) is the intended consumer
of the bearer credentials above: it re-plumbs its own server-rendered admin
pages to read and write the sidecar instead of its own Postgres tables,
region by region, then flips each region's `sidecar_base_url` to a Go host.
The Rails side holds **one service principal per sidecar deployment it talks
to** and mints **one push-scoped region API key per migrated region** on
demand, storing the key alongside the region row; it keeps its own push
wizard, copywriter, and scheduling, and sends through `POST …/pushes` with
its own `messages`. Un-migrated regions keep the Rails-hosted default
`sidecar_base_url` and need no principal. That client, its provisioning
triggers, its rotation and bulk-reprovisioning rake tasks, the cutover
task, and its error mapping from sidecar status codes to Rails-side behavior
are a contract this repository documents but does not implement -- see
[the design spec, §7](docs/superpowers/specs/2026-08-26-region-api-keys-and-admin-api-design.md#7-obacloud-contract-documented-built-later)
for the sidecar-side contract and OBACloud's own
`docs/superpowers/specs/2026-08-28-sidecar-migration-design.md` for the
cutover plan.
```

- [ ] **Step 11: README — Migrating a region from OBACloud**

Replace the paragraph under "#### Migrating a region from OBACloud" with:

```
`sidecar-admin import --file <export.json>` loads one region's content and
rider state from an export document (`internal/export`, format
`sidecar-export/1`): alerts with their translations, studies, surveys and
questions, survey responses, push registrations, and ghost bus reports
with their enrichment snapshots. OBACloud produces the document with
`bin/rails "sidecar:export[<region id>,<path>]"`. `--file -` reads it from
stdin, so the cutover pipes it straight over `render ssh` with no file
transfer step:

```sh
cat export.json | render ssh sidecar -- sidecar-admin --db /data/sidecar.db import --file -
```

Ids are preserved -- the feed's `Alert_<id>` entity ids and the survey and
question ids the apps persist locally must not change under riders -- and
every row is checked with the domain packages' own rules (enum names,
question content, answer shape, time windows; not the HTTP layer's length
caps) before anything is written, so a bad row rejects the whole document.
Rows that already exist under this region (same id, public id, or
region+token) are skipped and counted, which makes the migration two runs
of the same command: a bulk import the day before the region's
`sidecar_base_url` flips, and a delta from a fresh export right after --
translations added to an already migrated alert land on the delta run. An
id that already belongs to another region's content (alerts, studies,
surveys, and questions share one id sequence) is an error naming the row,
never a silent skip. `--dry-run` runs the same checks, including that the
region exists, without writing. The region itself must already be present
(`sidecar-admin region sync`); alarms and Live Activities are deliberately
not part of the document -- OBACloud keeps firing the ones it owns until
they expire.

**Id headroom.** Once the first region has migrated, this database mints
alert, study, survey, and question ids from that region's maximum upward
while OBACloud keeps minting for un-migrated regions from its own
sequences, so a later region's export can collide with content authored
here in between. Before the first cutover, run

```sh
sidecar-admin --db /data/sidecar.db sequence bump --min 1000000
sidecar-admin --db /data/sidecar.db sequence show
```

once per deployment: `bump` raises the `alerts`, `studies`, `surveys`, and
`survey_questions` sequences to at least the floor (re-running with the same
or a lower floor changes nothing), and `show` prints each current value so
the cutover runbook can verify the headroom is still there. 1,000,000 is
comfortably above any OBACloud id and its growth during the migration.
```

- [ ] **Step 12: README — production host notes (Render / Staging)**

In the "#### Staging" paragraph, after the sentence ending `and production devices never do.`, insert:

```
Production runs the same Blueprint (`render.yaml`) behind the custom domain
`sidecar2.onebusaway.org`, Cloudflare-proxied, with `SIDECAR_TRUSTED_PROXY=cloudflare`
and the Transform Rule secret so per-IP throttles key on `CF-Connecting-IP`,
and with `SIDECAR_REGIONS_URL` pointed at the directory's **`regions-v3.json`**
so experimental regions are addressable. `sidecar.onebusaway.org` keeps
resolving to OBACloud's Rails app until the last region has migrated and
drained; only then does it become a second custom domain here.
```

- [ ] **Step 13: Proofread and commit**

Run: `git diff --stat README.md docs/superpowers/specs/2026-08-26-region-api-keys-and-admin-api-design.md` and read the diff once for broken Markdown (unbalanced fences, table pipes).

```bash
git add README.md docs/superpowers/specs/2026-08-26-region-api-keys-and-admin-api-design.md
git commit -m "docs: amend keys spec and README for push scopes, messages, stale, stdin import, sequence headroom"
```

---

### Task 10: Full verification

**Files:** none new.

- [ ] **Step 1: Run everything CI runs**

Run: `make check`
Expected: every target green (`fmt-check vet lint generate-check test test-tz test-race web-check`). Fix any lint finding in the files this plan touched (doc comments on every exported identifier added: `Scope`, `ScopePush`, `ErrUnknownScope`, `Scopes`, `ParseScopes`, `Has`, `Strings`, `ErrInvalidMessages`, `ValidateMessages`, `TranslationStale`, `ErrUnknownQuestion`, `SequenceTables`, `Sequences`, `BumpSequences`).

- [ ] **Step 2: Mutation spot-checks (one each, revert after)**

- `apikey.ParseScopes`: return `out, nil` for unknown names → `TestParseScopes/unknown` and `TestAPIKeys_ScopesValidation` fail.
- `principalSet.admits`: drop the scope check → `TestAdminPushes_PushScopedKeyCanSendAndCancel` fails.
- `alertpush.Enqueue`: always call `BuildMessages` → `TestAdminCreatePush_CustomMessages` fails.
- `groupTranslations`: never set `Stale` → `TestAdminAlerts_TranslationStaleFlag` fails.
- `insertQuestions`: ignore `qd.ID` → `UpdatePreservesNamedQuestionIDs` fails.
- `BumpSequences`: skip the UPDATE branch → `TestBumpSequences` fails.

- [ ] **Step 3: Commit anything the checks changed**

```bash
git status --short
git add -A && git commit -m "phase 0: make check fixes" # only if there is anything to commit
```

---

### Task 11: Runbook — production host and operator commands (§2.1, §2.6)

Operational, not code. Nothing here is committed; it is the sequence the operator runs once the tasks above are deployed to the `sidecar` image tag `render.yaml` points at.

- [ ] **Step 1: Deploy production**

1. Apply `render.yaml` as a Blueprint (Render Dashboard → Blueprints → New, or `render blueprint launch`), production workspace, service `sidecar` + `gorush`, disk `sidecar-data` at `/data`.
2. Enter the `sync: false` secrets: `SIDECAR_APNS_TOPIC`, `SIDECAR_OBA_API_KEY`, `SIDECAR_PIRATE_WEATHER_KEY`, Stripe keys, `SIDECAR_SENTRY_DSN`, `SIDECAR_TRUSTED_PROXY_SECRET`, Litestream `SIDECAR_BACKUP_*`, and gorush's APNs `.p8` values; then the two hand-derived gorush settings the Blueprint comments describe (`GORUSH_CORE_FEEDBACK_HOOK_URL`, `GORUSH_CORE_FEEDBACK_HEADER`).
3. Set `SIDECAR_REGIONS_URL=https://regions.onebusaway.org/regions-v3.json` on the `sidecar` service (the binary's default is `regions-v3.json` already; set it explicitly so a later default change cannot move production).
4. Add custom domain `sidecar2.onebusaway.org` to the `sidecar` service; in Cloudflare create the proxied CNAME to the `*.onrender.com` host, the Transform Rule that adds the `SIDECAR_TRUSTED_PROXY_SECRET` header (README, Deployment), and the cache rule for `/api/v1/regions/*/alerts*` (README, Feed caching). `SIDECAR_TRUSTED_PROXY=cloudflare` is already in the Blueprint.
5. `deploy/smoke.sh https://sidecar2.onebusaway.org` → `/healthz` 200, `/admin` served, alerts feed for a known region 200.
6. Repeat 1–5 for staging with `render.staging.yaml` and the staging custom domain; point its `SIDECAR_REGIONS_URL` at the hand-maintained staging directory.

- [ ] **Step 2: Bootstrap the database on each host**

```sh
render ssh sidecar -- sidecar-admin --db /data/sidecar.db region sync
render ssh sidecar -- sidecar-admin --db /data/sidecar.db region list
render ssh sidecar -- sidecar-admin --db /data/sidecar.db user create --username <operator>   # first SPA operator
```

- [ ] **Step 3: Mint the service principal per deployment (§2.1)**

```sh
render ssh sidecar -- sidecar-admin --db /data/sidecar.db principal create --name obacloud-production
# obasp_…            <- paste into OBACloud credentials: sidecar.principals["https://sidecar2.onebusaway.org"]
# id: 1  name: obacloud-production

render ssh sidecar-staging -- sidecar-admin --db /data/sidecar.db principal create --name obacloud-staging
# obasp_…            <- OBACloud staging credentials: sidecar.principals["https://<staging host>"]
```

The raw key is printed once; it is never recoverable from the database.

- [ ] **Step 4: Id-sequence headroom before the first cutover (§2.6)**

```sh
render ssh sidecar -- sidecar-admin --db /data/sidecar.db sequence bump --min 1000000
# alerts: 0 -> 1000000
# studies: 0 -> 1000000
# surveys: 0 -> 1000000
# survey_questions: 0 -> 1000000
render ssh sidecar -- sidecar-admin --db /data/sidecar.db sequence show
```

Record the `show` output in the cutover ticket; the Phase 3 cutover task re-checks it.

- [ ] **Step 5: Prove the OBACloud contract end to end from a shell**

```sh
P=obasp_…   # the production principal
curl -s -X POST https://sidecar2.onebusaway.org/api/admin/v1/regions/<id>/api_keys \
  -H "Authorization: Bearer $P" -H 'Content-Type: application/json' \
  -d '{"name":"runbook check","scopes":["push"]}'
# expect 201 with "scopes":["push"]; note the id, then revoke it:
curl -s -X DELETE https://sidecar2.onebusaway.org/api/admin/v1/regions/<id>/api_keys/<keyId> -H "Authorization: Bearer $P"
# expect 204
```

Rehearse `rake sidecar:export` → `cat export.json | render ssh sidecar-staging -- sidecar-admin --db /data/sidecar.db import --file - --dry-run` on staging before any production cutover.

---

## Self-review

**Spec coverage (migration design §0.2, §2.1–2.8):**
- §0.2 both amendments → Task 9 Steps 1, 2, 6 (spec) and Steps 9, 10 (README). ✓
- §2.1 production host → Task 11 (operational) + Task 9 Step 12 (README note). ✓
- §2.2 `scopes` column, `createKeyRequest.scopes`, unknown → 400, "field took effect" test, `GET …/api_keys`, `key list`, `key create --scope push`, `operatorOrPushKey`, route-table test "those two routes and only those" → Tasks 1, 2, 3. ✓
- §2.3 optional `messages` validated with the derivation's caps, stored as snapshot → Task 4. ✓
- §2.4 `stale` per translation from `SourceSHA256`, feed unchanged → Task 5. ✓
- §2.5 `import --file -` → Task 6. ✓
- §2.6 `sequence bump --min N`, `sequence show` → Task 7; runbook Task 11 Step 4. ✓
- §2.7 question ids preserved on PUT with a test; ids assigned for id-less questions → Task 8. ✓
- §2.8 confirmed-present items need no work; `GET /regions/{id}` `features`, `?since`, idempotent import are untouched. ✓
- Task (j) `render.yaml` custom-domain / regions-v3 notes are documentation only → Task 9 Step 12 and Task 11 Step 1. ✓

**Placeholder scan:** no TBD/TODO; every code step has code; every test step has the test. The `QuestionsEqual` rule (Task 8) and the sqlite_sequence "row absent vs. seq 0" distinction (Task 7) are both stated in the code itself, not left to the executor.

**Type consistency:**
- `apikey.Scopes` / `ParseScopes` / `ScopePush` named identically in Tasks 1–3 and 9. ✓
- `CreateRegionKey(ctx, regionID, name, keyHash, scopes, by, now)` order used in Task 1 (store, storetest, CLI), Task 2 (handler), Task 3 (test helper). ✓
- `principalPushKey`, `principalSet.admits`, `principal.scopes`, `operatorOrPushKey` consistent between Task 3 code and tests. ✓
- `Enqueue(ctx, alertID, audience, messages, now)` used in Task 4 handler, CLI, and tests. ✓
- `translationJSON.Stale` / `groupTranslations(a alerts.Alert)` / `Alert.TranslationStale` consistent in Task 5. ✓
- `runImport(ctx, stdin, stdout, store, now, args)` in Task 6 matches the dispatch edit. ✓
- `sqlite.SequenceTables`, `Sequences`, `BumpSequences(ctx, min) (before map, error)` consistent between Task 7 store, tests, and CLI. ✓
- `surveys.QuestionDefinition.ID *int64`, `ErrUnknownQuestion`, `InsertQuestionWithID` consistent across Task 8 domain, store, handler, tests. ✓
