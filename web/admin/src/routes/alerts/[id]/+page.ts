import { api } from '$lib/api';
import { toLoadError } from '$lib/loaderror';
import { loadAudience, loadPushes } from '$lib/pushes';
import type { Alert, Region } from '$lib/types';
import type { PageLoad } from './$types';

export const load: PageLoad = async ({ parent, params }) => {
	// See the alerts list load: the session guard runs first so a signed-out
	// visitor never fires a request that can only 401.
	await parent();
	try {
		const [alert, regions, pushes, audience] = await Promise.all([
			// The single-alert endpoint, which is the ONLY one that populates
			// translations -- GET /alerts returns `translations: []` on every
			// item.
			api.get<Alert>(`/alerts/${params.id}`),
			api.get<Region[]>('/regions'),
			// Both swallow a 404, which on these two routes means the server
			// has no push transport and never registered them. A 404 because
			// the ALERT is missing is still fatal: it comes back from the
			// first request above and becomes the error page.
			loadPushes(params.id),
			loadAudience(params.id),
		]);
		return { alert, regions, pushes, audience };
	} catch (err) {
		// A deleted or mistyped alert id is a 404 with "alert not found", not
		// the "500 / Internal Error" an unconverted throw would produce.
		toLoadError(err);
	}
};
