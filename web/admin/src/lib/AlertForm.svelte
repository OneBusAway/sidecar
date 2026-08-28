<script lang="ts">
	import { untrack } from 'svelte';
	import { resolve } from '$app/paths';
	import { ApiError } from '$lib/api';
	import {
		blankFormValues,
		formValuesFromAlert,
		type AlertFormValues,
	} from '$lib/alerts';
	import { CAUSES, EFFECTS, SEVERITIES } from '$lib/enums';
	import type { Alert, Region } from '$lib/types';

	interface Props {
		/**
		 * The alert's region. Immutable through the API (see PatchAlertPayload),
		 * so it is never a form field here -- offering a control the server
		 * would refuse is worse than offering none at all.
		 */
		region: Region;
		/** Present in edit mode; absent in create mode. */
		initial?: Alert;
		submitLabel: string;
		/**
		 * The caller builds the request body -- create and edit send different
		 * shapes (PATCH has no region_id), so handing the caller the raw form
		 * values and the resolved zone keeps both sides exactly typed instead
		 * of pushing a union through here and narrowing it again at the far end.
		 */
		onsubmit: (values: AlertFormValues, timezone: string) => Promise<void>;
	}

	let { region, initial, submitLabel, onsubmit }: Props = $props();

	const editing = $derived(initial !== undefined);

	// untrack() states the intent the compiler otherwise warns about: this is a
	// snapshot of the props taken once. The form is an editing buffer, so
	// re-deriving it from `initial` would throw away whatever the operator had
	// typed the moment anything upstream refetched. Callers that DO want a
	// fresh buffer remount the component (see the edit page's {#key}).
	let values = $state<AlertFormValues>(
		untrack(() =>
			initial
				? formValuesFromAlert(initial, region.timezone)
				: blankFormValues(),
		),
	);
	let error = $state('');
	let busy = $state(false);

	// '' means the region has no configured timezone. It is the DEFAULT state:
	// timezone is one of the two locally-managed region fields, so a freshly
	// synced region has none until an operator sets one.
	const zone = $derived(region.timezone);

	async function submit(event: SubmitEvent) {
		event.preventDefault();
		busy = true;
		error = '';
		try {
			await onsubmit(values, zone);
		} catch (err) {
			// The server's message is the UX copy: it names the valid enum
			// values, the explicit-offset requirement, and which region needs a
			// default agency id. Anything this component invented would be a
			// worse second opinion on the same problem.
			error =
				err instanceof ApiError
					? err.message
					: err instanceof Error
						? err.message
						: 'save failed';
		} finally {
			busy = false;
		}
	}
</script>

<form onsubmit={submit}>
	{#if error}<p class="error" role="alert">{error}</p>{/if}

	<label>
		Agency id
		<input
			bind:value={values.agencyId}
			placeholder={region.default_agency_id
				? `default: ${region.default_agency_id}`
				: 'no region default — required'}
		/>
	</label>

	<label>
		Header
		<input bind:value={values.header} required />
	</label>

	<label>
		Description
		<textarea bind:value={values.description} rows="4"></textarea>
	</label>

	<label>
		URL
		<input bind:value={values.url} type="url" placeholder="https://" />
	</label>

	<div class="row">
		<label>
			Cause
			<select bind:value={values.cause}>
				{#each CAUSES as c (c.value)}
					<option value={c.value}>{c.label}</option>
				{/each}
			</select>
		</label>
		<label>
			Effect
			<select bind:value={values.effect}>
				{#each EFFECTS as e (e.value)}
					<option value={e.value}>{e.label}</option>
				{/each}
			</select>
		</label>
		<label>
			Severity
			<select bind:value={values.severity}>
				{#each SEVERITIES as s (s.value)}
					<option value={s.value}>{s.label}</option>
				{/each}
			</select>
		</label>
	</div>

	{#if zone === ''}
		<!--
			No date picker without a zone. A datetime-local value is a naive wall
			time, and turning it into an instant needs an offset this app must
			not invent: the browser's zone is the operator's, not the region's,
			and stamping it on a Tampa alert typed in Seattle moves it three
			hours with nothing on screen to show for it.
		-->
		<p class="hint">
			{region.name} has no configured timezone, so times must be typed as RFC 3339
			with an explicit offset — for example
			<code>2026-08-15T14:00:00-04:00</code>.
			<a href={resolve('/regions')}>Set a timezone on the Regions screen</a> to get
			a date picker instead.
		</p>
		<label>
			Start (RFC 3339 with offset)
			<input
				bind:value={values.start}
				placeholder="2026-08-15T14:00:00-04:00"
				required
			/>
		</label>
		<label>
			End (RFC 3339 with offset, optional)
			<input
				bind:value={values.end}
				placeholder="2026-08-15T18:00:00-04:00"
				disabled={values.clearEnd}
			/>
		</label>
	{:else}
		{#if zone === 'UTC'}
			<!--
				`regions.timezone` is NOT NULL DEFAULT 'UTC' and the directory sync
				never sets it, so 'UTC' is indistinguishable from "nobody has
				configured this region yet" -- the schema makes the guess this
				form is otherwise careful not to. An author typing 17:00 for an
				unconfigured Tampa Bay stores 17:00Z, four hours off, with the
				"(UTC)" label as the only clue.
				This over-warns for a region that really is on UTC. That is the
				right direction to err: a redundant hint costs a reading, a
				silently mis-stamped alert costs riders.
			-->
			<p class="hint">
				{region.name} reads as
				<code>UTC</code>, which is also the default for a region nobody has
				configured yet — times you enter will be read as UTC.
				<a href={resolve('/regions')}>Set its timezone on the Regions screen</a>
				if that is not what you want.
			</p>
		{/if}
		<label>
			Start <span class="zone">({zone})</span>
			<input type="datetime-local" bind:value={values.start} required />
		</label>
		<label>
			End <span class="zone">({zone}, optional)</span>
			<input
				type="datetime-local"
				bind:value={values.end}
				disabled={values.clearEnd}
			/>
		</label>
	{/if}

	{#if editing}
		<label class="check">
			<input type="checkbox" bind:checked={values.clearEnd} />
			Clear the end time (revert to the feed's default duration)
		</label>
	{/if}

	<label class="check">
		<input type="checkbox" bind:checked={values.isTest} />
		Test alert (kept out of the rider feed)
	</label>

	<div class="actions">
		<button disabled={busy}>{submitLabel}</button>
	</div>
</form>

<style>
	form {
		display: flex;
		flex-direction: column;
		gap: 1rem;
		max-width: 44rem;
	}

	label {
		display: flex;
		flex-direction: column;
		gap: 0.3rem;
		font-weight: 600;
	}

	label.check {
		flex-direction: row;
		align-items: center;
		gap: 0.5rem;
		font-weight: 400;
	}

	.row {
		display: flex;
		flex-wrap: wrap;
		gap: 1rem;
	}

	.row label {
		flex: 1 1 12rem;
	}

	input,
	select,
	textarea {
		font: inherit;
		font-weight: 400;
		padding: 0.45rem 0.6rem;
		border: 1px solid #b8c0c8;
		border-radius: 4px;
		background: #fff;
	}

	input:disabled,
	select:disabled {
		background: #eceff1;
		color: #5a6570;
	}

	textarea {
		resize: vertical;
	}

	.zone {
		font-weight: 400;
		color: #5a6570;
	}

	.hint {
		flex-basis: 100%;
		margin: 0;
		font-size: 0.9rem;
		color: #4a5560;
	}

	.actions {
		display: flex;
		gap: 0.75rem;
	}

	button {
		font: inherit;
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
