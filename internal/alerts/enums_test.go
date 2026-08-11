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

func TestEnumTableCompleteness(t *testing.T) {
	t.Parallel()

	// Test causes: verify all entries parse successfully and map to non-UNKNOWN values
	causes := []string{
		"UNKNOWN_CAUSE", "OTHER_CAUSE", "TECHNICAL_PROBLEM", "STRIKE",
		"DEMONSTRATION", "ACCIDENT", "HOLIDAY", "WEATHER", "MAINTENANCE",
		"CONSTRUCTION", "POLICE_ACTIVITY", "MEDICAL_EMERGENCY",
	}
	for _, name := range causes {
		// Verify ParseCause succeeds
		got, err := alerts.ParseCause(name)
		if err != nil || got != name {
			t.Errorf("ParseCause(%q) = %q, %v; want %q, nil", name, got, err, name)
		}

		// Verify CauseEnum doesn't degrade to fallback (except for UNKNOWN_CAUSE itself)
		enum := alerts.CauseEnum(name)
		if name != "UNKNOWN_CAUSE" && enum == gtfs.Alert_UNKNOWN_CAUSE {
			t.Errorf("CauseEnum(%q) degraded to UNKNOWN_CAUSE", name)
		}
		if name == "UNKNOWN_CAUSE" && enum != gtfs.Alert_UNKNOWN_CAUSE {
			t.Errorf("CauseEnum(UNKNOWN_CAUSE) = %v, want UNKNOWN_CAUSE", enum)
		}
	}

	// Test effects: verify all entries parse successfully and map to non-UNKNOWN values
	effects := []string{
		"NO_SERVICE", "REDUCED_SERVICE", "SIGNIFICANT_DELAYS", "DETOUR",
		"ADDITIONAL_SERVICE", "MODIFIED_SERVICE", "OTHER_EFFECT", "UNKNOWN_EFFECT",
		"STOP_MOVED", "NO_EFFECT", "ACCESSIBILITY_ISSUE",
	}
	for _, name := range effects {
		// Verify ParseEffect succeeds
		got, err := alerts.ParseEffect(name)
		if err != nil || got != name {
			t.Errorf("ParseEffect(%q) = %q, %v; want %q, nil", name, got, err, name)
		}

		// Verify EffectEnum doesn't degrade to fallback (except for UNKNOWN_EFFECT itself)
		enum := alerts.EffectEnum(name)
		if name != "UNKNOWN_EFFECT" && enum == gtfs.Alert_UNKNOWN_EFFECT {
			t.Errorf("EffectEnum(%q) degraded to UNKNOWN_EFFECT", name)
		}
		if name == "UNKNOWN_EFFECT" && enum != gtfs.Alert_UNKNOWN_EFFECT {
			t.Errorf("EffectEnum(UNKNOWN_EFFECT) = %v, want UNKNOWN_EFFECT", enum)
		}
	}

	// Test severities: verify all entries parse successfully and map to non-UNKNOWN values
	severities := []string{
		"UNKNOWN_SEVERITY", "INFO", "WARNING", "SEVERE",
	}
	for _, name := range severities {
		// Verify ParseSeverity succeeds
		got, err := alerts.ParseSeverity(name)
		if err != nil || got != name {
			t.Errorf("ParseSeverity(%q) = %q, %v; want %q, nil", name, got, err, name)
		}

		// Verify SeverityEnum doesn't degrade to fallback (except for UNKNOWN_SEVERITY itself)
		enum := alerts.SeverityEnum(name)
		if name != "UNKNOWN_SEVERITY" && enum == gtfs.Alert_UNKNOWN_SEVERITY {
			t.Errorf("SeverityEnum(%q) degraded to UNKNOWN_SEVERITY", name)
		}
		if name == "UNKNOWN_SEVERITY" && enum != gtfs.Alert_UNKNOWN_SEVERITY {
			t.Errorf("SeverityEnum(UNKNOWN_SEVERITY) = %v, want UNKNOWN_SEVERITY", enum)
		}
	}
}
