package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestHealthz pins the liveness route Render's health check and compose's
// smoke script depend on. It must not touch the database or any
// per-feature dependency: a fresh deploy has no regions yet, and the health
// check has to pass before anyone can ssh in to create them.
func TestHealthz(t *testing.T) {
	t.Parallel()
	h := NewRouter(Deps{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != "ok\n" {
		t.Errorf("body = %q, want %q", got, "ok\n")
	}
}
