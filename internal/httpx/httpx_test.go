package httpx

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestCapResponseBodies (SC-22): an upstream body past the cap fails the
// read with ErrResponseTooLarge instead of being buffered without bound;
// one at the cap reads whole; the caller's client is not mutated.
func TestCapResponseBodies(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, strings.Repeat("x", 100))
	}))
	defer srv.Close()

	base := srv.Client()
	transport := base.Transport
	get := func(c *http.Client) ([]byte, error) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
		if err != nil {
			return nil, err
		}
		resp, err := c.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		return io.ReadAll(resp.Body)
	}
	if b, err := get(CapResponseBodies(base, 100)); err != nil || len(b) != 100 {
		t.Fatalf("body at the cap: %d bytes, %v", len(b), err)
	}
	if _, err := get(CapResponseBodies(base, 99)); !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("body over the cap: err %v, want ErrResponseTooLarge", err)
	}
	if base.Transport != transport {
		t.Fatal("CapResponseBodies mutated the caller's client")
	}
	// A nil client and a nil transport fall back to the defaults.
	if c := CapResponseBodies(nil, 10); c.Transport == nil {
		t.Fatal("nil client: no capped transport")
	}
}
