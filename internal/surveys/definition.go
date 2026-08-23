package surveys

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// wireTimeLayout is what the reference emits and the iOS fixture decodes:
// RFC 3339 in UTC with exactly three fractional digits (design spec §2.3).
const wireTimeLayout = "2006-01-02T15:04:05.000Z"

// FormatTime renders t for the wire.
func FormatTime(t time.Time) string { return t.UTC().Format(wireTimeLayout) }

// ValidateWindow enforces "both or neither" and end > start. A half-set
// window would be "always active" under the reference's predicate but is
// never a thing an author means (design spec §2.4).
func ValidateWindow(start, end *time.Time) error {
	if (start == nil) != (end == nil) {
		return errors.New("both start_date and end_date must be set, or neither")
	}
	if start != nil && !end.After(*start) {
		return errors.New("end_date must be after start_date")
	}
	return nil
}

// NormalizeList trims entries, drops blanks, and collapses an empty result
// to nil, which the store writes as NULL and the wire emits as null --
// "everywhere" (design spec §2.11).
func NormalizeList(in []string) []string {
	var out []string
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// Validate checks the whole document before anything is stored, and
// normalizes it in place: lists per NormalizeList, sdk_configuration_values
// compacted, and Required forced false on label/external_survey questions
// (the reference's set_required_param; design spec §2.3).
func (d *Definition) Validate() error {
	if strings.TrimSpace(d.Name) == "" {
		return errors.New("name cannot be blank")
	}
	if err := ValidateWindow(d.StartTime, d.EndTime); err != nil {
		return err
	}
	d.VisibleStopList = NormalizeList(d.VisibleStopList)
	d.VisibleRouteList = NormalizeList(d.VisibleRouteList)
	for i := range d.Questions {
		q := &d.Questions[i]
		if err := q.Content.Validate(); err != nil {
			return fmt.Errorf("question %d: %w", i+1, err)
		}
		compact, err := compactJSON(q.Content.SDKConfigurationValues)
		if err != nil {
			return fmt.Errorf("question %d: sdk_configuration_values: %w", i+1, err)
		}
		q.Content.SDKConfigurationValues = compact
		if q.Content.Type == TypeLabel || q.Content.Type == TypeExternalSurvey {
			q.Required = false
		}
	}
	return nil
}

// ContentEqual compares two contents field by field. SDK values are
// compared as compacted bytes, which Validate guarantees for documents and
// the adapter guarantees for stored rows.
func ContentEqual(a, b Content) bool {
	return a.Type == b.Type && a.LabelText == b.LabelText &&
		slices.Equal(a.Options, b.Options) && a.URL == b.URL &&
		a.SurveyProvider == b.SurveyProvider &&
		slices.Equal(a.EmbeddedDataFields, b.EmbeddedDataFields) &&
		bytes.Equal(a.SDKConfigurationValues, b.SDKConfigurationValues)
}

// QuestionsEqual reports whether a document's questions are, in order,
// identical to the stored set -- the test for whether an edit touches a
// frozen survey's questions (design spec §2.13).
func QuestionsEqual(stored []Question, want []QuestionDefinition) bool {
	if len(stored) != len(want) {
		return false
	}
	for i := range stored {
		if stored[i].Required != want[i].Required || !ContentEqual(stored[i].Content, want[i].Content) {
			return false
		}
	}
	return true
}
