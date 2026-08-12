// Timezone-explicit datetime mapping between <input type="datetime-local">
// values and the API's RFC 3339-with-offset contract. The API rejects naive
// datetimes, so every submit path MUST go through localInputToRFC3339.

/**
 * offsetMinutes returns the UTC offset, in minutes, that `timeZone` was on at
 * `instant`. Whole-minute granularity is load-bearing: Asia/Kathmandu is
 * +05:45, so rounding to hours would silently move times by 15 minutes.
 */
export function offsetMinutes(instant: Date, timeZone: string): number {
	const dtf = new Intl.DateTimeFormat('en-US', {
		timeZone,
		year: 'numeric',
		month: '2-digit',
		day: '2-digit',
		hour: '2-digit',
		minute: '2-digit',
		second: '2-digit',
		hour12: false,
	});
	const p = Object.fromEntries(
		dtf.formatToParts(instant).map((x) => [x.type, x.value]),
	);
	const asUTC = Date.UTC(
		+p.year,
		+p.month - 1,
		+p.day,
		+p.hour % 24,
		+p.minute,
		+p.second,
	);
	return Math.round((asUTC - instant.getTime()) / 60_000);
}

/**
 * localInputToRFC3339 turns a naive `<input type="datetime-local">` value
 * ("2026-01-15T14:00") into RFC 3339 with the offset `timeZone` was on at that
 * WALL TIME -- not the offset in effect today. A January date entered in July
 * must carry January's offset, or the alert fires an hour off.
 */
export function localInputToRFC3339(input: string, timeZone: string): string {
	const [d, t] = input.split('T');
	const [y, mo, da] = d.split('-').map(Number);
	const [hh, mm] = t.split(':').map(Number);
	const naiveUTC = Date.UTC(y, mo - 1, da, hh, mm, 0);
	// The offset at the target wall time may differ from the offset at the
	// naive instant (DST boundary); one refinement pass settles it.
	let off = offsetMinutes(new Date(naiveUTC), timeZone);
	off = offsetMinutes(new Date(naiveUTC - off * 60_000), timeZone);
	const sign = off < 0 ? '-' : '+';
	const abs = Math.abs(off);
	const oh = String(Math.floor(abs / 60)).padStart(2, '0');
	const om = String(abs % 60).padStart(2, '0');
	return `${d}T${t}:00${sign}${oh}:${om}`;
}

/**
 * instantToLocalInput renders an RFC 3339 instant as a naive
 * `<input type="datetime-local">` value in `timeZone` -- the REGION's zone,
 * never the browser's. An operator in Seattle editing a Tampa alert must see
 * Tampa's wall clock.
 */
export function instantToLocalInput(iso: string, timeZone: string): string {
	const instant = new Date(iso);
	const dtf = new Intl.DateTimeFormat('en-CA', {
		timeZone,
		year: 'numeric',
		month: '2-digit',
		day: '2-digit',
		hour: '2-digit',
		minute: '2-digit',
		hour12: false,
	});
	const p = Object.fromEntries(
		dtf.formatToParts(instant).map((x) => [x.type, x.value]),
	);
	return `${p.year}-${p.month}-${p.day}T${p.hour === '24' ? '00' : p.hour}:${p.minute}`;
}
