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
	for _, bad := range []string{
		"", "has space", "a\tb", strings.Repeat("x", 65),
		// SC-30: control characters, bidi overrides, and anything outside
		// the conservative ASCII set end up in logs and the admin UI.
		"a\x00b", "a\x1b[31mb", "a\u202eb", "a\u200bb", "jos\u00e9", "a/b", "a<b>", "a\"b", "a;b",
	} {
		if err := ValidateUsername(NormalizeUsername(bad)); err == nil {
			t.Errorf("ValidateUsername(%q) should fail", bad)
		}
	}
	for _, good := range []string{
		"admin", "a", "kaylee.frye", strings.Repeat("x", 64),
		"ops-1", "first_last", "ops+sidecar@example.org", "0123456789",
	} {
		if err := ValidateUsername(NormalizeUsername(good)); err != nil {
			t.Errorf("ValidateUsername(%q): %v", good, err)
		}
	}
}

// TestLogSafeUsername pins the other half of SC-30: the failed-login log line
// records whatever the caller typed, up to the 8 KB body cap, so it is
// truncated and stripped of control characters before it is logged.
func TestLogSafeUsername(t *testing.T) {
	long := strings.Repeat("a", 8000)
	got := LogSafeUsername(long)
	if len(got) > 100 {
		t.Errorf("len(LogSafeUsername(8000 bytes)) = %d, want <= 100", len(got))
	}
	if !strings.HasPrefix(got, strings.Repeat("a", 64)) {
		t.Errorf("LogSafeUsername(long) = %q, want the first 64 characters kept", got)
	}
	if got := LogSafeUsername("  Admin\x1b[2J\r\nfake line "); strings.ContainsAny(got, "\x1b\r\n") {
		t.Errorf("LogSafeUsername kept a control character: %q", got)
	}
	if got := LogSafeUsername("  Admin "); got != "admin" {
		t.Errorf("LogSafeUsername(%q) = %q, want the normalized form", "  Admin ", got)
	}
}
