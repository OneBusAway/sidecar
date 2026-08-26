// Component tests, in the `component` vitest project (jsdom).
//
// The behaviour under test is the {#key} on the edit form -- markup, not a
// module. It was a bug found by hand in a real browser: after clearing the end
// time and saving, the old end value stayed in the (disabled) field with the
// checkbox still ticked, so unticking and saving again silently restored an
// end time the server no longer had. Reverting that key leaves every node-
// project test green, which is exactly why this file exists.
import { describe, expect, it, vi } from 'vitest';
import { render, screen, within } from '@testing-library/svelte';
import userEvent from '@testing-library/user-event';

const { patchMock, invalidateAllMock, postMock } = vi.hoisted(() => ({
	patchMock: vi.fn(),
	invalidateAllMock: vi.fn(),
	postMock: vi.fn(),
}));

vi.mock('$app/navigation', () => ({
	goto: vi.fn(),
	invalidateAll: invalidateAllMock,
}));
vi.mock('$app/paths', () => ({
	resolve: (p: string) => `/admin${p === '/' ? '' : p}`,
}));
vi.mock('$lib/api', async () => {
	const actual = await vi.importActual<typeof import('$lib/api')>('$lib/api');
	return {
		...actual,
		api: { ...actual.api, patch: patchMock, post: postMock },
	};
});

import EditPage from './+page.svelte';
import type { Alert, Region } from '$lib/types';

const REGION: Region = {
	id: 1,
	name: 'Puget Sound',
	oba_base_url: '',
	sidecar_base_url: '',
	language: 'en',
	active: true,
	default_agency_id: 'KCM',
	timezone: 'Asia/Kathmandu',
	latitude: null,
	longitude: null,
	oba_api_key: 'none',
};

function alert(over: Partial<Alert> = {}): Alert {
	return {
		id: 7,
		region_id: 1,
		agency_id: 'KCM',
		header: 'Bridge out',
		description: '',
		url: '',
		cause: 'CONSTRUCTION',
		effect: 'DETOUR',
		severity: 'WARNING',
		start_time: '2026-08-15T21:00:00Z',
		end_time: '2026-08-16T21:00:00Z',
		published: false,
		is_test: false,
		created_at: '2026-08-01T00:00:00Z',
		// Overridden per call below. Every mutation bumps this server-side --
		// publish and unpublish included, which was confirmed against the
		// running server -- so a fixture that reuses one timestamp across a
		// mutation quietly turns "key on updated_at" into "never remount" and
		// stops the tests telling those two apart.
		updated_at: '2026-08-01T00:00:00Z',
		translations: [],
		...over,
	};
}

/** The same alert as the server would return it after a mutation. */
function afterMutation(over: Partial<Alert> = {}): Alert {
	return alert({ updated_at: '2026-08-01T00:05:00Z', ...over });
}

/**
 * Mounts the page with an `invalidateAll` that behaves like the real one:
 * it resolves only once fresh data is in place.
 *
 * Getting this order right is the whole test. Resolving the mock first and
 * pushing new data afterwards would remount the form from the data it already
 * had, and the assertions would pass or fail for reasons that have nothing to
 * do with the code -- the page would look correct while modelling a sequence
 * SvelteKit never produces.
 */
function mount(initial: Alert) {
	// `user` and `sessionError` reach `data` from the root layout's load;
	// `params` is a sibling prop from the route. PageProps carries all three,
	// so the fixture supplies them rather than casting the shape away.
	const shell = { user: { username: 'admin' }, sessionError: '' };
	const props = (a: Alert) => ({
		// `pushes`/`audience` come from the same load; null audience is the
		// "no push transport configured" case, which keeps the push card out
		// of these tests without stubbing another endpoint.
		data: {
			...shell,
			alert: a,
			regions: [REGION],
			pushes: [],
			audience: null,
		},
		params: { id: String(a.id) },
	});
	let next = initial;
	invalidateAllMock.mockImplementation(async () => {
		await rerender(props(next));
	});
	const { rerender } = render(EditPage, props(initial));
	return {
		/** What the next re-read of the alert will return. */
		serverWillReturn(a: Alert) {
			next = a;
		},
	};
}

/** The alert form is the first <form> on the page; the second is translations. */
const detailsForm = () =>
	within(document.querySelectorAll('form')[0] as HTMLElement);
const endField = () => detailsForm().getByLabelText(/^End/) as HTMLInputElement;
const headerField = () =>
	detailsForm().getByLabelText(/^Header/) as HTMLInputElement;
const clearBox = () =>
	detailsForm().getByLabelText(/Clear the end time/) as HTMLInputElement;

describe('edit page: the form resynchronises with the server after a save', () => {
	it('empties the end field and unticks the box once a cleared end time is saved', async () => {
		const user = userEvent.setup();
		patchMock.mockResolvedValue(afterMutation({ end_time: null }));
		const page = mount(alert());
		page.serverWillReturn(afterMutation({ end_time: null }));

		// 2026-08-16T21:00Z is 2026-08-17T02:45 in Kathmandu (+05:45).
		expect(endField().value).toBe('2026-08-17T02:45');

		await user.click(clearBox());
		await user.click(screen.getByRole('button', { name: 'Save changes' }));

		expect(patchMock).toHaveBeenCalledTimes(1);
		const [path, body] = patchMock.mock.calls[0];
		expect(path).toBe('/alerts/7');
		expect(body.clear_end_time).toBe(true);
		// The API rejects end_time and clear_end_time together.
		expect('end_time' in body).toBe(false);
		expect(invalidateAllMock).toHaveBeenCalledTimes(1);

		// An end time the server no longer holds must not be left sitting in
		// the field, where unticking the box would put it straight back.
		expect(endField().value).toBe('');
		expect(clearBox().checked).toBe(false);
	});

	// The other half of the rule: a mutation that is NOT a save must not
	// remount the form, or pressing Publish would silently discard whatever the
	// operator had typed and not yet saved.
	it('keeps unsaved edits when publishing', async () => {
		const user = userEvent.setup();
		postMock.mockResolvedValue(undefined);
		const page = mount(alert());
		page.serverWillReturn(afterMutation({ published: true }));

		await user.clear(headerField());
		await user.type(headerField(), 'Bridge out, both directions');

		await user.click(screen.getByRole('button', { name: 'Publish' }));
		expect(postMock).toHaveBeenCalledWith('/alerts/7/publish');
		// The page itself reflects the server: the button has flipped.
		expect(
			screen.getByRole('button', { name: 'Unpublish' }),
		).toBeInTheDocument();
		// ...but the unsaved edit survives.
		expect(headerField().value).toBe('Bridge out, both directions');
	});
});
