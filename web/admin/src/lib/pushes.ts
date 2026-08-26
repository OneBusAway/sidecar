// View logic for the alert detail page's Push notification card (design spec
// §2.10).
//
// Like lib/alerts, everything here is a pure function on plain data (plus the
// two loaders, which only wrap `api`): the vitest node project has no DOM, so
// anything that lives in a component's markup has no automated coverage. The
// audience wording and the progress arithmetic are exactly the places a
// mistake is both easy to make and expensive -- an operator reading "3 test
// devices" and hitting 1,200 real ones -- so they live out here.

import { api, ApiError } from './api';
import type { Badge } from './alerts';
import type {
	AlertPush,
	PushAudience,
	PushAudienceKind,
	PushStatus,
} from './types';

// 'en-US' rather than the browser's locale, deliberately: the admin UI is
// English-only and a machine set to de-DE would otherwise render 1200 as
// "1.200" beside English copy.
const numbers = new Intl.NumberFormat('en-US');

/** count formats a device count with thousands separators. */
function count(n: number): string {
	return numbers.format(n);
}

/** devices renders a count with a correctly pluralised noun. */
function devices(n: number, noun = 'device'): string {
	return `${count(n)} ${noun}${n === 1 ? '' : 's'}`;
}

/** One entry of the card's audience radio group. */
export interface AudienceOption {
	value: PushAudienceKind;
	label: string;
}

/**
 * audienceOptions lists the audiences the operator may pick.
 *
 * A test alert forces the test audience -- the API rejects anything else -- so
 * the "everyone" option is not offered at all rather than offered and refused.
 */
export function audienceOptions(a: PushAudience): AudienceOption[] {
	const test: AudienceOption = {
		value: 'test',
		label: `Test devices (${count(a.test.total)})`,
	};
	if (a.forced_test) return [test];
	return [{ value: 'all', label: `Everyone (${devices(a.all.total)})` }, test];
}

/**
 * isInFlight reports whether a push may still change. It drives both the
 * Cancel button and the card's 3-second poll, so the terminal statuses must
 * stay out of it: treating `failed` as in flight would poll the page forever.
 */
export function isInFlight(p: Pick<AlertPush, 'status'>): boolean {
	return p.status === 'queued' || p.status === 'sending';
}

/**
 * progressLabel summarises one push's delivery counts.
 *
 * `device_count` is 0 until the dispatcher fixes the audience, which reads as
 * "pending" rather than as "0 sent of 0". The numbers are otherwise printed as
 * they arrive, including a `submitted_count` larger than `device_count`: a
 * resumed page re-sends a bounded duplicate batch, and clamping that to look
 * tidy would hide the duplicate from the only person who can act on it.
 */
export function progressLabel(
	p: Pick<AlertPush, 'device_count' | 'submitted_count' | 'failed_count'>,
): string {
	if (p.device_count === 0) return 'pending';
	return `${count(p.submitted_count)} sent · ${count(p.failed_count)} failed · of ${count(p.device_count)}`;
}

/**
 * audiencePhrase names an audience by size: "1,200 devices", "3 test devices".
 *
 * The button and the confirm() share it deliberately -- the operator reads the
 * count twice and must see the same number both times, or the dialog looks
 * like it is confirming something other than what they clicked.
 */
function audiencePhrase(kind: PushAudienceKind, a: PushAudience): string {
	const n = kind === 'test' ? a.test.total : a.all.total;
	return kind === 'test' ? devices(n, 'test device') : devices(n);
}

/**
 * sendButtonLabel is the Send button's text, which names the audience size
 * (design spec §2.10). The button is the last thing read before the click, so
 * the count belongs on it and not only in the dialog behind it.
 */
export function sendButtonLabel(
	kind: PushAudienceKind,
	a: PushAudience,
): string {
	return `Send push to ${audiencePhrase(kind, a)}`;
}

/**
 * sendConfirmMessage is the text of the confirm() shown before queueing, and
 * names the number of real devices about to be woken up. A push cannot be
 * unsent, so the size is the one fact the dialog must carry.
 */
export function sendConfirmMessage(
	kind: PushAudienceKind,
	a: PushAudience,
): string {
	return `Send this alert as a push notification to ${audiencePhrase(kind, a)}?`;
}

/**
 * statusTone maps a push status onto the alert badges' existing tones, so the
 * card reuses the page's `.badge-*` CSS instead of inventing a second palette:
 * sent is the green "done" tone, failed and canceled the amber "look at this"
 * tone, and the in-flight pair the neutral grey.
 */
export function statusTone(status: PushStatus): Badge['tone'] {
	switch (status) {
		case 'sent':
			return 'published';
		case 'failed':
		case 'canceled':
			return 'test';
		default:
			return 'draft';
	}
}

/**
 * notConfigured reports the "this server has no push transport" signal.
 *
 * When gorush is unconfigured the four push routes are never registered, so
 * the request falls through to the router's plain 404 with a non-JSON body.
 * The alert itself being missing produces the same status here, but that case
 * is already fatal to the page: the load fetches the alert alongside these,
 * and its 404 becomes the error page before the card is ever rendered.
 */
function notConfigured(err: unknown): boolean {
	return err instanceof ApiError && err.status === 404;
}

/**
 * loadPushes reads an alert's push history, newest first, or an empty history
 * when the server has no push transport.
 */
export async function loadPushes(id: string | number): Promise<AlertPush[]> {
	try {
		return await api.get<AlertPush[]>(`/alerts/${id}/pushes`);
	} catch (err) {
		if (notConfigured(err)) return [];
		throw err;
	}
}

/**
 * loadAudience reads how many devices each audience covers, or null when the
 * server has no push transport -- which is what the card renders its "not
 * configured" state from.
 */
export async function loadAudience(
	id: string | number,
): Promise<PushAudience | null> {
	try {
		return await api.get<PushAudience>(`/alerts/${id}/push_audience`);
	} catch (err) {
		if (notConfigured(err)) return null;
		throw err;
	}
}
