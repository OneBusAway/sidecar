package clientip_test

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/OneBusAway/sidecar/internal/clientip"
)

var secret = clientip.Options{Secret: "s3cret"}

func request(peer string, headers map[string]string) *http.Request {
	r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
	r.RemoteAddr = peer
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

func TestParse(t *testing.T) {
	proxied := map[string]string{
		"CF-Connecting-IP": "203.0.113.1", "True-Client-IP": "203.0.113.2", "X-Real-IP": "203.0.113.3",
		clientip.SecretHeader: "s3cret",
	}
	cases := []struct {
		setting string
		opts    clientip.Options
		want    string // "" = peer only
		wantErr bool
	}{
		{setting: ""},
		{setting: "off"},
		{setting: "cloudflare", opts: secret, want: "203.0.113.1"},
		{setting: "Cloudflare", opts: secret, want: "203.0.113.1"},
		{setting: "render", want: "203.0.113.2"}, // private peer below is trusted by default
		{setting: "header:X-Real-IP", opts: secret, want: "203.0.113.3"},
		{setting: "Header:x-real-ip", opts: secret, want: "203.0.113.3"},
		{setting: "cloudflare", wantErr: true}, // nothing to tell proxied from direct
		{setting: "header:X-Real-IP", wantErr: true},
		{setting: "header:", opts: secret, wantErr: true},
		{setting: "header:bad header", opts: secret, wantErr: true},
		{setting: "xff", opts: secret, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.setting, func(t *testing.T) {
			res, err := clientip.Parse(tc.setting, tc.opts)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Parse(%q): want error", tc.setting)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.setting, err)
			}
			want := tc.want
			if want == "" {
				want = "10.0.0.7"
			}
			if got := res(request("10.0.0.7:4242", proxied)); got != want {
				t.Fatalf("Parse(%q) resolver = %q, want %q", tc.setting, got, want)
			}
		})
	}
}

// TestHeaderIgnoredWithoutProof is the property that matters: a caller who
// reaches the origin directly cannot mint buckets by varying the header.
func TestHeaderIgnoredWithoutProof(t *testing.T) {
	res, err := clientip.Parse("cloudflare", secret)
	if err != nil {
		t.Fatal(err)
	}
	direct := request("198.51.100.9:1", map[string]string{"CF-Connecting-IP": "203.0.113.50"})
	if got := res(direct); got != "198.51.100.9" {
		t.Fatalf("direct caller with a forged header: got %q, want its peer", got)
	}
	direct.Header.Set(clientip.SecretHeader, "wrong")
	if got := res(direct); got != "198.51.100.9" {
		t.Fatalf("wrong secret: got %q, want peer", got)
	}
	direct.Header.Set(clientip.SecretHeader, "s3cret")
	if got := res(direct); got != "203.0.113.50" {
		t.Fatalf("right secret: got %q", got)
	}
}

func TestPrefixesAndFallbacks(t *testing.T) {
	opts := clientip.Options{Prefixes: []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}}
	res, err := clientip.Parse("header:X-Real-IP", opts)
	if err != nil {
		t.Fatal(err)
	}
	in := request("192.0.2.10:5", map[string]string{"X-Real-IP": " 2001:db8::1 "})
	if got := res(in); got != "2001:db8::/64" { // IPv6 keys are /64s (SC-05)
		t.Fatalf("peer inside prefix: got %q", got)
	}
	out := request("192.0.3.10:5", map[string]string{"X-Real-IP": "2001:db8::1"})
	if got := res(out); got != "192.0.3.10" {
		t.Fatalf("peer outside prefix: got %q", got)
	}
	garbage := request("192.0.2.10:5", map[string]string{"X-Real-IP": "not an ip"})
	if got := res(garbage); got != "192.0.2.10" {
		t.Fatalf("garbage header: got %q, want peer", got)
	}
	missing := request("192.0.2.10:5", nil)
	if got := res(missing); got != "192.0.2.10" {
		t.Fatalf("missing header: got %q, want peer", got)
	}
	// render: a public peer is not trusted even though the header is set.
	render, _ := clientip.Parse("render", clientip.Options{})
	if got := render(request("203.0.113.9:1", map[string]string{"True-Client-IP": "203.0.113.2"})); got != "203.0.113.9" {
		t.Fatalf("render with public peer: got %q", got)
	}
}

func TestParsePrefixes(t *testing.T) {
	got, err := clientip.ParsePrefixes(" 10.0.0.0/8, 2001:db8::/32 ,")
	if err != nil || len(got) != 2 {
		t.Fatalf("%v %v", got, err)
	}
	if _, err := clientip.ParsePrefixes("10.0.0.0"); err == nil {
		t.Fatal("bare address accepted as a prefix")
	}
}

func TestPeerWithoutPort(t *testing.T) {
	if got := clientip.Peer(request("unix-socket", nil)); got != "unix-socket" {
		t.Fatalf("got %q", got)
	}
}

// TestIPv6KeysCollapseToSlash64 pins SC-05: an IPv6 subscriber is handed a
// whole /64, so keying a throttle on the full address gives one client
// 2^64 buckets. Every resolver path reports the /64 instead; IPv4 (and
// IPv4-mapped IPv6) keeps its full address.
func TestIPv6KeysCollapseToSlash64(t *testing.T) {
	header, err := clientip.Parse("header:X-Real-IP", secret)
	if err != nil {
		t.Fatal(err)
	}
	proxied := func(ip string) *http.Request {
		return request("10.0.0.7:1", map[string]string{"X-Real-IP": ip, clientip.SecretHeader: "s3cret"})
	}
	resolvers := map[string]func(ip string) string{
		"peer":   func(ip string) string { return clientip.Peer(request(net.JoinHostPort(ip, "443"), nil)) },
		"header": func(ip string) string { return header(proxied(ip)) },
	}
	for name, resolve := range resolvers {
		t.Run(name, func(t *testing.T) {
			a, b := resolve("2001:db8:1:2::1"), resolve("2001:db8:1:2:ffff:ffff:ffff:ffff")
			if a != b {
				t.Fatalf("same /64 gave different keys: %q vs %q", a, b)
			}
			if a != "2001:db8:1:2::/64" {
				t.Fatalf("key = %q, want the /64 prefix", a)
			}
			if c := resolve("2001:db8:1:3::1"); c == a {
				t.Fatalf("different /64 shared key %q", c)
			}
			if got := resolve("203.0.113.5"); got != "203.0.113.5" {
				t.Fatalf("IPv4 key = %q, want unchanged", got)
			}
			if got := resolve("::ffff:203.0.113.5"); got != "203.0.113.5" {
				t.Fatalf("IPv4-mapped key = %q, want the IPv4 address", got)
			}
		})
	}
}

// TestProxyProofUsesFullPeerAddress: the /64 collapse is for throttle keys
// only. Deciding whether the peer is the proxy must still compare the full
// address, or ::1/128 would stop matching loopback.
func TestProxyProofUsesFullPeerAddress(t *testing.T) {
	opts := clientip.Options{Prefixes: []netip.Prefix{netip.MustParsePrefix("::1/128")}}
	res, err := clientip.Parse("header:X-Real-IP", opts)
	if err != nil {
		t.Fatal(err)
	}
	if got := res(request("[::1]:5", map[string]string{"X-Real-IP": "203.0.113.7"})); got != "203.0.113.7" {
		t.Fatalf("loopback proxy not trusted: got %q", got)
	}
}
