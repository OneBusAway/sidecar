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
}

/** The body of `GET`/`POST /session`. */
export interface SessionUser {
	username: string;
}
