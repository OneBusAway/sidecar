package lease

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// DefaultPoll is the cadence at which a Runner renews a lease it holds and
// retries one it does not, independent of any loop's tick interval: one
// cheap single-row upsert per loop per minute.
const DefaultPoll = time.Minute

// ttlPolls is the lease TTL expressed in polls. Three, not one: a single
// missed renewal -- a slow tick, a GC pause, a busy database -- must not
// hand the loop to another process while the holder is still running it.
// A holder that dies is replaced within TTL.
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
	// Wake, if set (nil means never), asks for a tick now. Honored only
	// while holding the lease; otherwise the holder's next scheduled tick
	// covers it.
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

// Go runs l on its own goroutine, tracked so Wait can block on it. It
// panics on a misconfigured Runner or Loop: an empty Holder would make
// every process the "same" holder and silently disable the mutual
// exclusion the type exists for, and a nil Tick would panic on first
// acquire inside the goroutine instead of at boot.
func (r *Runner) Go(ctx context.Context, l Loop) {
	if err := r.validate(l); err != nil {
		panic(err)
	}
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		r.Run(ctx, l)
	}()
}

// Wait blocks until every loop started with Go has returned -- which, once
// their ctx is done, means every loop has attempted its Release (a failed
// one is logged and costs the replacement one TTL). cmd/sidecar calls it
// before closing the store and exiting: a process that exits with its
// leases still written leaves the replacement waiting out the full TTL for
// each loop instead of taking over on its next poll.
func (r *Runner) Wait() {
	r.wg.Wait()
}

func (r *Runner) validate(l Loop) error {
	switch {
	case r.Repo == nil, r.Now == nil, r.Logger == nil:
		return fmt.Errorf("lease: loop %q: Runner needs Repo, Now and Logger", l.Name)
	case r.Holder == "":
		return fmt.Errorf("lease: loop %q: Runner.Holder is empty", l.Name)
	case l.Name == "":
		return errors.New("lease: Loop.Name is empty")
	case l.Tick == nil:
		return fmt.Errorf("lease: loop %q: Tick is nil", l.Name)
	}
	return nil
}

// Run drives l until ctx is done. Two clocks: the lease is renewed (or
// retried) every poll, and while it is held the loop ticks every
// l.Interval, starting with an immediate tick on acquiring the lease so a
// fresh deploy catches up at boot rather than an interval later. Renewal
// continues underneath a running cycle, so a long cycle keeps its lease;
// the residual overlap -- a holder whose store went unreachable for a TTL
// while its cycle kept running -- is the at-least-once duplicate every
// loop tolerates (spec section 12).
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
	ttl := ttlPolls * poll

	holding := false
	var lastRenewed time.Time
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// A cycle runs on its own goroutine so the select below keeps renewing
	// the lease underneath it: a holder that is busy is alive, and a cycle
	// longer than the TTL must not hand the loop to a peer. running is
	// non-nil while a cycle is in flight (a nil channel never selects), and
	// an interval fire or Wake during one is skipped, not queued -- one
	// runner never runs two cycles at once.
	var running chan struct{}
	var tickStarted time.Time
	startTick := func() {
		if running != nil {
			return
		}
		running = make(chan struct{})
		tickStarted = r.Now()
		go func() {
			defer close(running)
			l.Tick(ctx)
		}()
	}
	// renew acquires or renews the lease. On acquiring it, the loop ticks
	// at once and the interval restarts from that tick -- otherwise a poll
	// and an interval fire that coincide (both start from the same instant
	// and 60s divides both) would tick twice in the same moment.
	renew := func() {
		now := r.Now()
		ok, err := r.Repo.Acquire(ctx, l.Name, r.Holder, now, ttl)
		switch {
		case errors.Is(err, context.Canceled):
			// Shutdown: the ctx.Done branch is about to run Release.
			return
		case err != nil:
			// Conservative: a store we cannot reach is one we cannot renew
			// through either, so assume the lease is lost until it answers.
			r.Logger.Warn("lease: acquire failed; loop paused until the store answers", "loop", l.Name, "err", err)
		case !ok && holding:
			r.Logger.Warn("lease: lost to another process", "loop", l.Name, "held_for", now.Sub(lastRenewed))
		}
		was := holding
		holding = err == nil && ok
		if holding {
			lastRenewed = now
		}
		if holding && !was {
			r.Logger.Info("lease: acquired", "loop", l.Name, "holder", r.Holder)
			startTick()
			ticker.Reset(interval)
		}
	}

	renew()
	poller := time.NewTicker(poll)
	defer poller.Stop()
	for {
		select {
		case <-ctx.Done():
			if running != nil {
				<-running
			}
			r.release(l.Name)
			return
		case <-running:
			running = nil
			if took := r.Now().Sub(tickStarted); took > interval {
				r.Logger.Warn("lease: cycle outlived its interval; ticks were skipped", "loop", l.Name, "took", took, "interval", interval)
			}
		case <-poller.C:
			renew()
		case <-ticker.C:
			if holding {
				startTick()
			}
		case <-l.Wake:
			if !holding {
				r.Logger.Debug("lease: wake ignored, not the holder; its next tick covers it", "loop", l.Name)
				continue
			}
			startTick()
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
		r.Logger.Warn("lease: release failed; the replacement waits out the TTL", "loop", name, "err", err)
	}
}
