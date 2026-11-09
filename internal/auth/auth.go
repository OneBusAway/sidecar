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

// ErrLastUser is returned by DeleteUserUnlessLast when the named account is
// the only one left: deleting it would leave the admin UI unreachable until
// the next `sidecar-admin user create`.
var ErrLastUser = errors.New("last remaining admin account")

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
	// DeleteUserUnlessLast deletes the account unless it is the only one
	// left, in which case it returns ErrLastUser and deletes nothing. An
	// unknown username is ErrNotFound, never ErrLastUser. The check and the
	// delete must be atomic against concurrent calls: two callers deleting
	// different accounts from a two-account table must not both succeed.
	// On SQLite one immediate transaction gives this; an engine with weaker
	// default isolation must lock the rows it counts.
	DeleteUserUnlessLast(ctx context.Context, username string) error
	// UpdatePassword replaces the account's password hash and revokes every
	// session the account holds, atomically: either both happen or neither
	// does, so a failed password change never leaves old-password sessions
	// alive next to the new hash. An unknown username is ErrNotFound.
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
	// DeleteUserSessions revokes every session for a user. A password change
	// does not need it: UpdatePassword revokes the sessions itself.
	DeleteUserSessions(ctx context.Context, userID int64) (int64, error)
	DeleteExpiredSessions(ctx context.Context, now time.Time) (int64, error)
}
