// Component tests for the push card, in the `component` vitest project (jsdom).
//
// lib/pushes covers the wording and the arithmetic; what lives only in markup
// is which branch renders and what each button actually calls. Those are the
// parts an operator meets: a card that offers Send on an unpublished alert, or
// a Cancel button wired to the wrong id, would pass every node-project test.
import { describe, expect, it, vi, beforeEach } from 'vitest';
import { render, screen } from '@testing-library/svelte';
import userEvent from '@testing-library/user-event';

const { postMock, delMock, invalidateAllMock } = vi.hoisted(() => ({
	postMock: vi.fn(),
	delMock: vi.fn(),
	invalidateAllMock: vi.fn(),
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
		api: { ...actual.api, post: postMock, del: delMock },
	};
});

import AlertPage from './+page.svelte';
import type { Alert, AlertPush, PushAudience, Region } from '$lib/types';

// 'pushes' is in the default region's features, so the card renders its
// configured state unless a test overrides `region` to say otherwise.
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
	features: ['alerts', 'pushes'],
};

const AUDIENCE: PushAudience = {
	all: { total: 1200, ios: 900, android: 300 },
	test: { total: 3, ios: 3, android: 0 },
	forced_test: false,
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
		end_time: null,
		published: true,
		is_test: false,
		created_at: '2026-08-01T00:00:00Z',
		updated_at: '2026-08-01T00:00:00Z',
		translations: [],
		...over,
	};
}

function push(over: Partial<AlertPush> = {}): AlertPush {
	return {
		id: 4,
		alert_id: 7,
		region_id: 1,
		audience: 'all',
		status: 'sending',
		device_count: 1200,
		submitted_count: 500,
		failed_count: 2,
		attempts: 1,
		last_error: '',
		messages: { en: { title: 'Bridge out', body: '' } },
		failure_reasons: [{ reason: 'Unregistered', count: 2 }],
		created_at: '2026-08-01T00:00:00Z',
		started_at: '2026-08-01T00:00:05Z',
		completed_at: null,
		...over,
	};
}

function mount(opts: {
	alert?: Alert;
	pushes?: AlertPush[];
	audience?: PushAudience | null;
	region?: Region;
}) {
	return render(AlertPage, {
		data: {
			user: { username: 'admin' },
			sessionError: '',
			alert: opts.alert ?? alert(),
			region: opts.region ?? REGION,
			pushes: opts.pushes ?? [],
			audience: opts.audience === undefined ? AUDIENCE : opts.audience,
		},
		params: { region: '1', id: '7' },
	});
}

// Matched on the prefix: the label carries the audience size, which several
// tests assert exactly.
const sendButton = () =>
	screen.getByRole('button', { name: /^Send push/ }) as HTMLButtonElement;

beforeEach(() => {
	postMock.mockReset().mockResolvedValue(undefined);
	delMock.mockReset().mockResolvedValue(undefined);
	invalidateAllMock.mockReset().mockResolvedValue(undefined);
	vi.stubGlobal(
		'confirm',
		vi.fn(() => true),
	);
});

describe('the push card', () => {
	// The "not configured" signal comes from the region's `features`, not from
	// a swallowed 404 (see lib/pushes). A non-null audience here proves the
	// card is reading the gate off `hasFeature`, not off `data.audience ===
	// null` -- the old behaviour, which would have shown the Send button
	// whenever the load happened to populate audience.
	it('says push is not configured when the region does not have the feature', () => {
		mount({ region: { ...REGION, features: ['alerts'] }, audience: AUDIENCE });
		expect(screen.getByText(/not configured on this server/)).toBeVisible();
		expect(screen.queryByRole('button', { name: /^Send push/ })).toBeNull();
	});

	// Absent `features` -- the shape GET /regions (the list) actually returns
	// -- must read the same as "not configured", never as "everything is on".
	it('says push is not configured when features is absent entirely', () => {
		mount({ region: { ...REGION, features: undefined }, audience: AUDIENCE });
		expect(screen.getByText(/not configured on this server/)).toBeVisible();
	});

	it('refuses to send an unpublished alert', () => {
		mount({ alert: alert({ published: false }) });
		expect(screen.getByText(/^Publish the alert/)).toBeVisible();
		expect(sendButton()).toBeDisabled();
	});

	it('queues the chosen audience and re-reads the history', async () => {
		const user = userEvent.setup();
		mount({});

		// 'all' is the default: the audience options put it first, and the
		// button says so before it is pressed (design spec §2.10).
		expect(sendButton()).toHaveAccessibleName('Send push to 1,200 devices');
		await user.click(sendButton());
		expect(postMock).toHaveBeenCalledWith('/regions/1/alerts/7/pushes', {
			audience: 'all',
		});
		expect(invalidateAllMock).toHaveBeenCalledTimes(1);

		await user.click(screen.getByLabelText('Test devices (3)'));
		// The label follows the radio, so the button and the confirm() it opens
		// can never name different audiences.
		expect(sendButton()).toHaveAccessibleName('Send push to 3 test devices');
		await user.click(sendButton());
		expect(postMock).toHaveBeenLastCalledWith('/regions/1/alerts/7/pushes', {
			audience: 'test',
		});
	});

	// The confirm() is the only thing between a mis-click and 1,200 phones.
	it('sends nothing when the confirm is dismissed', async () => {
		const user = userEvent.setup();
		vi.stubGlobal(
			'confirm',
			vi.fn(() => false),
		);
		mount({});
		await user.click(sendButton());
		expect(postMock).not.toHaveBeenCalled();
	});

	it('hides the audience choice for a forced-test alert', () => {
		// A four-digit test fleet, so a raw interpolation ("1200") fails where
		// the shared formatter ("1,200") passes.
		mount({
			audience: {
				...AUDIENCE,
				test: { total: 1200, ios: 900, android: 300 },
				forced_test: true,
			},
		});
		expect(screen.queryByLabelText(/^Everyone/)).toBeNull();
		expect(
			screen.getByText(/the only audience on offer is Test devices \(1,200\)/),
		).toBeVisible();
		expect(sendButton()).toHaveAccessibleName(
			'Send push to 1,200 test devices',
		);
	});

	it('cancels the in-flight push by id and blocks a second send', async () => {
		const user = userEvent.setup();
		mount({ pushes: [push({ id: 9 })] });

		expect(screen.getByText('500 sent · 2 failed · of 1,200')).toBeVisible();
		expect(screen.getByText('Unregistered (2)')).toBeVisible();
		// The API allows one push in flight per alert; the button says so
		// before the server has to.
		expect(sendButton()).toBeDisabled();

		await user.click(screen.getByRole('button', { name: 'Cancel' }));
		expect(delMock).toHaveBeenCalledWith('/regions/1/alerts/7/pushes/9');
		expect(invalidateAllMock).toHaveBeenCalledTimes(1);
	});

	// The poll is what makes the counts move without a refresh -- and, if the
	// terminal statuses ever leaked into isInFlight, what would re-read an
	// idle alert page every three seconds forever.
	it('polls every three seconds while a push is in flight', async () => {
		vi.useFakeTimers();
		try {
			mount({ pushes: [push({ status: 'sending' })] });
			await vi.advanceTimersByTimeAsync(9000);
			expect(invalidateAllMock).toHaveBeenCalledTimes(3);
		} finally {
			vi.useRealTimers();
		}
	});

	// The timer outlives the page unless the effect's cleanup clears it, and a
	// SPA navigation away from an alert with a live push is exactly when that
	// happens: the leaked interval would keep re-reading a route nobody is on.
	it('stops polling when the page goes away', async () => {
		vi.useFakeTimers();
		try {
			const page = mount({ pushes: [push({ status: 'sending' })] });
			await vi.advanceTimersByTimeAsync(3000);
			expect(invalidateAllMock).toHaveBeenCalledTimes(1);

			page.unmount();
			await vi.advanceTimersByTimeAsync(9000);
			expect(invalidateAllMock).toHaveBeenCalledTimes(1);
		} finally {
			vi.useRealTimers();
		}
	});

	// Separate test, not a second mount in the one above: the first page is
	// only unmounted by testing-library's per-test cleanup, so its timer would
	// still be running and answer for this assertion.
	it('does not poll once every push is terminal', async () => {
		vi.useFakeTimers();
		try {
			mount({ pushes: [push({ status: 'sent' })] });
			await vi.advanceTimersByTimeAsync(9000);
			expect(invalidateAllMock).not.toHaveBeenCalled();
		} finally {
			vi.useRealTimers();
		}
	});

	it('offers no Cancel on a terminal push', () => {
		mount({ pushes: [push({ status: 'sent', submitted_count: 1200 })] });
		expect(screen.queryByRole('button', { name: 'Cancel' })).toBeNull();
		expect(sendButton()).toBeEnabled();
	});
});
