import { describe, expect, it } from 'vitest';
import {
	alertBadges,
	buildCreatePayload,
	buildPatchPayload,
	buildTranslationPayload,
	formValuesFromAlert,
	formatInstantForRegion,
	fromInstant,
	toInstant,
	type AlertFormValues,
} from './alerts';
import type { Alert } from './types';

// Fixture zones are chosen so no plausible development machine can make an
// assertion pass by accident: Asia/Kathmandu is +05:45 (a 45-minute offset no
// laptop is set to, and one that catches hour-rounding), and the
// America/Los_Angeles cases pin a specific offset for a specific date rather
// than "whatever the host is doing". `make test-tz` runs this file under both
// TZ=UTC and TZ=Asia/Kathmandu for the same reason.
const KATHMANDU = 'Asia/Kathmandu';
const LA = 'America/Los_Angeles';

function alert(over: Partial<Alert> = {}): Alert {
	return {
		id: 1,
		region_id: 0,
		agency_id: 'HART',
		header: 'Bridge out',
		description: 'Use the 12 instead.',
		url: 'https://example.test/notice',
		cause: 'CONSTRUCTION',
		effect: 'DETOUR',
		severity: 'WARNING',
		start_time: '2026-08-15T21:00:00Z',
		end_time: null,
		published: false,
		is_test: false,
		created_at: '2026-08-01T00:00:00Z',
		updated_at: '2026-08-01T00:00:00Z',
		translations: [],
		...over,
	};
}

function values(over: Partial<AlertFormValues> = {}): AlertFormValues {
	return {
		agencyId: 'HART',
		header: 'Bridge out',
		description: 'Use the 12 instead.',
		url: 'https://example.test/notice',
		cause: 'CONSTRUCTION',
		effect: 'DETOUR',
		severity: 'WARNING',
		start: '2026-08-16T02:45',
		end: '',
		clearEnd: false,
		isTest: false,
		...over,
	};
}

describe('formatInstantForRegion', () => {
	// 21:00Z is the next day in Kathmandu, so this fails if the region zone is
	// ignored -- under TZ=UTC and under TZ=Asia/Kathmandu alike, since the
	// browser zone is never consulted either way.
	it('renders in the region zone, crossing the date boundary', () => {
		expect(formatInstantForRegion('2026-08-15T21:00:00Z', KATHMANDU)).toBe(
			'2026-08-16 02:45',
		);
	});

	it('renders in the region zone for a negative offset', () => {
		expect(formatInstantForRegion('2026-08-15T21:00:00Z', LA)).toBe(
			'2026-08-15 14:00',
		);
	});

	// A region with no configured timezone gets the server's UTC string, not a
	// wall-clock time in a guessed zone.
	it('falls back to the raw instant when the region has no timezone', () => {
		expect(formatInstantForRegion('2026-08-15T21:00:00Z', '')).toBe(
			'2026-08-15T21:00:00Z',
		);
	});
});

describe('alertBadges', () => {
	it('marks an unpublished alert as a draft', () => {
		expect(alertBadges({ published: false, is_test: false })).toEqual([
			{ label: 'Draft', tone: 'draft' },
		]);
	});

	it('marks a published alert as published', () => {
		expect(alertBadges({ published: true, is_test: false })).toEqual([
			{ label: 'Published', tone: 'published' },
		]);
	});

	// A published test alert is still invisible to riders. Showing only
	// "Published" would say the opposite.
	it('carries both badges for a published test alert', () => {
		expect(alertBadges({ published: true, is_test: true })).toEqual([
			{ label: 'Published', tone: 'published' },
			{ label: 'Test', tone: 'test' },
		]);
	});
});

describe('toInstant', () => {
	it('stamps the region offset on a datetime-local value', () => {
		expect(toInstant('2026-08-16T02:45', KATHMANDU)).toBe(
			'2026-08-16T02:45:00+05:45',
		);
	});

	// The offset must be the one in force at the entered WALL TIME, not today's.
	it('uses the wall-time offset across a DST boundary', () => {
		expect(toInstant('2026-01-15T14:00', LA)).toBe('2026-01-15T14:00:00-08:00');
		expect(toInstant('2026-07-15T14:00', LA)).toBe('2026-07-15T14:00:00-07:00');
	});

	// No configured zone means the operator typed RFC 3339 themselves. Guessing
	// a zone here is the whole failure this design exists to prevent.
	it('passes a raw value through when the region has no timezone', () => {
		expect(toInstant('2026-08-15T14:00:00-07:00', '')).toBe(
			'2026-08-15T14:00:00-07:00',
		);
	});

	it('passes an empty value through rather than throwing', () => {
		expect(toInstant('', KATHMANDU)).toBe('');
	});
});

describe('buildCreatePayload', () => {
	it('buildCreatePayload no longer sends region_id', () => {
		const payload = buildCreatePayload(values(), 'America/Los_Angeles');
		// The region is in the URL. Sending it in the body is now a 400, so a
		// stale client cannot believe it targeted a region.
		expect(payload).not.toHaveProperty('region_id');
	});

	it('converts start through the region zone', () => {
		const payload = buildCreatePayload(
			values({ start: '2026-08-16T02:45' }),
			KATHMANDU,
		);
		expect(payload.start_time).toBe('2026-08-16T02:45:00+05:45');
	});

	it('omits end_time entirely when the end field is blank', () => {
		const payload = buildCreatePayload(values({ end: '' }), KATHMANDU);
		expect('end_time' in payload).toBe(false);
	});

	it('includes a converted end_time when the end field is set', () => {
		const payload = buildCreatePayload(
			values({ end: '2026-08-17T02:45' }),
			KATHMANDU,
		);
		expect(payload.end_time).toBe('2026-08-17T02:45:00+05:45');
	});

	// The server resolves an empty agency id against the region default and,
	// failing that, answers with the message that tells the operator to set
	// one. The client must not pre-empt either step.
	it('sends an empty agency id rather than inventing one', () => {
		const payload = buildCreatePayload(values({ agencyId: '' }), KATHMANDU);
		expect(payload.agency_id).toBe('');
	});
});

describe('buildPatchPayload', () => {
	// Region is immutable through the API; a region_id here would be ignored at
	// best and is a lie about what the form can do at worst.
	it('never sends a region_id', () => {
		expect('region_id' in buildPatchPayload(values(), KATHMANDU)).toBe(false);
	});

	// JSON cannot tell an absent field from null, so clearing needs the flag.
	it('clears the end time with the flag, not an absent field', () => {
		const payload = buildPatchPayload(
			values({ end: '2026-08-17T02:45', clearEnd: true }),
			KATHMANDU,
		);
		expect(payload.clear_end_time).toBe(true);
		// Sending both is a 400 ("send only one of end_time and clear_end_time").
		expect('end_time' in payload).toBe(false);
	});

	// Emptying the field is the same intent as ticking the box. Dropping it
	// would leave the old end time live while the form shows it blank.
	it('treats an emptied end field as a clear', () => {
		const payload = buildPatchPayload(values({ end: '' }), KATHMANDU);
		expect(payload.clear_end_time).toBe(true);
	});

	it('sends a converted end_time with no clear flag when the field is set', () => {
		const payload = buildPatchPayload(
			values({ end: '2026-08-17T02:45' }),
			KATHMANDU,
		);
		expect(payload.end_time).toBe('2026-08-17T02:45:00+05:45');
		expect('clear_end_time' in payload).toBe(false);
	});
});

describe('form prefill round trip', () => {
	// Loading an alert and saving it untouched must submit the same instants it
	// was loaded with, TO THE MINUTE (see the seconds case below -- every
	// fixture here is deliberately on a whole minute, so this says nothing
	// about seconds and must not be read as if it did). Every case pins the
	// region zone explicitly, and the comparison is on the absolute instant, so
	// the host zone cannot influence the result -- including the DST-boundary
	// case, where the naive wall time and the instant disagree about which
	// offset applies.
	it('preserves the instants to the minute across zones and a DST boundary', () => {
		const cases: [string, string][] = [
			['2026-08-15T21:00:00Z', KATHMANDU],
			['2026-01-15T22:00:00Z', LA],
			['2026-07-15T22:00:00Z', LA],
			['2026-03-08T18:00:00Z', LA],
			['2026-11-01T12:00:00Z', 'Europe/Berlin'],
		];
		for (const [iso, zone] of cases) {
			const end = '2026-12-01T00:00:00Z';
			const payload = buildPatchPayload(
				formValuesFromAlert(alert({ start_time: iso, end_time: end }), zone),
				zone,
			);
			expect(
				new Date(payload.start_time).toISOString(),
				`start ${iso} in ${zone}`,
			).toBe(iso.replace('Z', '.000Z'));
			expect(
				new Date(payload.end_time ?? '').toISOString(),
				`end in ${zone}`,
			).toBe(end.replace('Z', '.000Z'));
		}
	});

	// The zoned round trip is minute-granular, so a CLI-authored alert with
	// seconds on it loses them when the SPA saves. This is a decision, not an
	// accident (see fromInstant's comment): `datetime-local` without `step` is
	// a minute control, the truncation is at most 59 seconds, and the operator
	// can see the rounded value in the field before saving. Pinned here so it
	// cannot become an accident later -- if someone adds `step="1"` and carries
	// seconds through, this test fails and they have to say so.
	it('truncates seconds on the zoned round trip, by design', () => {
		const zoned = formValuesFromAlert(
			alert({ start_time: '2026-08-15T21:00:30Z' }),
			KATHMANDU,
		);
		expect(zoned.start).toBe('2026-08-16T02:45');
		expect(buildPatchPayload(zoned, KATHMANDU).start_time).toBe(
			'2026-08-16T02:45:00+05:45',
		);

		// The zoneless path has no such limit: it is a raw string, so seconds
		// survive untouched.
		const raw = formValuesFromAlert(
			alert({ start_time: '2026-08-15T21:00:30Z' }),
			'',
		);
		expect(buildPatchPayload(raw, '').start_time).toBe('2026-08-15T21:00:30Z');
	});

	// A zoneless region round-trips through the raw RFC 3339 text input, so the
	// value must come back out byte for byte -- not reformatted, not stamped.
	it('round-trips a zoneless region through the raw field', () => {
		const values = formValuesFromAlert(
			alert({ start_time: '2026-08-15T21:00:00Z' }),
			'',
		);
		expect(values.start).toBe('2026-08-15T21:00:00Z');
		expect(buildPatchPayload(values, '').start_time).toBe(
			'2026-08-15T21:00:00Z',
		);
	});

	it('leaves the end field blank when the alert has no end time', () => {
		expect(formValuesFromAlert(alert({ end_time: null }), KATHMANDU).end).toBe(
			'',
		);
	});
});

describe('fromInstant', () => {
	it('renders in the region zone, not the browser zone', () => {
		expect(fromInstant('2026-08-15T21:00:00Z', KATHMANDU)).toBe(
			'2026-08-16T02:45',
		);
	});
});

describe('buildTranslationPayload', () => {
	it('sends only the header when only the header is filled in', () => {
		expect(buildTranslationPayload('Puente cerrado', '')).toEqual({
			header: 'Puente cerrado',
		});
	});

	it('sends only the description when only the description is filled in', () => {
		expect(buildTranslationPayload('', 'Use la 12.')).toEqual({
			description: 'Use la 12.',
		});
	});

	it('sends both when both are filled in', () => {
		expect(buildTranslationPayload('Puente cerrado', 'Use la 12.')).toEqual({
			header: 'Puente cerrado',
			description: 'Use la 12.',
		});
	});

	// An empty string is a real translation -- it would put a blank header in
	// front of Spanish-speaking riders. Omission is the only way to say "leave
	// this field alone", and an empty form gets the server's own 400.
	it('omits blank and whitespace-only fields instead of storing them empty', () => {
		expect(buildTranslationPayload('   ', '\n')).toEqual({});
	});
});
