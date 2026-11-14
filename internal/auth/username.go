package auth

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxUsernameLen is the longest username ValidateUsername accepts, in bytes
// (every accepted character is one byte).
const MaxUsernameLen = 64

// usernamePunctuation is the punctuation ValidateUsername accepts beside
// lowercase ASCII letters and digits: enough for "first.last", "ops-1" and an
// email address, nothing that means anything to a shell, a log parser, or
// HTML.
const usernamePunctuation = "._@+-"

// NormalizeUsername trims and lowercases, in Go rather than SQL collation so
// SQLite and Postgres agree (spec section 3.2).
func NormalizeUsername(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// ValidateUsername checks an already-normalized username: 1-64 characters
// drawn from lowercase ASCII letters, digits, and "._@+-". The charset is
// deliberately narrow (SC-30): a username is echoed into logs, the CLI's
// tables, and the admin UI, so control characters, bidi overrides, and
// look-alike Unicode have no business in one. It applies on create only --
// login looks a name up as typed, so an account created before this rule
// still signs in.
func ValidateUsername(s string) error {
	if s == "" {
		return errors.New("username must not be empty")
	}
	if len(s) > MaxUsernameLen {
		return fmt.Errorf("username must be at most %d characters, got %d", MaxUsernameLen, len(s))
	}
	if strings.IndexFunc(s, unicode.IsSpace) >= 0 {
		return errors.New("username must not contain whitespace")
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && !strings.ContainsRune(usernamePunctuation, r) {
			return fmt.Errorf("username may contain only lowercase letters, digits, and %q", usernamePunctuation)
		}
	}
	return nil
}

// LogSafeUsername is the form of a caller-supplied username that may be
// written to a log line (SC-30): normalized, every control or non-printing
// rune replaced with '?', and cut to MaxUsernameLen runes with a marker, so a
// failed login cannot write 8 KB of attacker text -- or a forged log line --
// per request.
func LogSafeUsername(s string) string {
	s = NormalizeUsername(s)
	var b strings.Builder
	n := 0
	for i, r := range s {
		if n == MaxUsernameLen {
			b.WriteString("...(truncated, " + fmt.Sprint(utf8.RuneCountInString(s[i:])) + " more)")
			break
		}
		if r == utf8.RuneError || !unicode.IsPrint(r) {
			r = '?'
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}
