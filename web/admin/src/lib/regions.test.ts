import { describe, expect, it } from 'vitest';
import {
	buildRegionPatch,
	describeKeyStatus,
	formatCentroid,
	hasFeature,
	pickRegion,
} from './regions';
import type { KeyStatus, Region } from './types';

function region(over: Partial<Region> = {}): Region {
	return {
		id: 0,
		name: 'Tampa Bay',
		oba_base_url: 'https://api.tampa.example',
		sidecar_base_url: 'https://sidecar.tampa.example',
		language: 'en',
		active: true,
		default_agency_id: 'HART',
		timezone: '',
		latitude: null,
		longitude: null,
		oba_api_key: 'none',
		...over,
	};
}

// Region 0 is Tampa Bay -- a real region, never "unset" -- which is exactly
// why it is the fixture used throughout this file rather than a round-number
// id that would hide a truthiness bug.
const tampa = region({ id: 0, name: 'Tampa Bay' });
const puget = region({ id: 5, name: 'Puget Sound' });

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

describe('pickRegion', () => {
	// One region auto-forwards: making an operator choose from a list of one
	// is a click that can only have one outcome.
	it('returns the only region when there is exactly one', () => {
		expect(pickRegion([tampa], null)?.id).toBe(0);
	});

	// Region 0 is Tampa Bay. A remembered '0' must resolve to Tampa, and any
	// truthiness test on the id is a bug that would send the operator to the
	// picker instead.
	it('honours a remembered region 0', () => {
		expect(pickRegion([tampa, puget], '0')?.id).toBe(0);
	});

	it('ignores a remembered region that is no longer listed', () => {
		expect(pickRegion([tampa, puget], '99')).toBeNull();
	});

	it('ignores a non-numeric remembered value', () => {
		expect(pickRegion([tampa, puget], 'tampa')).toBeNull();
	});

	it('returns null with several regions and nothing remembered', () => {
		expect(pickRegion([tampa, puget], null)).toBeNull();
	});

	it('returns null with no regions at all', () => {
		expect(pickRegion([], '0')).toBeNull();
	});
});

describe('hasFeature', () => {
	// features is absent on the LIST endpoint's regions and present only on
	// GET /regions/{id}. Absent must not read as "everything is enabled":
	// that would render a Send button against routes that do not exist.
	it('is false when features is absent', () => {
		expect(hasFeature({ ...puget, features: undefined }, 'pushes')).toBe(false);
	});

	it('is true when the family is listed', () => {
		expect(
			hasFeature({ ...puget, features: ['alerts', 'pushes'] }, 'pushes'),
		).toBe(true);
	});

	it('is false when the family is not listed', () => {
		expect(hasFeature({ ...puget, features: ['alerts'] }, 'pushes')).toBe(
			false,
		);
	});
});
