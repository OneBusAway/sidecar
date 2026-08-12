import { resolve } from '$app/paths';
import { redirect } from '@sveltejs/kit';
import { whoami } from '$lib/api';
import type { LayoutLoad } from './$types';

// Pure client-rendered SPA: no SSR (there is no Node server in production)
// and no prerendering (it conflicts with the index.html fallback).
export const ssr = false;
export const prerender = false;

// The session guard. Every screen hangs off this layout, so one check here
// covers all of them -- a screen cannot forget to opt in.
//
// Reading url.pathname also registers `url` as a dependency, so SvelteKit
// re-runs this load on every navigation. That is what makes the shell notice a
// fresh sign-in (or a sign-out) without an explicit invalidate.
export const load: LayoutLoad = async ({ url }) => {
	const user = await whoami();
	const onLogin = url.pathname === resolve('/login');
	if (!user && !onLogin) {
		const dest = encodeURIComponent(url.pathname + url.search);
		redirect(307, `${resolve('/login')}?redirectTo=${dest}`);
	}
	return { user };
};
