<script lang="ts">
	import { goto } from '$app/navigation';
	import { resolve } from '$app/paths';
	import { api, ApiError } from '$lib/api';
	import type { LayoutProps } from './$types';

	let { data, children }: LayoutProps = $props();

	let signingOut = $state(false);
	let signOutError = $state('');

	async function signOut() {
		signingOut = true;
		signOutError = '';
		try {
			await api.del('/session');
		} catch (err) {
			// Logout is idempotent server-side (a missing cookie is still a
			// 204), so a failure here means the session row is probably still
			// live. Say so instead of showing a reassuring login screen.
			signOutError = err instanceof ApiError ? err.message : 'sign out failed';
			return;
		} finally {
			signingOut = false;
		}
		await goto(resolve('/login'));
	}
</script>

<header>
	{#if data.user}
		<a class="brand" href={resolve('/')}>Sidecar Admin</a>
		<nav>
			<a href={resolve('/')}>Alerts</a>
			<a href={resolve('/regions')}>Regions</a>
		</nav>
		<div class="account">
			<span class="who">{data.user.username}</span>
			<button type="button" onclick={signOut} disabled={signingOut}
				>Sign out</button
			>
		</div>
	{:else}
		<span class="brand">Sidecar Admin</span>
	{/if}
</header>

<main>
	{#if signOutError}
		<p class="error" role="alert">{signOutError}</p>
	{/if}
	{#if data.sessionError}
		<!--
			The session probe failed with something other than a 401. This cannot
			be shown on +error.svelte: that page lives inside this layout, so a
			root-layout failure falls through to SvelteKit's built-in error page
			instead. Reporting it here is the only way the operator sees the
			server's own words and keeps the shell.
		-->
		<p class="error" role="alert">
			Could not check your session: {data.sessionError}. You are seeing this
			page without a confirmed sign-in.
			<button type="button" class="link" onclick={() => location.reload()}>
				Reload
			</button>
		</p>
	{/if}
	{@render children()}
</main>

<style>
	:global(body) {
		margin: 0;
		font-family: system-ui, sans-serif;
		color: #14181c;
		background: #f6f7f9;
	}

	header {
		display: flex;
		align-items: center;
		gap: 1.5rem;
		padding: 0.75rem 1.25rem;
		background: #14181c;
		color: #f6f7f9;
	}

	.brand {
		font-weight: 700;
		letter-spacing: 0.01em;
	}

	nav {
		display: flex;
		gap: 1rem;
	}

	header a {
		color: inherit;
		text-decoration: none;
	}

	header a:hover,
	header a:focus-visible {
		text-decoration: underline;
	}

	.account {
		display: flex;
		align-items: center;
		gap: 0.75rem;
		margin-left: auto;
	}

	.who {
		opacity: 0.8;
	}

	.account button {
		font: inherit;
		padding: 0.3rem 0.7rem;
		border: 1px solid #6c757d;
		border-radius: 4px;
		background: transparent;
		color: inherit;
		cursor: pointer;
	}

	.account button:disabled {
		cursor: default;
		opacity: 0.6;
	}

	main {
		max-width: 60rem;
		margin: 0 auto;
		padding: 1.5rem 1.25rem 4rem;
	}

	.error {
		padding: 0.6rem 0.8rem;
		border: 1px solid #b3261e;
		border-radius: 4px;
		background: #fdecea;
		color: #7f1d1a;
	}

	button.link {
		font: inherit;
		padding: 0;
		border: none;
		background: none;
		color: inherit;
		text-decoration: underline;
		cursor: pointer;
	}
</style>
