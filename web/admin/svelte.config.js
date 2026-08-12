import adapter from '@sveltejs/adapter-static';
import { vitePreprocess } from '@sveltejs/vite-plugin-svelte';

/** @type {import('@sveltejs/kit').Config} */
const config = {
	preprocess: vitePreprocess(),
	kit: {
		// Pure SPA served by the Go binary under /admin (spec 6.1, 6.2).
		// fallback lets unknown paths client-side-route; nothing may ever
		// set prerender = true (it conflicts with the fallback).
		adapter: adapter({ fallback: 'index.html' }),
		paths: { base: '/admin' },
	},
};

export default config;
