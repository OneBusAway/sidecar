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

	// Reject anything that could still name another origin after the URL
	// parser has had its way with it:
	//
	//   '//evil.example'    protocol-relative.
	//   '/\evil.example'    the parser treats a backslash as a slash for
	//                       special schemes, so this is protocol-relative too.
	//   '/\tevil.example'   the parser DELETES U+0009/U+000A/U+000D from
	//                       anywhere in the input, so '/<TAB>/evil.example'
	//                       becomes '//evil.example' after parsing.
	//
	// The range covers the whole C0 block plus DEL rather than just those
	// three: no in-app path has a control character in it, and an allowlist
	// that enumerates only today's known tricks is the kind that ages badly.
	// eslint-disable-next-line no-control-regex
	if (target.startsWith('//') || /[\\\u0000-\u001f\u007f]/.test(target))
		return home;

	const base = home.endsWith('/') ? home.slice(0, -1) : home;
	// No special case for an empty base (paths.base = ''): the checks below
	// reduce to "starts with / or ?", which is exactly right once the guard
	// above has removed the strings that leave the origin.
	if (
		target === base ||
		target.startsWith(`${base}/`) ||
		target.startsWith(`${base}?`)
	) {
		return target;
	}
	return home;
}
