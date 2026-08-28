<script lang="ts">
	import { resolve } from '$app/paths';
	import { alertBadges, formatInstantForRegion } from '$lib/alerts';
	import type { PageProps } from './$types';

	let { data }: PageProps = $props();

	const regionParam = $derived(String(data.region.id));
</script>

<div class="head">
	<h1>{data.region.name} alerts</h1>
	<a
		class="button"
		href={resolve('/regions/[region]/alerts/new', { region: regionParam })}
	>
		New alert
	</a>
</div>

{#if data.alerts.length === 0}
	<p class="empty">
		No alerts yet.
		<a href={resolve('/regions/[region]/alerts/new', { region: regionParam })}>
			Create one
		</a>.
	</p>
{:else}
	<table>
		<thead>
			<tr>
				<th>Header</th>
				<th>Start</th>
				<th>Status</th>
			</tr>
		</thead>
		<tbody>
			{#each data.alerts as a (a.id)}
				<tr>
					<td>
						<a
							href={resolve('/regions/[region]/alerts/[id]', {
								region: regionParam,
								id: String(a.id),
							})}
						>
							{a.header}
						</a>
					</td>
					<td>
						{formatInstantForRegion(a.start_time, data.region.timezone)}
						{#if data.region.timezone}
							<span class="zone">{data.region.timezone}</span>
						{:else}
							<!--
								No configured timezone, so the cell shows the server's
								raw UTC string rather than a wall clock in a guessed
								zone. UTC is labelled so it is not mistaken for local.
							-->
							<span class="zone">UTC</span>
						{/if}
					</td>
					<td>
						{#each alertBadges(a) as badge (badge.tone)}
							<span class="badge {badge.tone}">{badge.label}</span>
						{/each}
					</td>
				</tr>
			{/each}
		</tbody>
	</table>
{/if}

<!--
	Deliberately no translation count or "no translations" column: GET /alerts
	always returns `translations: []` on every item regardless of what the alert
	really has (see the Alert type). Rendering anything from it here would say
	"none" about every translated alert in the system.
-->

<style>
	.head {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: 1rem;
	}

	.button {
		padding: 0.45rem 1.1rem;
		border: 1px solid #14181c;
		border-radius: 4px;
		background: #14181c;
		color: #f6f7f9;
		text-decoration: none;
	}

	table {
		width: 100%;
		border-collapse: collapse;
		background: #fff;
		border: 1px solid #dde2e6;
		border-radius: 4px;
	}

	th,
	td {
		padding: 0.55rem 0.7rem;
		text-align: left;
		border-bottom: 1px solid #eceff1;
		vertical-align: top;
	}

	th {
		font-size: 0.85rem;
		text-transform: uppercase;
		letter-spacing: 0.04em;
		color: #5a6570;
	}

	tbody tr:last-child td {
		border-bottom: none;
	}

	.zone {
		display: block;
		font-size: 0.8rem;
		color: #5a6570;
	}

	.badge {
		display: inline-block;
		margin-right: 0.35rem;
		padding: 0.1rem 0.5rem;
		border-radius: 999px;
		font-size: 0.8rem;
		font-weight: 600;
	}

	.badge.published {
		background: #d8f3dc;
		color: #1b4332;
	}

	.badge.draft {
		background: #e4e7ea;
		color: #3d454d;
	}

	.badge.test {
		background: #fdecc8;
		color: #6b4b00;
	}

	.empty {
		color: #4a5560;
	}
</style>
