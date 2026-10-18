package surveys_test

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/OneBusAway/sidecar/internal/surveys"
)

func TestContentValidate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		c       surveys.Content
		wantErr string // substring; "" = valid
	}{
		{"text ok", surveys.Content{Type: "text", LabelText: "Why?"}, ""},
		{"label ok", surveys.Content{Type: "label", LabelText: "Thanks"}, ""},
		{"radio ok", surveys.Content{Type: "radio", LabelText: "Trip?", Options: []string{"Good", "Bad"}}, ""},
		{"checkbox ok", surveys.Content{Type: "checkbox", LabelText: "Modes", Options: []string{"Bus"}}, ""},
		{"external ok", surveys.Content{Type: "external_survey", LabelText: "Go", URL: "https://example.org/s?x=1",
			SurveyProvider: "qualtrics", EmbeddedDataFields: []string{"user_id"},
			SDKConfigurationValues: json.RawMessage(`{"k":"v"}`)}, ""},
		{"unknown type", surveys.Content{Type: "slider", LabelText: "x"}, "not one of"},
		{"blank label", surveys.Content{Type: "text", LabelText: "  "}, "label_text"},
		{"radio no options", surveys.Content{Type: "radio", LabelText: "x"}, "at least one option"},
		{"radio blank option", surveys.Content{Type: "radio", LabelText: "x", Options: []string{"a", " "}}, "blank option"},
		{"text with options", surveys.Content{Type: "text", LabelText: "x", Options: []string{"a"}}, "cannot have options"},
		{"external no scheme", surveys.Content{Type: "external_survey", LabelText: "x", URL: "example.org/s"}, "absolute http(s)"},
		{"external ftp", surveys.Content{Type: "external_survey", LabelText: "x", URL: "ftp://example.org/s"}, "absolute http(s)"},
		{"external no host", surveys.Content{Type: "external_survey", LabelText: "x", URL: "https:///path"}, "absolute http(s)"},
		{"external bad sdk", surveys.Content{Type: "external_survey", LabelText: "x", URL: "https://e.org",
			SDKConfigurationValues: json.RawMessage(`[1]`)}, "JSON object"},
		{"external null sdk", surveys.Content{Type: "external_survey", LabelText: "x", URL: "https://e.org",
			SDKConfigurationValues: json.RawMessage(`null`)}, "JSON object"},
		{"text with url", surveys.Content{Type: "text", LabelText: "x", URL: "https://e.org"}, "cannot have url"},
		{"external learn_more", surveys.Content{Type: "external_survey", LabelText: "x", URL: "https://e.org", ButtonLabel: "learn_more"}, ""},
		{"external share_feedback", surveys.Content{Type: "external_survey", LabelText: "x", URL: "https://e.org", ButtonLabel: "share_feedback"}, ""},
		{"external sign_up", surveys.Content{Type: "external_survey", LabelText: "x", URL: "https://e.org", ButtonLabel: "sign_up"}, ""},
		{"external explicit default", surveys.Content{Type: "external_survey", LabelText: "x", URL: "https://e.org", ButtonLabel: "open_survey"}, ""},
		{"external unknown button_label", surveys.Content{Type: "external_survey", LabelText: "x", URL: "https://e.org", ButtonLabel: "Click Me"}, "button_label"},
		{"text with button_label", surveys.Content{Type: "text", LabelText: "x", ButtonLabel: "learn_more"}, "button_label"},
		{"label with button_label", surveys.Content{Type: "label", LabelText: "x", ButtonLabel: "sign_up"}, "button_label"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := tt.c.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

// keysOf decodes JSON and returns its sorted top-level keys: the test pins
// the key SET per type, never byte order.
func keysOf(t *testing.T, b []byte) []string {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal %s: %v", b, err)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func TestContentMarshalJSONPerTypeKeys(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		c    surveys.Content
		want []string
	}{
		{"text", surveys.Content{Type: "text", LabelText: "x"}, []string{"label_text", "type"}},
		{"label", surveys.Content{Type: "label", LabelText: "x"}, []string{"label_text", "type"}},
		{"radio", surveys.Content{Type: "radio", LabelText: "x", Options: []string{"a"}}, []string{"label_text", "options", "type"}},
		{"checkbox", surveys.Content{Type: "checkbox", LabelText: "x", Options: []string{"a"}}, []string{"label_text", "options", "type"}},
		{"external with sdk", surveys.Content{Type: "external_survey", LabelText: "x", URL: "https://e.org",
			SDKConfigurationValues: json.RawMessage(`{"a":1}`)},
			[]string{"embedded_data_fields", "label_text", "sdk_configuration_values", "survey_provider", "type", "url"}},
		{"external without sdk", surveys.Content{Type: "external_survey", LabelText: "x", URL: "https://e.org"},
			[]string{"embedded_data_fields", "label_text", "survey_provider", "type", "url"}},
		// The default label is never emitted, spelled out or not, so a
		// survey that does not customize its button stays byte-identical.
		{"external explicit default button", surveys.Content{Type: "external_survey", LabelText: "x", URL: "https://e.org", ButtonLabel: "open_survey"},
			[]string{"embedded_data_fields", "label_text", "survey_provider", "type", "url"}},
		{"external custom button", surveys.Content{Type: "external_survey", LabelText: "x", URL: "https://e.org", ButtonLabel: "learn_more"},
			[]string{"button_label", "embedded_data_fields", "label_text", "survey_provider", "type", "url"}},
		{"text never emits button", surveys.Content{Type: "text", LabelText: "x", ButtonLabel: "learn_more"}, []string{"label_text", "type"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b, err := json.Marshal(tt.c)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if got := keysOf(t, b); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("keys = %v, want %v (json: %s)", got, tt.want, b)
			}
		})
	}
}

// A nil EmbeddedDataFields must serialize as [] (the reference emits an
// array; iOS decodes [String]?, Android ArrayList<String>?), never null.
func TestContentMarshalJSONEmptyListsAreArrays(t *testing.T) {
	t.Parallel()
	b, err := json.Marshal(surveys.Content{Type: "external_survey", LabelText: "x", URL: "https://e.org"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"embedded_data_fields":[]`) {
		t.Fatalf("json = %s, want embedded_data_fields:[]", b)
	}
}

func TestContentJSONRoundTrip(t *testing.T) {
	t.Parallel()
	in := surveys.Content{Type: "external_survey", LabelText: "x", URL: "https://e.org", ButtonLabel: "sign_up",
		SurveyProvider: "p", EmbeddedDataFields: []string{"user_id"}, SDKConfigurationValues: json.RawMessage(`{"a":1}`)}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out surveys.Content
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out.Type != in.Type || out.LabelText != in.LabelText || out.URL != in.URL ||
		out.SurveyProvider != in.SurveyProvider || out.ButtonLabel != in.ButtonLabel || !reflect.DeepEqual(out.EmbeddedDataFields, in.EmbeddedDataFields) ||
		string(out.SDKConfigurationValues) != `{"a":1}` {
		t.Fatalf("round trip = %+v, want %+v", out, in)
	}
}

// Spelling out the default must not read as an edit to a frozen survey:
// it is the same button as leaving the key out.
func TestContentEqualTreatsDefaultButtonLabelAsAbsent(t *testing.T) {
	t.Parallel()
	a := surveys.Content{Type: "external_survey", LabelText: "x", URL: "https://e.org"}
	b := a
	b.ButtonLabel = "open_survey"
	if !surveys.ContentEqual(a, b) {
		t.Fatal("ContentEqual(absent, open_survey) = false, want true")
	}
	b.ButtonLabel = "learn_more"
	if surveys.ContentEqual(a, b) {
		t.Fatal("ContentEqual(absent, learn_more) = true, want false")
	}
}
