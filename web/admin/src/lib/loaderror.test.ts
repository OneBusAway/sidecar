import { describe, expect, it, vi } from 'vitest';
import { isHttpError, isRedirect, redirect } from '@sveltejs/kit';

// $app/navigation and $app/paths only work inside a running SvelteKit client;
// $lib/api pulls them in for its 401 redirect.
vi.mock('$app/navigation', () => ({ goto: vi.fn() }));
vi.mock('$app/paths', () => ({
	resolve: (path: string) => `/admin${path === '/' ? '' : path}`,
}));

import { ApiError } from './api';
import { toLoadError } from './loaderror';

function thrownBy(fn: () => never): unknown {
	try {
		fn();
	} catch (err) {
		return err;
	}
	throw new Error('expected a throw');
}

describe('toLoadError', () => {
	// Without the conversion this is an unhandled exception, and SvelteKit
	// renders "500 / Internal Error" for it -- the same screen a mistyped id, a
	// deleted alert and an unopenable database would all get.
	it('turns a 404 into an HTTP error carrying the status and message', () => {
		const err = thrownBy(() =>
			toLoadError(new ApiError(404, 'alert not found')),
		);
		expect(isHttpError(err)).toBe(true);
		expect((err as { status: number }).status).toBe(404);
		expect((err as { body: { message: string } }).body.message).toBe(
			'alert not found',
		);
	});

	it('keeps a 400 as a 400 with the server wording', () => {
		const err = thrownBy(() =>
			toLoadError(new ApiError(400, 'invalid id "abc": must be an integer')),
		);
		expect((err as { status: number }).status).toBe(400);
		expect((err as { body: { message: string } }).body.message).toBe(
			'invalid id "abc": must be an integer',
		);
	});

	// The api client has already started navigating to the login page; turning
	// the 401 into an error page would replace that redirect with a dead end.
	it('rethrows a 401 untouched so the login redirect wins', () => {
		const original = new ApiError(401, 'authentication required');
		const err = thrownBy(() => toLoadError(original));
		expect(err).toBe(original);
		expect(isHttpError(err)).toBe(false);
	});

	it('rethrows a non-ApiError untouched', () => {
		const original = new TypeError('fetch failed');
		expect(thrownBy(() => toLoadError(original))).toBe(original);
	});

	// A redirect thrown by SvelteKit itself must pass through, or a guarded
	// load would render an error page instead of redirecting.
	//
	// The redirect is produced by calling the real `redirect()` rather than
	// hand-written as `{ status, location }`: isRedirect() checks for an actual
	// Redirect instance, so the hand-written stand-in is not one, and a test
	// built on it would assert against a shape SvelteKit never produces.
	it('rethrows a SvelteKit redirect untouched', () => {
		const original = thrownBy(() => redirect(307, '/admin/login'));
		expect(isRedirect(original as never)).toBe(true);
		expect(thrownBy(() => toLoadError(original))).toBe(original);
	});
});
