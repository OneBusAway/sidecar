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
	st := &loopState{r: r, l: l, interval: interval, ttl: ttlPolls * poll}
	st.ticker = time.NewTicker(interval)
	defer st.ticker.Stop()
	poller := time.NewTicker(poll)
	defer poller.Stop()

	st.renew(ctx)
	for {
		select {
		case <-ctx.Done():
			st.finish()
			return
		case <-st.running:
			st.tickDone()
		case <-poller.C:
			st.renew(ctx)
		case <-st.ticker.C:
			if st.holding {
				st.startTick(ctx)
			}
		case _, ok := <-st.l.Wake:
			st.wake(ctx, ok)
		}
	}
}

// loopState is one Run's bookkeeping; every method runs on Run's goroutine.
type loopState struct {
	r        *Runner
	l        Loop
	interval time.Duration
	ttl      time.Duration
	ticker   *time.Ticker

	holding     bool
	lastRenewed time.Time
	// running is non-nil while a cycle is in flight (a nil channel never
	// selects). A cycle runs on its own goroutine so Run keeps renewing the
	// lease underneath it: a holder that is busy is alive, and a cycle
	// longer than the TTL must not hand the loop to a peer. An interval
	// fire or Wake during one is skipped, not queued -- one runner never
	// runs two cycles at once.
	running     chan struct{}
	tickStarted time.Time
}

// renew acquires or renews the lease. On acquiring it, the loop ticks at
// once and the interval restarts from that tick -- otherwise a poll and an
// interval fire that coincide (both start from the same instant and 60s
// divides both) would tick twice in the same moment.
func (st *loopState) renew(ctx context.Context) {
	now := st.r.Now()
	ok, err := st.r.Repo.Acquire(ctx, st.l.Name, st.r.Holder, now, st.ttl)
	switch {
	case errors.Is(err, context.Canceled):
		// Shutdown: the ctx.Done branch is about to run Release.
		return
	case err != nil:
		// Conservative: a store we cannot reach is one we cannot renew
		// through either, so assume the lease is lost until it answers.
		st.r.Logger.Warn("lease: acquire failed; loop paused until the store answers", "loop", st.l.Name, "err", err)
	case !ok && st.holding:
		st.r.Logger.Warn("lease: lost to another process", "loop", st.l.Name, "held_for", now.Sub(st.lastRenewed))
	}
	was := st.holding
	st.holding = err == nil && ok
	if st.holding {
		st.lastRenewed = now
	}
	if st.holding && !was {
		st.r.Logger.Info("lease: acquired", "loop", st.l.Name, "holder", st.r.Holder)
		st.startTick(ctx)
		st.ticker.Reset(st.interval)
	}
}

func (st *loopState) startTick(ctx context.Context) {
	if st.running != nil {
		return
	}
	st.running = make(chan struct{})
	st.tickStarted = st.r.Now()
	go func() {
		defer close(st.running)
		st.l.Tick(ctx)
	}()
}

func (st *loopState) tickDone() {
	st.running = nil
	if took := st.r.Now().Sub(st.tickStarted); took > st.interval {
		st.r.Logger.Warn("lease: cycle outlived its interval; ticks were skipped", "loop", st.l.Name, "took", took, "interval", st.interval)
	}
}

// wake handles a receive on Loop.Wake. A closed channel is always ready,
// which would start a new cycle the instant the previous one finished,
// forever; it is disarmed instead (a nil channel never selects).
func (st *loopState) wake(ctx context.Context, ok bool) {
	if !ok {
		st.r.Logger.Warn("lease: wake channel closed; wakes disabled for this loop", "loop", st.l.Name)
		st.l.Wake = nil
		return
	}
	if !st.holding {
		st.r.Logger.Debug("lease: wake ignored, not the holder; its next tick covers it", "loop", st.l.Name)
		return
	}
	st.startTick(ctx)
}

// finish waits for an in-flight cycle (ctx is done, so it is winding up)
// and releases the lease.
func (st *loopState) finish() {
	if st.running != nil {
		<-st.running
	}
	st.r.release(st.l.Name)
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
