package httpapi

import (
	"testing"

	"github.com/OneBusAway/sidecar/internal/ghostbus"
	"github.com/OneBusAway/sidecar/internal/pushreg"
	"github.com/OneBusAway/sidecar/internal/surveys"
)

// TestHTTPLimitsMatchImport (SC-15): `sidecar-admin import` holds migrated
// rows to the domain packages' caps; this pins the rider API's own caps to
// the same values so the two paths cannot drift apart.
func TestHTTPLimitsMatchImport(t *testing.T) {
	t.Parallel()
	for name, pair := range map[string][2]int{
		"token":                {maxTokenLen, pushreg.MaxTokenLen},
		"test-device descr.":   {maxDescriptionLen, pushreg.MaxDescriptionLen},
		"survey identifier":    {maxIdentifierLen, surveys.MaxIdentifierLen},
		"ghost bus identifier": {maxIdentifierLen, ghostbus.MaxIdentifierLen},
	} {
		if pair[0] != pair[1] {
			t.Errorf("%s: HTTP cap %d, import cap %d", name, pair[0], pair[1])
		}
	}
}
