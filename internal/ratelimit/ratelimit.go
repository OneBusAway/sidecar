// Package ratelimit is a fixed-window request counter for the spec §2.6
// throttles. Fixed windows (not sliding) match the reference implementation's
// rack-attack behavior, and the worst-case burst (2x limit straddling a
// window edge) is acceptable for an abuse brake. The limiter is clockless --
// callers pass now -- so it stays inside the repo-wide time.Now ban and
// tests are deterministic.
package ratelimit

import (
	"sync"
	"time"
)

type bucket struct {
	start time.Time
	count int
}

// Limiter counts requests per key in fixed windows. Safe for concurrent use.
type Limiter struct {
	limit  int
	window time.Duration

	mu        sync.Mutex
	buckets   map[string]*bucket
	lastSweep time.Time
}

// New builds a Limiter allowing limit requests per key per window.
func New(limit int, window time.Duration) *Limiter {
	return &Limiter{limit: limit, window: window, buckets: make(map[string]*bucket)}
}

// Allow reports whether key may make a request at now, counting it if so.
// A denied request is not counted -- the window budget measures successful
// admissions, so a hammering client does not extend its own lockout.
func (l *Limiter) Allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	// Sweep expired buckets at most once per window: unauthenticated
	// endpoints see arbitrary client IPs, so the map must not grow without
	// bound -- but a per-call scan would hand a flood of distinct IPs an
	// O(n) amplifier on every request. Time-gated, each window pays for one
	// scan; a bucket lives at most two windows past its last use.
	if now.Sub(l.lastSweep) >= l.window {
		for k, b := range l.buckets {
			if now.Sub(b.start) >= l.window {
				delete(l.buckets, k)
			}
		}
		l.lastSweep = now
	}

	b := l.buckets[key]
	if b == nil || now.Sub(b.start) >= l.window {
		l.buckets[key] = &bucket{start: now, count: 1}
		return true
	}
	if b.count >= l.limit {
		return false
	}
	b.count++
	return true
}

// Len reports the number of live buckets; test-only observability.
func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}
