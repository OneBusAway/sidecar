import { describe, expect, it } from 'vitest';
import {
	instantToLocalInput,
	localInputToRFC3339,
	offsetMinutes,
} from './datetime';

describe('offsetMinutes', () => {
	it('handles a 45-minute offset zone', () => {
		expect(
			offsetMinutes(new Date('2026-01-15T00:00:00Z'), 'Asia/Kathmandu'),
		).toBe(345);
	});
	it('tracks DST', () => {
		expect(
			offsetMinutes(new Date('2026-01-15T12:00:00Z'), 'America/Los_Angeles'),
		).toBe(-480);
		expect(
			offsetMinutes(new Date('2026-07-15T12:00:00Z'), 'America/Los_Angeles'),
		).toBe(-420);
	});
});

describe('localInputToRFC3339', () => {
	it('stamps the zone offset for the wall time, not the current offset', () => {
		// January wall time in LA is PST even if "today" is July.
		expect(localInputToRFC3339('2026-01-15T14:00', 'America/Los_Angeles')).toBe(
			'2026-01-15T14:00:00-08:00',
		);
		expect(localInputToRFC3339('2026-07-15T14:00', 'America/Los_Angeles')).toBe(
			'2026-07-15T14:00:00-07:00',
		);
	});
	it('handles positive offsets', () => {
		expect(localInputToRFC3339('2026-08-15T09:30', 'Asia/Kathmandu')).toBe(
			'2026-08-15T09:30:00+05:45',
		);
	});
	// The two assertions above land far from any transition, so they pass even
	// with a single-pass lookup. This one does not: LA springs forward at
	// 2026-03-08T10:00Z, so reading the offset at the naive instant (09:00Z,
	// still PST) gives -08:00, while the wall time 09:00 on that date is
	// genuinely PDT. Only the refinement pass gets it right.
	it('uses the offset in force at the wall time across a spring-forward boundary', () => {
		expect(localInputToRFC3339('2026-03-08T09:00', 'America/Los_Angeles')).toBe(
			'2026-03-08T09:00:00-07:00',
		);
	});
});

describe('instantToLocalInput', () => {
	it('round-trips through the region timezone', () => {
		expect(
			instantToLocalInput('2026-08-15T21:00:00Z', 'America/Los_Angeles'),
		).toBe('2026-08-15T14:00');
	});
	// The assertion above cannot fail on a machine whose own TZ is
	// America/Los_Angeles -- which is exactly where this was written. This one
	// pins the region zone against any plausible host zone, and crosses a date
	// boundary while it is at it.
	it('formats in the region zone, not the browser zone', () => {
		expect(instantToLocalInput('2026-08-15T21:00:00Z', 'Asia/Kathmandu')).toBe(
			'2026-08-16T02:45',
		);
	});
});

describe('round trip', () => {
	// The pair has to compose: what the edit form shows must submit back as the
	// same instant, or every save silently walks the alert through time.
	it('survives instant -> input -> instant across zones and seasons', () => {
		const cases: [string, string][] = [
			['2026-01-15T22:00:00Z', 'America/Los_Angeles'],
			['2026-07-15T22:00:00Z', 'America/Los_Angeles'],
			['2026-03-08T18:00:00Z', 'America/Los_Angeles'],
			['2026-08-15T21:00:00Z', 'Asia/Kathmandu'],
			['2026-11-01T12:00:00Z', 'Europe/Berlin'],
		];
		for (const [iso, zone] of cases) {
			const round = localInputToRFC3339(instantToLocalInput(iso, zone), zone);
			expect(new Date(round).toISOString(), `${iso} in ${zone}`).toBe(
				iso.replace('Z', '.000Z'),
			);
		}
	});
});
