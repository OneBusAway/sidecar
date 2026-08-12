// View logic for the regions screen. Pure functions, for the same reason
// $lib/alerts is: the vitest project has no DOM, so anything that lives in
// markup is untested.

/** The PATCH /regions/{id} body. Both fields are optional; sending neither is a 400. */
export interface RegionPatchPayload {
	default_agency_id?: string;
	timezone?: string;
}

/**
 * buildRegionPatch turns one row's two inputs into a PATCH body.
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
 */
export function buildRegionPatch(
	defaultAgencyID: string,
	timezone: string,
): RegionPatchPayload {
	const payload: RegionPatchPayload = {
		default_agency_id: defaultAgencyID.trim(),
	};
	const tz = timezone.trim();
	if (tz !== '') payload.timezone = tz;
	return payload;
}
