package surveys_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/surveys"
)

var (
	t0 = time.Date(2026, 9, 1, 7, 0, 0, 0, time.UTC)
	t1 = t0.Add(24 * time.Hour)
)

func validDefinition() surveys.Definition {
	return surveys.Definition{
		Name: "Rider satisfaction", Available: true,
		Questions: []surveys.QuestionDefinition{
			{Required: true, Content: surveys.Content{Type: "radio", LabelText: "Trip?", Options: []string{"Good", "Bad"}}},
			{Required: true, Content: surveys.Content{Type: "label", LabelText: "Thanks"}},
		},
	}
}

func TestDefinitionValidate(t *testing.T) {
	t.Parallel()
	t.Run("valid normalizes", func(t *testing.T) {
		t.Parallel()
		d := validDefinition()
		d.VisibleStopList = []string{" 1_570 ", "", "1_578"}
		d.VisibleRouteList = []string{}
		d.Questions[1].Content.SDKConfigurationValues = nil
		if err := d.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
		if !reflect.DeepEqual(d.VisibleStopList, []string{"1_570", "1_578"}) {
			t.Errorf("VisibleStopList = %v, want trimmed without blanks", d.VisibleStopList)
		}
		if d.VisibleRouteList != nil {
			t.Errorf("VisibleRouteList = %v, want nil for empty", d.VisibleRouteList)
		}
		if d.Questions[1].Required {
			t.Error("label question still Required; Validate must force false")
		}
	})
	t.Run("sdk values compacted", func(t *testing.T) {
		t.Parallel()
		d := validDefinition()
		d.Questions = []surveys.QuestionDefinition{{Content: surveys.Content{Type: "external_survey", LabelText: "Go",
			URL: "https://e.org", SDKConfigurationValues: json.RawMessage("{ \"a\" : 1 }")}}}
		if err := d.Validate(); err != nil {
			t.Fatal(err)
		}
		if got := string(d.Questions[0].Content.SDKConfigurationValues); got != `{"a":1}` {
			t.Errorf("sdk values = %s, want compacted", got)
		}
	})
	t.Run("sdk values match stored escaping", func(t *testing.T) {
		t.Parallel()
		// encoding/json HTML-escapes <, > and & when the row is stored;
		// the document must canonicalize the same way or an unchanged
		// question reads as changed on the next edit.
		c := surveys.Content{Type: "external_survey", LabelText: "Go", URL: "https://e.org",
			SDKConfigurationValues: json.RawMessage(`{"ref":"a&b<c>"}`)}
		stored, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		var back surveys.Content
		if err := json.Unmarshal(stored, &back); err != nil {
			t.Fatal(err)
		}
		d := validDefinition()
		d.Questions = []surveys.QuestionDefinition{{Content: c}}
		if err := d.Validate(); err != nil {
			t.Fatal(err)
		}
		if !surveys.QuestionsEqual([]surveys.Question{{Content: back}}, d.Questions) {
			t.Errorf("document %s != stored %s", d.Questions[0].Content.SDKConfigurationValues, back.SDKConfigurationValues)
		}
	})
	t.Run("sdk values null is absent", func(t *testing.T) {
		t.Parallel()
		for _, typ := range []string{"text", "external_survey"} {
			d := validDefinition()
			d.Questions = []surveys.QuestionDefinition{{Content: surveys.Content{Type: typ, LabelText: "Go",
				URL: "https://e.org", SDKConfigurationValues: json.RawMessage(" null ")}}}
			if typ == "text" {
				d.Questions[0].Content.URL = ""
			}
			if err := d.Validate(); err != nil {
				t.Fatalf("%s: Validate: %v", typ, err)
			}
			if d.Questions[0].Content.SDKConfigurationValues != nil {
				t.Errorf("%s: sdk values = %q, want nil", typ, d.Questions[0].Content.SDKConfigurationValues)
			}
		}
	})
	t.Run("sdk values invalid json", func(t *testing.T) {
		t.Parallel()
		d := validDefinition()
		d.Questions = []surveys.QuestionDefinition{{Content: surveys.Content{Type: "external_survey", LabelText: "Go",
			URL: "https://e.org", SDKConfigurationValues: json.RawMessage(`{"a":`)}}}
		err := d.Validate()
		if err == nil || !strings.Contains(err.Error(), "question 1: sdk_configuration_values") {
			t.Fatalf("Validate() = %v, want sdk_configuration_values parse error", err)
		}
	})
	tests := []struct {
		name    string
		mutate  func(*surveys.Definition)
		wantErr string
	}{
		{"blank name", func(d *surveys.Definition) { d.Name = " " }, "name cannot be blank"},
		{"start only", func(d *surveys.Definition) { d.StartTime = &t0 }, "both start_date and end_date"},
		{"end only", func(d *surveys.Definition) { d.EndTime = &t1 }, "both start_date and end_date"},
		{"end before start", func(d *surveys.Definition) { d.StartTime = &t1; d.EndTime = &t0 }, "end_date must be after"},
		{"end equals start", func(d *surveys.Definition) { d.StartTime = &t0; d.EndTime = &t0 }, "end_date must be after"},
		{"bad question", func(d *surveys.Definition) { d.Questions[0].Content.Options = nil }, "question 1: radio question needs"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d := validDefinition()
			tt.mutate(&d)
			err := d.Validate()
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
	t.Run("no questions is valid", func(t *testing.T) {
		t.Parallel()
		d := validDefinition()
		d.Questions = nil
		if err := d.Validate(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("window ok", func(t *testing.T) {
		t.Parallel()
		d := validDefinition()
		d.StartTime, d.EndTime = &t0, &t1
		if err := d.Validate(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestQuestionsEqual(t *testing.T) {
	t.Parallel()
	stored := []surveys.Question{
		{ID: 10, Position: 1, Required: true, Content: surveys.Content{Type: "radio", LabelText: "Trip?", Options: []string{"Good", "Bad"}}},
		{ID: 11, Position: 2, Content: surveys.Content{Type: "text", LabelText: "More?"}},
	}
	same := []surveys.QuestionDefinition{
		{Required: true, Content: surveys.Content{Type: "radio", LabelText: "Trip?", Options: []string{"Good", "Bad"}}},
		{Content: surveys.Content{Type: "text", LabelText: "More?"}},
	}
	if !surveys.QuestionsEqual(stored, same) {
		t.Fatal("identical questions reported unequal")
	}
	reordered := []surveys.QuestionDefinition{same[1], same[0]}
	if surveys.QuestionsEqual(stored, reordered) {
		t.Fatal("reordered questions reported equal")
	}
	requiredFlipped := []surveys.QuestionDefinition{{Required: false, Content: same[0].Content}, same[1]}
	if surveys.QuestionsEqual(stored, requiredFlipped) {
		t.Fatal("required flip reported equal")
	}
	optionChanged := []surveys.QuestionDefinition{{Required: true, Content: surveys.Content{Type: "radio", LabelText: "Trip?", Options: []string{"Good", "Meh"}}}, same[1]}
	if surveys.QuestionsEqual(stored, optionChanged) {
		t.Fatal("option change reported equal")
	}
	if surveys.QuestionsEqual(stored, same[:1]) {
		t.Fatal("shorter list reported equal")
	}
}

func TestFormatTime(t *testing.T) {
	t.Parallel()
	pst := time.FixedZone("PST", -8*3600)
	in := time.Date(2026, 6, 12, 16, 36, 45, 587_000_000, pst)
	if got := surveys.FormatTime(in); got != "2026-06-13T00:36:45.587Z" {
		t.Fatalf("FormatTime = %q", got)
	}
	if got := surveys.FormatTime(time.Unix(1780019200, 0)); got != "2026-05-29T01:46:40.000Z" {
		t.Fatalf("FormatTime(whole seconds) = %q, want three zero fraction digits", got)
	}
}

func TestNormalizeList(t *testing.T) {
	t.Parallel()
	if got := surveys.NormalizeList(nil); got != nil {
		t.Fatalf("nil -> %v", got)
	}
	if got := surveys.NormalizeList([]string{"", "  "}); got != nil {
		t.Fatalf("blanks -> %v, want nil", got)
	}
	if got := surveys.NormalizeList([]string{" a ", "b"}); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("got %v", got)
	}
}
