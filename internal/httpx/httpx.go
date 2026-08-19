// Package httpx holds the small HTTP-client policies the sidecar's upstream
// clients share. It depends on nothing else in this repo, so a client package
// can use it without being coupled to any sibling.
package httpx

import "net/http"

// NoRedirectClient returns a copy of c that refuses to follow redirects.
//
// Both upstream APIs this sidecar calls carry their API key in the URL --
// OneBusAway as a query parameter, Pirate Weather as a path segment -- and
// Go's default client sets Referer to the previous full URL, suppressing it
// only on an https-to-http downgrade. So a single redirect hop hands the
// credential to a host the operator never chose to contact, which is a worse
// exposure than a key written to our own logs. Refusing to follow redirects
// turns that into an ordinary non-2xx response, which every caller here
// already handles.
//
// The client is copied rather than mutated: setting CheckRedirect on the
// caller's *http.Client would silently change redirect behavior for anything
// else sharing it, and production passes http.DefaultClient.
func NoRedirectClient(c *http.Client) *http.Client {
	if c == nil {
		c = http.DefaultClient
	}
	copied := *c
	copied.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &copied
}
