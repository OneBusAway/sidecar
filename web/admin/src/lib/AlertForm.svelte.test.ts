// Component tests, in the `component` vitest project (jsdom).
//
// This behaviour cannot be lifted into a plain module: it is the form's
// reaction to a region change, which is a DOM event driving a branch in the
// markup. Without a DOM project, `selectRegion()` can be gutted and the whole
// node suite stays green.
import { describe, expect, it, vi } from 'vitest';
import { render, screen } from '@testing-library/svelte';
import userEvent from '@testing-library/user-event';

vi.mock('$app/paths', () => ({
	resolve: (p: string) => `/admin${p === '/' ? '' : p}`,
}));

import AlertForm from './AlertForm.svelte';
import type { Region } from './types';

function region(over: Partial<Region>): Region {
	return {
		id: 0,
		name: 'Tampa Bay',
		oba_base_url: '',
		sidecar_base_url: '',
		language: 'en',
		active: true,
		default_agency_id: 'HART',
		timezone: '',
		...over,
	};
}

// A zoned region and a zoneless one. Zoneless cannot be produced through the
// API today (see the regions screen's note), but the type allows it and the
// form has a whole branch for it, so it is exercised here.
const ZONED = region({
	id: 1,
	name: 'Puget Sound',
	timezone: 'Asia/Kathmandu',
});
const ZONELESS = region({ id: 16, name: 'Davis, CA', timezone: '' });

const startField = () => screen.getByLabelText(/^Start/) as HTMLInputElement;
const endField = () => screen.getByLabelText(/^End/) as HTMLInputElement;

describe('AlertForm region switching', () => {
	it('clears both timestamps and flips the input type when the new region has no timezone', async () => {
		const user = userEvent.setup();
		render(AlertForm, {
			regions: [ZONED, ZONELESS],
			submitLabel: 'Create alert',
			onsubmit: vi.fn(),
		});

		await user.selectOptions(screen.getByLabelText(/^Region/), '1');
		expect(startField().type).toBe('datetime-local');
		await user.type(startField(), '2026-08-16T02:45');
		await user.type(endField(), '2026-08-17T02:45');
		expect(startField().value).toBe('2026-08-16T02:45');

		// Crossing into a zoneless region turns the fields into raw RFC 3339
		// text inputs. A wall time left behind in one is not a valid RFC 3339
		// value, and a datetime-local silently drops a value it cannot parse --
		// so carrying either across would submit or display nonsense.
		await user.selectOptions(screen.getByLabelText(/^Region/), '16');
		expect(startField().type).toBe('text');
		expect(startField().value).toBe('');
		expect(endField().value).toBe('');
	});

	it('keeps the wall time when both regions have a timezone', async () => {
		const user = userEvent.setup();
		const other = region({
			id: 2,
			name: 'MTA New York',
			timezone: 'America/New_York',
		});
		render(AlertForm, {
			regions: [ZONED, other],
			submitLabel: 'Create alert',
			onsubmit: vi.fn(),
		});

		await user.selectOptions(screen.getByLabelText(/^Region/), '1');
		await user.type(startField(), '2026-08-16T02:45');
		// Both are date pickers, so the typed wall time survives and is simply
		// reinterpreted in the new zone -- which is what an operator correcting
		// a mis-picked region wants. Clearing here would be gratuitous.
		await user.selectOptions(screen.getByLabelText(/^Region/), '2');
		expect(startField().type).toBe('datetime-local');
		expect(startField().value).toBe('2026-08-16T02:45');
	});

	it('submits the region timezone alongside the form values', async () => {
		const user = userEvent.setup();
		const onsubmit = vi.fn().mockResolvedValue(undefined);
		render(AlertForm, {
			regions: [ZONED, ZONELESS],
			submitLabel: 'Create alert',
			onsubmit,
		});

		await user.selectOptions(screen.getByLabelText(/^Region/), '1');
		await user.type(screen.getByLabelText(/^Header/), 'Bridge out');
		await user.type(startField(), '2026-08-16T02:45');
		await user.click(screen.getByRole('button', { name: 'Create alert' }));

		expect(onsubmit).toHaveBeenCalledTimes(1);
		const [values, timezone] = onsubmit.mock.calls[0];
		expect(timezone).toBe('Asia/Kathmandu');
		expect(values.regionId).toBe('1');
		expect(values.start).toBe('2026-08-16T02:45');
	});

	// The server's message is the UX copy; the banner is the only place an
	// operator sees it.
	it('shows the rejection message from onsubmit in an alert banner', async () => {
		const user = userEvent.setup();
		render(AlertForm, {
			regions: [ZONED],
			submitLabel: 'Create alert',
			onsubmit: vi
				.fn()
				.mockRejectedValue(new Error('region 0 has no default agency id')),
		});

		await user.selectOptions(screen.getByLabelText(/^Region/), '1');
		await user.type(screen.getByLabelText(/^Header/), 'Bridge out');
		await user.type(startField(), '2026-08-16T02:45');
		await user.click(screen.getByRole('button', { name: 'Create alert' }));

		expect(await screen.findByRole('alert')).toHaveTextContent(
			'region 0 has no default agency id',
		);
	});

	// A region whose zone reads 'UTC' may simply be unconfigured: the column is
	// NOT NULL DEFAULT 'UTC' and the directory sync never fills it in.
	it('warns that a UTC region may be unconfigured', async () => {
		const user = userEvent.setup();
		render(AlertForm, {
			regions: [region({ id: 0, name: 'Tampa Bay', timezone: 'UTC' }), ZONED],
			submitLabel: 'Create alert',
			onsubmit: vi.fn(),
		});

		await user.selectOptions(screen.getByLabelText(/^Region/), '0');
		expect(screen.getByText(/nobody has configured yet/)).toBeInTheDocument();

		await user.selectOptions(screen.getByLabelText(/^Region/), '1');
		expect(screen.queryByText(/nobody has configured yet/)).toBeNull();
	});
});
