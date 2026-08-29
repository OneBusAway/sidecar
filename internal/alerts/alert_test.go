package alerts_test

import (
	"testing"

	"github.com/OneBusAway/sidecar/internal/alerts"
)

func TestAlert_TranslationStale(t *testing.T) {
	t.Parallel()
	a := alerts.Alert{HeaderText: "Header", DescriptionText: "Description"}
	for _, tc := range []struct {
		name string
		tr   alerts.Translation
		want bool
	}{
		{"fresh header", alerts.Translation{Field: alerts.FieldHeader, SourceSHA256: alerts.SourceHash("Header")}, false},
		{"fresh description", alerts.Translation{Field: alerts.FieldDescription, SourceSHA256: alerts.SourceHash("Description")}, false},
		{"stale header", alerts.Translation{Field: alerts.FieldHeader, SourceSHA256: alerts.SourceHash("Old header")}, true},
		{"stale description", alerts.Translation{Field: alerts.FieldDescription, SourceSHA256: alerts.SourceHash("Old")}, true},
		{"unknown field is never fresh", alerts.Translation{Field: "url", SourceSHA256: alerts.SourceHash("Header")}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := a.TranslationStale(tc.tr); got != tc.want {
				t.Errorf("TranslationStale = %v, want %v", got, tc.want)
			}
		})
	}
}
