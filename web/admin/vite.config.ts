import { sveltekit } from '@sveltejs/kit/vite';
import { svelteTesting } from '@testing-library/svelte/vite';
// vitest/config re-exports Vite's defineConfig with the `test` block typed;
// importing it from 'vite' makes svelte-check reject `test` as unknown.
import { defineConfig } from 'vitest/config';

export default defineConfig({
	plugins: [sveltekit()],
	server: {
		// Dev-mode same-origin proxy to the Go server (spec 6.4). No
		// changeOrigin: it would rewrite Host while the browser Origin stays
		// the Vite origin, and the API's cross-site guard would 403 every
		// write.
		//
		// The object form is REQUIRED. Vite expands the string shorthand
		// ('/api': 'http://localhost:8080') to { target, changeOrigin: true },
		// so the shorthand turns on exactly what this comment says is off --
		// and Vite's rewriteOriginHeader only fires for rewriteWsOrigin, so
		// Origin is never fixed up to match. Browsers survive it because the
		// guard checks Sec-Fetch-Site first; curl and embedded webviews get a
		// 403 on every write.
		proxy: { '/api': { target: 'http://localhost:8080', changeOrigin: false } },
	},
	test: {
		projects: [
			{
				// Plain modules: $lib/alerts, $lib/api, $lib/datetime,
				// $lib/loaderror, $lib/redirect, $lib/regions and the layout
				// guard. No DOM needed -- fetch, Response and Intl all come from
				// Node -- and no jsdom means these stay fast.
				extends: true,
				test: {
					name: 'node',
					environment: 'node',
					include: ['src/**/*.test.ts'],
					exclude: ['src/**/*.svelte.test.ts'],
				},
			},
			{
				// Component behaviour that genuinely lives in markup and cannot
				// be lifted into a module: the edit form's remount-on-save rule
				// and AlertForm's zoned/zoneless field switch. Both were bugs
				// found by hand in a browser, and without a DOM project either
				// one can be reverted with the whole suite still green.
				extends: true,
				plugins: [svelteTesting()],
				test: {
					name: 'component',
					environment: 'jsdom',
					include: ['src/**/*.svelte.test.ts'],
					setupFiles: ['./vitest-setup-client.ts'],
				},
			},
		],
	},
});
