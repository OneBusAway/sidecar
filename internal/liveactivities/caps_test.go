package liveactivities_test

import (
	"testing"

	"github.com/OneBusAway/sidecar/internal/liveactivities"
)

// TestCapsCheck pins SC-03's registration caps: every cap bounds NEW rows
// (or a move to a stop the region does not have yet); a re-registration of
// an existing activity -- ActivityKit token rotation -- always passes, or
// a live card would stop updating mid-trip.
func TestCapsCheck(t *testing.T) {
	caps := liveactivities.Caps{Total: 10, PerRegion: 5, StopsPerRegion: 3, PerPushToken: 2}
	for _, tc := range []struct {
		name string
		u    liveactivities.Usage
		stop string
		want liveactivities.CapResult
	}{
		{"room everywhere", liveactivities.Usage{Total: 1, InRegion: 1, StopsInRegion: 1, OnStop: 1}, "s", liveactivities.CapOK},
		{"push token full", liveactivities.Usage{ForPushToken: 2, OnStop: 1}, "s", liveactivities.CapPushToken},
		{"region full", liveactivities.Usage{InRegion: 5, OnStop: 1}, "s", liveactivities.CapCapacity},
		{"table full", liveactivities.Usage{Total: 10, OnStop: 1}, "s", liveactivities.CapCapacity},
		{"region stops full, known stop", liveactivities.Usage{StopsInRegion: 3, OnStop: 1}, "s", liveactivities.CapOK},
		{"region stops full, new stop", liveactivities.Usage{StopsInRegion: 3}, "s", liveactivities.CapCapacity},
		{"rotation when everything is full", liveactivities.Usage{Total: 10, InRegion: 5, StopsInRegion: 3, ForPushToken: 2, OnStop: 1, Exists: true, ExistingStopID: "s"}, "s", liveactivities.CapOK},
		{"existing row moving to a new stop when stops are full", liveactivities.Usage{StopsInRegion: 3, Exists: true, ExistingStopID: "old"}, "s", liveactivities.CapCapacity},
	} {
		if got := caps.Check(tc.u, tc.stop); got != tc.want {
			t.Errorf("%s: Check = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestDefaultCapsAreGenerous(t *testing.T) {
	d := liveactivities.DefaultCaps
	if d.PerPushToken < 2 || d.StopsPerRegion < 500 || d.PerRegion < 1000 || d.Total < d.PerRegion {
		t.Fatalf("DefaultCaps = %+v: too tight for real use", d)
	}
}
