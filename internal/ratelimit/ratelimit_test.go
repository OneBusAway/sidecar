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

// TestCountAndExhaustedDoNotCharge pins the read-only probes the login
// throttle uses (SC-02): it must decide "refuse before hashing" without
// spending budget, and charge only once a verification has actually failed.
func TestCountAndExhaustedDoNotCharge(t *testing.T) {
	t.Parallel()
	l := ratelimit.New(2, time.Minute)

	if got := l.Count("k", base); got != 0 {
		t.Fatalf("Count on an unseen key = %d, want 0", got)
	}
	for range 5 {
		if l.Exhausted("k", base) {
			t.Fatal("Exhausted on an unseen key; probing must not charge")
		}
	}
	l.Allow("k", base)
	if got := l.Count("k", base); got != 1 {
		t.Fatalf("Count after one Allow = %d, want 1", got)
	}
	l.Allow("k", base)
	if !l.Exhausted("k", base.Add(30*time.Second)) {
		t.Fatal("Exhausted = false at the limit")
	}
	// The window rolls over exactly as Allow sees it.
	if l.Exhausted("k", base.Add(time.Minute)) || l.Count("k", base.Add(time.Minute)) != 0 {
		t.Fatal("an expired window still reads as charged")
	}
	if !l.Allow("k", base.Add(time.Minute)) {
		t.Fatal("Allow denied in a fresh window")
	}
}
