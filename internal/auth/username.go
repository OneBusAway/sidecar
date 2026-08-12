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
