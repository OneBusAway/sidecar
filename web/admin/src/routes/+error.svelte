<script lang="ts">
	// Every load failure lands here, and the ones that matter are not 404s:
	// whoami() rethrows anything that is not a 401, so a 500 on the session
	// probe -- a database the server cannot read, say -- reaches this page.
	// Without it that shows as SvelteKit's unstyled default error screen, with
	// no app shell and no way back.
	import { resolve } from '$app/paths';
	import { page } from '$app/state';
</script>

<h1>{page.status}</h1>

<!-- page.error.message is server text or a framework string; Svelte escapes
     it on the way in, and {@html} is banned precisely so it stays that way. -->
<p class="error" role="alert">
	{page.error?.message ?? 'Something went wrong.'}
</p>

<!--
	The follow-up line has to match the status, or it contradicts the message
	right above it: telling an operator "the admin API did not answer" under a
	400 the API just produced sends them to the server logs for a problem that
	is in their URL.
-->
{#if page.status === 404}
	<p>That page does not exist.</p>
{:else if page.status < 500}
	<p>The admin API rejected the request. Its reason is above.</p>
{:else}
	<p>
		The admin API did not answer. Reloading is worth a try; if it keeps failing,
		the server logs will say why.
	</p>
{/if}

<p><a href={resolve('/')}>Back to alerts</a></p>

<style>
	h1 {
		margin-bottom: 0.5rem;
	}

	.error {
		max-width: 44rem;
		padding: 0.6rem 0.8rem;
		border: 1px solid #b3261e;
		border-radius: 4px;
		background: #fdecea;
		color: #7f1d1a;
	}
</style>
