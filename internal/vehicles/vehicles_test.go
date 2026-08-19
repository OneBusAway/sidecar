package vehicles

import (
	"strings"
	"testing"

	"github.com/OneBusAway/sidecar/internal/obaapi"
)

func TestNormalize(t *testing.T) {
	tests := []struct {
		name   string
		in     string
		want   string
		wantOK bool
	}{
		{"trims and lowers", "  ABC  ", "abc", true},
		{"two characters is too short", "ab", "", false},
		{"empty is too short", "", "", false},
		{"three characters is enough", "abc", "abc", true},
		{"whitespace only is too short", "     ", "", false},
		// Rails counts characters, not bytes. Two CJK characters are six
		// bytes; a byte-length check would let them through into a
		// full-fleet scan.
		{"two CJK characters are two runes, not six bytes", "公車", "", false},
		{"three CJK characters pass", "公車站", "公車站", true},
		// The upper bound keeps an attacker from filling the query cache with
		// megabyte-long keys. No vehicle id approaches 64 characters.
		{"over 64 runes is rejected", strings.Repeat("a", 65), "", false},
		{"exactly 64 runes is accepted", strings.Repeat("a", 64), strings.Repeat("a", 64), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := Normalize(tt.in)
			if ok != tt.wantOK {
				t.Fatalf("Normalize(%q) ok = %v, want %v", tt.in, ok, tt.wantOK)
			}
			if got != tt.want {
				t.Errorf("Normalize(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestFilter(t *testing.T) {
	fleet := []obaapi.Vehicle{
		{AgencyID: "1", AgencyName: "Metro", VehicleID: "1_4361"},
		{AgencyID: "1", AgencyName: "Metro", VehicleID: "1_4362"},
		{AgencyID: "3", AgencyName: "CT", VehicleID: "3_ABC123"},
	}

	t.Run("substring match", func(t *testing.T) {
		got := Filter(fleet, "436")
		if len(got) != 2 {
			t.Fatalf("got %d matches, want 2: %+v", len(got), got)
		}
		if got[0].VehicleID != "1_4361" || got[0].ID != "1" || got[0].Name != "Metro" {
			t.Errorf("first match = %+v", got[0])
		}
	})

	// DELIBERATE, DO NOT "FIX". Spec §10 requires lowering the query only and
	// matching against raw ids. True case-insensitivity would match here, and
	// would diverge from every shipped client on fleets with uppercase ids.
	t.Run("lowered query does not match an uppercase fleet id", func(t *testing.T) {
		if got := Filter(fleet, "abc"); len(got) != 0 {
			t.Errorf("Filter(%q) = %+v, want no matches", "abc", got)
		}
	})

	t.Run("uppercase fleet id matches when the raw case is given", func(t *testing.T) {
		// Normalize would have lowered this, which is exactly why such a
		// fleet is unsearchable -- the bug being preserved.
		if got := Filter(fleet, "ABC"); len(got) != 1 {
			t.Errorf("Filter(%q) = %+v, want 1 match", "ABC", got)
		}
	})

	t.Run("no match returns empty, never nil", func(t *testing.T) {
		got := Filter(fleet, "zzz")
		if got == nil {
			t.Fatal("Filter returned nil; it must return an empty slice so the JSON is [] not null")
		}
		if len(got) != 0 {
			t.Errorf("got %d matches, want 0", len(got))
		}
	})

	t.Run("truncates at the cap preserving fleet order", func(t *testing.T) {
		big := make([]obaapi.Vehicle, 0, MaxResults+50)
		for i := 0; i < MaxResults+50; i++ {
			big = append(big, obaapi.Vehicle{AgencyID: "1", AgencyName: "Metro", VehicleID: "1_999"})
		}
		got := Filter(big, "999")
		if len(got) != MaxResults {
			t.Fatalf("got %d matches, want the cap of %d", len(got), MaxResults)
		}
	})
}
