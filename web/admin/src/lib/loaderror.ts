import { error } from '@sveltejs/kit';
import { ApiError } from './api';

/**
 * toLoadError converts a failed admin API call inside a `load` into a
 * SvelteKit error that keeps the status and the server's wording.
 *
 * An ApiError thrown straight out of a load is just an unhandled exception as
 * far as SvelteKit is concerned, so the error page reports "500 / Internal
 * Error" -- the same screen for a mistyped alert id, a deleted alert and a
 * database that will not open. Passing it through `error()` puts the real
 * status and the server's own message on screen instead.
 *
 * 401 is deliberately NOT converted: the api client has already started a
 * navigation to the login page by the time it throws, and rendering "401
 * authentication required" over the top of that would replace a working
 * redirect with a dead end.
 */
export function toLoadError(err: unknown): never {
	if (err instanceof ApiError && err.status !== 401) {
		error(err.status, err.message);
	}
	throw err;
}
