import { redirect } from '@sveltejs/kit';
import { resolve } from '$app/paths';
import { api } from '$lib/api';
import { toLoadError } from '$lib/loaderror';
import { LAST_REGION_KEY, pickRegion } from '$lib/regions';
import type { Region } from '$lib/types';
import type { PageLoad } from './$types';

/**
 * rememberedRegion reads the last region an operator successfully reached.
 *
 * Guarded: localStorage throws outright in some privacy modes (Safari
 * private browsing, a cookies-disabled profile), and a remembered region is
 * a convenience -- it must never take down the one screen that exists to
 * recover from having no region at all.
 */
function rememberedRegion(): string | null {
	try {
		return localStorage.getItem(LAST_REGION_KEY);
	} catch {
		return null;
	}
}

// This is the admin root: every other screen needs a region in its URL (see
// design spec §2.5), so this load either forwards straight past the picker
// -- one reachable region, or a remembered one still in the list -- or hands
// the picker the region list to render as links. `pickRegion` owns that
// decision; this load is just wiring it to a redirect.
export const load: PageLoad = async ({ parent }) => {
	// The session guard first, as before: a signed-out visitor must not fire
	// a request that can only 401.
	await parent();
	try {
		const regions = await api.get<Region[]>('/regions');
		const target = pickRegion(regions, rememberedRegion());
		if (target !== null) {
			redirect(
				307,
				resolve('/regions/[region]/alerts', { region: String(target.id) }),
			);
		}
		return { regions };
	} catch (err) {
		toLoadError(err);
	}
};
