import { describe, expect, it } from 'vitest';
import { buildRegionPatch } from './regions';

describe('buildRegionPatch', () => {
	it('sends both fields when both are filled in', () => {
		expect(buildRegionPatch('HART', 'America/New_York')).toEqual({
			default_agency_id: 'HART',
			timezone: 'America/New_York',
		});
	});

	// Regions arrive from the directory with no timezone -- it is one of the
	// two locally-managed fields -- so a blank timezone box is the normal
	// starting state. Sending `timezone: ""` would make the server reject the
	// save with "timezone must not be empty", so setting an agency id on a
	// zoneless region would fail with a message about timezones.
	it('omits a blank timezone rather than sending an empty one', () => {
		const payload = buildRegionPatch('HART', '');
		expect('timezone' in payload).toBe(false);
		expect(payload.default_agency_id).toBe('HART');
	});

	it('omits a whitespace-only timezone', () => {
		expect('timezone' in buildRegionPatch('HART', '   ')).toBe(false);
	});

	// Clearing the agency id is a real operation: afterwards, creating an alert
	// in that region requires an explicit agency_id. Dropping the field would
	// make the box un-clearable.
	it('sends an empty agency id so the default can be cleared', () => {
		expect(buildRegionPatch('', 'America/New_York')).toEqual({
			default_agency_id: '',
			timezone: 'America/New_York',
		});
	});

	// An invalid zone is the server's call: it owns the tzdata and its 400 names
	// the bad value. Filtering here would only add a second, worse opinion.
	it('passes an unrecognised timezone through for the server to reject', () => {
		expect(buildRegionPatch('HART', 'America/Nowhere').timezone).toBe(
			'America/Nowhere',
		);
	});
});
