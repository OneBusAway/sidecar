import { api, regionPath } from '$lib/api';
import { toLoadError } from '$lib/loaderror';
import { rememberRegion } from '$lib/regions';
import type { Alert, Region } from '$lib/types';
import type { PageLoad } from './$types';

export const load: PageLoad = async ({ parent, params }) => {
	// The session guard first, as before: a signed-out visitor must not fire
	// requests that can only 401.
	await parent();
	try {
		const [alerts, region] = await Promise.all([
			api.get<Alert[]>(regionPath(params.region, '/alerts')),
			// The single-region endpoint, not the list: it is the only one
			// that carries `features`, and it 404s for a region this
			// operator cannot reach -- which is what turns a hand-edited URL
			// into the error page rather than an empty list.
			api.get<Region>(regionPath(params.region, '')),
		]);
		// Remembered so a plain "/admin" visit (or a reload) can skip the
		// picker and land straight back here.
		rememberRegion(region.id);
		return { alerts, region };
	} catch (err) {
		toLoadError(err);
	}
};
