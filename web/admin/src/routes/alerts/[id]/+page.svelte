<script lang="ts">
	import { goto, invalidateAll } from '$app/navigation';
	import { resolve } from '$app/paths';
	import AlertForm from '$lib/AlertForm.svelte';
	import { api, ApiError } from '$lib/api';
	import {
		alertBadges,
		buildPatchPayload,
		buildTranslationPayload,
		formatInstantForRegion,
		regionById,
		regionName,
		type AlertFormValues,
	} from '$lib/alerts';
	import type { Alert, Translation } from '$lib/types';
	import type { PageProps } from './$types';

	let { data }: PageProps = $props();

	const alert = $derived(data.alert);
	const region = $derived(regionById(data.regions, data.alert.region_id));
	const zone = $derived(region?.timezone ?? '');

	let actionError = $state('');
	let busy = $state(false);

	/**
	 * Bumped after a successful save, and used with the alert id to key the
	 * form (see the {#key} below).
	 *
	 * The form is an editing buffer, so it cannot simply re-derive from the
	 * loaded alert -- that would discard whatever the operator had typed the
	 * instant any other action refetched. But it must not outlive a save
	 * either: clearing the end time leaves the old end sitting in the (now
	 * disabled) field, so unticking the box and saving again would quietly put
	 * it back. Remounting on save, and only on save, is what keeps the buffer
	 * and the server agreeing without throwing away unsaved work.
	 */
	let savedRevision = $state(0);

	function message(err: unknown, fallback: string): string {
		if (err instanceof ApiError) return err.message;
		if (err instanceof Error) return err.message;
		return fallback;
	}

	/**
	 * Every mutation re-reads the alert instead of patching local state from
	 * the response. Staleness is computed server-side against the alert's
	 * CURRENT English text, so editing a header legitimately drops that
	 * alert's translations out of the rider feed -- an optimistic local update
	 * would keep showing them as live.
	 */
	async function reload() {
		await invalidateAll();
	}

	async function setPublished(published: boolean) {
		busy = true;
		actionError = '';
		try {
			await api.post(
				`/alerts/${alert.id}/${published ? 'publish' : 'unpublish'}`,
			);
			await reload();
		} catch (err) {
			actionError = message(err, 'could not change published state');
		} finally {
			busy = false;
		}
	}

	async function remove() {
		if (
			!confirm(
				`Delete alert #${alert.id} (“${alert.header}”)? This cannot be undone.`,
			)
		) {
			return;
		}
		busy = true;
		actionError = '';
		try {
			await api.del(`/alerts/${alert.id}`);
		} catch (err) {
			actionError = message(err, 'could not delete this alert');
			busy = false;
			return;
		}
		// No reload here: the alert is gone, so re-reading it would 404 and
		// replace a successful delete with an error page.
		await goto(resolve('/'));
	}

	// Thrown errors reach the form's own banner, which is where the server's
	// wording belongs.
	async function save(values: AlertFormValues, timezone: string) {
		await api.patch<Alert>(
			`/alerts/${alert.id}`,
			buildPatchPayload(values, timezone),
		);
		await reload();
		savedRevision += 1;
	}

	let tLanguage = $state('');
	let tHeader = $state('');
	let tDescription = $state('');
	let tError = $state('');
	let tBusy = $state(false);

	function edit(t: Translation) {
		tLanguage = t.language;
		// null means "this field has no translation", which is not the same as
		// a translation whose text is empty -- but a textarea can only hold a
		// string, so both arrive as ''. buildTranslationPayload then omits an
		// untouched empty field, leaving whatever the server has alone.
		tHeader = t.header ?? '';
		tDescription = t.description ?? '';
	}

	async function saveTranslation(event: SubmitEvent) {
		event.preventDefault();
		tBusy = true;
		tError = '';
		try {
			await api.put(
				`/alerts/${alert.id}/translations/${encodeURIComponent(tLanguage.trim())}`,
				buildTranslationPayload(tHeader, tDescription),
			);
			await reload();
			tLanguage = '';
			tHeader = '';
			tDescription = '';
		} catch (err) {
			tError = message(err, 'could not save this translation');
		} finally {
			tBusy = false;
		}
	}

	async function removeTranslation(language: string) {
		if (!confirm(`Delete the ${language} translation of alert #${alert.id}?`)) {
			return;
		}
		tBusy = true;
		tError = '';
		try {
			await api.del(
				`/alerts/${alert.id}/translations/${encodeURIComponent(language)}`,
			);
			// DELETE answers 204, so the alert has to be re-read to find out
			// what is left.
			await reload();
		} catch (err) {
			tError = message(err, 'could not delete this translation');
		} finally {
			tBusy = false;
		}
	}
</script>

<div class="head">
	<h1>Alert #{alert.id}</h1>
	<span class="badges">
		{#each alertBadges(alert) as badge (badge.tone)}
			<span class="badge {badge.tone}">{badge.label}</span>
		{/each}
	</span>
</div>

<dl class="meta">
	<dt>Region</dt>
	<dd>{regionName(data.regions, alert.region_id)} (#{alert.region_id})</dd>
	<dt>Starts</dt>
	<dd>
		{formatInstantForRegion(alert.start_time, zone)}
		<span class="zone">{zone === '' ? 'UTC' : zone}</span>
	</dd>
	<dt>Ends</dt>
	<dd>
		{#if alert.end_time === null}
			<span class="zone">no end time (feed default duration applies)</span>
		{:else}
			{formatInstantForRegion(alert.end_time, zone)}
			<span class="zone">{zone === '' ? 'UTC' : zone}</span>
		{/if}
	</dd>
	<dt>Updated</dt>
	<dd>{alert.updated_at}</dd>
</dl>

{#if actionError}<p class="error" role="alert">{actionError}</p>{/if}

<div class="actions">
	<button
		type="button"
		disabled={busy}
		onclick={() => setPublished(!alert.published)}
	>
		{alert.published ? 'Unpublish' : 'Publish'}
	</button>
	<button type="button" class="danger" disabled={busy} onclick={remove}>
		Delete
	</button>
	<a href={resolve('/')}>Back to alerts</a>
</div>

<h2>Details</h2>

<!--
	Keyed on the id and the save counter, not on updated_at: remounting after
	EVERY mutation would throw away whatever the operator had typed but not yet
	saved the moment they hit Publish, while never remounting leaves the buffer
	disagreeing with the server after a save. A different alert needs a fresh
	form too, which the id covers.
-->
{#key `${alert.id}:${savedRevision}`}
	<AlertForm
		regions={data.regions}
		initial={alert}
		submitLabel="Save changes"
		onsubmit={save}
	/>
{/key}

<h2>Translations</h2>

<p class="note">
	A translation is withheld from the rider feed once the English text it was
	written against changes — riders get accurate English rather than stale
	translated text. Editing the header or description above therefore drops that
	field's translations from the feed until they are saved again.
</p>

{#if alert.translations.length === 0}
	<p class="empty">No translations yet.</p>
{:else}
	<table>
		<thead>
			<tr>
				<th>Language</th>
				<th>Header</th>
				<th>Description</th>
				<th></th>
			</tr>
		</thead>
		<tbody>
			{#each alert.translations as t (t.language)}
				<tr>
					<td>{t.language}</td>
					<td>{t.header ?? '—'}</td>
					<td>{t.description ?? '—'}</td>
					<td class="rowactions">
						<button type="button" disabled={tBusy} onclick={() => edit(t)}>
							Edit
						</button>
						<button
							type="button"
							class="danger"
							disabled={tBusy}
							onclick={() => removeTranslation(t.language)}
						>
							Delete
						</button>
					</td>
				</tr>
			{/each}
		</tbody>
	</table>
{/if}

<form onsubmit={saveTranslation}>
	{#if tError}<p class="error" role="alert">{tError}</p>{/if}
	<label>
		Language
		<input bind:value={tLanguage} placeholder="es" required />
	</label>
	<label>
		Header
		<textarea bind:value={tHeader} rows="2"></textarea>
	</label>
	<label>
		Description
		<textarea bind:value={tDescription} rows="3"></textarea>
	</label>
	<p class="note">
		A field left blank is left alone rather than stored as an empty translation.
		Leave both blank and the server will say so.
	</p>
	<div class="actions">
		<button disabled={tBusy}>Save translation</button>
	</div>
</form>

<style>
	.head {
		display: flex;
		align-items: center;
		gap: 1rem;
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

	.meta {
		display: grid;
		grid-template-columns: max-content 1fr;
		gap: 0.25rem 1rem;
		margin: 0 0 1.25rem;
	}

	.meta dt {
		font-weight: 600;
		color: #5a6570;
	}

	.meta dd {
		margin: 0;
	}

	.zone {
		font-size: 0.85rem;
		color: #5a6570;
	}

	.actions {
		display: flex;
		align-items: center;
		gap: 0.75rem;
		margin-bottom: 1.5rem;
	}

	button {
		font: inherit;
		padding: 0.4rem 1rem;
		border: 1px solid #14181c;
		border-radius: 4px;
		background: #14181c;
		color: #f6f7f9;
		cursor: pointer;
	}

	button.danger {
		border-color: #b3261e;
		background: #b3261e;
	}

	button:disabled {
		cursor: default;
		opacity: 0.6;
	}

	table {
		width: 100%;
		border-collapse: collapse;
		background: #fff;
		border: 1px solid #dde2e6;
		border-radius: 4px;
		margin-bottom: 1.25rem;
	}

	th,
	td {
		padding: 0.5rem 0.7rem;
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

	.rowactions {
		display: flex;
		gap: 0.4rem;
	}

	.rowactions button {
		padding: 0.2rem 0.6rem;
		font-size: 0.9rem;
	}

	form {
		display: flex;
		flex-direction: column;
		gap: 0.75rem;
		max-width: 44rem;
	}

	form label {
		display: flex;
		flex-direction: column;
		gap: 0.3rem;
		font-weight: 600;
	}

	input,
	textarea {
		font: inherit;
		font-weight: 400;
		padding: 0.45rem 0.6rem;
		border: 1px solid #b8c0c8;
		border-radius: 4px;
		background: #fff;
	}

	textarea {
		resize: vertical;
	}

	.note,
	.empty {
		margin: 0 0 0.75rem;
		font-size: 0.9rem;
		color: #4a5560;
	}

	.error {
		margin: 0 0 0.75rem;
		padding: 0.6rem 0.8rem;
		border: 1px solid #b3261e;
		border-radius: 4px;
		background: #fdecea;
		color: #7f1d1a;
	}
</style>
