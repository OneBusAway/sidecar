<script lang="ts">
	import { goto } from '$app/navigation';
	import { resolve } from '$app/paths';
	import { page } from '$app/state';
	import { api, ApiError } from '$lib/api';
	import { safeRedirect } from '$lib/redirect';

	let username = $state('');
	let password = $state('');
	let error = $state('');
	let busy = $state(false);

	async function submit(e: SubmitEvent) {
		e.preventDefault();
		busy = true;
		error = '';
		try {
			await api.post('/session', { username, password });
		} catch (err) {
			// The server answers every bad sign-in identically ("invalid
			// credentials") on purpose, so there is nothing to add here.
			error = err instanceof ApiError ? err.message : 'sign in failed';
			return;
		} finally {
			busy = false;
		}
		// ?redirectTo= reaches us through the address bar, so it is untrusted;
		// safeRedirect returns either resolve('/') or a path already under it,
		// so the destination is always inside this app. goto() also only ever
		// takes a relative path -- never location.href = -- so nothing here can
		// be turned into an open redirect off-site.
		const dest = safeRedirect(
			page.url.searchParams.get('redirectTo'),
			resolve('/'),
		);
		// The lint rule below only recognises a literal resolve() call as an
		// argument; it cannot see that every value safeRedirect returns came
		// from one.
		// eslint-disable-next-line svelte/no-navigation-without-resolve
		await goto(dest);
	}
</script>

<h1>Sign in</h1>

<form onsubmit={submit}>
	<label>
		Username
		<input bind:value={username} autocomplete="username" required />
	</label>
	<label>
		Password
		<input
			type="password"
			bind:value={password}
			autocomplete="current-password"
			required
		/>
	</label>
	{#if error}<p class="error" role="alert">{error}</p>{/if}
	<button disabled={busy}>Sign in</button>
</form>

<style>
	form {
		display: flex;
		flex-direction: column;
		gap: 1rem;
		max-width: 22rem;
	}

	label {
		display: flex;
		flex-direction: column;
		gap: 0.3rem;
		font-weight: 600;
	}

	input {
		font: inherit;
		font-weight: 400;
		padding: 0.45rem 0.6rem;
		border: 1px solid #b8c0c8;
		border-radius: 4px;
	}

	button {
		font: inherit;
		align-self: flex-start;
		padding: 0.45rem 1.1rem;
		border: 1px solid #14181c;
		border-radius: 4px;
		background: #14181c;
		color: #f6f7f9;
		cursor: pointer;
	}

	button:disabled {
		cursor: default;
		opacity: 0.6;
	}

	.error {
		margin: 0;
		padding: 0.6rem 0.8rem;
		border: 1px solid #b3261e;
		border-radius: 4px;
		background: #fdecea;
		color: #7f1d1a;
	}
</style>
