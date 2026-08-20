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

	tests := []struct {
		name        string
		m           map[string]any
		key         string
		wantVal     bool
		wantPresent bool
	}{
		{
			name:        "string 1",
			m:           map[string]any{"flag": "1"},
			key:         "flag",
			wantVal:     true,
			wantPresent: true,
		},
		{
			name:        "string t",
			m:           map[string]any{"flag": "t"},
			key:         "flag",
			wantVal:     true,
			wantPresent: true,
		},
		{
			name:        "string TRUE uppercase",
			m:           map[string]any{"flag": "TRUE"},
			key:         "flag",
			wantVal:     true,
			wantPresent: true,
		},
		{
			name:        "string on",
			m:           map[string]any{"flag": "on"},
			key:         "flag",
			wantVal:     true,
			wantPresent: true,
		},
		{
			name:        "string 0",
			m:           map[string]any{"flag": "0"},
			key:         "flag",
			wantVal:     false,
			wantPresent: true,
		},
		{
			name:        "string f",
			m:           map[string]any{"flag": "f"},
			key:         "flag",
			wantVal:     false,
			wantPresent: true,
		},
		{
			name:        "string false",
			m:           map[string]any{"flag": "false"},
			key:         "flag",
			wantVal:     false,
			wantPresent: true,
		},
		{
			name:        "string off",
			m:           map[string]any{"flag": "off"},
			key:         "flag",
			wantVal:     false,
			wantPresent: true,
		},
		{
			name:        "string empty",
			m:           map[string]any{"flag": ""},
			key:         "flag",
			wantVal:     false,
			wantPresent: false,
		},
		{
			name:        "string unrecognized",
			m:           map[string]any{"flag": "yes"},
			key:         "flag",
			wantVal:     false,
			wantPresent: false,
		},
		{
			name:        "missing key",
			m:           map[string]any{},
			key:         "flag",
			wantVal:     false,
			wantPresent: false,
		},
		{
			name:        "string with trailing space",
			m:           map[string]any{"flag": "on "},
			key:         "flag",
			wantVal:     true,
			wantPresent: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := params{m: tt.m}
			val, present := p.boolish(tt.key)
			if val != tt.wantVal || present != tt.wantPresent {
				t.Errorf("boolish(%q) = (%v, %v), want (%v, %v)", tt.key, val, present, tt.wantVal, tt.wantPresent)
			}
		})
	}
}

func TestParseAPNSSandbox(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		m           map[string]any
		input       string
		wantBool    bool
		wantLogWarn bool
		logContent  string
	}{
		{
			name:        "string 1",
			m:           map[string]any{"apns_sandbox": "1"},
			wantBool:    true,
			wantLogWarn: false,
		},
		{
			name:        "string t",
			m:           map[string]any{"apns_sandbox": "t"},
			wantBool:    true,
			wantLogWarn: false,
		},
		{
			name:        "string true",
			m:           map[string]any{"apns_sandbox": "true"},
			wantBool:    true,
			wantLogWarn: false,
		},
		{
			name:        "string on",
			m:           map[string]any{"apns_sandbox": "on"},
			wantBool:    true,
			wantLogWarn: false,
		},
		{
			name:        "string 0",
			m:           map[string]any{"apns_sandbox": "0"},
			wantBool:    false,
			wantLogWarn: false,
		},
		{
			name:        "string f",
			m:           map[string]any{"apns_sandbox": "f"},
			wantBool:    false,
			wantLogWarn: false,
		},
		{
			name:        "string false",
			m:           map[string]any{"apns_sandbox": "false"},
			wantBool:    false,
			wantLogWarn: false,
		},
		{
			name:        "string off",
			m:           map[string]any{"apns_sandbox": "off"},
			wantBool:    false,
			wantLogWarn: false,
		},
		{
			name:        "missing key",
			m:           map[string]any{},
			wantBool:    false,
			wantLogWarn: false,
		},
		{
			name:        "string yes unrecognized",
			m:           map[string]any{"apns_sandbox": "yes"},
			wantBool:    false,
			wantLogWarn: true,
			logContent:  "yes",
		},
		{
			name:        "JSON boolean true",
			m:           map[string]any{"apns_sandbox": true},
			wantBool:    true,
			wantLogWarn: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&buf, nil))
			p := params{m: tt.m}

			got := parseAPNSSandbox(p, logger)
			if got != tt.wantBool {
				t.Errorf("parseAPNSSandbox() = %v, want %v", got, tt.wantBool)
			}

			logOutput := buf.String()
			if tt.wantLogWarn {
				if logOutput == "" {
					t.Errorf("parseAPNSSandbox() should have logged a warning, but got empty log")
				}
				if !strings.Contains(logOutput, "Warn") && !strings.Contains(logOutput, "WARN") {
					t.Errorf("parseAPNSSandbox() log should contain Warn level, got: %s", logOutput)
				}
				if tt.logContent != "" && !strings.Contains(logOutput, tt.logContent) {
					t.Errorf("parseAPNSSandbox() log should contain %q, got: %s", tt.logContent, logOutput)
				}
			} else if logOutput != "" && strings.Contains(logOutput, "Warn") {
				t.Errorf("parseAPNSSandbox() should not have logged a warning, but got: %s", logOutput)
			}
		})
	}
}
