package clientip_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/OneBusAway/sidecar/internal/clientip"
)

func TestParse(t *testing.T) {
	cases := []struct {
		setting string
		header  string // header the resolver should read; "" means peer only
		wantErr bool
	}{
		{setting: "", header: ""},
		{setting: "off", header: ""},
		{setting: "cloudflare", header: "CF-Connecting-IP"},
		{setting: "Cloudflare", header: "CF-Connecting-IP"},
		{setting: "render", header: "True-Client-IP"},
		{setting: "header:X-Real-IP", header: "X-Real-IP"},
		{setting: "header:", wantErr: true},
		{setting: "header:bad header", wantErr: true},
		{setting: "xff", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.setting, func(t *testing.T) {
			res, err := clientip.Parse(tc.setting)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Parse(%q): want error", tc.setting)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.setting, err)
			}
			r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
			r.RemoteAddr = "10.0.0.7:4242"
			r.Header.Set("CF-Connecting-IP", "203.0.113.1")
			r.Header.Set("True-Client-IP", "203.0.113.2")
			r.Header.Set("X-Real-IP", "203.0.113.3")
			want := "10.0.0.7"
			if tc.header != "" {
				want = r.Header.Get(tc.header)
			}
			if got := res(r); got != want {
				t.Fatalf("Parse(%q) resolver = %q, want %q", tc.setting, got, want)
			}
		})
	}
}

func TestHeaderFallsBackToPeer(t *testing.T) {
	res, err := clientip.Parse("cloudflare")
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
	r.RemoteAddr = "10.0.0.7:4242"
	if got := res(r); got != "10.0.0.7" {
		t.Fatalf("missing header: got %q, want peer", got)
	}
	r.Header.Set("CF-Connecting-IP", "  not an ip ")
	if got := res(r); got != "10.0.0.7" {
		t.Fatalf("garbage header: got %q, want peer", got)
	}
	r.Header.Set("CF-Connecting-IP", " 2001:db8::1 ")
	if got := res(r); got != "2001:db8::1" {
		t.Fatalf("ipv6 header: got %q", got)
	}
}

func TestPeerWithoutPort(t *testing.T) {
	r := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
	r.RemoteAddr = "unix-socket"
	if got := clientip.Peer(r); got != "unix-socket" {
		t.Fatalf("got %q", got)
	}
}
