// Component tests for the Regions screen, in the `component` vitest project
// (jsdom).
//
// The "not configured" marker lives only in markup: it reads the saved
// region's timezone, which the directory sync leaves as '' until an operator
// sets one. A marker keyed on the wrong value -- the old 'UTC' default, or
// the input instead of the saved region -- would pass every node-project
// test.
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { render, screen } from '@testing-library/svelte';
import userEvent from '@testing-library/user-event';

const { patchMock } = vi.hoisted(() => ({ patchMock: vi.fn() }));

vi.mock('$lib/api', async () => {
	const actual = await vi.importActual<typeof import('$lib/api')>('$lib/api');
	return { ...actual, api: { ...actual.api, patch: patchMock } };
});

import RegionsPage from './+page.svelte';
import type { Region } from '$lib/types';

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

const UNCONFIGURED = region({ id: 0, name: 'Tampa Bay', timezone: '' });
const ON_UTC = region({ id: 1, name: 'Reykjavik', timezone: 'UTC' });
const ZONED = region({
	id: 5,
	name: 'Puget Sound',
	timezone: 'America/Los_Angeles',
});

const MARKER = /Not configured — alert times need an explicit offset/;

function renderPage(regions: Region[]) {
	// The page reads only `data.regions`; the rest of PageProps is unused.
	render(RegionsPage, { data: { regions } } as never);
}

// The marker sits in the same cell as the region's timezone input, so this
// finds it by walking from the input rather than by text alone.
function timezoneCell(name: string): HTMLElement {
	const input = screen.getByLabelText(`Timezone for ${name}`);
	return input.closest('td') as HTMLElement;
}

describe('Regions screen timezone column', () => {
	beforeEach(() => {
		patchMock.mockReset();
	});

	it('marks a region with no timezone as not configured', () => {
		renderPage([UNCONFIGURED, ON_UTC, ZONED]);
		expect(timezoneCell('Tampa Bay').textContent).toMatch(MARKER);
		expect(screen.getAllByText(MARKER)).toHaveLength(1);
	});

	// UTC used to be the column default, so a stored 'UTC' was flagged as
	// possibly unconfigured. The sync now stores '' instead, so UTC is a zone
	// an operator chose and gets no marker.
	it('does not mark a UTC or other zoned region', () => {
		renderPage([ON_UTC, ZONED]);
		expect(screen.queryByText(MARKER)).toBeNull();
		expect(screen.queryByText(/default — not configured/)).toBeNull();
	});

	// An empty field must not look filled in: the placeholder reads as an
	// example, not as the region's zone.
	it('shows the timezone placeholder as an example', () => {
		renderPage([UNCONFIGURED]);
		const input = screen.getByLabelText(
			'Timezone for Tampa Bay',
		) as HTMLInputElement;
		expect(input.value).toBe('');
		expect(input.placeholder).toBe('e.g. America/New_York');
	});

	// The marker reads the saved region, so typing a zone leaves it in place
	// until the server confirms the save.
	it('keeps the marker while typing and clears it after a save', async () => {
		const user = userEvent.setup();
		patchMock.mockResolvedValue({
			...UNCONFIGURED,
			timezone: 'America/New_York',
		});
		renderPage([UNCONFIGURED]);

		await user.type(
			screen.getByLabelText('Timezone for Tampa Bay'),
			'America/New_York',
		);
		expect(screen.getByText(MARKER)).toBeInTheDocument();

		await user.click(screen.getByRole('button', { name: 'Save' }));
		expect(await screen.findByText('Saved')).toBeInTheDocument();
		expect(patchMock).toHaveBeenCalledWith('/regions/0', {
			default_agency_id: 'HART',
			timezone: 'America/New_York',
		});
		expect(screen.queryByText(MARKER)).toBeNull();
	});
});
