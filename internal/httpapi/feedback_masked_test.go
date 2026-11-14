package httpapi_test

import (
	"bytes"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/httpapi"
	"github.com/OneBusAway/sidecar/internal/store/sqlitetest"
)

// TestFeedbackMaskedTokenWarnsOnce (SC-06): with gorush's default
// log.hide_token the feedback token arrives with '*' in place of its first
// and last ten characters and can never match a stored token, so dead-token
// pruning silently never happens. The webhook says so once, loudly, and
// does not try to delete by the masked value.
func TestFeedbackMaskedTokenWarnsOnce(t *testing.T) {
	t.Parallel()
	store := sqlitetest.Open(t)
	putRegion(t, store.Regions(), 1)
	repo := &fakePushRepo{real: store.PushRegs()}
	var logs bytes.Buffer
	h := httpapi.NewRouter(httpapi.Deps{
		PushRegs: repo,
		Regions:  store.Regions(),
		Now:      func() time.Time { return base },
		Logger:   slog.New(slog.NewTextHandler(&logs, nil)),
	})
	body := `{"type":"failed-push","platform":"ios","token":"**********4bc779f968465506ef**********","error":"Unregistered"}`
	for range 2 {
		if rec := feedbackRequest(t, h, body); rec.Code != http.StatusOK {
			t.Fatalf("status %d", rec.Code)
		}
	}
	if repo.deleteByTokenCalled {
		t.Error("deleted by a masked token")
	}
	if n := strings.Count(logs.String(), "GORUSH_LOG_HIDE_TOKEN"); n != 1 {
		t.Fatalf("masked-token warnings = %d, want 1; logs:\n%s", n, logs.String())
	}
	if !strings.Contains(logs.String(), "level=ERROR") {
		t.Fatalf("warning is not loud: %s", logs.String())
	}
}
