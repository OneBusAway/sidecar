import { afterEach, describe, expect, it, vi } from 'vitest';

// $app/navigation and $app/paths only work inside a running SvelteKit client,
// and lib/pushes imports $lib/api, which imports both. Stubbed exactly as
// api.test.ts does.
vi.mock('$app/navigation', () => ({ goto: vi.fn() }));
vi.mock('$app/paths', () => ({
	resolve: (path: string) => `/admin${path === '/' ? '' : path}`,
}));

import { api, ApiError } from './api';
import {
	audienceOptions,
	isInFlight,
	loadAudience,
	loadPushes,
	progressLabel,
	sendConfirmMessage,
	statusTone,
} from './pushes';
import type { AlertPush, PushAudience } from './types';

const audience: PushAudience = {
	all: { total: 1200, ios: 900, android: 300 },
	test: { total: 3, ios: 3, android: 0 },
	forced_test: false,
};

describe('audienceOptions', () => {
	it('offers both audiences for a normal alert', () => {
		expect(audienceOptions(audience).map((o) => o.value)).toEqual([
			'all',
			'test',
		]);
		expect(audienceOptions(audience)[0].label).toBe('Everyone (1,200 devices)');
		expect(audienceOptions(audience)[1].label).toBe('Test devices (3)');
	});

	it('offers only test devices when forced', () => {
		const opts = audienceOptions({ ...audience, forced_test: true });
		expect(opts).toHaveLength(1);
		expect(opts[0].value).toBe('test');
		expect(opts[0].label).toBe('Test devices (3)');
	});

	// "Everyone (1 devices)" is the kind of thing an operator screenshots.
	it('says device, singular, for one device', () => {
		const one = { ...audience, all: { total: 1, ios: 1, android: 0 } };
		expect(audienceOptions(one)[0].label).toBe('Everyone (1 device)');
	});
});

describe('isInFlight', () => {
	it('is true for queued and sending only', () => {
		expect(isInFlight({ status: 'queued' })).toBe(true);
		expect(isInFlight({ status: 'sending' })).toBe(true);
		for (const s of ['sent', 'failed', 'canceled'] as const) {
			expect(isInFlight({ status: s })).toBe(false);
		}
	});
});

describe('progressLabel', () => {
	it('reports submitted/failed of devices', () => {
		expect(
			progressLabel({
				device_count: 1200,
				submitted_count: 500,
				failed_count: 2,
			}),
		).toBe('500 sent · 2 failed · of 1,200');
	});

	it('says pending while the count is unknown', () => {
		expect(
			progressLabel({ device_count: 0, submitted_count: 0, failed_count: 0 }),
		).toBe('pending');
	});

	// A resumed page can submit a bounded duplicate, so submitted may exceed
	// the device count. That is a real, expected state: print the numbers
	// rather than clamping them and hiding it.
	it('prints the numbers when more were submitted than there are devices', () => {
		expect(
			progressLabel({ device_count: 10, submitted_count: 12, failed_count: 1 }),
		).toBe('12 sent · 1 failed · of 10');
	});
});

describe('sendConfirmMessage', () => {
	it('names the audience size', () => {
		expect(sendConfirmMessage('all', audience)).toBe(
			'Send this alert as a push notification to 1,200 devices?',
		);
		expect(sendConfirmMessage('test', audience)).toBe(
			'Send this alert as a push notification to 3 test devices?',
		);
	});

	it('is singular for one device', () => {
		const one: PushAudience = {
			all: { total: 1, ios: 1, android: 0 },
			test: { total: 1, ios: 0, android: 1 },
			forced_test: false,
		};
		expect(sendConfirmMessage('all', one)).toBe(
			'Send this alert as a push notification to 1 device?',
		);
		expect(sendConfirmMessage('test', one)).toBe(
			'Send this alert as a push notification to 1 test device?',
		);
	});
});

describe('statusTone', () => {
	it('maps statuses to badge tones', () => {
		expect(statusTone('sent')).toBe('published');
		expect(statusTone('failed')).toBe('test');
		expect(statusTone('canceled')).toBe('test');
		expect(statusTone('queued')).toBe('draft');
		expect(statusTone('sending')).toBe('draft');
	});
});

afterEach(() => {
	vi.restoreAllMocks();
});

describe('loadPushes / loadAudience', () => {
	it('request the alert-scoped paths', async () => {
		const push = { id: 4 } as AlertPush;
		const get = vi.spyOn(api, 'get').mockImplementation(async (path) => {
			if (path === '/alerts/7/pushes') return [push] as never;
			return audience as never;
		});

		expect(await loadPushes('7')).toEqual([push]);
		expect(await loadAudience('7')).toEqual(audience);
		expect(get.mock.calls.map((c) => c[0])).toEqual([
			'/alerts/7/pushes',
			'/alerts/7/push_audience',
		]);
	});

	// The four push routes are not registered at all when the server has no
	// push transport, so the 404 is the "not configured" signal, not a fault.
	it('treat a 404 as "push is not configured"', async () => {
		vi.spyOn(api, 'get').mockRejectedValue(new ApiError(404, '404 Not Found'));
		expect(await loadPushes('7')).toEqual([]);
		expect(await loadAudience('7')).toBeNull();
	});

	// Anything else is a real failure: swallowing it would render an empty
	// history over a server that is actually broken.
	it('rethrow any other error', async () => {
		vi.spyOn(api, 'get').mockRejectedValue(new ApiError(500, 'boom'));
		await expect(loadPushes('7')).rejects.toThrow('boom');
		await expect(loadAudience('7')).rejects.toThrow('boom');
	});
});
