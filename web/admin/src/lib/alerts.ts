// View logic for the alert screens.
//
// Everything here is a pure function on plain data, deliberately: the vitest
// project runs in `environment: 'node'` with no DOM, so logic that lives in a
// component's markup has no automated coverage at all. The region filter, the
// timestamp mapping and the request payloads are exactly where a mistake is
// both easy and expensive, so they live out here where a test can reach them
// and the components stay thin.

import { localInputToRFC3339, instantToLocalInput } from './datetime';
import { DEFAULT_CAUSE, DEFAULT_EFFECT, DEFAULT_SEVERITY } from './enums';
import type { Alert, Region } from './types';

/**
 * ALL_REGIONS is the region filter's "no filter" value.
 *
 * It is the empty string and never 0: region 0 is Tampa Bay, a real region.
 * Anything that tests a region id for truthiness is a bug -- selecting Tampa
 * Bay would silently mean "show everything".
 */
export const ALL_REGIONS = '';

/** regionById finds a region by id, or undefined. Handles region 0. */
export function regionById(regions: Region[], id: number): Region | undefined {
	return regions.find((r) => r.id === id);
}

/**
 * regionName is the display name for an alert's region. An id with no
 * matching region still renders as something an operator can act on rather
 * than as a blank cell.
 */
export function regionName(regions: Region[], id: number): string {
	return regionById(regions, id)?.name ?? `region ${id}`;
}

/**
 * selectedRegion resolves the alert form's region <select> value.
 *
 * The empty-string check comes FIRST and is exact. `Number('')` is 0, so
 * looking the value up before testing it would resolve "nothing chosen" to
 * region 0 -- Tampa Bay -- and hand the form Tampa's timezone, agency default
 * and identity for an alert the operator has not assigned yet.
 */
export function selectedRegion(
	regions: Region[],
	value: string,
): Region | undefined {
	if (value === '') return undefined;
	return regionById(regions, Number(value));
}

/**
 * filterByRegion applies the list screen's region filter. `filter` is the
 * <select>'s string value: ALL_REGIONS ('') means every region, and "0" means
 * region 0 -- not "unset".
 */
export function filterByRegion(list: Alert[], filter: string): Alert[] {
	if (filter === ALL_REGIONS) return list;
	const id = Number(filter);
	return list.filter((a) => a.region_id === id);
}

/**
 * formatInstantForRegion renders an API instant for display in a region's own
 * timezone. A region with no configured timezone falls back to the raw UTC
 * string from the server: showing a wall-clock time in a guessed zone would be
 * a lie, and the browser's zone is a guess.
 */
export function formatInstantForRegion(iso: string, timezone: string): string {
	if (!timezone) return iso;
	return instantToLocalInput(iso, timezone).replace('T', ' ');
}

/** One status badge on the list screen. `tone` picks the CSS class. */
export interface Badge {
	label: string;
	tone: 'published' | 'draft' | 'test';
}

/**
 * alertBadges describes an alert's status. Published and test are independent:
 * a published test alert carries both badges, because "published" alone would
 * hide that riders on the real feed never see it.
 */
export function alertBadges(a: Pick<Alert, 'published' | 'is_test'>): Badge[] {
	const badges: Badge[] = [
		a.published
			? { label: 'Published', tone: 'published' }
			: { label: 'Draft', tone: 'draft' },
	];
	if (a.is_test) badges.push({ label: 'Test', tone: 'test' });
	return badges;
}

/**
 * toInstant maps one form timestamp onto the API's RFC 3339-with-offset
 * contract.
 *
 * With a region timezone the field is a `datetime-local` value and the zone
 * supplies the offset. WITHOUT one the field is a raw RFC 3339 text input and
 * the value passes through untouched -- the API rejects a naive datetime with
 * a 400 that names the requirement, which is the right answer. What must never
 * happen is guessing: stamping the browser's offset onto a Tampa alert typed
 * in Seattle would move it three hours with nothing to show for it.
 *
 * An empty value also passes through, so the server's "must be RFC 3339"
 * message is what the operator sees rather than a TypeError from splitting it.
 */
export function toInstant(value: string, timezone: string): string {
	const v = value.trim();
	if (v === '' || timezone === '') return v;
	return localInputToRFC3339(v, timezone);
}

/**
 * fromInstant is toInstant's inverse: it fills a form field from an API
 * instant. With a region timezone the field is a `datetime-local` value in
 * THAT zone -- an operator in Seattle editing a Tampa alert must see Tampa's
 * wall clock, not their own. With no configured zone the raw RFC 3339 string
 * goes into the raw text input unchanged, so that pair round-trips exactly.
 *
 * THE ZONED PAIR IS PRECISE TO THE MINUTE, NOT THE SECOND. `datetime-local`
 * without `step` is a minute-granularity control, `instantToLocalInput` emits
 * `YYYY-MM-DDTHH:MM`, and `localInputToRFC3339` appends `:00`. So an alert
 * created by the CLI at 21:00:30Z, opened here and saved untouched, is stored
 * back as 21:00:00Z.
 *
 * That is deliberate rather than overlooked. This form authors service alerts,
 * where the meaningful unit is the minute; carrying seconds through would mean
 * `step="1"` and a seconds spinner in every picker, making the common case
 * worse to serve a precision nothing in the product uses. The rounding is at
 * most 59 seconds, it is visible in the field before the operator saves, and
 * `alerts.test.ts` pins it ("truncates seconds..."), so it is a stated
 * behaviour and not a silent one. Change both helpers together, and add
 * `step="1"` to both inputs, if that ever stops being true.
 */
export function fromInstant(iso: string, timezone: string): string {
	if (timezone === '') return iso;
	return instantToLocalInput(iso, timezone);
}

/** The editable state of the alert form, as strings straight off the inputs. */
export interface AlertFormValues {
	/** The region <select>'s value. '' means nothing chosen; '0' is Tampa Bay. */
	regionId: string;
	agencyId: string;
	header: string;
	description: string;
	url: string;
	cause: string;
	effect: string;
	severity: string;
	/** Either a datetime-local value or a raw RFC 3339 string; see toInstant. */
	start: string;
	end: string;
	/** Edit only: drop the end time and revert to the feed's fallback duration. */
	clearEnd: boolean;
	isTest: boolean;
}

/**
 * blankFormValues is the create form's starting state. The three enum
 * defaults are the same fallbacks alerts.ParseCause and friends apply to an
 * empty value, so an operator who touches none of them gets what the CLI
 * would have given them.
 */
export function blankFormValues(): AlertFormValues {
	return {
		regionId: '',
		agencyId: '',
		header: '',
		description: '',
		url: '',
		cause: DEFAULT_CAUSE,
		effect: DEFAULT_EFFECT,
		severity: DEFAULT_SEVERITY,
		start: '',
		end: '',
		clearEnd: false,
		isTest: false,
	};
}

/**
 * formValuesFromAlert prefills the edit form. Paired with buildPatchPayload it
 * round-trips: loading an alert and saving it without touching a field must
 * submit the same instants it was loaded with, or every save walks the alert
 * through time.
 */
export function formValuesFromAlert(
	a: Alert,
	timezone: string,
): AlertFormValues {
	return {
		regionId: String(a.region_id),
		agencyId: a.agency_id,
		header: a.header,
		description: a.description,
		url: a.url,
		cause: a.cause,
		effect: a.effect,
		severity: a.severity,
		start: fromInstant(a.start_time, timezone),
		end: a.end_time === null ? '' : fromInstant(a.end_time, timezone),
		clearEnd: false,
		isTest: a.is_test,
	};
}

/** The POST /alerts body. */
export interface CreateAlertPayload {
	region_id: number;
	agency_id: string;
	header: string;
	description: string;
	url: string;
	cause: string;
	effect: string;
	severity: string;
	start_time: string;
	end_time?: string;
	is_test: boolean;
}

/**
 * The PATCH /alerts/{id} body. No region_id: region is immutable through the
 * API, which is why the form's region select is disabled when editing.
 */
export interface PatchAlertPayload {
	agency_id: string;
	header: string;
	description: string;
	url: string;
	cause: string;
	effect: string;
	severity: string;
	start_time: string;
	end_time?: string;
	clear_end_time?: boolean;
	is_test: boolean;
}

/**
 * buildCreatePayload turns the form state into a POST /alerts body.
 *
 * agency_id is sent as typed, empty included: the server resolves an empty one
 * against the region's default and, if the region has none either, answers
 * with the message telling the operator to set one. Re-deriving that here
 * would only give the same problem a second, worse wording.
 *
 * Throws when no region is chosen. That is the one piece of client-side
 * validation, and only because there is no sensible payload to send: an absent
 * region_id cannot be spelled 0.
 */
export function buildCreatePayload(
	v: AlertFormValues,
	timezone: string,
): CreateAlertPayload {
	if (v.regionId === '') throw new Error('choose a region');
	const payload: CreateAlertPayload = {
		region_id: Number(v.regionId),
		agency_id: v.agencyId.trim(),
		header: v.header,
		description: v.description,
		url: v.url.trim(),
		cause: v.cause,
		effect: v.effect,
		severity: v.severity,
		start_time: toInstant(v.start, timezone),
		is_test: v.isTest,
	};
	// Absent, not null: the field means "no end time", and the API spells that
	// by leaving it out.
	const end = toInstant(v.end, timezone);
	if (end !== '') payload.end_time = end;
	return payload;
}

/**
 * buildPatchPayload turns the form state into a PATCH /alerts/{id} body.
 *
 * Every editable field is sent rather than only the changed ones. The endpoint
 * is a patch, so that is a legal (if chatty) request, and diffing against the
 * loaded alert would add a second source of truth about what the operator
 * meant for no user-visible gain.
 *
 * An emptied end field means "no end time", exactly like ticking the clear
 * checkbox: the alternative is to drop it on the floor and leave the old end
 * time in place while the form shows it blank. The two are never sent together
 * -- the API rejects that pair -- so the checkbox wins when both are set.
 */
export function buildPatchPayload(
	v: AlertFormValues,
	timezone: string,
): PatchAlertPayload {
	const payload: PatchAlertPayload = {
		agency_id: v.agencyId.trim(),
		header: v.header,
		description: v.description,
		url: v.url.trim(),
		cause: v.cause,
		effect: v.effect,
		severity: v.severity,
		start_time: toInstant(v.start, timezone),
		is_test: v.isTest,
	};
	const end = v.clearEnd ? '' : toInstant(v.end, timezone);
	if (end === '') payload.clear_end_time = true;
	else payload.end_time = end;
	return payload;
}

/** The PUT /alerts/{id}/translations/{lang} body. */
export interface TranslationPayload {
	header?: string;
	description?: string;
}

/**
 * buildTranslationPayload includes only the fields the operator actually
 * filled in: an absent field leaves that language's existing translation
 * alone, while an empty string would store a real, empty translation and hand
 * riders a blank header.
 *
 * Both blank yields `{}`, which the API answers with "provide header and/or
 * description" -- its wording, not a second copy of it here.
 */
export function buildTranslationPayload(
	header: string,
	description: string,
): TranslationPayload {
	const payload: TranslationPayload = {};
	if (header.trim() !== '') payload.header = header;
	if (description.trim() !== '') payload.description = description;
	return payload;
}
