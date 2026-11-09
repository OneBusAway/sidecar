package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/OneBusAway/sidecar/internal/auth"
	"github.com/OneBusAway/sidecar/internal/store/sqlite/gen"
)

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

func sessionFromRow(s gen.Session) auth.Session {
	return auth.Session{
		TokenHash: s.TokenHash,
		UserID:    s.UserID,
		CreatedAt: unixToTime(s.CreatedAt),
		ExpiresAt: unixToTime(s.ExpiresAt),
	}
}

// ---------------------------------------------------------------------------
// users
// ---------------------------------------------------------------------------

// CreateUser normalizes and validates here, not just in callers, for the same
// reason alertRepo.Create enforces AgencyID: the Repository contract protects
// every caller, including ones not written yet.
func (r *authRepo) CreateUser(ctx context.Context, username, passwordHash string, now time.Time) (auth.User, error) {
	username = auth.NormalizeUsername(username)
	if err := auth.ValidateUsername(username); err != nil {
		return auth.User{}, fmt.Errorf("sqlite: create user: %w", err)
	}
	// The column is NOT NULL but not non-empty: without this a caller that
	// forgot to hash would store an account whose verification compares
	// against "".
	if passwordHash == "" {
		return auth.User{}, errors.New("sqlite: create user: password hash must not be empty")
	}

	ts := now.Unix()
	row, err := r.q.CreateUser(ctx, gen.CreateUserParams{
		Username:     username,
		PasswordHash: passwordHash,
		CreatedAt:    ts,
		UpdatedAt:    ts,
	})
	if err != nil {
		// modernc.org/sqlite exposes constraint failures via the error string;
		// matching the full "UNIQUE constraint failed: <table.col>" text keeps
		// this specific to the username constraint. Mapping the violation is
		// deliberate: a pre-check SELECT would race two concurrent creates and
		// surface a raw driver error to whichever lost.
		if isUniqueViolation(err, "users.username") {
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

func (r *authRepo) GetUserByID(ctx context.Context, id int64) (auth.User, error) {
	row, err := r.q.GetUserByID(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return auth.User{}, fmt.Errorf("sqlite: get user %d: %w", id, auth.ErrNotFound)
		}
		return auth.User{}, fmt.Errorf("sqlite: get user %d: %w", id, err)
	}
	return userFromRow(row), nil
}

func (r *authRepo) ListUsers(ctx context.Context) ([]auth.User, error) {
	rows, err := r.q.ListUsers(ctx)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list users: %w", err)
	}
	out := make([]auth.User, len(rows))
	for i, row := range rows {
		out[i] = userFromRow(row)
	}
	return out, nil
}

// DeleteUser reports auth.ErrNotFound when nothing matched. The DELETE is an
// :execrows statement, so a missing account is not a driver error: without the
// count check `sidecar-admin user delete typo` would exit 0 and print nothing
// while the real account stayed live. Sessions go with the user via the
// schema's ON DELETE CASCADE.
func (r *authRepo) DeleteUser(ctx context.Context, username string) error {
	username = auth.NormalizeUsername(username)
	n, err := r.q.DeleteUser(ctx, username)
	if err != nil {
		return fmt.Errorf("sqlite: delete user %q: %w", username, err)
	}
	if n == 0 {
		return fmt.Errorf("sqlite: delete user %q: %w", username, auth.ErrNotFound)
	}
	return nil
}

// DeleteUserUnlessLast reads the user, counts the table, and deletes in one
// immediate transaction (the store opens with _txlock=immediate), so a
// concurrent delete waits on busy_timeout at BEGIN instead of counting the
// same rows this call is about to change. The lookup runs first so an
// unknown username is ErrNotFound even when the table holds one account.
func (r *authRepo) DeleteUserUnlessLast(ctx context.Context, username string) error {
	username = auth.NormalizeUsername(username)
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: delete user %q: begin tx: %w", username, err)
	}
	//nolint:errcheck // rollback after a successful commit is a documented no-op; the error is expected and safe to ignore
	defer func() { _ = tx.Rollback() }()
	q := r.q.WithTx(tx)

	if _, err = q.GetUserByUsername(ctx, username); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("sqlite: delete user %q: %w", username, auth.ErrNotFound)
		}
		return fmt.Errorf("sqlite: delete user %q: %w", username, err)
	}
	n, err := q.CountUsers(ctx)
	if err != nil {
		return fmt.Errorf("sqlite: delete user %q: count users: %w", username, err)
	}
	if n <= 1 {
		return fmt.Errorf("sqlite: delete user %q: %w", username, auth.ErrLastUser)
	}
	if _, err = q.DeleteUser(ctx, username); err != nil {
		return fmt.Errorf("sqlite: delete user %q: %w", username, err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: delete user %q: commit: %w", username, err)
	}
	return nil
}

// UpdatePassword looks the user up, writes the new hash, and deletes the
// user's sessions in one immediate transaction (the store opens with
// _txlock=immediate). A failure at any step rolls the hash back too, so a
// password change cannot half-apply and leave old-password sessions alive.
// An unknown username is auth.ErrNotFound, for the same reason DeleteUser
// reports it.
func (r *authRepo) UpdatePassword(ctx context.Context, username, passwordHash string, now time.Time) error {
	username = auth.NormalizeUsername(username)
	if passwordHash == "" {
		return errors.New("sqlite: update password: password hash must not be empty")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: update password for %q: begin tx: %w", username, err)
	}
	//nolint:errcheck // rollback after a successful commit is a documented no-op; the error is expected and safe to ignore
	defer func() { _ = tx.Rollback() }()
	q := r.q.WithTx(tx)

	user, err := q.GetUserByUsername(ctx, username)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("sqlite: update password for %q: %w", username, auth.ErrNotFound)
		}
		return fmt.Errorf("sqlite: update password for %q: %w", username, err)
	}
	if _, err = q.UpdateUserPassword(ctx, gen.UpdateUserPasswordParams{
		PasswordHash: passwordHash,
		UpdatedAt:    now.Unix(),
		Username:     username,
	}); err != nil {
		return fmt.Errorf("sqlite: update password for %q: %w", username, err)
	}
	if _, err = q.DeleteUserSessions(ctx, user.ID); err != nil {
		return fmt.Errorf("sqlite: update password for %q: revoke sessions: %w", username, err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: update password for %q: commit: %w", username, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// sessions
// ---------------------------------------------------------------------------

// CreateSession reads the user, compares its password hash, and inserts the
// session in one immediate transaction (the store opens with
// _txlock=immediate). A concurrent UpdatePassword therefore either commits
// first, and the stale hash is refused here, or waits at BEGIN until this
// session exists and then revokes it with the rest.
func (r *authRepo) CreateSession(ctx context.Context, tokenHash string, userID int64, passwordHash string, now, expiresAt time.Time) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: create session for user %d: begin tx: %w", userID, err)
	}
	//nolint:errcheck // rollback after a successful commit is a documented no-op; the error is expected and safe to ignore
	defer func() { _ = tx.Rollback() }()
	q := r.q.WithTx(tx)

	user, err := q.GetUserByID(ctx, userID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("sqlite: create session for user %d: %w", userID, auth.ErrNotFound)
		}
		return fmt.Errorf("sqlite: create session for user %d: %w", userID, err)
	}
	if user.PasswordHash != passwordHash {
		return fmt.Errorf("sqlite: create session for user %d: %w", userID, auth.ErrPasswordChanged)
	}
	if err = q.CreateSession(ctx, gen.CreateSessionParams{
		TokenHash: tokenHash,
		UserID:    userID,
		CreatedAt: now.Unix(),
		ExpiresAt: expiresAt.Unix(),
	}); err != nil {
		return fmt.Errorf("sqlite: create session for user %d: %w", userID, err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: create session for user %d: commit: %w", userID, err)
	}
	return nil
}

// GetSession implements the delete-on-read contract: an observed expired row
// is removed before ErrNotFound is returned (auth.Repository doc comment).
// Expiry is evaluated against the passed now, never the database clock, and a
// session is dead AT expires_at, not one second later.
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
	return sessionFromRow(row), nil
}

// DeleteSession maps zero affected rows to auth.ErrNotFound so a logout for a
// token that is already gone is distinguishable from one that worked.
func (r *authRepo) DeleteSession(ctx context.Context, tokenHash string) error {
	n, err := r.q.DeleteSession(ctx, tokenHash)
	if err != nil {
		return fmt.Errorf("sqlite: delete session: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("sqlite: delete session: %w", auth.ErrNotFound)
	}
	return nil
}

// DeleteUserSessions returns the number of sessions revoked and does NOT
// report zero as an error: bulk deletes are idempotent by design, and a
// password change for an admin who was never logged in is a success, not a
// failure.
func (r *authRepo) DeleteUserSessions(ctx context.Context, userID int64) (int64, error) {
	n, err := r.q.DeleteUserSessions(ctx, userID)
	if err != nil {
		return 0, fmt.Errorf("sqlite: delete sessions for user %d: %w", userID, err)
	}
	return n, nil
}

// DeleteExpiredSessions reaps every session at or past now, matching
// GetSession's inclusive boundary. Like DeleteUserSessions it treats zero as a
// count, not an error.
func (r *authRepo) DeleteExpiredSessions(ctx context.Context, now time.Time) (int64, error) {
	n, err := r.q.DeleteExpiredSessions(ctx, now.Unix())
	if err != nil {
		return 0, fmt.Errorf("sqlite: delete expired sessions: %w", err)
	}
	return n, nil
}
