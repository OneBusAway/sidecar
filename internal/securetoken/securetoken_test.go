package securetoken_test

import (
	"regexp"
	"testing"

	"github.com/OneBusAway/sidecar/internal/securetoken"
)

func TestNew(t *testing.T) {
	t.Parallel()

	urlSafe := regexp.MustCompile(`^[A-Za-z0-9_-]{22}$`)
	seen := make(map[string]bool)
	for range 1000 {
		tok, err := securetoken.New()
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if !urlSafe.MatchString(tok) {
			t.Fatalf("New() = %q; want 22 URL-safe base64 chars", tok)
		}
		if seen[tok] {
			t.Fatalf("New() repeated token %q", tok)
		}
		seen[tok] = true
	}
}
