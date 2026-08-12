import js from '@eslint/js';
import ts from 'typescript-eslint';
import svelte from 'eslint-plugin-svelte';
import svelteConfig from './svelte.config.js';

export default ts.config(
	js.configs.recommended,
	...ts.configs.recommended,
	...svelte.configs.recommended,
	{
		files: ['**/*.svelte', '**/*.svelte.ts'],
		languageOptions: { parserOptions: { parser: ts.parser, svelteConfig } },
		rules: {
			// typescript-eslint turns no-undef off for .ts files because the
			// compiler already reports undefined identifiers and does it
			// correctly -- no-undef has no idea about DOM lib types, so a
			// `SubmitEvent` annotation reads as an undefined global. These
			// components are TypeScript too, so they need the same treatment;
			// svelte-check is what actually catches typos here.
			'no-undef': 'off',
		},
	},
	{ ignores: ['build/', '.svelte-kit/', 'node_modules/'] },
);
