package httpapi_test

import (
	"log/slog"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/httpapi"
)

// TestNewServer_Timeouts pins the four explicit timeouts NewServer sets.
// These are the only process-level defence for an endpoint that is
// unauthenticated by design (Go's http.Server defaults every one of these
// to zero), so a regression here silently restores a slowloris exposure
// without changing anything about the feed's observable behavior -- nothing
// else would catch it.
func TestNewServer_Timeouts(t *testing.T) {
	t.Parallel()

	srv := httpapi.NewServer(httpapi.ServerConfig{
		Addr: ":0",
		Deps: httpapi.Deps{
			Now:    func() time.Time { return base },
			Logger: slog.New(slog.DiscardHandler),
		},
	})

	if got, want := srv.ReadHeaderTimeout, 5*time.Second; got != want {
		t.Errorf("ReadHeaderTimeout = %v, want %v", got, want)
	}
	if got, want := srv.ReadTimeout, 10*time.Second; got != want {
		t.Errorf("ReadTimeout = %v, want %v", got, want)
	}
	if got, want := srv.WriteTimeout, 15*time.Second; got != want {
		t.Errorf("WriteTimeout = %v, want %v", got, want)
	}
	if got, want := srv.IdleTimeout, 60*time.Second; got != want {
		t.Errorf("IdleTimeout = %v, want %v", got, want)
	}
}
