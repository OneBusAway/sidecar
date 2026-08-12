<script lang="ts">
	import { untrack } from 'svelte';
	import { api, ApiError } from '$lib/api';
	import { buildRegionPatch } from '$lib/regions';
	import type { Region } from '$lib/types';
	import type { PageProps } from './$types';

	let { data }: PageProps = $props();

	/** One table row: the region plus its two editable fields and status. */
	interface Row {
		region: Region;
		agencyID: string;
		timezone: string;
		error: string;
		saved: boolean;
		busy: boolean;
	}

	function toRow(region: Region): Row {
		return {
			region,
			agencyID: region.default_agency_id,
			timezone: region.timezone,
			error: '',
			saved: false,
			busy: false,
		};
	}

	// untrack(): the rows are an editing buffer seeded once from the load. Each
	// save replaces its own row from the PATCH response, so nothing here needs
	// to track `data` -- and re-deriving it would wipe half-typed values in
	// every other row.
	let rows = $state<Row[]>(untrack(() => data.regions.map(toRow)));

	async function save(row: Row) {
		row.busy = true;
		row.error = '';
		row.saved = false;
		try {
			// PATCH answers with the region re-read from the database, so the
			// row is refreshed from the server's copy rather than from what was
			// typed -- if the server normalised or rejected part of it, the
			// difference shows up here instead of being papered over.
			const updated = await api.patch<Region>(
				`/regions/${row.region.id}`,
				buildRegionPatch(row.agencyID, row.timezone),
			);
			row.region = updated;
			row.agencyID = updated.default_agency_id;
			row.timezone = updated.timezone;
			row.saved = true;
		} catch (err) {
			// The server owns the tzdata, so an unknown zone comes back as its
			// own 400 naming the bad value. That message is the UX copy.
			row.error =
				err instanceof ApiError
					? err.message
					: err instanceof Error
						? err.message
						: 'could not save this region';
		} finally {
			row.busy = false;
		}
	}
</script>

<h1>Regions</h1>

<p class="note">
	Regions come from the OneBusAway regions directory and cannot be added or
	removed here. The default agency id and the timezone are the two
	locally-managed fields; a directory refresh leaves them alone.
</p>

<p class="note">
	A region with no timezone gets no date picker on the alert form — times have
	to be typed as RFC 3339 with an explicit offset, because nothing may guess a
	zone on the region's behalf.
</p>

<table>
	<thead>
		<tr>
			<th>Id</th>
			<th>Name</th>
			<th>Active</th>
			<th>Default agency id</th>
			<th>Timezone</th>
			<th></th>
		</tr>
	</thead>
	<tbody>
		{#each rows as row (row.region.id)}
			<tr>
				<td>{row.region.id}</td>
				<td>{row.region.name}</td>
				<td>{row.region.active ? 'yes' : 'no'}</td>
				<td>
					<input
						bind:value={row.agencyID}
						aria-label="Default agency id for {row.region.name}"
						placeholder="none"
					/>
				</td>
				<td>
					<input
						bind:value={row.timezone}
						aria-label="Timezone for {row.region.name}"
						placeholder="America/New_York"
					/>
				</td>
				<td class="rowactions">
					<button type="button" disabled={row.busy} onclick={() => save(row)}>
						Save
					</button>
					{#if row.saved}<span class="ok">Saved</span>{/if}
				</td>
			</tr>
			{#if row.error}
				<tr class="errorrow">
					<td colspan="6"><p class="error" role="alert">{row.error}</p></td>
				</tr>
			{/if}
		{/each}
	</tbody>
</table>

<style>
	.note {
		margin: 0 0 0.75rem;
		font-size: 0.9rem;
		color: #4a5560;
		max-width: 48rem;
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
		padding: 0.5rem 0.7rem;
		text-align: left;
		border-bottom: 1px solid #eceff1;
		vertical-align: middle;
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

	.errorrow td {
		padding-top: 0;
	}

	input {
		font: inherit;
		width: 100%;
		min-width: 10rem;
		padding: 0.35rem 0.5rem;
		border: 1px solid #b8c0c8;
		border-radius: 4px;
	}

	.rowactions {
		white-space: nowrap;
	}

	button {
		font: inherit;
		padding: 0.35rem 0.9rem;
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

	.ok {
		margin-left: 0.5rem;
		font-size: 0.85rem;
		color: #1b4332;
	}

	.error {
		margin: 0;
		padding: 0.5rem 0.7rem;
		border: 1px solid #b3261e;
		border-radius: 4px;
		background: #fdecea;
		color: #7f1d1a;
	}
</style>
