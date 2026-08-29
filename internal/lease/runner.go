package lease

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// DefaultPoll is the cadence at which a Runner renews a lease it holds and
// retries one it does not, unless the loop's own interval is shorter. One
// cheap single-row upsert per loop per minute; a holder that dies is
// replaced within TTL (three polls).
const DefaultPoll = time.Minute

// ttlPolls is the lease TTL expressed in polls. Three, not one: a single
// missed renewal -- a slow tick, a GC pause, a busy database -- must not
// hand the loop to another process while the holder is still running it.
const ttlPolls = 3

// releaseTimeout bounds the shutdown Release, which runs after the loop's
// own ctx is already done.
const releaseTimeout = 5 * time.Second

// Loop describes one background loop for Runner.Run.
type Loop struct {
	// Name is the lease's key; every process running this loop against the
	// same database must use the same name.
	Name string
	// Interval is the tick cadence while holding the lease. Non-positive
	// values are floored to DefaultPoll rather than handed to
	// time.NewTicker, which panics on them.
	Interval time.Duration
	// Wake, if set, asks for a tick now. Honored only while holding the
	// lease; otherwise the holder's next scheduled tick covers it.
	Wake <-chan struct{}
	// Tick is one cycle of the loop.
	Tick func(ctx context.Context)
}

// Runner drives background loops under a lease each (spec section 12): a
// loop ticks only in the process holding its lease, ownership passes to a
// survivor one TTL after the holder dies, and a clean shutdown releases
// every lease at once so the replacement takes over on its next poll.
type Runner struct {
	Repo Repository
	// Holder identifies this process; cmd/sidecar derives it from the host
	// name and pid plus random bytes.
	Holder string
	Now    func() time.Time
	Logger *slog.Logger
	// Poll overrides DefaultPoll; tests set it to milliseconds.
	Poll time.Duration

	wg sync.WaitGroup
}

// Go runs l on its own goroutine, tracked so Wait can block on it.
func (r *Runner) Go(ctx context.Context, l Loop) {
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		r.Run(ctx, l)
	}()
}

// Wait blocks until every loop started with Go has returned -- which, once
// their ctx is done, means every lease has been released. cmd/sidecar
// calls it before closing the store and exiting: a process that exits
// with its leases still written leaves the replacement waiting out the
// full TTL for each loop instead of taking over on its next poll.
func (r *Runner) Wait() {
	r.wg.Wait()
}

// Run drives l until ctx is done. It ticks immediately on acquiring the
// lease (so a fresh deploy catches up at boot rather than an interval
// later), then every l.Interval; a Tick that outlives the TTL can overlap
// with a new holder's, which every loop tolerates (at-least-once, spec
// section 12).
func (r *Runner) Run(ctx context.Context, l Loop) {
	interval := l.Interval
	if interval <= 0 {
		r.Logger.Error("lease: invalid loop interval, using fallback", "loop", l.Name, "interval", interval, "fallback", DefaultPoll)
		interval = DefaultPoll
	}
	poll := r.Poll
	if poll <= 0 {
		poll = DefaultPoll
	}
	if interval < poll {
		poll = interval
	}
	ttl := ttlPolls * poll

	holding := false
	var next time.Time // zero: a tick is due as soon as the lease is held
	step := func() {
		now := r.Now()
		ok, err := r.Repo.Acquire(ctx, l.Name, r.Holder, now, ttl)
		if err != nil {
			// Conservative: a store we cannot reach is one we cannot renew
			// through either, so assume the lease is lost until it answers.
			r.Logger.Warn("lease: acquire", "loop", l.Name, "err", err)
			holding = false
			return
		}
		if !ok {
			if holding {
				r.Logger.Warn("lease: lost to another process", "loop", l.Name)
			}
			holding = false
			return
		}
		if !holding {
			r.Logger.Info("lease: acquired", "loop", l.Name, "holder", r.Holder)
			holding = true
			next = time.Time{}
		}
		// Due within half a poll counts as due: next is stamped after the
		// tick returns, so the ticker fire that should run it lands a hair
		// early, and an exact comparison would skip every other tick.
		if now.Add(poll / 2).Before(next) {
			return
		}
		l.Tick(ctx)
		next = r.Now().Add(interval)
	}

	step()
	ticker := time.NewTicker(poll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			r.release(l.Name)
			return
		case <-ticker.C:
			step()
		case <-l.Wake:
			if holding {
				l.Tick(ctx)
				next = r.Now().Add(interval)
			}
		}
	}
}

// release drops the lease at shutdown on a fresh context: the loop's own
// is already done, and a Release that never reaches the store only costs
// the replacement one TTL of waiting.
func (r *Runner) release(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), releaseTimeout)
	defer cancel()
	if err := r.Repo.Release(ctx, name, r.Holder); err != nil {
		r.Logger.Warn("lease: release", "loop", name, "err", err)
	}
}
