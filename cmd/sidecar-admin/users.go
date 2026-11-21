package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/OneBusAway/sidecar/internal/auth"
	"github.com/OneBusAway/sidecar/internal/store/sqlite"
)

// ---------------------------------------------------------------------------
// user
// ---------------------------------------------------------------------------

// runUser dispatches `sidecar-admin user`'s subcommands. This is the only way
// an admin account is ever created -- there is deliberately no web signup --
// so `user create` is what makes the admin UI reachable at all.
func runUser(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, store *sqlite.Store, now time.Time, args []string) error {
	if len(args) == 0 {
		return errors.New("user requires a subcommand: create, passwd, list, delete, sessions, revoke")
	}
	cmd, cmdArgs := args[0], args[1:]
	switch cmd {
	case "create":
		return userCreate(ctx, stdin, stdout, stderr, store, now, cmdArgs)
	case "passwd":
		return userPasswd(ctx, stdin, stdout, stderr, store, now, cmdArgs)
	case "list":
		return userList(ctx, stdout, store, cmdArgs)
	case "delete":
		return userDelete(ctx, stdout, store, cmdArgs)
	case "sessions":
		return userSessions(ctx, stdout, store, now, cmdArgs)
	case "revoke":
		return userRevoke(ctx, stdout, store, now, cmdArgs)
	default:
		return fmt.Errorf("unknown user subcommand %q; expected create, passwd, list, delete, sessions, or revoke", cmd)
	}
}

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
		if valErr := auth.ValidatePassword(string(first)); valErr != nil {
			fmt.Fprintln(stderr, valErr)
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

// userCreate is the only way an admin account is ever created: there is
// deliberately no web signup path. The whole request is validated --
// username shape, password length -- before any write, and the password
// never reaches disk unhashed.
func userCreate(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, store *sqlite.Store, now time.Time, args []string) error {
	fs := flag.NewFlagSet("user create", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	username := fs.String("username", "", "admin username (required)")
	fromStdin := fs.Bool("password-stdin", false, "read the password from stdin instead of an interactive prompt")
	if err := fs.Parse(args); err != nil {
		return err
	}
	seen := visitedFlags(fs)
	if !seen["username"] {
		return errors.New("user create requires --username")
	}

	normalized := auth.NormalizeUsername(*username)
	if err := auth.ValidateUsername(normalized); err != nil {
		return fmt.Errorf("user create: %w", err)
	}

	password, err := readPassword(stdin, stderr, *fromStdin)
	if err != nil {
		return fmt.Errorf("user create: %w", err)
	}
	// The interactive prompt in readPassword already validated the length
	// before it would ever return; the --password-stdin path has not, so it
	// is checked here regardless of which path was taken.
	if valErr := auth.ValidatePassword(password); valErr != nil {
		return fmt.Errorf("user create: %w", valErr)
	}

	hash, err := auth.HashPassword(password)
	if err != nil {
		return fmt.Errorf("user create: %w", err)
	}

	if _, createErr := store.Auth().CreateUser(ctx, normalized, hash, now); createErr != nil {
		if errors.Is(createErr, auth.ErrUsernameTaken) {
			return fmt.Errorf("user create: %q: %w", normalized, auth.ErrUsernameTaken)
		}
		return fmt.Errorf("user create: %w", createErr)
	}

	fmt.Fprintf(stdout, "created user %s\n", normalized)
	return nil
}

// userPasswd changes an existing admin's password and revokes every session
// tied to it: a password change is the compromised-credential recovery path,
// so a session surviving it for up to SessionLifetime would defeat the
// point. UpdatePassword does both in one transaction, so a failure part way
// through leaves the old password and its sessions as they were.
func userPasswd(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, store *sqlite.Store, now time.Time, args []string) error {
	fs := flag.NewFlagSet("user passwd", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	username := fs.String("username", "", "admin username (required)")
	fromStdin := fs.Bool("password-stdin", false, "read the password from stdin instead of an interactive prompt")
	if err := fs.Parse(args); err != nil {
		return err
	}
	seen := visitedFlags(fs)
	if !seen["username"] {
		return errors.New("user passwd requires --username")
	}
	normalized := auth.NormalizeUsername(*username)

	password, err := readPassword(stdin, stderr, *fromStdin)
	if err != nil {
		return fmt.Errorf("user passwd: %w", err)
	}
	if valErr := auth.ValidatePassword(password); valErr != nil {
		return fmt.Errorf("user passwd: %w", valErr)
	}

	hash, err := auth.HashPassword(password)
	if err != nil {
		return fmt.Errorf("user passwd: %w", err)
	}

	if updateErr := store.Auth().UpdatePassword(ctx, normalized, hash, now); updateErr != nil {
		if errors.Is(updateErr, auth.ErrNotFound) {
			return fmt.Errorf("user passwd: user %q not found", normalized)
		}
		return fmt.Errorf("user passwd: %w", updateErr)
	}

	fmt.Fprintf(stdout, "password updated for %s; all sessions revoked\n", normalized)
	return nil
}

// userList prints one line per admin account: username, a tab, and the
// account's creation time in RFC 3339 UTC.
func userList(ctx context.Context, stdout io.Writer, store *sqlite.Store, args []string) error {
	fs := flag.NewFlagSet("user list", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		return err
	}

	users, err := store.Auth().ListUsers(ctx)
	if err != nil {
		return fmt.Errorf("user list: %w", err)
	}
	sort.Slice(users, func(i, j int) bool { return users[i].Username < users[j].Username })
	for _, u := range users {
		fmt.Fprintf(stdout, "%s\t%s\n", u.Username, u.CreatedAt.UTC().Format(time.RFC3339))
	}
	return nil
}

// userDelete removes an admin account and, via the schema's ON DELETE
// CASCADE, every session tied to it. Deleting an unknown username is always
// an error, never a silent success -- an established convention in this CLI
// (see DeleteUser's doc comment). Deleting the last remaining account is
// also rejected unless --force is passed: it would make the admin UI
// unreachable until the next `user create`, and there is deliberately no web
// signup to recover through. The last-user check and the delete happen in
// one atomic repository call, so two concurrent deletes cannot both pass it.
func userDelete(ctx context.Context, stdout io.Writer, store *sqlite.Store, args []string) error {
	fs := flag.NewFlagSet("user delete", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	username := fs.String("username", "", "admin username (required)")
	force := fs.Bool("force", false, "allow deleting the last remaining admin account")
	if err := fs.Parse(args); err != nil {
		return err
	}
	seen := visitedFlags(fs)
	if !seen["username"] {
		return errors.New("user delete requires --username")
	}
	normalized := auth.NormalizeUsername(*username)

	// DeleteUserUnlessLast reports an unknown username as ErrNotFound before
	// it considers the last-user guard, so a typo is never answered with the
	// --force hint.
	var deleteErr error
	if *force {
		deleteErr = store.Auth().DeleteUser(ctx, normalized)
	} else {
		deleteErr = store.Auth().DeleteUserUnlessLast(ctx, normalized)
	}
	switch {
	case errors.Is(deleteErr, auth.ErrNotFound):
		return fmt.Errorf("user delete: user %q not found", normalized)
	case errors.Is(deleteErr, auth.ErrLastUser):
		return fmt.Errorf(
			"user delete: %q is the last admin account; deleting it would leave the admin UI "+
				"unreachable until the next `sidecar-admin user create`. Pass --force to delete it anyway",
			normalized)
	case deleteErr != nil:
		return fmt.Errorf("user delete: %w", deleteErr)
	}

	fmt.Fprintf(stdout, "deleted user %s\n", normalized)
	return nil
}

// sessionIDLen is how much of a session's token hash `user sessions` shows
// and `user revoke --session` matches on; minSessionIDLen is the shortest
// prefix revoke accepts. The hash is SHA-256 of the cookie token, so even
// the whole value grants nothing -- the prefix is just shorter to type.
const (
	sessionIDLen    = 12
	minSessionIDLen = 8
)

// userSessions prints one line per live admin session (SC-25): id, a tab,
// username, created-at, and the current idle deadline, all RFC 3339 UTC.
func userSessions(ctx context.Context, stdout io.Writer, store *sqlite.Store, now time.Time, args []string) error {
	fs := flag.NewFlagSet("user sessions", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	username := fs.String("username", "", "only this account's sessions")
	if err := fs.Parse(args); err != nil {
		return err
	}
	sessions, names, err := liveSessions(ctx, store, now)
	if err != nil {
		return fmt.Errorf("user sessions: %w", err)
	}
	filter := auth.NormalizeUsername(*username)
	for _, s := range sessions {
		if filter != "" && names[s.UserID] != filter {
			continue
		}
		fmt.Fprintf(stdout, "%s\t%s\t%s\t%s\n", s.TokenHash[:sessionIDLen], names[s.UserID],
			s.CreatedAt.UTC().Format(time.RFC3339), s.ExpiresAt.UTC().Format(time.RFC3339))
	}
	return nil
}

// userRevoke ends one session (--session <id>) or every session of one
// account (--username). Exactly one selector is required; an id must be at
// least minSessionIDLen characters and match exactly one live session.
func userRevoke(ctx context.Context, stdout io.Writer, store *sqlite.Store, now time.Time, args []string) error {
	fs := flag.NewFlagSet("user revoke", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	id := fs.String("session", "", "id (token-hash prefix) from `user sessions`")
	username := fs.String("username", "", "revoke every session of this account")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if (*id == "") == (*username == "") {
		return errors.New("user revoke requires exactly one of --session or --username")
	}
	if *username != "" {
		normalized := auth.NormalizeUsername(*username)
		user, err := store.Auth().GetUserByUsername(ctx, normalized)
		if err != nil {
			if errors.Is(err, auth.ErrNotFound) {
				return fmt.Errorf("user revoke: user %q not found", normalized)
			}
			return fmt.Errorf("user revoke: %w", err)
		}
		n, err := store.Auth().DeleteUserSessions(ctx, user.ID)
		if err != nil {
			return fmt.Errorf("user revoke: %w", err)
		}
		fmt.Fprintf(stdout, "revoked %d session(s) for %s\n", n, normalized)
		return nil
	}

	prefix := strings.ToLower(*id)
	if len(prefix) < minSessionIDLen {
		return fmt.Errorf("user revoke: --session needs at least %d characters of the id", minSessionIDLen)
	}
	sessions, _, err := liveSessions(ctx, store, now)
	if err != nil {
		return fmt.Errorf("user revoke: %w", err)
	}
	var match []auth.Session
	for _, s := range sessions {
		if strings.HasPrefix(s.TokenHash, prefix) {
			match = append(match, s)
		}
	}
	switch len(match) {
	case 0:
		return fmt.Errorf("user revoke: no live session with id %q", prefix)
	case 1:
	default:
		return fmt.Errorf("user revoke: id %q matches %d sessions; give more characters", prefix, len(match))
	}
	if err := store.Auth().DeleteSession(ctx, match[0].TokenHash); err != nil {
		return fmt.Errorf("user revoke: %w", err)
	}
	fmt.Fprintf(stdout, "revoked session %s\n", match[0].TokenHash[:sessionIDLen])
	return nil
}

// liveSessions returns the sessions still live at now (an expired row awaits
// the next sign-in's sweep and is not worth showing) and a user-id to
// username map.
func liveSessions(ctx context.Context, store *sqlite.Store, now time.Time) ([]auth.Session, map[int64]string, error) {
	all, err := store.Auth().ListSessions(ctx)
	if err != nil {
		return nil, nil, err
	}
	users, err := store.Auth().ListUsers(ctx)
	if err != nil {
		return nil, nil, err
	}
	names := make(map[int64]string, len(users))
	for _, u := range users {
		names[u.ID] = u.Username
	}
	live := all[:0]
	for _, s := range all {
		if s.ExpiresAt.After(now) {
			live = append(live, s)
		}
	}
	return live, names, nil
}
