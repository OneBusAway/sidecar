import { api, regionPath } from '$lib/api';
import { toLoadError } from '$lib/loaderror';
import { loadAudience, loadPushes } from '$lib/pushes';
import { hasFeature, rememberRegion } from '$lib/regions';
import type { Alert, AlertPush, PushAudience, Region } from '$lib/types';
import type { PageLoad } from './$types';

export const load: PageLoad = async ({ parent, params }) => {
	// See the alerts list load: the session guard runs first so a signed-out
	// visitor never fires a request that can only 401.
	await parent();
	try {
		const [alert, region] = await Promise.all([
			// The single-alert endpoint, which is the ONLY one that populates
			// translations -- GET /alerts returns `translations: []` on every
			// item.
			api.get<Alert>(regionPath(params.region, `/alerts/${params.id}`)),
			// The single-region endpoint: the only one that carries `features`.
			api.get<Region>(regionPath(params.region, '')),
		]);
		rememberRegion(region.id);

		// Pushes are fetched only when this deployment registered the family.
		// That is now the "not configured" signal (see hasFeature in
		// lib/regions), not a swallowed 404 from these two routes: inferring it
		// from a 404 made a genuinely deleted alert and a missing route look
		// identical. A deleted or mistyped alert id is still fatal to the
		// page -- it comes back from the alert fetch above and becomes the
		// error page via the catch below, before this ever runs.
		let pushes: AlertPush[] = [];
		let audience: PushAudience | null = null;
		if (hasFeature(region, 'pushes')) {
			[pushes, audience] = await Promise.all([
				loadPushes(params.region, params.id),
				loadAudience(params.region, params.id),
			]);
		}

		return { alert, region, pushes, audience };
	} catch (err) {
		// A deleted or mistyped alert id is a 404 with "alert not found", not
		// the "500 / Internal Error" an unconverted throw would produce.
		toLoadError(err);
	}
};
