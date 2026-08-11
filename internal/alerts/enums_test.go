package alerts_test

import (
	"testing"

	"github.com/MobilityData/gtfs-realtime-bindings/golang/gtfs"

	"github.com/OneBusAway/sidecar/internal/alerts"
)

func TestParseCause(t *testing.T) {
	t.Parallel()

	if got, err := alerts.ParseCause("construction"); err != nil || got != "CONSTRUCTION" {
		t.Errorf("ParseCause(construction) = %q, %v; want CONSTRUCTION, nil", got, err)
	}
	if got, err := alerts.ParseCause(""); err != nil || got != "UNKNOWN_CAUSE" {
		t.Errorf("ParseCause(empty) = %q, %v; want UNKNOWN_CAUSE, nil", got, err)
	}
	if _, err := alerts.ParseCause("NOT_A_CAUSE"); err == nil {
		t.Error("ParseCause(NOT_A_CAUSE) = nil error, want error listing valid values")
	}
}

func TestEnumMapping(t *testing.T) {
	t.Parallel()

	if got := alerts.CauseEnum("CONSTRUCTION"); got != gtfs.Alert_CONSTRUCTION {
		t.Errorf("CauseEnum(CONSTRUCTION) = %v, want CONSTRUCTION", got)
	}
	// An unmappable name must degrade, never panic: one bad row would
	// otherwise darken an entire region's feed.
	if got := alerts.CauseEnum("GARBAGE"); got != gtfs.Alert_UNKNOWN_CAUSE {
		t.Errorf("CauseEnum(GARBAGE) = %v, want UNKNOWN_CAUSE", got)
	}
	if got := alerts.EffectEnum("GARBAGE"); got != gtfs.Alert_UNKNOWN_EFFECT {
		t.Errorf("EffectEnum(GARBAGE) = %v, want UNKNOWN_EFFECT", got)
	}
	if got := alerts.SeverityEnum("GARBAGE"); got != gtfs.Alert_UNKNOWN_SEVERITY {
		t.Errorf("SeverityEnum(GARBAGE) = %v, want UNKNOWN_SEVERITY", got)
	}
}

func TestNormalizeLanguage(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"ES":      "es",
		" es-MX ": "es-mx", //nolint:gocritic // spaces are intentional test inputs
		"zh-Hans": "zh-hans",
	}
	for in, want := range tests {
		if got := alerts.NormalizeLanguage(in); got != want {
			t.Errorf("NormalizeLanguage(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSourceHash(t *testing.T) {
	t.Parallel()

	if alerts.SourceHash("a") == alerts.SourceHash("b") {
		t.Error("SourceHash collided on different inputs")
	}
	h1 := alerts.SourceHash("a")
	h2 := alerts.SourceHash("a")
	if h1 != h2 {
		t.Error("SourceHash is not deterministic")
	}
}
