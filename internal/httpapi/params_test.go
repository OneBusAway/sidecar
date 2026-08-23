package httpapi

import (
	"bytes"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseRequestParams(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		method   string
		query    string
		body     string
		ct       string
		maxBytes int64
		want     map[string]any
		wantErr  bool
	}{
		{
			name:     "JSON body with null values",
			method:   "POST",
			body:     `{"token":"abc","n":7,"flag":true,"nil":null}`,
			ct:       "application/json",
			maxBytes: 1024,
			want: map[string]any{
				"token": "abc",
				"n":     float64(7),
				"flag":  true,
			},
		},
		{
			name:     "form body",
			method:   "POST",
			body:     "token=abc&n=7",
			ct:       "application/x-www-form-urlencoded",
			maxBytes: 1024,
			want: map[string]any{
				"token": "abc",
				"n":     "7",
			},
		},
		{
			name:     "query params merge with JSON body",
			method:   "POST",
			query:    "a=1",
			body:     `{"b":2}`,
			ct:       "application/json",
			maxBytes: 1024,
			want: map[string]any{
				"a": "1",
				"b": float64(2),
			},
		},
		{
			name:     "body wins on collision",
			method:   "POST",
			query:    "a=1",
			body:     `{"a":2}`,
			ct:       "application/json",
			maxBytes: 1024,
			want: map[string]any{
				"a": float64(2),
			},
		},
		{
			name:     "DELETE with form body",
			method:   "DELETE",
			body:     "token=abc",
			ct:       "application/x-www-form-urlencoded",
			maxBytes: 1024,
			want: map[string]any{
				"token": "abc",
			},
		},
		{
			name:     "DELETE with query and empty body no content-type",
			method:   "DELETE",
			query:    "token=abc",
			body:     "",
			ct:       "",
			maxBytes: 1024,
			want: map[string]any{
				"token": "abc",
			},
		},
		{
			name:     "DELETE with query and empty body JSON content-type",
			method:   "DELETE",
			query:    "token=abc",
			body:     "",
			ct:       "application/json",
			maxBytes: 1024,
			want: map[string]any{
				"token": "abc",
			},
		},
		{
			name:     "empty body JSON content-type",
			method:   "POST",
			body:     "",
			ct:       "application/json",
			maxBytes: 1024,
			want:     map[string]any{},
		},
		{
			name:     "body too large",
			method:   "POST",
			body:     "x=1&y=2&z=3",
			ct:       "application/x-www-form-urlencoded",
			maxBytes: 5,
			wantErr:  true,
		},
		{
			name:     "malformed JSON",
			method:   "POST",
			body:     `{"invalid":`,
			ct:       "application/json",
			maxBytes: 1024,
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(t.Context(), tt.method, "http://example.com?"+tt.query, strings.NewReader(tt.body))
			if tt.ct != "" {
				req.Header.Set("Content-Type", tt.ct)
			}
			w := httptest.NewRecorder()

			p, err := parseRequestParams(w, req, tt.maxBytes)
			if (err != nil) != tt.wantErr {
				t.Errorf("parseRequestParams error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if err != nil {
				return
			}

			if len(p.m) != len(tt.want) {
				t.Errorf("parseRequestParams got %d params, want %d", len(p.m), len(tt.want))
			}
			for k, v := range tt.want {
				got, ok := p.m[k]
				if !ok {
					t.Errorf("parseRequestParams missing key %q", k)
					continue
				}
				if got != v {
					t.Errorf("parseRequestParams[%q] = %v, want %v", k, got, v)
				}
			}
		})
	}
}

func TestParamsStr(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		m       map[string]any
		key     string
		wantVal string
		wantOk  bool
	}{
		{
			name:    "string value",
			m:       map[string]any{"token": "abc"},
			key:     "token",
			wantVal: "abc",
			wantOk:  true,
		},
		{
			name:    "string with whitespace",
			m:       map[string]any{"token": "  abc  "},
			key:     "token",
			wantVal: "abc",
			wantOk:  true,
		},
		{
			name:    "bool true",
			m:       map[string]any{"flag": true},
			key:     "flag",
			wantVal: "true",
			wantOk:  true,
		},
		{
			name:    "bool false",
			m:       map[string]any{"flag": false},
			key:     "flag",
			wantVal: "false",
			wantOk:  true,
		},
		{
			name:    "float number",
			m:       map[string]any{"n": float64(7)},
			key:     "n",
			wantVal: "7",
			wantOk:  true,
		},
		{
			name:    "float with decimal",
			m:       map[string]any{"n": float64(5.5)},
			key:     "n",
			wantVal: "5.5",
			wantOk:  true,
		},
		{
			name:    "missing key",
			m:       map[string]any{},
			key:     "missing",
			wantVal: "",
			wantOk:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := params{m: tt.m}
			val, ok := p.str(tt.key)
			if val != tt.wantVal || ok != tt.wantOk {
				t.Errorf("str(%q) = (%q, %v), want (%q, %v)", tt.key, val, ok, tt.wantVal, tt.wantOk)
			}
		})
	}
}

func TestParamsInt64(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		m       map[string]any
		key     string
		wantVal int64
		wantOk  bool
	}{
		{
			name:    "JSON integer",
			m:       map[string]any{"n": float64(7)},
			key:     "n",
			wantVal: 7,
			wantOk:  true,
		},
		{
			name:    "JSON float with no fractional part",
			m:       map[string]any{"n": float64(5.0)},
			key:     "n",
			wantVal: 5,
			wantOk:  true,
		},
		{
			name:    "JSON float with fractional part",
			m:       map[string]any{"n": float64(5.5)},
			key:     "n",
			wantVal: 0,
			wantOk:  false,
		},
		{
			name:    "string integer",
			m:       map[string]any{"n": "42"},
			key:     "n",
			wantVal: 42,
			wantOk:  true,
		},
		{
			name:    "string with whitespace",
			m:       map[string]any{"n": "  123  "},
			key:     "n",
			wantVal: 123,
			wantOk:  true,
		},
		{
			name:    "string non-numeric",
			m:       map[string]any{"n": "12x"},
			key:     "n",
			wantVal: 0,
			wantOk:  false,
		},
		{
			name:    "string date format",
			m:       map[string]any{"n": "2026-08-20"},
			key:     "n",
			wantVal: 0,
			wantOk:  false,
		},
		{
			name:    "string empty",
			m:       map[string]any{"n": ""},
			key:     "n",
			wantVal: 0,
			wantOk:  false,
		},
		{
			name:    "missing key",
			m:       map[string]any{},
			key:     "n",
			wantVal: 0,
			wantOk:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := params{m: tt.m}
			val, ok := p.int64(tt.key)
			if val != tt.wantVal || ok != tt.wantOk {
				t.Errorf("int64(%q) = (%d, %v), want (%d, %v)", tt.key, val, ok, tt.wantVal, tt.wantOk)
			}
		})
	}
}

func TestParamsBoolish(t *testing.T) {
	t.Parallel()

	// One line per case: every case keys the same map under the same name,
	// so spelling that out per case only repeated the scaffolding.
	// present=false is boolish's "I could not read a bool here" -- an empty,
	// unrecognized, or absent value -- which callers treat as "not supplied".
	const key = "flag"
	for _, tt := range []struct {
		name        string
		value       any // nil means the key is absent entirely
		wantVal     bool
		wantPresent bool
	}{
		{"string 1", "1", true, true},
		{"string t", "t", true, true},
		{"string TRUE uppercase", "TRUE", true, true},
		{"string on", "on", true, true},
		{"string with trailing space", "on ", true, true},
		{"string 0", "0", false, true},
		{"string f", "f", false, true},
		{"string false", "false", false, true},
		{"string off", "off", false, true},
		{"string empty", "", false, false},
		{"string unrecognized", "yes", false, false},
		{"missing key", nil, false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := map[string]any{}
			if tt.value != nil {
				m[key] = tt.value
			}
			val, present := params{m: m}.boolish(key)
			if val != tt.wantVal || present != tt.wantPresent {
				t.Errorf("boolish(%q) = (%v, %v), want (%v, %v)", key, val, present, tt.wantVal, tt.wantPresent)
			}
		})
	}
}

func TestParseAPNSSandbox(t *testing.T) {
	t.Parallel()

	// §2.7: anything unrecognized is production, and says so in the log --
	// misrouting a production token to the sandbox bounces in front of a
	// rider, so the parser never guesses silently.
	const key = "apns_sandbox"
	for _, tt := range []struct {
		name        string
		value       any // nil means the key is absent entirely
		wantBool    bool
		wantLogWarn bool
	}{
		{"string 1", "1", true, false},
		{"string t", "t", true, false},
		{"string true", "true", true, false},
		{"string on", "on", true, false},
		{"JSON boolean true", true, true, false},
		{"string 0", "0", false, false},
		{"string f", "f", false, false},
		{"string false", "false", false, false},
		{"string off", "off", false, false},
		{"missing key", nil, false, false},
		{"string yes unrecognized", "yes", false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := map[string]any{}
			if tt.value != nil {
				m[key] = tt.value
			}
			var buf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&buf, nil))

			if got := parseAPNSSandbox(params{m: m}, logger); got != tt.wantBool {
				t.Errorf("parseAPNSSandbox() = %v, want %v", got, tt.wantBool)
			}

			logOutput := buf.String()
			loggedWarn := strings.Contains(logOutput, "WARN") || strings.Contains(logOutput, "Warn")
			if loggedWarn != tt.wantLogWarn {
				t.Errorf("logged a warning = %v, want %v; log = %q", loggedWarn, tt.wantLogWarn, logOutput)
			}
			// The rejected value has to reach the log, or an operator cannot
			// tell which client is sending garbage.
			if tt.wantLogWarn {
				if raw, ok := tt.value.(string); ok && !strings.Contains(logOutput, raw) {
					t.Errorf("log should quote the rejected value %q, got: %s", raw, logOutput)
				}
			}
		})
	}
}
