import { api } from '$lib/api';
import { toLoadError } from '$lib/loaderror';
import type { Alert, Region } from '$lib/types';
import type { PageLoad } from './$types';

export const load: PageLoad = async ({ parent }) => {
	// Wait for the root layout's session guard before fetching anything. Loads
	// otherwise run in parallel, so a signed-out visitor would fire these
	// requests, collect a 401 from each, and race the guard's redirect with the
	// api client's own -- two navigations for one expired cookie.
	await parent();
	try {
		const [alerts, regions] = await Promise.all([
			api.get<Alert[]>('/alerts'),
			api.get<Region[]>('/regions'),
		]);
		return { alerts, regions };
	} catch (err) {
		// Keeps the server's status and wording on the error page instead of a
		// blanket "500 / Internal Error".
		toLoadError(err);
	}
};
