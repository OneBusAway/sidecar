package main

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/auth"
	"github.com/OneBusAway/sidecar/internal/store/sqlite"
)

// cliStdin is cli (see commands_test.go) with an explicit stdin reader, for
// `user create`/`user passwd --password-stdin`.
func cliStdin(t *testing.T, stdin io.Reader, dbPath string, args ...string) (string, string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	full := append([]string{"--db", dbPath}, args...)
	err := run(stdin, &stdout, &stderr, full)
	return stdout.String(), stderr.String(), err
}

// validPassword satisfies auth.MinPasswordLen (12) with room to spare.
const validPassword = "hunter2hunter2"

// createUserWithSession creates username through `user create` with
// validPassword and seeds one live session for it, the way a real login
// would have. It returns the session's token hash and the instant the
// session was minted.
func createUserWithSession(t *testing.T, dbPath string, store *sqlite.Store, username, token string) (tokenHash string, now time.Time) {
	t.Helper()
	ctx := context.Background()

	if _, _, err := cliStdin(t, strings.NewReader(validPassword), dbPath,
		"user", "create", "--username", username, "--password-stdin"); err != nil {
		t.Fatalf("user create: %v", err)
	}
	user, err := store.Auth().GetUserByUsername(ctx, username)
	if err != nil {
		t.Fatalf("GetUserByUsername: %v", err)
	}
	tokenHash = auth.HashToken(token)
	now = time.Now()
	if err = store.Auth().CreateSession(ctx, tokenHash, user.ID, user.PasswordHash, now, now.Add(time.Hour)); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return tokenHash, now
}

func TestUserCreate_ValidPasswordCreatesRow(t *testing.T) {
	t.Parallel()
	dbPath, store := newDB(t)

	stdout, _, err := cliStdin(t, strings.NewReader(validPassword), dbPath,
		"user", "create", "--username", "alice", "--password-stdin")
	if err != nil {
		t.Fatalf("user create: %v", err)
	}
	if !strings.Contains(stdout, "created user alice") {
		t.Errorf("stdout = %q, want it to contain %q", stdout, "created user alice")
	}

	row, err := store.Auth().GetUserByUsername(context.Background(), "alice")
	if err != nil {
		t.Fatalf("GetUserByUsername: %v", err)
	}
	ok, err := auth.VerifyPassword(row.PasswordHash, validPassword)
	if err != nil {
		t.Fatalf("VerifyPassword: %v", err)
	}
	if !ok {
		t.Error("VerifyPassword(stored hash, the password just created with) = false, want true")
	}
}

func TestUserCreate_ShortPasswordRejected(t *testing.T) {
	t.Parallel()
	dbPath, store := newDB(t)

	const short = "shortpasswd" // 11 bytes, one short of auth.MinPasswordLen
	if len(short) != 11 {
		t.Fatalf("fixture: len(short) = %d, want 11", len(short))
	}

	_, _, err := cliStdin(t, strings.NewReader(short), dbPath,
		"user", "create", "--username", "alice", "--password-stdin")
	if err == nil {
		t.Fatal("user create with an 11-char password: want error, got nil")
	}
	if !strings.Contains(err.Error(), "at least 12") {
		t.Errorf("error = %q, want it to mention the minimum length", err.Error())
	}

	if _, gerr := store.Auth().GetUserByUsername(context.Background(), "alice"); !errors.Is(gerr, auth.ErrNotFound) {
		t.Errorf("GetUserByUsername after a rejected create: err = %v, want auth.ErrNotFound (rejection must write nothing)", gerr)
	}
}

func TestUserCreate_DuplicateUsernameRejected(t *testing.T) {
	t.Parallel()
	dbPath, _ := newDB(t)

	if _, _, err := cliStdin(t, strings.NewReader(validPassword), dbPath,
		"user", "create", "--username", "alice", "--password-stdin"); err != nil {
		t.Fatalf("first user create: %v", err)
	}

	// A case variant must collide too: usernames are normalized.
	_, _, err := cliStdin(t, strings.NewReader(validPassword), dbPath,
		"user", "create", "--username", "Alice", "--password-stdin")
	if err == nil {
		t.Fatal("user create with a duplicate (case-variant) username: want error, got nil")
	}
	if !strings.Contains(err.Error(), "already taken") {
		t.Errorf("error = %q, want it to mention the username is already taken", err.Error())
	}
}

// TestUserCreate_PasswordFlagRejected and TestUserPasswd_PasswordFlagRejected
// lock in the single security invariant this task exists to protect: there
// is deliberately no --password flag, because passwords must never reach
// shell history or `ps` output. This asserts the flag is genuinely
// unregistered with flag.FlagSet -- not merely unused -- so a future
// "for convenience" addition of --password breaks a test rather than
// shipping silently with every other test still green.
func TestUserCreate_PasswordFlagRejected(t *testing.T) {
	t.Parallel()
	dbPath, _ := newDB(t)

	_, _, err := cli(t, dbPath, "user", "create", "--username", "alice", "--password", validPassword)
	if err == nil {
		t.Fatal("user create --password: want error, got nil")
	}
	if !strings.Contains(err.Error(), "flag provided but not defined") {
		t.Errorf("error = %q, want it to report --password as an undefined flag", err.Error())
	}
}

func TestUserPasswd_PasswordFlagRejected(t *testing.T) {
	t.Parallel()
	dbPath, _ := newDB(t)

	_, _, err := cli(t, dbPath, "user", "passwd", "--username", "alice", "--password", validPassword)
	if err == nil {
		t.Fatal("user passwd --password: want error, got nil")
	}
	if !strings.Contains(err.Error(), "flag provided but not defined") {
		t.Errorf("error = %q, want it to report --password as an undefined flag", err.Error())
	}
}

func TestUserCreate_MissingUsernameRejected(t *testing.T) {
	t.Parallel()
	dbPath, _ := newDB(t)

	_, _, err := cliStdin(t, strings.NewReader(validPassword), dbPath,
		"user", "create", "--password-stdin")
	if err == nil {
		t.Fatal("user create without --username: want error, got nil")
	}
}

func TestUserCreate_PasswordTrailingNewlineTrimmed(t *testing.T) {
	t.Parallel()
	dbPath, store := newDB(t)

	_, _, err := cliStdin(t, strings.NewReader(validPassword+"\n"), dbPath,
		"user", "create", "--username", "alice", "--password-stdin")
	if err != nil {
		t.Fatalf("user create: %v", err)
	}

	row, err := store.Auth().GetUserByUsername(context.Background(), "alice")
	if err != nil {
		t.Fatalf("GetUserByUsername: %v", err)
	}
	ok, err := auth.VerifyPassword(row.PasswordHash, validPassword)
	if err != nil {
		t.Fatalf("VerifyPassword: %v", err)
	}
	if !ok {
		t.Error("VerifyPassword against the bare password (no trailing newline) = false, want true -- the newline must be trimmed")
	}
}

func TestUserList_PrintsUsernameAndCreatedAt(t *testing.T) {
	t.Parallel()
	dbPath, _ := newDB(t)

	if _, _, err := cliStdin(t, strings.NewReader(validPassword), dbPath,
		"user", "create", "--username", "alice", "--password-stdin"); err != nil {
		t.Fatalf("user create: %v", err)
	}

	stdout, _, err := cli(t, dbPath, "user", "list")
	if err != nil {
		t.Fatalf("user list: %v", err)
	}

	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("user list produced %d lines, want 1: %q", len(lines), stdout)
	}
	fields := strings.Split(lines[0], "\t")
	if len(fields) != 2 {
		t.Fatalf("user list line = %q, want two tab-separated fields", lines[0])
	}
	if fields[0] != "alice" {
		t.Errorf("username field = %q, want %q", fields[0], "alice")
	}
	if _, perr := time.Parse(time.RFC3339, fields[1]); perr != nil {
		t.Errorf("created-at field = %q, want RFC 3339: %v", fields[1], perr)
	}
}

func TestUserPasswd_UpdatesHashAndRevokesSessions(t *testing.T) {
	t.Parallel()
	dbPath, store := newDB(t)
	ctx := context.Background()
	tokenHash, now := createUserWithSession(t, dbPath, store, "alice", "test-token")

	const newPassword = "newpassword12"
	stdout, _, err := cliStdin(t, strings.NewReader(newPassword), dbPath,
		"user", "passwd", "--username", "alice", "--password-stdin")
	if err != nil {
		t.Fatalf("user passwd: %v", err)
	}
	if !strings.Contains(stdout, "password updated for alice; all sessions revoked") {
		t.Errorf("stdout = %q, want it to contain %q", stdout, "password updated for alice; all sessions revoked")
	}

	updated, err := store.Auth().GetUserByUsername(ctx, "alice")
	if err != nil {
		t.Fatalf("GetUserByUsername after passwd: %v", err)
	}
	if ok, verr := auth.VerifyPassword(updated.PasswordHash, validPassword); verr != nil {
		t.Fatalf("VerifyPassword(old): %v", verr)
	} else if ok {
		t.Error("old password still verifies after `user passwd`, want it rejected")
	}
	if ok, verr := auth.VerifyPassword(updated.PasswordHash, newPassword); verr != nil {
		t.Fatalf("VerifyPassword(new): %v", verr)
	} else if !ok {
		t.Error("new password does not verify after `user passwd`, want it accepted")
	}

	if _, gerr := store.Auth().GetSession(ctx, tokenHash, now); !errors.Is(gerr, auth.ErrNotFound) {
		t.Errorf("GetSession after `user passwd`: err = %v, want auth.ErrNotFound (sessions must be revoked)", gerr)
	}
}

func TestUserPasswd_UnknownUserRejected(t *testing.T) {
	t.Parallel()
	dbPath, _ := newDB(t)

	_, _, err := cliStdin(t, strings.NewReader(validPassword), dbPath,
		"user", "passwd", "--username", "ghost", "--password-stdin")
	if err == nil {
		t.Fatal("user passwd for an unknown user: want error, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %q, want it to mention the account was not found", err.Error())
	}
}

// TestUserPasswd_RevocationFailureKeepsOldPassword makes the session delete
// fail inside the database, through a trigger, after the new hash has been
// written. The command must report an error and change nothing: the old
// password still verifies and the old session still works. Before the hash
// change and the revocation shared a transaction, this left the new hash
// committed and the old session alive.
func TestUserPasswd_RevocationFailureKeepsOldPassword(t *testing.T) {
	t.Parallel()
	dbPath, store := newDB(t)
	ctx := context.Background()
	tokenHash, now := createUserWithSession(t, dbPath, store, "alice", "old-session-token")

	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })
	if _, err = raw.ExecContext(ctx, `CREATE TRIGGER fail_session_delete BEFORE DELETE ON sessions
		BEGIN SELECT RAISE(ABORT, 'injected session delete failure'); END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	t.Cleanup(func() {
		if _, dropErr := raw.ExecContext(context.Background(), `DROP TRIGGER IF EXISTS fail_session_delete`); dropErr != nil {
			t.Errorf("drop trigger: %v", dropErr)
		}
	})

	stdout, _, err := cliStdin(t, strings.NewReader("newpassword12"), dbPath,
		"user", "passwd", "--username", "alice", "--password-stdin")
	if err == nil {
		t.Fatal("user passwd with a failing session delete: want error, got nil")
	}
	if !strings.Contains(err.Error(), "injected session delete failure") {
		t.Errorf("error = %q, want it to carry the trigger's failure", err.Error())
	}
	if strings.Contains(stdout, "password updated") {
		t.Errorf("stdout = %q, want no success line after a failed change", stdout)
	}

	after, err := store.Auth().GetUserByUsername(ctx, "alice")
	if err != nil {
		t.Fatalf("GetUserByUsername after failed passwd: %v", err)
	}
	if ok, verr := auth.VerifyPassword(after.PasswordHash, validPassword); verr != nil || !ok {
		t.Errorf("VerifyPassword(old) after failed passwd = %v, %v, want true, nil", ok, verr)
	}
	if _, gerr := store.Auth().GetSession(ctx, tokenHash, now); gerr != nil {
		t.Errorf("GetSession after failed passwd = %v, want the old session still alive", gerr)
	}
}

func TestUserDelete_TwoUsersCascadesSessions(t *testing.T) {
	t.Parallel()
	dbPath, store := newDB(t)
	ctx := context.Background()

	for _, name := range []string{"alice", "bob"} {
		if _, _, err := cliStdin(t, strings.NewReader(validPassword), dbPath,
			"user", "create", "--username", name, "--password-stdin"); err != nil {
			t.Fatalf("user create %s: %v", name, err)
		}
	}
	alice, err := store.Auth().GetUserByUsername(ctx, "alice")
	if err != nil {
		t.Fatalf("GetUserByUsername: %v", err)
	}
	tokenHash := auth.HashToken("alice-token")
	now := time.Now()
	if sessErr := store.Auth().CreateSession(ctx, tokenHash, alice.ID, alice.PasswordHash, now, now.Add(time.Hour)); sessErr != nil {
		t.Fatalf("CreateSession: %v", sessErr)
	}

	stdout, _, err := cli(t, dbPath, "user", "delete", "--username", "alice")
	if err != nil {
		t.Fatalf("user delete: %v", err)
	}
	if !strings.Contains(stdout, "deleted user alice") {
		t.Errorf("stdout = %q, want it to contain %q", stdout, "deleted user alice")
	}

	if _, gerr := store.Auth().GetUserByUsername(ctx, "alice"); !errors.Is(gerr, auth.ErrNotFound) {
		t.Errorf("GetUserByUsername(alice) after delete: err = %v, want auth.ErrNotFound", gerr)
	}
	if _, gerr := store.Auth().GetSession(ctx, tokenHash, now); !errors.Is(gerr, auth.ErrNotFound) {
		t.Errorf("GetSession after deleting its owner: err = %v, want auth.ErrNotFound (cascade)", gerr)
	}
	if _, gerr := store.Auth().GetUserByUsername(ctx, "bob"); gerr != nil {
		t.Errorf("GetUserByUsername(bob) after deleting alice: %v, want bob untouched", gerr)
	}
}

func TestUserDelete_LastUserWithoutForceRejected(t *testing.T) {
	t.Parallel()
	dbPath, store := newDB(t)

	if _, _, err := cliStdin(t, strings.NewReader(validPassword), dbPath,
		"user", "create", "--username", "alice", "--password-stdin"); err != nil {
		t.Fatalf("user create: %v", err)
	}

	_, _, err := cli(t, dbPath, "user", "delete", "--username", "alice")
	if err == nil {
		t.Fatal("user delete of the last admin account without --force: want error, got nil")
	}
	if !strings.Contains(err.Error(), "--force") {
		t.Errorf("error = %q, want it to suggest --force", err.Error())
	}
	if !strings.Contains(err.Error(), "user create") {
		t.Errorf("error = %q, want it to name the `user create` recovery path", err.Error())
	}

	if _, gerr := store.Auth().GetUserByUsername(context.Background(), "alice"); gerr != nil {
		t.Errorf("GetUserByUsername(alice) after a rejected delete: %v, want the account still present", gerr)
	}
}

func TestUserDelete_LastUserWithForceSucceeds(t *testing.T) {
	t.Parallel()
	dbPath, store := newDB(t)

	if _, _, err := cliStdin(t, strings.NewReader(validPassword), dbPath,
		"user", "create", "--username", "alice", "--password-stdin"); err != nil {
		t.Fatalf("user create: %v", err)
	}

	if _, _, err := cli(t, dbPath, "user", "delete", "--username", "alice", "--force"); err != nil {
		t.Fatalf("user delete --force: %v", err)
	}

	if _, gerr := store.Auth().GetUserByUsername(context.Background(), "alice"); !errors.Is(gerr, auth.ErrNotFound) {
		t.Errorf("GetUserByUsername(alice) after --force delete: err = %v, want auth.ErrNotFound", gerr)
	}
}

func TestUserDelete_UnknownUserRejected(t *testing.T) {
	t.Parallel()
	dbPath, _ := newDB(t)

	_, _, err := cli(t, dbPath, "user", "delete", "--username", "ghost")
	if err == nil {
		t.Fatal("user delete of an unknown username: want error, got nil (silent success is never acceptable here)")
	}
}

// TestUserSessions_ListsAndRevokes pins SC-25's operator tooling: `user
// sessions` lists live admin sessions by a short id (a token-hash prefix,
// never the token), and `user revoke` ends one session by that id or every
// session of one account.
func TestUserSessions_ListsAndRevokes(t *testing.T) {
	t.Parallel()
	dbPath, store := newDB(t)
	ctx := context.Background()
	aliceHash, now := createUserWithSession(t, dbPath, store, "alice", "alice-token-1")
	bobHash, _ := createUserWithSession(t, dbPath, store, "bob", "bob-token-1")
	bob, err := store.Auth().GetUserByUsername(ctx, "bob")
	if err != nil {
		t.Fatal(err)
	}
	bob2 := auth.HashToken("bob-token-2")
	if err = store.Auth().CreateSession(ctx, bob2, bob.ID, bob.PasswordHash, now, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	stdout, _, err := cli(t, dbPath, "user", "sessions")
	if err != nil {
		t.Fatalf("user sessions: %v", err)
	}
	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("user sessions printed %d lines, want 3: %q", len(lines), stdout)
	}
	if !strings.Contains(stdout, aliceHash[:12]) || strings.Contains(stdout, aliceHash) {
		t.Errorf("user sessions should show a 12-char id, not the full hash: %q", stdout)
	}
	if strings.Contains(stdout, "alice-token-1") {
		t.Error("user sessions printed a raw token")
	}

	filtered, _, err := cli(t, dbPath, "user", "sessions", "--username", "bob")
	if err != nil {
		t.Fatalf("user sessions --username: %v", err)
	}
	if n := strings.Count(filtered, "\tbob\t"); n != 2 || strings.Contains(filtered, "alice") {
		t.Errorf("user sessions --username bob = %q, want bob's two sessions only", filtered)
	}

	if _, _, err = cli(t, dbPath, "user", "revoke", "--session", aliceHash[:12]); err != nil {
		t.Fatalf("user revoke --session: %v", err)
	}
	if _, gerr := store.Auth().GetSession(ctx, aliceHash, now); !errors.Is(gerr, auth.ErrNotFound) {
		t.Errorf("alice's session after revoke = %v, want ErrNotFound", gerr)
	}

	if _, _, err = cli(t, dbPath, "user", "revoke", "--username", "bob"); err != nil {
		t.Fatalf("user revoke --username: %v", err)
	}
	for _, h := range []string{bobHash, bob2} {
		if _, gerr := store.Auth().GetSession(ctx, h, now); !errors.Is(gerr, auth.ErrNotFound) {
			t.Errorf("bob's session after revoke --username = %v, want ErrNotFound", gerr)
		}
	}

	// Refusals: a too-short or unknown id, and neither/both selectors.
	for _, args := range [][]string{
		{"user", "revoke", "--session", "abc"},
		{"user", "revoke", "--session", "ffffffffffff"},
		{"user", "revoke"},
		{"user", "revoke", "--session", aliceHash[:12], "--username", "bob"},
		{"user", "revoke", "--username", "nobody"},
	} {
		if _, _, rerr := cli(t, dbPath, args...); rerr == nil {
			t.Errorf("%v succeeded, want an error", args)
		}
	}
}
