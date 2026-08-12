import { describe, expect, it } from 'vitest';
import { safeRedirect } from './redirect';

// What resolve('/') actually returns with paths.base = '/admin': the base plus
// a trailing slash. Verified in the browser -- the layout's home link renders
// as href="/admin/". Testing with '/admin' instead hid a bug where every deep
// redirectTo was discarded.
const home = '/admin/';

describe('safeRedirect', () => {
	it('keeps a path inside the app', () => {
		expect(safeRedirect('/admin/alerts/7?tab=translations', home)).toBe(
			'/admin/alerts/7?tab=translations',
		);
		expect(safeRedirect('/admin/regions', home)).toBe('/admin/regions');
		expect(safeRedirect('/admin?filter=published', home)).toBe(
			'/admin?filter=published',
		);
	});

	it('accepts the base itself, with or without the trailing slash', () => {
		expect(safeRedirect('/admin', home)).toBe('/admin');
		expect(safeRedirect('/admin/', home)).toBe('/admin/');
	});

	it('works the same when home has no trailing slash', () => {
		expect(safeRedirect('/admin/regions', '/admin')).toBe('/admin/regions');
		expect(safeRedirect('/elsewhere', '/admin')).toBe('/admin');
	});

	it('falls back home when there is no target', () => {
		expect(safeRedirect(null, home)).toBe(home);
		expect(safeRedirect(undefined, home)).toBe(home);
		expect(safeRedirect('', home)).toBe(home);
	});

	it('refuses to leave the app', () => {
		expect(safeRedirect('https://evil.example/admin/', home)).toBe(home);
		expect(safeRedirect('//evil.example/admin/', home)).toBe(home);
		expect(safeRedirect('/\\evil.example', home)).toBe(home);
		expect(safeRedirect('http://localhost:8080/admin/', home)).toBe(home);
	});

	it('refuses a path outside the base, including one that merely shares its prefix', () => {
		expect(safeRedirect('/api/admin/v1/alerts', home)).toBe(home);
		expect(safeRedirect('/adminfoo/bar', home)).toBe(home);
		expect(safeRedirect('/', home)).toBe(home);
	});

	it('still refuses to change origin when the app is mounted at the root', () => {
		expect(safeRedirect('/alerts/7', '/')).toBe('/alerts/7');
		expect(safeRedirect('//evil.example', '/')).toBe('/');
		expect(safeRedirect('/\\evil.example', '/')).toBe('/');
		expect(safeRedirect('https://evil.example', '/')).toBe('/');
	});
});
