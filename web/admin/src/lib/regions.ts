// View logic for the regions screen. Pure functions, for the same reason
// $lib/alerts is: the vitest project has no DOM, so anything that lives in
// markup is untested.

import type { KeyStatus } from './types';

/** The PATCH /regions/{id} body. All fields are optional; sending none is a 400. */
export interface RegionPatchPayload {
	default_agency_id?: string;
	timezone?: string;
	oba_api_key?: string;
}

/**
 * buildRegionPatch turns one row's inputs into a PATCH body.
 *
 * default_agency_id is always sent, empty included: an empty value clears the
 * default on purpose, after which creating an alert in that region needs an
 * explicit agency id.
 *
 * timezone is sent only when non-empty. Regions arrive from the directory with
 * no timezone at all -- it is one of the two locally-managed fields -- so an
 * empty box is the normal starting state, and the API rejects an empty
 * timezone ("timezone must not be empty"). Sending it anyway would make every
 * attempt to set an agency id on a zoneless region fail with a message about
 * timezones.
 *
 * obaAPIKey is sent only when it is defined, and `undefined` is the normal
 * state: the server never sends a key back, so an untouched field has no
 * value to resend. Sending '' for an untouched field would clear the region's
 * key on every unrelated edit. An explicit '' -- from the clear action --
 * is a deliberate clear and IS sent.
 */
export function buildRegionPatch(
	defaultAgencyID: string,
	timezone: string,
	obaAPIKey?: string,
): RegionPatchPayload {
	const payload: RegionPatchPayload = {
		default_agency_id: defaultAgencyID.trim(),
	};
	const tz = timezone.trim();
	if (tz !== '') payload.timezone = tz;
	if (obaAPIKey !== undefined) payload.oba_api_key = obaAPIKey.trim();
	return payload;
}

/**
 * formatCentroid renders a region's centroid for display.
 *
 * A null coordinate means the directory has not supplied usable bounds yet.
 * It must not render as 0, and 0 must not render as absent: 0,0 is a real
 * coordinate in the Gulf of Guinea, the same reason region id 0 is a real
 * region.
 */
export function formatCentroid(region: {
	latitude: number | null;
	longitude: number | null;
}): string {
	const { latitude, longitude } = region;
	if (latitude === null || longitude === null) return '—';
	return `${latitude.toFixed(4)}, ${longitude.toFixed(4)}`;
}

/**
 * describeKeyStatus turns the server's key status into operator-facing text.
 *
 * Three states rather than a boolean: a region with no key of its own but a
 * server default configured has working vehicle search, and reporting that as
 * "not configured" would send an operator chasing a problem that isn't there.
 */
export function describeKeyStatus(status: KeyStatus): string {
	switch (status) {
		case 'region':
			return 'Configured for this region';
		case 'default':
			return 'Using the server default';
		case 'none':
			return 'Not configured — vehicle search unavailable';
		default:
			return 'Unknown';
	}
}
