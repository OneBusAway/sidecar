// Wire shapes for the admin JSON API (design spec §5).
//
// These mirror the Go response structs field for field, snake_case included.
// There is deliberately no camelCase mapping layer: a mapper is one more place
// for the client's idea of the payload to drift from the server's.

/** One language's rendering of an alert. */
export interface Translation {
	language: string;
	/**
	 * null means this field has no translation at all, which is different from
	 * a translation whose text is the empty string.
	 */
	header: string | null;
	description: string | null;
}

/**
 * A service alert as the admin API returns it.
 *
 * TRAP -- `translations` is only populated by `GET /alerts/{id}`. The list
 * endpoint (`GET /alerts`) always returns `translations: []` on every item, no
 * matter how many translations the alert really has. On a list item an empty
 * array means "not loaded", never "none exist": never render a translation
 * count, a language badge, or a "no translations yet" state from a list
 * response -- fetch the single alert first.
 */
export interface Alert {
	id: number;
	/**
	 * Region 0 is a real region (Tampa Bay), never "unset". Anything that
	 * needs "no region selected" must spell it null/undefined, not 0.
	 */
	region_id: number;
	agency_id: string;
	header: string;
	description: string;
	url: string;
	cause: string;
	effect: string;
	severity: string;
	/** RFC 3339. Responses are UTC; requests MUST carry an explicit offset. */
	start_time: string;
	/** null means the feed's default fallback duration applies. */
	end_time: string | null;
	published: boolean;
	is_test: boolean;
	created_at: string;
	updated_at: string;
	translations: Translation[];
}

export type KeyStatus = 'region' | 'default' | 'none';

/** One admin route family a deployment may or may not have registered. */
export type AdminFeature =
	| 'alerts'
	| 'pushes'
	| 'surveys'
	| 'ghost_bus_reports'
	| 'alarms'
	| 'push_registrations'
	| 'api_keys';

/** A configured region. */
export interface Region {
	/** Region 0 is a real region (Tampa Bay), never "unset". */
	id: number;
	name: string;
	oba_base_url: string;
	sidecar_base_url: string;
	language: string;
	active: boolean;
	default_agency_id: string;
	/** IANA name. Every datetime the operator types is interpreted in it. */
	timezone: string;
	/** null until a directory sync supplies usable bounds. 0 is a real value. */
	latitude: number | null;
	/** null until a directory sync supplies usable bounds. 0 is a real value. */
	longitude: number | null;
	/**
	 * Key status, never the key -- the server never sends one back.
	 * 'region'  this region carries its own
	 * 'default' empty here, but the server has a process-wide default
	 * 'none'    nothing configured; vehicle search will fail
	 */
	oba_api_key: KeyStatus;
	/**
	 * TRAP -- like `Alert.translations`, this is only populated by one
	 * endpoint. `GET /regions/{id}` returns the admin route families this
	 * deployment registered for the region; `GET /regions` (the list) never
	 * includes it on any item. An absent array must read as "nothing is
	 * enabled", never as "everything is" -- see `hasFeature` in lib/regions,
	 * and never render a feature-gated control (the push Send button, a
	 * survey editor) from a list response.
	 */
	features?: AdminFeature[];
}

/** The body of `GET`/`POST /session`. */
export interface SessionUser {
	username: string;
}

/**
 * A push's lifecycle. `queued` and `sending` are the in-flight pair; the other
 * three are terminal and never change again (see `isInFlight` in lib/pushes).
 */
export type PushStatus = 'queued' | 'sending' | 'sent' | 'failed' | 'canceled';

/**
 * Who a push goes to. `test` is every device registered against a test alert's
 * region by the apps' internal builds; `all` is everyone.
 */
export type PushAudienceKind = 'all' | 'test';

/** One language's rendered notification text. */
export interface PushMessage {
	title: string;
	body: string;
}

/** One push of one alert, as the admin API returns it. */
export interface AlertPush {
	id: number;
	alert_id: number;
	region_id: number;
	audience: PushAudienceKind;
	status: PushStatus;
	/**
	 * The audience size fixed when the push started, so it is 0 until then.
	 * `submitted_count` can legitimately EXCEED it: a page that was resumed
	 * after a crash re-sends a bounded duplicate batch.
	 */
	device_count: number;
	submitted_count: number;
	failed_count: number;
	attempts: number;
	/** Always a string. `''` means no error, never null. */
	last_error: string;
	/** Language code to text, always including `'en'`. */
	messages: Record<string, PushMessage>;
	/** Grouped per-device failures, top 10 by count. Empty when there are none. */
	failure_reasons: { reason: string; count: number }[];
	created_at: string;
	/** null until the dispatcher picks the push up. */
	started_at: string | null;
	/** null until the push reaches a terminal status. */
	completed_at: string | null;
}

/** How many devices one audience covers, split by platform. */
export interface AudienceCount {
	total: number;
	ios: number;
	android: number;
}

/** The body of `GET /alerts/{id}/push_audience`. */
export interface PushAudience {
	all: AudienceCount;
	test: AudienceCount;
	/**
	 * True for a test alert, which can only ever go to test devices. The SPA
	 * hides the audience choice rather than offering one the API would reject.
	 */
	forced_test: boolean;
}
