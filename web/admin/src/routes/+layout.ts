import { resolve } from '$app/paths';
import { redirect } from '@sveltejs/kit';
import { ApiError, whoami } from '$lib/api';
import type { SessionUser } from '$lib/types';
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
//
// Nothing but a redirect may be thrown out of THIS load. `src/routes/+error.svelte`
// lives inside this layout, so it cannot catch an error the layout itself
// raised: SvelteKit falls back to its built-in error page, which is a bare
// `<h1>Internal Error</h1>` with no shell, no nav, no way back, and no trace of
// the server's message. (`src/error.html` is no help either -- there is no SSR
// here, so that template is never rendered.) A failed session probe is
// therefore CAUGHT and handed to the shell as data, which +layout.svelte shows
// as a banner.
export const load: LayoutLoad = async ({ url }) => {
	let user: SessionUser | null = null;
	let sessionError = '';
	try {
		user = await whoami();
	} catch (err) {
		// whoami() already turns a 401 into `null`, so anything arriving here is
		// a real fault -- a 500, a dropped connection, an unreadable database.
		// A non-ApiError is a bug in this app rather than a server condition,
		// and has no message worth showing an operator, so it still throws.
		if (!(err instanceof ApiError)) throw err;
		sessionError = err.message;
	}

	const onLogin = url.pathname === resolve('/login');
	// Only a KNOWN-signed-out visitor is redirected. Bouncing to the login page
	// because the session endpoint returned a 500 would send the operator to a
	// form that cannot possibly succeed and hide the actual fault.
	if (!user && sessionError === '' && !onLogin) {
		const dest = encodeURIComponent(url.pathname + url.search);
		redirect(307, `${resolve('/login')}?redirectTo=${dest}`);
	}
	return { user, sessionError };
};
