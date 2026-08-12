<script lang="ts">
	import { goto } from '$app/navigation';
	import { resolve } from '$app/paths';
	import AlertForm from '$lib/AlertForm.svelte';
	import { api } from '$lib/api';
	import { buildCreatePayload, type AlertFormValues } from '$lib/alerts';
	import type { Alert } from '$lib/types';
	import type { PageProps } from './$types';

	let { data }: PageProps = $props();

	// Anything thrown here lands in the form's error banner, which is where an
	// ApiError's message -- the server's own wording about enums, offsets or a
	// missing default agency id -- belongs.
	async function create(vals: AlertFormValues, timezone: string) {
		const created = await api.post<Alert>(
			'/alerts',
			buildCreatePayload(vals, timezone),
		);
		await goto(resolve('/alerts/[id]', { id: String(created.id) }));
	}
</script>

<h1>New alert</h1>

<AlertForm
	regions={data.regions}
	submitLabel="Create alert"
	onsubmit={create}
/>

<p><a href={resolve('/')}>Back to alerts</a></p>
