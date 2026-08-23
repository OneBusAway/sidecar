package ghostbus

import (
	"math"
	"testing"
)

func TestHaversineMeters(t *testing.T) {
	// Pike Place Market to Space Needle: ~1,300 m. The tolerance is loose
	// because the test pins "sane great-circle math", not a specific
	// earth-radius constant.
	got := HaversineMeters(47.6097, -122.3422, 47.6205, -122.3493)
	if got < 1200 || got > 1450 {
		t.Errorf("Seattle distance = %f, want ~1300", got)
	}
	if d := HaversineMeters(47.6, -122.3, 47.6, -122.3); d != 0 {
		t.Errorf("zero distance = %f, want 0", d)
	}
	// Antipodal-ish sanity: half the earth's circumference, ~20,000 km.
	far := HaversineMeters(0, 0, 0, 180)
	if math.Abs(far-20015000) > 100000 {
		t.Errorf("antipodal distance = %f, want ~20015000", far)
	}
}
