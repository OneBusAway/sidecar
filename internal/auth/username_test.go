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
