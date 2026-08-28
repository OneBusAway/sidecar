package surveys_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/surveys"
)

// testInstantParser stands in for the CLI's and the API's own region-aware
// parsers: DefinitionFromDocument only needs a parser that rejects a naive
// datetime and reports the moment in UTC, not the region-specific wording
// each real caller writes around that.
func testInstantParser(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("%q must be RFC 3339 with an explicit offset: %w", s, err)
	}
	return t.UTC(), nil
}

func minimalDoc() surveys.Document {
	return surveys.Document{Name: "Rider satisfaction"}
}

// TestDefinitionFromDocument moves cmd/sidecar-admin's former
// definitionFromDocument coverage (previously exercised only indirectly,
// through the CLI's TestSurveyCreateRejectsBadDocuments) down to the shared
// codec, now that both the CLI and POST/PUT /surveys call the same function.
func TestDefinitionFromDocument(t *testing.T) {
	t.Parallel()

	t.Run("defaults Available to true", func(t *testing.T) {
		t.Parallel()
		def, err := surveys.DefinitionFromDocument(minimalDoc(), testInstantParser)
		if err != nil {
			t.Fatal(err)
		}
		if !def.Available {
			t.Error("Available = false, want true when the document omits it")
		}
	})

	t.Run("an explicit Available false is honored", func(t *testing.T) {
		t.Parallel()
		f := false
		doc := minimalDoc()
		doc.Available = &f
		def, err := surveys.DefinitionFromDocument(doc, testInstantParser)
		if err != nil {
			t.Fatal(err)
		}
		if def.Available {
			t.Error("Available = true, want false")
		}
	})

	t.Run("dates go through the given parser", func(t *testing.T) {
		t.Parallel()
		start, end := "2026-09-01T00:00:00-07:00", "2026-09-02T00:00:00-07:00"
		doc := minimalDoc()
		doc.StartDate, doc.EndDate = &start, &end
		def, err := surveys.DefinitionFromDocument(doc, testInstantParser)
		if err != nil {
			t.Fatal(err)
		}
		if def.StartTime == nil || !def.StartTime.Equal(time.Date(2026, 9, 1, 7, 0, 0, 0, time.UTC)) {
			t.Errorf("StartTime = %v", def.StartTime)
		}
		if def.EndTime == nil || !def.EndTime.Equal(time.Date(2026, 9, 2, 7, 0, 0, 0, time.UTC)) {
			t.Errorf("EndTime = %v", def.EndTime)
		}
	})

	t.Run("half window", func(t *testing.T) {
		t.Parallel()
		start := "2026-09-01T00:00:00-07:00"
		doc := minimalDoc()
		doc.StartDate = &start
		if _, err := surveys.DefinitionFromDocument(doc, testInstantParser); err == nil ||
			!strings.Contains(err.Error(), "both start_date and end_date") {
			t.Fatalf("err = %v, want the half-window message", err)
		}
	})

	t.Run("naive start_date names the field", func(t *testing.T) {
		t.Parallel()
		naive := "2026-09-01T00:00:00"
		doc := minimalDoc()
		doc.StartDate = &naive
		if _, err := surveys.DefinitionFromDocument(doc, testInstantParser); err == nil ||
			!strings.HasPrefix(err.Error(), "start_date:") {
			t.Fatalf("err = %v, want it prefixed with %q", err, "start_date:")
		}
	})

	t.Run("naive end_date names the field", func(t *testing.T) {
		t.Parallel()
		start := "2026-09-01T00:00:00-07:00"
		naive := "2026-09-02T00:00:00"
		doc := minimalDoc()
		doc.StartDate, doc.EndDate = &start, &naive
		if _, err := surveys.DefinitionFromDocument(doc, testInstantParser); err == nil ||
			!strings.HasPrefix(err.Error(), "end_date:") {
			t.Fatalf("err = %v, want it prefixed with %q", err, "end_date:")
		}
	})

	t.Run("blank option", func(t *testing.T) {
		t.Parallel()
		doc := minimalDoc()
		doc.Questions = []surveys.QuestionDocument{
			{Content: surveys.Content{Type: "radio", LabelText: "q", Options: []string{"a", ""}}},
		}
		if _, err := surveys.DefinitionFromDocument(doc, testInstantParser); err == nil ||
			!strings.Contains(err.Error(), "blank option") {
			t.Fatalf("err = %v, want a blank-option refusal", err)
		}
	})

	t.Run("schemeless external_survey url", func(t *testing.T) {
		t.Parallel()
		doc := minimalDoc()
		doc.Questions = []surveys.QuestionDocument{
			{Content: surveys.Content{Type: "external_survey", LabelText: "q", URL: "example.org"}},
		}
		if _, err := surveys.DefinitionFromDocument(doc, testInstantParser); err == nil ||
			!strings.Contains(err.Error(), "absolute http(s)") {
			t.Fatalf("err = %v, want an absolute-http(s) refusal", err)
		}
	})

	t.Run("visible lists are trimmed", func(t *testing.T) {
		t.Parallel()
		doc := minimalDoc()
		doc.VisibleStopList = []string{" 1_570 ", "", "1_578"}
		def, err := surveys.DefinitionFromDocument(doc, testInstantParser)
		if err != nil {
			t.Fatal(err)
		}
		if len(def.VisibleStopList) != 2 || def.VisibleStopList[1] != "1_578" {
			t.Errorf("VisibleStopList = %v, want [1_570 1_578]", def.VisibleStopList)
		}
	})

	t.Run("blank name", func(t *testing.T) {
		t.Parallel()
		doc := minimalDoc()
		doc.Name = "  "
		if _, err := surveys.DefinitionFromDocument(doc, testInstantParser); err == nil ||
			!strings.Contains(err.Error(), "name cannot be blank") {
			t.Fatalf("err = %v, want the blank-name refusal", err)
		}
	})
}
