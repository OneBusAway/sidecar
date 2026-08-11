package alerts

import (
	"fmt"
	"sort"
	"strings"

	"github.com/MobilityData/gtfs-realtime-bindings/golang/gtfs"
)

// Enum names are stored as text rather than numbers: portable across database
// engines, legible in `sidecar-admin alert list`, and stable if the protobuf
// numbering ever changes.

const (
	// UnknownCause is the fallback when a cause is unknown.
	UnknownCause = "UNKNOWN_CAUSE"
	// UnknownEffect is the fallback when an effect is unknown.
	UnknownEffect = "UNKNOWN_EFFECT"
	// UnknownSeverity is the fallback when a severity is unknown.
	UnknownSeverity = "UNKNOWN_SEVERITY"
)

var (
	causes = map[string]gtfs.Alert_Cause{
		"UNKNOWN_CAUSE":     gtfs.Alert_UNKNOWN_CAUSE,
		"OTHER_CAUSE":       gtfs.Alert_OTHER_CAUSE,
		"TECHNICAL_PROBLEM": gtfs.Alert_TECHNICAL_PROBLEM,
		"STRIKE":            gtfs.Alert_STRIKE,
		"DEMONSTRATION":     gtfs.Alert_DEMONSTRATION,
		"ACCIDENT":          gtfs.Alert_ACCIDENT,
		"HOLIDAY":           gtfs.Alert_HOLIDAY,
		"WEATHER":           gtfs.Alert_WEATHER,
		"MAINTENANCE":       gtfs.Alert_MAINTENANCE,
		"CONSTRUCTION":      gtfs.Alert_CONSTRUCTION,
		"POLICE_ACTIVITY":   gtfs.Alert_POLICE_ACTIVITY,
		"MEDICAL_EMERGENCY": gtfs.Alert_MEDICAL_EMERGENCY,
	}
	effects = map[string]gtfs.Alert_Effect{
		"NO_SERVICE":          gtfs.Alert_NO_SERVICE,
		"REDUCED_SERVICE":     gtfs.Alert_REDUCED_SERVICE,
		"SIGNIFICANT_DELAYS":  gtfs.Alert_SIGNIFICANT_DELAYS,
		"DETOUR":              gtfs.Alert_DETOUR,
		"ADDITIONAL_SERVICE":  gtfs.Alert_ADDITIONAL_SERVICE,
		"MODIFIED_SERVICE":    gtfs.Alert_MODIFIED_SERVICE,
		"OTHER_EFFECT":        gtfs.Alert_OTHER_EFFECT,
		"UNKNOWN_EFFECT":      gtfs.Alert_UNKNOWN_EFFECT,
		"STOP_MOVED":          gtfs.Alert_STOP_MOVED,
		"NO_EFFECT":           gtfs.Alert_NO_EFFECT,
		"ACCESSIBILITY_ISSUE": gtfs.Alert_ACCESSIBILITY_ISSUE,
	}
	severities = map[string]gtfs.Alert_SeverityLevel{
		"UNKNOWN_SEVERITY": gtfs.Alert_UNKNOWN_SEVERITY,
		"INFO":             gtfs.Alert_INFO,
		"WARNING":          gtfs.Alert_WARNING,
		"SEVERE":           gtfs.Alert_SEVERE,
	}
)

func parseEnum[T any](kind, in, fallback string, table map[string]T) (string, error) {
	name := strings.ToUpper(strings.TrimSpace(in))
	if name == "" {
		return fallback, nil
	}
	if _, ok := table[name]; !ok {
		valid := make([]string, 0, len(table))
		for k := range table {
			valid = append(valid, k)
		}
		sort.Strings(valid)
		return "", fmt.Errorf("unknown %s %q; valid values: %s", kind, in, strings.Join(valid, ", "))
	}
	return name, nil
}

// ParseCause validates an author-supplied cause name. Empty means unknown.
func ParseCause(in string) (string, error) {
	return parseEnum("cause", in, UnknownCause, causes)
}

// ParseEffect validates an author-supplied effect name. Empty means unknown.
func ParseEffect(in string) (string, error) {
	return parseEnum("effect", in, UnknownEffect, effects)
}

// ParseSeverity validates an author-supplied severity name. Empty means unknown.
func ParseSeverity(in string) (string, error) {
	return parseEnum("severity", in, UnknownSeverity, severities)
}

// CauseEnum maps a stored name to its protobuf value. An unmappable name
// degrades to UNKNOWN_CAUSE rather than failing: names are validated at author
// time, so a bad value here means schema drift or a hand-edited row, and one
// such row must not darken the whole region's feed.
func CauseEnum(name string) gtfs.Alert_Cause {
	if v, ok := causes[name]; ok {
		return v
	}
	return gtfs.Alert_UNKNOWN_CAUSE
}

// EffectEnum maps a stored name to its protobuf value, degrading as CauseEnum does.
func EffectEnum(name string) gtfs.Alert_Effect {
	if v, ok := effects[name]; ok {
		return v
	}
	return gtfs.Alert_UNKNOWN_EFFECT
}

// SeverityEnum maps a stored name to its protobuf value, degrading as CauseEnum does.
func SeverityEnum(name string) gtfs.Alert_SeverityLevel {
	if v, ok := severities[name]; ok {
		return v
	}
	return gtfs.Alert_UNKNOWN_SEVERITY
}
