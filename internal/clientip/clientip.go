// Package clientip resolves the address every per-IP throttle keys on.
//
// The default is the TCP peer, which is the only value a client cannot
// forge. Behind a proxy that re-originates connections (Render, Cloudflare)
// every request shares the proxy's address and one bucket, so a deployment
// may opt in to a header that its proxy overwrites on every request. The
// header is honoured only for requests proven to have come through that
// proxy -- the peer is inside a configured CIDR, or the request carries a
// shared secret the proxy adds -- because an origin that is also reachable
// directly (Render's *.onrender.com hostname beside a Cloudflare-proxied
// domain) would otherwise let any caller mint a fresh bucket per request
// by varying the header. There is no X-Forwarded-For mode: its first entry
// is whatever the client sent.
package clientip

import (
	"crypto/subtle"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/textproto"
	"strings"
)

// SecretHeader is the request header a proxy adds to prove a request came
// through it (a Cloudflare Transform Rule can set it on the proxied
// route). Its value is compared to Options.Secret.
const SecretHeader = "X-Sidecar-Proxy-Secret"

const headerPrefix = "header:"

// Resolver returns the throttle key for a request.
type Resolver func(*http.Request) string

// Options says how a header-reading Resolver decides a request really came
// through the proxy. At least one of Prefixes or Secret is required for
// every header mode except "render", which defaults to the private
// address ranges (only Render's own proxy reaches a service there).
type Options struct {
	// Prefixes are peer address ranges the proxy connects from.
	Prefixes []netip.Prefix
	// Secret is the value the proxy sends in SecretHeader.
	Secret string
}

// Peer is the default Resolver: the connection's remote host, or the raw
// RemoteAddr when it carries no port.
func Peer(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// Header returns a Resolver that reads the named header for requests the
// options prove came through the proxy, and falls back to Peer for every
// other request and whenever the header is absent or not a bare IP
// address. The fallback keeps a misconfigured deployment throttling on
// something rather than on the empty string, which every request would
// share.
func Header(name string, opts Options) Resolver {
	return func(r *http.Request) string {
		if !viaProxy(r, opts) {
			return Peer(r)
		}
		v := strings.TrimSpace(r.Header.Get(name))
		if ip := net.ParseIP(v); ip != nil {
			return ip.String()
		}
		return Peer(r)
	}
}

func viaProxy(r *http.Request, opts Options) bool {
	if opts.Secret != "" && subtle.ConstantTimeCompare([]byte(r.Header.Get(SecretHeader)), []byte(opts.Secret)) == 1 {
		return true
	}
	if len(opts.Prefixes) == 0 {
		return false
	}
	peer, err := netip.ParseAddr(Peer(r))
	if err != nil {
		return false
	}
	peer = peer.Unmap()
	for _, p := range opts.Prefixes {
		if p.Contains(peer) {
			return true
		}
	}
	return false
}

// PrivatePrefixes are the RFC 1918 / RFC 4193 ranges plus loopback: what a
// platform's own proxy connects from on its private network.
var PrivatePrefixes = []netip.Prefix{
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("::1/128"),
}

// Parse maps the SIDECAR_TRUSTED_PROXY setting to a Resolver. Accepted
// values, case-insensitively: "" or "off" (Peer), "cloudflare"
// (CF-Connecting-IP), "render" (True-Client-IP), and "header:<Name>" for
// any other proxy that overwrites a header of its own. Header modes need
// opts to prove the proxy hop; "render" falls back to PrivatePrefixes.
func Parse(setting string, opts Options) (Resolver, error) {
	s := strings.TrimSpace(setting)
	lower := strings.ToLower(s)
	var name string
	switch {
	case lower == "" || lower == "off":
		return Peer, nil
	case lower == "cloudflare":
		name = "CF-Connecting-IP"
	case lower == "render":
		name = "True-Client-IP"
		if len(opts.Prefixes) == 0 && opts.Secret == "" {
			opts.Prefixes = PrivatePrefixes
		}
	case strings.HasPrefix(lower, headerPrefix):
		name = strings.TrimSpace(s[len(headerPrefix):])
		if name == "" || !validHeaderName(name) {
			return nil, fmt.Errorf("trusted proxy: %q is not a valid header name", name)
		}
		name = textproto.CanonicalMIMEHeaderKey(name)
	default:
		return nil, fmt.Errorf("trusted proxy: unknown setting %q (want off, cloudflare, render, or header:<Name>)", setting)
	}
	if len(opts.Prefixes) == 0 && opts.Secret == "" {
		return nil, fmt.Errorf("trusted proxy: %q needs SIDECAR_TRUSTED_PROXY_SECRET or SIDECAR_TRUSTED_PROXY_CIDRS to tell proxied requests from direct ones", s)
	}
	return Header(name, opts), nil
}

// ParsePrefixes reads a comma-separated CIDR list (SIDECAR_TRUSTED_PROXY_CIDRS).
func ParsePrefixes(list string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, item := range strings.Split(list, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		p, err := netip.ParsePrefix(item)
		if err != nil {
			return nil, fmt.Errorf("trusted proxy cidrs: %w", err)
		}
		out = append(out, p)
	}
	return out, nil
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
