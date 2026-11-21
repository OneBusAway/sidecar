// Package httpx holds the small HTTP-client policies the sidecar's upstream
// clients share. It depends on nothing else in this repo, so a client package
// can use it without being coupled to any sibling.
package httpx

import (
	"errors"
	"io"
	"net/http"
)

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

// ErrResponseTooLarge is the read error of a body CapResponseBodies cut off.
var ErrResponseTooLarge = errors.New("httpx: response body exceeds the size cap")

// CapResponseBodies returns a copy of c whose response bodies fail with
// ErrResponseTooLarge once more than max bytes have been read, so a broken
// or hostile upstream cannot make a caller -- or an SDK decoding on its
// behalf -- buffer an unbounded body. Copied, not mutated, for the same
// reason as NoRedirectClient; a nil client or transport means the default.
func CapResponseBodies(c *http.Client, max int64) *http.Client {
	if c == nil {
		c = http.DefaultClient
	}
	copied := *c
	next := copied.Transport
	if next == nil {
		next = http.DefaultTransport
	}
	copied.Transport = cappedTransport{next: next, max: max}
	return &copied
}

type cappedTransport struct {
	next http.RoundTripper
	max  int64
}

func (t cappedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.next.RoundTrip(req)
	if err != nil || resp.Body == nil {
		return resp, err
	}
	resp.Body = &cappedBody{rc: resp.Body, left: t.max}
	return resp, nil
}

// cappedBody reads up to left bytes, then fails if the body has more.
type cappedBody struct {
	rc   io.ReadCloser
	left int64
}

func (b *cappedBody) Read(p []byte) (int, error) {
	if b.left <= 0 {
		var one [1]byte
		n, err := b.rc.Read(one[:])
		if n > 0 {
			return 0, ErrResponseTooLarge
		}
		return 0, err
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.rc.Read(p)
	b.left -= int64(n)
	return n, err
}

func (b *cappedBody) Close() error { return b.rc.Close() }
