package storetest

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/auth"
)

// testPasswordHash is a stand-in for a real PHC-format argon2id string. The
// repository never parses it -- it is opaque storage -- so a fixed literal
// keeps these subtests free of argon2 cost.
const testPasswordHash = "$argon2id$v=19$m=65536,t=3,p=4$c29tZXNhbHQ$c29tZWhhc2g"

// newAuthRepoFunc is shorthand for the callback every auth subtest receives:
// a fresh, migrated auth.Repository.
type newAuthRepoFunc func(*testing.T) auth.Repository

// RunAuthRepository exercises an auth.Repository against the behavioral
// contract every engine must satisfy. Each subtest gets a fresh repository
// from newRepo.
//
// The HTTP session middleware and the sidecar-admin user commands trust these
// semantics without re-checking them, so each subtest below is written to
// fail if the behavior it names regresses -- not merely to exercise the code.
func RunAuthRepository(t *testing.T, newRepo newAuthRepoFunc) {
	t.Helper()

	t.Run("UserRoundTrip", func(t *testing.T) { testUserRoundTrip(t, newRepo) })
	t.Run("UsernameNormalizedOnWriteAndRead", func(t *testing.T) { testUsernameNormalizedOnWriteAndRead(t, newRepo) })
	t.Run("DuplicateUsernameIsErrUsernameTaken", func(t *testing.T) { testDuplicateUsernameIsErrUsernameTaken(t, newRepo) })
	t.Run("CreateRejectsInvalidUsername", func(t *testing.T) { testCreateRejectsInvalidUsername(t, newRepo) })
	t.Run("CreateRejectsEmptyPasswordHash", func(t *testing.T) { testCreateRejectsEmptyPasswordHash(t, newRepo) })
	t.Run("UnknownUserIsErrNotFound", func(t *testing.T) { testUnknownUserIsErrNotFound(t, newRepo) })
	t.Run("ListUsersOrderedByUsername", func(t *testing.T) { testListUsersOrderedByUsername(t, newRepo) })
	t.Run("SessionRoundTrip", func(t *testing.T) { testSessionRoundTrip(t, newRepo) })
	t.Run("SessionExpiryBoundary", func(t *testing.T) { testSessionExpiryBoundary(t, newRepo) })
	t.Run("ExpiredSessionDeletedOnRead", func(t *testing.T) { testExpiredSessionDeletedOnRead(t, newRepo) })
	t.Run("DeleteUserSessionsRevokesAllAndOnlyTheirs", func(t *testing.T) {
		testDeleteUserSessionsRevokesAllAndOnlyTheirs(t, newRepo)
	})
	t.Run("DeleteExpiredSessionsCount", func(t *testing.T) { testDeleteExpiredSessionsCount(t, newRepo) })
	t.Run("UserDeleteCascadesToSessions", func(t *testing.T) { testUserDeleteCascadesToSessions(t, newRepo) })
	t.Run("TokenHashIsNotTheToken", func(t *testing.T) { testTokenHashIsNotTheToken(t, newRepo) })
	t.Run("PasswordUpdatePersists", func(t *testing.T) { testPasswordUpdatePersists(t, newRepo) })
	t.Run("TimestampsSurvive32BitBoundary", func(t *testing.T) { testTimestampsSurvive32BitBoundary(t, newRepo) })
}

// assertInstant checks both that got names the same instant as want AND that
// it carries the UTC location. The location half matters on its own: a value
// read back as the machine's local zone names the right instant and passes an
// Equal check, then formats as a different wall-clock time everywhere it is
// rendered. The suite runs under TZ=Asia/Kathmandu in `make test-tz`, which is
// what makes this assertion bite.
func assertInstant(t *testing.T, label string, got, want time.Time) {
	t.Helper()
	if !got.Equal(want) {
		t.Errorf("%s = %v (unix %d), want %v (unix %d)", label, got, got.Unix(), want, want.Unix())
	}
	if got.Location() != time.UTC {
		t.Errorf("%s location = %v, want UTC", label, got.Location())
	}
}

// assertSameUser compares every field of two auth.User values.
func assertSameUser(t *testing.T, label string, got, want auth.User) {
	t.Helper()
	if got.ID != want.ID {
		t.Errorf("%s ID = %d, want %d", label, got.ID, want.ID)
	}
	if got.Username != want.Username {
		t.Errorf("%s Username = %q, want %q", label, got.Username, want.Username)
	}
	if got.PasswordHash != want.PasswordHash {
		t.Errorf("%s PasswordHash = %q, want %q", label, got.PasswordHash, want.PasswordHash)
	}
	assertInstant(t, label+" CreatedAt", got.CreatedAt, want.CreatedAt)
	assertInstant(t, label+" UpdatedAt", got.UpdatedAt, want.UpdatedAt)
}

// mustCreateUser creates a user at the fixed base instant, failing the test if
// creation errors.
func mustCreateUser(t *testing.T, repo auth.Repository, username string) auth.User {
	t.Helper()
	u, err := repo.CreateUser(context.Background(), username, testPasswordHash, base)
	if err != nil {
		t.Fatalf("CreateUser(%q): %v", username, err)
	}
	return u
}

// mustCreateSession mints a fresh token pair, stores the session, and returns
// both halves so callers can assert that only the hash is a valid lookup key.
func mustCreateSession(t *testing.T, repo auth.Repository, userID int64, now, expiresAt time.Time) (token, tokenHash string) {
	t.Helper()
	token, tokenHash, err := auth.NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	if err := repo.CreateSession(context.Background(), tokenHash, userID, now, expiresAt); err != nil {
		t.Fatalf("CreateSession(user %d): %v", userID, err)
	}
	return token, tokenHash
}

// assertSessionGone asserts that a token hash names no row at all, not merely
// one the expiry filter hides. DeleteSession maps zero affected rows to
// ErrNotFound, so it reports physical absence -- something GetSession alone
// cannot distinguish from "present but expired".
func assertSessionGone(t *testing.T, repo auth.Repository, label, tokenHash string) {
	t.Helper()
	if err := repo.DeleteSession(context.Background(), tokenHash); !errors.Is(err, auth.ErrNotFound) {
		t.Errorf("DeleteSession(%s) = %v, want auth.ErrNotFound (the row should no longer exist)", label, err)
	}
}

// testUserRoundTrip asserts that every User field survives a write and both
// read paths, and that the timestamps come back as UTC instants rather than
// zero values or local-zone times. GetUserByID is the lookup the HTTP whoami
// handler uses; GetUserByUsername is the one login uses, and both must agree.
func testUserRoundTrip(t *testing.T, newRepo newAuthRepoFunc) {
	repo := newRepo(t)
	ctx := context.Background()

	created, err := repo.CreateUser(ctx, "admin", testPasswordHash, base)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if created.ID == 0 {
		t.Error("CreateUser returned ID 0, want the assigned row id")
	}
	if created.Username != "admin" {
		t.Errorf("Username = %q, want %q", created.Username, "admin")
	}
	if created.PasswordHash != testPasswordHash {
		t.Errorf("PasswordHash = %q, want %q", created.PasswordHash, testPasswordHash)
	}
	assertInstant(t, "CreateUser CreatedAt", created.CreatedAt, base)
	assertInstant(t, "CreateUser UpdatedAt", created.UpdatedAt, base)

	byName, err := repo.GetUserByUsername(ctx, "admin")
	if err != nil {
		t.Fatalf("GetUserByUsername: %v", err)
	}
	assertSameUser(t, "GetUserByUsername", byName, created)

	byID, err := repo.GetUserByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetUserByID: %v", err)
	}
	assertSameUser(t, "GetUserByID", byID, created)

	users, err := repo.ListUsers(ctx)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(users) != 1 {
		t.Fatalf("ListUsers = %d users, want 1", len(users))
	}
	assertSameUser(t, "ListUsers[0]", users[0], created)
}

// testUsernameNormalizedOnWriteAndRead asserts that normalization happens
// inside the repository on EVERY write and EVERY lookup, not only in the CLI.
// Usernames are normalized in Go rather than by SQL collation so SQLite and a
// future Postgres agree; an adapter that normalized only on create would store
// "admin" and then fail to find it for a login typed as "Admin", locking the
// operator out of their own account.
func testUsernameNormalizedOnWriteAndRead(t *testing.T, newRepo newAuthRepoFunc) {
	repo := newRepo(t)
	ctx := context.Background()

	created, err := repo.CreateUser(ctx, "Admin ", testPasswordHash, base)
	if err != nil {
		t.Fatalf("CreateUser(%q): %v", "Admin ", err)
	}
	if created.Username != "admin" {
		t.Fatalf("stored Username = %q, want normalized %q", created.Username, "admin")
	}

	for _, lookup := range []string{"admin", "  ADMIN", "Admin\t", " aDmIn "} {
		got, getErr := repo.GetUserByUsername(ctx, lookup)
		if getErr != nil {
			t.Fatalf("GetUserByUsername(%q): %v", lookup, getErr)
		}
		if got.ID != created.ID {
			t.Errorf("GetUserByUsername(%q).ID = %d, want %d", lookup, got.ID, created.ID)
		}
	}

	// UpdatePassword and DeleteUser take a username too; each must normalize
	// on its own or `sidecar-admin user passwd Admin` silently reports "not
	// found" for an account that plainly exists.
	if err = repo.UpdatePassword(ctx, "  ADMIN ", testPasswordHash+"2", base.Add(time.Hour)); err != nil {
		t.Fatalf("UpdatePassword(%q): %v", "  ADMIN ", err)
	}
	after, err := repo.GetUserByUsername(ctx, "admin")
	if err != nil {
		t.Fatalf("GetUserByUsername after UpdatePassword: %v", err)
	}
	if after.PasswordHash != testPasswordHash+"2" {
		t.Errorf("PasswordHash = %q, want the value written through the mixed-case name", after.PasswordHash)
	}

	if err = repo.DeleteUser(ctx, "ADMIN"); err != nil {
		t.Fatalf("DeleteUser(%q): %v", "ADMIN", err)
	}
	if _, err = repo.GetUserByUsername(ctx, "admin"); !errors.Is(err, auth.ErrNotFound) {
		t.Errorf("GetUserByUsername after DeleteUser = %v, want auth.ErrNotFound", err)
	}
}

// testDuplicateUsernameIsErrUsernameTaken asserts the duplicate is reported as
// the auth.ErrUsernameTaken sentinel, so `sidecar-admin user add` can print a
// useful message instead of a raw driver string. It also asserts the collision
// survives case differences: normalization happens before the UNIQUE index
// sees the value, so "ADMIN" must collide with an existing "admin" rather than
// creating a second account that shadows the first at login.
func testDuplicateUsernameIsErrUsernameTaken(t *testing.T, newRepo newAuthRepoFunc) {
	repo := newRepo(t)
	ctx := context.Background()

	mustCreateUser(t, repo, "admin")

	for _, dup := range []string{"admin", "ADMIN", " Admin "} {
		_, err := repo.CreateUser(ctx, dup, testPasswordHash, base.Add(time.Hour))
		if !errors.Is(err, auth.ErrUsernameTaken) {
			t.Errorf("CreateUser(%q) = %v, want auth.ErrUsernameTaken", dup, err)
		}
	}

	users, err := repo.ListUsers(ctx)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(users) != 1 {
		t.Errorf("ListUsers = %d users, want 1 (no duplicate should have been stored)", len(users))
	}
}

// testCreateRejectsInvalidUsername asserts CreateUser enforces
// auth.ValidateUsername itself rather than trusting callers. A blank or
// whitespace-only name would otherwise become an unreachable account (nothing
// a human can type normalizes to ""), and a name containing a space breaks the
// CLI's own argument handling.
func testCreateRejectsInvalidUsername(t *testing.T, newRepo newAuthRepoFunc) {
	repo := newRepo(t)
	ctx := context.Background()

	cases := []struct {
		name     string
		username string
	}{
		{"empty", ""},
		{"whitespace only", "   "},
		{"embedded space", "has space"},
		{"embedded tab", "has\ttab"},
		{"too long", strings.Repeat("a", 65)},
	}
	for _, tc := range cases {
		if _, err := repo.CreateUser(ctx, tc.username, testPasswordHash, base); err == nil {
			t.Errorf("CreateUser(%s, %q): want error, got nil", tc.name, tc.username)
		}
	}

	users, err := repo.ListUsers(ctx)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	if len(users) != 0 {
		t.Errorf("ListUsers = %d users, want 0 (no rejected create may have written a row)", len(users))
	}
}

// testCreateRejectsEmptyPasswordHash asserts the repository refuses to store a
// user with no password hash. The column is NOT NULL but not non-empty, so
// without this check a caller that forgot to hash would create an account
// whose verification step compares against "" -- a login shape nobody
// intended, produced silently.
func testCreateRejectsEmptyPasswordHash(t *testing.T, newRepo newAuthRepoFunc) {
	repo := newRepo(t)
	ctx := context.Background()

	if _, err := repo.CreateUser(ctx, "admin", "", base); err == nil {
		t.Error("CreateUser with an empty password hash: want error, got nil")
	}
	if _, err := repo.GetUserByUsername(ctx, "admin"); !errors.Is(err, auth.ErrNotFound) {
		t.Errorf("GetUserByUsername after a rejected create = %v, want auth.ErrNotFound (no row may exist)", err)
	}
}

// testUnknownUserIsErrNotFound asserts every user-facing operation reports a
// missing account as auth.ErrNotFound rather than returning nil. DeleteUser
// and UpdatePassword run as :execrows statements that affect zero rows and
// report no driver error, so without an explicit rows-affected check
// `sidecar-admin user rm typo` would exit 0 and print nothing while the real
// account stayed live.
func testUnknownUserIsErrNotFound(t *testing.T, newRepo newAuthRepoFunc) {
	repo := newRepo(t)
	ctx := context.Background()

	// A real user exists, so a passing result cannot come from an empty table.
	mustCreateUser(t, repo, "admin")

	if _, err := repo.GetUserByUsername(ctx, "ghost"); !errors.Is(err, auth.ErrNotFound) {
		t.Errorf("GetUserByUsername(ghost) = %v, want auth.ErrNotFound", err)
	}
	if _, err := repo.GetUserByID(ctx, 999999); !errors.Is(err, auth.ErrNotFound) {
		t.Errorf("GetUserByID(999999) = %v, want auth.ErrNotFound", err)
	}
	if err := repo.DeleteUser(ctx, "ghost"); !errors.Is(err, auth.ErrNotFound) {
		t.Errorf("DeleteUser(ghost) = %v, want auth.ErrNotFound", err)
	}
	if err := repo.UpdatePassword(ctx, "ghost", testPasswordHash, base); !errors.Is(err, auth.ErrNotFound) {
		t.Errorf("UpdatePassword(ghost) = %v, want auth.ErrNotFound", err)
	}

	// An unknown session token is the same contract on the session side.
	if _, err := repo.GetSession(ctx, "0000000000000000000000000000000000000000000000000000000000000000", base); !errors.Is(err, auth.ErrNotFound) {
		t.Errorf("GetSession(unknown) = %v, want auth.ErrNotFound", err)
	}
	if err := repo.DeleteSession(ctx, "0000000000000000000000000000000000000000000000000000000000000000"); !errors.Is(err, auth.ErrNotFound) {
		t.Errorf("DeleteSession(unknown) = %v, want auth.ErrNotFound", err)
	}
}

// testListUsersOrderedByUsername asserts ListUsers returns a stable,
// username-ordered result. `sidecar-admin user ls` prints it directly, and an
// unordered listing makes the output shuffle between invocations for no
// reason the operator can see.
func testListUsersOrderedByUsername(t *testing.T, newRepo newAuthRepoFunc) {
	repo := newRepo(t)
	ctx := context.Background()

	// Inserted out of order so insertion order and sorted order differ.
	for _, name := range []string{"zoe", "alice", "mallory"} {
		mustCreateUser(t, repo, name)
	}

	users, err := repo.ListUsers(ctx)
	if err != nil {
		t.Fatalf("ListUsers: %v", err)
	}
	got := make([]string, len(users))
	for i, u := range users {
		got[i] = u.Username
	}
	want := []string{"alice", "mallory", "zoe"}
	if !slices.Equal(got, want) {
		t.Errorf("ListUsers = %v, want %v", got, want)
	}
}

// testSessionRoundTrip asserts a live session comes back with every field
// intact and both timestamps as UTC instants. The middleware reads UserID off
// this value to identify the caller, so a mismatch here is an authorization
// bug, not a formatting one.
func testSessionRoundTrip(t *testing.T, newRepo newAuthRepoFunc) {
	repo := newRepo(t)
	ctx := context.Background()

	user := mustCreateUser(t, repo, "admin")
	expires := base.Add(auth.SessionLifetime)
	_, hash := mustCreateSession(t, repo, user.ID, base, expires)

	got, err := repo.GetSession(ctx, hash, base)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.TokenHash != hash {
		t.Errorf("TokenHash = %q, want %q", got.TokenHash, hash)
	}
	if got.UserID != user.ID {
		t.Errorf("UserID = %d, want %d", got.UserID, user.ID)
	}
	assertInstant(t, "Session CreatedAt", got.CreatedAt, base)
	assertInstant(t, "Session ExpiresAt", got.ExpiresAt, expires)
}

// testSessionExpiryBoundary pins the expiry comparison to the second, in the
// exclusive direction: a session is live strictly BEFORE expires_at and dead
// at it. Expiry is evaluated against the injected now, never a database clock,
// so this is the assertion that catches an off-by-one (`<` instead of `<=`)
// leaving a session usable for one extra second past its stated lifetime, and
// the reverse (`<=` on the live side) killing it a second early.
func testSessionExpiryBoundary(t *testing.T, newRepo newAuthRepoFunc) {
	repo := newRepo(t)
	ctx := context.Background()

	user := mustCreateUser(t, repo, "admin")
	expires := base.Add(auth.SessionLifetime)
	_, hash := mustCreateSession(t, repo, user.ID, base, expires)

	// One second before expiry: still live.
	alive, err := repo.GetSession(ctx, hash, expires.Add(-time.Second))
	if err != nil {
		t.Fatalf("GetSession(now = expiry-1s) = %v, want the live session", err)
	}
	if alive.TokenHash != hash {
		t.Errorf("GetSession(now = expiry-1s).TokenHash = %q, want %q", alive.TokenHash, hash)
	}

	// Exactly AT expiry: dead. This probe is deliberately last, because
	// observing an expired row also deletes it (see
	// testExpiredSessionDeletedOnRead).
	if _, err = repo.GetSession(ctx, hash, expires); !errors.Is(err, auth.ErrNotFound) {
		t.Errorf("GetSession(now = expiry) = %v, want auth.ErrNotFound (a session is dead AT expires_at)", err)
	}
}

// testExpiredSessionDeletedOnRead asserts the delete-on-read half of the
// GetSession contract: observing an expired row REMOVES it before returning
// ErrNotFound. That deliberate write inside a read path is what keeps the
// sessions table from accumulating dead rows between sweeps, and every
// implementation -- including a future Postgres one -- must do it.
//
// The discriminating move is the second lookup: it passes a now well BEFORE
// the expiry, so an implementation that merely filtered on expiry (and never
// deleted) would happily return the session and fail here.
func testExpiredSessionDeletedOnRead(t *testing.T, newRepo newAuthRepoFunc) {
	repo := newRepo(t)
	ctx := context.Background()

	user := mustCreateUser(t, repo, "admin")
	expires := base.Add(time.Hour)
	_, hash := mustCreateSession(t, repo, user.ID, base, expires)

	if _, err := repo.GetSession(ctx, hash, expires.Add(time.Hour)); !errors.Is(err, auth.ErrNotFound) {
		t.Fatalf("GetSession(now = expiry+1h) = %v, want auth.ErrNotFound", err)
	}

	if _, err := repo.GetSession(ctx, hash, base); !errors.Is(err, auth.ErrNotFound) {
		t.Errorf("GetSession(now = base, before expiry) after an expired read = %v, want auth.ErrNotFound: the expired row must have been DELETED, not merely filtered", err)
	}
	assertSessionGone(t, repo, "expired session", hash)
}

// testDeleteUserSessionsRevokesAllAndOnlyTheirs asserts bulk revocation is
// both complete and scoped. `sidecar-admin user passwd` calls this so a
// password change locks out whoever held the old password; a WHERE clause that
// matched too broadly would log out every other admin at the same time, and
// one that matched too narrowly would leave the compromised session live.
func testDeleteUserSessionsRevokesAllAndOnlyTheirs(t *testing.T, newRepo newAuthRepoFunc) {
	repo := newRepo(t)
	ctx := context.Background()

	alice := mustCreateUser(t, repo, "alice")
	bob := mustCreateUser(t, repo, "bob")
	expires := base.Add(auth.SessionLifetime)

	_, aliceLaptop := mustCreateSession(t, repo, alice.ID, base, expires)
	_, alicePhone := mustCreateSession(t, repo, alice.ID, base.Add(time.Minute), expires)
	_, bobLaptop := mustCreateSession(t, repo, bob.ID, base, expires)

	n, err := repo.DeleteUserSessions(ctx, alice.ID)
	if err != nil {
		t.Fatalf("DeleteUserSessions(alice): %v", err)
	}
	if n != 2 {
		t.Errorf("DeleteUserSessions(alice) = %d, want 2", n)
	}

	assertSessionGone(t, repo, "alice laptop", aliceLaptop)
	assertSessionGone(t, repo, "alice phone", alicePhone)

	if _, err = repo.GetSession(ctx, bobLaptop, base); err != nil {
		t.Errorf("GetSession(bob) after revoking alice's sessions = %v, want the live session", err)
	}

	// Bulk revocation is idempotent by design: a second call affects nothing
	// and must not be reported as an error.
	n, err = repo.DeleteUserSessions(ctx, alice.ID)
	if err != nil {
		t.Errorf("second DeleteUserSessions(alice) = %v, want nil (bulk deletes are idempotent)", err)
	}
	if n != 0 {
		t.Errorf("second DeleteUserSessions(alice) = %d, want 0", n)
	}
}

// testDeleteExpiredSessionsCount asserts the periodic sweep removes exactly
// the dead rows and reports how many. The cutoff is inclusive, matching
// GetSession's "dead AT expires_at": a session whose expires_at equals now is
// already unusable, so leaving it behind would strand a row the sweep can
// never reclaim on the boundary tick.
func testDeleteExpiredSessionsCount(t *testing.T, newRepo newAuthRepoFunc) {
	repo := newRepo(t)
	ctx := context.Background()

	user := mustCreateUser(t, repo, "admin")
	cutoff := base.Add(3 * time.Hour)

	_, longGone := mustCreateSession(t, repo, user.ID, base, base.Add(time.Hour))
	_, atCutoff := mustCreateSession(t, repo, user.ID, base, cutoff)
	_, live := mustCreateSession(t, repo, user.ID, base, base.Add(auth.SessionLifetime))

	n, err := repo.DeleteExpiredSessions(ctx, cutoff)
	if err != nil {
		t.Fatalf("DeleteExpiredSessions: %v", err)
	}
	if n != 2 {
		t.Errorf("DeleteExpiredSessions = %d, want 2 (the past-expiry row and the one expiring exactly at the cutoff)", n)
	}

	assertSessionGone(t, repo, "long-expired session", longGone)
	assertSessionGone(t, repo, "session expiring at the cutoff", atCutoff)

	if _, err = repo.GetSession(ctx, live, cutoff); err != nil {
		t.Errorf("GetSession(live session) after the sweep = %v, want the live session", err)
	}

	// Nothing left to reap: zero is a count, not an error.
	n, err = repo.DeleteExpiredSessions(ctx, cutoff)
	if err != nil {
		t.Errorf("second DeleteExpiredSessions = %v, want nil", err)
	}
	if n != 0 {
		t.Errorf("second DeleteExpiredSessions = %d, want 0", n)
	}
}

// testUserDeleteCascadesToSessions asserts that removing a user removes their
// sessions with them. Without the cascade, `sidecar-admin user rm` would leave
// live session rows pointing at a deleted account -- either a dangling
// credential or, under enforced foreign keys, a delete that fails outright.
//
// Two details make this discriminating rather than incidental. Every lookup
// after the delete passes now = base, far before the sessions expire, so
// expiry cannot be the reason a session is missing; and the surviving user's
// session is asserted live, so a cascade that over-matched (or a wholesale
// table wipe) fails here too.
func testUserDeleteCascadesToSessions(t *testing.T, newRepo newAuthRepoFunc) {
	repo := newRepo(t)
	ctx := context.Background()

	doomed := mustCreateUser(t, repo, "doomed")
	keeper := mustCreateUser(t, repo, "keeper")
	expires := base.Add(auth.SessionLifetime)

	_, doomedSession := mustCreateSession(t, repo, doomed.ID, base, expires)
	_, keeperSession := mustCreateSession(t, repo, keeper.ID, base, expires)

	// Sanity: both sessions are live before the delete, so the assertions
	// below cannot pass because a session was never stored in the first place.
	if _, err := repo.GetSession(ctx, doomedSession, base); err != nil {
		t.Fatalf("GetSession(doomed) before DeleteUser = %v, want the live session", err)
	}

	// Under enforced foreign keys, a schema missing ON DELETE CASCADE fails
	// this call outright rather than silently orphaning the session.
	if err := repo.DeleteUser(ctx, "doomed"); err != nil {
		t.Fatalf("DeleteUser(doomed) = %v, want no error (sessions must cascade)", err)
	}

	if _, err := repo.GetUserByID(ctx, doomed.ID); !errors.Is(err, auth.ErrNotFound) {
		t.Errorf("GetUserByID(doomed) after delete = %v, want auth.ErrNotFound", err)
	}
	if _, err := repo.GetSession(ctx, doomedSession, base); !errors.Is(err, auth.ErrNotFound) {
		t.Errorf("GetSession(doomed session, now well before expiry) = %v, want auth.ErrNotFound", err)
	}
	assertSessionGone(t, repo, "doomed user's session", doomedSession)

	if _, err := repo.GetSession(ctx, keeperSession, base); err != nil {
		t.Errorf("GetSession(keeper) after deleting the other user = %v, want the live session", err)
	}
}

// testTokenHashIsNotTheToken asserts the stored key is the hash and only the
// hash. The raw token lives in the cookie and must never be a valid lookup
// key, or a database leak would hand an attacker replayable sessions -- the
// whole reason NewToken returns two values.
func testTokenHashIsNotTheToken(t *testing.T, newRepo newAuthRepoFunc) {
	repo := newRepo(t)
	ctx := context.Background()

	user := mustCreateUser(t, repo, "admin")
	token, hash, err := auth.NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	if token == hash {
		t.Fatal("NewToken returned an identical token and hash")
	}
	if hash != auth.HashToken(token) {
		t.Fatalf("NewToken hash = %q, want HashToken(token) = %q", hash, auth.HashToken(token))
	}

	expires := base.Add(auth.SessionLifetime)
	if err = repo.CreateSession(ctx, hash, user.ID, base, expires); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// The raw token must not resolve. It would if the adapter stored the value
	// it was handed under a different name, or hashed its lookup argument.
	if _, err = repo.GetSession(ctx, token, base); !errors.Is(err, auth.ErrNotFound) {
		t.Errorf("GetSession(raw token) = %v, want auth.ErrNotFound (only the hash is stored)", err)
	}

	got, err := repo.GetSession(ctx, hash, base)
	if err != nil {
		t.Fatalf("GetSession(hash): %v", err)
	}
	if got.TokenHash != hash {
		t.Errorf("TokenHash = %q, want %q", got.TokenHash, hash)
	}
	if got.TokenHash == token {
		t.Error("stored TokenHash equals the raw token; the raw token must never reach the database")
	}
}

// testPasswordUpdatePersists asserts a password change is durable, stamps
// updated_at from the injected clock, and leaves created_at alone. A rewritten
// created_at is invisible in normal use and destroys the only record of when
// an account was provisioned.
func testPasswordUpdatePersists(t *testing.T, newRepo newAuthRepoFunc) {
	repo := newRepo(t)
	ctx := context.Background()

	created := mustCreateUser(t, repo, "admin")

	const newHash = "$argon2id$v=19$m=65536,t=3,p=4$bmV3c2FsdA$bmV3aGFzaA"
	changed := base.Add(48 * time.Hour)
	if err := repo.UpdatePassword(ctx, "admin", newHash, changed); err != nil {
		t.Fatalf("UpdatePassword: %v", err)
	}

	got, err := repo.GetUserByUsername(ctx, "admin")
	if err != nil {
		t.Fatalf("GetUserByUsername: %v", err)
	}
	if got.PasswordHash != newHash {
		t.Errorf("PasswordHash = %q, want %q", got.PasswordHash, newHash)
	}
	if got.ID != created.ID {
		t.Errorf("ID = %d, want %d (UpdatePassword must not replace the row)", got.ID, created.ID)
	}
	assertInstant(t, "UpdatedAt", got.UpdatedAt, changed)
	assertInstant(t, "CreatedAt", got.CreatedAt, base)
}

// testTimestampsSurvive32BitBoundary stores instants a day past the 32-bit
// signed overflow boundary (2038-01-19) and requires them back exactly. The
// migration test asserts only that the users and sessions tables exist -- it
// checks no column types -- so this subtest is what holds the epoch-seconds
// invariant: a column typed as 32-bit INTEGER (which is what Postgres INTEGER
// means) would truncate these, and a DATETIME/text column would round-trip a
// reformatted string rather than the instant written.
func testTimestampsSurvive32BitBoundary(t *testing.T, newRepo newAuthRepoFunc) {
	repo := newRepo(t)
	ctx := context.Background()

	far := time.Unix((1<<31)+86400, 0).UTC()

	user, err := repo.CreateUser(ctx, "admin", testPasswordHash, far)
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	assertInstant(t, "CreateUser CreatedAt", user.CreatedAt, far)

	got, err := repo.GetUserByID(ctx, user.ID)
	if err != nil {
		t.Fatalf("GetUserByID: %v", err)
	}
	assertInstant(t, "GetUserByID CreatedAt", got.CreatedAt, far)
	assertInstant(t, "GetUserByID UpdatedAt", got.UpdatedAt, far)

	expires := far.Add(auth.SessionLifetime)
	_, hash := mustCreateSession(t, repo, user.ID, far, expires)

	session, err := repo.GetSession(ctx, hash, far)
	if err != nil {
		t.Fatalf("GetSession(now = far future): %v", err)
	}
	assertInstant(t, "Session CreatedAt", session.CreatedAt, far)
	assertInstant(t, "Session ExpiresAt", session.ExpiresAt, expires)

	// The expiry comparison must still work past the boundary: a truncated
	// expires_at would read as a past instant and kill a live session.
	if _, err = repo.GetSession(ctx, hash, expires.Add(-time.Second)); err != nil {
		t.Errorf("GetSession(now = expiry-1s, past 2038) = %v, want the live session", err)
	}
}
