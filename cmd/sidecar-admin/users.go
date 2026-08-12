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
		return errors.New("user requires a subcommand: create, passwd, list, delete")
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
	default:
		return fmt.Errorf("unknown user subcommand %q; expected create, passwd, list, or delete", cmd)
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
			return fmt.Errorf("user create: username %q already taken: %w", normalized, auth.ErrUsernameTaken)
		}
		return fmt.Errorf("user create: %w", createErr)
	}

	fmt.Fprintf(stdout, "created user %s\n", normalized)
	return nil
}

// userPasswd changes an existing admin's password and revokes every session
// tied to it: a password change is the compromised-credential recovery path,
// so a session surviving it for up to SessionLifetime would defeat the
// point.
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

	// UpdatePassword takes a username, not an id; the id is needed to scope
	// the session revocation that follows.
	user, err := store.Auth().GetUserByUsername(ctx, normalized)
	if err != nil {
		return fmt.Errorf("user passwd: %w", err)
	}
	if _, revokeErr := store.Auth().DeleteUserSessions(ctx, user.ID); revokeErr != nil {
		return fmt.Errorf("user passwd: revoke sessions for %q: %w", normalized, revokeErr)
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
// signup to recover through.
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

	// Confirmed to exist before the last-user guard runs, so an unknown
	// username is always reported as "not found" rather than folded into
	// (or masked by) the last-user check below.
	if _, getErr := store.Auth().GetUserByUsername(ctx, normalized); getErr != nil {
		if errors.Is(getErr, auth.ErrNotFound) {
			return fmt.Errorf("user delete: user %q not found", normalized)
		}
		return fmt.Errorf("user delete: %w", getErr)
	}

	if !*force {
		users, listErr := store.Auth().ListUsers(ctx)
		if listErr != nil {
			return fmt.Errorf("user delete: %w", listErr)
		}
		if len(users) == 1 {
			return fmt.Errorf(
				"user delete: %q is the last admin account; deleting it would leave the admin UI "+
					"unreachable until the next `sidecar-admin user create`. Pass --force to delete it anyway",
				normalized)
		}
	}

	if deleteErr := store.Auth().DeleteUser(ctx, normalized); deleteErr != nil {
		if errors.Is(deleteErr, auth.ErrNotFound) {
			return fmt.Errorf("user delete: user %q not found", normalized)
		}
		return fmt.Errorf("user delete: %w", deleteErr)
	}

	fmt.Fprintf(stdout, "deleted user %s\n", normalized)
	return nil
}
