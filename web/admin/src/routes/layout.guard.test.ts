// Deliberately NOT named +layout.test.ts: a leading `+` would make SvelteKit
// treat this file as a route.
//
// This is the security boundary of the whole admin UI -- every screen hangs
// off the root layout, so this load is the only thing standing between a
// signed-out visitor and the app. Without these tests the entire guard can be
// deleted and svelte-check, eslint, prettier and vitest all stay green.
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { isRedirect } from '@sveltejs/kit';

const { whoamiMock } = vi.hoisted(() => ({ whoamiMock: vi.fn() }));
vi.mock('$app/navigation', () => ({ goto: vi.fn() }));
vi.mock('$app/paths', () => ({
	resolve: (p: string) => `/admin${p === '/' ? '' : p}`,
}));
// Only whoami is replaced. ApiError comes through as the REAL class, because
// the load under test branches on `instanceof ApiError` -- a stand-in class
// here would be a fixture that no production code path can produce, and the
// test would pass against a load that never matches anything.
vi.mock('$lib/api', async () => {
	const actual = await vi.importActual<typeof import('$lib/api')>('$lib/api');
	return { ...actual, whoami: whoamiMock };
});

import { ApiError } from '$lib/api';
import { load } from './+layout';

type LoadResult = { user: unknown; sessionError: string };

const call = (path: string, search = '') =>
	(load as unknown as (e: { url: URL }) => Promise<LoadResult>)({
		url: new URL(`http://host${path}${search}`),
	});

// The braces are load-bearing. `mockReset()` returns the mock, and an arrow
// body without braces returns it -- Vitest treats a function returned from
// beforeEach as a teardown callback and CALLS it after every test. With a
// resolving mock that is merely a wasted call; with mockRejectedValue it
// produces an unhandled rejection after the test has finished, which Vitest
// then attributes to the test as a failure. Every case below rejects.
beforeEach(() => {
	whoamiMock.mockReset();
});

describe('session guard', () => {
	it('bounces a signed-out visitor to login with a way back', async () => {
		whoamiMock.mockResolvedValue(null);
		const err = await call('/admin/regions', '?filter=active').catch(
			(e: unknown) => e,
		);
		expect(isRedirect(err)).toBe(true);
		expect((err as { location: string }).location).toBe(
			'/admin/login?redirectTo=%2Fadmin%2Fregions%3Ffilter%3Dactive',
		);
	});

	it('does not bounce the login page onto itself', async () => {
		whoamiMock.mockResolvedValue(null);
		await expect(call('/admin/login')).resolves.toEqual({
			user: null,
			sessionError: '',
		});
	});

	it('lets a signed-in operator through and publishes the user', async () => {
		whoamiMock.mockResolvedValue({ username: 'admin' });
		await expect(call('/admin/regions')).resolves.toEqual({
			user: { username: 'admin' },
			sessionError: '',
		});
	});
});

describe('session probe failure', () => {
	// This load must not throw. `src/routes/+error.svelte` lives INSIDE this
	// layout, so it cannot catch an error the layout itself raised: SvelteKit
	// falls back to its built-in page, a bare `<h1>Internal Error</h1>` with no
	// shell, no nav, no way back and no sign of the server's message. Returning
	// the message as data is what keeps the operator in the app.
	it('returns the server message as data instead of throwing', async () => {
		whoamiMock.mockRejectedValue(new ApiError(500, 'database is locked'));
		await expect(call('/admin/regions')).resolves.toEqual({
			user: null,
			sessionError: 'database is locked',
		});
	});

	// A 500 is not a sign-out. Redirecting on one would send the operator to a
	// login form that cannot possibly succeed and bury the real fault.
	it('does not redirect to login when the probe failed', async () => {
		whoamiMock.mockRejectedValue(new ApiError(500, 'database is locked'));
		const result = await call('/admin/regions').catch((e: unknown) => e);
		expect(isRedirect(result as never)).toBe(false);
	});

	it('reports a 503 the same way', async () => {
		whoamiMock.mockRejectedValue(new ApiError(503, 'service unavailable'));
		await expect(call('/admin')).resolves.toEqual({
			user: null,
			sessionError: 'service unavailable',
		});
	});

	// A non-ApiError is a defect in this app, not a server condition. It has no
	// message worth showing an operator, so it keeps throwing rather than being
	// laundered into a banner that says nothing.
	it('rethrows a non-ApiError untouched', async () => {
		const boom = new TypeError('fetch is not a function');
		whoamiMock.mockRejectedValue(boom);
		await expect(call('/admin/regions')).rejects.toBe(boom);
	});
});
