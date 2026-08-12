# Admin API + SPA Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** An authenticated JSON admin API under `/api/admin/v1/` plus a SvelteKit SPA served at `/admin` from the Go binary, with users bootstrapped by `sidecar-admin user create`.

**Architecture:** New `internal/auth` domain package (argon2id passwords, opaque DB-backed session tokens) behind an `auth.Repository` implemented by the existing SQLite store; admin HTTP handlers in `internal/httpapi` reusing the existing `alerts.Repository`/`regions.Repository`; a static-adapter SvelteKit app in `web/admin/` embedded via `go:embed`.

**Tech Stack:** Go 1.26, `golang.org/x/crypto/argon2`, `golang.org/x/term`, sqlc + modernc.org/sqlite + goose, SvelteKit (Svelte 5, TypeScript, `@sveltejs/adapter-static`), vitest, Node (mise-managed).

**Normative spec:** `docs/superpowers/specs/2026-08-11-admin-api-spa-design.md`. Where this plan and the spec disagree, the spec governs — stop and flag it.

## Global Constraints

- Every timestamp column is epoch-seconds `INTEGER`. `DATETIME` anywhere is a defect (alerts spec §2.3).
- `time.Now` and `time.Local` are banned outside `cmd/` (forbidigo enforces). All new packages take injected `Now func() time.Time` / `now time.Time` parameters.
- Region id 0 is a real region (Tampa Bay). Never use 0 or nil-zero as "unset".
- sqlc queries: never mix `sqlc.arg()` with bare `?` in one query — all bare `?` here, matching the existing query files. `.sql` comments are ASCII-only (an em dash corrupts sqlc output file-wide).
- Domain types only outside `internal/store/sqlite` — nothing else may import `gen`.
- Session lifetime 30 days absolute; cookie name `sidecar_session`; login failure delay 500 ms in production, injected as `Deps.FailDelay` (0 in tests).
- Password min 12 bytes, max 512 bytes. Usernames are normalized (trim + lowercase) in Go, never via SQL collation; 1–64 chars, no internal whitespace.
- argon2id parameters: `m=19456` KiB, `t=2`, `p=1`, 16-byte salt, 32-byte key, PHC string format.
- API errors are JSON `{"error": "message"}`. Response timestamps are RFC 3339 UTC; request timestamps require an explicit offset.
- The rider feed endpoints (`/api/v1/regions/{id}/alerts*`) must remain unauthenticated and byte-identical in behavior.
- Frontend: SvelteKit `paths.base = '/admin'`; route dirs do NOT repeat the base; in-app links use `resolve()` from `$app/paths`; `ssr = false`, `prerender = false` everywhere, adapter `fallback: 'index.html'`; no `{@html}`; no `changeOrigin` in the vite proxy.
- Run `make check` before every commit. Frontend tasks also run `make web-check` once it exists (Task 9 wires it into `check`).

## File Structure

```
internal/auth/                      auth.go (types, errors, Repository), password.go,
                                    token.go, username.go + _test.go files
internal/store/sqlite/
  migrations/00002_users_sessions.sql
  queries/users.sql, queries/sessions.sql
  auth.go                           authRepo adapter + Store.Auth()
  gen/                              regenerated (committed)
internal/store/storetest/authtest.go  RunAuthRepository conformance suite
internal/httpapi/
  json.go                           writeJSON / writeJSONError / decodeJSON
  middleware.go                     cross-site guard, requireSession
  session.go                        login / logout / whoami
  admin_alerts.go, admin_regions.go admin JSON API
  spa.go                            embedded SPA server
  adminui/adminui.go                go:embed of dist/
  adminui/dist/.gitkeep             committed placeholder
cmd/sidecar-admin/users.go          user create/list/passwd/delete
web/admin/                          SvelteKit project (see Task 9)
```

---

### Task 1: `internal/auth` domain package

**Files:**
- Create: `internal/auth/auth.go`, `internal/auth/password.go`, `internal/auth/token.go`, `internal/auth/username.go`
- Test: `internal/auth/password_test.go`, `internal/auth/token_test.go`, `internal/auth/username_test.go`

**Interfaces:**
- Consumes: nothing (leaf package; stdlib + `golang.org/x/crypto/argon2`).
- Produces (later tasks rely on these exact names):
  - `type User struct { ID int64; Username string; PasswordHash string; CreatedAt, UpdatedAt time.Time }`
  - `type Session struct { TokenHash string; UserID int64; CreatedAt, ExpiresAt time.Time }`
  - `type Repository interface { ... }` (full text below)
  - `var ErrNotFound`, `var ErrUsernameTaken`
  - `const CookieName = "sidecar_session"`, `const SessionLifetime = 30 * 24 * time.Hour`
  - `func HashPassword(password string) (string, error)`
  - `func VerifyPassword(phc, password string) (bool, error)`
  - `const DummyPHC` (valid-format hash matching no password, for timing equalization)
  - `func ValidatePassword(password string) error`
  - `func NormalizeUsername(s string) string`, `func ValidateUsername(s string) error`
  - `func NewToken() (token, tokenHash string, err error)`, `func HashToken(token string) string`

- [ ] **Step 1: `go get golang.org/x/crypto@latest golang.org/x/term@latest`** (term is used in Task 8; fetch both now so go.mod churn happens once). Commit nothing yet.

- [ ] **Step 2: Write failing tests for password hashing** (`internal/auth/password_test.go`):

```go
package auth

import (
	"strings"
	"testing"
)

func TestHashVerifyRoundTrip(t *testing.T) {
	phc, err := HashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(phc, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Fatalf("unexpected PHC prefix: %s", phc)
	}
	ok, err := VerifyPassword(phc, "correct horse battery")
	if err != nil || !ok {
		t.Fatalf("want match, got ok=%v err=%v", ok, err)
	}
	ok, err = VerifyPassword(phc, "wrong password xx")
	if err != nil || ok {
		t.Fatalf("want mismatch, got ok=%v err=%v", ok, err)
	}
}

func TestHashesAreSalted(t *testing.T) {
	a, _ := HashPassword("same password 12")
	b, _ := HashPassword("same password 12")
	if a == b {
		t.Fatal("two hashes of one password must differ (random salt)")
	}
}

// Foreign parameters: a hash written with different cost params must verify
// with THOSE params, so raising defaults later never breaks old rows.
func TestVerifyForeignParameters(t *testing.T) {
	// m=8 t=1 p=1 hash of "pw" is cheap to compute inline for the fixture.
	phc := hashWithParams("legacy password!", 8, 1, 1)
	ok, err := VerifyPassword(phc, "legacy password!")
	if err != nil || !ok {
		t.Fatalf("want match with stored params, got ok=%v err=%v", ok, err)
	}
}

func TestVerifyRejectsGarbage(t *testing.T) {
	for _, phc := range []string{"", "$argon2id$nope", "$argon2i$v=19$m=8,t=1,p=1$AAAA$AAAA", "plaintext"} {
		if _, err := VerifyPassword(phc, "x"); err == nil {
			t.Errorf("VerifyPassword(%q) should error", phc)
		}
	}
}

func TestDummyPHCIsWellFormed(t *testing.T) {
	ok, err := VerifyPassword(DummyPHC, "anything at all")
	if err != nil {
		t.Fatalf("DummyPHC must parse cleanly: %v", err)
	}
	if ok {
		t.Fatal("DummyPHC must match no password")
	}
}

func TestValidatePassword(t *testing.T) {
	if err := ValidatePassword("elevenchars"); err == nil {
		t.Error("11 chars must fail")
	}
	if err := ValidatePassword("twelve chars"); err != nil {
		t.Errorf("12 chars must pass: %v", err)
	}
	if err := ValidatePassword(strings.Repeat("a", 513)); err == nil {
		t.Error("513 bytes must fail")
	}
}
```

`hashWithParams` is an internal helper the implementation exports within the package (lowercase) for this test.

- [ ] **Step 3: Run to verify failure.** Run: `go test ./internal/auth/`. Expected: FAIL (undefined symbols).

- [ ] **Step 4: Implement `internal/auth/password.go`:**

```go
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// argon2id parameters per current OWASP guidance (spec section 4.1). Raising
// them later is safe: VerifyPassword always uses the parameters stored in
// the row's own PHC string.
const (
	argonMemoryKiB = 19456
	argonTime      = 2
	argonThreads   = 1
	saltLen        = 16
	keyLen         = 32
)

// Password length bounds (spec section 4.1). Max exists because argon2 input
// length is attacker-controlled CPU cost.
const (
	MinPasswordLen   = 12
	MaxPasswordBytes = 512
)

// DummyPHC is a syntactically valid argon2id hash that matches no password.
// The login handler verifies against it when the username does not exist, so
// response timing does not reveal which usernames are real. The hash bytes
// are all zero, which no real key derivation produces.
const DummyPHC = "$argon2id$v=19$m=19456,t=2,p=1$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

// ValidatePassword enforces the length bounds shared by every surface (CLI
// today, any future one).
func ValidatePassword(password string) error {
	if len(password) < MinPasswordLen {
		return fmt.Errorf("password must be at least %d characters", MinPasswordLen)
	}
	if len(password) > MaxPasswordBytes {
		return fmt.Errorf("password must be at most %d bytes", MaxPasswordBytes)
	}
	return nil
}

// HashPassword derives an argon2id hash and encodes it in PHC string format.
func HashPassword(password string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: generate salt: %w", err)
	}
	return encodePHC(password, salt, argonMemoryKiB, argonTime, argonThreads), nil
}

func hashWithParams(password string, m uint32, t uint32, p uint8) string {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		panic(err) // test-only helper; crypto/rand failure is unrecoverable anyway
	}
	return encodePHC(password, salt, m, t, p)
}

func encodePHC(password string, salt []byte, m uint32, t uint32, p uint8) string {
	key := argon2.IDKey([]byte(password), salt, t, m, p, keyLen)
	b64 := base64.RawStdEncoding.EncodeToString
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, m, t, p, b64(salt), b64(key))
}

// VerifyPassword reports whether password matches the PHC-encoded hash,
// using the parameters stored in the hash itself. A malformed hash is an
// error; a clean mismatch is (false, nil).
func VerifyPassword(phc, password string) (bool, error) {
	parts := strings.Split(phc, "$")
	// "" / "argon2id" / "v=19" / "m=..,t=..,p=.." / salt / hash
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return false, errors.New("auth: malformed password hash")
	}
	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false, errors.New("auth: unsupported hash version")
	}
	var m, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false, errors.New("auth: malformed hash parameters")
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false, errors.New("auth: malformed hash salt")
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false, errors.New("auth: malformed hash value")
	}
	got := argon2.IDKey([]byte(password), salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}
```

- [ ] **Step 5: Run password tests.** Run: `go test ./internal/auth/ -run 'Password|Hash|Dummy|Foreign|Garbage|Salted'`. Expected: PASS.

- [ ] **Step 6: Write failing token + username tests:**

`internal/auth/token_test.go`:

```go
package auth

import "testing"

func TestNewTokenDistinctAndHashed(t *testing.T) {
	tok1, hash1, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	tok2, hash2, _ := NewToken()
	if tok1 == tok2 || hash1 == hash2 {
		t.Fatal("tokens must be unique")
	}
	if tok1 == hash1 {
		t.Fatal("hash must not equal the raw token")
	}
	if HashToken(tok1) != hash1 {
		t.Fatal("HashToken must reproduce the hash NewToken returned")
	}
	if len(hash1) != 64 {
		t.Fatalf("want 64 hex chars, got %d", len(hash1))
	}
}
```

`internal/auth/username_test.go`:

```go
package auth

import (
	"strings"
	"testing"
)

func TestNormalizeUsername(t *testing.T) {
	if got := NormalizeUsername("  Admin "); got != "admin" {
		t.Fatalf("got %q", got)
	}
}

func TestValidateUsername(t *testing.T) {
	for _, bad := range []string{"", "has space", "a\tb", strings.Repeat("x", 65)} {
		if err := ValidateUsername(NormalizeUsername(bad)); err == nil {
			t.Errorf("ValidateUsername(%q) should fail", bad)
		}
	}
	for _, good := range []string{"admin", "a", "kaylee.frye", strings.Repeat("x", 64)} {
		if err := ValidateUsername(NormalizeUsername(good)); err != nil {
			t.Errorf("ValidateUsername(%q): %v", good, err)
		}
	}
}
```

- [ ] **Step 7: Run to verify failure, then implement.**

`internal/auth/token.go`:

```go
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// NewToken mints an opaque 256-bit session token and its storage hash. The
// raw token goes only into the cookie; only tokenHash is ever stored, so a
// leaked database copy cannot be replayed into a live session (spec 2.1).
func NewToken() (token, tokenHash string, err error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", fmt.Errorf("auth: generate token: %w", err)
	}
	token = base64.RawURLEncoding.EncodeToString(raw)
	return token, HashToken(token), nil
}

// HashToken returns the hex SHA-256 of a raw token, the sessions.token_hash
// column value.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
```

`internal/auth/username.go`:

```go
package auth

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// NormalizeUsername trims and lowercases, in Go rather than SQL collation so
// SQLite and Postgres agree (spec section 3.2).
func NormalizeUsername(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// ValidateUsername checks an already-normalized username: 1-64 characters,
// no whitespace.
func ValidateUsername(s string) error {
	if s == "" {
		return errors.New("username must not be empty")
	}
	if len(s) > 64 {
		return fmt.Errorf("username must be at most 64 characters, got %d", len(s))
	}
	if strings.IndexFunc(s, unicode.IsSpace) >= 0 {
		return errors.New("username must not contain whitespace")
	}
	return nil
}
```

`internal/auth/auth.go`:

```go
// Package auth holds the admin authentication domain: users, sessions,
// password hashing, and opaque session tokens. Nothing here performs I/O or
// reads the clock; storage lives behind Repository, and every time value is
// injected (alerts spec section 2.3 applies).
package auth

import (
	"context"
	"errors"
	"time"
)

// CookieName is the session cookie the HTTP layer sets and reads.
const CookieName = "sidecar_session"

// SessionLifetime is the absolute session duration; there is no sliding
// renewal (spec section 4.2).
const SessionLifetime = 30 * 24 * time.Hour

// ErrNotFound is returned when a user or session lookup finds no live row.
var ErrNotFound = errors.New("user or session not found")

// ErrUsernameTaken is returned by CreateUser on a duplicate username, mapped
// from the UNIQUE violation inside the store, never a racy pre-check.
var ErrUsernameTaken = errors.New("username already taken")

// User is an admin account. PasswordHash is a PHC-format argon2id string.
type User struct {
	ID           int64
	Username     string
	PasswordHash string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Session is one login. TokenHash is the hex SHA-256 of the cookie token;
// the raw token is never stored.
type Session struct {
	TokenHash string
	UserID    int64
	CreatedAt time.Time
	ExpiresAt time.Time
}

// Repository stores users and sessions. Implementations must be safe for
// concurrent use, must normalize usernames with NormalizeUsername on every
// write and lookup, and must enforce ValidateUsername on create.
type Repository interface {
	CreateUser(ctx context.Context, username, passwordHash string, now time.Time) (User, error)
	GetUserByUsername(ctx context.Context, username string) (User, error)
	GetUserByID(ctx context.Context, id int64) (User, error)
	ListUsers(ctx context.Context) ([]User, error)
	DeleteUser(ctx context.Context, username string) error
	UpdatePassword(ctx context.Context, username, passwordHash string, now time.Time) error

	CreateSession(ctx context.Context, tokenHash string, userID int64, now, expiresAt time.Time) error
	// GetSession returns ErrNotFound for unknown OR expired tokens; expiry
	// is evaluated against the passed now, never the database clock. When it
	// observes an expired row it DELETES it before returning ErrNotFound --
	// a deliberate write inside a read path, part of this contract (the
	// storetest suite asserts it), so every implementation including a
	// future Postgres one must do the same.
	GetSession(ctx context.Context, tokenHash string, now time.Time) (Session, error)
	DeleteSession(ctx context.Context, tokenHash string) error
	// DeleteUserSessions revokes every session for a user; user passwd calls
	// it so a password change locks out whoever held the old password.
	DeleteUserSessions(ctx context.Context, userID int64) (int64, error)
	DeleteExpiredSessions(ctx context.Context, now time.Time) (int64, error)
}
```

- [ ] **Step 8: Run the full gate.** Run: `go test ./internal/auth/ && make check`. Expected: PASS. (A session expiring *at* now is dead — that contract lands in the adapter, Task 3.)

- [ ] **Step 9: Commit.**

```bash
git add internal/auth go.mod go.sum
git commit -m "Add auth domain package: argon2id passwords, session tokens, repository contract"
```

---

### Task 2: Schema migration and sqlc queries

**Files:**
- Create: `internal/store/sqlite/migrations/00002_users_sessions.sql`
- Create: `internal/store/sqlite/queries/users.sql`, `internal/store/sqlite/queries/sessions.sql`
- Modify: `internal/store/sqlite/gen/` (regenerated, committed)

**Interfaces:**
- Consumes: existing migration/queries layout, `make generate` / `make generate-check`.
- Produces: `gen.Queries` methods `CreateUser, GetUserByUsername, GetUserByID, ListUsers, DeleteUser, UpdateUserPassword, CreateSession, GetSessionRow, DeleteSession, DeleteUserSessions, DeleteExpiredSessions` used by Task 3.

- [ ] **Step 1: Write the migration** `internal/store/sqlite/migrations/00002_users_sessions.sql` (ASCII-only comments):

```sql
-- +goose Up
-- Admin authentication (design spec section 3). Every timestamp is epoch
-- seconds in an INTEGER column, never DATETIME.
CREATE TABLE users (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  username      TEXT NOT NULL UNIQUE,
  -- PHC-formatted argon2id string, self-describing so parameters can be
  -- raised per-row without a migration.
  password_hash TEXT NOT NULL,
  created_at    INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL
);

CREATE TABLE sessions (
  -- Hex SHA-256 of the opaque token. The raw token never touches the DB.
  token_hash  TEXT PRIMARY KEY,
  user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_at  INTEGER NOT NULL,
  expires_at  INTEGER NOT NULL
);

CREATE INDEX sessions_user_idx ON sessions (user_id);
CREATE INDEX sessions_expires_idx ON sessions (expires_at);

-- +goose Down
DROP TABLE sessions;
DROP TABLE users;
```

- [ ] **Step 2: Write the queries.** All-bare-`?` placeholders (never mix with `sqlc.arg()`), ASCII-only comments.

`internal/store/sqlite/queries/users.sql`:

```sql
-- name: CreateUser :one
INSERT INTO users (username, password_hash, created_at, updated_at)
VALUES (?, ?, ?, ?)
RETURNING *;

-- name: GetUserByUsername :one
SELECT * FROM users WHERE username = ?;

-- name: GetUserByID :one
SELECT * FROM users WHERE id = ?;

-- name: ListUsers :many
SELECT * FROM users ORDER BY username;

-- name: DeleteUser :execrows
DELETE FROM users WHERE username = ?;

-- name: UpdateUserPassword :execrows
UPDATE users SET password_hash = ?, updated_at = ? WHERE username = ?;
```

`internal/store/sqlite/queries/sessions.sql`:

```sql
-- name: CreateSession :exec
INSERT INTO sessions (token_hash, user_id, created_at, expires_at)
VALUES (?, ?, ?, ?);

-- Expiry is NOT filtered here: the adapter evaluates it against the
-- injected clock and deletes expired rows itself (delete-on-read contract).
-- name: GetSessionRow :one
SELECT * FROM sessions WHERE token_hash = ?;

-- name: DeleteSession :execrows
DELETE FROM sessions WHERE token_hash = ?;

-- name: DeleteUserSessions :execrows
DELETE FROM sessions WHERE user_id = ?;

-- name: DeleteExpiredSessions :execrows
DELETE FROM sessions WHERE expires_at <= ?;
```

- [ ] **Step 3: Generate and inspect.** Run: `make generate && make generate-check && go build ./...`. Expected: clean build; `gen/` gains `users.sql.go`, `sessions.sql.go`, and `models.go` gains `User`, `Session`. Verify `gen.User.CreatedAt` is `int64` (not `time.Time`) — if it isn't, the schema violated the INTEGER rule.

- [ ] **Step 4: Migration smoke test.** Add to `internal/store/sqlite/store_test.go`:

```go
func TestMigrateCreatesAuthTables(t *testing.T) {
	s := newTestStore(t) // existing helper that opens a temp DB and migrates
	for _, table := range []string{"users", "sessions"} {
		var n int
		err := s.DB().QueryRow( // if no DB accessor exists, query via a repo-less *sql.DB opened on the same path
			"SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&n)
		if err != nil || n != 1 {
			t.Fatalf("table %s missing after migrate (err=%v)", table, err)
		}
	}
}
```

If `store_test.go` has no `newTestStore`/DB accessor, follow whatever pattern its existing tests use to reach the migrated database — do not add a public `DB()` method just for this; an unexported test helper in the package is fine (the test file is `package sqlite`).

- [ ] **Step 5: Run and commit.** Run: `make check`. Expected: PASS.

```bash
git add internal/store/sqlite
git commit -m "Add users and sessions schema and sqlc queries"
```

---

### Task 3: `auth.Repository` conformance suite + SQLite adapter

**Files:**
- Create: `internal/store/storetest/authtest.go`
- Create: `internal/store/sqlite/auth.go`
- Modify: `internal/store/sqlite/store_test.go` (wire the suite)

**Interfaces:**
- Consumes: Task 1's `auth` package, Task 2's `gen` queries, storetest's existing `base` time fixture pattern.
- Produces: `storetest.RunAuthRepository(t *testing.T, newRepo func(*testing.T) auth.Repository)`; `(*sqlite.Store).Auth() auth.Repository`.

- [ ] **Step 1: Write the conformance suite** `internal/store/storetest/authtest.go`. Follow the existing file's conventions (fixed `base` instant, no wall clock — storetest is a non-test package so forbidigo applies). Subtests, each getting a fresh repo:

```go
// RunAuthRepository exercises an auth.Repository against the behavioral
// contract every engine must satisfy.
func RunAuthRepository(t *testing.T, newRepo func(*testing.T) auth.Repository) {
	t.Helper()
	t.Run("UserRoundTrip", ...)                 // CreateUser -> GetUserByUsername/GetUserByID; fields incl. CreatedAt UTC epoch round-trip
	t.Run("UsernameNormalizedOnWriteAndRead", ...) // CreateUser("Admin ") then GetUserByUsername("  ADMIN") finds it; stored Username == "admin"
	t.Run("DuplicateUsernameIsErrUsernameTaken", ...) // second create ("admin", then "ADMIN") -> errors.Is(err, auth.ErrUsernameTaken)
	t.Run("CreateRejectsInvalidUsername", ...)  // "" and "has space" -> error, no row
	t.Run("UnknownUserIsErrNotFound", ...)      // Get/Delete/UpdatePassword on missing username -> auth.ErrNotFound
	t.Run("SessionRoundTrip", ...)              // CreateSession -> GetSession(now=base) returns it; ExpiresAt UTC
	t.Run("SessionExpiryBoundary", ...)         // expires base+30d: GetSession(now=base+30d-1s) alive; GetSession(now=base+30d) -> ErrNotFound (AT expiry is dead)
	t.Run("ExpiredSessionDeletedOnRead", ...)   // after the ErrNotFound above, a later GetSession with now=base (before expiry!) STILL ErrNotFound: the row is gone
	t.Run("DeleteUserSessionsRevokesAllAndOnlyTheirs", ...) // 2 users, 2+1 sessions; delete user A's -> returns 2, A's dead, B's alive
	t.Run("DeleteExpiredSessionsCount", ...)    // 3 sessions, 2 past expiry -> returns 2, live one survives
	t.Run("UserDeleteCascadesToSessions", ...)  // DeleteUser -> their session ErrNotFound
	t.Run("TokenHashIsNotTheToken", ...)        // create via auth.NewToken(); GetSession(rawToken) -> ErrNotFound; GetSession(hash) -> found
	t.Run("PasswordUpdatePersists", ...)        // UpdatePassword -> GetUserByUsername sees new hash and bumped UpdatedAt
}
```

Write each subtest body in full — the sketches above name the behavior; the bodies follow the existing `authtest`-sibling style in `storetest.go` (create repo, act, assert with `t.Fatalf`). `ExpiredSessionDeletedOnRead` is the spec's delete-on-read contract and must be a real assertion, not a comment.

- [ ] **Step 2: Wire into sqlite tests** (`store_test.go`):

```go
func TestAuthRepositoryConformance(t *testing.T) {
	storetest.RunAuthRepository(t, func(t *testing.T) auth.Repository {
		return newTestStore(t).Auth()
	})
}
```

- [ ] **Step 3: Run to verify failure.** Run: `go test ./internal/store/...`. Expected: FAIL — `Store.Auth` undefined.

- [ ] **Step 4: Implement the adapter** `internal/store/sqlite/auth.go`:

```go
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/OneBusAway/sidecar/internal/auth"
	"github.com/OneBusAway/sidecar/internal/store/sqlite/gen"
)

// Auth returns the auth.Repository backed by this store.
func (s *Store) Auth() auth.Repository {
	return &authRepo{db: s.db, q: s.q}
}

type authRepo struct {
	db *sql.DB
	q  *gen.Queries
}

func userFromRow(u gen.User) auth.User {
	return auth.User{
		ID:           u.ID,
		Username:     u.Username,
		PasswordHash: u.PasswordHash,
		CreatedAt:    unixToTime(u.CreatedAt),
		UpdatedAt:    unixToTime(u.UpdatedAt),
	}
}

// CreateUser normalizes and validates here, not just in callers, for the
// same reason alertRepo.Create enforces AgencyID: the Repository contract
// protects every caller, including ones not written yet.
func (r *authRepo) CreateUser(ctx context.Context, username, passwordHash string, now time.Time) (auth.User, error) {
	username = auth.NormalizeUsername(username)
	if err := auth.ValidateUsername(username); err != nil {
		return auth.User{}, fmt.Errorf("sqlite: create user: %w", err)
	}
	if passwordHash == "" {
		return auth.User{}, errors.New("sqlite: create user: password hash must not be empty")
	}
	ts := now.Unix()
	row, err := r.q.CreateUser(ctx, gen.CreateUserParams{
		Username: username, PasswordHash: passwordHash, CreatedAt: ts, UpdatedAt: ts,
	})
	if err != nil {
		// modernc.org/sqlite exposes constraint failures via the error
		// string; matching the full "UNIQUE constraint failed: <table.col>"
		// text keeps this specific to the username constraint.
		if strings.Contains(err.Error(), "UNIQUE constraint failed: users.username") {
			return auth.User{}, fmt.Errorf("sqlite: create user %q: %w", username, auth.ErrUsernameTaken)
		}
		return auth.User{}, fmt.Errorf("sqlite: create user %q: %w", username, err)
	}
	return userFromRow(row), nil
}

func (r *authRepo) GetUserByUsername(ctx context.Context, username string) (auth.User, error) {
	username = auth.NormalizeUsername(username)
	row, err := r.q.GetUserByUsername(ctx, username)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return auth.User{}, fmt.Errorf("sqlite: get user %q: %w", username, auth.ErrNotFound)
		}
		return auth.User{}, fmt.Errorf("sqlite: get user %q: %w", username, err)
	}
	return userFromRow(row), nil
}

// GetUserByID, ListUsers, DeleteUser, UpdatePassword follow the exact same
// mapping pattern (ErrNoRows / zero rows affected -> auth.ErrNotFound;
// DeleteUser and UpdatePassword normalize the username first and check the
// :execrows count).

func (r *authRepo) CreateSession(ctx context.Context, tokenHash string, userID int64, now, expiresAt time.Time) error {
	if err := r.q.CreateSession(ctx, gen.CreateSessionParams{
		TokenHash: tokenHash, UserID: userID,
		CreatedAt: now.Unix(), ExpiresAt: expiresAt.Unix(),
	}); err != nil {
		return fmt.Errorf("sqlite: create session: %w", err)
	}
	return nil
}

// GetSession implements the delete-on-read contract: an observed expired row
// is removed before ErrNotFound is returned (auth.Repository doc comment).
func (r *authRepo) GetSession(ctx context.Context, tokenHash string, now time.Time) (auth.Session, error) {
	row, err := r.q.GetSessionRow(ctx, tokenHash)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return auth.Session{}, fmt.Errorf("sqlite: get session: %w", auth.ErrNotFound)
		}
		return auth.Session{}, fmt.Errorf("sqlite: get session: %w", err)
	}
	if row.ExpiresAt <= now.Unix() {
		if _, err := r.q.DeleteSession(ctx, tokenHash); err != nil {
			return auth.Session{}, fmt.Errorf("sqlite: delete expired session: %w", err)
		}
		return auth.Session{}, fmt.Errorf("sqlite: get session: %w", auth.ErrNotFound)
	}
	return auth.Session{
		TokenHash: row.TokenHash, UserID: row.UserID,
		CreatedAt: unixToTime(row.CreatedAt), ExpiresAt: unixToTime(row.ExpiresAt),
	}, nil
}

// DeleteSession maps zero rows to ErrNotFound. DeleteUserSessions and
// DeleteExpiredSessions return the count and do NOT error on zero -- bulk
// deletes are idempotent by design.
```

Write the elided methods in full following the shown patterns.

- [ ] **Step 5: Run to green.** Run: `go test ./internal/store/... && make check`. Expected: PASS, including `ExpiredSessionDeletedOnRead`.

- [ ] **Step 6: Mutation check (required, not optional).** Temporarily break the adapter — change `row.ExpiresAt <= now.Unix()` to `<` — and confirm `SessionExpiryBoundary` fails; revert. Temporarily remove the `DeleteSession` call inside `GetSession` and confirm `ExpiredSessionDeletedOnRead` fails; revert. If either mutation survives, the suite is vacuous — fix the test, not the code.

- [ ] **Step 7: Commit.**

```bash
git add internal/store
git commit -m "Add auth repository: conformance suite and SQLite adapter"
```

---

### Task 4: Alerts repository additions the API needs

**Files:**
- Modify: `internal/alerts/alert.go` (add `CreatedAt`, `UpdatedAt` to `Alert`)
- Modify: `internal/alerts/repository.go` (add `DeleteTranslation`)
- Modify: `internal/store/sqlite/queries/alerts.sql`, `internal/store/sqlite/store.go`
- Modify: `internal/store/storetest/storetest.go`

**Interfaces:**
- Consumes: existing alerts stack.
- Produces: `Alert.CreatedAt time.Time`, `Alert.UpdatedAt time.Time` (populated by every read path); `Repository.DeleteTranslation(ctx context.Context, alertID int64, language string) error` — deletes ALL field rows for the normalized language, `ErrNotFound` when nothing was deleted.

- [ ] **Step 1: Failing storetest cases.** Add to `RunAlertRepository`'s subtest list and write bodies:
  - `AlertTimestampsPopulated`: after `Create` at `base`, `Get` returns `CreatedAt == base` and `UpdatedAt == base`, both `time.UTC` location; after `Update` at `base.Add(time.Hour)`, `UpdatedAt` advances and `CreatedAt` does not.
  - `DeleteTranslationRemovesBothFields`: upsert header + description translations for `"es"`, `DeleteTranslation(id, "ES")` (note the case — normalization is under test), then `Get` shows zero translations; a second delete returns `alerts.ErrNotFound`.

- [ ] **Step 2: Run to verify failure.** Run: `go test ./internal/store/...`. Expected: FAIL (undefined field/method).

- [ ] **Step 3: Implement.**
  - `alert.go`: add `CreatedAt time.Time` and `UpdatedAt time.Time` to `Alert` (after `IsTest`, before `Translations`).
  - `repository.go`: add to the interface:

```go
	// DeleteTranslation removes every field row for one language on one
	// alert (language is normalized first). It returns ErrNotFound when no
	// rows matched, so the HTTP layer can 404.
	DeleteTranslation(ctx context.Context, alertID int64, language string) error
```

  - `queries/alerts.sql`: append (ASCII comment only):

```sql
-- name: DeleteAlertTranslations :execrows
DELETE FROM alert_translations WHERE alert_id = ? AND language = ?;
```

  - `store.go`: populate `CreatedAt: unixToTime(a.CreatedAt), UpdatedAt: unixToTime(a.UpdatedAt)` in `alertFromRow`, and add:

```go
func (r *alertRepo) DeleteTranslation(ctx context.Context, alertID int64, language string) error {
	n, err := r.q.DeleteAlertTranslations(ctx, gen.DeleteAlertTranslationsParams{
		AlertID: alertID, Language: alerts.NormalizeLanguage(language),
	})
	if err != nil {
		return fmt.Errorf("sqlite: delete translations for alert %d: %w", alertID, err)
	}
	if n == 0 {
		return fmt.Errorf("sqlite: delete translations for alert %d: %w", alertID, alerts.ErrNotFound)
	}
	return nil
}
```

  Run `make generate` after editing the `.sql` file.

- [ ] **Step 4: Fix fallout.** Existing storetest subtests that compare whole `Alert` structs may now fail on the new fields — update their expected values to assert the timestamps rather than zeroing them out. Run: `make check`. Expected: PASS.

- [ ] **Step 5: Commit.**

```bash
git add internal/alerts internal/store
git commit -m "Expose alert timestamps and translation deletion through the repository"
```

---

### Task 5: httpapi authentication — session endpoints and middleware

**Files:**
- Create: `internal/httpapi/json.go`, `internal/httpapi/middleware.go`, `internal/httpapi/session.go`
- Modify: `internal/httpapi/router.go`
- Test: `internal/httpapi/session_test.go`, `internal/httpapi/middleware_test.go`

**Interfaces:**
- Consumes: `auth` package (Task 1), `auth.Repository` (Task 3).
- Produces (Task 6 and 7 rely on these):
  - `Deps` gains `Auth auth.Repository`, `FailDelay time.Duration`, `AdminUI fs.FS` (field added now, used in Task 7).
  - `func writeJSON(w http.ResponseWriter, logger *slog.Logger, status int, v any)`
  - `func writeJSONError(w http.ResponseWriter, logger *slog.Logger, status int, msg string)`
  - `func decodeJSON(w http.ResponseWriter, r *http.Request, maxBytes int64, dst any) error` (wraps `http.MaxBytesReader` + `json.Decoder` with `DisallowUnknownFields` NOT set — unknown fields are ignored)
  - `func (h *authMiddleware) requireSession(next http.Handler) http.Handler` and `func userFrom(ctx context.Context) (auth.User, bool)`
  - `func crossSiteGuard(logger *slog.Logger, next http.Handler) http.Handler`
  - Routes: `POST/DELETE/GET /api/admin/v1/session` (admin routes registered only when `deps.Auth != nil`, so existing feed tests with a zero `Deps` keep passing).

- [ ] **Step 1: Write failing middleware tests** (`middleware_test.go`). Table-driven over `httptest.NewRequest`:
  - crossSiteGuard: GET always passes; POST with `Sec-Fetch-Site: same-origin` and `none` pass; `cross-site`, `same-site` → 403 JSON; no Sec-Fetch-Site + `Origin: http://evil.test` vs `Host: sidecar.test` → 403; matching Origin host → pass; no headers at all → pass (curl); **rewritten-proxy-Host case**: `Origin: https://alerts.example.org`, `Host: localhost:8080` → 403 whose body says `cross-site request rejected` (this is the legible-failure test the spec §4.4 requires); malformed Origin → 403.
  - requireSession: no cookie → 401 `{"error":"authentication required"}`; garbage token → 401; valid token → next handler runs and `userFrom(ctx)` returns the user; expired token → 401 (use a fake `auth.Repository` stub in the test file — define `type stubAuth struct{ ... }` implementing the interface over maps; do not spin up SQLite for handler tests).

- [ ] **Step 2: Write failing session-endpoint tests** (`session_test.go`), using the same stub repo and a fixed `Now`:
  - login success → 200 `{"username":"admin"}`, `Set-Cookie` has `sidecar_session=`, `HttpOnly`, `SameSite=Lax`, `Path=/`, `Max-Age=2592000`, no `Secure` on plain HTTP; with `X-Forwarded-Proto: https` → `Secure` present; a session row was created whose hash != cookie value.
  - login unknown user and wrong password → both exactly 401 `{"error":"invalid credentials"}` (assert byte-equal bodies — the responses must not distinguish the cases).
  - login with `FailDelay: 5 * time.Millisecond` → failure path takes ≥5ms (measure with `time.Since` — allowed in `_test.go`? **No**: forbidigo bans `time.Now` outside cmd/ including test files of internal packages. Use the stub's call-count instead: give `Deps` a `Sleep func(time.Duration)` — NO. Simplest compliant design: `FailDelay` is plumbed to `time.Sleep` in the handler, and the test asserts the *configured* path by injecting `Sleep func(time.Duration)` into `Deps` — set it in tests to record the duration. Production leaves `Sleep` nil and `NewRouter` defaults it to `time.Sleep`.) So: `Deps` also gains `Sleep func(time.Duration)`; test asserts `Sleep` was called with exactly `FailDelay` on failure and NOT called on success.
  - login body over 8 KB → 400; malformed JSON → 400; empty username/password → 401 (same generic message — do not leak validation detail on the login path).
  - logout with valid cookie → 204, session row gone, `Set-Cookie` clears (MaxAge<0); logout with no/unknown cookie → still 204 (idempotent).
  - whoami logged in → 200 `{"username":...}`; logged out → 401.
  - login success calls `DeleteExpiredSessions` (assert via stub counter).

- [ ] **Step 3: Run to verify failure.** Run: `go test ./internal/httpapi/`. Expected: FAIL.

- [ ] **Step 4: Implement.** Key excerpts (write complete files around them):

`json.go`:

```go
func writeJSON(w http.ResponseWriter, logger *slog.Logger, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		logger.Warn("httpapi: encode json response", "err", err)
	}
}

func writeJSONError(w http.ResponseWriter, logger *slog.Logger, status int, msg string) {
	writeJSON(w, logger, status, map[string]string{"error": msg})
}

// decodeJSON reads at most maxBytes of body into dst. It returns a
// caller-safe error message; the HTTP layer maps any non-nil return to 400.
func decodeJSON(w http.ResponseWriter, r *http.Request, maxBytes int64, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	return nil
}
```

`middleware.go` — the guard applies to every admin route including login (spec §4.4):

```go
// crossSiteGuard rejects state-changing requests that a browser marked as
// cross-site. Applies to ALL admin routes including POST /session: login
// CSRF is cheap to close and login sits outside requireSession. Requires
// the deployment's reverse proxy to preserve the public Host header (spec
// section 4.4); the README documents that beside X-Forwarded-Proto.
func crossSiteGuard(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if sfs := r.Header.Get("Sec-Fetch-Site"); sfs != "" {
				if sfs != "same-origin" && sfs != "none" {
					writeJSONError(w, logger, http.StatusForbidden, "cross-site request rejected")
					return
				}
			} else if origin := r.Header.Get("Origin"); origin != "" {
				u, err := url.Parse(origin)
				if err != nil || u.Host != r.Host {
					writeJSONError(w, logger, http.StatusForbidden, "cross-site request rejected")
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}
```

`requireSession` reads the cookie, `auth.HashToken`, `deps.Auth.GetSession(ctx, hash, deps.Now())`, then `GetUserByID`; on any failure → 401 `authentication required`; on success stores the user with an unexported context key. `userFrom` retrieves it.

`session.go` login core:

```go
func (h *sessionHandler) login(w http.ResponseWriter, r *http.Request) {
	var req struct{ Username, Password string }
	if err := decodeJSON(w, r, 8192, &req); err != nil {
		writeJSONError(w, h.deps.Logger, http.StatusBadRequest, err.Error())
		return
	}
	fail := func() {
		h.deps.Sleep(h.deps.FailDelay)
		h.deps.Logger.Warn("httpapi: failed login", "username", auth.NormalizeUsername(req.Username), "remote", r.RemoteAddr)
		writeJSONError(w, h.deps.Logger, http.StatusUnauthorized, "invalid credentials")
	}
	user, err := h.deps.Auth.GetUserByUsername(r.Context(), req.Username)
	if errors.Is(err, auth.ErrNotFound) {
		// Burn the same argon2 cost as a real verification so timing does
		// not reveal which usernames exist (spec section 4.3).
		_, _ = auth.VerifyPassword(auth.DummyPHC, req.Password)
		fail()
		return
	}
	if err != nil {
		h.serverErrorJSON(w, "get user", err)
		return
	}
	ok, err := auth.VerifyPassword(user.PasswordHash, req.Password)
	if err != nil {
		h.serverErrorJSON(w, "verify password", err)
		return
	}
	if !ok {
		fail()
		return
	}
	token, hash, err := auth.NewToken()
	if err != nil {
		h.serverErrorJSON(w, "mint token", err)
		return
	}
	now := h.deps.Now()
	if err := h.deps.Auth.CreateSession(r.Context(), hash, user.ID, now, now.Add(auth.SessionLifetime)); err != nil {
		h.serverErrorJSON(w, "create session", err)
		return
	}
	if _, err := h.deps.Auth.DeleteExpiredSessions(r.Context(), now); err != nil {
		h.deps.Logger.Warn("httpapi: gc expired sessions", "err", err) // best-effort, never blocks login
	}
	http.SetCookie(w, sessionCookie(r, token, int(auth.SessionLifetime/time.Second)))
	writeJSON(w, h.deps.Logger, http.StatusOK, map[string]string{"username": user.Username})
}

func sessionCookie(r *http.Request, value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name: auth.CookieName, Value: value, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: maxAge,
		Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
	}
}
```

`router.go`: add to `Deps`: `Auth auth.Repository`, `FailDelay time.Duration`, `Sleep func(time.Duration)`, `AdminUI fs.FS`. In `NewRouter`, after the feed routes:

```go
	if deps.Sleep == nil {
		deps.Sleep = time.Sleep
	}
	if deps.Auth != nil {
		registerAdminRoutes(mux, deps) // session now; alerts/regions in Task 6
	}
```

`registerAdminRoutes` wraps each admin handler in `crossSiteGuard`, and everything except `POST /session` also in `requireSession`.

- [ ] **Step 5: Run to green.** Run: `go test ./internal/httpapi/ && make check`. Expected: PASS, including the existing feed tests untouched.

- [ ] **Step 6: Commit.**

```bash
git add internal/httpapi
git commit -m "Add admin session endpoints with CSRF guard and session middleware"
```

---

### Task 6: httpapi admin alerts + regions API

**Files:**
- Create: `internal/httpapi/admin_alerts.go`, `internal/httpapi/admin_regions.go`
- Modify: `internal/httpapi/router.go` (extend `registerAdminRoutes`)
- Modify: `cmd/sidecar/main.go` (add `_ "time/tzdata"` import — timezone validation must work on machines without a zoneinfo db, same reason `sidecar-admin` already imports it)
- Test: `internal/httpapi/admin_alerts_test.go`, `internal/httpapi/admin_regions_test.go`

**Interfaces:**
- Consumes: Tasks 4–5. Enum validation via `alerts.ParseCause/ParseEffect/ParseSeverity` (empty input already degrades to the `UNKNOWN_*` fallback — pass request strings straight through). Agency resolution mirrors the CLI: explicit value, else region `DefaultAgencyID`, else 400.
- Produces: the full route table from spec §5. JSON field names exactly as spec §5's example (`region_id`, `agency_id`, `header`, `description`, `url`, `cause`, `effect`, `severity`, `start_time`, `end_time`, `published`, `is_test`, `created_at`, `updated_at`, `translations[].language/header/description`).

Route registrations (all inside `registerAdminRoutes`, all guarded, all but session behind `requireSession`):

```
GET    /api/admin/v1/alerts
POST   /api/admin/v1/alerts
GET    /api/admin/v1/alerts/{id}
PATCH  /api/admin/v1/alerts/{id}
DELETE /api/admin/v1/alerts/{id}
POST   /api/admin/v1/alerts/{id}/publish
POST   /api/admin/v1/alerts/{id}/unpublish
PUT    /api/admin/v1/alerts/{id}/translations/{lang}
DELETE /api/admin/v1/alerts/{id}/translations/{lang}
GET    /api/admin/v1/regions
PATCH  /api/admin/v1/regions/{id}
```

Request/response shapes:

```go
// Response: one alert. Translations grouped per language (the storage rows
// are per-field; group them for the API).
type alertJSON struct {
	ID           int64             `json:"id"`
	RegionID     int64             `json:"region_id"`
	AgencyID     string            `json:"agency_id"`
	Header       string            `json:"header"`
	Description  string            `json:"description"`
	URL          string            `json:"url"`
	Cause        string            `json:"cause"`
	Effect       string            `json:"effect"`
	Severity     string            `json:"severity"`
	StartTime    string            `json:"start_time"`          // RFC 3339 UTC
	EndTime      *string           `json:"end_time"`            // null when open-ended
	Published    bool              `json:"published"`
	IsTest       bool              `json:"is_test"`
	CreatedAt    string            `json:"created_at"`
	UpdatedAt    string            `json:"updated_at"`
	Translations []translationJSON `json:"translations"`
}

type translationJSON struct {
	Language    string  `json:"language"`
	Header      *string `json:"header"`      // nil = no translation for that field
	Description *string `json:"description"`
}

type createAlertRequest struct {
	RegionID    *int64  `json:"region_id"` // pointer: region 0 is real, absent must be an error not region 0
	AgencyID    string  `json:"agency_id"`
	Header      string  `json:"header"`
	Description string  `json:"description"`
	URL         string  `json:"url"`
	Cause       string  `json:"cause"`
	Effect      string  `json:"effect"`
	Severity    string  `json:"severity"`
	StartTime   string  `json:"start_time"`
	EndTime     *string `json:"end_time"`
	IsTest      bool    `json:"is_test"`
}

type patchAlertRequest struct {
	AgencyID     *string `json:"agency_id"`
	Header       *string `json:"header"`
	Description  *string `json:"description"`
	URL          *string `json:"url"`
	Cause        *string `json:"cause"`
	Effect       *string `json:"effect"`
	Severity     *string `json:"severity"`
	StartTime    *string `json:"start_time"`
	EndTime      *string `json:"end_time"`
	ClearEndTime bool    `json:"clear_end_time"` // JSON cannot distinguish null from absent; explicit flag, SPA sends it
	IsTest       *bool   `json:"is_test"`
}
```

Behavioral requirements (each one is a test):

- Timestamp parsing: `time.Parse(time.RFC3339, s)` — a naive datetime fails the parse; the 400 message names the region's configured timezone, mirroring the CLI's `parseInstant` copy: `"...must be RFC 3339 with an explicit offset (e.g. 2026-08-15T14:00:00-07:00); region N is configured as <tz>"`. Implement `parseInstantJSON(s string, region regions.Region) (time.Time, error)` in `admin_alerts.go` (the CLI's version lives in `cmd/` and cannot be imported; the duplication is two lines of logic and each side's error copy is surface-appropriate).
- `POST /alerts`: missing `region_id` → 400; unknown region → 404; agency resolution as CLI (explicit → region default → 400 `"no agency_id given and region N has no default agency id; set one with PATCH /api/admin/v1/regions/N or pass agency_id"`); enum values through `ParseX` → 400 on invalid, listing valid values (ParseX's error already does); success → 201, `Location: /api/admin/v1/alerts/{id}`, body = full alertJSON.
- `PATCH /alerts/{id}`: maps 1:1 onto `alerts.Patch` (including `ClearEndTime`); enum and time validation as create; unknown id → 404; `ValidateWindow` violations from the repo → 400 (map the repo error: `errors.Is(err, alerts.ErrNotFound)` → 404, window/agency validation errors → 400 — the repo returns wrapped sentinel-less errors for these, so match on `alerts.ValidateWindow` by validating in the handler BEFORE calling Update, and treat any residual repo validation error as 400 via a `errValidation` check; keep it simple: handler pre-validates, repo re-validates as backstop, backstop failures map to 400 when `!errors.Is(err, alerts.ErrNotFound)` and the request came with those fields).
- Publish/unpublish: `SetPublished(id, true/false)` → 200 with the re-`Get` alert; unknown → 404.
- `GET /alerts`: `region` absent → all (nil filter); `region=abc` → 400; `region=999` (unknown) → 200 `[]` (filter, not lookup; test asserts `[]` not `null` — `emit_empty_slices` handles the repo, the handler must marshal `[]alertJSON{}`).
- Translations PUT: body `{header?, description?}`, both `*string`; both nil → 400 `"provide header and/or description"`; alert missing → 404; each provided field upserts `alerts.Translation{Language: lang, Field: FieldHeader/FieldDescription, Text: *v, SourceSHA256: alerts.SourceHash(<current English of that field>)}` (fetch the alert first); → 200 with the updated alertJSON. DELETE → `DeleteTranslation`, `ErrNotFound` → 404, success → 204.
- `GET /regions` → array of `{id, name, oba_base_url, sidecar_base_url, language, active, default_agency_id, timezone}`.
- `PATCH /regions/{id}`: `{default_agency_id?, timezone?}` both `*string`; both nil → 400; timezone validated with `time.LoadLocation` (400 naming the bad value); unknown region → 404; success → 200 region JSON. Absent fields keep current values (read region first, overlay, `SetLocalFields`).
- All handlers: repo errors that aren't 4xx map to 500 `{"error":"internal error"}` with details logged, never leaked.

- [ ] **Step 1: Write failing tests for the alerts surface** — table-driven against a real SQLite store (`sqlite.Open` on `t.TempDir()` path + `Migrate`), not stubs: these handlers' value is the mapping onto real repo semantics. Cover every behavioral bullet above. Login helper: create a user via `store.Auth()` + `auth.HashPassword`, hit login, reuse the cookie.
- [ ] **Step 2: Run to verify failure.** `go test ./internal/httpapi/`. Expected: FAIL.
- [ ] **Step 3: Implement `admin_alerts.go`** per the shapes above.
- [ ] **Step 4: Write failing tests for regions surface; implement `admin_regions.go`.**
- [ ] **Step 5: Add `_ "time/tzdata"` to `cmd/sidecar/main.go` imports.**
- [ ] **Step 6: Run to green.** `make check`. Expected: PASS.
- [ ] **Step 7: Commit.**

```bash
git add internal/httpapi cmd/sidecar
git commit -m "Add admin JSON API for alerts and regions"
```

---

### Task 7: Embedded SPA serving + server wiring

**Files:**
- Create: `internal/httpapi/adminui/adminui.go`, `internal/httpapi/adminui/dist/.gitkeep`
- Create: `internal/httpapi/spa.go`
- Modify: `internal/httpapi/router.go`, `cmd/sidecar/main.go`, `.gitignore`
- Test: `internal/httpapi/spa_test.go`

**Interfaces:**
- Consumes: `Deps.AdminUI fs.FS` (added Task 5).
- Produces: `adminui.FS() fs.FS`; routes `GET /admin` and `GET /admin/{path...}`.

- [ ] **Step 1: Write failing tests** (`spa_test.go`) using `fstest.MapFS` injected as `Deps.AdminUI` (this is why the handler takes `fs.FS`, not the embed directly):
  - `index.html` + `_app/immutable/chunk.abc.js` + `_app/version.json` + `favicon.png` in the map.
  - `GET /admin` → 200, body = index.html, `Cache-Control: no-cache`, `Content-Type` contains `text/html`.
  - `GET /admin/alerts/17` (no such file) → 200 index.html (SPA fallback), `no-cache`.
  - `GET /admin/_app/immutable/chunk.abc.js` → 200, `Cache-Control: public, max-age=31536000, immutable`.
  - `GET /admin/_app/version.json` → 200, `no-cache` (unhashed `_app` file — spec §6.5).
  - `GET /admin/_app/immutable/missing.js` → **404**, never index.html (spec §6.5's no-fallback rule).
  - `GET /admin/favicon.png` → 200, `no-cache`.
  - Empty `fstest.MapFS{}` (built-without-Node case) → 503, body contains `admin UI not built; run make web`.
  - `Deps.AdminUI == nil` → routes not registered (404 from the mux).

- [ ] **Step 2: Run to verify failure**, then implement.

`adminui/adminui.go`:

```go
// Package adminui embeds the built admin SPA. The dist directory is
// populated by `make web` and gitignored except for .gitkeep; the all:
// prefix is load-bearing twice over: plain go:embed patterns exclude files
// beginning with "." or "_", which would drop both .gitkeep and SvelteKit's
// entire _app/ output tree -- the build would succeed and every asset 404
// (design spec section 2.3).
package adminui

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// FS returns the embedded SPA rooted at the dist directory.
func FS() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		// Only reachable if the dist directory vanishes from the module,
		// which the committed .gitkeep prevents; a broken binary should say
		// so at boot, not serve mystery 500s.
		panic("adminui: embedded dist directory missing: " + err.Error())
	}
	return sub
}
```

`spa.go` core:

```go
// spaHandler serves the embedded admin SPA per spec section 6.5: real files
// as-is, unknown paths fall back to index.html for client-side routing --
// except under _app/, where a missing asset is a stale-deploy artifact and
// must 404 rather than hand HTML to a script tag.
func (h *spaHandler) serve(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/admin"), "/")
	if p == "" {
		p = "index.html"
	}
	if !fileExists(h.fs, p) {
		if strings.HasPrefix(p, "_app/") {
			http.NotFound(w, r)
			return
		}
		p = "index.html"
		if !fileExists(h.fs, p) {
			http.Error(w, "admin UI not built; run make web", http.StatusServiceUnavailable)
			return
		}
	}
	if strings.HasPrefix(p, "_app/immutable/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	http.ServeFileFS(w, r, h.fs, p)
}
```

(`fileExists` opens and stats, rejecting directories. Note `http.ServeFileFS` redirects a literal `/admin/index.html` request to `./`; that's harmless — do not special-case it, but don't write a test that requests `/admin/index.html` directly and expects 200.)

Router: `if deps.AdminUI != nil { h := &spaHandler{fs: deps.AdminUI, logger: deps.Logger}; mux.HandleFunc("GET /admin", h.serve); mux.HandleFunc("GET /admin/{path...}", h.serve) }`.

`cmd/sidecar/main.go`: wire `Auth: store.Auth(), AdminUI: adminui.FS(), FailDelay: 500 * time.Millisecond` into `httpapi.Deps`.

`.gitignore` append:

```
# Built admin SPA (populated by `make web`; .gitkeep keeps the embed dir)
internal/httpapi/adminui/dist/*
!internal/httpapi/adminui/dist/.gitkeep
web/admin/node_modules/
web/admin/.svelte-kit/
web/admin/build/
```

- [ ] **Step 3: Run to green.** `make check`. Expected: PASS.
- [ ] **Step 4: Commit.**

```bash
git add internal/httpapi cmd/sidecar .gitignore
git commit -m "Serve the embedded admin SPA with cache and fallback rules"
```

---

### Task 8: `sidecar-admin user` commands

**Files:**
- Create: `cmd/sidecar-admin/users.go`
- Modify: `cmd/sidecar-admin/main.go`, `cmd/sidecar-admin/commands.go` (run signature + dispatch)
- Test: `cmd/sidecar-admin/users_test.go` (+ mechanical updates to `commands_test.go`)

**Interfaces:**
- Consumes: `auth` package, `store.Auth()`.
- Produces: CLI surface `user create --username NAME [--password-stdin]`, `user passwd --username NAME [--password-stdin]`, `user list`, `user delete --username NAME [--force]`.

- [ ] **Step 1: Change `run`'s signature to accept stdin**: `run(stdin io.Reader, stdout, stderr io.Writer, args []string) error`; `main` passes `os.Stdin`. Update every existing call in `commands_test.go` mechanically (`strings.NewReader("")` where stdin is unused). Run `make check` to confirm nothing else broke. Commit this refactor alone:

```bash
git add cmd/sidecar-admin
git commit -m "Thread stdin through sidecar-admin's run for password input"
```

- [ ] **Step 2: Write failing tests** (`users_test.go`), all via `--password-stdin` (in-process, no TTY):
  - `create` with valid input → prints `created user alice`; row exists (verify via `store.Auth().GetUserByUsername`); password verifies with `auth.VerifyPassword`.
  - `create` with 11-char password → error mentioning `at least 12`; no row.
  - `create` duplicate (including case variant `Alice`) → error mentioning `already taken`.
  - `create` missing `--username` → error.
  - `list` → one line per user: `alice\t2026-...` (username + RFC 3339 UTC created time).
  - `passwd` → old password no longer verifies, new one does, **and all sessions for that user are gone** (seed one first via `store.Auth().CreateSession`); prints `password updated for alice; all sessions revoked`.
  - `passwd` unknown user → error with `not found`.
  - `delete` with two users → deletes, prints `deleted user alice`; their sessions gone (cascade).
  - `delete` the last user without `--force` → error explaining the admin UI becomes unreachable until a new `user create`, suggests `--force`; user still present.
  - `delete` last user with `--force` → succeeds.
  - `delete` unknown user → error (never silent success — established convention).
  - password with trailing `\n` via stdin → trimmed (verifies against the bare password).

- [ ] **Step 3: Run to verify failure**, then implement `users.go`. Structure mirrors `runRegion`/`runAlert` (flag.FlagSet per subcommand, `flag.ContinueOnError`, errors returned not printed, error text without the `sidecar-admin:` prefix — main adds the single prefix). Password acquisition:

```go
// readPassword returns the password from stdin (--password-stdin) or an
// interactive no-echo double prompt. The bare --password flag deliberately
// does not exist: passwords do not belong in shell history or ps output
// (spec section 7).
func readPassword(stdin io.Reader, stderr io.Writer, fromStdin bool) (string, error) {
	if fromStdin {
		b, err := io.ReadAll(stdin)
		if err != nil {
			return "", fmt.Errorf("read password from stdin: %w", err)
		}
		return strings.TrimRight(string(b), "\r\n"), nil
	}
	f, ok := stdin.(*os.File)
	if !ok || !term.IsTerminal(int(f.Fd())) {
		return "", errors.New("stdin is not a terminal; use --password-stdin")
	}
	for attempt := 1; attempt <= 3; attempt++ {
		fmt.Fprint(stderr, "Password: ")
		first, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(stderr)
		if err != nil {
			return "", fmt.Errorf("read password: %w", err)
		}
		if err := auth.ValidatePassword(string(first)); err != nil {
			fmt.Fprintln(stderr, err)
			continue
		}
		fmt.Fprint(stderr, "Confirm password: ")
		second, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(stderr)
		if err != nil {
			return "", fmt.Errorf("read password: %w", err)
		}
		if string(first) == string(second) {
			return string(first), nil
		}
		fmt.Fprintln(stderr, "passwords do not match")
	}
	return "", errors.New("giving up after 3 attempts")
}
```

`user create` flow: normalize+validate username → readPassword → `auth.ValidatePassword` (stdin path validates here; the interactive loop already did) → `auth.HashPassword` → `CreateUser` → print. `user passwd`: hash → `UpdatePassword` → `GetUserByUsername` for the ID → `DeleteUserSessions`. `user delete`: `GetUserByUsername` (existence + casing), `ListUsers` for the last-user check, then `DeleteUser`.

Dispatch: add `case "user": return runUser(ctx, stdin, stdout, stderr, store, now, cmdArgs)` and update the two "expected region, alert, or migrate" messages to include `user`.

- [ ] **Step 4: Run to green.** `make check`. Expected: PASS.
- [ ] **Step 5: End-to-end smoke** (scratch DB): `printf 'hunter2hunter2' | go run ./cmd/sidecar-admin --db /tmp/admin-smoke.db user create --username admin --password-stdin` then `user list`, then boot the server and `curl -si -X POST localhost:8080/api/admin/v1/session -d '{"username":"admin","password":"hunter2hunter2"}'` → 200 + Set-Cookie. Delete the scratch DB.
- [ ] **Step 6: Commit.**

```bash
git add cmd/sidecar-admin
git commit -m "Add sidecar-admin user commands for account bootstrap and management"
```

---

### Task 9: SvelteKit scaffold + build wiring

**Files:**
- Create: `web/admin/` — `package.json`, `svelte.config.js`, `vite.config.ts`, `tsconfig.json`, `eslint.config.js`, `.prettierrc`, `.prettierignore`, `src/app.html`, `src/app.d.ts`, `src/routes/+layout.ts`, `src/routes/+layout.svelte`, `src/routes/+page.svelte` (placeholder), `src/lib/index.ts` (empty placeholder for `$lib`)
- Modify: `mise.toml`, `Makefile`, `README.md` is Task 12

**Interfaces:**
- Produces: `make web` (build → `internal/httpapi/adminui/dist/`), `make web-check` (svelte-check + prettier + eslint + vitest), `make check` includes `web-check`, `make build` depends on `web`.

The scaffold is written by hand rather than `npx sv create` — the interactive scaffolder doesn't run in this environment, and hand-writing pins exactly what the spec needs. Dependency versions come from `npm install`, not hand-written pins.

- [ ] **Step 1: `mise.toml`** — add Node beside Go:

```toml
[tools]
go = "latest"
node = "24"
```

Run `mise install` and verify `node --version` prints v24.x.

- [ ] **Step 2: Write the project files.**

`web/admin/package.json` (versions filled by npm in step 3):

```json
{
	"name": "sidecar-admin-ui",
	"private": true,
	"type": "module",
	"scripts": {
		"dev": "vite dev",
		"build": "vite build",
		"check": "svelte-kit sync && svelte-check --tsconfig ./tsconfig.json",
		"lint": "prettier --check . && eslint .",
		"format": "prettier --write .",
		"test:unit": "vitest --run"
	}
}
```

`web/admin/svelte.config.js`:

```js
import adapter from '@sveltejs/adapter-static';
import { vitePreprocess } from '@sveltejs/vite-plugin-svelte';

/** @type {import('@sveltejs/kit').Config} */
const config = {
	preprocess: vitePreprocess(),
	kit: {
		// Pure SPA served by the Go binary under /admin (spec 6.1, 6.2).
		// fallback lets unknown paths client-side-route; nothing may ever
		// set prerender = true (it conflicts with the fallback).
		adapter: adapter({ fallback: 'index.html' }),
		paths: { base: '/admin' }
	}
};

export default config;
```

`web/admin/vite.config.ts`:

```ts
import { sveltekit } from '@sveltejs/kit/vite';
import { defineConfig } from 'vite';

export default defineConfig({
	plugins: [sveltekit()],
	server: {
		// Dev-mode same-origin proxy to the Go server (spec 6.4). No
		// changeOrigin: it would rewrite Host while the browser Origin stays
		// the Vite origin, and the API's cross-site guard would 403 every
		// write.
		proxy: { '/api': 'http://localhost:8080' }
	}
});
```

`web/admin/tsconfig.json`:

```json
{
	"extends": "./.svelte-kit/tsconfig.json",
	"compilerOptions": {
		"strict": true,
		"moduleResolution": "bundler"
	}
}
```

`web/admin/src/app.html`:

```html
<!doctype html>
<html lang="en">
	<head>
		<meta charset="utf-8" />
		<meta name="viewport" content="width=device-width, initial-scale=1" />
		<title>Sidecar Admin</title>
		%sveltekit.head%
	</head>
	<body>
		<div style="display: contents">%sveltekit.body%</div>
	</body>
</html>
```

`web/admin/src/app.d.ts`:

```ts
declare global {
	namespace App {}
}
export {};
```

`web/admin/src/routes/+layout.ts`:

```ts
// Pure client-rendered SPA: no SSR (there is no Node server in production)
// and no prerendering (it conflicts with the index.html fallback).
export const ssr = false;
export const prerender = false;
```

`web/admin/src/routes/+layout.svelte`:

```svelte
<script lang="ts">
	let { children } = $props();
</script>

{@render children()}
```

`web/admin/src/routes/+page.svelte` (placeholder until Task 10):

```svelte
<h1>Sidecar Admin</h1>
```

`web/admin/eslint.config.js`:

```js
import js from '@eslint/js';
import ts from 'typescript-eslint';
import svelte from 'eslint-plugin-svelte';
import svelteConfig from './svelte.config.js';

export default ts.config(
	js.configs.recommended,
	...ts.configs.recommended,
	...svelte.configs.recommended,
	{
		files: ['**/*.svelte', '**/*.svelte.ts'],
		languageOptions: { parserOptions: { parser: ts.parser, svelteConfig } }
	},
	{ ignores: ['build/', '.svelte-kit/', 'node_modules/'] }
);
```

(If the installed `eslint-plugin-svelte` exports its flat config under a different name, consult its README — the requirement is that `npm run lint` runs clean over `.svelte` and `.ts` files, not this exact text.)

`web/admin/.prettierrc`:

```json
{
	"useTabs": true,
	"singleQuote": true,
	"plugins": ["prettier-plugin-svelte"],
	"overrides": [{ "files": "*.svelte", "options": { "parser": "svelte" } }]
}
```

`web/admin/.prettierignore`:

```
.svelte-kit/
build/
node_modules/
package-lock.json
```

- [ ] **Step 3: Install dependencies** (from `web/admin/`):

```bash
npm install -D @sveltejs/adapter-static @sveltejs/kit @sveltejs/vite-plugin-svelte svelte vite svelte-check typescript vitest prettier prettier-plugin-svelte eslint @eslint/js typescript-eslint eslint-plugin-svelte
```

Then `npm run check`, `npm run lint` (run `npm run format` first to settle formatting), `npm run build` — all must pass. `npm run test:unit` may fail with "no test files" — add a `passWithNoTests: true` to a `test` block in `vite.config.ts` (`test: { passWithNoTests: true }`) so the target is green until Task 10 adds tests.

- [ ] **Step 4: Makefile wiring.**

```make
WEB_DIR   := web/admin
EMBED_DIR := internal/httpapi/adminui/dist

.PHONY: web
web: ## Build the admin SPA into the Go embed directory
	cd $(WEB_DIR) && npm ci && npm run build
	find $(EMBED_DIR) -mindepth 1 ! -name '.gitkeep' -delete
	cp -R $(WEB_DIR)/build/. $(EMBED_DIR)/

.PHONY: web-check
web-check: ## Frontend checks: svelte-check, prettier, eslint, vitest
	cd $(WEB_DIR) && npm ci && npm run check && npm run lint && npm run test:unit
```

Change `build` to depend on `web` (`build: web`), and `check` to `check: fmt-check vet lint test test-tz test-race web-check`.

- [ ] **Step 5: Verify the whole loop.**

```bash
make web
test -f internal/httpapi/adminui/dist/index.html
test -f internal/httpapi/adminui/dist/.gitkeep
go build -o bin/sidecar ./cmd/sidecar
./bin/sidecar --db /tmp/spa-smoke.db &
curl -si localhost:8080/admin | head -5                      # 200, text/html
curl -si localhost:8080/admin/_app/does-not-exist.js | head -1  # 404
kill %1; rm -f /tmp/spa-smoke.db*
git status --short   # dist/ contents must NOT appear (gitignore from Task 7)
```

- [ ] **Step 6: `make check`** (now including web-check). Expected: PASS.
- [ ] **Step 7: Commit.**

```bash
git add mise.toml Makefile web/admin
git commit -m "Scaffold the SvelteKit admin SPA and wire it into make"
```

---

### Task 10: SPA core — API client, datetime module, login, session guard

**Files:**
- Create: `web/admin/src/lib/api.ts`, `web/admin/src/lib/types.ts`, `web/admin/src/lib/datetime.ts`, `web/admin/src/lib/datetime.test.ts`
- Create: `web/admin/src/routes/login/+page.svelte`
- Modify: `web/admin/src/routes/+layout.ts`, `web/admin/src/routes/+layout.svelte`

**Interfaces:**
- Consumes: the admin API (Tasks 5–6).
- Produces: `api` object (`get/post/patch/put/del`), `ApiError`, types `Alert`, `Region`, `Translation`, `whoami()`; datetime functions `offsetMinutes`, `localInputToRFC3339`, `instantToLocalInput` — Task 11's screens import these exact names.

- [ ] **Step 1: `types.ts`** — mirror the wire shapes exactly (`Alert` with `region_id`, `start_time: string`, `end_time: string | null`, `translations: Translation[]`; `Region` with `default_agency_id`, `timezone`; keep snake_case — no mapping layer to drift).

- [ ] **Step 2: Failing datetime tests** (`datetime.test.ts`, vitest — this module is the one place the CLI's timezone lessons could regress in the browser):

```ts
import { describe, expect, it } from 'vitest';
import { instantToLocalInput, localInputToRFC3339, offsetMinutes } from './datetime';

describe('offsetMinutes', () => {
	it('handles a 45-minute offset zone', () => {
		expect(offsetMinutes(new Date('2026-01-15T00:00:00Z'), 'Asia/Kathmandu')).toBe(345);
	});
	it('tracks DST', () => {
		expect(offsetMinutes(new Date('2026-01-15T12:00:00Z'), 'America/Los_Angeles')).toBe(-480);
		expect(offsetMinutes(new Date('2026-07-15T12:00:00Z'), 'America/Los_Angeles')).toBe(-420);
	});
});

describe('localInputToRFC3339', () => {
	it('stamps the zone offset for the wall time, not the current offset', () => {
		// January wall time in LA is PST even if "today" is July.
		expect(localInputToRFC3339('2026-01-15T14:00', 'America/Los_Angeles')).toBe('2026-01-15T14:00:00-08:00');
		expect(localInputToRFC3339('2026-07-15T14:00', 'America/Los_Angeles')).toBe('2026-07-15T14:00:00-07:00');
	});
	it('handles positive offsets', () => {
		expect(localInputToRFC3339('2026-08-15T09:30', 'Asia/Kathmandu')).toBe('2026-08-15T09:30:00+05:45');
	});
});

describe('instantToLocalInput', () => {
	it('round-trips through the region timezone', () => {
		expect(instantToLocalInput('2026-08-15T21:00:00Z', 'America/Los_Angeles')).toBe('2026-08-15T14:00');
	});
});
```

- [ ] **Step 3: Implement `datetime.ts`:**

```ts
// Timezone-explicit datetime mapping between <input type="datetime-local">
// values and the API's RFC 3339-with-offset contract. The API rejects naive
// datetimes, so every submit path MUST go through localInputToRFC3339.

export function offsetMinutes(instant: Date, timeZone: string): number {
	const dtf = new Intl.DateTimeFormat('en-US', {
		timeZone,
		year: 'numeric',
		month: '2-digit',
		day: '2-digit',
		hour: '2-digit',
		minute: '2-digit',
		second: '2-digit',
		hour12: false
	});
	const p = Object.fromEntries(dtf.formatToParts(instant).map((x) => [x.type, x.value]));
	const asUTC = Date.UTC(+p.year, +p.month - 1, +p.day, +p.hour % 24, +p.minute, +p.second);
	return Math.round((asUTC - instant.getTime()) / 60_000);
}

export function localInputToRFC3339(input: string, timeZone: string): string {
	const [d, t] = input.split('T');
	const [y, mo, da] = d.split('-').map(Number);
	const [hh, mm] = t.split(':').map(Number);
	const naiveUTC = Date.UTC(y, mo - 1, da, hh, mm, 0);
	// The offset at the target wall time may differ from the offset at the
	// naive instant (DST boundary); one refinement pass settles it.
	let off = offsetMinutes(new Date(naiveUTC), timeZone);
	off = offsetMinutes(new Date(naiveUTC - off * 60_000), timeZone);
	const sign = off < 0 ? '-' : '+';
	const abs = Math.abs(off);
	const oh = String(Math.floor(abs / 60)).padStart(2, '0');
	const om = String(abs % 60).padStart(2, '0');
	return `${d}T${t}:00${sign}${oh}:${om}`;
}

export function instantToLocalInput(iso: string, timeZone: string): string {
	const instant = new Date(iso);
	const dtf = new Intl.DateTimeFormat('en-CA', {
		timeZone,
		year: 'numeric',
		month: '2-digit',
		day: '2-digit',
		hour: '2-digit',
		minute: '2-digit',
		hour12: false
	});
	const p = Object.fromEntries(dtf.formatToParts(instant).map((x) => [x.type, x.value]));
	return `${p.year}-${p.month}-${p.day}T${p.hour === '24' ? '00' : p.hour}:${p.minute}`;
}
```

Run: `npm run test:unit`. Expected: PASS.

- [ ] **Step 4: `api.ts`:**

```ts
import { goto } from '$app/navigation';
import { resolve } from '$app/paths';

export class ApiError extends Error {
	constructor(
		public status: number,
		message: string
	) {
		super(message);
	}
}

async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
	const res = await fetch(`/api/admin/v1${path}`, {
		method,
		headers: body === undefined ? undefined : { 'Content-Type': 'application/json' },
		body: body === undefined ? undefined : JSON.stringify(body)
	});
	if (res.status === 401 && !path.startsWith('/session')) {
		const dest = location.pathname + location.search;
		void goto(`${resolve('/login')}?redirectTo=${encodeURIComponent(dest)}`);
		throw new ApiError(401, 'authentication required');
	}
	if (!res.ok) {
		let msg = `${res.status} ${res.statusText}`;
		try {
			const parsed = (await res.json()) as { error?: string };
			if (parsed.error) msg = parsed.error;
		} catch {
			// non-JSON error body; keep the status text
		}
		throw new ApiError(res.status, msg);
	}
	if (res.status === 204) return undefined as T;
	return (await res.json()) as T;
}

export const api = {
	get: <T>(path: string) => request<T>('GET', path),
	post: <T>(path: string, body?: unknown) => request<T>('POST', path, body),
	patch: <T>(path: string, body: unknown) => request<T>('PATCH', path, body),
	put: <T>(path: string, body: unknown) => request<T>('PUT', path, body),
	del: (path: string) => request<void>('DELETE', path)
};

export async function whoami(): Promise<{ username: string } | null> {
	const res = await fetch('/api/admin/v1/session');
	if (!res.ok) return null;
	return (await res.json()) as { username: string };
}
```

- [ ] **Step 5: Session guard in the root layout.** `+layout.ts` grows a `load`:

```ts
import { redirect } from '@sveltejs/kit';
import { resolve } from '$app/paths';
import { whoami } from '$lib/api';

export const ssr = false;
export const prerender = false;

export async function load({ url }) {
	const user = await whoami();
	const onLogin = url.pathname === resolve('/login');
	if (!user && !onLogin) {
		redirect(307, `${resolve('/login')}?redirectTo=${encodeURIComponent(url.pathname + url.search)}`);
	}
	return { user };
}
```

`+layout.svelte` becomes the app shell: nav with links (`resolve('/')`, `resolve('/regions')`) shown only when `data.user`, the username, and a Logout button calling `api.del('/session')` then `goto(resolve('/login'))`. Hand-written CSS in the same file (`<style>`): simple header bar, max-width content column. No `{@html}` anywhere.

- [ ] **Step 6: Login page** `src/routes/login/+page.svelte`:

```svelte
<script lang="ts">
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { resolve } from '$app/paths';
	import { api, ApiError } from '$lib/api';

	let username = $state('');
	let password = $state('');
	let error = $state('');
	let busy = $state(false);

	async function submit(e: SubmitEvent) {
		e.preventDefault();
		busy = true;
		error = '';
		try {
			await api.post('/session', { username, password });
			await goto(page.url.searchParams.get('redirectTo') ?? resolve('/'));
		} catch (err) {
			error = err instanceof ApiError ? err.message : 'login failed';
		} finally {
			busy = false;
		}
	}
</script>

<h1>Sign in</h1>
<form onsubmit={submit}>
	<label>Username <input bind:value={username} autocomplete="username" required /></label>
	<label>
		Password
		<input type="password" bind:value={password} autocomplete="current-password" required />
	</label>
	{#if error}<p class="error">{error}</p>{/if}
	<button disabled={busy}>Sign in</button>
</form>
```

(Plus a small `<style>` block.) `redirectTo` values come from our own `encodeURIComponent(location.pathname...)` only; still, only `goto` relative paths — never `location.href =` — so an injected absolute URL cannot leave the app.

- [ ] **Step 7: Verify.** `npm run check && npm run lint && npm run test:unit` clean. Then live: `make web && go run ./cmd/sidecar --db /tmp/spa2.db` (create a user first with sidecar-admin), open `http://localhost:8080/admin` — expect redirect to login, successful sign-in, nav visible, logout returns to login. Also `make check`.
- [ ] **Step 8: Commit.**

```bash
git add web/admin
git commit -m "Add SPA core: API client, timezone-explicit datetime module, login and session guard"
```

---

### Task 11: SPA screens — alerts and regions

**Files:**
- Create: `web/admin/src/lib/AlertForm.svelte`
- Create: `web/admin/src/routes/+page.svelte` (replace placeholder), `src/routes/alerts/new/+page.svelte`, `src/routes/alerts/[id]/+page.svelte`, `src/routes/regions/+page.svelte`
- Create: `web/admin/src/lib/enums.ts`

**Interfaces:**
- Consumes: Task 10's `api`, `types`, `datetime`; the admin API.
- Produces: the five screens of spec §6.2.

- [ ] **Step 1: `enums.ts`** — the option lists, copied from `internal/alerts/enums.go` (12 causes, 11 effects, 4 severities — copy the exact names from that file at implementation time; a drifted list turns into a 400 at submit, which the form surfaces, so drift is visible not silent).

- [ ] **Step 2: Alerts list** (`src/routes/+page.svelte` + `+page.ts` load): fetch `api.get<Alert[]>('/alerts')` and `api.get<Region[]>('/regions')`; region `<select>` filter (client-side; "All regions" default — remember region 0 is real, key the "all" option on `''` never `0`); table of header / region name / start (rendered with `instantToLocalInput` in the region's timezone, falling back to the raw UTC string when the region has no timezone) / badges (`published` green, `draft` gray, `test` amber — plain CSS classes); row links to `/alerts/[id]` via `resolve`; "New alert" button.

- [ ] **Step 3: `AlertForm.svelte`** — shared by create and edit. Props: `{ regions: Region[], initial?: Alert, submitLabel: string, onsubmit: (payload) => Promise<void> }`. Fields: region select (disabled in edit — region is immutable through the API; the PATCH shape has no region_id), agency id (placeholder showing the region default when present), header (required), description (textarea), url, cause/effect/severity selects from `enums.ts`, start (`datetime-local`, converted with `localInputToRFC3339(value, regionTimezone)` at submit — when the region has no timezone configured, show a field-level hint and refuse local entry, offering a raw RFC 3339 text input instead: never guess a zone), optional end (+ "clear end" checkbox in edit → `clear_end_time: true`), is_test checkbox. Error banner shows `ApiError.message` — the server's messages (naming valid enum values, the offset requirement, the set-a-default-agency guidance) are the UX copy; do not duplicate validation client-side beyond `required`.

- [ ] **Step 4: Create page** (`alerts/new/+page.svelte`): loads regions, renders `AlertForm`, `api.post<Alert>('/alerts', payload)` → `goto(resolve('/alerts/[id]', { id: String(created.id) }))`.

- [ ] **Step 5: Edit page** (`alerts/[id]/+page.svelte` + `+page.ts` load by `page.params.id`): `AlertForm` prefilled (PATCH with only changed fields is a nicety — sending all editable fields is acceptable and simpler; do that); Publish/Unpublish button (`api.post` to `/alerts/{id}/publish|unpublish`, then reload data); Delete button with `confirm()`; translations section: list existing (language, header text, description text, per-language Delete via `api.del`), add/update form (language input + header/description textareas → `api.put('/alerts/{id}/translations/{lang}', {...})`). After every mutation re-fetch the alert so staleness (`SourceSHA256`) reflects server truth.

- [ ] **Step 6: Regions page** (`regions/+page.svelte`): table of regions (id, name, active, default agency id, timezone) with inline edit per row — two inputs + Save calling `api.patch('/regions/{id}', { default_agency_id, timezone })`, error shown per-row (an invalid timezone comes back as the server's 400 message).

- [ ] **Step 7: Verify end-to-end in the browser.** `make web`, run the server, and walk the whole flow: login → create alert in a region with a default agency id → see draft badge → publish → confirm it appears in `curl localhost:8080/api/v1/regions/1/alerts.pbtext` → add a Spanish translation → confirm in pbtext → edit header → confirm translation vanishes from pbtext (stale hash) → unpublish → delete → regions page: set a timezone, set an invalid timezone (`America/Nowhere`) and see the server's 400 surfaced. `npm run check && npm run lint`, then `make check`.
- [ ] **Step 8: Commit.**

```bash
git add web/admin
git commit -m "Add admin SPA screens: alerts list, create, edit, translations, regions"
```

---

### Task 12: README, final gate

**Files:**
- Modify: `README.md`

- [ ] **Step 1: README.** Add an "Admin UI" section after the sidecar-admin section:
  - Bootstrap + run, as one sequential block (the alerts quickstart's hard-won discipline — steps are sequential, say so):

```sh
# Create the first admin user (prompts for a password; 12 char minimum).
./bin/sidecar-admin --db ./sidecar.db user create --username admin

# Build the admin SPA into the server binary, then run it.
make build
./bin/sidecar --db ./sidecar.db
# open http://localhost:8080/admin and sign in
```

  - `user` command surface added to the existing command-surface block (`user create --username NAME [--password-stdin] | passwd | list | delete [--force]`).
  - A deployment note: sessions require the reverse proxy to preserve the public `Host` header (nginx: `proxy_set_header Host $host;`) and to set `X-Forwarded-Proto https` when terminating TLS — otherwise admin writes are rejected as cross-site / cookies lose `Secure`.
  - Development note: `cd web/admin && npm run dev` for hot reload (proxies `/api` to `localhost:8080`); `make web` to rebuild the embedded copy; there is deliberately no CORS anywhere.
- [ ] **Step 2: Run the README block verbatim** against a scratch database (use `--password-stdin` for the non-interactive environment and note the prompt in the README stays as-is for humans). Every command must succeed in sequence, including opening `/admin` (curl the HTML).
- [ ] **Step 3: Final gate.** `make check` (fmt, vet, lint, Go tests ×3 TZ runs, race, web-check) — all green.
- [ ] **Step 4: Commit.**

```bash
git add README.md
git commit -m "Document the admin UI: bootstrap, deployment, and development workflow"
```

---

## Self-Review Notes

- **Spec coverage:** §2.1–2.5 → Tasks 1–3, 7, 9; §3 → Tasks 1–3; §4.1–4.5 → Tasks 1, 5; §5 → Tasks 4, 6; §6.1–6.5 → Tasks 7, 9–11; §7 → Task 8; §8 → distributed per task (storetest T3/T4, httpapi T5–T7, CLI T8, vitest T10, tz discipline unchanged); §9 → guard/middleware T5, `{@html}` ban T10–11; §10 → Tasks 9, 12.
- **Known deviation:** `Repository.GetUserByID` is not in the spec's §3.2 sketch; the middleware needs it for whoami. It follows the sketch's own conventions.
- **Type consistency spot-checks:** `Deps.Sleep` introduced in Task 5 and used only there; `DeleteTranslation` (T4) matches T6's usage; datetime function names in T10 match T11's imports; `alertJSON` field list matches spec §5's example including `created_at`/`updated_at` (backed by T4's domain fields).
