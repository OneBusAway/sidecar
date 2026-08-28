import { api, regionPath } from '$lib/api';
import { toLoadError } from '$lib/loaderror';
import { rememberRegion } from '$lib/regions';
import type { Region } from '$lib/types';
import type { PageLoad } from './$types';

export const load: PageLoad = async ({ parent, params }) => {
	// See the alerts list load: the session guard runs first so a signed-out
	// visitor never fires a request that can only 401.
	await parent();
	try {
		// Only the region: the create form has nothing else to fetch, and the
		// region 404s for a region this operator cannot reach.
		const region = await api.get<Region>(regionPath(params.region, ''));
		rememberRegion(region.id);
		return { region };
	} catch (err) {
		toLoadError(err);
	}
};
