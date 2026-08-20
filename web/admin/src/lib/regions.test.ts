import { describe, expect, it } from 'vitest';
import { buildRegionPatch, describeKeyStatus, formatCentroid } from './regions';
import type { KeyStatus } from './types';

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

describe('buildRegionPatch with an API key', () => {
	// Omission means unchanged. If an untouched key field sent '', every
	// unrelated edit an operator makes would silently wipe the region's key.
	it('omits oba_api_key entirely when undefined', () => {
		const payload = buildRegionPatch('1', 'America/Los_Angeles', undefined);
		expect('oba_api_key' in payload).toBe(false);
	});

	it('sends an empty oba_api_key when the operator clears it', () => {
		const payload = buildRegionPatch('1', 'America/Los_Angeles', '');
		expect(payload.oba_api_key).toBe('');
	});

	it('sends a trimmed key when one is entered', () => {
		const payload = buildRegionPatch('1', 'America/Los_Angeles', '  abc  ');
		expect(payload.oba_api_key).toBe('abc');
	});
});

describe('formatCentroid', () => {
	it('renders a point to four decimals', () => {
		expect(
			formatCentroid({ latitude: 47.752812, longitude: -122.492431 }),
		).toBe('47.7528, -122.4924');
	});

	// 0,0 is a real coordinate in the Gulf of Guinea, so null must render as
	// "unknown" and 0 must render as 0 -- never the other way round.
	it('renders null as an em dash', () => {
		expect(formatCentroid({ latitude: null, longitude: null })).toBe('—');
	});

	it('renders Null Island as a real point', () => {
		expect(formatCentroid({ latitude: 0, longitude: 0 })).toBe(
			'0.0000, 0.0000',
		);
	});

	it('treats a half-set centroid as absent', () => {
		expect(formatCentroid({ latitude: 47.75, longitude: null })).toBe('—');
	});
});

describe('describeKeyStatus', () => {
	// Three states, not a boolean: a region whose vehicle search works fine
	// through the server default must not read as "not configured".
	it('distinguishes all three states', () => {
		expect(describeKeyStatus('region')).toBe('Configured for this region');
		expect(describeKeyStatus('default')).toBe('Using the server default');
		expect(describeKeyStatus('none')).toBe(
			'Not configured — vehicle search unavailable',
		);
	});

	it('falls back rather than rendering undefined for an unknown value', () => {
		expect(describeKeyStatus('something-new' as KeyStatus)).toBe('Unknown');
	});
});
