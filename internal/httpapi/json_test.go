package httpapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestDecodeJSONBody_RejectsTrailingData pins SC-28: a body is exactly one
// JSON value. encoding/json's Decoder stops after the first value and leaves
// the rest unread, so `{"a":1}{"a":2}` used to decode as the first object and
// silently drop the second -- the kind of parser disagreement a proxy or WAF
// in front of the sidecar can be steered around. Trailing whitespace is still
// fine (curl and most encoders append a newline).
func TestDecodeJSONBody_RejectsTrailingData(t *testing.T) {
	t.Parallel()

	type doc struct {
		A int `json:"a"`
	}
	tests := []struct {
		name    string
		body    string
		strict  bool
		wantErr bool
		wantEOF bool
	}{
		{name: "single object", body: `{"a":1}`},
		{name: "trailing newline", body: "{\"a\":1}\n"},
		{name: "trailing whitespace", body: "{\"a\":1} \r\n\t "},
		{name: "second object", body: `{"a":1}{"a":2}`, wantErr: true},
		{name: "second object strict", body: `{"a":1}{"a":2}`, strict: true, wantErr: true},
		{name: "trailing garbage", body: `{"a":1} x`, wantErr: true},
		{name: "trailing scalar", body: `{"a":1} 2`, wantErr: true},
		{name: "empty body stays io.EOF", body: ``, wantErr: true, wantEOF: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/", strings.NewReader(tt.body))
			var got doc
			err := decodeJSONBody(httptest.NewRecorder(), req, 1024, &got, tt.strict)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantEOF && !errors.Is(err, io.EOF) {
				// The push-create handler treats io.EOF as "empty body means
				// {}"; trailing-data detection must not disturb that.
				t.Errorf("err = %v, want io.EOF", err)
			}
			if !tt.wantErr && got.A != 1 {
				t.Errorf("decoded a = %d, want 1", got.A)
			}
		})
	}
}

// TestLogin_TrailingJSONRejected walks SC-28 through a real route.
func TestLogin_TrailingJSONRejected(t *testing.T) {
	t.Parallel()

	f := newLoginFixture(t)
	rec := f.postLogin(credentials("admin", testPassword)+`{"username":"other"}`, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
	if f.repo.sessionCount() != 0 {
		t.Error("a session was created from a body with trailing data")
	}
}
