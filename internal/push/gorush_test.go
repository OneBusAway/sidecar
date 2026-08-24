package push

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestGorushSendPostsExpectedJSON(t *testing.T) {
	var (
		capturedMethod string
		capturedPath   string
		capturedBody   []byte
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedMethod = r.Method
		capturedPath = r.URL.Path
		var err error
		capturedBody, err = io.ReadAll(r.Body)
		if err != nil {
			// t.Fatalf would stop only this handler goroutine, leaving the
			// request without a response and the client blocked until its
			// timeout; the test would keep running on stale captures.
			t.Errorf("failed to read body in server: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	g := NewGorush(server.URL, "", server.Client())

	n := Notification{
		Tokens:   []string{"tok1"},
		Platform: PlatformIOS,
		Sandbox:  true,
		Title:    "OneBusAway",
		Message:  "The 44 to Ballard leaves in 10 minutes",
		Data: map[string]any{
			"arrival_and_departure": map[string]any{
				"region_id": 1,
			},
		},
	}

	err := g.Send(context.Background(), n)
	if err != nil {
		t.Fatalf("Send() returned unexpected error: %v", err)
	}

	if capturedMethod != http.MethodPost {
		t.Errorf("expected method POST, got %s", capturedMethod)
	}
	if capturedPath != "/api/push" {
		t.Errorf("expected path /api/push, got %s", capturedPath)
	}

	var reqBody map[string]any
	err = json.Unmarshal(capturedBody, &reqBody)
	if err != nil {
		t.Fatalf("failed to unmarshal body: %v", err)
	}

	notifs, ok := reqBody["notifications"].([]any)
	if !ok {
		t.Fatal("notifications is not an array")
	}
	if len(notifs) != 1 {
		t.Errorf("expected 1 notification, got %d", len(notifs))
	}

	notif, ok := notifs[0].(map[string]any)
	if !ok {
		t.Fatal("notification is not a map")
	}

	// Check tokens
	tokensAny := notif["tokens"]
	tokensSlice, ok := tokensAny.([]any)
	if !ok {
		t.Fatal("tokens is not a slice")
	}
	if len(tokensSlice) != 1 || tokensSlice[0] != "tok1" {
		t.Errorf("expected tokens [tok1], got %v", tokensSlice)
	}

	// Check platform
	platform, ok := notif["platform"].(float64)
	if !ok || platform != 1 {
		t.Errorf("expected platform 1, got %v", notif["platform"])
	}

	// Check title
	if notif["title"] != "OneBusAway" {
		t.Errorf("expected title OneBusAway, got %v", notif["title"])
	}

	// Check message
	if notif["message"] != "The 44 to Ballard leaves in 10 minutes" {
		t.Errorf("expected message, got %v", notif["message"])
	}

	// Check priority
	if notif["priority"] != "high" {
		t.Errorf("expected priority high, got %v", notif["priority"])
	}

	// Check development
	if notif["development"] != true {
		t.Errorf("expected development true, got %v", notif["development"])
	}

	// Check data
	data, ok := notif["data"].(map[string]any)
	if !ok {
		t.Fatal("data is not a map")
	}
	arrivalData, ok := data["arrival_and_departure"].(map[string]any)
	if !ok {
		t.Fatal("arrival_and_departure is not a map")
	}
	regionID, ok := arrivalData["region_id"].(float64)
	if !ok || regionID != 1 {
		t.Errorf("expected region_id 1, got %v", arrivalData["region_id"])
	}
}

// TestGorushOmitsDevelopment covers when the sandbox flag must not appear on
// the wire at all. `development` is omitempty, so a false value is absent --
// but the two cases get there differently: Android never sets it (the field
// is meaningless off APNs), while an iOS production send sets it to false.
// Both must produce a payload with no `development` key.
func TestGorushOmitsDevelopment(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		platform     Platform
		sandbox      bool
		wantPlatform float64
	}{
		// Sandbox: true is garbage for Android -- the flag is APNs-only. It
		// is set deliberately: with false, a sender that wrongly forwarded
		// it would still produce an absent (omitempty) key and look correct.
		{"android never carries it", PlatformAndroid, true, 2},
		{"ios production sends false", PlatformIOS, false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var capturedBody []byte
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var err error
				capturedBody, err = io.ReadAll(r.Body)
				if err != nil {
					// t.Fatalf would stop only this handler goroutine,
					// leaving the request unanswered and the client blocked.
					t.Errorf("failed to read body in server: %v", err)
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()

			g := NewGorush(server.URL, "", server.Client())
			if err := g.Send(context.Background(), Notification{
				Tokens: []string{"tok1"}, Platform: tc.platform, Sandbox: tc.sandbox,
				Title: "Test", Message: "Test message",
			}); err != nil {
				t.Fatalf("Send() returned unexpected error: %v", err)
			}

			var reqBody map[string]any
			if err := json.Unmarshal(capturedBody, &reqBody); err != nil {
				t.Fatalf("failed to unmarshal body: %v", err)
			}
			notif := reqBody["notifications"].([]any)[0].(map[string]any)

			if platform, ok := notif["platform"].(float64); !ok || platform != tc.wantPlatform {
				t.Errorf("platform = %v, want %v", notif["platform"], tc.wantPlatform)
			}
			if _, hasDevelopment := notif["development"]; hasDevelopment {
				t.Errorf("development key present, want it omitted; payload = %s", capturedBody)
			}
		})
	}
}

func TestGorushNon2xxIsError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		// Response body echoes the token to test that it's NOT included in the error
		w.Write([]byte(`{"error":"invalid token tok1"}`))
	}))
	defer server.Close()

	g := NewGorush(server.URL, "", server.Client())

	n := Notification{
		Tokens:   []string{"tok1"},
		Platform: PlatformAndroid,
		Title:    "Test",
		Message:  "Test message",
	}

	err := g.Send(context.Background(), n)
	if err == nil {
		t.Fatal("Send() should have returned an error")
	}

	errMsg := err.Error()
	if !strings.Contains(errMsg, "400") {
		t.Errorf("error message should contain '400', got: %s", errMsg)
	}
	if strings.Contains(errMsg, "tok1") {
		t.Errorf("error message should not contain token 'tok1', got: %s", errMsg)
	}
}

// TestGorushSendTimesOutOnHungServer asserts Send returns an error rather
// than blocking forever when gorush accepts the connection but never
// responds -- the failure mode NewGorush's timeout exists to bound (a hung
// send otherwise exhausts the scheduler's errgroup limit permanently).
// The client's Timeout (50ms) is set explicitly, well under the handler's
// 500ms sleep, so the test itself stays fast.
func TestGorushSendTimesOutOnHungServer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := *server.Client()
	client.Timeout = 50 * time.Millisecond
	g := NewGorush(server.URL, "", &client)

	n := Notification{
		Tokens:   []string{"tok1"},
		Platform: PlatformAndroid,
		Title:    "Test",
		Message:  "Test message",
	}

	start := time.Now()
	err := g.Send(context.Background(), n)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Send() should have returned an error on timeout")
	}
	if elapsed > 400*time.Millisecond {
		t.Errorf("Send() took %v, want well under the handler's 500ms sleep (timeout not applied?)", elapsed)
	}
}

func TestIsTerminal(t *testing.T) {
	tests := []struct {
		reason   string
		expected bool
	}{
		{"Unregistered", true},
		{"apns: BadDeviceToken", true},
		{"DeviceTokenNotForTopic", true},
		{"ExpiredProviderToken", false},
		{"InternalServerError", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.reason, func(t *testing.T) {
			result := IsTerminal(tt.reason)
			if result != tt.expected {
				t.Errorf("IsTerminal(%q) = %v, want %v", tt.reason, result, tt.expected)
			}
		})
	}
}

// TestNewGorushAppliesDefaultTimeout pins the constructor's timeout
// contract. TestGorushSendTimesOutOnHungServer above cannot: it sets the
// client's Timeout to 50ms explicitly so the test stays fast, which means it
// proves http.Client honors a Timeout -- a stdlib property -- not that
// NewGorush supplies gorushTimeout when the caller's client has none. That
// is the case cmd/sidecar actually hits (it passes http.DefaultClient), and
// deleting the default left the whole suite green.
func TestNewGorushAppliesDefaultTimeout(t *testing.T) {
	t.Parallel()

	if g := NewGorush("http://gorush.test", "", &http.Client{}); g.http.Timeout != gorushTimeout {
		t.Errorf("client without a Timeout: got %v, want the %v default", g.http.Timeout, gorushTimeout)
	}
	if g := NewGorush("http://gorush.test", "", nil); g.http.Timeout != gorushTimeout {
		t.Errorf("nil client: got %v, want the %v default", g.http.Timeout, gorushTimeout)
	}
	if http.DefaultClient.Timeout != 0 {
		t.Errorf("http.DefaultClient.Timeout = %v, want 0; the shared client must never be mutated",
			http.DefaultClient.Timeout)
	}

	// A caller who set their own keeps it, and their client is copied, not
	// modified in place.
	custom := &http.Client{Timeout: 3 * time.Second}
	if g := NewGorush("http://gorush.test", "", custom); g.http.Timeout != 3*time.Second {
		t.Errorf("caller Timeout = %v, want it preserved at 3s", g.http.Timeout)
	}
	if custom.Timeout != 3*time.Second {
		t.Errorf("caller's client was mutated: Timeout = %v, want 3s", custom.Timeout)
	}
}

// TestNewGorushDefaultsScheme pins that a bare host:port -- what Render's
// fromService hostport property yields -- is usable verbatim, while an
// explicit scheme is left alone.
func TestNewGorushDefaultsScheme(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"gorush:8088":        "http://gorush:8088/api/push",
		"http://gorush:8088": "http://gorush:8088/api/push",
		"https://g.example":  "https://g.example/api/push",
		"https://g.example/": "https://g.example/api/push",
	}
	for base, want := range cases {
		if got := NewGorush(base, "", nil).pushURL; got != want {
			t.Errorf("NewGorush(%q).pushURL = %q, want %q", base, got, want)
		}
	}
}

// TestNewGorushTrimsTrailingSlash keeps a trailing slash in an
// operator-supplied SIDECAR_GORUSH_URL from producing a double-slashed
// //api/push.
func TestNewGorushTrimsTrailingSlash(t *testing.T) {
	t.Parallel()

	const want = "http://gorush.test/api/push"
	for _, base := range []string{"http://gorush.test", "http://gorush.test/", "http://gorush.test///"} {
		if got := NewGorush(base, "", nil).pushURL; got != want {
			t.Errorf("NewGorush(%q).pushURL = %q, want %q", base, got, want)
		}
	}
}

// TestGorushSendSetsAPNsTopicForIOSOnly pins the one field APNs token auth
// cannot live without: under .p8 auth Apple requires apns-topic on every
// request and gorush passes it through verbatim from the notification's
// "topic". Android has no such concept, so the field must be absent there
// rather than sent as an empty string.
func TestGorushSendSetsAPNsTopicForIOSOnly(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		platform  Platform
		wantTopic string
		wantSet   bool
	}{
		{"ios carries topic", PlatformIOS, "org.onebusaway.iphone", true},
		{"android omits topic", PlatformAndroid, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var captured map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Errorf("read body: %v", err)
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				var req map[string]any
				if err := json.Unmarshal(body, &req); err != nil {
					t.Errorf("unmarshal body: %v", err)
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				captured = req["notifications"].([]any)[0].(map[string]any)
				w.WriteHeader(http.StatusOK)
			}))
			defer server.Close()

			g := NewGorush(server.URL, "org.onebusaway.iphone", server.Client())
			err := g.Send(context.Background(), Notification{
				Tokens: []string{"tok"}, Platform: tc.platform, Message: "hi",
			})
			if err != nil {
				t.Fatalf("Send: %v", err)
			}
			got, set := captured["topic"]
			if set != tc.wantSet {
				t.Fatalf("topic present = %v, want %v (payload %v)", set, tc.wantSet, captured)
			}
			if set && got != tc.wantTopic {
				t.Errorf("topic = %v, want %q", got, tc.wantTopic)
			}
		})
	}
}
