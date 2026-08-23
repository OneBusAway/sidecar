package ratelimit

// White-box (package ratelimit, not ratelimit_test): the once-per-window
// sweep gate is a cost invariant, invisible to any black-box end-state
// assertion -- an ungated sweep produces the identical map contents and
// differs only in per-call work. Asserting lastSweep is the only net that
// can catch the gate being removed.

import (
	"testing"
	"time"
)

func TestSweepIsTimeGated(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l := New(1, time.Minute)

	l.Allow("a", base) // first call sweeps the empty map and stamps the gate
	if !l.lastSweep.Equal(base) {
		t.Fatalf("lastSweep = %v after first Allow; want %v", l.lastSweep, base)
	}
	l.Allow("b", base.Add(30*time.Second))
	if !l.lastSweep.Equal(base) {
		t.Fatal("mid-window Allow ran a sweep; the gate must hold it to once per window")
	}
	l.Allow("c", base.Add(time.Minute))
	if !l.lastSweep.Equal(base.Add(time.Minute)) {
		t.Fatal("window-boundary Allow did not sweep")
	}
}
