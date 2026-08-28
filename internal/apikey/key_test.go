package apikey_test

import (
	"strings"
	"testing"

	"github.com/OneBusAway/sidecar/internal/apikey"
)

// awkwardSecret is the fixture the spec (section 3.1, section 8) requires: a
// random segment that contains BOTH '_' and '-'. The base64url alphabet
// includes '_', so about half of all real keys look like this. A
// strings.Split or a cut on the LAST '_' passes every other fixture and
// fails only on this one.
const awkwardSecret = "Qm9-abc_defGHIjklMNOpqrSTUvwxYZ0123456789-_x"

func TestParsePrefix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		raw        string
		wantKind   apikey.Kind
		wantRegion int64
		wantOK     bool
	}{
		{"region key with underscores and dashes in the secret",
			"obask_1_" + awkwardSecret, apikey.KindRegion, 1, true},
		{"region 0 is a real region",
			"obask_0_" + awkwardSecret, apikey.KindRegion, 0, true},
		{"multi digit region", "obask_4082_" + awkwardSecret, apikey.KindRegion, 4082, true},
		{"principal", "obasp_" + awkwardSecret, apikey.KindPrincipal, 0, true},
		{"leading zero region is rejected", "obask_01_" + awkwardSecret, "", 0, false},
		{"negative region is rejected", "obask_-1_" + awkwardSecret, "", 0, false},
		{"plus signed region is rejected", "obask_+1_" + awkwardSecret, "", 0, false},
		{"non numeric region is rejected", "obask_one_" + awkwardSecret, "", 0, false},
		{"region key with no secret is rejected", "obask_1_", "", 0, false},
		{"region key with no region segment is rejected", "obask_" + awkwardSecret, "", 0, false},
		{"principal with no secret is rejected", "obasp_", "", 0, false},
		{"unknown prefix is rejected", "sk_live_" + awkwardSecret, "", 0, false},
		{"no underscore at all is rejected", "obask", "", 0, false},
		{"empty is rejected", "", "", 0, false},
		{"leading space is rejected", " obask_1_" + awkwardSecret, "", 0, false},
		{"region id larger than int64 is rejected",
			"obask_99999999999999999999_" + awkwardSecret, "", 0, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			kind, region, ok := apikey.ParsePrefix(tc.raw)
			if ok != tc.wantOK || kind != tc.wantKind || region != tc.wantRegion {
				t.Errorf("ParsePrefix(%q) = (%q, %d, %v), want (%q, %d, %v)",
					tc.raw, kind, region, ok, tc.wantKind, tc.wantRegion, tc.wantOK)
			}
		})
	}
}

// TestNewRegionKey pins the wire format: the plaintext carries the region id
// so an operator reading a log line or a Rails column can tell which region a
// key belongs to without a database lookup. The hash lookup is still what
// decides (spec section 3.1).
func TestNewRegionKey(t *testing.T) {
	t.Parallel()

	raw, hash, err := apikey.NewRegionKey(7)
	if err != nil {
		t.Fatalf("NewRegionKey: %v", err)
	}
	if !strings.HasPrefix(raw, "obask_7_") {
		t.Errorf("raw = %q, want an obask_7_ prefix", raw)
	}
	if got, want := len(strings.TrimPrefix(raw, "obask_7_")), 43; got != want {
		t.Errorf("secret length = %d, want %d (32 random bytes, raw base64url)", got, want)
	}
	if hash != apikey.Hash(raw) {
		t.Errorf("hash = %q, want Hash(raw) = %q", hash, apikey.Hash(raw))
	}
	if len(hash) != 64 {
		t.Errorf("hash length = %d, want 64 (hex SHA-256)", len(hash))
	}
	kind, region, ok := apikey.ParsePrefix(raw)
	if !ok || kind != apikey.KindRegion || region != 7 {
		t.Errorf("ParsePrefix(minted) = (%q, %d, %v), want (region, 7, true)", kind, region, ok)
	}

	other, _, err := apikey.NewRegionKey(7)
	if err != nil {
		t.Fatalf("NewRegionKey: %v", err)
	}
	if other == raw {
		t.Error("two mints produced the same key")
	}
}

// TestNewPrincipalKey mirrors TestNewRegionKey for the deployment-wide
// credential, which carries no region segment.
func TestNewPrincipalKey(t *testing.T) {
	t.Parallel()

	raw, hash, err := apikey.NewPrincipalKey()
	if err != nil {
		t.Fatalf("NewPrincipalKey: %v", err)
	}
	if !strings.HasPrefix(raw, "obasp_") {
		t.Errorf("raw = %q, want an obasp_ prefix", raw)
	}
	if got, want := len(strings.TrimPrefix(raw, "obasp_")), 43; got != want {
		t.Errorf("secret length = %d, want %d", got, want)
	}
	if hash != apikey.Hash(raw) {
		t.Errorf("hash = %q, want Hash(raw)", hash)
	}
	kind, region, ok := apikey.ParsePrefix(raw)
	if !ok || kind != apikey.KindPrincipal || region != 0 {
		t.Errorf("ParsePrefix(minted) = (%q, %d, %v), want (principal, 0, true)", kind, region, ok)
	}
}
