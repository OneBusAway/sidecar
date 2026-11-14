package httpapi

import (
	"bytes"
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

// TestLogin_FailedLoginLogTruncatesUsername pins SC-30 at the HTTP layer:
// the username in the failed-login Warn line is whatever the caller sent, up
// to the 8 KB body cap, so it must be cut down before it is logged.
func TestLogin_FailedLoginLogTruncatesUsername(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	sleeps := &sleepRecorder{}
	h := newTestRouter(newStubAuth(), func(d *Deps) {
		d.Logger = slog.New(slog.NewTextHandler(&buf, nil))
		d.Sleep = sleeps.sleep
	})
	f := &loginFixture{handler: h}
	long := strings.Repeat("a", 7000)
	rec := f.postLogin(credentials(long, "wrong password!!"), nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body = %s", rec.Code, rec.Body.String())
	}
	logged := buf.String()
	if !strings.Contains(logged, "failed login") {
		t.Fatalf("no failed-login line logged: %q", logged)
	}
	if strings.Contains(logged, strings.Repeat("a", 65)) {
		t.Errorf("failed-login line carries more than 64 characters of the username (%d bytes logged)", len(logged))
	}
}
