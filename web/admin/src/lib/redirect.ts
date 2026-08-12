/**
 * safeRedirect picks where to send an operator after sign-in.
 *
 * `?redirectTo=` is written by this app, but it arrives through the address
 * bar, so it is attacker-supplied in practice: anyone can hand an operator a
 * link to the login page carrying whatever destination they like. Only paths
 * inside this app's base are honoured; everything else falls back to `home`.
 *
 * `home` is `resolve('/')`, which is the base plus a trailing slash
 * ('/admin/'), while the pathnames that end up in `?redirectTo=` have none
 * ('/admin/alerts/7'). The comparison is against the base without it, or
 * every deep link would be thrown away and land the operator at the root.
 */
export function safeRedirect(
	target: string | null | undefined,
	home: string,
): string {
	if (!target) return home;

	// Both '//evil.example' and '/\evil.example' begin with a single '/' yet
	// resolve to another origin -- the URL parser treats a backslash here the
	// way it treats a slash. Neither has any business in an in-app path.
	if (target.startsWith('//') || target.includes('\\')) return home;

	const base = home.endsWith('/') ? home.slice(0, -1) : home;
	// paths.base is '/admin' today, but an empty base is legal; then any
	// rooted path is in-app (the two checks above already excluded the ones
	// that would leave the origin).
	if (base === '') return target.startsWith('/') ? target : home;

	if (
		target === base ||
		target.startsWith(`${base}/`) ||
		target.startsWith(`${base}?`)
	) {
		return target;
	}
	return home;
}
