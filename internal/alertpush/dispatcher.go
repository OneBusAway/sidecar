package alertpush

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/OneBusAway/sidecar/internal/alerts"
	"github.com/OneBusAway/sidecar/internal/push"
	"github.com/OneBusAway/sidecar/internal/pushreg"
)

// Waker is the one method the admin API needs from the Dispatcher: a
// non-blocking nudge so an enqueued push starts now rather than at the
// next tick (design spec §2.6).
type Waker interface {
	Wake()
}

// Dispatcher performs alert push fan-out (spec §4, §12 row 3): it claims
// queued (and stuck) pushes, pages each audience by registration id, groups
// every page by (platform, normalized locale, APNs environment), sends one
// gorush batch per group, and commits progress one page at a time so a
// crash resumes at the last committed cursor.
type Dispatcher struct {
	Repo     Repository
	Alerts   alerts.Repository
	PushRegs pushreg.Repository
	// Sender may be nil (no push transport configured): claimed pushes are
	// failed immediately with an explanatory last_error rather than left
	// queued forever, because the CLI can enqueue without a server.
	Sender push.BatchSender
	Now    func() time.Time
	Logger *slog.Logger

	once sync.Once
	wake chan struct{}

	// adopt fires on the first RunOnce only; see there.
	adopt sync.Once
}

func (d *Dispatcher) wakeCh() chan struct{} {
	d.once.Do(func() { d.wake = make(chan struct{}, 1) })
	return d.wake
}

// Wake asks the loop to run a cycle now. Never blocks; a pending wake is
// coalesced with any already queued.
func (d *Dispatcher) Wake() {
	select {
	case d.wakeCh() <- struct{}{}:
	default:
	}
}

// WakeC is the channel Wake signals on, for the lease.Runner that drives
// RunOnce (cmd/sidecar hands it over as the loop's Wake).
func (d *Dispatcher) WakeC() <-chan struct{} {
	return d.wakeCh()
}

// RunOnce claims every push that is due and sends each in turn. Exported
// so tests drive cycles without a ticker.
//
// The FIRST cycle after construction adopts every sending row outright
// rather than only those idle for StuckAfter (design spec §2.6). A deploy is
// a SIGTERM, not a crash: the previous process stopped between pages and
// left its in-flight rows sending with a fresh updated_at, so waiting out
// the stuck clock would stall the send for 15 minutes. Only a crash leaves a
// stale updated_at, which the later cycles' now-StuckAfter window is for.
// Adoption is safe enough because the lease.Runner keeps at most one live
// holder per loop: sending rows seen on a first cycle are almost certainly
// the previous holder's. The residual case -- a peer's cycle outliving the
// lease TTL -- is the at-least-once duplicate spec §12 accepts.
func (d *Dispatcher) RunOnce(ctx context.Context) {
	now := d.Now()
	stuckBefore := now.Add(-StuckAfter)
	// Claim's window is exclusive (updated_at < stuckBefore) over whole
	// epoch seconds, so adopting a row stamped in this very second -- the
	// process that died a moment ago -- needs one second of slack.
	d.adopt.Do(func() { stuckBefore = now.Add(time.Second) })
	claimed, err := d.Repo.Claim(ctx, now, stuckBefore)
	if err != nil {
		d.Logger.Error("alertpush: claim", "err", err)
		return
	}
	for _, p := range claimed {
		if ctx.Err() != nil {
			return
		}
		d.send(ctx, p)
	}
}

// send runs one push from its current cursor to the end of its audience.
func (d *Dispatcher) send(ctx context.Context, p Push) {
	log := d.Logger.With("push_id", p.ID, "alert_id", p.AlertID, "region_id", p.RegionID)

	if d.Sender == nil {
		d.complete(ctx, log, p.ID, StatusFailed, "no push transport configured (--gorush-url/SIDECAR_GORUSH_URL)")
		return
	}

	// The alert re-check happens once, here at claim time, not per page: an
	// alert unpublished mid-send still reaches the remainder of the audience
	// (design spec §2.6).
	a, err := d.Alerts.Get(ctx, p.AlertID)
	switch {
	case errors.Is(err, alerts.ErrNotFound):
		d.complete(ctx, log, p.ID, StatusCanceled, "alert deleted before send")
		return
	case err != nil:
		log.Warn("alertpush: load alert", "err", err) // store blip: reclaimed as stuck later
		return
	case !a.Published:
		d.complete(ctx, log, p.ID, StatusCanceled, "alert unpublished before send")
		return
	}

	testOnly := p.Audience == AudienceTest
	if p.DeviceCount == 0 {
		count, err := d.PushRegs.CountAudience(ctx, p.RegionID, testOnly)
		if err != nil {
			log.Warn("alertpush: count audience", "err", err)
			return
		}
		if err := d.Repo.SetDeviceCount(ctx, p.ID, count.Total, d.Now()); err != nil {
			// A store write failure counts as an attempt, like a transport
			// one: a store that never accepts this write would otherwise
			// restart the same send on every reclaim forever (spec §2.6).
			d.recordAttempt(ctx, log, p.ID, err)
			return
		}
	}

	catalog := p.Messages.Catalog()
	cursor := p.BatchCursor
	for {
		page, err := d.PushRegs.ListAudience(ctx, p.RegionID, testOnly, cursor, BatchSize)
		if err != nil {
			log.Warn("alertpush: list audience", "err", err)
			return
		}
		if len(page) == 0 {
			break
		}
		submitted, err := d.sendPage(ctx, log, p, page, catalog)
		if err != nil {
			d.recordAttempt(ctx, log, p.ID, err)
			return
		}
		last := page[len(page)-1].ID
		ok, err := d.Repo.AdvanceCursor(ctx, p.ID, cursor, last, submitted, d.Now())
		if err != nil {
			// The page went out but its progress did not land, so the next
			// reclaim re-sends it. Count it as an attempt so MaxAttempts
			// bounds those duplicates instead of letting a write-failing
			// store re-send this page every StuckAfter forever (spec §2.6).
			d.recordAttempt(ctx, log.With("from_cursor", cursor, "to_cursor", last), p.ID, err)
			return
		}
		if !ok {
			log.Info("alertpush: push no longer ours (advanced elsewhere or canceled); yielding")
			return
		}
		cursor = last
	}
	d.complete(ctx, log, p.ID, StatusSent, "")
}

// groupKey is one gorush batch's identity within a page (spec §4).
type groupKey struct {
	platform push.Platform
	locale   string
	sandbox  bool
}

// sendPage sends one audience page as one batch per (platform, locale,
// sandbox) group and returns how many tokens gorush accepted. A transport
// error aborts the page; groups already sent in it are re-sent on resume
// (a bounded duplicate, design spec §2.6). The error is returned undecorated
// because it is stored verbatim as the operator-visible last_error.
func (d *Dispatcher) sendPage(ctx context.Context, log *slog.Logger, p Push, page []pushreg.Registration, catalog []string) (int64, error) {
	groups := make(map[groupKey][]string)
	var order []groupKey // deterministic send order for logs and tests
	for _, reg := range page {
		platform := push.PlatformIOS
		if reg.OperatingSystem == pushreg.OSAndroid {
			platform = push.PlatformAndroid
		}
		// apns_sandbox is normalized to false off iOS: registration writes
		// it for every platform, and honoring it on Android would split one
		// FCM batch into two identical calls (design spec §2.6).
		k := groupKey{
			platform: platform,
			locale:   pushreg.NormalizeLocale(reg.Locale, catalog),
			sandbox:  reg.APNSSandbox && platform == push.PlatformIOS,
		}
		if _, seen := groups[k]; !seen {
			order = append(order, k)
		}
		groups[k] = append(groups[k], reg.Token)
	}

	var submitted int64
	notifID := NotifID(p.ID)
	for _, k := range order {
		tokens := groups[k]
		msg := p.Messages.For(k.locale)
		res, err := d.Sender.SendBatch(ctx, push.Notification{
			Tokens: tokens, Platform: k.platform, Sandbox: k.sandbox,
			Title: msg.Title, Message: msg.Body,
		}, notifID)
		if err != nil {
			return submitted, err
		}
		for _, rej := range res.Rejected {
			// The error names only the push id; RecordFailure never puts a
			// token in its error strings (design spec §2.8).
			if _, err := d.Repo.RecordFailure(ctx, p.ID, rej.Token, rej.Reason, d.Now()); err != nil {
				log.Warn("alertpush: record inline rejection", "err", err)
			}
		}
		submitted += int64(len(tokens) - len(res.Rejected))
	}
	return submitted, nil
}

// recordAttempt counts one failed attempt -- a transport error, or a store
// write that lost a sent page's progress -- and gives up at MaxAttempts
// (design spec §2.7). The push keeps its cursor either way. Counting is
// best-effort: if the counter write fails too (the same broken store, most
// likely), the attempt is only logged, at Error, with whatever context the
// caller put on log -- the page's cursor range for a failed cursor commit.
func (d *Dispatcher) recordAttempt(ctx context.Context, log *slog.Logger, id int64, sendErr error) {
	attempts, err := d.Repo.RecordAttempt(ctx, id, sendErr.Error(), d.Now())
	if err != nil {
		log.Error("alertpush: record attempt", "err", err, "attempt_err", sendErr)
		return
	}
	if attempts >= MaxAttempts {
		log.Error("alertpush: giving up", "attempts", attempts, "err", sendErr)
		d.complete(ctx, log, id, StatusFailed, sendErr.Error())
		return
	}
	log.Warn("alertpush: send failed; will resume from cursor", "attempts", attempts, "err", sendErr)
}

// complete moves a sending push to a terminal status. A false from
// MarkCompleted means an operator canceled it mid-flight, or another worker
// already finished it: logged, never an error.
func (d *Dispatcher) complete(ctx context.Context, log *slog.Logger, id int64, status Status, lastError string) {
	ok, err := d.Repo.MarkCompleted(ctx, id, status, lastError, d.Now())
	if err != nil {
		log.Error("alertpush: mark completed", "status", status, "err", err)
		return
	}
	if !ok {
		// An operator canceled between the last AdvanceCursor and here, or
		// another worker finished it: the row is no longer ours to move.
		log.Info("alertpush: push no longer sending; completion skipped", "status", status)
		return
	}
	log.Info("alertpush: completed", "status", status)
}
