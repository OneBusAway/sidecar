import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

// $app/navigation and $app/paths only work inside a running SvelteKit client,
// so they are stubbed. resolve() is stubbed to do exactly what the real one
// does with `paths.base = '/admin'` (see svelte.config.js): prefix the base.
const { gotoMock } = vi.hoisted(() => ({ gotoMock: vi.fn() }));
vi.mock('$app/navigation', () => ({ goto: gotoMock }));
vi.mock('$app/paths', () => ({
	resolve: (path: string) => `/admin${path === '/' ? '' : path}`,
}));

// vi.mock is hoisted above these imports by Vitest, so ./api sees the stubs.
import { api, ApiError, whoami } from './api';

const fetchMock = vi.fn<typeof fetch>();

function jsonResponse(status: number, body: unknown): Response {
	return new Response(JSON.stringify(body), {
		status,
		headers: { 'Content-Type': 'application/json' },
	});
}

/** The RequestInit the code under test handed to fetch on its Nth call. */
function initOf(call = 0): RequestInit {
	const args = fetchMock.mock.calls[call];
	expect(args, `fetch call ${call}`).toBeDefined();
	return args[1] ?? {};
}

beforeEach(() => {
	gotoMock.mockReset();
	fetchMock.mockReset();
	vi.stubGlobal('fetch', fetchMock);
	vi.stubGlobal('location', { pathname: '/admin', search: '' });
});

afterEach(() => {
	vi.unstubAllGlobals();
});

describe('request', () => {
	it('posts JSON to the versioned admin path and returns the parsed body', async () => {
		fetchMock.mockResolvedValue(jsonResponse(201, { id: 7 }));

		const created = await api.post<{ id: number }>('/alerts', {
			header: 'Bridge out',
		});

		expect(created).toEqual({ id: 7 });
		expect(fetchMock.mock.calls[0][0]).toBe('/api/admin/v1/alerts');
		const init = initOf();
		expect(init.method).toBe('POST');
		expect(init.headers).toEqual({ 'Content-Type': 'application/json' });
		expect(init.body).toBe('{"header":"Bridge out"}');
	});

	it('sends no body and no Content-Type on a bodyless request', async () => {
		fetchMock.mockResolvedValue(jsonResponse(200, []));

		await api.get('/alerts');

		const init = initOf();
		expect(init.method).toBe('GET');
		expect(init.body).toBeUndefined();
		expect(init.headers).toBeUndefined();
		// The API is same-origin and its cross-site guard depends on staying
		// that way. Naming these keeps the "nothing here is CORS" comment in
		// api.ts honest: setting either would otherwise pass every test.
		expect(init.mode).toBeUndefined();
		expect(init.credentials).toBeUndefined();
	});

	it('uses the method matching the helper', async () => {
		// A fresh Response per call: a Response body can only be read once.
		fetchMock.mockImplementation(() => Promise.resolve(jsonResponse(200, {})));
		await api.patch('/alerts/1', {});
		await api.put('/alerts/1/translations/es', {});
		expect(initOf(0).method).toBe('PATCH');
		expect(initOf(1).method).toBe('PUT');
	});

	it('resolves to undefined on 204 rather than choking on an empty body', async () => {
		fetchMock.mockResolvedValue(new Response(null, { status: 204 }));

		await expect(api.del('/alerts/1')).resolves.toBeUndefined();
	});

	it("surfaces the server's error message", async () => {
		fetchMock.mockResolvedValue(
			jsonResponse(400, {
				error: 'start_time must include a UTC offset (region timezone: UTC)',
			}),
		);

		await expect(api.post('/alerts', {})).rejects.toMatchObject({
			status: 400,
			message: 'start_time must include a UTC offset (region timezone: UTC)',
		});
	});

	it('falls back to the status line when the error body is not JSON', async () => {
		fetchMock.mockResolvedValue(
			new Response('<html>gateway</html>', {
				status: 502,
				statusText: 'Bad Gateway',
			}),
		);

		const err = await api.get('/alerts').catch((e: unknown) => e);
		expect(err).toBeInstanceOf(ApiError);
		expect(err).toMatchObject({ status: 502, message: '502 Bad Gateway' });
	});
});

describe('401 handling', () => {
	it('redirects to login with a way back when the session expires mid-session', async () => {
		vi.stubGlobal('location', {
			pathname: '/admin/alerts/7',
			search: '?tab=translations',
		});
		fetchMock.mockResolvedValue(
			jsonResponse(401, { error: 'authentication required' }),
		);

		await expect(api.get('/alerts/7')).rejects.toBeInstanceOf(ApiError);

		expect(gotoMock).toHaveBeenCalledTimes(1);
		expect(gotoMock).toHaveBeenCalledWith(
			'/admin/login?redirectTo=%2Fadmin%2Falerts%2F7%3Ftab%3Dtranslations',
		);
	});

	it('does not redirect when the 401 comes from the session endpoint itself', async () => {
		vi.stubGlobal('location', { pathname: '/admin/login', search: '' });
		fetchMock.mockResolvedValue(
			jsonResponse(401, { error: 'invalid credentials' }),
		);

		await expect(
			api.post('/session', { username: 'a', password: 'b' }),
		).rejects.toMatchObject({
			status: 401,
			message: 'invalid credentials',
		});

		expect(gotoMock).not.toHaveBeenCalled();
	});
});

describe('whoami', () => {
	it('returns the signed-in operator', async () => {
		fetchMock.mockResolvedValue(jsonResponse(200, { username: 'admin' }));

		await expect(whoami()).resolves.toEqual({ username: 'admin' });
		expect(fetchMock.mock.calls[0][0]).toBe('/api/admin/v1/session');
	});

	it('returns null on 401 without bouncing the caller anywhere', async () => {
		fetchMock.mockResolvedValue(
			jsonResponse(401, { error: 'authentication required' }),
		);

		await expect(whoami()).resolves.toBeNull();
		expect(gotoMock).not.toHaveBeenCalled();
	});

	it('rethrows a server failure instead of reporting it as signed out', async () => {
		fetchMock.mockResolvedValue(jsonResponse(500, { error: 'internal error' }));

		await expect(whoami()).rejects.toMatchObject({
			status: 500,
			message: 'internal error',
		});
	});
});
