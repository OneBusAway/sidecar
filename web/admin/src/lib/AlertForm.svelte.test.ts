// Component tests, in the `component` vitest project (jsdom).
//
// This behaviour cannot be lifted into a plain module: it is the form's
// reaction to its `region` prop, which drives a branch in the markup (the
// zoned/zoneless field switch). Without a DOM project, the
// whole node suite stays green even if these branches are gutted.
import { describe, expect, it, vi } from 'vitest';
import { render, screen } from '@testing-library/svelte';
import userEvent from '@testing-library/user-event';

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
		latitude: null,
		longitude: null,
		oba_api_key: 'none',
		...over,
	};
}

// A zoned region and a zoneless one. Zoneless is what the directory sync
// gives a new region: '' until an operator sets a zone on the Regions screen.
const ZONED = region({
	id: 1,
	name: 'Puget Sound',
	timezone: 'Asia/Kathmandu',
});
const ZONELESS = region({ id: 16, name: 'Davis, CA', timezone: '' });

const startField = () => screen.getByLabelText(/^Start/) as HTMLInputElement;
const endField = () => screen.getByLabelText(/^End/) as HTMLInputElement;

describe('AlertForm region rendering', () => {
	// Region is a prop now, not a form field: it comes from the page's URL
	// (see routes/regions/[region]/...), and offering a control the API would
	// refuse -- region is immutable through PATCH -- would be a lie about what
	// the form can do.
	it('renders no region selector', () => {
		render(AlertForm, {
			region: ZONED,
			submitLabel: 'Create alert',
			onsubmit: vi.fn(),
		});
		expect(screen.queryByLabelText(/^Region/)).toBeNull();
	});

	it('renders a date picker for a region with a configured timezone', () => {
		render(AlertForm, {
			region: ZONED,
			submitLabel: 'Create alert',
			onsubmit: vi.fn(),
		});
		expect(startField().type).toBe('datetime-local');
		expect(endField().type).toBe('datetime-local');
	});

	// No date picker without a zone: a datetime-local value is a naive wall
	// time, and turning it into an instant needs an offset this app must not
	// invent (the browser's zone is the operator's, not the region's).
	it('renders a raw RFC 3339 text field for a region with no timezone', async () => {
		const user = userEvent.setup();
		render(AlertForm, {
			region: ZONELESS,
			submitLabel: 'Create alert',
			onsubmit: vi.fn(),
		});
		expect(startField().type).toBe('text');
		expect(endField().type).toBe('text');
		expect(
			screen.getByText(/Davis, CA has no configured timezone/),
		).toBeInTheDocument();
		expect(
			screen.getByText(/times must be typed as RFC 3339/),
		).toBeInTheDocument();
		expect(
			screen.getByRole('link', {
				name: /Set a timezone on the Regions screen/,
			}),
		).toHaveAttribute('href', '/admin/regions');

		await user.type(startField(), '2026-08-15T14:00:00-04:00');
		expect(startField().value).toBe('2026-08-15T14:00:00-04:00');
	});

	it('submits the region timezone alongside the form values', async () => {
		const user = userEvent.setup();
		const onsubmit = vi.fn().mockResolvedValue(undefined);
		render(AlertForm, { region: ZONED, submitLabel: 'Create alert', onsubmit });

		await user.type(screen.getByLabelText(/^Header/), 'Bridge out');
		await user.type(startField(), '2026-08-16T02:45');
		await user.click(screen.getByRole('button', { name: 'Create alert' }));

		expect(onsubmit).toHaveBeenCalledTimes(1);
		const [values, timezone] = onsubmit.mock.calls[0];
		expect(timezone).toBe('Asia/Kathmandu');
		expect(values.start).toBe('2026-08-16T02:45');
	});

	// The server's message is the UX copy; the banner is the only place an
	// operator sees it.
	it('shows the rejection message from onsubmit in an alert banner', async () => {
		const user = userEvent.setup();
		render(AlertForm, {
			region: ZONED,
			submitLabel: 'Create alert',
			onsubmit: vi
				.fn()
				.mockRejectedValue(new Error('region 0 has no default agency id')),
		});

		await user.type(screen.getByLabelText(/^Header/), 'Bridge out');
		await user.type(startField(), '2026-08-16T02:45');
		await user.click(screen.getByRole('button', { name: 'Create alert' }));

		expect(await screen.findByRole('alert')).toHaveTextContent(
			'region 0 has no default agency id',
		);
	});

	// UTC is a real zone now: the directory sync stores '' for a region nobody
	// has configured, so a stored 'UTC' is one an operator chose. It gets the
	// same date picker as any other zone, with no "unconfigured" hint.
	it('treats a UTC region as configured', () => {
		render(AlertForm, {
			region: region({ id: 0, name: 'Tampa Bay', timezone: 'UTC' }),
			submitLabel: 'Create alert',
			onsubmit: vi.fn(),
		});
		expect(startField().type).toBe('datetime-local');
		expect(screen.getByText('(UTC)')).toBeInTheDocument();
		expect(screen.queryByText(/no configured timezone/)).toBeNull();
		expect(screen.queryByText(/configured yet/)).toBeNull();
	});
});
