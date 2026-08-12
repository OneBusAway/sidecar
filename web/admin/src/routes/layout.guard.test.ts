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
vi.mock('$lib/api', () => ({ whoami: whoamiMock }));
vi.mock('$app/paths', () => ({
	resolve: (p: string) => `/admin${p === '/' ? '' : p}`,
}));

import { load } from './+layout';

const call = (path: string, search = '') =>
	(load as unknown as (e: { url: URL }) => Promise<{ user: unknown }>)({
		url: new URL(`http://host${path}${search}`),
	});

beforeEach(() => whoamiMock.mockReset());

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
		await expect(call('/admin/login')).resolves.toEqual({ user: null });
	});

	it('lets a signed-in operator through and publishes the user', async () => {
		whoamiMock.mockResolvedValue({ username: 'admin' });
		await expect(call('/admin/regions')).resolves.toEqual({
			user: { username: 'admin' },
		});
	});
});
