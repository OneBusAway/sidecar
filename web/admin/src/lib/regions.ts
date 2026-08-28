// View logic for the regions screen. Pure functions, for the same reason
// $lib/alerts is: the vitest project has no DOM, so anything that lives in
// markup is untested.

import type { AdminFeature, KeyStatus, Region } from './types';

/** localStorage key for the last region an operator successfully reached. */
export const LAST_REGION_KEY = 'sidecar.lastRegion';

/**
 * pickRegion resolves the region picker screen's outcome.
 *
 * One region auto-forwards: making an operator choose from a list of one is
 * a click that can only have one outcome. Otherwise a remembered region is
 * honoured only when it is still in the reachable list -- compared with
 * `String(r.id) === remembered`, deliberately not `Number(remembered)`, so a
 * non-numeric or since-removed value matches nothing rather than coercing to
 * some region by accident.
 *
 * Returns null rather than a default for "no answer" (nothing remembered,
 * several regions, or none at all): the picker screen is what handles that
 * case, not this function guessing on its behalf.
 */
export function pickRegion(
	regions: Region[],
	remembered: string | null,
): Region | null {
	if (regions.length === 1) return regions[0];
	if (remembered !== null) {
		const match = regions.find((r) => String(r.id) === remembered);
		if (match) return match;
	}
	return null;
}

/**
 * hasFeature reports whether this deployment registered an admin route
 * family for a region.
 *
 * `features` is populated only by `GET /regions/{id}` -- the list endpoint
 * never includes it (see the trap documented on `Region.features`). An
 * absent array must return false here, never true: reading "absent" as
 * "everything is enabled" would render a control -- the push Send button, a
 * survey editor -- against a route this deployment never registered.
 */
export function hasFeature(region: Region, feature: AdminFeature): boolean {
	return region.features?.includes(feature) ?? false;
}

/**
 * rememberRegion persists the last region an operator successfully reached,
 * so a plain visit to the admin root (or a reload) can return them straight
 * there instead of stopping at the picker every time.
 *
 * Wrapped in try/catch: localStorage throws outright in some privacy modes
 * (Safari private browsing, a cookies-disabled profile), and remembering a
 * region is a convenience -- it must never take down a page whose actual job
 * is loading alerts.
 */
export function rememberRegion(id: number): void {
	try {
		localStorage.setItem(LAST_REGION_KEY, String(id));
	} catch {
		// Best-effort only; see the comment above.
	}
}

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
