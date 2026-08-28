<script lang="ts">
	import { resolve } from '$app/paths';
	import type { PageProps } from './$types';

	let { data }: PageProps = $props();
</script>

<!--
	Reached only when the load could not pick a region on its own: several
	reachable regions with nothing remembered (or a remembered one no longer
	in the list). One region, or a remembered one still in the list, forwards
	straight past this page (see +page.ts) -- an operator who can only reach
	one region has no real choice to make here.
-->
<h1>Choose a region</h1>

{#if data.regions.length === 0}
	<p class="empty">No regions are configured yet.</p>
{:else}
	<ul class="regions">
		{#each data.regions as r (r.id)}
			<li>
				<a href={resolve('/regions/[region]/alerts', { region: String(r.id) })}>
					{r.name} (#{r.id})
				</a>
			</li>
		{/each}
	</ul>
{/if}

<style>
	.regions {
		list-style: none;
		margin: 0;
		padding: 0;
		display: flex;
		flex-direction: column;
		gap: 0.5rem;
		max-width: 24rem;
	}

	.regions a {
		display: block;
		padding: 0.55rem 0.8rem;
		border: 1px solid #dde2e6;
		border-radius: 4px;
		background: #fff;
		text-decoration: none;
		color: inherit;
	}

	.regions a:hover,
	.regions a:focus-visible {
		border-color: #14181c;
	}

	.empty {
		color: #4a5560;
	}
</style>
