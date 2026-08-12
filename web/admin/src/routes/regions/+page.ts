import { api } from '$lib/api';
import { toLoadError } from '$lib/loaderror';
import type { Region } from '$lib/types';
import type { PageLoad } from './$types';

export const load: PageLoad = async ({ parent }) => {
	// See the alerts list load: the session guard runs first so a signed-out
	// visitor never fires a request that can only 401.
	await parent();
	try {
		return { regions: await api.get<Region[]>('/regions') };
	} catch (err) {
		toLoadError(err);
	}
};
