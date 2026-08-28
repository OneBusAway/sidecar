import { goto } from '$app/navigation';
import { resolve } from '$app/paths';
import type { SessionUser } from './types';

/**
 * ApiError carries the HTTP status alongside the server's `{"error": "..."}`
 * message, so callers can branch on the status (404 -> "gone", 409 -> "someone
 * else edited this") while still showing the operator the server's wording.
 */
export class ApiError extends Error {
	constructor(
		public status: number,
		message: string,
	) {
		super(message);
		this.name = 'ApiError';
	}
}

async function request<T>(
	method: string,
	path: string,
	body?: unknown,
): Promise<T> {
	// Same-origin, always: the SPA is served by the same Go binary as the API,
	// and the API's cross-site guard depends on it. Nothing here is CORS.
	const res = await fetch(`/api/admin/v1${path}`, {
		method,
		headers:
			body === undefined ? undefined : { 'Content-Type': 'application/json' },
		body: body === undefined ? undefined : JSON.stringify(body),
	});
	// A 401 anywhere but the session endpoints means the cookie expired mid-
	// session; bounce to login with a way back. The session endpoints are
	// excluded because a failed sign-in is an answer, not a lost session --
	// redirecting there would loop the login page onto itself.
	if (res.status === 401 && !path.startsWith('/session')) {
		const dest = location.pathname + location.search;
		// The query string goes through resolve() rather than being appended
		// to its result: same output, but it keeps the base path in exactly
		// one place and lets svelte/no-navigation-without-resolve see it.
		void goto(resolve(`/login?redirectTo=${encodeURIComponent(dest)}`));
		throw new ApiError(401, 'authentication required');
	}
	if (!res.ok) {
		let msg = `${res.status} ${res.statusText}`;
		try {
			const parsed = (await res.json()) as { error?: string };
			if (parsed.error) msg = parsed.error;
		} catch {
			// non-JSON error body; keep the status text
		}
		throw new ApiError(res.status, msg);
	}
	if (res.status === 204) return undefined as T;
	return (await res.json()) as T;
}

export const api = {
	get: <T>(path: string) => request<T>('GET', path),
	post: <T>(path: string, body?: unknown) => request<T>('POST', path, body),
	patch: <T>(path: string, body: unknown) => request<T>('PATCH', path, body),
	put: <T>(path: string, body: unknown) => request<T>('PUT', path, body),
	del: (path: string) => request<void>('DELETE', path),
};

/**
 * regionPath prefixes a region-scoped admin API path.
 *
 * Region 0 is Tampa Bay, a real region, so the id is interpolated
 * unconditionally -- a truthiness test on it would emit '/regions//alerts'
 * for Tampa and quietly send every request there to the wrong route.
 */
export function regionPath(region: string | number, path: string): string {
	return `/regions/${region}${path}`;
}

/**
 * whoami reports the signed-in operator, or null when there is no session.
 *
 * Only a 401 means "signed out". Any other failure is rethrown: reporting a
 * 500 or a dropped connection as "logged out" would bounce the operator to a
 * login form that cannot possibly succeed, hiding the real fault.
 */
export async function whoami(): Promise<SessionUser | null> {
	try {
		return await api.get<SessionUser>('/session');
	} catch (err) {
		if (err instanceof ApiError && err.status === 401) return null;
		throw err;
	}
}
