// Package clientip resolves the address every per-IP throttle keys on.
//
// The default is the TCP peer, which is the only value a client cannot
// forge. Behind a proxy that re-originates connections (Render, Cloudflare)
// every request shares the proxy's address and one bucket, so a deployment
// may opt in to a header that its proxy is known to overwrite on every
// request. Trusting a header the proxy merely forwards would let any client
// pick its own bucket, which is why there is no X-Forwarded-For mode: its
// first entry is whatever the client sent.
package clientip

import (
	"fmt"
	"net"
	"net/http"
	"net/textproto"
	"strings"
)

// Resolver returns the throttle key for a request.
type Resolver func(*http.Request) string

// Peer is the default Resolver: the connection's remote host, or the raw
// RemoteAddr when it carries no port.
func Peer(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// Header returns a Resolver that keys on the named header's address
// qualified by the TCP peer -- "peer|client" -- and on the peer alone when
// the header is absent or is not a bare IP address. Qualifying by the peer
// is what makes the header safe to trust: a request that reaches the origin
// without passing through the proxy (Render's own *.onrender.com hostname
// stays reachable beside a Cloudflare-proxied custom domain) can forge the
// header, but a forged value only subdivides that sender's own peer bucket;
// it can never join or escape anyone else's. Traffic that does come through
// the proxy shares the proxy's peer address and is told apart by the header,
// which is the whole point.
func Header(name string) Resolver {
	return func(r *http.Request) string {
		peer := Peer(r)
		v := strings.TrimSpace(r.Header.Get(name))
		if ip := net.ParseIP(v); ip != nil {
			return peer + "|" + ip.String()
		}
		return peer
	}
}

// Parse maps the SIDECAR_TRUSTED_PROXY setting to a Resolver. Accepted
// values, case-insensitively: "" or "off" (Peer), "cloudflare"
// (CF-Connecting-IP), "render" (True-Client-IP), and "header:<Name>" for
// any other proxy that overwrites a header of its own.
func Parse(setting string) (Resolver, error) {
	s := strings.TrimSpace(setting)
	switch strings.ToLower(s) {
	case "", "off":
		return Peer, nil
	case "cloudflare":
		return Header("CF-Connecting-IP"), nil
	case "render":
		return Header("True-Client-IP"), nil
	}
	if name, ok := strings.CutPrefix(strings.ToLower(s[:min(len(s), len("header:"))])+s[min(len(s), len("header:")):], "header:"); ok {
		name = strings.TrimSpace(name)
		if name == "" || !validHeaderName(name) {
			return nil, fmt.Errorf("trusted proxy: %q is not a valid header name", name)
		}
		return Header(textproto.CanonicalMIMEHeaderKey(name)), nil
	}
	return nil, fmt.Errorf("trusted proxy: unknown setting %q (want off, cloudflare, render, or header:<Name>)", setting)
}

// validHeaderName accepts RFC 9110 token characters only.
func validHeaderName(name string) bool {
	for _, c := range name {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.ContainsRune("!#$%&'*+-.^_`|~", c):
		default:
			return false
		}
	}
	return true
}
