import { sveltekit } from '@sveltejs/kit/vite';
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
		proxy: { '/api': 'http://localhost:8080' },
	},
	test: {
		// Task 10 adds the first tests; keep `make web-check` green until then.
		passWithNoTests: true,
	},
});
