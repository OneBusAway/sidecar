package push

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestIsMaskedToken (SC-06): gorush's log.hide_token (default true)
// replaces the first and last ten characters of a token with '*' in its
// logs -- which are what it returns inline and posts to the feedback hook.
// No APNs (hex) or FCM token contains '*'.
func TestIsMaskedToken(t *testing.T) {
	t.Parallel()
	for tok, want := range map[string]bool{
		"**********d4bc779f968465506ef8294be71**********": true,
		"****":                     true,
		"80651d4bc779f968465506ef": false,
		"fcm:APA91bH-abc_def":      false,
		"":                         false,
	} {
		if got := IsMaskedToken(tok); got != want {
			t.Errorf("IsMaskedToken(%q) = %v, want %v", tok, got, want)
		}
	}
}

// TestGorushSendBatchWarnsOnceOnMaskedTokens (SC-06): a masked token in
// gorush's inline logs means hide_token is on and no rejection can ever be
// matched to a device, so SendBatch says so -- loudly, once per sender.
func TestGorushSendBatchWarnsOnceOnMaskedTokens(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"logs":[{"type":"failed-push","platform":"ios","token":"**********4bc779f96**********","error":"BadDeviceToken"}]}`))
	}))
	defer server.Close()
	var logs bytes.Buffer
	g := NewGorush(server.URL, "org.example.app", server.Client())
	g.SetLogger(slog.New(slog.NewTextHandler(&logs, nil)))
	for range 2 {
		if _, err := g.SendBatch(context.Background(), Notification{Tokens: []string{"t"}, Platform: PlatformIOS, Message: "m"}, "x"); err != nil {
			t.Fatal(err)
		}
	}
	if n := strings.Count(logs.String(), "GORUSH_LOG_HIDE_TOKEN"); n != 1 {
		t.Fatalf("masked-token warnings = %d, want exactly 1; logs:\n%s", n, logs.String())
	}
	if !strings.Contains(logs.String(), "level=ERROR") {
		t.Fatalf("warning is not loud: %s", logs.String())
	}
}
