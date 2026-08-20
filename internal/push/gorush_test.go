package push

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
			t.Fatalf("failed to read body in server: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	g := NewGorush(server.URL, server.Client(), nil)

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

func TestGorushAndroidOmitsDevelopment(t *testing.T) {
	var capturedBody []byte

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		capturedBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("failed to read body in server: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	g := NewGorush(server.URL, server.Client(), nil)

	n := Notification{
		Tokens:   []string{"tok1"},
		Platform: PlatformAndroid,
		Sandbox:  true, // garbage for Android
		Title:    "Test",
		Message:  "Test message",
	}

	err := g.Send(context.Background(), n)
	if err != nil {
		t.Fatalf("Send() returned unexpected error: %v", err)
	}

	var reqBody map[string]any
	err = json.Unmarshal(capturedBody, &reqBody)
	if err != nil {
		t.Fatalf("failed to unmarshal body: %v", err)
	}

	notifs := reqBody["notifications"].([]any)
	notif := notifs[0].(map[string]any)

	platform, ok := notif["platform"].(float64)
	if !ok || platform != 2 {
		t.Errorf("expected platform 2, got %v", notif["platform"])
	}

	_, hasDevelopment := notif["development"]
	if hasDevelopment {
		t.Error("development key should not be present for Android")
	}
}

func TestGorushProductionOmitsDevelopment(t *testing.T) {
	var capturedBody []byte

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		capturedBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("failed to read body in server: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	g := NewGorush(server.URL, server.Client(), nil)

	n := Notification{
		Tokens:   []string{"tok1"},
		Platform: PlatformIOS,
		Sandbox:  false,
		Title:    "Test",
		Message:  "Test message",
	}

	err := g.Send(context.Background(), n)
	if err != nil {
		t.Fatalf("Send() returned unexpected error: %v", err)
	}

	var reqBody map[string]any
	err = json.Unmarshal(capturedBody, &reqBody)
	if err != nil {
		t.Fatalf("failed to unmarshal body: %v", err)
	}

	notifs := reqBody["notifications"].([]any)
	notif := notifs[0].(map[string]any)

	_, hasDevelopment := notif["development"]
	if hasDevelopment {
		t.Error("development key should not be present for iOS production")
	}
}

func TestGorushNon2xxIsError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		// Response body echoes the token to test that it's NOT included in the error
		w.Write([]byte(`{"error":"invalid token tok1"}`))
	}))
	defer server.Close()

	g := NewGorush(server.URL, server.Client(), nil)

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
