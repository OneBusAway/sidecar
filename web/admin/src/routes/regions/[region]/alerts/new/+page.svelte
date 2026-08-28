<script lang="ts">
	import { goto } from '$app/navigation';
	import { resolve } from '$app/paths';
	import AlertForm from '$lib/AlertForm.svelte';
	import { api, regionPath } from '$lib/api';
	import { buildCreatePayload, type AlertFormValues } from '$lib/alerts';
	import type { Alert } from '$lib/types';
	import type { PageProps } from './$types';

	let { data }: PageProps = $props();

	// Anything thrown here lands in the form's error banner, which is where an
	// ApiError's message -- the server's own wording about enums, offsets or a
	// missing default agency id -- belongs.
	async function create(vals: AlertFormValues, timezone: string) {
		const created = await api.post<Alert>(
			regionPath(data.region.id, '/alerts'),
			buildCreatePayload(vals, timezone),
		);
		await goto(
			resolve('/regions/[region]/alerts/[id]', {
				region: String(data.region.id),
				id: String(created.id),
			}),
		);
	}
</script>

<h1>New alert in {data.region.name}</h1>

<AlertForm region={data.region} submitLabel="Create alert" onsubmit={create} />

<p>
	<a
		href={resolve('/regions/[region]/alerts', {
			region: String(data.region.id),
		})}
	>
		Back to alerts
	</a>
</p>
