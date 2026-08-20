package ratelimit_test

import (
	"strconv"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/ratelimit"
)

var base = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func TestAllowEnforcesLimitPerWindow(t *testing.T) {
	t.Parallel()
	l := ratelimit.New(3, time.Minute)

	for i := range 3 {
		if !l.Allow("1.2.3.4", base) {
			t.Fatalf("request %d: denied inside limit", i)
		}
	}
	if l.Allow("1.2.3.4", base.Add(59*time.Second)) {
		t.Fatal("4th request in window allowed")
	}
	// Other keys have their own budget.
	if !l.Allow("5.6.7.8", base) {
		t.Fatal("different key denied")
	}
	// A new window resets the count.
	if !l.Allow("1.2.3.4", base.Add(time.Minute)) {
		t.Fatal("request in next window denied")
	}
}

func TestSweepBoundsMemory(t *testing.T) {
	t.Parallel()
	l := ratelimit.New(1, time.Minute)
	for i := range 4096 {
		l.Allow("key-"+strconv.Itoa(i), base)
	}
	// Two windows later every earlier bucket is stale; the next Allow
	// sweeps them. The sweep is time-gated to once per window so a flood of
	// distinct keys can never turn Allow into a per-request full-map scan.
	l.Allow("fresh", base.Add(2*time.Minute))
	if got := l.Len(); got > 1 {
		t.Fatalf("Len() = %d after sweep; want <= 1", got)
	}
}
