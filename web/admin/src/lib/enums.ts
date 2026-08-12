// GTFS-realtime enum option lists for the alert form.
//
// These are copied from internal/alerts/enums.go and MUST stay identical to
// it: the server validates every submitted value against that table and
// answers a drifted name with a 400 naming the valid values, which the form
// shows verbatim. So drift here is visible rather than silent -- but it still
// costs the operator a round trip, so keep the lists in sync.
//
// The names are the GTFS-realtime spec's own, shouted-snake-case included;
// labels are only a display nicety, so the value is what travels.

/** One option in a cause/effect/severity select. */
export interface EnumOption {
	value: string;
	label: string;
}

/**
 * titleCase renders a GTFS enum name for humans: TECHNICAL_PROBLEM ->
 * "Technical problem". The value submitted is always the untouched name.
 */
function option(value: string): EnumOption {
	const words = value.toLowerCase().split('_');
	return {
		value,
		label:
			words[0][0].toUpperCase() +
			words[0].slice(1) +
			' ' +
			words.slice(1).join(' '),
	};
}

/** Alert.Cause, the 12 names accepted by alerts.ParseCause. */
export const CAUSES: EnumOption[] = [
	'UNKNOWN_CAUSE',
	'OTHER_CAUSE',
	'TECHNICAL_PROBLEM',
	'STRIKE',
	'DEMONSTRATION',
	'ACCIDENT',
	'HOLIDAY',
	'WEATHER',
	'MAINTENANCE',
	'CONSTRUCTION',
	'POLICE_ACTIVITY',
	'MEDICAL_EMERGENCY',
].map(option);

/** Alert.Effect, the 11 names accepted by alerts.ParseEffect. */
export const EFFECTS: EnumOption[] = [
	'NO_SERVICE',
	'REDUCED_SERVICE',
	'SIGNIFICANT_DELAYS',
	'DETOUR',
	'ADDITIONAL_SERVICE',
	'MODIFIED_SERVICE',
	'OTHER_EFFECT',
	'UNKNOWN_EFFECT',
	'STOP_MOVED',
	'NO_EFFECT',
	'ACCESSIBILITY_ISSUE',
].map(option);

/** Alert.SeverityLevel, the 4 names accepted by alerts.ParseSeverity. */
export const SEVERITIES: EnumOption[] = [
	'UNKNOWN_SEVERITY',
	'INFO',
	'WARNING',
	'SEVERE',
].map(option);

/** The fallbacks alerts.ParseCause and friends apply to an empty value. */
export const DEFAULT_CAUSE = 'UNKNOWN_CAUSE';
export const DEFAULT_EFFECT = 'UNKNOWN_EFFECT';
export const DEFAULT_SEVERITY = 'UNKNOWN_SEVERITY';
