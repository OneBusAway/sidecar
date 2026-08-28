// Deliberately NOT named +page.test.ts: a leading `+` would make SvelteKit
// treat this file as a route (see layout.guard.test.ts for the same note).
//
// pickRegion itself is covered in lib/regions.test.ts; what is only covered
// here is that this load actually wires it to a redirect, to the right
// destination, and returns the region list to render otherwise -- which is
// the part a test asserting only "goto was called" would miss (see task
// notes: assert the destination, not that navigation happened at all).
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { isRedirect } from '@sveltejs/kit';

vi.mock('$app/paths', () => ({
	resolve: (p: string, params?: Record<string, string>) => {
		const path = params ? p.replace('[region]', params.region) : p;
		return `/admin${path === '/' ? '' : path}`;
	},
}));

const { getMock } = vi.hoisted(() => ({ getMock: vi.fn() }));
vi.mock('$lib/api', async () => {
	const actual = await vi.importActual<typeof import('$lib/api')>('$lib/api');
	return { ...actual, api: { ...actual.api, get: getMock } };
});

import type { Region } from '$lib/types';
import { load } from './+page';

type LoadResult = { regions: Region[] };

const call = () =>
	(
		load as unknown as (e: {
			parent: () => Promise<void>;
		}) => Promise<LoadResult>
	)({ parent: () => Promise.resolve() });

function region(over: Partial<Region>): Region {
	return {
		id: 0,
		name: 'Tampa Bay',
		oba_base_url: '',
		sidecar_base_url: '',
		language: 'en',
		active: true,
		default_agency_id: 'HART',
		timezone: '',
		latitude: null,
		longitude: null,
		oba_api_key: 'none',
		...over,
	};
}

const tampa = region({ id: 0, name: 'Tampa Bay' });
const puget = region({ id: 5, name: 'Puget Sound' });

// A localStorage stand-in. Assigned to globalThis directly rather than via
// vi.stubGlobal so the store persists across the get/set calls a single test
// makes -- vi.stubGlobal would otherwise need its own backing object anyway.
function stubStorage(initial: Record<string, string> = {}) {
	const store = { ...initial };
	vi.stubGlobal('localStorage', {
		getItem: (k: string) => store[k] ?? null,
		setItem: (k: string, v: string) => {
			store[k] = v;
		},
	});
}

beforeEach(() => {
	getMock.mockReset();
});

afterEach(() => {
	vi.unstubAllGlobals();
});

describe('the admin root', () => {
	// Region 0 is Tampa Bay: redirecting to '/regions//alerts' (a truthiness
	// bug on the id) would be a broken link, not merely a wrong one.
	it('redirects to the only region, by id, when there is exactly one', async () => {
		stubStorage();
		getMock.mockResolvedValue([tampa]);

		const result = await call().catch((e: unknown) => e);
		expect(isRedirect(result)).toBe(true);
		expect((result as { location: string }).location).toBe(
			'/admin/regions/0/alerts',
		);
	});

	it('redirects to a remembered region among several', async () => {
		stubStorage({ 'sidecar.lastRegion': '5' });
		getMock.mockResolvedValue([tampa, puget]);

		const result = await call().catch((e: unknown) => e);
		expect(isRedirect(result)).toBe(true);
		expect((result as { location: string }).location).toBe(
			'/admin/regions/5/alerts',
		);
	});

	it('renders the picker instead of redirecting with nothing remembered', async () => {
		stubStorage();
		getMock.mockResolvedValue([tampa, puget]);

		await expect(call()).resolves.toEqual({ regions: [tampa, puget] });
	});

	// localStorage throws outright in some privacy modes. That must not take
	// down the one screen that exists to recover from having no region.
	it('renders the picker rather than throwing when localStorage is unavailable', async () => {
		vi.stubGlobal('localStorage', {
			getItem: () => {
				throw new Error('SecurityError');
			},
		});
		getMock.mockResolvedValue([tampa, puget]);

		await expect(call()).resolves.toEqual({ regions: [tampa, puget] });
	});

	it('propagates a server failure as a load error', async () => {
		const { ApiError } = await import('$lib/api');
		stubStorage();
		getMock.mockRejectedValue(new ApiError(500, 'database is locked'));

		await expect(call()).rejects.toMatchObject({ status: 500 });
	});
});
