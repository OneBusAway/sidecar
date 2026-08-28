# Region API Keys and the Region-Scoped Admin API — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give the sidecar admin API a per-region bearer credential OBACloud can hold, move every region-scoped admin route under a `/regions/{regionId}/` path segment, and complete the authoring surface (studies, surveys, responses, ghost bus reports, alarms, push counts, API keys) so a server-side consumer can drive it.

**Architecture:** A new `internal/apikey` domain package holds region keys (`obask_…`) and service principals (`obasp_…`) behind a `Repository`, stored hash-only in SQLite (migration `00010`). `internal/httpapi`'s `requireSession` becomes `requirePrincipal`, which yields a `principal` of one of three kinds (operator cookie, region key, service principal); each row of the `adminRoutes` table declares which kinds it accepts and which region-scoping middleware wraps it. One middleware (`requireRegion`, or `requireKeyAdminRegion` for the key family) parses `{regionId}`, checks tenancy, loads the region, and puts it in the request context; handlers never parse a region themselves, and every resource lookup goes through a loader or a region-scoped repository method. New route families reuse the existing nil-means-absent `Deps` convention.

**Tech Stack:** Go 1.26 (stdlib `net/http` ServeMux, `log/slog`, `crypto/rand`, `crypto/sha256`), sqlc 1.31.1 + goose over modernc SQLite, SvelteKit 2 + Svelte 5 with adapter-static for the admin SPA, vitest for SPA unit tests.

**Spec:** `docs/superpowers/specs/2026-08-26-region-api-keys-and-admin-api-design.md`

## Global Constraints

Every task's requirements implicitly include this section. Values are copied verbatim from the spec and from `CLAUDE.md`.

- **No `time.Now` / `time.Local` outside `cmd/` and `_test.go`.** Inject `now func() time.Time` or pass `now time.Time`. This includes `internal/store/storetest`, which is not a test file — derive instants from its fixed `base`.
- **Every timestamp column is `INTEGER` epoch seconds**, never `DATETIME`/`TEXT`. Use the `unixToTime` / `nullUnixToTime` / `timeToNullUnix` helpers in `internal/store/sqlite/store.go`.
- **Every API instant in JSON is RFC 3339 with an explicit UTC offset.** Naive datetimes are rejected, never interpreted in server-local time.
- **`queries/*.sql` comments must be ASCII-only.** sqlc renumbers `sqlc.arg()` by byte offset into the statement text; a multi-byte rune (`§`, an em dash) in a preceding comment shifts the offsets and emits garbage SQL. Cite the design spec as `spec section N`.
- **Never write `sqlc.arg(x) IN ('a','b')`.** sqlc 1.31.1 does not extract a parameter inside an `IN (...)` list; it compiles, diffs clean, and hands the literal text `sqlc.arg(x)` to the driver. Spell such guards out as OR comparisons.
- **Never mix `sqlc.arg()` with a bare `?` in one query.** Run `make generate` after touching any `.sql` file and commit `internal/store/sqlite/gen/`.
- **Rate limiters key on the TCP peer address** (`clientIP`), deliberately ignoring `X-Forwarded-For`.
- **`revive` enforces doc comments on every exported identifier and package.** `nolint` directives need a specific linter and an explanation.
- **Rider-sourced CSV cells go through the formula-injection guard**: a leading apostrophe for a cell starting with `=`, `+`, `-`, `@`, `\t`, or `\r`.
- **Key format.** Region key: `obask_<regionID>_<43 base64url chars>` from 32 `crypto/rand` bytes. Service principal: `obasp_<43 base64url chars>`. Only the hex SHA-256 of the raw key is stored. The raw key is printed once by the CLI or returned once by the mint route and is **never** written to a log line at any level.
- **`ParsePrefix` parsing is pinned**: `strings.Cut` on the FIRST `_` for the kind, then a second `strings.Cut` for the region id, remainder opaque. Never `strings.Split`, never a cut on the last `_`. The base64url alphabet contains `_`, so about half of all random segments contain one.
- **The region segment regex is `^(0|[1-9][0-9]*)$`**, then `strconv.ParseInt(s, 10, 64)`. Anything else is **404**, not 400 — an unparseable region is "no such region", and the code must not differ between "malformed", "not yours", and "does not exist".
- **Region 0 is a real region (Tampa Bay).** Nothing may use 0 as an "absent region" sentinel.
- **Status codes.** Moved alert and region routes keep their existing codes (validation failures are **400**). New families: **400** for malformed JSON, an oversized body, an unparseable path id or query parameter; **422** for a well-formed body that fails domain validation. **403** `{"error":"forbidden"}` for a principal kind a route does not allow — distinct from the cross-site guard's `{"error":"cross-site request rejected"}`.
- **Existing shipped-client status codes are contracts**: weather failures 403, ghost bus duplicates/validation 422, vehicle upstream failure 502. Check README before changing any response code.
- **`regions.Region.Active` is not consulted for admin access.** An inactive region stays authorable.
- **Tests must be able to fail.** After writing a test, mutate the code under test and confirm the assertion fires. Timezone-dependent assertions must hold under both `make test-tz` zones (`UTC` and `Asia/Kathmandu`).
- **Frontend checks:** `npm run check`, `npm run lint`, `npm run test:unit` in `web/admin`.
- **Go tests need the SPA built once** (`make web`) before `go test ./...` passes in `internal/httpapi/adminui`.

---

## File Structure

**New Go packages and files**

| Path | Responsibility |
|---|---|
| `internal/apikey/apikey.go` | `Actor`, `RegionKey`, `ServicePrincipal`, `Repository`, `ErrNotFound`, `ErrRevoked`, `LogValue` methods |
| `internal/apikey/key.go` | `Kind`, `NewRegionKey`, `NewPrincipalKey`, `Hash`, `ParsePrefix` |
| `internal/apikey/key_test.go`, `internal/apikey/apikey_test.go` | pure unit tests |
| `internal/store/sqlite/migrations/00010_api_keys.sql` | the two tables and their indexes |
| `internal/store/sqlite/queries/apikeys.sql` | sqlc queries |
| `internal/store/sqlite/apikeys.go` | the `apikey.Repository` adapter |
| `internal/store/storetest/apikeytest.go` | `RunAPIKeyRepository` conformance suite |
| `internal/csvsafe/csvsafe.go` | `Cell` (formula-injection guard) and `Float`, shared by the two CSV writers now moving into domain packages |
| `internal/surveys/csv.go` | `WriteResponsesCSV(w, survey, responses)` |
| `internal/surveys/codec.go` | `InstantParser`, `DefinitionFromDocument` |
| `internal/ghostbus/csv.go` | `WriteReportsCSV(w, region, reports)` and its cell helpers |
| `internal/httpapi/principal.go` | `principalKind`, `principalSet`, `principal`, `canAccessRegion`, `actor`, `LogValue`, `principalFrom` |
| `internal/httpapi/bearer.go` | the `Authorization: Bearer` authentication path |
| `internal/httpapi/region_scope.go` | `routeScope`, `requireRegion`, `requireKeyAdminRegion`, `regionFrom`, and every `load*` loader |
| `internal/httpapi/admin_surveys.go` | studies + surveys admin handlers and their wire shapes |
| `internal/httpapi/admin_responses.go` | survey response JSON + CSV handlers |
| `internal/httpapi/admin_ghostbus.go` | ghost bus report JSON + CSV handlers |
| `internal/httpapi/admin_alarms.go` | read-only alarm handlers |
| `internal/httpapi/admin_pushregs.go` | push registration count handler |
| `internal/httpapi/admin_apikeys.go` | mint / list / revoke handlers |
| `cmd/sidecar-admin/keys.go` | `key` and `principal` CLI commands |

**Modified**

| Path | Change |
|---|---|
| `internal/httpapi/router.go` | `Deps.APIKeys`, `Deps.BearerFailLimiter`, `adminRoute` gains `allowed` + `scope`, the whole `adminRoutes` table moves under the region segment and grows, `registerAdminRoutes` composes the new middleware, `adminFeatures` |
| `internal/httpapi/middleware.go` | `requireSession` → `requirePrincipal`; context keys become a block |
| `internal/httpapi/json.go` | `decodeJSONStrict` |
| `internal/httpapi/session.go` | `whoami` reads the principal |
| `internal/httpapi/admin_alerts.go` | region comes from the context; `region_id` in the create body is a 400; `loadAlert` replaces bare `pathID` lookups |
| `internal/httpapi/admin_regions.go` | new `get` handler with `features`; `patch` uses the context region |
| `internal/httpapi/admin_pushes.go` | `loadAlert` + an explicit `push.RegionID` assertion |
| `internal/alarms/alarms.go` | `ListByRegion`, `GetInRegion` |
| `internal/ghostbus/ghostbus.go` | `GetByPublicID` |
| `internal/surveys/surveys.go` | `UpdateStudy`, `CreateSurveyInRegion`, `GetResponseInRegion` |
| `internal/store/sqlite/{alarms,ghostbus,surveys}.go` + their `queries/*.sql` | the new region-scoped methods |
| `internal/store/storetest/{alarmtest,ghostbustest,surveytest}.go` | conformance coverage for those methods |
| `cmd/sidecar/main.go` | `Deps.APIKeys = store.APIKeys()` |
| `cmd/sidecar-admin/commands.go` | `key` and `principal` dispatch |
| `cmd/sidecar-admin/surveys.go`, `cmd/sidecar-admin/ghostbus.go` | call the moved codec and CSV writers |
| `web/admin/src/**` | region-scoped SPA routes, region picker, `features`-driven push card |
| `README.md` | region API keys, the new route list, the SPA URL change, the proxy note |

---

## Task 1: The `apikey` domain package

**Files:**
- Create: `internal/apikey/apikey.go`
- Create: `internal/apikey/key.go`
- Test: `internal/apikey/apikey_test.go`
- Test: `internal/apikey/key_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `apikey.Kind` (`string`), constants `apikey.KindRegion = "region"`, `apikey.KindPrincipal = "principal"`
  - `apikey.RegionPrefix = "obask"`, `apikey.PrincipalPrefix = "obasp"`, `apikey.MaxRawLen = 128`
  - `apikey.Actor{Kind string; ID int64}` with `apikey.ActorOperator/ActorPrincipal/ActorCLI` string constants
  - `apikey.RegionKey`, `apikey.ServicePrincipal` (fields exactly as spec section 3.2)
  - `apikey.Repository` interface (spec section 3.2, reproduced in Step 5 below)
  - `apikey.ErrNotFound`, `apikey.ErrRevoked`
  - `func NewRegionKey(regionID int64) (raw, hash string, err error)`
  - `func NewPrincipalKey() (raw, hash string, err error)`
  - `func Hash(raw string) string`
  - `func ParsePrefix(raw string) (kind Kind, regionID int64, ok bool)`

- [ ] **Step 1: Write the failing `ParsePrefix` test**

Create `internal/apikey/key_test.go`:

```go
package apikey_test

import (
	"strings"
	"testing"

	"github.com/OneBusAway/sidecar/internal/apikey"
)

// awkwardSecret is the fixture the spec (section 3.1, section 8) requires: a
// random segment that contains BOTH '_' and '-'. The base64url alphabet
// includes '_', so about half of all real keys look like this. A
// strings.Split or a cut on the LAST '_' passes every other fixture and
// fails only on this one.
const awkwardSecret = "Qm9-abc_defGHIjklMNOpqrSTUvwxYZ0123456789-_x"

func TestParsePrefix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		raw        string
		wantKind   apikey.Kind
		wantRegion int64
		wantOK     bool
	}{
		{"region key with underscores and dashes in the secret",
			"obask_1_" + awkwardSecret, apikey.KindRegion, 1, true},
		{"region 0 is a real region",
			"obask_0_" + awkwardSecret, apikey.KindRegion, 0, true},
		{"multi digit region", "obask_4082_" + awkwardSecret, apikey.KindRegion, 4082, true},
		{"principal", "obasp_" + awkwardSecret, apikey.KindPrincipal, 0, true},
		{"leading zero region is rejected", "obask_01_" + awkwardSecret, "", 0, false},
		{"negative region is rejected", "obask_-1_" + awkwardSecret, "", 0, false},
		{"plus signed region is rejected", "obask_+1_" + awkwardSecret, "", 0, false},
		{"non numeric region is rejected", "obask_one_" + awkwardSecret, "", 0, false},
		{"region key with no secret is rejected", "obask_1_", "", 0, false},
		{"region key with no region segment is rejected", "obask_" + awkwardSecret, "", 0, false},
		{"principal with no secret is rejected", "obasp_", "", 0, false},
		{"unknown prefix is rejected", "sk_live_" + awkwardSecret, "", 0, false},
		{"no underscore at all is rejected", "obask", "", 0, false},
		{"empty is rejected", "", "", 0, false},
		{"leading space is rejected", " obask_1_" + awkwardSecret, "", 0, false},
		{"region id larger than int64 is rejected",
			"obask_99999999999999999999_" + awkwardSecret, "", 0, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			kind, region, ok := apikey.ParsePrefix(tc.raw)
			if ok != tc.wantOK || kind != tc.wantKind || region != tc.wantRegion {
				t.Errorf("ParsePrefix(%q) = (%q, %d, %v), want (%q, %d, %v)",
					tc.raw, kind, region, ok, tc.wantKind, tc.wantRegion, tc.wantOK)
			}
		})
	}
}

// TestNewRegionKey pins the wire format: the plaintext carries the region id
// so an operator reading a log line or a Rails column can tell which region a
// key belongs to without a database lookup. The hash lookup is still what
// decides (spec section 3.1).
func TestNewRegionKey(t *testing.T) {
	t.Parallel()

	raw, hash, err := apikey.NewRegionKey(7)
	if err != nil {
		t.Fatalf("NewRegionKey: %v", err)
	}
	if !strings.HasPrefix(raw, "obask_7_") {
		t.Errorf("raw = %q, want an obask_7_ prefix", raw)
	}
	if got, want := len(strings.TrimPrefix(raw, "obask_7_")), 43; got != want {
		t.Errorf("secret length = %d, want %d (32 random bytes, raw base64url)", got, want)
	}
	if hash != apikey.Hash(raw) {
		t.Errorf("hash = %q, want Hash(raw) = %q", hash, apikey.Hash(raw))
	}
	if len(hash) != 64 {
		t.Errorf("hash length = %d, want 64 (hex SHA-256)", len(hash))
	}
	kind, region, ok := apikey.ParsePrefix(raw)
	if !ok || kind != apikey.KindRegion || region != 7 {
		t.Errorf("ParsePrefix(minted) = (%q, %d, %v), want (region, 7, true)", kind, region, ok)
	}

	other, _, err := apikey.NewRegionKey(7)
	if err != nil {
		t.Fatalf("NewRegionKey: %v", err)
	}
	if other == raw {
		t.Error("two mints produced the same key")
	}
}

// TestNewPrincipalKey mirrors TestNewRegionKey for the deployment-wide
// credential, which carries no region segment.
func TestNewPrincipalKey(t *testing.T) {
	t.Parallel()

	raw, hash, err := apikey.NewPrincipalKey()
	if err != nil {
		t.Fatalf("NewPrincipalKey: %v", err)
	}
	if !strings.HasPrefix(raw, "obasp_") {
		t.Errorf("raw = %q, want an obasp_ prefix", raw)
	}
	if got, want := len(strings.TrimPrefix(raw, "obasp_")), 43; got != want {
		t.Errorf("secret length = %d, want %d", got, want)
	}
	if hash != apikey.Hash(raw) {
		t.Errorf("hash = %q, want Hash(raw)", hash)
	}
	kind, region, ok := apikey.ParsePrefix(raw)
	if !ok || kind != apikey.KindPrincipal || region != 0 {
		t.Errorf("ParsePrefix(minted) = (%q, %d, %v), want (principal, 0, true)", kind, region, ok)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/apikey/...`
Expected: FAIL — the package does not exist (`no required module provides package .../internal/apikey`).

- [ ] **Step 3: Write `internal/apikey/key.go`**

```go
// Package apikey is the domain for the two credentials a server-side
// consumer can hold against the admin API: a region key, scoped to exactly
// one region, and a service principal, whose only power is to mint, list,
// and revoke region keys.
//
// Nothing here performs I/O beyond crypto/rand and nothing reads the clock;
// storage lives behind Repository and every instant is injected.
package apikey

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Kind distinguishes the two credential families a raw key can name.
type Kind string

const (
	// KindRegion is a per-region key: obask_<regionID>_<secret>.
	KindRegion Kind = "region"
	// KindPrincipal is a deployment-wide provisioning credential: obasp_<secret>.
	KindPrincipal Kind = "principal"
)

const (
	// RegionPrefix is the first segment of a region key's plaintext.
	RegionPrefix = "obask"
	// PrincipalPrefix is the first segment of a service principal's plaintext.
	PrincipalPrefix = "obasp"
	// MaxRawLen bounds a credential the middleware is willing to hash. A real
	// key is 51 bytes at most; the cap exists so an unauthenticated caller
	// cannot make the server SHA-256 a megabyte for free (spec section 4.2).
	MaxRawLen = 128
	// secretBytes is the entropy behind every key. 256 bits is what makes
	// salting and constant-time comparison unnecessary (spec section 2.6).
	secretBytes = 32
)

// regionSegment is the exact grammar of the region id in a region key's
// plaintext: no leading zeros, no sign, no whitespace (spec section 3.1).
// The same grammar governs the {regionId} path segment.
var regionSegment = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)

// newSecret returns 43 characters of raw URL-safe base64 over 32 random bytes.
func newSecret() (string, error) {
	var b [secretBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("apikey: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// NewRegionKey mints a region key and its stored hash. The raw key is
// returned to the caller once and must never be logged or persisted.
func NewRegionKey(regionID int64) (raw, hash string, err error) {
	secret, err := newSecret()
	if err != nil {
		return "", "", err
	}
	raw = RegionPrefix + "_" + strconv.FormatInt(regionID, 10) + "_" + secret
	return raw, Hash(raw), nil
}

// NewPrincipalKey mints a service principal key and its stored hash. Same
// handling rules as NewRegionKey.
func NewPrincipalKey() (raw, hash string, err error) {
	secret, err := newSecret()
	if err != nil {
		return "", "", err
	}
	raw = PrincipalPrefix + "_" + secret
	return raw, Hash(raw), nil
}

// Hash is the stored form of a key: hex SHA-256, unsalted. The input is 256
// bits of crypto/rand output, so there is nothing to salt against and the
// lookup can be a plain index hit (spec section 2.6). This is the same
// posture auth.HashToken takes for session tokens.
func Hash(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// ParsePrefix classifies a raw key by its plaintext prefix, so the caller
// knows which table to look the hash up in without a second query.
//
// The parsing is pinned by the spec (section 3.1) and must not be
// "simplified": the base64url alphabet contains '_', so about half of all
// real secrets contain one. Only a Cut on the FIRST '_' -- twice for a
// region key -- is correct. A strings.Split or a cut on the last '_' passes
// every hand-written fixture and mangles half of production.
//
// The region id in the plaintext is a debugging aid only. The hash lookup
// decides; the caller must still reject a stored row whose region_id
// disagrees with what this returned.
func ParsePrefix(raw string) (kind Kind, regionID int64, ok bool) {
	prefix, rest, found := strings.Cut(raw, "_")
	if !found || rest == "" {
		return "", 0, false
	}
	switch prefix {
	case PrincipalPrefix:
		return KindPrincipal, 0, true
	case RegionPrefix:
		idPart, secret, split := strings.Cut(rest, "_")
		if !split || secret == "" || !regionSegment.MatchString(idPart) {
			return "", 0, false
		}
		id, err := strconv.ParseInt(idPart, 10, 64)
		if err != nil {
			// Only reachable for a digit string too long for int64; the
			// regex has already excluded every other shape.
			return "", 0, false
		}
		return KindRegion, id, true
	default:
		return "", 0, false
	}
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/apikey/...`
Expected: PASS.

- [ ] **Step 5: Write the failing `LogValue` test**

Create `internal/apikey/apikey_test.go`:

```go
package apikey_test

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/apikey"
)

// TestLogValueOmitsHashes is the leak test. A hash is not a live credential,
// but it IS the lookup key: an attacker holding a database backup plus a hash
// from a log line learns nothing new, while an attacker holding only the log
// learns which row to target. regions.Region.LogValue omits the OBA key for
// the same reason; these two types follow it.
func TestLogValueOmitsHashes(t *testing.T) {
	t.Parallel()

	const hash = "c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00"
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	logger.Info("region key",
		"key", apikey.RegionKey{
			ID: 3, RegionID: 1, Name: "obacloud", KeyHash: hash,
			CreatedBy: apikey.Actor{Kind: apikey.ActorPrincipal, ID: 2}, CreatedAt: at,
		})
	logger.Info("principal",
		"principal", apikey.ServicePrincipal{ID: 2, Name: "rails", KeyHash: hash, CreatedAt: at})

	if strings.Contains(buf.String(), hash) {
		t.Errorf("log output contains the key hash:\n%s", buf.String())
	}
	for _, want := range []string{"id=3", "region_id=1", "name=obacloud"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("log output missing %q:\n%s", want, buf.String())
		}
	}
}
```

- [ ] **Step 6: Run it to verify it fails**

Run: `go test ./internal/apikey/... -run TestLogValueOmitsHashes`
Expected: FAIL — `undefined: apikey.RegionKey`.

- [ ] **Step 7: Write `internal/apikey/apikey.go`**

```go
package apikey

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

var (
	// ErrNotFound is returned when no row matches an id or a key hash.
	ErrNotFound = errors.New("api key not found")
	// ErrRevoked is returned by the by-hash lookups when the hash matches a
	// revoked row. It is deliberately distinct from ErrNotFound: a revoked
	// key being replayed is the clearest signal that a credential leaked,
	// and the middleware logs it as reason=revoked (spec section 4.2).
	ErrRevoked = errors.New("api key revoked")
)

// Actor names who minted or revoked a key. It is not a foreign key: a
// deleted operator or a revoked principal must not orphan the audit trail
// (spec section 3).
type Actor struct {
	// Kind is ActorOperator, ActorPrincipal, or ActorCLI.
	Kind string
	// ID is the users.id or service_principals.id, and 0 for the CLI.
	ID int64
}

// The three actor kinds. They are the same strings the CHECK constraints on
// region_api_keys enforce.
const (
	ActorOperator  = "operator"
	ActorPrincipal = "principal"
	ActorCLI       = "cli"
)

// RegionKey is one bearer credential scoped to exactly one region.
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

// LogValue implements slog.LogValuer, omitting KeyHash for the same reason
// regions.Region.LogValue omits its OBA key: the omission makes the leak
// unrepresentable rather than merely unwritten.
func (k RegionKey) LogValue() slog.Value {
	attrs := []slog.Attr{
		slog.Int64("id", k.ID),
		slog.Int64("region_id", k.RegionID),
		slog.String("name", k.Name),
		slog.String("created_by_kind", k.CreatedBy.Kind),
		slog.Int64("created_by_id", k.CreatedBy.ID),
		slog.Bool("revoked", k.RevokedAt != nil),
	}
	if k.RevokedBy != nil {
		attrs = append(attrs,
			slog.String("revoked_by_kind", k.RevokedBy.Kind),
			slog.Int64("revoked_by_id", k.RevokedBy.ID))
	}
	return slog.GroupValue(attrs...)
}

// ServicePrincipal is the deployment-wide credential that may mint, list,
// and revoke region keys, and nothing else.
type ServicePrincipal struct {
	ID         int64
	Name       string
	KeyHash    string
	CreatedAt  time.Time
	LastUsedAt *time.Time
	RevokedAt  *time.Time
}

// LogValue implements slog.LogValuer, omitting KeyHash. See RegionKey.LogValue.
func (p ServicePrincipal) LogValue() slog.Value {
	return slog.GroupValue(
		slog.Int64("id", p.ID),
		slog.String("name", p.Name),
		slog.Bool("revoked", p.RevokedAt != nil),
	)
}

// Repository stores region keys and service principals. Implementations must
// be safe for concurrent use. Revoked rows are kept rather than deleted, so
// `key list` shows history and a revoked hash can never be re-minted by
// accident.
type Repository interface {
	CreateRegionKey(ctx context.Context, regionID int64, name, keyHash string, by Actor, now time.Time) (RegionKey, error)
	// GetRegionKeyByHash returns ErrNotFound for unknown hashes and
	// ErrRevoked for a hash that matches a revoked row, so the caller can
	// log a replay distinctly.
	GetRegionKeyByHash(ctx context.Context, keyHash string) (RegionKey, error)
	// ListRegionKeys returns live and revoked keys, newest first.
	ListRegionKeys(ctx context.Context, regionID int64) ([]RegionKey, error)
	// ListRegionKeysByCreator returns every key an actor minted, across
	// regions, newest first. Actor{Kind: ActorCLI} matches created_by_id IS
	// NULL; an implementation must never spell that as a bare "= ?", which
	// silently matches nothing.
	ListRegionKeysByCreator(ctx context.Context, by Actor) ([]RegionKey, error)
	// RevokeRegionKey is region-scoped: a key in another region is
	// ErrNotFound. An already-revoked key is a no-op success.
	RevokeRegionKey(ctx context.Context, regionID, id int64, by Actor, now time.Time) error
	// RevokeRegionKeysByCreator revokes every live key the actor minted, in
	// one transaction, and returns their ids ascending.
	RevokeRegionKeysByCreator(ctx context.Context, minted, by Actor, now time.Time) ([]int64, error)
	// TouchRegionKey records use. Callers touch at most hourly (spec
	// section 4.2), so last_used_at may be up to an hour stale.
	TouchRegionKey(ctx context.Context, id int64, now time.Time) error

	CreatePrincipal(ctx context.Context, name, keyHash string, now time.Time) (ServicePrincipal, error)
	// GetPrincipalByHash returns ErrNotFound or ErrRevoked, as
	// GetRegionKeyByHash does.
	GetPrincipalByHash(ctx context.Context, keyHash string) (ServicePrincipal, error)
	ListPrincipals(ctx context.Context) ([]ServicePrincipal, error)
	// RevokePrincipal is a no-op success for an already-revoked principal
	// and ErrNotFound for an unknown id.
	RevokePrincipal(ctx context.Context, id int64, now time.Time) error
	TouchPrincipal(ctx context.Context, id int64, now time.Time) error
}
```

- [ ] **Step 8: Run the tests to verify they pass**

Run: `go test ./internal/apikey/... && go vet ./internal/apikey/... && golangci-lint run internal/apikey/...`
Expected: PASS, no vet or lint output.

- [ ] **Step 9: Prove the ParsePrefix test can fail**

Temporarily replace `strings.Cut(rest, "_")` with `strings.LastIndex`-based splitting (or `strings.Split(raw, "_")` indexing), re-run `go test ./internal/apikey/...`, confirm the "region key with underscores and dashes in the secret" case fails, then revert.

- [ ] **Step 10: Commit**

```bash
git add internal/apikey
git commit -m "feat(apikey): region key and service principal domain types"
```

---

## Task 2: Migration, sqlc queries, SQLite adapter, and the storetest suite

**Files:**
- Create: `internal/store/sqlite/migrations/00010_api_keys.sql`
- Create: `internal/store/sqlite/queries/apikeys.sql`
- Create: `internal/store/sqlite/apikeys.go`
- Create: `internal/store/storetest/apikeytest.go`
- Modify: `internal/store/sqlite/store.go` (add `APIKeys()`; extend the package doc comment)
- Test: `internal/store/sqlite/store_test.go` (hook the suite up)

**Interfaces:**
- Consumes: everything Task 1 produced.
- Produces:
  - `func (s *Store) APIKeys() apikey.Repository`
  - `func storetest.RunAPIKeyRepository(t *testing.T, newStore func(*testing.T) (apikey.Repository, regions.Repository))`

- [ ] **Step 1: Write the migration**

Create `internal/store/sqlite/migrations/00010_api_keys.sql`:

```sql
-- +goose Up
-- Region API keys and service principals (design spec section 3).
--
-- Both tables store only the hex SHA-256 of the raw key, the same posture
-- sessions take. A stolen backup therefore yields no usable credential --
-- though it still yields every plaintext secret the sidecar already keeps
-- (region OBA keys, push tokens), so this narrows that threat rather than
-- closing it.
CREATE TABLE service_principals (
    id           INTEGER PRIMARY KEY,
    name         TEXT    NOT NULL,
    key_hash     TEXT    NOT NULL UNIQUE,
    created_at   INTEGER NOT NULL,
    last_used_at INTEGER,
    revoked_at   INTEGER
);

-- Revoked rows are kept, never deleted: `key list` shows history, and a
-- revoked key's hash can never be re-minted by accident. created_by_* and
-- revoked_by_* are deliberately NOT foreign keys -- a deleted operator or a
-- revoked principal must not orphan the audit trail.
CREATE TABLE region_api_keys (
    id              INTEGER PRIMARY KEY,
    region_id       INTEGER NOT NULL REFERENCES regions(id) ON DELETE CASCADE,
    name            TEXT    NOT NULL,
    key_hash        TEXT    NOT NULL UNIQUE,
    created_by_kind TEXT    NOT NULL CHECK (created_by_kind IN ('operator', 'principal', 'cli')),
    created_by_id   INTEGER,
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

-- +goose Down
DROP INDEX region_api_keys_creator;
DROP INDEX region_api_keys_region;
DROP TABLE region_api_keys;
DROP TABLE service_principals;
```

- [ ] **Step 2: Write the queries**

Create `internal/store/sqlite/queries/apikeys.sql`:

```sql
-- Comments in this file must stay ASCII-only. sqlc renumbers sqlc.arg() by
-- byte offset into each statement's text, and a multi-byte rune anywhere in
-- a preceding comment shifts those offsets, emitting garbage SQL. Cite the
-- design spec as "spec section N", not with the section sign.

-- name: CreateRegionAPIKey :one
INSERT INTO region_api_keys (
  region_id, name, key_hash, created_by_kind, created_by_id, created_at
) VALUES (
  sqlc.arg(region_id), sqlc.arg(name), sqlc.arg(key_hash),
  sqlc.arg(created_by_kind), sqlc.arg(created_by_id), sqlc.arg(created_at)
)
RETURNING *;

-- name: GetRegionAPIKeyByHash :one
SELECT * FROM region_api_keys WHERE key_hash = sqlc.arg(key_hash);

-- name: GetRegionAPIKey :one
SELECT * FROM region_api_keys
WHERE id = sqlc.arg(id) AND region_id = sqlc.arg(region_id);

-- name: ListRegionAPIKeys :many
SELECT * FROM region_api_keys
WHERE region_id = sqlc.arg(region_id)
ORDER BY id DESC;

-- name: ListRegionAPIKeysByCreator :many
-- The CLI case has created_by_id IS NULL and lives in its own query below.
-- "created_by_id = ?" with a NULL bind matches no row in SQL, so folding the
-- two cases into one statement would silently return nothing for the CLI --
-- the failure mode the design spec calls out.
SELECT * FROM region_api_keys
WHERE created_by_kind = sqlc.arg(created_by_kind)
  AND created_by_id = sqlc.arg(created_by_id)
ORDER BY id DESC;

-- name: ListRegionAPIKeysByCLI :many
SELECT * FROM region_api_keys
WHERE created_by_kind = 'cli' AND created_by_id IS NULL
ORDER BY id DESC;

-- name: RevokeRegionAPIKey :execrows
UPDATE region_api_keys SET
  revoked_at      = sqlc.arg(revoked_at),
  revoked_by_kind = sqlc.arg(revoked_by_kind),
  revoked_by_id   = sqlc.arg(revoked_by_id)
WHERE id = sqlc.arg(id) AND region_id = sqlc.arg(region_id) AND revoked_at IS NULL;

-- name: RevokeRegionAPIKeysByCreator :many
UPDATE region_api_keys SET
  revoked_at      = sqlc.arg(revoked_at),
  revoked_by_kind = sqlc.arg(revoked_by_kind),
  revoked_by_id   = sqlc.arg(revoked_by_id)
WHERE created_by_kind = sqlc.arg(created_by_kind)
  AND created_by_id = sqlc.arg(created_by_id)
  AND revoked_at IS NULL
RETURNING id;

-- name: RevokeRegionAPIKeysByCLI :many
UPDATE region_api_keys SET
  revoked_at      = sqlc.arg(revoked_at),
  revoked_by_kind = sqlc.arg(revoked_by_kind),
  revoked_by_id   = sqlc.arg(revoked_by_id)
WHERE created_by_kind = 'cli' AND created_by_id IS NULL AND revoked_at IS NULL
RETURNING id;

-- name: TouchRegionAPIKey :exec
UPDATE region_api_keys SET last_used_at = sqlc.arg(last_used_at) WHERE id = sqlc.arg(id);

-- name: CreateServicePrincipal :one
INSERT INTO service_principals (name, key_hash, created_at)
VALUES (sqlc.arg(name), sqlc.arg(key_hash), sqlc.arg(created_at))
RETURNING *;

-- name: GetServicePrincipalByHash :one
SELECT * FROM service_principals WHERE key_hash = sqlc.arg(key_hash);

-- name: GetServicePrincipal :one
SELECT * FROM service_principals WHERE id = sqlc.arg(id);

-- name: ListServicePrincipals :many
SELECT * FROM service_principals ORDER BY id DESC;

-- name: RevokeServicePrincipal :execrows
UPDATE service_principals SET revoked_at = sqlc.arg(revoked_at)
WHERE id = sqlc.arg(id) AND revoked_at IS NULL;

-- name: TouchServicePrincipal :exec
UPDATE service_principals SET last_used_at = sqlc.arg(last_used_at) WHERE id = sqlc.arg(id);
```

- [ ] **Step 3: Generate and confirm the generated code compiles**

Run: `make generate && git status --short internal/store/sqlite/gen`
Expected: new/changed files under `internal/store/sqlite/gen/`. Then `go build ./...` — expect success (nothing consumes them yet).

- [ ] **Step 4: Write the failing conformance suite**

Create `internal/store/storetest/apikeytest.go`. It is a normal (non-test) file, so the `time.Now` ban applies: derive everything from `base`.

```go
package storetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/apikey"
	"github.com/OneBusAway/sidecar/internal/regions"
)

// newAPIKeyStoreFunc returns a fresh pair of repositories over one store.
// The regions repository is required because the cascade case needs a region
// to delete.
type newAPIKeyStoreFunc func(*testing.T) (apikey.Repository, regions.Repository)

// RunAPIKeyRepository exercises an apikey.Repository against the behavioral
// contract every engine must satisfy (design spec section 8).
func RunAPIKeyRepository(t *testing.T, newStore newAPIKeyStoreFunc) {
	t.Helper()

	t.Run("CreateGetRoundTrip", func(t *testing.T) { testAPIKeyRoundTrip(t, newStore) })
	t.Run("RevokedHashIsDistinctFromUnknown", func(t *testing.T) { testAPIKeyRevokedHash(t, newStore) })
	t.Run("RevokeIsRegionScoped", func(t *testing.T) { testAPIKeyRevokeRegionScoped(t, newStore) })
	t.Run("RevokeTwiceSucceeds", func(t *testing.T) { testAPIKeyRevokeIdempotent(t, newStore) })
	t.Run("ListByCreatorCoversAllThreeKinds", func(t *testing.T) { testAPIKeyListByCreator(t, newStore) })
	t.Run("RevokeByCreatorIsAtomic", func(t *testing.T) { testAPIKeyRevokeByCreator(t, newStore) })
	t.Run("TouchRecordsUse", func(t *testing.T) { testAPIKeyTouch(t, newStore) })
	t.Run("ListIsNewestFirst", func(t *testing.T) { testAPIKeyListOrder(t, newStore) })
	t.Run("KeysCascadeOnRegionDelete", func(t *testing.T) { testAPIKeyCascade(t, newStore) })
	t.Run("PrincipalLifecycle", func(t *testing.T) { testPrincipalLifecycle(t, newStore) })
}

// seedAPIKeyRegions upserts the two regions every subtest below uses. Region
// 0 is deliberately one of them: it is a real region, so a repository that
// treats 0 as "no region" fails here.
func seedAPIKeyRegions(t *testing.T, repo regions.Repository) {
	t.Helper()
	err := repo.UpsertFromDirectory(context.Background(), []regions.Region{
		{ID: 0, Name: "Tampa Bay", OBABaseURL: "https://tampa.example/", Language: "en", Active: true},
		{ID: 1, Name: "Puget Sound", OBABaseURL: "https://puget.example/", Language: "en", Active: true},
	}, base)
	if err != nil {
		t.Fatalf("seed regions: %v", err)
	}
}

func testAPIKeyRoundTrip(t *testing.T, newStore newAPIKeyStoreFunc) {
	keys, regionRepo := newStore(t)
	seedAPIKeyRegions(t, regionRepo)
	ctx := context.Background()

	by := apikey.Actor{Kind: apikey.ActorOperator, ID: 9}
	created, err := keys.CreateRegionKey(ctx, 0, "obacloud", "hash-a", by, base)
	if err != nil {
		t.Fatalf("CreateRegionKey: %v", err)
	}
	if created.ID == 0 {
		t.Error("CreateRegionKey returned id 0")
	}
	if created.RegionID != 0 || created.Name != "obacloud" || created.KeyHash != "hash-a" {
		t.Errorf("created = %+v, want region 0 / obacloud / hash-a", created)
	}
	if created.CreatedBy != by {
		t.Errorf("CreatedBy = %+v, want %+v", created.CreatedBy, by)
	}
	if !created.CreatedAt.Equal(base) {
		t.Errorf("CreatedAt = %v, want %v", created.CreatedAt, base)
	}
	if created.LastUsedAt != nil || created.RevokedAt != nil || created.RevokedBy != nil {
		t.Errorf("a fresh key must have no last-used or revocation: %+v", created)
	}

	got, err := keys.GetRegionKeyByHash(ctx, "hash-a")
	if err != nil {
		t.Fatalf("GetRegionKeyByHash: %v", err)
	}
	if got.ID != created.ID {
		t.Errorf("round trip id = %d, want %d", got.ID, created.ID)
	}

	if _, err := keys.GetRegionKeyByHash(ctx, "nope"); !errors.Is(err, apikey.ErrNotFound) {
		t.Errorf("unknown hash: err = %v, want ErrNotFound", err)
	}
}

func testAPIKeyRevokedHash(t *testing.T, newStore newAPIKeyStoreFunc) {
	keys, regionRepo := newStore(t)
	seedAPIKeyRegions(t, regionRepo)
	ctx := context.Background()

	k, err := keys.CreateRegionKey(ctx, 1, "k", "hash-a", apikey.Actor{Kind: apikey.ActorCLI}, base)
	if err != nil {
		t.Fatalf("CreateRegionKey: %v", err)
	}
	revoker := apikey.Actor{Kind: apikey.ActorPrincipal, ID: 4}
	later := base.Add(time.Hour)
	if err := keys.RevokeRegionKey(ctx, 1, k.ID, revoker, later); err != nil {
		t.Fatalf("RevokeRegionKey: %v", err)
	}

	// ErrRevoked, not ErrNotFound: a revoked key being replayed is the
	// clearest signal a credential leaked, and the middleware logs it
	// distinctly (design spec section 4.2).
	got, err := keys.GetRegionKeyByHash(ctx, "hash-a")
	if !errors.Is(err, apikey.ErrRevoked) {
		t.Fatalf("revoked hash: err = %v, want ErrRevoked", err)
	}
	if got.ID != k.ID {
		t.Errorf("ErrRevoked must still carry the row: id = %d, want %d", got.ID, k.ID)
	}

	list, err := keys.ListRegionKeys(ctx, 1)
	if err != nil {
		t.Fatalf("ListRegionKeys: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("ListRegionKeys returned %d rows, want 1 (revoked rows are kept)", len(list))
	}
	if list[0].RevokedAt == nil || !list[0].RevokedAt.Equal(later) {
		t.Errorf("RevokedAt = %v, want %v", list[0].RevokedAt, later)
	}
	if list[0].RevokedBy == nil || *list[0].RevokedBy != revoker {
		t.Errorf("RevokedBy = %+v, want %+v", list[0].RevokedBy, revoker)
	}
}

func testAPIKeyRevokeRegionScoped(t *testing.T, newStore newAPIKeyStoreFunc) {
	keys, regionRepo := newStore(t)
	seedAPIKeyRegions(t, regionRepo)
	ctx := context.Background()

	cli := apikey.Actor{Kind: apikey.ActorCLI}
	k, err := keys.CreateRegionKey(ctx, 1, "k", "hash-a", cli, base)
	if err != nil {
		t.Fatalf("CreateRegionKey: %v", err)
	}
	// The wrong region is ErrNotFound, never a successful revoke: this is
	// the fence that makes the {regionId} path segment real for the key
	// family (design spec section 3.2).
	if err := keys.RevokeRegionKey(ctx, 0, k.ID, cli, base); !errors.Is(err, apikey.ErrNotFound) {
		t.Fatalf("cross-region revoke: err = %v, want ErrNotFound", err)
	}
	if _, err := keys.GetRegionKeyByHash(ctx, "hash-a"); err != nil {
		t.Errorf("the key must still be live: %v", err)
	}
	if err := keys.RevokeRegionKey(ctx, 1, 99999, cli, base); !errors.Is(err, apikey.ErrNotFound) {
		t.Errorf("unknown id: err = %v, want ErrNotFound", err)
	}
}

func testAPIKeyRevokeIdempotent(t *testing.T, newStore newAPIKeyStoreFunc) {
	keys, regionRepo := newStore(t)
	seedAPIKeyRegions(t, regionRepo)
	ctx := context.Background()

	cli := apikey.Actor{Kind: apikey.ActorCLI}
	k, err := keys.CreateRegionKey(ctx, 1, "k", "hash-a", cli, base)
	if err != nil {
		t.Fatalf("CreateRegionKey: %v", err)
	}
	first := base.Add(time.Hour)
	if err := keys.RevokeRegionKey(ctx, 1, k.ID, cli, first); err != nil {
		t.Fatalf("first revoke: %v", err)
	}
	// A second revoke is a no-op SUCCESS: DELETE .../api_keys/{id} answers
	// 204 for an already-revoked key, and it must not move the timestamp
	// that records when the credential actually died.
	if err := keys.RevokeRegionKey(ctx, 1, k.ID, cli, base.Add(2*time.Hour)); err != nil {
		t.Fatalf("second revoke: err = %v, want nil", err)
	}
	list, err := keys.ListRegionKeys(ctx, 1)
	if err != nil {
		t.Fatalf("ListRegionKeys: %v", err)
	}
	if list[0].RevokedAt == nil || !list[0].RevokedAt.Equal(first) {
		t.Errorf("RevokedAt = %v, want the first revocation %v", list[0].RevokedAt, first)
	}
}

func testAPIKeyListByCreator(t *testing.T, newStore newAPIKeyStoreFunc) {
	keys, regionRepo := newStore(t)
	seedAPIKeyRegions(t, regionRepo)
	ctx := context.Background()

	cli := apikey.Actor{Kind: apikey.ActorCLI}
	operator := apikey.Actor{Kind: apikey.ActorOperator, ID: 9}
	principal := apikey.Actor{Kind: apikey.ActorPrincipal, ID: 4}
	other := apikey.Actor{Kind: apikey.ActorPrincipal, ID: 5}

	for i, spec := range []struct {
		region int64
		by     apikey.Actor
		hash   string
	}{
		{0, cli, "h-cli"},
		{1, operator, "h-op"},
		{0, principal, "h-p4-a"},
		{1, principal, "h-p4-b"},
		{1, other, "h-p5"},
	} {
		if _, err := keys.CreateRegionKey(ctx, spec.region, "k", spec.hash, spec.by, base.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatalf("CreateRegionKey %s: %v", spec.hash, err)
		}
	}

	for _, tc := range []struct {
		name  string
		by    apikey.Actor
		want  []string
	}{
		// The CLI case is the one a bare "created_by_id = ?" silently
		// returns nothing for.
		{"cli", cli, []string{"h-cli"}},
		{"operator", operator, []string{"h-op"}},
		{"principal 4 across regions", principal, []string{"h-p4-b", "h-p4-a"}},
		{"principal 5", other, []string{"h-p5"}},
		{"unknown principal", apikey.Actor{Kind: apikey.ActorPrincipal, ID: 99}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := keys.ListRegionKeysByCreator(ctx, tc.by)
			if err != nil {
				t.Fatalf("ListRegionKeysByCreator: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %d keys, want %d: %+v", len(got), len(tc.want), got)
			}
			for i, hash := range tc.want {
				if got[i].KeyHash != hash {
					t.Errorf("key %d hash = %q, want %q", i, got[i].KeyHash, hash)
				}
			}
		})
	}
}

func testAPIKeyRevokeByCreator(t *testing.T, newStore newAPIKeyStoreFunc) {
	keys, regionRepo := newStore(t)
	seedAPIKeyRegions(t, regionRepo)
	ctx := context.Background()

	principal := apikey.Actor{Kind: apikey.ActorPrincipal, ID: 4}
	cli := apikey.Actor{Kind: apikey.ActorCLI}
	a, err := keys.CreateRegionKey(ctx, 0, "a", "h-a", principal, base)
	if err != nil {
		t.Fatalf("CreateRegionKey: %v", err)
	}
	b, err := keys.CreateRegionKey(ctx, 1, "b", "h-b", principal, base.Add(time.Minute))
	if err != nil {
		t.Fatalf("CreateRegionKey: %v", err)
	}
	survivor, err := keys.CreateRegionKey(ctx, 1, "c", "h-c", cli, base.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("CreateRegionKey: %v", err)
	}
	// Already revoked: it must not appear in the returned ids, so the
	// operator's "these are the keys I just killed" list is accurate.
	if err := keys.RevokeRegionKey(ctx, 0, a.ID, cli, base.Add(time.Hour)); err != nil {
		t.Fatalf("pre-revoke: %v", err)
	}

	at := base.Add(2 * time.Hour)
	ids, err := keys.RevokeRegionKeysByCreator(ctx, principal, cli, at)
	if err != nil {
		t.Fatalf("RevokeRegionKeysByCreator: %v", err)
	}
	if len(ids) != 1 || ids[0] != b.ID {
		t.Fatalf("ids = %v, want [%d]", ids, b.ID)
	}
	if _, err := keys.GetRegionKeyByHash(ctx, "h-b"); !errors.Is(err, apikey.ErrRevoked) {
		t.Errorf("h-b: err = %v, want ErrRevoked", err)
	}
	if _, err := keys.GetRegionKeyByHash(ctx, survivor.KeyHash); err != nil {
		t.Errorf("a key minted by a different actor must survive: %v", err)
	}
}

func testAPIKeyTouch(t *testing.T, newStore newAPIKeyStoreFunc) {
	keys, regionRepo := newStore(t)
	seedAPIKeyRegions(t, regionRepo)
	ctx := context.Background()

	k, err := keys.CreateRegionKey(ctx, 1, "k", "h", apikey.Actor{Kind: apikey.ActorCLI}, base)
	if err != nil {
		t.Fatalf("CreateRegionKey: %v", err)
	}
	at := base.Add(90 * time.Minute)
	if err := keys.TouchRegionKey(ctx, k.ID, at); err != nil {
		t.Fatalf("TouchRegionKey: %v", err)
	}
	got, err := keys.GetRegionKeyByHash(ctx, "h")
	if err != nil {
		t.Fatalf("GetRegionKeyByHash: %v", err)
	}
	if got.LastUsedAt == nil || !got.LastUsedAt.Equal(at) {
		t.Errorf("LastUsedAt = %v, want %v", got.LastUsedAt, at)
	}
	// Touching a row that is gone must not be an error: the touch is
	// best-effort and races a concurrent revoke.
	if err := keys.TouchRegionKey(ctx, 99999, at); err != nil {
		t.Errorf("touch of an unknown id: err = %v, want nil", err)
	}
}

func testAPIKeyListOrder(t *testing.T, newStore newAPIKeyStoreFunc) {
	keys, regionRepo := newStore(t)
	seedAPIKeyRegions(t, regionRepo)
	ctx := context.Background()

	cli := apikey.Actor{Kind: apikey.ActorCLI}
	for i, hash := range []string{"h1", "h2", "h3"} {
		if _, err := keys.CreateRegionKey(ctx, 1, hash, hash, cli, base.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatalf("CreateRegionKey: %v", err)
		}
	}
	if _, err := keys.CreateRegionKey(ctx, 0, "other", "h-other", cli, base); err != nil {
		t.Fatalf("CreateRegionKey: %v", err)
	}
	got, err := keys.ListRegionKeys(ctx, 1)
	if err != nil {
		t.Fatalf("ListRegionKeys: %v", err)
	}
	want := []string{"h3", "h2", "h1"}
	if len(got) != len(want) {
		t.Fatalf("got %d keys, want %d (another region's key must not appear)", len(got), len(want))
	}
	for i := range want {
		if got[i].KeyHash != want[i] {
			t.Errorf("key %d = %q, want %q (newest first)", i, got[i].KeyHash, want[i])
		}
	}
}

func testAPIKeyCascade(t *testing.T, newStore newAPIKeyStoreFunc) {
	keys, regionRepo := newStore(t)
	seedAPIKeyRegions(t, regionRepo)
	ctx := context.Background()

	if _, err := keys.CreateRegionKey(ctx, 1, "k", "h", apikey.Actor{Kind: apikey.ActorCLI}, base); err != nil {
		t.Fatalf("CreateRegionKey: %v", err)
	}
	// regions.Repository has no Delete -- the sidecar never removes a region
	// -- so the cascade is asserted through the raw deleter the adapter
	// exposes for exactly this test. See RegionDeleter below.
	deleter, ok := regionRepo.(RegionDeleter)
	if !ok {
		t.Skip("this adapter does not expose DeleteRegionForTest")
	}
	if err := deleter.DeleteRegionForTest(ctx, 1); err != nil {
		t.Fatalf("DeleteRegionForTest: %v", err)
	}
	if _, err := keys.GetRegionKeyByHash(ctx, "h"); !errors.Is(err, apikey.ErrNotFound) {
		t.Errorf("after the region is deleted: err = %v, want ErrNotFound", err)
	}
}

func testPrincipalLifecycle(t *testing.T, newStore newAPIKeyStoreFunc) {
	keys, _ := newStore(t)
	ctx := context.Background()

	p, err := keys.CreatePrincipal(ctx, "rails", "ph", base)
	if err != nil {
		t.Fatalf("CreatePrincipal: %v", err)
	}
	if p.ID == 0 || p.Name != "rails" || !p.CreatedAt.Equal(base) {
		t.Errorf("created = %+v", p)
	}
	if _, err := keys.GetPrincipalByHash(ctx, "ph"); err != nil {
		t.Fatalf("GetPrincipalByHash: %v", err)
	}
	if _, err := keys.GetPrincipalByHash(ctx, "nope"); !errors.Is(err, apikey.ErrNotFound) {
		t.Errorf("unknown hash: err = %v, want ErrNotFound", err)
	}

	at := base.Add(time.Hour)
	if err := keys.TouchPrincipal(ctx, p.ID, at); err != nil {
		t.Fatalf("TouchPrincipal: %v", err)
	}
	list, err := keys.ListPrincipals(ctx)
	if err != nil {
		t.Fatalf("ListPrincipals: %v", err)
	}
	if len(list) != 1 || list[0].LastUsedAt == nil || !list[0].LastUsedAt.Equal(at) {
		t.Fatalf("ListPrincipals = %+v", list)
	}

	if err := keys.RevokePrincipal(ctx, p.ID, at); err != nil {
		t.Fatalf("RevokePrincipal: %v", err)
	}
	if _, err := keys.GetPrincipalByHash(ctx, "ph"); !errors.Is(err, apikey.ErrRevoked) {
		t.Errorf("revoked principal: err = %v, want ErrRevoked", err)
	}
	if err := keys.RevokePrincipal(ctx, p.ID, at.Add(time.Hour)); err != nil {
		t.Errorf("second revoke: err = %v, want nil (no-op success)", err)
	}
	if err := keys.RevokePrincipal(ctx, 99999, at); !errors.Is(err, apikey.ErrNotFound) {
		t.Errorf("unknown principal: err = %v, want ErrNotFound", err)
	}
}

// RegionDeleter is the escape hatch testAPIKeyCascade needs: regions are
// never deleted through regions.Repository (a directory sync only upserts),
// so an adapter opts into the cascade assertion by implementing this.
type RegionDeleter interface {
	DeleteRegionForTest(ctx context.Context, id int64) error
}
```

- [ ] **Step 5: Hook the suite into the SQLite tests**

Add to `internal/store/sqlite/store_test.go`, following the shape of the existing `TestSurveyRepositoryConformance`:

```go
func TestAPIKeyRepositoryConformance(t *testing.T) {
	t.Parallel()

	storetest.RunAPIKeyRepository(t, func(t *testing.T) (apikey.Repository, regions.Repository) {
		t.Helper()
		store := sqlitetest.Open(t)
		return store.APIKeys(), store.Regions()
	})
}
```

Add the `apikey` import.

- [ ] **Step 6: Run it to verify it fails**

Run: `go test ./internal/store/... -run TestAPIKeyRepositoryConformance`
Expected: FAIL — `store.APIKeys undefined`.

- [ ] **Step 7: Write the adapter**

Create `internal/store/sqlite/apikeys.go`. Key implementation notes:

- `apiKeyRepo` mirrors the other repos: a struct holding `q *gen.Queries`, `db *sql.DB`, and `logger *slog.Logger` exactly as the surveys repo does; copy that constructor shape.
- `regionKeyFromRow` maps `gen.RegionApiKey` to `apikey.RegionKey` using `unixToTime` and `nullUnixToTime`. `CreatedBy` is `apikey.Actor{Kind: r.CreatedByKind, ID: r.CreatedByID.Int64}` — the `CHECK` constraint guarantees `ID` is 0 exactly when the kind is `cli`. `RevokedBy` is nil unless `r.RevokedByKind.Valid`.
- `actorToColumns(a apikey.Actor) (kind string, id sql.NullInt64)` returns `{Valid: false}` for `ActorCLI` and `{Int64: a.ID, Valid: true}` otherwise. Reuse it for both create and revoke.
- `GetRegionKeyByHash` / `GetPrincipalByHash`: `sql.ErrNoRows` → `apikey.ErrNotFound`; a row whose `revoked_at` is non-NULL → the mapped row plus `apikey.ErrRevoked`.
- `RevokeRegionKey` runs in one `immediate` transaction: `GetRegionAPIKey(id, region_id)` → `sql.ErrNoRows` becomes `apikey.ErrNotFound`; a row already carrying `revoked_at` commits nothing and returns nil; otherwise `RevokeRegionAPIKey`. The read-then-write must be in a transaction so a concurrent revoke cannot make the second caller see 0 rows and report ErrNotFound for a key that exists.
- `RevokePrincipal` mirrors it via `GetServicePrincipal`.
- `ListRegionKeysByCreator` / `RevokeRegionKeysByCreator` dispatch on `by.Kind == apikey.ActorCLI` to the `...ByCLI` query, otherwise the `...ByCreator` one.
- `TouchRegionKey` / `TouchPrincipal` use the `:exec` queries; a missing row is not an error.
- Add `DeleteRegionForTest(ctx, id)` to `regionRepo` in `store.go`, next to the existing `WriteHalfSetCentroidForTest` / `InsertHalfSetCentroidForTest` helpers, executing `DELETE FROM regions WHERE id = ?` through `r.db`. Doc comment: it exists solely so the storetest cascade case can prove `ON DELETE CASCADE` is enforced; production never deletes a region.
- Add to `store.go`:

```go
// APIKeys returns the region API key and service principal repository.
func (s *Store) APIKeys() apikey.Repository {
	return &apiKeyRepo{db: s.db, q: s.q, logger: s.logger}
}
```

and extend the package doc comment's list of repositories.

- [ ] **Step 8: Run the suite to verify it passes**

Run: `go test ./internal/store/... -run 'TestAPIKeyRepositoryConformance'`
Expected: PASS, including every subtest.

- [ ] **Step 9: Prove the CLI-creator case can fail**

Change `ListRegionKeysByCreator` to always use the `...ByCreator` query (no CLI dispatch), re-run, confirm `ListByCreatorCoversAllThreeKinds/cli` fails with "got 0 keys, want 1", then revert.

- [ ] **Step 10: Full check and commit**

```bash
make generate-check
go test ./internal/... && go vet ./... && make lint
git add internal/store internal/apikey
git commit -m "feat(store): region_api_keys and service_principals tables and repository"
```

---

## Task 3: Region-scoped reads for alarms and ghost bus reports

The design principle here (spec section 3.2): **every new method that addresses a resource takes the region as a query condition**, so tenancy lives in SQL rather than in a Go comparison a refactor could drop.

**Files:**
- Modify: `internal/alarms/alarms.go` (Repository interface)
- Modify: `internal/ghostbus/ghostbus.go` (Repository interface)
- Modify: `internal/store/sqlite/queries/alarms.sql`, `internal/store/sqlite/queries/ghostbus.sql`
- Modify: `internal/store/sqlite/alarms.go`, `internal/store/sqlite/ghostbus.go`
- Test: `internal/store/storetest/alarmtest.go`, `internal/store/storetest/ghostbustest.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces:
  - `alarms.Repository.ListByRegion(ctx context.Context, regionID int64) ([]Alarm, error)`
  - `alarms.Repository.GetInRegion(ctx context.Context, regionID, id int64) (Alarm, error)` — `ErrNotFound` for an unknown id **or** an id in another region
  - `ghostbus.Repository.GetByPublicID(ctx context.Context, regionID int64, publicID string) (Report, error)` — `ErrNotFound` for an unknown public id **or** one in another region

Note: `alarms.List` (the scheduler's all-region sweep) is unchanged.

- [ ] **Step 1: Write the failing storetest cases**

Add to `internal/store/storetest/alarmtest.go` — a new subtest registered in `RunAlarmRepository`:

```go
	t.Run("RegionScopedReads", func(t *testing.T) { testAlarmRegionScopedReads(t, newStore) })
```

```go
// testAlarmRegionScopedReads pins the tenancy fence the admin API leans on:
// the region is a query condition, not something a handler compares after
// the fact (design spec section 3.2).
func testAlarmRegionScopedReads(t *testing.T, newStore newAlarmStoreFunc) {
	repo, regionRepo := newStore(t)
	ctx := context.Background()
	seedAlarmRegions(t, regionRepo) // reuse this file's existing region seeder

	inA, err := repo.Create(ctx, alarms.NewAlarm{
		RegionID: 0, Token: "tok-a", APIVersion: 2, UserPushID: "u1",
		OperatingSystem: "ios", StopID: "s1", SecondsBefore: 600, Message: "a",
	}, base)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	inB, err := repo.Create(ctx, alarms.NewAlarm{
		RegionID: 1, Token: "tok-b", APIVersion: 2, UserPushID: "u2",
		OperatingSystem: "android", StopID: "s2", SecondsBefore: 600, Message: "b",
	}, base.Add(time.Minute))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	listed, err := repo.ListByRegion(ctx, 0)
	if err != nil {
		t.Fatalf("ListByRegion: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != inA.ID {
		t.Fatalf("ListByRegion(0) = %+v, want only alarm %d", listed, inA.ID)
	}

	got, err := repo.GetInRegion(ctx, 0, inA.ID)
	if err != nil {
		t.Fatalf("GetInRegion: %v", err)
	}
	if got.Token != "tok-a" {
		t.Errorf("GetInRegion token = %q, want tok-a", got.Token)
	}
	// The whole point: an alarm that exists, addressed through the wrong
	// region, is indistinguishable from one that does not exist.
	if _, err := repo.GetInRegion(ctx, 0, inB.ID); !errors.Is(err, alarms.ErrNotFound) {
		t.Errorf("GetInRegion across regions: err = %v, want ErrNotFound", err)
	}
	if _, err := repo.GetInRegion(ctx, 0, 99999); !errors.Is(err, alarms.ErrNotFound) {
		t.Errorf("GetInRegion unknown id: err = %v, want ErrNotFound", err)
	}
}
```

If `alarmtest.go` has no shared region seeder, add `seedAlarmRegions` upserting regions 0 and 1 from `base`, matching the seeder in `apikeytest.go`.

Add the mirror case to `internal/store/storetest/ghostbustest.go`, registered as `t.Run("GetByPublicIDIsRegionScoped", ...)`:

```go
func testGhostBusGetByPublicID(t *testing.T, newStore newGhostBusStoreFunc) {
	repo, regionRepo := newStore(t)
	ctx := context.Background()
	seedGhostBusRegions(t, regionRepo) // reuse this file's existing seeder

	inA, err := repo.Create(ctx, ghostbus.NewReport{
		RegionID: 0, PublicID: "pub-a", UserIdentifier: "u1", TripIdentifier: "t1",
		ServiceDate: 1767225600000, RouteIdentifier: "r1", StopIdentifier: "s1",
		WaitDurationMinutes: 10,
	}, base)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := repo.Create(ctx, ghostbus.NewReport{
		RegionID: 1, PublicID: "pub-b", UserIdentifier: "u2", TripIdentifier: "t2",
		ServiceDate: 1767225600000, RouteIdentifier: "r2", StopIdentifier: "s2",
		WaitDurationMinutes: 10,
	}, base); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := repo.GetByPublicID(ctx, 0, "pub-a")
	if err != nil {
		t.Fatalf("GetByPublicID: %v", err)
	}
	if got.ID != inA.ID {
		t.Errorf("id = %d, want %d", got.ID, inA.ID)
	}
	if _, err := repo.GetByPublicID(ctx, 0, "pub-b"); !errors.Is(err, ghostbus.ErrNotFound) {
		t.Errorf("across regions: err = %v, want ErrNotFound", err)
	}
	if _, err := repo.GetByPublicID(ctx, 0, "nope"); !errors.Is(err, ghostbus.ErrNotFound) {
		t.Errorf("unknown public id: err = %v, want ErrNotFound", err)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/store/... -run 'TestAlarmRepositoryConformance|TestGhostBusRepositoryConformance'`
Expected: FAIL to compile — `repo.ListByRegion undefined`, `repo.GetByPublicID undefined`.

- [ ] **Step 3: Extend the two domain interfaces**

In `internal/alarms/alarms.go`, inside `Repository`:

```go
	// ListByRegion returns one region's alarms, oldest first. The admin API
	// reads it; the scheduler still uses List, which sweeps every region.
	ListByRegion(ctx context.Context, regionID int64) ([]Alarm, error)
	// GetInRegion takes the region as a query condition rather than
	// comparing it afterwards, so an alarm addressed through the wrong
	// region is ErrNotFound in SQL and cannot become a Go check somebody
	// later deletes (design spec section 3.2).
	GetInRegion(ctx context.Context, regionID, id int64) (Alarm, error)
```

In `internal/ghostbus/ghostbus.go`, inside `Repository`:

```go
	// GetByPublicID resolves one report within a region. The region is a
	// query condition: a report in another region is ErrNotFound, never a
	// row the caller has to remember to check.
	GetByPublicID(ctx context.Context, regionID int64, publicID string) (Report, error)
```

- [ ] **Step 4: Add the queries**

Append to `internal/store/sqlite/queries/alarms.sql` (this file uses `@name` shorthand — match it):

```sql
-- name: ListAlarmsByRegion :many
SELECT * FROM alarms WHERE region_id = @region_id ORDER BY id;

-- name: GetAlarmInRegion :one
SELECT * FROM alarms WHERE id = @id AND region_id = @region_id;
```

Append to `internal/store/sqlite/queries/ghostbus.sql`:

```sql
-- name: GetGhostBusReportByPublicID :one
SELECT * FROM ghost_bus_reports
WHERE region_id = @region_id AND public_identifier = @public_identifier;
```

Confirm the column name against `migrations/00007_ghost_bus_reports.sql` before writing it (the unique index is `ghost_bus_reports_public_identifier_idx`).

- [ ] **Step 5: Generate and implement**

Run `make generate`, then add the three adapter methods, reusing each file's existing row-mapping helper (`alarmFromRow`, `reportFromRow`) and its `sql.ErrNoRows` → domain-`ErrNotFound` mapping. No new mapping logic; these are the same shapes as the existing `FindV1` and `ListForExport`.

- [ ] **Step 6: Run to verify it passes**

Run: `make generate-check && go test ./internal/store/...`
Expected: PASS.

- [ ] **Step 7: Prove the fence can fail**

Drop `AND region_id = @region_id` from `GetAlarmInRegion`, run `make generate && go test ./internal/store/... -run TestAlarmRepositoryConformance`, confirm `RegionScopedReads` fails on the cross-region case, then restore and regenerate.

- [ ] **Step 8: Commit**

```bash
git add internal/alarms internal/ghostbus internal/store
git commit -m "feat(store): region-scoped alarm and ghost bus report reads"
```

---

## Task 4: Region-scoped surveys repository methods

**Files:**
- Modify: `internal/surveys/surveys.go` (Repository interface)
- Modify: `internal/store/sqlite/queries/surveys.sql`
- Modify: `internal/store/sqlite/surveys.go`
- Test: `internal/store/storetest/surveytest.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces, on `surveys.Repository`:
  - `UpdateStudy(ctx context.Context, regionID, id int64, name, description string, now time.Time) (Study, error)` — `ErrNotFound` for an unknown id or an id in another region
  - `CreateSurveyInRegion(ctx context.Context, regionID, studyID int64, def Definition, now time.Time) (Survey, error)` — the study's region is a **join condition**, so a body-borne `study_id` from another region is `ErrNotFound`
  - `GetResponseInRegion(ctx context.Context, regionID int64, publicID string) (Response, error)` — one query joining `survey_responses → surveys → studies`

Existing methods keep their bare-id signatures (`GetStudy`, `GetSurvey`, `UpdateSurvey`, `DeleteSurvey`, `ListResponses`); the HTTP loaders in Task 8/9 are what fence those.

- [ ] **Step 1: Write the failing storetest cases**

Register three subtests in `RunSurveyRepository`:

```go
	t.Run("UpdateStudyIsRegionScoped", func(t *testing.T) { testUpdateStudyRegionScoped(t, newStore) })
	t.Run("CreateSurveyInRegionRejectsForeignStudy", func(t *testing.T) { testCreateSurveyInRegion(t, newStore) })
	t.Run("GetResponseInRegionIsScoped", func(t *testing.T) { testGetResponseInRegion(t, newStore) })
```

```go
func testUpdateStudyRegionScoped(t *testing.T, newStore newSurveyStoreFunc) {
	repo, regionRepo := newStore(t)
	ctx := context.Background()
	seedSurveyRegions(t, regionRepo) // reuse this file's existing seeder

	inA, err := repo.CreateStudy(ctx, 0, "A", "first", base)
	if err != nil {
		t.Fatalf("CreateStudy: %v", err)
	}
	at := base.Add(time.Hour)
	updated, err := repo.UpdateStudy(ctx, 0, inA.ID, "A2", "second", at)
	if err != nil {
		t.Fatalf("UpdateStudy: %v", err)
	}
	if updated.Name != "A2" || updated.Description != "second" {
		t.Errorf("updated = %+v, want A2/second", updated)
	}
	if !updated.UpdatedAt.Equal(at) {
		t.Errorf("UpdatedAt = %v, want %v", updated.UpdatedAt, at)
	}
	if !updated.CreatedAt.Equal(base) {
		t.Errorf("CreatedAt moved: %v, want %v", updated.CreatedAt, base)
	}

	// The same study, addressed through the wrong region, must not update
	// and must not report success.
	if _, err := repo.UpdateStudy(ctx, 1, inA.ID, "hijacked", "", at); !errors.Is(err, surveys.ErrNotFound) {
		t.Fatalf("cross-region UpdateStudy: err = %v, want ErrNotFound", err)
	}
	after, err := repo.GetStudy(ctx, inA.ID)
	if err != nil {
		t.Fatalf("GetStudy: %v", err)
	}
	if after.Name != "A2" {
		t.Errorf("a refused update still wrote: name = %q", after.Name)
	}
}

func testCreateSurveyInRegion(t *testing.T, newStore newSurveyStoreFunc) {
	repo, regionRepo := newStore(t)
	ctx := context.Background()
	seedSurveyRegions(t, regionRepo)

	studyA, err := repo.CreateStudy(ctx, 0, "A", "", base)
	if err != nil {
		t.Fatalf("CreateStudy: %v", err)
	}
	def := surveys.Definition{
		Name: "Ride quality", Available: true,
		Questions: []surveys.QuestionDefinition{{Content: minimalQuestionContent(t)}},
	}

	created, err := repo.CreateSurveyInRegion(ctx, 0, studyA.ID, def, base)
	if err != nil {
		t.Fatalf("CreateSurveyInRegion: %v", err)
	}
	if created.StudyID != studyA.ID {
		t.Errorf("StudyID = %d, want %d", created.StudyID, studyA.ID)
	}

	// A study_id from another region is ErrNotFound, decided by the join --
	// this is what stops POST /regions/1/surveys {"study_id": <region 0's>}.
	if _, err := repo.CreateSurveyInRegion(ctx, 1, studyA.ID, def, base); !errors.Is(err, surveys.ErrNotFound) {
		t.Errorf("foreign study: err = %v, want ErrNotFound", err)
	}
	if _, err := repo.CreateSurveyInRegion(ctx, 0, 99999, def, base); !errors.Is(err, surveys.ErrNotFound) {
		t.Errorf("unknown study: err = %v, want ErrNotFound", err)
	}
	list, err := repo.ListSurveys(ctx, 1)
	if err != nil {
		t.Fatalf("ListSurveys: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("region 1 gained %d surveys from a refused create", len(list))
	}
}

func testGetResponseInRegion(t *testing.T, newStore newSurveyStoreFunc) {
	repo, regionRepo := newStore(t)
	ctx := context.Background()
	seedSurveyRegions(t, regionRepo)

	study, err := repo.CreateStudy(ctx, 0, "A", "", base)
	if err != nil {
		t.Fatalf("CreateStudy: %v", err)
	}
	survey, err := repo.CreateSurvey(ctx, study.ID, surveys.Definition{
		Name: "s", Available: true,
		Questions: []surveys.QuestionDefinition{{Content: minimalQuestionContent(t)}},
	}, base)
	if err != nil {
		t.Fatalf("CreateSurvey: %v", err)
	}
	resp, err := repo.CreateResponse(ctx, surveys.NewResponse{
		SurveyID: survey.ID, PublicID: "pub-1", UserIdentifier: "rider-1",
	}, base)
	if err != nil {
		t.Fatalf("CreateResponse: %v", err)
	}

	got, err := repo.GetResponseInRegion(ctx, 0, "pub-1")
	if err != nil {
		t.Fatalf("GetResponseInRegion: %v", err)
	}
	if got.ID != resp.ID || got.UserIdentifier != "rider-1" {
		t.Errorf("got = %+v, want response %d", got, resp.ID)
	}
	// Reaching a response through another region's survey is the case the
	// handler tests mirror at the HTTP layer.
	if _, err := repo.GetResponseInRegion(ctx, 1, "pub-1"); !errors.Is(err, surveys.ErrNotFound) {
		t.Errorf("across regions: err = %v, want ErrNotFound", err)
	}
	if _, err := repo.GetResponseInRegion(ctx, 0, "nope"); !errors.Is(err, surveys.ErrNotFound) {
		t.Errorf("unknown public id: err = %v, want ErrNotFound", err)
	}
}
```

`minimalQuestionContent` is whatever helper `surveytest.go` already uses to build a valid `surveys.Content`; reuse it rather than inventing a second one. If none exists, add one that produces the smallest content `Definition.Validate` accepts, and use it from all three cases.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/store/... -run TestSurveyRepositoryConformance`
Expected: FAIL to compile — `repo.UpdateStudy undefined`.

- [ ] **Step 3: Extend `surveys.Repository`**

```go
	// UpdateStudy renames a study. The region is a query condition, so a
	// study in another region is ErrNotFound and nothing is written
	// (design spec section 3.2).
	UpdateStudy(ctx context.Context, regionID, id int64, name, description string, now time.Time) (Study, error)
	// CreateSurveyInRegion is CreateSurvey with the study's region as a
	// JOIN condition: a study_id that arrived in a request body but belongs
	// to another region is ErrNotFound. Body-borne ids never go through a
	// load-then-compare.
	CreateSurveyInRegion(ctx context.Context, regionID, studyID int64, def Definition, now time.Time) (Survey, error)
	// GetResponseInRegion resolves one response through its survey's study's
	// region, in a single query.
	GetResponseInRegion(ctx context.Context, regionID int64, publicID string) (Response, error)
```

- [ ] **Step 4: Add the queries**

Append to `internal/store/sqlite/queries/surveys.sql` (`@name` shorthand, matching the file):

```sql
-- name: UpdateStudy :one
UPDATE studies SET name = @name, description = @description, updated_at = @now
WHERE id = @id AND region_id = @region_id
RETURNING *;

-- name: GetStudyInRegion :one
SELECT * FROM studies WHERE id = @id AND region_id = @region_id;

-- name: GetResponseByPublicIDInRegion :one
SELECT survey_responses.* FROM survey_responses
JOIN surveys ON surveys.id = survey_responses.survey_id
JOIN studies ON studies.id = surveys.study_id
WHERE survey_responses.public_identifier = @public_identifier
  AND studies.region_id = @region_id;
```

Confirm `public_identifier` against `migrations/00006_surveys.sql` and against the existing `GetResponseByPublicID` before writing it.

- [ ] **Step 5: Implement the three adapter methods**

In `internal/store/sqlite/surveys.go`:

- `UpdateStudy` calls the new query; `sql.ErrNoRows` → `surveys.ErrNotFound`; maps the row with the file's existing `studyFromRow`.
- `CreateSurveyInRegion` runs inside the same write transaction shape `CreateSurvey` already uses (the survey row plus its questions). Its first statement is `GetStudyInRegion`; `sql.ErrNoRows` → `surveys.ErrNotFound`, aborting before any insert. Then it reuses `CreateSurvey`'s existing transactional body — extract that body into an unexported `createSurveyTx(ctx, q *gen.Queries, studyID int64, def surveys.Definition, now time.Time) (surveys.Survey, error)` and have **both** `CreateSurvey` and `CreateSurveyInRegion` call it, so the two entry points cannot drift on question insertion or `Study` population.
- `GetResponseInRegion` calls the new query; `sql.ErrNoRows` → `surveys.ErrNotFound`; maps with the file's existing `responseFromRow` (including its answers decoding).

- [ ] **Step 6: Run to verify it passes**

Run: `make generate && make generate-check && go test ./internal/store/...`
Expected: PASS.

- [ ] **Step 7: Prove the join can fail**

Drop `AND studies.region_id = @region_id` from `GetResponseByPublicIDInRegion`, regenerate, run `go test ./internal/store/... -run TestSurveyRepositoryConformance`, confirm `GetResponseInRegionIsScoped` fails on the cross-region case, then restore and regenerate.

- [ ] **Step 8: Commit**

```bash
git add internal/surveys internal/store
git commit -m "feat(store): region-scoped study, survey create, and response reads"
```

---

## Task 5: The principal model and bearer authentication

This replaces `requireSession` with `requirePrincipal` and gives every route in `adminRoutes` an `allowed` set. Route **paths** do not move yet — that is Task 6 — so this task's blast radius is authentication only.

**Files:**
- Create: `internal/httpapi/principal.go`
- Create: `internal/httpapi/bearer.go`
- Create: `internal/httpapi/bearer_test.go`
- Modify: `internal/httpapi/middleware.go`, `internal/httpapi/router.go`, `internal/httpapi/session.go`
- Modify: `internal/httpapi/admin_alerts_test.go` (the route-table sweeps)
- Modify: `cmd/sidecar/main.go`

**Interfaces:**
- Consumes: `apikey.Repository`, `apikey.ParsePrefix`, `apikey.Hash`, `apikey.ErrRevoked`, `apikey.Actor` (Task 1); `Store.APIKeys()` (Task 2).
- Produces:
  - `Deps.APIKeys apikey.Repository` and `Deps.BearerFailLimiter *ratelimit.Limiter`
  - `principalKind` with `principalOperator`, `principalRegionKey`, `principalService`
  - `principalSet` with `func (principalSet) has(principalKind) bool`; the three named sets `operatorOnly`, `operatorOrKey`, `operatorOrService`
  - `principal` with `canAccessRegion(int64) bool`, `actor() apikey.Actor`, `LogValue() slog.Value`
  - `func principalFrom(ctx context.Context) (principal, bool)`
  - `adminRoute.allowed principalSet` (nil means "no principal required": login and logout only)
  - `func (h *authMiddleware) requirePrincipal(allowed principalSet, next http.Handler) http.Handler`
  - `const bearerFailuresPerMinute = 60`
  - `const forbiddenBody = "forbidden"`, `const invalidAPIKeyBody = "invalid api key"`

- [ ] **Step 1: Write the failing bearer tests**

Create `internal/httpapi/bearer_test.go` (package `httpapi`, so it can reach `adminRoutes` and the unexported types). It builds on `newAdminFixtureWithDeps`, which Step 5 extends to wire `APIKeys`.

```go
package httpapi

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/apikey"
	"github.com/OneBusAway/sidecar/internal/auth"
	"github.com/OneBusAway/sidecar/internal/ratelimit"
)

// mintRegionKey creates a live region key in the fixture's store and returns
// its raw form. The raw key exists only here and in the Authorization header
// the test sends -- exactly as in production.
func (f *adminFixture) mintRegionKey(t *testing.T, regionID int64) string {
	t.Helper()
	raw, hash, err := apikey.NewRegionKey(regionID)
	if err != nil {
		t.Fatalf("NewRegionKey: %v", err)
	}
	_, err = f.store.APIKeys().CreateRegionKey(context.Background(), regionID, "test",
		hash, apikey.Actor{Kind: apikey.ActorCLI}, testNow)
	if err != nil {
		t.Fatalf("CreateRegionKey: %v", err)
	}
	return raw
}

func (f *adminFixture) mintPrincipal(t *testing.T) string {
	t.Helper()
	raw, hash, err := apikey.NewPrincipalKey()
	if err != nil {
		t.Fatalf("NewPrincipalKey: %v", err)
	}
	if _, err := f.store.APIKeys().CreatePrincipal(context.Background(), "test", hash, testNow); err != nil {
		t.Fatalf("CreatePrincipal: %v", err)
	}
	return raw
}

// sendBearer issues a request authenticated by an Authorization header. It
// deliberately sends NO Sec-Fetch-Site and NO Origin, which is what a
// server-side HTTP client looks like and what crossSiteGuard already passes.
func sendBearer(h http.Handler, method, target, body, authorization string) *httptest.ResponseRecorder {
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequestWithContext(context.Background(), method, target, r)
	req.Host = "sidecar.test"
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestBearer_ValidRegionKeyAuthenticates is the happy path: no cookie, no
// browser headers, just the header Rails will send.
func TestBearer_ValidRegionKeyAuthenticates(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	raw := f.mintRegionKey(t, regionPuget)

	rec := sendBearer(f.handler, http.MethodGet, "/api/admin/v1/alerts", "", "Bearer "+raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
}

// TestBearer_BeatsCookie: an Authorization header means cookies are ignored
// ENTIRELY. Falling back to the cookie would let a browser session silently
// rescue a request that was meant to be, and failed as, a bearer call --
// hiding a revoked key from whoever revoked it.
func TestBearer_BeatsCookie(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/admin/v1/session", nil)
	req.Host = "sidecar.test"
	req.Header.Set("Authorization", "Bearer obask_1_not-a-real-key-not-a-real-key-not-a-real")
	req.AddCookie(f.cookie) // a perfectly good operator session
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body = %s", rec.Code, rec.Body.String())
	}
	if got, want := bodyText(rec), `{"error":"invalid api key"}`; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}

// TestBearer_MalformedHeadersAre401 walks every shape that must NOT fall
// through to the cookie path.
func TestBearer_MalformedHeadersAre401(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	live := f.mintRegionKey(t, regionPuget)

	tests := []struct {
		name   string
		header string
	}{
		{"empty value", ""},
		{"no scheme", live},
		{"wrong scheme", "Token " + live},
		{"basic", "Basic " + live},
		{"two spaces", "Bearer  " + live},
		{"no space", "Bearer" + live},
		{"unparseable prefix", "Bearer sk_live_abcdef"},
		{"unknown key", "Bearer obask_1_" + strings.Repeat("A", 43)},
		{"over long", "Bearer " + strings.Repeat("A", 200)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// An empty header value must be "a bearer attempt that failed",
			// not "no header at all"; Set with "" would drop it, so the map
			// is written directly.
			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/admin/v1/alerts", nil)
			req.Host = "sidecar.test"
			req.Header["Authorization"] = []string{tc.header}
			rec := httptest.NewRecorder()
			f.handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401; body = %s", rec.Code, rec.Body.String())
			}
			if got, want := bodyText(rec), `{"error":"invalid api key"}`; got != want {
				t.Errorf("body = %q, want %q", got, want)
			}
		})
	}
}

// TestBearer_DuplicateHeaderIs401: two Authorization headers is ambiguous,
// and picking one is how a proxy-injected header gets silently preferred
// over the client's.
func TestBearer_DuplicateHeaderIs401(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	live := f.mintRegionKey(t, regionPuget)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/admin/v1/alerts", nil)
	req.Host = "sidecar.test"
	req.Header["Authorization"] = []string{"Bearer " + live, "Bearer " + live}
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401; body = %s", rec.Code, rec.Body.String())
	}
}

// TestBearer_RevokedKeyIsLoggedDistinctly. A revoked key being replayed is
// the clearest signal a credential leaked, so it must be greppable --
// reason=revoked -- and it must never carry any part of the secret.
func TestBearer_RevokedKeyIsLoggedDistinctly(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	f := newAdminFixtureWithDeps(t, func(d *Deps) {
		d.Logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	})
	raw := f.mintRegionKey(t, regionPuget)
	keys, err := f.store.APIKeys().ListRegionKeys(context.Background(), regionPuget)
	if err != nil {
		t.Fatalf("ListRegionKeys: %v", err)
	}
	if err := f.store.APIKeys().RevokeRegionKey(context.Background(), regionPuget, keys[0].ID,
		apikey.Actor{Kind: apikey.ActorCLI}, testNow); err != nil {
		t.Fatalf("RevokeRegionKey: %v", err)
	}

	rec := sendBearer(f.handler, http.MethodGet, "/api/admin/v1/alerts", "", "Bearer "+raw)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if !strings.Contains(buf.String(), "reason=revoked") {
		t.Errorf("log missing reason=revoked:\n%s", buf.String())
	}
	secret := strings.TrimPrefix(raw, "obask_1_")
	if strings.Contains(buf.String(), secret) {
		t.Errorf("log leaked the key's random segment:\n%s", buf.String())
	}
}

// TestBearer_NoFailDelay. A 256-bit random key is not guessable, so a delay
// would defend nothing while pinning a goroutine per garbage request. Deps.
// Sleep is the recorder that proves the login delay is NOT applied here.
func TestBearer_NoFailDelay(t *testing.T) {
	t.Parallel()

	var slept int
	f := newAdminFixtureWithDeps(t, func(d *Deps) {
		d.FailDelay = time.Hour
		d.Sleep = func(time.Duration) { slept++ }
	})
	sendBearer(f.handler, http.MethodGet, "/api/admin/v1/alerts", "", "Bearer obask_1_nope")
	if slept != 0 {
		t.Errorf("Sleep called %d times on a bearer failure, want 0", slept)
	}
}

// TestBearer_ThrottleChargesFailuresOnly. Rails's successful calls are the
// hot path and must be unmetered; garbage from anywhere else is not.
func TestBearer_ThrottleChargesFailuresOnly(t *testing.T) {
	t.Parallel()

	f := newAdminFixtureWithDeps(t, func(d *Deps) {
		d.BearerFailLimiter = ratelimit.New(2, time.Minute)
	})
	raw := f.mintRegionKey(t, regionPuget)

	// Ten successes do not consume the bucket.
	for i := 0; i < 10; i++ {
		if rec := sendBearer(f.handler, http.MethodGet, "/api/admin/v1/alerts", "", "Bearer "+raw); rec.Code != http.StatusOK {
			t.Fatalf("success %d: status = %d, want 200", i, rec.Code)
		}
	}
	// Two failures fit; the third is refused outright.
	for i := 0; i < 2; i++ {
		if rec := sendBearer(f.handler, http.MethodGet, "/api/admin/v1/alerts", "", "Bearer obask_1_nope"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("failure %d: status = %d, want 401", i, rec.Code)
		}
	}
	if rec := sendBearer(f.handler, http.MethodGet, "/api/admin/v1/alerts", "", "Bearer obask_1_nope"); rec.Code != http.StatusTooManyRequests {
		t.Errorf("third failure: status = %d, want 429", rec.Code)
	}
	// A valid key still works: the throttle bounds guessing, not service.
	if rec := sendBearer(f.handler, http.MethodGet, "/api/admin/v1/alerts", "", "Bearer "+raw); rec.Code != http.StatusOK {
		t.Errorf("valid key after throttling: status = %d, want 200", rec.Code)
	}
}

// TestBearer_TouchAtMostHourly. last_used_at is what tells an operator
// whether an old key is still in use before revoking it, and it must not
// cost a write on every request.
func TestBearer_TouchAtMostHourly(t *testing.T) {
	t.Parallel()

	now := testNow
	f := newAdminFixtureWithDeps(t, func(d *Deps) {
		d.Now = func() time.Time { return now }
	})
	raw := f.mintRegionKey(t, regionPuget)
	ctx := context.Background()

	read := func() *time.Time {
		list, err := f.store.APIKeys().ListRegionKeys(ctx, regionPuget)
		if err != nil {
			t.Fatalf("ListRegionKeys: %v", err)
		}
		return list[0].LastUsedAt
	}

	sendBearer(f.handler, http.MethodGet, "/api/admin/v1/alerts", "", "Bearer "+raw)
	first := read()
	if first == nil || !first.Equal(testNow) {
		t.Fatalf("first use: LastUsedAt = %v, want %v", first, testNow)
	}

	now = testNow.Add(59 * time.Minute)
	sendBearer(f.handler, http.MethodGet, "/api/admin/v1/alerts", "", "Bearer "+raw)
	if got := read(); got == nil || !got.Equal(testNow) {
		t.Errorf("after 59m: LastUsedAt = %v, want it unchanged at %v", got, testNow)
	}

	now = testNow.Add(time.Hour)
	sendBearer(f.handler, http.MethodGet, "/api/admin/v1/alerts", "", "Bearer "+raw)
	if got := read(); got == nil || !got.Equal(now) {
		t.Errorf("after 60m: LastUsedAt = %v, want %v", got, now)
	}
}

// TestBearer_PrefixRowMismatchIs401. The region id in the plaintext is a
// debugging aid; the hash decides. If the two ever disagree the key is not
// trustworthy for either region.
func TestBearer_PrefixRowMismatchIs401(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	// Mint the plaintext for region 1 but store the row against region 0.
	raw, hash, err := apikey.NewRegionKey(regionPuget)
	if err != nil {
		t.Fatalf("NewRegionKey: %v", err)
	}
	if _, err := f.store.APIKeys().CreateRegionKey(context.Background(), regionTampa, "mismatched",
		hash, apikey.Actor{Kind: apikey.ActorCLI}, testNow); err != nil {
		t.Fatalf("CreateRegionKey: %v", err)
	}

	rec := sendBearer(f.handler, http.MethodGet, "/api/admin/v1/alerts", "", "Bearer "+raw)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401; body = %s", rec.Code, rec.Body.String())
	}
}

// TestBearer_NilAPIKeysRejects: bearer auth is not configured, so an
// Authorization header is a 401 rather than a fall-through to the cookie.
func TestBearer_NilAPIKeysRejects(t *testing.T) {
	t.Parallel()

	f := newAdminFixtureWithDeps(t, func(d *Deps) { d.APIKeys = nil })
	rec := sendBearer(f.handler, http.MethodGet, "/api/admin/v1/alerts", "", "Bearer obask_1_whatever")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

// TestBearer_CrossSiteGuardStillApplies. A bearer request that carries a
// browser's foreign Origin is rejected BEFORE authentication -- the guard is
// outermost, and no bearer bypass was added to it.
func TestBearer_CrossSiteGuardStillApplies(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	raw := f.mintRegionKey(t, regionPuget)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost,
		"/api/admin/v1/alerts", strings.NewReader(minimalAlert(regionPuget, "x")))
	req.Host = "sidecar.test"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("Authorization", "Bearer "+raw)
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if got := bodyText(rec); got != crossSiteBody {
		t.Errorf("body = %q, want %q", got, crossSiteBody)
	}
}

// TestPrincipalLogValueOmitsPasswordHash. principal embeds auth.User, which
// carries the argon2 PHC string. A future slog.Any("principal", p) must not
// print it, and only a LogValue that omits it makes that structural.
func TestPrincipalLogValueOmitsPasswordHash(t *testing.T) {
	t.Parallel()

	const phc = "$argon2id$v=19$m=65536,t=3,p=4$c2VjcmV0$SECRETHASHVALUE"
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	logger.Info("who",
		"principal", principal{
			kind: principalOperator,
			user: auth.User{ID: 1, Username: "admin", PasswordHash: phc},
		})
	logger.Info("who",
		"principal", principal{kind: principalRegionKey, regionID: 1, keyID: 8})

	if strings.Contains(buf.String(), phc) || strings.Contains(buf.String(), "SECRETHASHVALUE") {
		t.Errorf("log leaked the password hash:\n%s", buf.String())
	}
	for _, want := range []string{"username=admin", "region_id=1", "key_id=8"} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("log missing %q:\n%s", want, buf.String())
		}
	}
}
```

Add the imports this file needs (`bytes`, `io`, `log/slog`, `net/http/httptest`) — the fixture helpers `bodyText`, `crossSiteBody`, `minimalAlert`, `testNow`, `regionTampa`, `regionPuget` already exist in `admin_alerts_test.go`.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/httpapi/ -run TestBearer`
Expected: FAIL to compile — `d.APIKeys undefined`, `principal undefined`.

- [ ] **Step 3: Write `internal/httpapi/principal.go`**

```go
package httpapi

import (
	"context"
	"log/slog"

	"github.com/OneBusAway/sidecar/internal/apikey"
	"github.com/OneBusAway/sidecar/internal/auth"
)

// principalKind is which credential authenticated a request. The zero value
// is deliberately not a kind, so a principal that never went through
// requirePrincipal cannot pass an allow-list check by accident.
type principalKind int

const (
	principalOperator  principalKind = iota + 1 // session cookie
	principalRegionKey                          // obask_
	principalService                            // obasp_
)

// String is for log lines and test failure messages only; it is never a wire
// value.
func (k principalKind) String() string {
	switch k {
	case principalOperator:
		return "operator"
	case principalRegionKey:
		return "region_key"
	case principalService:
		return "service_principal"
	default:
		return "unknown"
	}
}

// principalSet is a route's allow-list. A nil set means "no principal
// required", which is true of exactly two routes (login and logout).
type principalSet []principalKind

func (s principalSet) has(k principalKind) bool {
	for _, want := range s {
		if want == k {
			return true
		}
	}
	return false
}

// The three allow-lists every admin route draws from (design spec section
// 4.5). They are named values rather than inline literals so the route table
// reads as policy and a fourth combination has to be introduced deliberately.
var (
	// operatorOnly is for routes a leaked region key must not reach:
	// sending or cancelling a push, and the cross-region region list.
	operatorOnly = principalSet{principalOperator}
	// operatorOrKey is the ordinary region-scoped authoring surface.
	operatorOrKey = principalSet{principalOperator, principalRegionKey}
	// operatorOrService is the key-management family, and the only place
	// principalService appears at all.
	operatorOrService = principalSet{principalOperator, principalService}
)

// principal is who is making an admin request.
type principal struct {
	kind principalKind
	// user is populated for operators only; whoami needs the username.
	user auth.User
	// regionID is populated for region keys only.
	regionID int64
	// keyID is populated for region keys and service principals.
	keyID int64
}

// canAccessRegion is the tenancy fence for every region-scoped route EXCEPT
// the key-management family. A service principal is never granted region
// access here: its reach comes only from requireKeyAdminRegion, which is a
// separate function precisely so that reach is visible in one place.
func (p principal) canAccessRegion(id int64) bool {
	return p.kind == principalOperator || (p.kind == principalRegionKey && p.regionID == id)
}

// actor renders the principal for a key's created_by / revoked_by columns.
func (p principal) actor() apikey.Actor {
	switch p.kind {
	case principalOperator:
		return apikey.Actor{Kind: apikey.ActorOperator, ID: p.user.ID}
	case principalService:
		return apikey.Actor{Kind: apikey.ActorPrincipal, ID: p.keyID}
	default:
		// A region key can never mint or revoke a key (design spec section
		// 2.2), so this is unreachable through the router. Returning the CLI
		// actor rather than a zero Actor keeps the CHECK constraints
		// satisfiable if it ever is reached, instead of failing the write
		// with a constraint error nobody can read.
		return apikey.Actor{Kind: apikey.ActorCLI}
	}
}

// LogValue implements slog.LogValuer. principal embeds auth.User, which
// carries the argon2 password hash: without this method a future
// slog.Any("principal", p) would print it. Emitting only kind, username,
// region id and key id makes that leak unrepresentable rather than merely
// unwritten -- the same argument as regions.Region.LogValue.
func (p principal) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("kind", p.kind.String()),
		slog.String("username", p.user.Username),
		slog.Int64("region_id", p.regionID),
		slog.Int64("key_id", p.keyID),
	)
}

// principalFrom returns the principal requirePrincipal authenticated for this
// request. The boolean is false for any context that did not pass through
// requirePrincipal, so a handler can never mistake a zero value for an
// authenticated caller.
func principalFrom(ctx context.Context) (principal, bool) {
	p, ok := ctx.Value(principalContextKey).(principal)
	return p, ok
}
```

- [ ] **Step 4: Write `internal/httpapi/bearer.go`**

```go
package httpapi

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/OneBusAway/sidecar/internal/apikey"
)

const (
	// invalidAPIKeyBody is the single message every bearer failure returns.
	// Unknown, revoked, malformed and mismatched all look identical on the
	// wire: telling them apart is a probing oracle, and the operator's log
	// carries the distinction instead.
	invalidAPIKeyBody = "invalid api key"
	// forbiddenBody is a principal kind a route does not allow. It is
	// deliberately distinct from crossSiteGuard's message so tests and Rails
	// can tell a policy refusal from a browser-origin refusal.
	forbiddenBody = "forbidden"
	// bearerFailuresPerMinute bounds failed bearer attempts per peer. Only
	// failures are charged, so Rails's steady traffic is unmetered; this is
	// the repo's one unauthenticated code path and it keeps the
	// throttle-everything-unauthenticated posture.
	bearerFailuresPerMinute = 60
	// touchInterval is how stale last_used_at is allowed to get. A write on
	// every request would put an UPDATE in front of every read.
	touchInterval = time.Hour
	// bearerScheme is matched case-insensitively, followed by exactly one
	// space.
	bearerScheme = "Bearer"
)

// authenticateBearer resolves an Authorization header into a principal. It
// writes the response itself on every failure and reports ok=false.
//
// Cookies are not consulted: if the header is present the request either
// authenticates by bearer or fails. Falling back would let a browser session
// silently rescue a call that was meant to be a bearer call, which is exactly
// how a revoked key stays invisible to whoever revoked it.
func (h *authMiddleware) authenticateBearer(w http.ResponseWriter, r *http.Request, values []string) (principal, bool) {
	if len(values) != 1 {
		// Two Authorization headers is ambiguous. Picking one is how a
		// proxy-injected header quietly wins over the client's.
		return h.rejectBearer(w, r, "duplicate authorization header", "count", len(values))
	}
	if h.deps.APIKeys == nil {
		return h.rejectBearer(w, r, "bearer auth not configured")
	}
	raw := values[0]
	if len(raw) > len(bearerScheme)+1+apikey.MaxRawLen {
		// Checked BEFORE hashing: an unauthenticated caller must not be able
		// to make the server SHA-256 an arbitrary body.
		return h.rejectBearer(w, r, "authorization header too long", "length", len(raw))
	}
	scheme, credential, found := strings.Cut(raw, " ")
	if !found || !strings.EqualFold(scheme, bearerScheme) || strings.HasPrefix(credential, " ") {
		return h.rejectBearer(w, r, "not a bearer credential", "length", len(raw))
	}

	kind, prefixRegion, ok := apikey.ParsePrefix(credential)
	if !ok {
		return h.rejectBearer(w, r, "unparseable key prefix", "length", len(credential))
	}
	switch kind {
	case apikey.KindRegion:
		return h.authenticateRegionKey(w, r, credential, prefixRegion)
	case apikey.KindPrincipal:
		return h.authenticatePrincipalKey(w, r, credential)
	default:
		return h.rejectBearer(w, r, "unknown key kind", "kind", string(kind))
	}
}

func (h *authMiddleware) authenticateRegionKey(w http.ResponseWriter, r *http.Request, credential string, prefixRegion int64) (principal, bool) {
	key, err := h.deps.APIKeys.GetRegionKeyByHash(r.Context(), apikey.Hash(credential))
	switch {
	case errors.Is(err, apikey.ErrRevoked):
		return h.rejectBearer(w, r, "revoked key replayed",
			"reason", "revoked", "kind", string(apikey.KindRegion),
			"prefix_region_id", prefixRegion, "key_id", key.ID)
	case errors.Is(err, apikey.ErrNotFound):
		return h.rejectBearer(w, r, "unknown key",
			"kind", string(apikey.KindRegion), "prefix_region_id", prefixRegion)
	case err != nil:
		serverErrorJSON(w, h.deps.Logger, "get region api key", err)
		return principal{}, false
	}
	if key.RegionID != prefixRegion {
		// The plaintext's region id is a debugging aid and the hash lookup
		// decides -- but if the two disagree the row is not trustworthy for
		// either region.
		return h.rejectBearer(w, r, "key prefix and row disagree on region",
			"prefix_region_id", prefixRegion, "row_region_id", key.RegionID, "key_id", key.ID)
	}

	h.touch(r, key.LastUsedAt, func(now time.Time) error {
		return h.deps.APIKeys.TouchRegionKey(r.Context(), key.ID, now)
	})
	return principal{kind: principalRegionKey, regionID: key.RegionID, keyID: key.ID}, true
}

func (h *authMiddleware) authenticatePrincipalKey(w http.ResponseWriter, r *http.Request, credential string) (principal, bool) {
	p, err := h.deps.APIKeys.GetPrincipalByHash(r.Context(), apikey.Hash(credential))
	switch {
	case errors.Is(err, apikey.ErrRevoked):
		return h.rejectBearer(w, r, "revoked principal replayed",
			"reason", "revoked", "kind", string(apikey.KindPrincipal), "key_id", p.ID)
	case errors.Is(err, apikey.ErrNotFound):
		return h.rejectBearer(w, r, "unknown principal", "kind", string(apikey.KindPrincipal))
	case err != nil:
		serverErrorJSON(w, h.deps.Logger, "get service principal", err)
		return principal{}, false
	}
	h.touch(r, p.LastUsedAt, func(now time.Time) error {
		return h.deps.APIKeys.TouchPrincipal(r.Context(), p.ID, now)
	})
	return principal{kind: principalService, keyID: p.ID}, true
}

// touch records use at most once per touchInterval. It is best effort: a
// failed write is logged, never surfaced, because last_used_at is an
// operator convenience and losing it must not cost Rails a request.
func (h *authMiddleware) touch(r *http.Request, lastUsed *time.Time, write func(time.Time) error) {
	now := h.deps.Now()
	if lastUsed != nil && now.Sub(*lastUsed) < touchInterval {
		return
	}
	if err := write(now); err != nil {
		h.deps.Logger.Warn("httpapi: touch api key", "err", err)
	}
}

// rejectBearer charges the failure bucket and writes the response.
//
// The bucket is charged HERE rather than by wrapping the route in
// throttleByIP, because throttleByIP charges every call: a wrapper would
// meter Rails's successful traffic, which is the hot path this must not
// touch. There is deliberately no FailDelay -- a 256-bit random key is not
// guessable, so a delay would defend nothing while pinning a goroutine per
// garbage request (design spec section 4.2).
//
// Nothing logged here may contain any part of the credential's random
// segment; only its length, its parsed kind, and the prefix's region id.
func (h *authMiddleware) rejectBearer(w http.ResponseWriter, r *http.Request, reason string, fields ...any) (principal, bool) {
	attrs := append([]any{"reason_text", reason, "remote", clientIP(r), "path", r.URL.Path}, fields...)
	h.deps.Logger.Warn("httpapi: bearer authentication failed", attrs...)
	if !h.deps.BearerFailLimiter.Allow(clientIP(r), h.deps.Now()) {
		w.WriteHeader(http.StatusTooManyRequests)
		return principal{}, false
	}
	writeJSONError(w, h.deps.Logger, http.StatusUnauthorized, invalidAPIKeyBody)
	return principal{}, false
}
```

- [ ] **Step 5: Rework `middleware.go`, `router.go`, `session.go`, and the fixture**

`internal/httpapi/middleware.go`:

- Replace the single `const userContextKey contextKey = iota` with a block that will also hold the region key added in Task 6:

```go
const (
	// principalContextKey holds the principal requirePrincipal attached.
	principalContextKey contextKey = iota + 1
)
```

- Rename `requireSession` to `authenticateSession`, returning `(principal, bool)` instead of calling `next`: same cookie/store logic, same 401 messages, but the result is `principal{kind: principalOperator, user: user}` rather than a context write.
- Add `requirePrincipal`:

```go
// requirePrincipal authenticates a request and enforces the route's
// allow-list. It replaces requireSession: the admin API now has three kinds
// of caller, and a route says which it accepts rather than every handler
// re-deriving it.
func (h *authMiddleware) requirePrincipal(allowed principalSet, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var (
			p  principal
			ok bool
		)
		// A present Authorization header means cookies are ignored entirely
		// (design spec section 4.2). r.Header.Values, not Get: a
		// present-but-empty value is a failed bearer attempt, not "absent".
		if values, present := r.Header["Authorization"]; present {
			p, ok = h.authenticateBearer(w, r, values)
		} else {
			p, ok = h.authenticateSession(w, r)
		}
		if !ok {
			return
		}
		if !allowed.has(p.kind) {
			h.deps.Logger.Warn("httpapi: principal not allowed on route",
				"principal", p, "path", r.URL.Path, "method", r.Method)
			writeJSONError(w, h.deps.Logger, http.StatusForbidden, forbiddenBody)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalContextKey, p)))
	})
}
```

- Delete `userFrom`.

`internal/httpapi/router.go`:

- Add to `Deps`, with doc comments in the existing style:

```go
	// APIKeys backs bearer authentication and the region API key routes
	// (design spec section 4.2, 5.6). Nil means bearer auth is not
	// configured: any Authorization header is a 401 and the key routes are
	// not registered. main always sets it.
	APIKeys apikey.Repository
	// BearerFailLimiter bounds FAILED bearer attempts per peer address.
	// NewRouter defaults it (60/minute); successful calls are never
	// charged, so a busy consumer is unmetered.
	BearerFailLimiter *ratelimit.Limiter
```

- In `NewRouter`, inside the `deps.Auth != nil` block: `if deps.BearerFailLimiter == nil { deps.BearerFailLimiter = ratelimit.New(bearerFailuresPerMinute, time.Minute) }`.
- Change `adminRoute`: replace `requiresSession bool` with `allowed principalSet` and document that nil means no principal required.
- Update every entry in `adminRoutes` to carry a set: `nil` for login and logout; `operatorOnly` for whoami, `GET /regions`, `POST .../pushes`, `DELETE .../pushes/{pushId}`; `operatorOrKey` for every alert route, `PATCH /regions/{id}`, `GET .../pushes`, `GET .../push_audience`.
- `registerAdminRoutes` becomes:

```go
	for _, route := range adminRoutes(deps) {
		var h http.Handler = route.handler
		if route.allowed != nil {
			h = mw.requirePrincipal(route.allowed, h)
		}
		mux.Handle(route.pattern, crossSiteGuard(deps.Logger, h))
	}
```

`internal/httpapi/session.go`: `whoami` reads `principalFrom(r.Context())` and writes `user.Username`. Keep the "unreachable through the router" 500 for `!ok`, and add the same 500 when `p.kind != principalOperator` — whoami is `operatorOnly`, so a non-operator reaching it means a route lost its allow-list, and answering 200 with an empty username would hide that.

`internal/httpapi/admin_alerts_test.go`:

- `newAdminFixtureWithDeps` sets `APIKeys: store.APIKeys()` in the `Deps` literal.
- Rename `TestAdminRoutes_EveryRouteRequiresASession` to `TestAdminRoutes_EveryRouteRequiresAPrincipal`, replacing `rt.requiresSession != wantSession` with `(rt.allowed != nil) != wantPrincipal`, and keeping the pinned `sessionFree` map (now `principalFree`) and the pinned route count of 18.
- Add a per-kind 403 walk to the same file:

```go
// TestAdminRoutes_PrincipalAllowLists walks the table with each kind of
// credential and asserts each route answers 403 exactly when the kind is not
// in its allowed set. A route that quietly widened its allow-list -- letting
// a region key send a push, say -- fails here rather than in production.
func TestAdminRoutes_PrincipalAllowLists(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	regionKey := f.mintRegionKey(t, regionPuget)
	servicePrincipal := f.mintPrincipal(t)

	kinds := []struct {
		kind   principalKind
		header string
	}{
		{principalRegionKey, "Bearer " + regionKey},
		{principalService, "Bearer " + servicePrincipal},
	}

	for _, rt := range adminRoutes(f.deps) {
		if rt.allowed == nil {
			continue // login and logout
		}
		method, target := concreteRoute(t, rt.pattern)
		for _, k := range kinds {
			rec := sendBearer(f.handler, method, target, "", k.header)
			forbidden := rec.Code == http.StatusForbidden && bodyText(rec) == `{"error":"forbidden"}`
			if allowed := rt.allowed.has(k.kind); allowed == forbidden {
				t.Errorf("%s %s with %s: status = %d body = %s; allowed = %v",
					method, target, k.kind, rec.Code, rec.Body.String(), allowed)
			}
		}
	}
}
```

`cmd/sidecar/main.go`: add `APIKeys: store.APIKeys(),` to the `httpapi.Deps` literal in `buildDeps`.

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./internal/httpapi/... ./cmd/...`
Expected: PASS.

- [ ] **Step 7: Prove the cookie-fallback test can fail**

Change `requirePrincipal` to fall back to `authenticateSession` when `authenticateBearer` fails, re-run `go test ./internal/httpapi/ -run TestBearer_BeatsCookie`, confirm it fails with 200, then revert.

- [ ] **Step 8: Commit**

```bash
make check
git add internal/httpapi cmd/sidecar
git commit -m "feat(httpapi): principal model and Authorization: Bearer authentication"
```

---

## Task 6: The region becomes a path segment

Every region-scoped admin route moves to `/api/admin/v1/regions/{regionId}/…`. There is **no compatibility shim**: the admin API's only client today is the SPA shipped in the same binary, and the SPA moves in Task 13. Existing `/admin/alerts/{id}` bookmarks break and show the SPA's not-found page.

**Files:**
- Create: `internal/httpapi/region_scope.go`
- Create: `internal/httpapi/region_scope_test.go`
- Modify: `internal/httpapi/router.go`, `internal/httpapi/middleware.go`
- Modify: `internal/httpapi/admin_alerts.go`, `internal/httpapi/admin_regions.go`, `internal/httpapi/admin_pushes.go`
- Modify: `internal/httpapi/admin_alerts_test.go`, `internal/httpapi/admin_regions_test.go`, `internal/httpapi/admin_pushes_test.go`

**Interfaces:**
- Consumes: `principal.canAccessRegion`, `principalFrom`, `requirePrincipal` (Task 5).
- Produces:
  - `routeScope` with `scopeNone`, `scopeRegion`, `scopeKeyAdmin`; `adminRoute.scope routeScope`
  - `func (h *authMiddleware) requireRegion(next http.Handler) http.Handler`
  - `func regionFrom(ctx context.Context) (regions.Region, bool)`
  - `func mustRegion(w http.ResponseWriter, r *http.Request, deps Deps) (regions.Region, bool)` — the accessor handlers use; a missing context region is a logged 500, never a silent zero region
  - `func loadAlert(w http.ResponseWriter, r *http.Request, deps Deps) (alerts.Alert, bool)`
  - `func pathInt64(r *http.Request, name string) (int64, error)` — `pathID` generalized to any wildcard
  - `const regionNotFoundBody = "region not found"`
  - `func adminFeatures(deps Deps) []string`
  - `GET /api/admin/v1/regions/{regionId}` returning the region plus `"features"`

- [ ] **Step 1: Write the failing region-scope tests**

Create `internal/httpapi/region_scope_test.go` (package `httpapi`):

```go
package httpapi

import (
	"net/http"
	"testing"
)

// TestRequireRegion_MalformedSegmentIs404. Deliberately NOT pathID's 400: an
// unparseable region is "no such region", and the response must not differ
// between malformed, not-yours, and does-not-exist -- otherwise the status
// code alone tells a region key which region ids exist.
func TestRequireRegion_MalformedSegmentIs404(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	for _, segment := range []string{"abc", "01", "-1", "+1", "1.0", "1%20", " 1", "999999999999999999999"} {
		rec := f.do(http.MethodGet, "/api/admin/v1/regions/"+segment+"/alerts", "")
		if rec.Code != http.StatusNotFound {
			t.Errorf("region segment %q: status = %d, want 404; body = %s", segment, rec.Code, rec.Body.String())
			continue
		}
		if got, want := bodyText(rec), `{"error":"region not found"}`; got != want {
			t.Errorf("region segment %q: body = %q, want %q", segment, got, want)
		}
	}
}

// TestRequireRegion_UnknownAndForeignAreIndistinguishable is the probing
// defence: a region key must not be able to learn which region ids exist by
// comparing status codes or bodies.
func TestRequireRegion_UnknownAndForeignAreIndistinguishable(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	raw := f.mintRegionKey(t, regionPuget)

	foreign := sendBearer(f.handler, http.MethodGet, "/api/admin/v1/regions/0/alerts", "", "Bearer "+raw)
	unknown := sendBearer(f.handler, http.MethodGet, "/api/admin/v1/regions/9999/alerts", "", "Bearer "+raw)

	if foreign.Code != http.StatusNotFound || unknown.Code != http.StatusNotFound {
		t.Fatalf("foreign = %d, unknown = %d; want 404 for both", foreign.Code, unknown.Code)
	}
	if bodyText(foreign) != bodyText(unknown) {
		t.Errorf("foreign body %q differs from unknown body %q", bodyText(foreign), bodyText(unknown))
	}
}

// TestRequireRegion_OperatorReachesEveryRegion. The cookie is not
// region-scoped; only keys are.
func TestRequireRegion_OperatorReachesEveryRegion(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	for _, id := range []int{regionTampa, regionPuget, regionBare} {
		rec := f.do(http.MethodGet, fmt.Sprintf("/api/admin/v1/regions/%d/alerts", id), "")
		if rec.Code != http.StatusOK {
			t.Errorf("region %d: status = %d, want 200; body = %s", id, rec.Code, rec.Body.String())
		}
	}
}

// TestRequireRegion_InactiveRegionStaysAuthorable. regions.Region.Active is
// the directory's flag and is deliberately not consulted for admin access
// (design spec section 2.7); regionBare is seeded inactive.
func TestRequireRegion_InactiveRegionStaysAuthorable(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	raw := f.mintRegionKey(t, regionBare)
	rec := sendBearer(f.handler, http.MethodGet, "/api/admin/v1/regions/2/alerts", "", "Bearer "+raw)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
}

// TestLoadAlert_ForeignAlertIs404 is what stops /regions/A/alerts/{id-of-B}.
// The id is globally unique, so without the loader the {regionId} segment
// would be decoration.
func TestLoadAlert_ForeignAlertIs404(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	id := f.createAlertIn(t, regionPuget, minimalAlertBody("puget alert"))

	rec := f.do(http.MethodGet, fmt.Sprintf("/api/admin/v1/regions/%d/alerts/%d", regionTampa, id), "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
	}
	if got, want := bodyText(rec), `{"error":"alert not found"}`; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
	// And it must not have been mutated either.
	rec = f.do(http.MethodDelete, fmt.Sprintf("/api/admin/v1/regions/%d/alerts/%d", regionTampa, id), "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("cross-region DELETE: status = %d, want 404", rec.Code)
	}
	if rec := f.do(http.MethodGet, fmt.Sprintf("/api/admin/v1/regions/%d/alerts/%d", regionPuget, id), ""); rec.Code != http.StatusOK {
		t.Errorf("the alert was deleted through the wrong region: status = %d", rec.Code)
	}
}

// TestAdminAlerts_ListIsRegionScoped: the ?region= query filter is gone; the
// path segment is the only region source, and it is not a filter.
func TestAdminAlerts_ListIsRegionScoped(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	f.createAlertIn(t, regionPuget, minimalAlertBody("puget"))
	f.createAlertIn(t, regionTampa, minimalAlertBody("tampa"))

	rec := f.do(http.MethodGet, "/api/admin/v1/regions/1/alerts?region=0", "")
	list := array(t, rec, http.StatusOK)
	if len(list) != 1 {
		t.Fatalf("got %d alerts, want 1 (the query parameter must be ignored)", len(list))
	}
	if got := list[0]["header"]; got != "puget" {
		t.Errorf("header = %v, want puget", got)
	}
}

// TestAdminAlerts_CreateRejectsRegionID. A stale client that still sends
// region_id must not believe it targeted a region; the field is rejected
// rather than ignored (design spec section 5.1).
func TestAdminAlerts_CreateRejectsRegionID(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	rec := f.do(http.MethodPost, "/api/admin/v1/regions/1/alerts", `{
		"region_id": 0, "header": "x", "start_time": "2026-08-15T14:00:00-07:00"
	}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(bodyText(rec), "region_id") {
		t.Errorf("body = %q, want it to name region_id", bodyText(rec))
	}
}

// TestAdminAlerts_CreateLocationCarriesTheRegion pins the Location header's
// new shape, which the SPA follows.
func TestAdminAlerts_CreateLocationCarriesTheRegion(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	rec := f.do(http.MethodPost, "/api/admin/v1/regions/1/alerts", minimalAlertBody("x"))
	got := object(t, rec, http.StatusCreated)
	want := fmt.Sprintf("/api/admin/v1/regions/1/alerts/%d", jsonID(t, got))
	if rec.Header().Get("Location") != want {
		t.Errorf("Location = %q, want %q", rec.Header().Get("Location"), want)
	}
}

// TestGetRegion_ReportsFeatures lets a consumer tell "family not enabled
// here" from a 404 (design spec section 5.1, 5.7).
func TestGetRegion_ReportsFeatures(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t) // alerts, pushes, api_keys, push_registrations wired
	rec := f.do(http.MethodGet, "/api/admin/v1/regions/1", "")
	got := object(t, rec, http.StatusOK)
	if got["name"] != "Puget Sound" {
		t.Errorf("name = %v, want Puget Sound", got["name"])
	}
	// The key is never echoed: oba_api_key is a status word, as on the list.
	if got["oba_api_key"] == nil {
		t.Error("oba_api_key status word missing")
	}
	features := stringSet(t, got["features"])
	for _, want := range []string{"alerts", "pushes", "push_registrations", "api_keys"} {
		if !features[want] {
			t.Errorf("features %v missing %q", got["features"], want)
		}
	}
	for _, absent := range []string{"surveys", "ghost_bus_reports", "alarms"} {
		if features[absent] {
			t.Errorf("features %v contains %q, which is not wired in this fixture", got["features"], absent)
		}
	}
}

// TestRouteTable_ScopeAgreesWithPattern is the invariant that keeps handlers
// from parsing a region by hand: a pattern with {regionId} must be scoped,
// and a scoped route must have the segment.
func TestRouteTable_ScopeAgreesWithPattern(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	for _, rt := range adminRoutes(f.deps) {
		hasSegment := strings.Contains(rt.pattern, "{regionId}")
		if hasSegment != (rt.scope != scopeNone) {
			t.Errorf("route %q: has {regionId} = %v but scope = %v", rt.pattern, hasSegment, rt.scope)
		}
	}
}

// TestRouteTable_TenancyWalk calls every scoped route with a region-A key
// against fixtures created in region B. Reads are 404 (or an empty list);
// writes are 404 and change nothing. A route added to adminRoutes without a
// fixture entry fails here, which is what keeps this walk complete as
// families are added.
func TestRouteTable_TenancyWalk(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	keyForA := f.mintRegionKey(t, regionPuget) // region A = 1
	fx := f.seedTenancyFixtures(t)             // creates everything in region B = 0

	for _, rt := range adminRoutes(f.deps) {
		if rt.scope == scopeNone {
			continue
		}
		spec, ok := tenancyFixtures[rt.pattern]
		if !ok {
			t.Errorf("route %q has no tenancyFixtures entry; add one so the walk stays complete", rt.pattern)
			continue
		}
		method, target := f.tenancyTarget(t, rt.pattern, fx)
		rec := sendBearer(f.handler, method, target, spec.body(fx), "Bearer "+keyForA)
		if spec.wantEmptyList {
			list := array(t, rec, http.StatusOK)
			if len(list) != 0 {
				t.Errorf("%s %s: returned %d region-B rows to a region-A key", method, target, len(list))
			}
			continue
		}
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s with a region-A key against region-B data: status = %d, want 404; body = %s",
				method, target, rec.Code, rec.Body.String())
		}
	}
}
```

Add the supporting test scaffolding to `admin_alerts_test.go` (or a new `tenancy_test.go`):

```go
// tenancySpec says how the tenancy walk should call one route.
type tenancySpec struct {
	// wantEmptyList marks a collection route: it answers 200 with an empty
	// array rather than 404, because the region itself IS the caller's.
	wantEmptyList bool
	// body builds the request body, given the region-B fixture ids. Nil
	// means no body.
	bodyFor func(fx tenancyFixtureIDs) string
}

func (s tenancySpec) body(fx tenancyFixtureIDs) string {
	if s.bodyFor == nil {
		return ""
	}
	return s.bodyFor(fx)
}

// tenancyFixtureIDs are the resources seeded in region B that the walk tries
// to reach through region A.
type tenancyFixtureIDs struct {
	alertID    int64
	pushID     int64
	studyID    int64
	surveyID   int64
	responseID string
	reportID   string
	alarmID    int64
	keyID      int64
}

// tenancyFixtures is keyed by route pattern. Every scoped route needs an
// entry; TestRouteTable_TenancyWalk fails on a missing one.
var tenancyFixtures = map[string]tenancySpec{
	"GET /api/admin/v1/regions/{regionId}":       {},
	"PATCH /api/admin/v1/regions/{regionId}":     {bodyFor: func(tenancyFixtureIDs) string { return `{"default_agency_id":"x"}` }},
	"GET /api/admin/v1/regions/{regionId}/alerts": {wantEmptyList: true},
	// ... one entry per scoped route; later tasks add theirs.
}
```

`f.tenancyTarget` substitutes the region-A id into `{regionId}` and the region-B fixture id into every other wildcard — that combination (my region, your resource) is precisely what must 404.

Note on the walk's `{regionId}`: the walk uses **region A** in the path, because the key is region A's, so `requireRegion` passes and the loader is what must refuse. A walk that put region B in the path would only prove `canAccessRegion` works, which is the weaker of the two fences.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/httpapi/ -run 'TestRequireRegion|TestLoadAlert|TestRouteTable|TestGetRegion'`
Expected: FAIL — the routes 404 at the mux (they are still at `/api/admin/v1/alerts`), `scopeNone` undefined.

- [ ] **Step 3: Write `internal/httpapi/region_scope.go`**

```go
package httpapi

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strconv"

	"github.com/OneBusAway/sidecar/internal/alerts"
	"github.com/OneBusAway/sidecar/internal/regions"
)

// routeScope is which region middleware an admin route carries.
type routeScope int

const (
	// scopeNone is a route with no {regionId} segment.
	scopeNone routeScope = iota
	// scopeRegion is the ordinary tenancy fence: the principal must be able
	// to access the path's region.
	scopeRegion
	// scopeKeyAdmin is the key-management family only. It grants operators
	// and service principals access without consulting canAccessRegion, and
	// is a separate scope so a service principal's reach is visible in one
	// place and assertable by the route-table test.
	scopeKeyAdmin
)

// regionNotFoundBody is the ONE body every region-scoping failure returns.
// Malformed, unknown, and "not yours" must be indistinguishable, or the
// status code becomes an oracle a region key can use to enumerate regions.
const regionNotFoundBody = "region not found"

// adminRegionSegment is the exact grammar of {regionId}: no leading zeros,
// no sign, no whitespace. It deliberately differs from the rider feed's
// lenient ParseRegionSegment, which the admin API does not reuse.
var adminRegionSegment = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)

// requireRegion is the tenancy fence for every region-scoped route except
// the key-management family. It parses {regionId}, checks the principal,
// loads the region, and stores it in the context. Handlers read it from
// there and never fetch a region themselves -- which is what makes tenancy
// one check rather than one per handler.
func (h *authMiddleware) requireRegion(next http.Handler) http.Handler {
	return h.scopedRegion(next, false)
}

// requireKeyAdminRegion is requireRegion for the .../api_keys family. It
// parses and loads the region the same way but grants access to operators
// and service principals without consulting canAccessRegion. It is a
// separate function so the service principal's reach is confined to one
// visible place; the route-table test asserts it is applied to exactly the
// patterns ending in /api_keys or /api_keys/{keyId}.
func (h *authMiddleware) requireKeyAdminRegion(next http.Handler) http.Handler {
	return h.scopedRegion(next, true)
}

func (h *authMiddleware) scopedRegion(next http.Handler, keyAdmin bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := principalFrom(r.Context())
		if !ok {
			// Unreachable through the router: requirePrincipal always runs
			// first. Reaching it means a route lost its middleware, and
			// serving the request would be worse than failing loudly.
			serverErrorJSON(w, h.deps.Logger, "region scope reached without a principal",
				errors.New("no principal on request context"))
			return
		}

		raw := r.PathValue("regionId")
		if !adminRegionSegment.MatchString(raw) {
			h.regionNotFound(w, r, "malformed region segment")
			return
		}
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			// Only reachable for a digit string too long for int64.
			h.regionNotFound(w, r, "region segment out of range")
			return
		}

		allowed := p.canAccessRegion(id)
		if keyAdmin {
			allowed = p.kind == principalOperator || p.kind == principalService
		}
		if !allowed {
			h.regionNotFound(w, r, "principal may not access this region")
			return
		}

		region, err := h.deps.Regions.Get(r.Context(), id)
		if err != nil {
			if errors.Is(err, regions.ErrNotFound) {
				h.regionNotFound(w, r, "no such region")
				return
			}
			serverErrorJSON(w, h.deps.Logger, "get region", err)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), regionContextKey, region)))
	})
}

// regionNotFound writes the single 404 and logs why. The client never sees
// the reason; that asymmetry is the point.
func (h *authMiddleware) regionNotFound(w http.ResponseWriter, r *http.Request, reason string) {
	h.deps.Logger.Debug("httpapi: region scope refused",
		"reason", reason, "segment", r.PathValue("regionId"), "path", r.URL.Path)
	writeJSONError(w, h.deps.Logger, http.StatusNotFound, regionNotFoundBody)
}

// regionFrom returns the region requireRegion loaded for this request.
func regionFrom(ctx context.Context) (regions.Region, bool) {
	region, ok := ctx.Value(regionContextKey).(regions.Region)
	return region, ok
}

// mustRegion is regionFrom for handlers: a missing context region means a
// route lost its scope middleware, which is a 500 rather than a silent fall
// back to region 0 (a real region).
func mustRegion(w http.ResponseWriter, r *http.Request, deps Deps) (regions.Region, bool) {
	region, ok := regionFrom(r.Context())
	if !ok {
		serverErrorJSON(w, deps.Logger, "handler reached without a scoped region",
			errors.New("no region on request context"))
		return regions.Region{}, false
	}
	return region, true
}

// pathInt64 parses an integer path wildcard, returning a caller-safe message
// the HTTP layer maps to 400. pathID is this with name = "id".
func pathInt64(r *http.Request, name string) (int64, error) {
	raw := r.PathValue(name)
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q: must be an integer", name, raw)
	}
	return v, nil
}

// loadAlert resolves {id} within the request's region. An alert in another
// region is the same 404 as one that does not exist: alert ids are globally
// unique, so this loader is what makes the {regionId} segment a fence rather
// than decoration.
func loadAlert(w http.ResponseWriter, r *http.Request, deps Deps) (alerts.Alert, bool) {
	region, ok := mustRegion(w, r, deps)
	if !ok {
		return alerts.Alert{}, false
	}
	id, err := pathID(r)
	if err != nil {
		writeJSONError(w, deps.Logger, http.StatusBadRequest, err.Error())
		return alerts.Alert{}, false
	}
	a, err := deps.Alerts.Get(r.Context(), id)
	if err != nil {
		writeStoreError(w, deps.Logger, "get alert", err)
		return alerts.Alert{}, false
	}
	if a.RegionID != region.ID {
		writeJSONError(w, deps.Logger, http.StatusNotFound, "alert not found")
		return alerts.Alert{}, false
	}
	return a, true
}
```

Add `regionContextKey` to the `contextKey` block in `middleware.go`.

- [ ] **Step 4: Move the routes and rework the handlers**

`router.go`:

- `adminRoute` gains `scope routeScope`.
- Rewrite the table (patterns only shown; `allowed` from Task 5 carries over):

```
POST   /api/admin/v1/session                                            nil            scopeNone
DELETE /api/admin/v1/session                                            nil            scopeNone
GET    /api/admin/v1/session                                            operatorOnly   scopeNone
GET    /api/admin/v1/regions                                            operatorOnly   scopeNone
GET    /api/admin/v1/regions/{regionId}                                 operatorOrKey  scopeRegion
PATCH  /api/admin/v1/regions/{regionId}                                 operatorOrKey  scopeRegion
GET    /api/admin/v1/regions/{regionId}/alerts                          operatorOrKey  scopeRegion
POST   /api/admin/v1/regions/{regionId}/alerts                          operatorOrKey  scopeRegion
GET    /api/admin/v1/regions/{regionId}/alerts/{id}                     operatorOrKey  scopeRegion
PATCH  /api/admin/v1/regions/{regionId}/alerts/{id}                     operatorOrKey  scopeRegion
DELETE /api/admin/v1/regions/{regionId}/alerts/{id}                     operatorOrKey  scopeRegion
POST   /api/admin/v1/regions/{regionId}/alerts/{id}/publish             operatorOrKey  scopeRegion
POST   /api/admin/v1/regions/{regionId}/alerts/{id}/unpublish           operatorOrKey  scopeRegion
PUT    /api/admin/v1/regions/{regionId}/alerts/{id}/translations/{lang} operatorOrKey  scopeRegion
DELETE /api/admin/v1/regions/{regionId}/alerts/{id}/translations/{lang} operatorOrKey  scopeRegion
POST   /api/admin/v1/regions/{regionId}/alerts/{id}/pushes              operatorOnly   scopeRegion
GET    /api/admin/v1/regions/{regionId}/alerts/{id}/pushes              operatorOrKey  scopeRegion
DELETE /api/admin/v1/regions/{regionId}/alerts/{id}/pushes/{pushId}     operatorOnly   scopeRegion
GET    /api/admin/v1/regions/{regionId}/alerts/{id}/push_audience       operatorOrKey  scopeRegion
```

- `registerAdminRoutes` composes `crossSiteGuard(requirePrincipal(allowed, [requireRegion|requireKeyAdminRegion,] handler))` — the principal middleware is OUTSIDE the region middleware, because the region check reads the principal.
- Add `adminFeatures`:

```go
// adminFeatures lists the admin route families registered in this
// deployment (design spec section 5.1), so a consumer can tell "family not
// enabled here" from a 404. It is derived from the same Deps fields the
// route table gates on, so the two cannot drift.
func adminFeatures(deps Deps) []string {
	features := []string{"alerts"}
	if alertPushRoutesEnabled(deps) {
		features = append(features, "pushes")
	}
	if deps.Surveys != nil {
		features = append(features, "surveys")
	}
	if deps.GhostBus != nil {
		features = append(features, "ghost_bus_reports")
	}
	if deps.Alarms != nil {
		features = append(features, "alarms")
	}
	if deps.PushRegs != nil {
		features = append(features, "push_registrations")
	}
	if deps.APIKeys != nil {
		features = append(features, "api_keys")
	}
	return features
}
```

`admin_alerts.go`:

- `list` drops the `?region=` parsing entirely and calls `h.deps.Alerts.List(ctx, alerts.ListFilter{RegionID: &region.ID})` with the region from `mustRegion`. Update the doc comment: the region is no longer a filter, it is the resource's scope.
- `createAlertRequest.RegionID` stays as a field but only so it can be rejected. Replace the "region_id is required" check with:

```go
	if req.RegionID != nil {
		// Rejected, not ignored: a stale client that still sends region_id
		// must not believe it targeted a region (design spec section 5.1).
		writeJSONError(w, h.deps.Logger, http.StatusBadRequest,
			"region_id is not accepted; the region comes from the path")
		return
	}
```

and take the region from `mustRegion`.
- The `Location` header becomes `fmt.Sprintf("/api/admin/v1/regions/%d/alerts/%d", region.ID, created.ID)`.
- `get`, `patch`, `delete`, `setPublished`, `putTranslation`, `deleteTranslation` all start from `loadAlert` instead of `pathID` + `Alerts.Get`. `patch` and `putTranslation` already needed the current alert, so they use the loaded value rather than re-reading. `respondWithAlert` keeps re-reading by id — it runs after the loader has already fenced the region.
- `patch` no longer refetches the region for timestamp errors: it uses the context region (the alert is in it by construction).

`admin_regions.go`:

- `patch` takes the region from `mustRegion` instead of `pathID` + `Regions.Get`, and keeps its current 400 validation codes.
- Add `get`:

```go
// get handles GET /api/admin/v1/regions/{regionId}. It is the one region
// endpoint a region key may call, and it carries "features" so a consumer
// can distinguish "this family is not enabled here" from a 404 on a route
// that was never registered (design spec section 5.1).
func (h *adminRegionsHandler) get(w http.ResponseWriter, r *http.Request) {
	region, ok := mustRegion(w, r, h.deps)
	if !ok {
		return
	}
	writeJSON(w, h.deps.Logger, http.StatusOK, regionDetailJSON{
		regionJSON: toRegionJSON(region, h.deps.OBADefaultKeySet),
		Features:   adminFeatures(h.deps),
	})
}

// regionDetailJSON is regionJSON plus the deployment's feature list. It is
// only ever returned by the single-region endpoint; the list endpoint keeps
// the flat shape, because features are a property of the deployment rather
// than of any one region.
type regionDetailJSON struct {
	regionJSON
	Features []string `json:"features"`
}
```

`admin_pushes.go`: `create`, `list`, `cancel`, and `audience` call `loadAlert` instead of `pathID`. `cancel` additionally asserts `p.RegionID == region.ID` alongside its existing `p.AlertID == id` check — belt and braces, since a push row carries its own region and the spec calls for that assertion explicitly.

- [ ] **Step 5: Update the existing handler tests to the new paths**

First, the shared helpers the new tests above assume. Add these to `admin_alerts_test.go` beside the ones already there:

```go
// minimalAlertBody is minimalAlert without region_id, which the create body
// no longer accepts. minimalAlert stays, for the tests that deliberately
// send the now-rejected field.
func minimalAlertBody(header string) string {
	return fmt.Sprintf(`{"header":%q,"start_time":"2026-08-15T14:00:00-07:00"}`, header)
}

// createAlertIn posts an alert to one region and returns its id. It replaces
// createAlertID, whose target path had no region in it.
func (f *adminFixture) createAlertIn(t *testing.T, regionID int64, body string) int64 {
	t.Helper()
	rec := f.do(http.MethodPost, fmt.Sprintf("/api/admin/v1/regions/%d/alerts", regionID), body)
	return jsonID(t, object(t, rec, http.StatusCreated))
}

// stringSet turns a JSON array of strings into a set, so a features
// assertion does not depend on the order the server happened to build it in.
func stringSet(t *testing.T, v any) map[string]bool {
	t.Helper()
	raw, ok := v.([]any)
	if !ok {
		t.Fatalf("expected a JSON array, got %#v", v)
	}
	out := make(map[string]bool, len(raw))
	for _, item := range raw {
		str, isString := item.(string)
		if !isString {
			t.Fatalf("expected a string in the array, got %#v", item)
		}
		out[str] = true
	}
	return out
}
```

Then the mechanical sweep. In `admin_alerts_test.go`, `admin_regions_test.go`, `admin_pushes_test.go`:

- Add `func alertPath(regionID, id int64, suffix string) string` (or update the existing `alertPath`) to build `/api/admin/v1/regions/{regionID}/alerts/{id}{suffix}`, and route every existing call through it.
- Replace `/api/admin/v1/alerts` with `/api/admin/v1/regions/{n}/alerts`.
- Delete the `?region=` filter tests and the "region_id is required" test; replace them with the two new tests from Step 1.
- Extend `concreteRoute` to fill `{regionId}` (use `"1"`), `{publicId}`, and `{keyId}`.
- Update the pinned route count in `TestAdminRoutes_EveryRouteRequiresAPrincipal` from 18 to **19** (`GET /regions/{regionId}` is new), and update its inline arithmetic comment.

- [ ] **Step 6: Run to verify it passes**

Run: `go test ./internal/httpapi/...`
Expected: PASS.

- [ ] **Step 7: Prove the loader can fail**

Delete the `a.RegionID != region.ID` check in `loadAlert`, re-run `go test ./internal/httpapi/ -run 'TestLoadAlert|TestRouteTable_TenancyWalk'`, confirm both fail, then restore.

- [ ] **Step 8: Commit**

```bash
make check
git add internal/httpapi
git commit -m "feat(httpapi): move every region-scoped admin route under /regions/{regionId}"
```

---

## Task 7: Region API key routes

**Files:**
- Create: `internal/httpapi/admin_apikeys.go`
- Create: `internal/httpapi/admin_apikeys_test.go`
- Modify: `internal/httpapi/router.go` (three routes, `scopeKeyAdmin`)
- Modify: `internal/httpapi/region_scope.go` (nothing new; `requireKeyAdminRegion` already exists from Task 6)
- Modify: `internal/httpapi/admin_alerts_test.go` (route count, `scopeKeyAdmin` invariants)

**Interfaces:**
- Consumes: `apikey.NewRegionKey`, `apikey.Repository` (Tasks 1–2); `requireKeyAdminRegion`, `mustRegion`, `pathInt64` (Task 6); `principal.actor()`, `operatorOrService` (Task 5).
- Produces:
  - `POST /api/admin/v1/regions/{regionId}/api_keys`
  - `GET /api/admin/v1/regions/{regionId}/api_keys`
  - `DELETE /api/admin/v1/regions/{regionId}/api_keys/{keyId}`
  - `func StripControlChars(s string) string` exported from `internal/regions` (the guard `internal/regions` already has unexported)

- [ ] **Step 1: Export the control-character guard**

`internal/regions/directory.go` already has `stripControlChars`. Rename it to `StripControlChars` with an exported doc comment and update its two call sites; leave `isASCIIControl` unexported.

```go
// StripControlChars removes ASCII control characters (0x00-0x1F and the
// 0x7F DEL) from s, leaving everything else -- including non-ASCII text --
// untouched. It guards every string that arrives from outside and later
// reaches an operator's terminal: directory names, and the name on a region
// API key, which a compromised service principal controls and which
// `sidecar-admin key list` prints to the terminal of the operator
// investigating that compromise.
func StripControlChars(s string) string {
```

- [ ] **Step 2: Write the failing tests**

Create `internal/httpapi/admin_apikeys_test.go` (package `httpapi`):

```go
package httpapi

// TestAPIKeys_MintReturnsTheRawKeyOnce pins the whole 201 contract. The raw
// key appears here and nowhere else: not in a Location header, not in a URL,
// not in a log line.
func TestAPIKeys_MintReturnsTheRawKeyOnce(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	f := newAdminFixtureWithDeps(t, func(d *Deps) {
		d.Logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	})

	rec := f.do(http.MethodPost, "/api/admin/v1/regions/1/api_keys", `{"name":"obacloud prod"}`)
	got := object(t, rec, http.StatusCreated)
	assertKeys(t, "api key", got, []string{"id", "name", "key", "created_by", "created_at"})

	raw, _ := got["key"].(string)
	if !strings.HasPrefix(raw, "obask_1_") {
		t.Fatalf("key = %q, want an obask_1_ prefix", raw)
	}
	if rec.Header().Get("Location") != "" {
		t.Error("a Location header would put the raw key in a URL")
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", rec.Header().Get("Cache-Control"))
	}
	if got["name"] != "obacloud prod" {
		t.Errorf("name = %v", got["name"])
	}
	createdBy, _ := got["created_by"].(map[string]any)
	if createdBy["kind"] != "operator" {
		t.Errorf("created_by = %v, want kind operator", got["created_by"])
	}
	if strings.Contains(buf.String(), raw) || strings.Contains(buf.String(), strings.TrimPrefix(raw, "obask_1_")) {
		t.Errorf("the raw key reached a log line:\n%s", buf.String())
	}

	// The key it returned actually works, and only for its own region.
	if rec := sendBearer(f.handler, http.MethodGet, "/api/admin/v1/regions/1/alerts", "", "Bearer "+raw); rec.Code != http.StatusOK {
		t.Errorf("minted key against its own region: status = %d, want 200", rec.Code)
	}
	if rec := sendBearer(f.handler, http.MethodGet, "/api/admin/v1/regions/0/alerts", "", "Bearer "+raw); rec.Code != http.StatusNotFound {
		t.Errorf("minted key against another region: status = %d, want 404", rec.Code)
	}
}

// TestAPIKeys_NameValidation. The name is 1-100 BYTES after stripping
// control characters and trimming; a compromised principal controls this
// string, and `key list` prints it to a terminal.
func TestAPIKeys_NameValidation(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	for _, tc := range []struct {
		name string
		body string
		want int
	}{
		{"blank", `{"name":""}`, http.StatusUnprocessableEntity},
		{"whitespace only", `{"name":"   "}`, http.StatusUnprocessableEntity},
		{"control chars only", "{\"name\":\"\\u0007\\u0007\"}", http.StatusUnprocessableEntity},
		{"missing", `{}`, http.StatusUnprocessableEntity},
		{"101 bytes", `{"name":"` + strings.Repeat("a", 101) + `"}`, http.StatusUnprocessableEntity},
		{"100 bytes", `{"name":"` + strings.Repeat("a", 100) + `"}`, http.StatusCreated},
		// 100 bytes is a BYTE cap, not a rune cap: 34 three-byte runes fit,
		// 35 do not.
		{"34 three-byte runes", `{"name":"` + strings.Repeat("éé", 0) + strings.Repeat("中", 33) + `"}`, http.StatusCreated},
		{"malformed json", `{`, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := f.do(http.MethodPost, "/api/admin/v1/regions/1/api_keys", tc.body)
			if rec.Code != tc.want {
				t.Errorf("status = %d, want %d; body = %s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

// TestAPIKeys_NameControlCharsAreStripped.
func TestAPIKeys_NameControlCharsAreStripped(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	// A name carrying an ANSI escape would repaint the terminal of whoever
	// runs `key list` -- which, after a principal compromise, is exactly the
	// operator trying to read the list.
	rec := f.do(http.MethodPost, "/api/admin/v1/regions/1/api_keys",
		"{\"name\":\"ob\\u001b[2Jacloud\"}")
	got := object(t, rec, http.StatusCreated)
	if got["name"] != "ob[2Jacloud" {
		t.Errorf("name = %q, want the escape byte stripped", got["name"])
	}
}

// TestAPIKeys_ListNeverEchoesTheKey.
func TestAPIKeys_ListNeverEchoesTheKey(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	mint := object(t, f.do(http.MethodPost, "/api/admin/v1/regions/1/api_keys", `{"name":"a"}`), http.StatusCreated)
	raw, _ := mint["key"].(string)

	list := array(t, f.do(http.MethodGet, "/api/admin/v1/regions/1/api_keys", ""), http.StatusOK)
	if len(list) != 1 {
		t.Fatalf("got %d keys, want 1", len(list))
	}
	assertKeys(t, "api key", list[0],
		[]string{"id", "name", "created_by", "created_at", "last_used_at", "revoked_at", "revoked_by"})
	if strings.Contains(bodyText(f.do(http.MethodGet, "/api/admin/v1/regions/1/api_keys", "")), raw) {
		t.Error("the list echoed the raw key")
	}
}

// TestAPIKeys_RevokeIsRegionScopedAndIdempotent.
func TestAPIKeys_RevokeIsRegionScopedAndIdempotent(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	mint := object(t, f.do(http.MethodPost, "/api/admin/v1/regions/1/api_keys", `{"name":"a"}`), http.StatusCreated)
	id := jsonID(t, mint)
	raw, _ := mint["key"].(string)

	// Another region's key id is a 404, not a successful revoke.
	if rec := f.do(http.MethodDelete, fmt.Sprintf("/api/admin/v1/regions/0/api_keys/%d", id), ""); rec.Code != http.StatusNotFound {
		t.Errorf("cross-region revoke: status = %d, want 404", rec.Code)
	}
	if rec := sendBearer(f.handler, http.MethodGet, "/api/admin/v1/regions/1/alerts", "", "Bearer "+raw); rec.Code != http.StatusOK {
		t.Fatalf("the key must still be live: status = %d", rec.Code)
	}

	for i := 0; i < 2; i++ {
		if rec := f.do(http.MethodDelete, fmt.Sprintf("/api/admin/v1/regions/1/api_keys/%d", id), ""); rec.Code != http.StatusNoContent {
			t.Fatalf("revoke %d: status = %d, want 204", i, rec.Code)
		}
	}
	if rec := sendBearer(f.handler, http.MethodGet, "/api/admin/v1/regions/1/alerts", "", "Bearer "+raw); rec.Code != http.StatusUnauthorized {
		t.Errorf("revoked key: status = %d, want 401", rec.Code)
	}
	if rec := f.do(http.MethodDelete, "/api/admin/v1/regions/1/api_keys/99999", ""); rec.Code != http.StatusNotFound {
		t.Errorf("unknown key id: status = %d, want 404", rec.Code)
	}
	if rec := f.do(http.MethodDelete, "/api/admin/v1/regions/1/api_keys/abc", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("unparseable key id: status = %d, want 400", rec.Code)
	}
}

// TestAPIKeys_ServicePrincipalCanMintAnywhereButReadNothing is the whole
// point of the separate scope: the principal's reach is key management in
// every region, and nothing else anywhere.
func TestAPIKeys_ServicePrincipalCanMintAnywhereButReadNothing(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	sp := f.mintPrincipal(t)

	for _, region := range []string{"0", "1", "2"} {
		rec := sendBearer(f.handler, http.MethodPost, "/api/admin/v1/regions/"+region+"/api_keys",
			`{"name":"obacloud"}`, "Bearer "+sp)
		got := object(t, rec, http.StatusCreated)
		createdBy, _ := got["created_by"].(map[string]any)
		if createdBy["kind"] != "principal" {
			t.Errorf("region %s: created_by = %v, want kind principal", region, got["created_by"])
		}
	}
	// A region that is not in the directory cannot be provisioned: regions
	// come from OBACloud's own export, so a principal can only mint keys for
	// regions OBACloud has published (design spec section 5.6).
	if rec := sendBearer(f.handler, http.MethodPost, "/api/admin/v1/regions/9999/api_keys",
		`{"name":"x"}`, "Bearer "+sp); rec.Code != http.StatusNotFound {
		t.Errorf("unpublished region: status = %d, want 404", rec.Code)
	}
	// And it can read no tenant data.
	if rec := sendBearer(f.handler, http.MethodGet, "/api/admin/v1/regions/1/alerts", "", "Bearer "+sp); rec.Code != http.StatusForbidden {
		t.Errorf("principal reading alerts: status = %d, want 403", rec.Code)
	}
}

// TestAPIKeys_RegionKeyCannotReachKeyManagement: a leaked region key must
// not be able to propagate (design spec section 2.2).
func TestAPIKeys_RegionKeyCannotReachKeyManagement(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	raw := f.mintRegionKey(t, regionPuget)
	for _, tc := range []struct{ method, target, body string }{
		{http.MethodPost, "/api/admin/v1/regions/1/api_keys", `{"name":"x"}`},
		{http.MethodGet, "/api/admin/v1/regions/1/api_keys", ""},
		{http.MethodDelete, "/api/admin/v1/regions/1/api_keys/1", ""},
	} {
		rec := sendBearer(f.handler, tc.method, tc.target, tc.body, "Bearer "+raw)
		if rec.Code != http.StatusForbidden || bodyText(rec) != `{"error":"forbidden"}` {
			t.Errorf("%s %s: status = %d body = %s, want 403 forbidden", tc.method, tc.target, rec.Code, rec.Body.String())
		}
	}
}

// TestRouteTable_KeyAdminScopeIsExactlyTheAPIKeyFamily. scopeKeyAdmin is the
// one middleware that lets a service principal past a region, so the set of
// routes carrying it must be pinned rather than trusted.
func TestRouteTable_KeyAdminScopeIsExactlyTheAPIKeyFamily(t *testing.T) {
	t.Parallel()

	f := newAdminFixture(t)
	for _, rt := range adminRoutes(f.deps) {
		_, path, _ := strings.Cut(rt.pattern, " ")
		isKeyFamily := strings.HasSuffix(path, "/api_keys") || strings.HasSuffix(path, "/api_keys/{keyId}")
		if (rt.scope == scopeKeyAdmin) != isKeyFamily {
			t.Errorf("route %q: scope = %v, api_keys family = %v", rt.pattern, rt.scope, isKeyFamily)
		}
		if rt.allowed.has(principalService) && rt.scope != scopeKeyAdmin {
			t.Errorf("route %q allows a service principal outside scopeKeyAdmin", rt.pattern)
		}
	}
}
```

- [ ] **Step 3: Run to verify it fails**

Run: `go test ./internal/httpapi/ -run 'TestAPIKeys|TestRouteTable_KeyAdmin'`
Expected: FAIL — routes not registered (404 / 405).

- [ ] **Step 4: Write `internal/httpapi/admin_apikeys.go`**

```go
package httpapi

// maxKeyNameBytes is the cap on a key's name, in BYTES rather than runes:
// the value is a display label with no structure, and a byte cap is the one
// a storage layer can also enforce.
const maxKeyNameBytes = 100

// actorJSON is who minted or revoked a key. Kind and id rather than a
// resolved username: the columns are deliberately not foreign keys, so the
// referenced row may be gone.
type actorJSON struct {
	Kind string `json:"kind"`
	ID   int64  `json:"id"`
}

// apiKeyJSON is one key as listed. There is no `key` field: the raw value
// exists only in the mint response.
type apiKeyJSON struct {
	ID         int64      `json:"id"`
	Name       string     `json:"name"`
	CreatedBy  actorJSON  `json:"created_by"`
	CreatedAt  string     `json:"created_at"`
	LastUsedAt *string    `json:"last_used_at"`
	RevokedAt  *string    `json:"revoked_at"`
	RevokedBy  *actorJSON `json:"revoked_by"`
}

// mintedKeyJSON is the 201 body, and the only place the raw key is ever
// written. It is a separate type from apiKeyJSON so "the shape with the
// secret in it" cannot be reused by accident on a list route.
type mintedKeyJSON struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Key       string    `json:"key"`
	CreatedBy actorJSON `json:"created_by"`
	CreatedAt string    `json:"created_at"`
}

type createKeyRequest struct {
	Name string `json:"name"`
}

type adminAPIKeysHandler struct{ deps Deps }

// create handles POST /api/admin/v1/regions/{regionId}/api_keys.
//
// No Location header and Cache-Control: no-store, both because the body
// carries a live credential: a Location header would put a key-shaped
// resource in a URL, and a cached 201 would leave the secret in a proxy.
func (h *adminAPIKeysHandler) create(w http.ResponseWriter, r *http.Request) {
	region, ok := mustRegion(w, r, h.deps)
	if !ok {
		return
	}
	var req createKeyRequest
	if err := decodeJSON(w, r, maxAdminBody, &req); err != nil {
		writeJSONError(w, h.deps.Logger, http.StatusBadRequest, err.Error())
		return
	}
	// Strip first, then trim: a name that is only control characters must
	// come out empty rather than passing a length check on invisible bytes.
	name := strings.TrimSpace(regions.StripControlChars(req.Name))
	if name == "" || len(name) > maxKeyNameBytes {
		writeJSONError(w, h.deps.Logger, http.StatusUnprocessableEntity,
			fmt.Sprintf("name must be 1-%d bytes after trimming", maxKeyNameBytes))
		return
	}
	p, _ := principalFrom(r.Context()) // guaranteed by requirePrincipal

	raw, hash, err := apikey.NewRegionKey(region.ID)
	if err != nil {
		serverErrorJSON(w, h.deps.Logger, "mint region api key", err)
		return
	}
	created, err := h.deps.APIKeys.CreateRegionKey(r.Context(), region.ID, name, hash, p.actor(), h.deps.Now())
	if err != nil {
		serverErrorJSON(w, h.deps.Logger, "create region api key", err)
		return
	}
	h.deps.Logger.Info("httpapi: minted region api key", "key", created, "principal", p)

	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, h.deps.Logger, http.StatusCreated, mintedKeyJSON{
		ID: created.ID, Name: created.Name, Key: raw,
		CreatedBy: actorJSON{Kind: created.CreatedBy.Kind, ID: created.CreatedBy.ID},
		CreatedAt: formatInstant(created.CreatedAt),
	})
}

// list handles GET .../api_keys, live and revoked, newest first.
func (h *adminAPIKeysHandler) list(w http.ResponseWriter, r *http.Request) { ... }

// revoke handles DELETE .../api_keys/{keyId}. 204 for a live key and for one
// already revoked; 404 for an unknown id or an id in another region -- the
// repository's region-scoped RevokeRegionKey is the fence, so there is no
// load-then-compare here to get wrong.
func (h *adminAPIKeysHandler) revoke(w http.ResponseWriter, r *http.Request) {
	region, ok := mustRegion(w, r, h.deps)
	if !ok {
		return
	}
	id, err := pathInt64(r, "keyId")
	if err != nil {
		writeJSONError(w, h.deps.Logger, http.StatusBadRequest, err.Error())
		return
	}
	p, _ := principalFrom(r.Context())
	switch err := h.deps.APIKeys.RevokeRegionKey(r.Context(), region.ID, id, p.actor(), h.deps.Now()); {
	case err == nil:
		h.deps.Logger.Info("httpapi: revoked region api key",
			"key_id", id, "region_id", region.ID, "principal", p)
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, apikey.ErrNotFound):
		writeJSONError(w, h.deps.Logger, http.StatusNotFound, "api key not found")
	default:
		serverErrorJSON(w, h.deps.Logger, "revoke region api key", err)
	}
}
```

`list` maps each `apikey.RegionKey` to `apiKeyJSON` with `formatInstant` on `CreatedAt` and optional-pointer formatting for `LastUsedAt` / `RevokedAt`; `make`, not nil, so an empty result marshals as `[]`.

- [ ] **Step 5: Register the routes**

In `adminRoutes`, gated on `deps.APIKeys != nil`:

```go
	if deps.APIKeys != nil {
		keysAdmin := &adminAPIKeysHandler{deps: deps}
		routes = append(routes,
			adminRoute{"POST /api/admin/v1/regions/{regionId}/api_keys", keysAdmin.create, operatorOrService, scopeKeyAdmin},
			adminRoute{"GET /api/admin/v1/regions/{regionId}/api_keys", keysAdmin.list, operatorOrService, scopeKeyAdmin},
			adminRoute{"DELETE /api/admin/v1/regions/{regionId}/api_keys/{keyId}", keysAdmin.revoke, operatorOrService, scopeKeyAdmin},
		)
	}
```

Update the pinned route count to **22** and add the three `tenancyFixtures` entries. For the key family the walk's expectations differ: a region key is refused with **403** by the allow-list before tenancy is ever reached, so mark those three entries so the walk skips them and let `TestAPIKeys_RegionKeyCannotReachKeyManagement` cover them instead. Add a `skipTenancyWalk bool` to `tenancySpec` with that reason in its doc comment.

- [ ] **Step 6: Run to verify it passes**

Run: `go test ./internal/httpapi/...`
Expected: PASS.

- [ ] **Step 7: Prove the scope test can fail**

Change one `/api_keys` route's scope to `scopeRegion`, re-run `go test ./internal/httpapi/ -run 'TestRouteTable_KeyAdmin|TestAPIKeys_ServicePrincipal'`, confirm both fail, then revert.

- [ ] **Step 8: Commit**

```bash
make check
git add internal/httpapi internal/regions
git commit -m "feat(httpapi): region API key mint, list, and revoke routes"
```

---

## Task 8: Studies and surveys admin routes

**Wire-time convention, stated once:** the survey family (studies, surveys, responses) formats every instant with `surveys.FormatTime` (`2006-01-02T15:04:05.000Z`, valid RFC 3339 UTC) rather than `formatInstant`, so `GET /surveys/{id}` and `PUT /surveys/{id}` round-trip through `surveys.Document` unchanged. Every other admin family keeps `formatInstant` (`time.RFC3339`).

**Files:**
- Create: `internal/surveys/codec.go`, `internal/surveys/codec_test.go`
- Create: `internal/httpapi/admin_surveys.go`, `internal/httpapi/admin_surveys_test.go`
- Modify: `internal/httpapi/json.go` (`decodeJSONStrict`), `internal/httpapi/router.go`
- Modify: `cmd/sidecar-admin/surveys.go` (call the moved codec)

**Interfaces:**
- Consumes: `surveys.UpdateStudy`, `CreateSurveyInRegion` (Task 4); `requireRegion`, `mustRegion` (Task 6).
- Produces:
  - `type surveys.InstantParser func(string) (time.Time, error)`
  - `func surveys.DefinitionFromDocument(doc Document, parse InstantParser) (Definition, error)`
  - `func decodeJSONStrict(w http.ResponseWriter, r *http.Request, maxBytes int64, dst any) error`
  - `const maxSurveyBody = 256 << 10`
  - `loadStudy`, `loadSurvey` in `region_scope.go`
  - the nine study/survey routes

- [ ] **Step 1: Move the document codec into `internal/surveys`**

Create `internal/surveys/codec.go`:

```go
package surveys

import (
	"fmt"
	"time"
)

// InstantParser parses one date field of an authoring Document. It is a
// callback rather than a region parameter so this package stays free of an
// internal/regions import: the CLI and the HTTP API each supply their own,
// with error copy written for their own audience, and both enforce the
// explicit-UTC-offset rule.
type InstantParser func(s string) (time.Time, error)

// DefinitionFromDocument converts an authoring document into a validated
// Definition (design spec section 2.13). It is shared by `sidecar-admin
// survey create --file` and POST/PUT /surveys so the two authoring surfaces
// cannot drift on defaults -- notably Available, which is true when the
// document omits it.
func DefinitionFromDocument(doc Document, parse InstantParser) (Definition, error) {
	def := Definition{
		Name: doc.Name, Available: true,
		ShowOnMap: doc.ShowOnMap, ShowOnStops: doc.ShowOnStops, AlwaysVisible: doc.AlwaysVisible,
		AllowsMultipleResponses: doc.AllowsMultipleResponses,
		VisibleStopList:         doc.VisibleStopList, VisibleRouteList: doc.VisibleRouteList,
	}
	if doc.Available != nil {
		def.Available = *doc.Available
	}
	if doc.StartDate != nil {
		t, err := parse(*doc.StartDate)
		if err != nil {
			return Definition{}, fmt.Errorf("start_date: %w", err)
		}
		def.StartTime = &t
	}
	if doc.EndDate != nil {
		t, err := parse(*doc.EndDate)
		if err != nil {
			return Definition{}, fmt.Errorf("end_date: %w", err)
		}
		def.EndTime = &t
	}
	for _, q := range doc.Questions {
		def.Questions = append(def.Questions, QuestionDefinition{Required: q.Required, Content: q.Content})
	}
	if err := def.Validate(); err != nil {
		return Definition{}, err
	}
	return def, nil
}
```

Delete `definitionFromDocument` from `cmd/sidecar-admin/surveys.go` and replace its call sites with:

```go
	def, err := surveys.DefinitionFromDocument(doc, func(s string) (time.Time, error) {
		return parseInstant(s, region)
	})
```

Move the CLI's `definitionFromDocument` tests from `cmd/sidecar-admin/surveys_test.go` into `internal/surveys/codec_test.go`, adapting them to pass an explicit parser. Keep at least one CLI-level test that proves the region's timezone still reaches the error message.

- [ ] **Step 2: Run the moved tests**

Run: `go test ./internal/surveys/... ./cmd/sidecar-admin/...`
Expected: PASS.

- [ ] **Step 3: Write the failing handler tests**

Create `internal/httpapi/admin_surveys_test.go`. It needs a fixture with the optional repositories wired, so first restructure the existing fixture helper in `admin_alerts_test.go`.

The `mutate func(*Deps)` hook runs after the `Deps` literal is built and has no handle on the store, so it cannot wire a repository. Change the helper to take the flag directly and keep the two existing names as thin wrappers, so no existing call site changes:

```go
// newAdminFixture is the default wiring: alerts, regions, auth, push
// registrations, alert pushes, and API keys.
func newAdminFixture(t *testing.T) *adminFixture {
	t.Helper()
	return newAdminFixtureWith(t, false, nil)
}

// newAdminFixtureWithDeps is newAdminFixture with a hook that adjusts Deps
// just before the router is built, for tests that need a deliberately
// partial wiring.
func newAdminFixtureWithDeps(t *testing.T, mutate func(*Deps)) *adminFixture {
	t.Helper()
	return newAdminFixtureWith(t, false, mutate)
}

// newFullAdminFixture additionally wires the repositories the surveys, ghost
// bus, and alarm route families are gated on (design spec section 5.7), so
// those routes are actually registered. It is separate from
// newAdminFixture so the default fixture keeps proving that an unwired
// family registers NO routes at all.
func newFullAdminFixture(t *testing.T) *adminFixture {
	t.Helper()
	return newAdminFixtureWith(t, true, nil)
}

func newAdminFixtureWith(t *testing.T, full bool, mutate func(*Deps)) *adminFixture {
	// ... existing body up to the Deps literal ...
	if full {
		deps.Surveys = store.Surveys()
		deps.GhostBus = store.GhostBus()
		deps.Alarms = store.Alarms()
		// Alarm creation resolves a departure through OBA; a stub that
		// always fails degrades every alarm to the generic message, which is
		// the documented fallback and keeps these tests off the network.
		deps.OBA = failingOBA{}
	}
	if mutate != nil {
		mutate(&deps)
	}
	// ... unchanged from here ...
}
```

Tests:

```go
// TestAdminStudies_CRUD pins the study family's happy paths and codes.
func TestAdminStudies_CRUD(t *testing.T) {
	t.Parallel()

	f := newFullAdminFixture(t)
	created := object(t, f.do(http.MethodPost, "/api/admin/v1/regions/1/studies",
		`{"name":"Fall 2026","description":"rider experience"}`), http.StatusCreated)
	assertKeys(t, "study", created, []string{"id", "name", "description", "created_at", "updated_at"})
	id := jsonID(t, created)

	got := object(t, f.do(http.MethodGet, fmt.Sprintf("/api/admin/v1/regions/1/studies/%d", id), ""), http.StatusOK)
	if got["name"] != "Fall 2026" {
		t.Errorf("name = %v", got["name"])
	}

	patched := object(t, f.do(http.MethodPatch, fmt.Sprintf("/api/admin/v1/regions/1/studies/%d", id),
		`{"name":"Fall 2026 (revised)"}`), http.StatusOK)
	if patched["name"] != "Fall 2026 (revised)" || patched["description"] != "rider experience" {
		t.Errorf("PATCH must merge, not replace: %v", patched)
	}

	list := array(t, f.do(http.MethodGet, "/api/admin/v1/regions/1/studies", ""), http.StatusOK)
	if len(list) != 1 {
		t.Errorf("got %d studies, want 1", len(list))
	}
	if empty := array(t, f.do(http.MethodGet, "/api/admin/v1/regions/0/studies", ""), http.StatusOK); len(empty) != 0 {
		t.Errorf("region 0 shows %d of region 1's studies", len(empty))
	}
	// A blank name is a well-formed body that fails domain validation: 422,
	// not 400 (design spec section 5, "Status codes").
	if rec := f.do(http.MethodPost, "/api/admin/v1/regions/1/studies", `{"name":"  "}`); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("blank name: status = %d, want 422", rec.Code)
	}
	if rec := f.do(http.MethodPost, "/api/admin/v1/regions/1/studies", `{`); rec.Code != http.StatusBadRequest {
		t.Errorf("malformed JSON: status = %d, want 400", rec.Code)
	}
}

// TestAdminStudies_ForeignStudyIs404 -- the loader, not a post-hoc compare.
func TestAdminStudies_ForeignStudyIs404(t *testing.T) {
	t.Parallel()

	f := newFullAdminFixture(t)
	id := jsonID(t, object(t, f.do(http.MethodPost, "/api/admin/v1/regions/1/studies", `{"name":"a"}`), http.StatusCreated))

	for _, tc := range []struct{ method, body string }{
		{http.MethodGet, ""},
		{http.MethodPatch, `{"name":"hijacked"}`},
	} {
		rec := f.do(tc.method, fmt.Sprintf("/api/admin/v1/regions/0/studies/%d", id), tc.body)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s across regions: status = %d, want 404; body = %s", tc.method, rec.Code, rec.Body.String())
		}
	}
	after := object(t, f.do(http.MethodGet, fmt.Sprintf("/api/admin/v1/regions/1/studies/%d", id), ""), http.StatusOK)
	if after["name"] != "a" {
		t.Errorf("a refused PATCH still wrote: name = %v", after["name"])
	}
}

// TestAdminSurveys_StrictDecoding. DisallowUnknownFields, exactly as the CLI
// does: a misspelled show_on_maps must not silently hide a survey.
func TestAdminSurveys_StrictDecoding(t *testing.T) {
	t.Parallel()

	f := newFullAdminFixture(t)
	studyID := jsonID(t, object(t, f.do(http.MethodPost, "/api/admin/v1/regions/1/studies", `{"name":"s"}`), http.StatusCreated))

	body := fmt.Sprintf(`{"study_id":%d,"name":"q","show_on_maps":true,%s}`, studyID, minimalQuestionsJSON)
	rec := f.do(http.MethodPost, "/api/admin/v1/regions/1/surveys", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown field: status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(bodyText(rec), "show_on_maps") {
		t.Errorf("body = %q, want it to name the unknown field", bodyText(rec))
	}
}

// TestAdminSurveys_BodyCap: 256 KB, mapped to 400 with the operator-facing
// message rather than encoding/json's Go-developer wording.
func TestAdminSurveys_BodyCap(t *testing.T) {
	t.Parallel()

	f := newFullAdminFixture(t)
	studyID := jsonID(t, object(t, f.do(http.MethodPost, "/api/admin/v1/regions/1/studies", `{"name":"s"}`), http.StatusCreated))
	huge := fmt.Sprintf(`{"study_id":%d,"name":"%s",%s}`, studyID, strings.Repeat("a", 300*1024), minimalQuestionsJSON)

	rec := f.do(http.MethodPost, "/api/admin/v1/regions/1/surveys", huge)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if got, want := bodyText(rec), `{"error":"request body too large"}`; got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}

// TestAdminSurveys_ServerOwnedFieldsRejected. id, study, created_at and
// updated_at round-trip out of `survey show`, so a client that pipes show
// into create must be told, not silently obeyed (design spec section 5.2).
func TestAdminSurveys_ServerOwnedFieldsRejected(t *testing.T) {
	t.Parallel()

	f := newFullAdminFixture(t)
	studyID := jsonID(t, object(t, f.do(http.MethodPost, "/api/admin/v1/regions/1/studies", `{"name":"s"}`), http.StatusCreated))

	for _, extra := range []string{
		`"id":5`, `"study":{"id":1,"name":"x","description":""}`,
		`"created_at":"2026-01-01T00:00:00.000Z"`, `"updated_at":"2026-01-01T00:00:00.000Z"`,
	} {
		body := fmt.Sprintf(`{"study_id":%d,"name":"q",%s,%s}`, studyID, extra, minimalQuestionsJSON)
		if rec := f.do(http.MethodPost, "/api/admin/v1/regions/1/surveys", body); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status = %d, want 422; body = %s", extra, rec.Code, rec.Body.String())
		}
	}
}

// TestAdminSurveys_ForeignStudyIsNotFound. The study_id arrives in the BODY,
// so this is what CreateSurveyInRegion's join condition exists for -- a
// loader-then-compare here would be the thing a refactor drops.
func TestAdminSurveys_ForeignStudyIsNotFound(t *testing.T) {
	t.Parallel()

	f := newFullAdminFixture(t)
	studyID := jsonID(t, object(t, f.do(http.MethodPost, "/api/admin/v1/regions/0/studies", `{"name":"tampa"}`), http.StatusCreated))

	body := fmt.Sprintf(`{"study_id":%d,"name":"q",%s}`, studyID, minimalQuestionsJSON)
	if rec := f.do(http.MethodPost, "/api/admin/v1/regions/1/surveys", body); rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
	}
}

// TestAdminSurveys_RoundTrip: GET returns the same document PUT accepts.
func TestAdminSurveys_RoundTrip(t *testing.T) {
	t.Parallel()

	f := newFullAdminFixture(t)
	studyID := jsonID(t, object(t, f.do(http.MethodPost, "/api/admin/v1/regions/1/studies", `{"name":"s"}`), http.StatusCreated))
	created := object(t, f.do(http.MethodPost, "/api/admin/v1/regions/1/surveys",
		fmt.Sprintf(`{"study_id":%d,"name":"q",%s}`, studyID, minimalQuestionsJSON)), http.StatusCreated)
	id := jsonID(t, created)

	shown := object(t, f.do(http.MethodGet, fmt.Sprintf("/api/admin/v1/regions/1/surveys/%d", id), ""), http.StatusOK)
	// The GET document carries the server-owned keys; PUT must reject them,
	// so a caller edits the document rather than replaying it verbatim. This
	// asserts the shape, not a blind round trip.
	assertKeys(t, "survey", shown, []string{
		"id", "name", "available", "start_date", "end_date", "show_on_map", "show_on_stops",
		"always_visible", "allows_multiple_responses", "visible_stop_list", "visible_route_list",
		"questions", "study", "created_at", "updated_at",
	})

	updated := object(t, f.do(http.MethodPut, fmt.Sprintf("/api/admin/v1/regions/1/surveys/%d", id),
		fmt.Sprintf(`{"name":"q2",%s}`, minimalQuestionsJSON)), http.StatusOK)
	if updated["name"] != "q2" {
		t.Errorf("name = %v, want q2", updated["name"])
	}
	// PUT cannot move a survey between studies.
	if rec := f.do(http.MethodPut, fmt.Sprintf("/api/admin/v1/regions/1/surveys/%d", id),
		fmt.Sprintf(`{"study_id":%d,"name":"q3",%s}`, studyID, minimalQuestionsJSON)); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("study_id on PUT: status = %d, want 422", rec.Code)
	}
}

// TestAdminSurveys_ConflictCodes: both repository refusals are 409, carrying
// the sentinel's own text.
func TestAdminSurveys_ConflictCodes(t *testing.T) {
	t.Parallel()
	// Create a survey, add a response through the rider API, then assert:
	//   PUT with a different question set -> 409 ErrQuestionsFrozen
	//   DELETE                            -> 409 ErrHasResponses
	// and that a survey with no responses DELETEs with 204.
}

// TestAdminSurveys_ListCarriesResponseCounts.
func TestAdminSurveys_ListCarriesResponseCounts(t *testing.T) {
	t.Parallel()
	// Two surveys, one with two responses: assert study_id and
	// response_count on each list entry, and that region 0's list is empty.
}
```

Add `minimalQuestionsJSON` — the smallest `"questions": [...]` fragment `Definition.Validate` accepts — as a package-level test constant, derived from the fixtures `internal/surveys/definition_test.go` already uses.

- [ ] **Step 4: Run to verify it fails**

Run: `go test ./internal/httpapi/ -run 'TestAdminStudies|TestAdminSurveys'`
Expected: FAIL — 404 from the mux.

- [ ] **Step 5: Add `decodeJSONStrict`**

In `internal/httpapi/json.go`, beside `decodeJSON`:

```go
// decodeJSONStrict is decodeJSON with DisallowUnknownFields. It exists for
// the survey authoring document, where the CLI has always been strict for a
// concrete reason: a misspelled "show_on_maps" would decode as absent and
// silently ship a hidden survey. Everywhere else this API is deliberately
// lenient, so a newer SPA sending a field this server has not learned about
// yet is not rejected outright -- do not "unify" the two.
func decodeJSONStrict(w http.ResponseWriter, r *http.Request, maxBytes int64, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return errBodyTooLarge
		}
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	return nil
}
```

- [ ] **Step 6: Write `internal/httpapi/admin_surveys.go`**

Contents:

- `const maxSurveyBody = 256 << 10` with the "a survey document is questions and copy, not payloads" rationale.
- `adminStudyJSON{ID, Name, Description, CreatedAt, UpdatedAt}` — **named `adminStudyJSON`, not `studyJSON`**: `studyJSON` and `surveyJSON` already exist in this package for the rider feed and must not be reused or shadowed.
- `adminSurveySummaryJSON` — the summary list entry: `{id, study_id, name, available, start_date, end_date, show_on_map, show_on_stops, always_visible, allows_multiple_responses, visible_stop_list, visible_route_list, response_count, created_at, updated_at}`.
- `surveyWriteRequest`:

```go
// surveyWriteRequest is the POST and PUT body: the authoring document plus
// study_id. StudyID is a pointer so "absent" and "0" stay distinguishable,
// and because PUT must REJECT it rather than ignore it.
type surveyWriteRequest struct {
	StudyID *int64 `json:"study_id"`
	surveys.Document
}
```

- `rejectServerOwned(doc surveys.Document) error` returning a 422-worthy message when `doc.ID != nil`, `doc.Study != nil`, `doc.CreatedAt != ""`, or `doc.UpdatedAt != ""`.
- `adminSurveysHandler` with `listStudies`, `createStudy`, `getStudy`, `patchStudy`, `listSurveys`, `createSurvey`, `getSurvey`, `putSurvey`, `deleteSurvey`.
- `createStudy` / `patchStudy` trim the name and return 422 on empty; `patchStudy` merges against the loaded study.
- `createSurvey` decodes strictly, rejects server-owned fields, requires `study_id` (422 when absent), builds the definition via `surveys.DefinitionFromDocument` with `func(s string) (time.Time, error) { return parseInstantJSON(s, region) }`, and calls `CreateSurveyInRegion`. `surveys.ErrNotFound` → 404 `"study not found"`; a `Definition.Validate` failure → 422 carrying the domain message (the same text the CLI prints).
- `putSurvey` goes through `loadSurvey`, rejects a present `study_id` (422), and calls `UpdateSurvey`. `surveys.ErrQuestionsFrozen` → 409 with the sentinel's own text.
- `deleteSurvey` goes through `loadSurvey`; `surveys.ErrHasResponses` → 409; otherwise 204.
- `getSurvey` writes `surveys.DocumentFromSurvey(s)` directly.
- `writeSurveyStoreError(w, logger, op string, err error)` mapping `surveys.ErrNotFound` → 404, `ErrQuestionsFrozen` / `ErrHasResponses` → 409 with the sentinel's own text (never `err.Error()`, which carries the failing statement), everything else → logged 500.

Add to `region_scope.go`:

```go
// loadStudy resolves {id} within the request's region.
func loadStudy(w http.ResponseWriter, r *http.Request, deps Deps) (surveys.Study, bool)

// loadSurvey resolves {id} within the request's region THROUGH ITS STUDY:
// surveys carry no region of their own, so the study is the only place the
// tenancy answer lives. GetSurvey populates Study on every read, so this
// needs no second query.
func loadSurvey(w http.ResponseWriter, r *http.Request, deps Deps) (surveys.Survey, bool)
```

- [ ] **Step 7: Register the routes**

In `adminRoutes`, gated on `deps.Surveys != nil`, all `operatorOrKey` / `scopeRegion`: the four study routes and the five survey routes (`GET`/`POST` `/surveys`, `GET`/`PUT`/`DELETE` `/surveys/{id}`). Update the pinned route count to **31** and add nine `tenancyFixtures` entries; the `POST /surveys` entry supplies a body carrying region B's `study_id`.

- [ ] **Step 8: Run to verify it passes, then prove a test can fail**

Run: `go test ./internal/httpapi/...` — expect PASS.
Then remove `dec.DisallowUnknownFields()` from `decodeJSONStrict`, re-run `-run TestAdminSurveys_StrictDecoding`, confirm it fails, and restore.

- [ ] **Step 9: Commit**

```bash
make check
git add internal/httpapi internal/surveys cmd/sidecar-admin
git commit -m "feat(httpapi): study and survey admin routes"
```

---

## Task 9: Survey responses, JSON and CSV

**Files:**
- Create: `internal/csvsafe/csvsafe.go`, `internal/csvsafe/csvsafe_test.go`
- Create: `internal/surveys/csv.go`, `internal/surveys/csv_test.go`
- Create: `internal/httpapi/admin_responses.go`, `internal/httpapi/admin_responses_test.go`
- Modify: `cmd/sidecar-admin/surveys.go` (call `surveys.WriteResponsesCSV`)
- Modify: `internal/httpapi/router.go`, `internal/httpapi/region_scope.go`

**Interfaces:**
- Consumes: `surveys.GetResponseInRegion` (Task 4); `loadSurvey` (Task 8).
- Produces:
  - `func csvsafe.Cell(s string) string`, `func csvsafe.Float(v *float64) string`
  - `func surveys.WriteResponsesCSV(w io.Writer, survey Survey, responses []Response) error`
  - `loadResponse` in `region_scope.go`
  - `func writeCSVHeaders(w http.ResponseWriter, filename string)`
  - three routes: `GET .../surveys/{id}/responses`, `GET .../surveys/{id}/responses.csv`, `GET .../survey_responses/{publicId}`

- [ ] **Step 1: Extract the CSV guard**

The formula-injection guard and the nullable-float cell currently live in `cmd/sidecar-admin/surveys.go` and are used by both CSV writers. Both writers are moving into their own domain packages, and neither may import the other, so the guard gets a package of its own.

Create `internal/csvsafe/csvsafe.go`:

```go
// Package csvsafe holds the cell formatting every CSV export in this repo
// shares. It exists as its own package because the two exports now live in
// internal/surveys and internal/ghostbus, neither of which may import the
// other -- and two copies of a formula-injection guard is how one of them
// quietly loses a character from its trigger set.
package csvsafe

import "strconv"

// Cell renders a text cell, defusing spreadsheet formula injection.
//
// Excel, Numbers and Sheets treat a leading '=', '+', '-', '@', tab or
// carriage return as the start of a formula, so a rider-supplied comment can
// become executable in whichever spreadsheet an agency opens the export in.
// A single leading apostrophe forces the cell to be read as literal text in
// every one of those tools while leaving the visible value -- and a
// re-import through this same reader -- unchanged for every other cell.
func Cell(s string) string {
	if s == "" {
		return s
	}
	switch s[0] {
	case '=', '+', '-', '@', '\t', '\r':
		return "'" + s
	default:
		return s
	}
}

// Float renders an optional float64 cell: blank when absent, else the
// shortest decimal that round-trips exactly.
func Float(v *float64) string {
	if v == nil {
		return ""
	}
	return strconv.FormatFloat(*v, 'f', -1, 64)
}
```

Create `internal/csvsafe/csvsafe_test.go` covering each trigger character, the empty string, a benign value, a value whose trigger is not first, and `Float(nil)` / `Float(&0.0)` (0 must render as `0`, not blank).

Delete `csvCell` and `floatCell` from `cmd/sidecar-admin/surveys.go` and repoint their uses in `cmd/sidecar-admin/ghostbus.go` at `csvsafe.Cell` / `csvsafe.Float` for now; Task 10 moves that writer too.

- [ ] **Step 2: Move the survey response CSV writer**

Create `internal/surveys/csv.go` holding `WriteResponsesCSV(w io.Writer, survey Survey, responses []Response) error` — the body of `surveyResponses` from `cmd/sidecar-admin/surveys.go` with the store lookups removed, using `csvsafe.Cell` / `csvsafe.Float`. Keep the long-format comment verbatim (one row per answer; a response with no answers still gets a row).

The `survey` parameter is currently unused by the row builder. Keep it in the signature anyway: the HTTP handler has already loaded the survey for its tenancy check, and passing it makes "these responses belong to this survey" part of the call rather than a convention. Add a comment saying exactly that, so nobody deletes the parameter as dead.

Rewrite `cmd/sidecar-admin/surveys.go`'s `surveyResponses` to load the survey and responses and then call the moved writer. Move its CSV assertions from `cmd/sidecar-admin/surveys_test.go` into `internal/surveys/csv_test.go`, keeping one CLI test that the command still writes a header row and exits 0.

- [ ] **Step 3: Write the failing handler tests**

Create `internal/httpapi/admin_responses_test.go`:

```go
// TestAdminResponses_CSVContract pins every header the export must carry.
// Content-Disposition is server-generated and fixed: a filename derived from
// a survey name would put rider-influenced text in a header.
func TestAdminResponses_CSVContract(t *testing.T) {
	t.Parallel()

	f := newFullAdminFixture(t)
	surveyID := f.seedSurveyWithResponses(t, regionPuget, []string{"=cmd|' /C calc'!A0", "fine"})

	rec := f.do(http.MethodGet, fmt.Sprintf("/api/admin/v1/regions/1/surveys/%d/responses.csv", surveyID), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	for header, want := range map[string]string{
		"Content-Type":              "text/csv",
		"X-Content-Type-Options":    "nosniff",
		"Cache-Control":             "no-store",
		"Content-Disposition":       fmt.Sprintf(`attachment; filename="survey-%d-responses.csv"`, surveyID),
	} {
		if got := rec.Header().Get(header); !strings.HasPrefix(got, want) {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	// The formula guard survives the move out of the CLI.
	if !strings.Contains(rec.Body.String(), `"'=cmd`) {
		t.Errorf("rider answer not defused:\n%s", rec.Body.String())
	}
}

// TestAdminResponses_ReachedThroughAnotherRegionsSurveyIs404 is the case the
// single joined query exists for: /regions/A/survey_responses/{id-of-B}.
func TestAdminResponses_ReachedThroughAnotherRegionsSurveyIs404(t *testing.T) {
	t.Parallel()

	f := newFullAdminFixture(t)
	surveyID := f.seedSurveyWithResponses(t, regionPuget, []string{"a"})
	list := array(t, f.do(http.MethodGet, fmt.Sprintf("/api/admin/v1/regions/1/surveys/%d/responses", surveyID), ""), http.StatusOK)
	publicID, _ := list[0]["public_id"].(string)

	if rec := f.do(http.MethodGet, "/api/admin/v1/regions/0/survey_responses/"+publicID, ""); rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
	}
	if rec := f.do(http.MethodGet, "/api/admin/v1/regions/1/survey_responses/"+publicID, ""); rec.Code != http.StatusOK {
		t.Errorf("own region: status = %d, want 200", rec.Code)
	}
	// Listing responses loads and checks the SURVEY first, so a foreign
	// survey id is a 404 rather than an empty array.
	if rec := f.do(http.MethodGet, fmt.Sprintf("/api/admin/v1/regions/0/surveys/%d/responses", surveyID), ""); rec.Code != http.StatusNotFound {
		t.Errorf("foreign survey responses: status = %d, want 404", rec.Code)
	}
	if rec := f.do(http.MethodGet, fmt.Sprintf("/api/admin/v1/regions/0/surveys/%d/responses.csv", surveyID), ""); rec.Code != http.StatusNotFound {
		t.Errorf("foreign survey CSV: status = %d, want 404", rec.Code)
	}
}

// TestAdminResponses_JSONShape.
func TestAdminResponses_JSONShape(t *testing.T) {
	t.Parallel()
	// assertKeys on one response: id, survey_id, public_id, user_identifier,
	// stop_identifier, stop_latitude, stop_longitude, answers, created_at,
	// updated_at; each answer: question_id, question_type, question_label,
	// answer. Instants use surveys.FormatTime, so assert the .000Z shape.
}
```

`f.seedSurveyWithResponses` creates a study, a survey, and one response per supplied answer string, going through `f.store.Surveys()` directly.

- [ ] **Step 4: Run to verify it fails, then implement**

Run: `go test ./internal/httpapi/ -run TestAdminResponses` — expect FAIL (404).

Create `internal/httpapi/admin_responses.go`:

- `adminResponseJSON` and `answerJSON` wire shapes, instants via `surveys.FormatTime`.
- `listResponses` → `loadSurvey` then `ListResponses`; `make`, not nil.
- `responsesCSV` → `loadSurvey`, `ListResponses`, `writeCSVHeaders`, `surveys.WriteResponsesCSV`. A write error after the status line is committed can only be logged.
- `getResponse` → `loadResponse` (via `GetResponseInRegion` on `r.PathValue("publicId")`).

Add the shared header helper to `json.go`:

```go
// writeCSVHeaders sets the response headers every CSV export shares.
// nosniff stops a browser from re-interpreting the body as HTML; no-store
// keeps rider data out of intermediary caches; the filename is fixed and
// server-generated, never derived from a name, so nothing rider- or
// author-supplied reaches a Content-Disposition header.
func writeCSVHeaders(w http.ResponseWriter, filename string) {
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
}
```

Add `loadResponse` to `region_scope.go`. Register the three routes (`operatorOrKey`, `scopeRegion`) gated on `deps.Surveys != nil`, update the pinned count to **34**, and add three `tenancyFixtures` entries.

- [ ] **Step 5: Run and prove a test can fail**

Run: `go test ./internal/... ./cmd/...` — expect PASS.
Then remove `AND studies.region_id` from the response query (or drop the `loadSurvey` call in `listResponses`), confirm `TestAdminResponses_ReachedThroughAnotherRegionsSurveyIs404` fails, and restore.

- [ ] **Step 6: Commit**

```bash
make check
git add internal cmd
git commit -m "feat(httpapi): survey response JSON and CSV admin routes"
```

---

## Task 10: Ghost bus report admin routes

**Files:**
- Create: `internal/ghostbus/csv.go`, `internal/ghostbus/csv_test.go`
- Create: `internal/httpapi/admin_ghostbus.go`, `internal/httpapi/admin_ghostbus_test.go`
- Modify: `cmd/sidecar-admin/ghostbus.go` (call the moved writer)
- Modify: `internal/httpapi/router.go`, `internal/httpapi/region_scope.go`

**Interfaces:**
- Consumes: `ghostbus.GetByPublicID` (Task 3); `csvsafe` (Task 9); `writeCSVHeaders` (Task 9).
- Produces:
  - `func ghostbus.WriteReportsCSV(w io.Writer, region regions.Region, reports []Report) error`
  - `loadReport` in `region_scope.go`
  - `func parseSinceQuery(r *http.Request) (int64, error)`
  - three routes: `GET .../ghost_bus_reports`, `GET .../ghost_bus_reports.csv`, `GET .../ghost_bus_reports/{publicId}`

Note: `internal/ghostbus` currently does not import `internal/regions`. `WriteReportsCSV` needs the region only for its timezone (`reported_at_local`, `service_date`), so take `region regions.Region` — the import is one-directional (`regions` does not import `ghostbus`) and it keeps the CSV's column set identical to the CLI's, which is the point of moving it rather than rewriting it.

- [ ] **Step 1: Move the writer**

Create `internal/ghostbus/csv.go` with `WriteReportsCSV`, `ghostBusHeader`, `reportRow`, `vehiclePosition`, `int64Cell`, `boolCell`, `msToUTC`, `timeCell`, `stalenessCell`, and the `csvSnapshot` / `csvLatLon` decode structs — the existing bodies verbatim, with `csvCell`/`floatCell` replaced by `csvsafe.Cell`/`csvsafe.Float`. Keep every comment, especially the 60,000-not-60 note on `stalenessCell`.

Rewrite `cmd/sidecar-admin/ghostbus.go`'s export path to call `ghostbus.WriteReportsCSV`, and move `cmd/sidecar-admin/ghostbus_test.go`'s row-level assertions (including the R1 staleness fixture) into `internal/ghostbus/csv_test.go`. Keep one CLI test end to end.

Run: `go test ./internal/ghostbus/... ./cmd/sidecar-admin/...` — expect PASS.

- [ ] **Step 2: Write the failing handler tests**

Create `internal/httpapi/admin_ghostbus_test.go`:

```go
// TestAdminGhostBus_ListAndSince. `since` is optional (absent means all) and
// must carry an explicit UTC offset; a naive datetime is a 400, never
// interpreted in server-local time.
func TestAdminGhostBus_ListAndSince(t *testing.T) {
	t.Parallel()

	f := newFullAdminFixture(t)
	f.seedReport(t, regionPuget, "old", testNow.Add(-48*time.Hour))
	f.seedReport(t, regionPuget, "new", testNow)
	f.seedReport(t, regionTampa, "other-region", testNow)

	all := array(t, f.do(http.MethodGet, "/api/admin/v1/regions/1/ghost_bus_reports", ""), http.StatusOK)
	if len(all) != 2 {
		t.Fatalf("got %d reports, want 2 (region 0's must not appear)", len(all))
	}
	since := testNow.Add(-time.Hour).UTC().Format(time.RFC3339)
	recent := array(t, f.do(http.MethodGet,
		"/api/admin/v1/regions/1/ghost_bus_reports?since="+url.QueryEscape(since), ""), http.StatusOK)
	if len(recent) != 1 {
		t.Errorf("got %d reports since %s, want 1", len(recent), since)
	}
	for _, bad := range []string{"2026-08-27T00:00:00", "yesterday", "1756252800"} {
		if rec := f.do(http.MethodGet, "/api/admin/v1/regions/1/ghost_bus_reports?since="+url.QueryEscape(bad), ""); rec.Code != http.StatusBadRequest {
			t.Errorf("since=%q: status = %d, want 400", bad, rec.Code)
		}
	}
}

// TestAdminGhostBus_EpochMillisecondFieldsPassThrough. service_date and the
// three arrival timestamps are OBA identifiers and dedupe keys, not
// instants: they cross the wire as the integers they arrived as, and
// reformatting one as RFC 3339 would break the dedupe key (design spec
// section 5).
func TestAdminGhostBus_EpochMillisecondFieldsPassThrough(t *testing.T) {
	t.Parallel()
	// Seed one report with known epoch-ms values, assert each comes back as
	// the same JSON number.
}

// TestAdminGhostBus_DetailCarriesTheRawSnapshot.
func TestAdminGhostBus_DetailCarriesTheRawSnapshot(t *testing.T) {
	t.Parallel()
	// Mark a report captured with a known snapshot document; assert the
	// detail route returns snapshot_status and the snapshot JSON exactly as
	// captured, and that a report in another region is 404.
}

// TestAdminGhostBus_CSVContract mirrors the survey CSV contract.
func TestAdminGhostBus_CSVContract(t *testing.T) {
	t.Parallel()
	// nosniff, no-store, text/csv,
	// Content-Disposition: attachment; filename="ghost-bus-reports-1.csv",
	// and a rider comment beginning with '=' defused.
}
```

- [ ] **Step 3: Run to verify it fails, then implement**

Create `internal/httpapi/admin_ghostbus.go`:

- `ghostBusReportJSON` with every `ghostbus.Report` field. `ServiceDate`, `ScheduledArrivalAt`, `PredictedArrivalAt`, `PredictionLastUpdatedAt` are `int64` / `*int64` and pass through unchanged; `CreatedAt` and `SnapshotCapturedAt` use `formatInstant`. `SnapshotJSON` is emitted as a `json.RawMessage` when it parses and as `null` when the column is empty — with a comment saying the raw column is the source of truth and a malformed document degrades to null rather than failing the response, matching the CSV writer's posture.
- `parseSinceQuery` in `params.go` or beside the handler:

```go
// parseSinceQuery reads ?since=RFC3339 as epoch seconds. Absent is 0, which
// ListForExport reads as "everything". An explicit UTC offset is required:
// interpreting a naive datetime in the server's local zone is exactly the
// machine-local dependence this repo bans everywhere else, and here it would
// silently shift an agency's export window.
func parseSinceQuery(r *http.Request) (int64, error)
```

- `list`, `csv`, `get` handlers; `get` uses `loadReport`.

Register three routes (`operatorOrKey`, `scopeRegion`) gated on `deps.GhostBus != nil`. Update the pinned route count to **37** and add three `tenancyFixtures` entries.

Run: `go test ./internal/httpapi/...` — expect PASS.

- [ ] **Step 4: Prove a test can fail**

Change `parseSinceQuery` to accept a naive datetime (parse with `"2006-01-02T15:04:05"` as a fallback), confirm `TestAdminGhostBus_ListAndSince` fails on the `2026-08-27T00:00:00` case, then revert.

- [ ] **Step 5: Commit**

```bash
make check && make test-tz
git add internal cmd
git commit -m "feat(httpapi): ghost bus report admin routes"
```

---

## Task 11: Alarms and push registration counts

**Files:**
- Create: `internal/httpapi/admin_alarms.go`, `internal/httpapi/admin_alarms_test.go`
- Create: `internal/httpapi/admin_pushregs.go`, `internal/httpapi/admin_pushregs_test.go`
- Modify: `internal/httpapi/router.go`, `internal/httpapi/region_scope.go`

**Interfaces:**
- Consumes: `alarms.ListByRegion`, `alarms.GetInRegion` (Task 3); `pushreg.CountAudience`.
- Produces:
  - `loadAlarm` in `region_scope.go`
  - `GET .../alarms`, `GET .../alarms/{id}`, `GET .../push_registrations/count`

- [ ] **Step 1: Write the failing tests**

```go
// TestAdminAlarms_OmitsPushCredentials is the whole reason this route has a
// hand-written wire shape rather than marshalling alarms.Alarm: token and
// user_push_id are push credentials, not UI data, and a struct that grew a
// field by default would ship them.
func TestAdminAlarms_OmitsPushCredentials(t *testing.T) {
	t.Parallel()

	f := newFullAdminFixture(t)
	id := f.seedAlarm(t, regionPuget, "secret-token", "secret-user-push-id")

	for _, target := range []string{
		"/api/admin/v1/regions/1/alarms",
		fmt.Sprintf("/api/admin/v1/regions/1/alarms/%d", id),
	} {
		rec := f.do(http.MethodGet, target, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200; body = %s", target, rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		for _, secret := range []string{"secret-token", "secret-user-push-id", "\"token\"", "user_push_id"} {
			if strings.Contains(body, secret) {
				t.Errorf("%s leaked %q:\n%s", target, secret, body)
			}
		}
	}

	one := object(t, f.do(http.MethodGet, fmt.Sprintf("/api/admin/v1/regions/1/alarms/%d", id), ""), http.StatusOK)
	assertKeys(t, "alarm", one, []string{
		"id", "api_version", "operating_system", "stop_id", "trip_id", "service_date",
		"vehicle_id", "stop_sequence", "seconds_before", "message", "failure_count", "created_at",
	})
	// service_date is epoch milliseconds and passes through as an integer.
	if _, ok := one["service_date"].(float64); !ok {
		t.Errorf("service_date = %#v, want a JSON number", one["service_date"])
	}
}

// TestAdminAlarms_RegionScoped.
func TestAdminAlarms_RegionScoped(t *testing.T) {
	t.Parallel()

	f := newFullAdminFixture(t)
	id := f.seedAlarm(t, regionPuget, "t", "u")
	if rec := f.do(http.MethodGet, fmt.Sprintf("/api/admin/v1/regions/0/alarms/%d", id), ""); rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	if list := array(t, f.do(http.MethodGet, "/api/admin/v1/regions/0/alarms", ""), http.StatusOK); len(list) != 0 {
		t.Errorf("region 0 shows %d of region 1's alarms", len(list))
	}
}

// TestAdminPushRegistrations_Count. Counts only: no token listing exists,
// and adding one later must be a deliberate change rather than a field.
func TestAdminPushRegistrations_Count(t *testing.T) {
	t.Parallel()

	f := newFullAdminFixture(t)
	// seedRegistrationOn is seedRegistration with an explicit platform; add
	// it beside the existing helper rather than changing that helper's
	// signature, so the alert-push tests that call seedRegistration keep
	// compiling unchanged.
	f.seedRegistrationOn(t, regionPuget, "ios-1", "ios", false)
	f.seedRegistrationOn(t, regionPuget, "ios-2", "ios", true)
	f.seedRegistrationOn(t, regionPuget, "android-1", "android", false)
	f.seedRegistrationOn(t, regionTampa, "elsewhere", "ios", false)

	got := object(t, f.do(http.MethodGet, "/api/admin/v1/regions/1/push_registrations/count", ""), http.StatusOK)
	assertKeys(t, "counts", got, []string{"total", "ios", "android", "test"})
	if got["total"] != float64(3) || got["ios"] != float64(2) || got["android"] != float64(1) {
		t.Errorf("counts = %v", got)
	}
	test, _ := got["test"].(map[string]any)
	if test["total"] != float64(1) || test["ios"] != float64(1) || test["android"] != float64(0) {
		t.Errorf("test counts = %v", got["test"])
	}
	if strings.Contains(bodyText(f.do(http.MethodGet, "/api/admin/v1/regions/1/push_registrations/count", "")), "ios-1") {
		t.Error("a token reached the response")
	}
}
```

Run: `go test ./internal/httpapi/ -run 'TestAdminAlarms|TestAdminPushRegistrations'` — expect FAIL (404).

- [ ] **Step 2: Implement**

`internal/httpapi/admin_alarms.go`:

```go
// adminAlarmJSON is one alarm as the admin API renders it (design spec
// section 5.4).
//
// Token and UserPushID are DELIBERATELY ABSENT. They are push credentials,
// not UI data, and this is a hand-written projection rather than a
// marshalled alarms.Alarm precisely so a field added to the domain type
// cannot start shipping them.
//
// ServiceDate is epoch MILLISECONDS -- an OBA identifier, not an instant --
// and passes through as an integer. CreatedAt is a real instant and is
// RFC 3339 UTC.
type adminAlarmJSON struct {
	ID              int64  `json:"id"`
	APIVersion      int    `json:"api_version"`
	OperatingSystem string `json:"operating_system"`
	StopID          string `json:"stop_id"`
	TripID          string `json:"trip_id"`
	ServiceDate     int64  `json:"service_date"`
	VehicleID       string `json:"vehicle_id"`
	StopSequence    *int64 `json:"stop_sequence"`
	SecondsBefore   int64  `json:"seconds_before"`
	Message         string `json:"message"`
	FailureCount    int64  `json:"failure_count"`
	CreatedAt       string `json:"created_at"`
}
```

with `list` and `get` handlers, `get` through `loadAlarm`.

Test helper: add `seedRegistrationOn(t, regionID int64, token, platform string, testDevice bool)` to `admin_pushes_test.go` beside the existing `seedRegistration`, which hardcodes a platform. A second helper rather than a widened signature keeps every existing alert-push test compiling unchanged.

`internal/httpapi/admin_pushregs.go`: `audienceCountJSON` already exists in `admin_pushes.go` — reuse it rather than defining a second shape. Add:

```go
// pushRegistrationCountJSON is one region's registration counts. The `test`
// sub-object is a second CountAudience call rather than a subtraction, so
// the two numbers cannot drift.
type pushRegistrationCountJSON struct {
	audienceCountJSON
	Test audienceCountJSON `json:"test"`
}
```

- [ ] **Step 3: Register the routes**

Alarms (2 routes) gated on `deps.Alarms != nil`; the count route gated on `deps.PushRegs != nil`. Both `operatorOrKey` / `scopeRegion`. `NewRouter` already panics at boot if `Deps.Alarms` is set without `PushRegs`, `Regions`, and `Now`, so no new guard is needed — note that in the registration comment so a reader does not add a redundant one.

Update the pinned route count to **40** and add three `tenancyFixtures` entries.

- [ ] **Step 4: Run, prove a test can fail, commit**

Run: `go test ./internal/httpapi/...` — expect PASS.
Then add `Token string \`json:"token"\`` to `adminAlarmJSON` and populate it; confirm `TestAdminAlarms_OmitsPushCredentials` fails; revert.

```bash
make check
git add internal/httpapi
git commit -m "feat(httpapi): read-only alarm routes and push registration counts"
```

---

## Task 12: `sidecar-admin key` and `principal`

**Files:**
- Create: `cmd/sidecar-admin/keys.go`, `cmd/sidecar-admin/keys_test.go`
- Modify: `cmd/sidecar-admin/commands.go` (dispatch), `README.md` is Task 14

**Interfaces:**
- Consumes: `apikey` (Task 1), `Store.APIKeys()` (Task 2), `regions.StripControlChars` (Task 7).
- Produces the commands in spec section 6.1:

```
sidecar-admin key create --region N --name NAME
sidecar-admin key list --region N
sidecar-admin key list --minted-by-principal N
sidecar-admin key revoke --region N --id N
sidecar-admin principal create --name NAME
sidecar-admin principal list
sidecar-admin principal revoke --id N [--keep-keys]
```

- [ ] **Step 1: Write the failing CLI tests**

Create `cmd/sidecar-admin/keys_test.go`, following the shape of `users_test.go` (which drives `run(stdin, stdout, stderr, args)` against a `sqlitetest.OpenAt` database):

```go
// keyFixture is a migrated database plus the two seeded regions, and the
// path the CLI's --db flag points at.
func keyFixture(t *testing.T) (path string, store *sqlite.Store) {
	t.Helper()
	path, store = sqlitetest.OpenAt(t)
	err := store.Regions().UpsertFromDirectory(context.Background(), []regions.Region{
		{ID: 0, Name: "Tampa Bay", OBABaseURL: "https://tampa.example/", Language: "en", Active: true},
		{ID: 1, Name: "Puget Sound", OBABaseURL: "https://puget.example/", Language: "en", Active: true},
	}, testNow)
	if err != nil {
		t.Fatalf("seed regions: %v", err)
	}
	return path, store
}

// TestKeyCreate_PrintsTheRawKeyOnce. The CLI is the only place an operator
// ever sees a key: it is printed here and nowhere else, and created_by is
// cli, which is what `key list --minted-by-principal` later excludes.
func TestKeyCreate_PrintsTheRawKeyOnce(t *testing.T) {
	t.Parallel()

	path, store := keyFixture(t)
	var stdout bytes.Buffer
	err := run(strings.NewReader(""), &stdout, io.Discard,
		[]string{"--db", path, "key", "create", "--region", "1", "--name", "obacloud"})
	if err != nil {
		t.Fatalf("key create: %v", err)
	}

	raw := findKeyInOutput(t, stdout.String()) // scans for the obask_ token
	if !strings.HasPrefix(raw, "obask_1_") {
		t.Fatalf("printed key = %q, want an obask_1_ prefix", raw)
	}
	stored, err := store.APIKeys().GetRegionKeyByHash(context.Background(), apikey.Hash(raw))
	if err != nil {
		t.Fatalf("the printed key does not resolve: %v", err)
	}
	if stored.RegionID != 1 || stored.Name != "obacloud" {
		t.Errorf("stored = %+v, want region 1 / obacloud", stored)
	}
	if stored.CreatedBy.Kind != apikey.ActorCLI || stored.CreatedBy.ID != 0 {
		t.Errorf("CreatedBy = %+v, want the cli actor with no id", stored.CreatedBy)
	}
	// The hash is never printed: an operator pasting this output into a
	// ticket must not paste the lookup key along with it.
	if strings.Contains(stdout.String(), stored.KeyHash) {
		t.Errorf("output contains the stored hash:\n%s", stdout.String())
	}
}

// TestKeyCreate_RequiresAKnownRegion. Regions come from OBACloud's directory
// export, so minting for an id the directory has never published must fail
// loudly rather than create a key nothing can ever use.
func TestKeyCreate_RequiresAKnownRegion(t *testing.T) {
	t.Parallel()

	path, store := keyFixture(t)
	err := run(strings.NewReader(""), io.Discard, io.Discard,
		[]string{"--db", path, "key", "create", "--region", "99", "--name", "x"})
	if err == nil {
		t.Fatal("key create for an unknown region: err = nil, want an error")
	}
	if !strings.Contains(err.Error(), "99") {
		t.Errorf("err = %v, want it to name the region", err)
	}
	list, listErr := store.APIKeys().ListRegionKeys(context.Background(), 99)
	if listErr != nil {
		t.Fatalf("ListRegionKeys: %v", listErr)
	}
	if len(list) != 0 {
		t.Errorf("a failed create left %d rows behind", len(list))
	}
}

// TestPrincipalRevoke_AlsoRevokesItsKeysByDefault is the recovery path.
// After a principal leaks, the keys it minted cannot be told apart from the
// legitimate ones -- the attacker mints with the same credential Rails uses
// -- so the default is to kill them all and re-provision (design spec
// section 2.2).
func TestPrincipalRevoke_AlsoRevokesItsKeysByDefault(t *testing.T) {
	t.Parallel()

	path, store := keyFixture(t)
	ctx := context.Background()
	p, err := store.APIKeys().CreatePrincipal(ctx, "rails", "ph", testNow)
	if err != nil {
		t.Fatalf("CreatePrincipal: %v", err)
	}
	minted := apikey.Actor{Kind: apikey.ActorPrincipal, ID: p.ID}
	byPrincipal, err := store.APIKeys().CreateRegionKey(ctx, 0, "a", "h-a", minted, testNow)
	if err != nil {
		t.Fatalf("CreateRegionKey: %v", err)
	}
	byCLI, err := store.APIKeys().CreateRegionKey(ctx, 1, "b", "h-b", apikey.Actor{Kind: apikey.ActorCLI}, testNow)
	if err != nil {
		t.Fatalf("CreateRegionKey: %v", err)
	}

	var stdout bytes.Buffer
	if err := run(strings.NewReader(""), &stdout, io.Discard,
		[]string{"--db", path, "principal", "revoke", "--id", strconv.FormatInt(p.ID, 10)}); err != nil {
		t.Fatalf("principal revoke: %v", err)
	}
	// The ids are printed so the operator can reconcile them against
	// whatever the consumer still holds.
	if !strings.Contains(stdout.String(), strconv.FormatInt(byPrincipal.ID, 10)) {
		t.Errorf("output does not name the revoked key %d:\n%s", byPrincipal.ID, stdout.String())
	}
	if _, err := store.APIKeys().GetRegionKeyByHash(ctx, "h-a"); !errors.Is(err, apikey.ErrRevoked) {
		t.Errorf("the principal's key: err = %v, want ErrRevoked", err)
	}
	if _, err := store.APIKeys().GetRegionKeyByHash(ctx, byCLI.KeyHash); err != nil {
		t.Errorf("a key minted by the CLI must survive: %v", err)
	}
	if _, err := store.APIKeys().GetPrincipalByHash(ctx, "ph"); !errors.Is(err, apikey.ErrRevoked) {
		t.Errorf("the principal: err = %v, want ErrRevoked", err)
	}
}

// TestKeyList_GuardsNamesForTheTerminal. Defence in depth: the API already
// strips control characters on the way in, but a row written before that
// guard existed -- or by a future path -- must not repaint the terminal of
// the operator investigating a compromise. The name goes in through the
// repository, bypassing the API's guard, which is the case this covers.
func TestKeyList_GuardsNamesForTheTerminal(t *testing.T) {
	t.Parallel()

	path, store := keyFixture(t)
	if _, err := store.APIKeys().CreateRegionKey(context.Background(), 1,
		"ob\x1b[2Jacloud", "h", apikey.Actor{Kind: apikey.ActorCLI}, testNow); err != nil {
		t.Fatalf("CreateRegionKey: %v", err)
	}

	var stdout bytes.Buffer
	if err := run(strings.NewReader(""), &stdout, io.Discard,
		[]string{"--db", path, "key", "list", "--region", "1"}); err != nil {
		t.Fatalf("key list: %v", err)
	}
	if strings.ContainsRune(stdout.String(), 0x1b) {
		t.Errorf("output carries a raw escape byte:\n%q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "ob[2Jacloud") {
		t.Errorf("output = %q, want the name with its escape byte stripped", stdout.String())
	}
}

// TestKeyList_MintedByPrincipalCrossesRegions is the post-compromise triage
// query: one principal's keys, across every region, which --region cannot
// answer.
func TestKeyList_MintedByPrincipalCrossesRegions(t *testing.T) {
	t.Parallel()
	// Seed one principal with keys in regions 0 AND 1, plus one CLI key and
	// one from a second principal. Assert the listing names both of the
	// first principal's keys and neither of the others.
}

// TestKeyList_ShowsHistoryAndCreators. Revoked rows are kept, so the list
// shows who minted each key and who revoked it -- the audit trail the
// recovery path depends on.
func TestKeyList_ShowsHistoryAndCreators(t *testing.T) {
	t.Parallel()
	// Mint two keys, revoke one; assert BOTH appear, and that the revoked
	// row carries its revocation timestamp and revoker.
}

// TestKeyRevoke_IsRegionScoped: --region and --id must agree, or it is an
// error rather than a revoke of somebody else's key.
func TestKeyRevoke_IsRegionScoped(t *testing.T) {
	t.Parallel()
	// Mint in region 1. `key revoke --region 0 --id N` errors and leaves the
	// key live; `--region 1 --id N` succeeds; a second run succeeds too,
	// because an already-revoked key is a no-op.
}

// TestPrincipalRevoke_KeepKeys opts out, for the planned rotation of a
// principal whose keys are known good.
func TestPrincipalRevoke_KeepKeys(t *testing.T) {
	t.Parallel()
	// Same setup as the default case; assert the principal is revoked and
	// its keys are still live.
}

// TestKeyAndPrincipal_FlagErrors: missing --region, missing --name, a
// non-integer --id, both --region and --minted-by-principal on `key list`,
// neither of them, and an unknown subcommand each return a non-nil error
// naming the flag, and write nothing to the database.
func TestKeyAndPrincipal_FlagErrors(t *testing.T) {
	t.Parallel()
}
```

`findKeyInOutput` is a small test helper that scans the CLI output for the single whitespace-delimited token beginning `obask_` or `obasp_` and fails if there is not exactly one.

Run: `go test ./cmd/sidecar-admin/ -run 'TestKey|TestPrincipal'` — expect FAIL.

- [ ] **Step 2: Implement**

`cmd/sidecar-admin/keys.go`, matching `users.go`'s conventions exactly (`flag.NewFlagSet` with `flag.ContinueOnError` and `SetOutput(io.Discard)`, `visitedFlags` for required-flag checks, errors wrapped as `"key create: ..."`, tabular output via the same writer the other list commands use):

- `runKey(ctx, stdout, store, now, args)` dispatching `create`, `list`, `revoke`.
- `keyCreate`: `--region` (required, and `visitedFlags` must see it, because region 0 is real), `--name` (required). Verify the region exists via `store.Regions().Get` first — an unknown region is an error, not an orphan key. Mint with `apikey.NewRegionKey`, store with `apikey.Actor{Kind: apikey.ActorCLI}`, print the raw key on its own line followed by the id and name.
- `keyList`: exactly one of `--region` / `--minted-by-principal`; columns `id, name, created by, created, last used, revoked, revoked by`, with names through `regions.StripControlChars` and instants through the existing `formatInZone` / RFC 3339 helper the other list commands use.
- `keyRevoke`: `--region` and `--id`, both required; `apikey.ErrNotFound` becomes `"key revoke: key N not found in region M"`.
- `runPrincipal(ctx, stdout, store, now, args)` dispatching `create`, `list`, `revoke`.
- `principalCreate`: `--name` required, prints `obasp_…` once.
- `principalList`: `id, name, created, last used, revoked`.
- `principalRevoke`: `--id` required, `--keep-keys` optional. Without `--keep-keys` it calls `RevokeRegionKeysByCreator(ctx, apikey.Actor{Kind: apikey.ActorPrincipal, ID: id}, apikey.Actor{Kind: apikey.ActorCLI}, now)` **before** `RevokePrincipal`, and prints the returned ids. Doc comment: the key revocations and the principal revocation are two statements, and the key sweep runs first so an interrupted run leaves a live principal with dead keys (re-runnable) rather than a dead principal with live keys (unrecoverable through this command).

Add `case "key":` and `case "principal":` to `run` in `commands.go`, and add both to the usage text.

- [ ] **Step 3: Run, prove a test can fail, commit**

Run: `go test ./cmd/...` — expect PASS.
Then delete the `RevokeRegionKeysByCreator` call from `principalRevoke`, re-run `go test ./cmd/sidecar-admin/ -run TestPrincipalRevoke_AlsoRevokesItsKeysByDefault`, confirm it fails on the still-live key, and restore. (Do not try to prove the statement ORDER can fail: it is a crash-safety property with no observable difference on a successful run, which is why it lives in a comment rather than an assertion.)

```bash
make check
git add cmd/sidecar-admin
git commit -m "feat(cli): sidecar-admin key and principal commands"
```

---

## Task 13: The SPA moves to region-scoped routes

The SPA's own URLs become region-scoped so every page has a region to put in the API path, including on reload and deep link. Old `/admin/alerts/…` bookmarks break and show the SPA's not-found page; this is stated in the spec (section 2.5) and is not worked around.

**Files:**
- Create: `web/admin/src/routes/regions/[region]/alerts/+page.svelte|ts`
- Create: `web/admin/src/routes/regions/[region]/alerts/new/+page.svelte|ts`
- Create: `web/admin/src/routes/regions/[region]/alerts/[id]/+page.svelte|ts`
- Move: the three `edit-page` / `push-card` / list component tests alongside them
- Delete: `web/admin/src/routes/alerts/**`
- Modify: `web/admin/src/routes/+page.svelte|ts` (region picker), `+layout.svelte` (nav)
- Modify: `web/admin/src/lib/api.ts`, `alerts.ts`, `pushes.ts`, `regions.ts`, `types.ts`, `AlertForm.svelte` and their tests

**Interfaces:**
- Consumes: the Task 6 and Task 11 route shapes.
- Produces:
  - `export function regionPath(region: string | number, path: string): string` in `lib/api.ts`
  - `Region.features?: string[]` and `export type AdminFeature = 'alerts' | 'pushes' | 'surveys' | 'ghost_bus_reports' | 'alarms' | 'push_registrations' | 'api_keys'` in `lib/types.ts`
  - `export function hasFeature(region: Region, feature: AdminFeature): boolean` in `lib/regions.ts`
  - `export const LAST_REGION_KEY = 'sidecar.lastRegion'` and `export function pickRegion(regions: Region[], remembered: string | null): Region | null` in `lib/regions.ts`
  - `loadPushes(region, id)` / `loadAudience(region, id)` in `lib/pushes.ts`

- [ ] **Step 1: Write the failing unit tests**

`web/admin/src/lib/regions.test.ts` — add:

```ts
describe('pickRegion', () => {
	// One region auto-forwards: making an operator choose from a list of one
	// is a click that can only have one outcome.
	it('returns the only region when there is exactly one', () => {
		expect(pickRegion([tampa], null)?.id).toBe(0);
	});

	// Region 0 is Tampa Bay. A remembered '0' must resolve to Tampa, and any
	// truthiness test on the id is a bug that would send the operator to the
	// picker instead.
	it('honours a remembered region 0', () => {
		expect(pickRegion([tampa, puget], '0')?.id).toBe(0);
	});

	it('ignores a remembered region that is no longer listed', () => {
		expect(pickRegion([tampa, puget], '99')).toBeNull();
	});

	it('ignores a non-numeric remembered value', () => {
		expect(pickRegion([tampa, puget], 'tampa')).toBeNull();
	});

	it('returns null with several regions and nothing remembered', () => {
		expect(pickRegion([tampa, puget], null)).toBeNull();
	});

	it('returns null with no regions at all', () => {
		expect(pickRegion([], '0')).toBeNull();
	});
});

describe('hasFeature', () => {
	// features is absent on the LIST endpoint's regions and present only on
	// GET /regions/{id}. Absent must not read as "everything is enabled":
	// that would render a Send button against routes that do not exist.
	it('is false when features is absent', () => {
		expect(hasFeature({ ...puget, features: undefined }, 'pushes')).toBe(false);
	});
	it('is true when the family is listed', () => {
		expect(hasFeature({ ...puget, features: ['alerts', 'pushes'] }, 'pushes')).toBe(true);
	});
	it('is false when the family is not listed', () => {
		expect(hasFeature({ ...puget, features: ['alerts'] }, 'pushes')).toBe(false);
	});
});
```

`web/admin/src/lib/api.test.ts` — add:

```ts
describe('regionPath', () => {
	it('builds a region-scoped path', () => {
		expect(regionPath(1, '/alerts')).toBe('/regions/1/alerts');
	});
	// Region 0 is Tampa Bay: a template that tests the id for truthiness
	// would emit '/regions//alerts'.
	it('handles region 0', () => {
		expect(regionPath(0, '/alerts/7')).toBe('/regions/0/alerts/7');
	});
	it('accepts the string form route params arrive as', () => {
		expect(regionPath('2', '')).toBe('/regions/2');
	});
});
```

`web/admin/src/lib/alerts.test.ts` — replace the `region_id` assertions:

```ts
it('buildCreatePayload no longer sends region_id', () => {
	const payload = buildCreatePayload(values, 'America/Los_Angeles');
	// The region is in the URL. Sending it in the body is now a 400, so a
	// stale client cannot believe it targeted a region.
	expect(payload).not.toHaveProperty('region_id');
});
```

and delete the tests for `ALL_REGIONS`, `filterByRegion`, and `selectedRegion` along with those functions.

`web/admin/src/lib/pushes.test.ts` — replace the `notConfigured` behaviour:

```ts
it('loadPushes builds a region-scoped path', async () => { ... });

// The "no transport configured" signal now comes from the region's
// `features`, read once, instead of being inferred from a per-alert 404.
// Inferring it meant a genuinely missing alert and a missing route looked
// identical, and the card silently rendered "not configured" for a deleted
// alert.
it('loadPushes propagates a 404 instead of swallowing it', async () => { ... });
```

- [ ] **Step 2: Run to verify they fail**

Run: `cd web/admin && npm run test:unit`
Expected: FAIL — `pickRegion`, `hasFeature`, `regionPath` are not exported.

- [ ] **Step 3: Implement the lib changes**

`lib/api.ts`:

```ts
/**
 * regionPath prefixes a region-scoped admin API path.
 *
 * Region 0 is Tampa Bay, a real region, so the id is interpolated
 * unconditionally -- a truthiness test would emit '/regions//alerts'.
 */
export function regionPath(region: string | number, path: string): string {
	return `/regions/${region}${path}`;
}
```

`lib/types.ts`: add `AdminFeature` and `features?: AdminFeature[]` to `Region`, with a comment that `features` is populated only by `GET /regions/{id}` and is absent on every list item — the same trap `Alert.translations` documents.

`lib/regions.ts`: add `LAST_REGION_KEY`, `pickRegion`, `hasFeature`. `pickRegion` must compare `String(r.id) === remembered` and return `null` rather than a default, so the picker screen is what handles "no answer".

`lib/pushes.ts`: `loadPushes(region, id)` and `loadAudience(region, id)` use `regionPath`; delete `notConfigured` and both `catch` blocks.

`lib/alerts.ts`: drop `region_id` from `CreateAlertPayload` and `buildCreatePayload`; drop `regionId` from `AlertFormValues`, `blankFormValues`, and `formValuesFromAlert`; delete `ALL_REGIONS`, `filterByRegion`, and `selectedRegion`. Keep `regionById` and `regionName`.

`AlertForm.svelte`: remove the region `<select>` entirely and take `region: Region` as a prop. The form previously disabled the select when editing; now the region is simply not a form field, which is the honest rendering of "region is immutable through the API".

- [ ] **Step 4: Move the routes**

Create `routes/regions/[region]/alerts/+page.ts`:

```ts
export const load: PageLoad = async ({ parent, params }) => {
	// The session guard first, as before: a signed-out visitor must not fire
	// requests that can only 401.
	await parent();
	try {
		const [alerts, region] = await Promise.all([
			api.get<Alert[]>(regionPath(params.region, '/alerts')),
			// The single-region endpoint, not the list: it is the only one
			// that carries `features`, and it 404s for a region this
			// operator cannot reach -- which is what turns a hand-edited URL
			// into the error page rather than an empty list.
			api.get<Region>(regionPath(params.region, '')),
		]);
		return { alerts, region };
	} catch (err) {
		toLoadError(err);
	}
};
```

The list page drops the region `<select>` (the region is the page) and shows the region name in its heading. `routes/regions/[region]/alerts/[id]/+page.ts` fetches the alert, the region, and — only when `hasFeature(region, 'pushes')` — the pushes and audience; the push card renders its "not configured" state from `hasFeature` rather than from a swallowed 404. `routes/regions/[region]/alerts/new/+page.ts` fetches only the region.

Delete `routes/alerts/**` and move `edit-page.svelte.test.ts` and `push-card.svelte.test.ts` into the new directory, updating their fixtures.

- [ ] **Step 5: The region picker**

`routes/+page.ts` loads `GET /regions`; `routes/+page.svelte` uses `pickRegion(regions, localStorage.getItem(LAST_REGION_KEY))` and `goto`s `/regions/{id}/alerts` when it returns a region, otherwise renders a list of region links. Every navigation to a region's pages writes `LAST_REGION_KEY`.

Guard the `localStorage` access in a `try`/`catch`: it throws outright in some privacy modes, and a remembered region is a convenience that must never take down the only screen that can recover from having no region.

`+layout.svelte`: the "Alerts" nav link points at `resolve('/')` (the picker, which forwards); "Regions" is unchanged.

- [ ] **Step 6: Run the frontend checks**

```bash
cd web/admin && npm run check && npm run lint && npm run test:unit
```
Expected: all pass.

- [ ] **Step 7: Verify against the real server**

```bash
make web && make run ARGS="--db sidecar.db"
```
Then in a browser: `/admin` forwards or shows the picker; `/admin/regions/1/alerts` lists that region's alerts; creating an alert lands on its detail page; `/admin/alerts/7` shows the SPA's not-found page. Confirm the push card says "not configured" when gorush is unset.

- [ ] **Step 8: Prove a test can fail**

Change `pickRegion` to `if (regions.length === 1 && regions[0].id)`, confirm the "honours a remembered region 0" and "only region" tests fail for Tampa, then revert.

- [ ] **Step 9: Commit**

```bash
make check
git add web/admin
git commit -m "feat(admin-ui): region-scoped SPA routes and a region picker"
```

---

## Task 14: Documentation

**Files:**
- Modify: `README.md`
- Modify: `.env.example` only if a new key is introduced (none is — `Deps.APIKeys` is always wired)

`specification/openapi.yaml` does not describe the admin API today and is deliberately left as is (spec section 9).

- [ ] **Step 1: Update the admin API route list**

Every admin path in the README gains its `/regions/{regionId}` segment. Search for `\/api\/admin\/v1` and update each occurrence, including the "Admin API" subsection under "Sending alerts as push notifications" (README line ~142).

- [ ] **Step 2: Add "Region API keys and service principals"**

A new subsection under the admin API section covering:

- the two credential formats (`obask_<regionID>_…`, `obasp_…`) and that only a hash is stored;
- the CLI: `key create/list/revoke`, `principal create/list/revoke`, with a worked provisioning example;
- the provisioning flow: an operator mints a principal, the consumer mints per-region keys with it, and a key is scoped to exactly one region;
- rotation: mint → swap → revoke, and that `last_used_at` is touched at most hourly so it can be up to an hour stale;
- the audit columns (`created_by_*`, `revoked_by_*`) and the `key list --minted-by-principal N` triage query;
- **what each leaked credential can do**, copied faithfully from spec sections 2.1 and 2.2 — a region key reads and writes one region's resources including setting its OBA API key, but cannot send a push, reach another region, or mint keys; a service principal can mint a key for any published region and revoke every key in the deployment, but can read no tenant data. Recovery is `principal revoke` (which takes its keys with it) plus re-provisioning.

- [ ] **Step 3: Add the deployment note**

In the existing Deployment section, beside the `X-Forwarded-Proto` and `Host` notes:

> The fronting proxy must not log the `Authorization` header. Region API keys are bearer credentials sent on every request; an access log that captures request headers turns log retention into credential retention.

- [ ] **Step 4: Note the SPA URL change and add the OBACloud pointer**

Under "Admin UI": the SPA's pages are now `/admin/regions/{regionId}/alerts…` and `/admin` is a region picker; old `/admin/alerts/…` bookmarks show the not-found page.

Add a short "OBACloud integration" subsection summarising that the Rails side holds one service principal per sidecar deployment and one region key per region, and pointing at `docs/superpowers/specs/2026-08-26-region-api-keys-and-admin-api-design.md` section 7 for the contract, which is documented but not built here.

- [ ] **Step 5: Verify every documented command and path**

Run each README command block against a scratch database and confirm the output matches what is written. Check every `/api/admin/v1/...` path in the README against `adminRoutes` — a documented path that is not in the table is a bug in one of them.

- [ ] **Step 6: Full check and commit**

```bash
make check && make test-tz && deploy/smoke.sh
git add README.md
git commit -m "docs: region API keys, the region-scoped admin API, and the SPA URL change"
```

---

## Done criteria

- `make check` and `make test-tz` pass.
- `web/admin`: `npm run check`, `npm run lint`, `npm run test:unit` pass.
- `TestRouteTable_TenancyWalk` covers all 40 routes with no missing fixture entries.
- `sidecar-admin key create --region N --name x` prints a key that authenticates against that region and 404s against every other.
- `sidecar-admin principal create` prints a key that can mint in any published region and is 403 everywhere else.
- The SPA works end to end at `/admin/regions/{id}/alerts`.
