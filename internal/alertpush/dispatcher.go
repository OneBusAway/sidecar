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

// RunLoop runs a cycle on every tick and on every Wake until ctx is done.
func (d *Dispatcher) RunLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	wake := d.wakeCh()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.RunOnce(ctx)
		case <-wake:
			d.RunOnce(ctx)
		}
	}
}

// RunOnce claims every push that is due and sends each in turn. Exported
// so tests drive cycles without a ticker.
func (d *Dispatcher) RunOnce(ctx context.Context) {
	now := d.Now()
	claimed, err := d.Repo.Claim(ctx, now, now.Add(-StuckAfter))
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
			log.Warn("alertpush: set device count", "err", err)
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
		submitted, err := d.sendPage(ctx, p, page, catalog)
		if err != nil {
			d.recordAttempt(ctx, log, p.ID, err)
			return
		}
		last := page[len(page)-1].ID
		ok, err := d.Repo.AdvanceCursor(ctx, p.ID, cursor, last, submitted, d.Now())
		if err != nil {
			log.Warn("alertpush: advance cursor", "err", err)
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
func (d *Dispatcher) sendPage(ctx context.Context, p Push, page []pushreg.Registration, catalog []string) (int64, error) {
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
				d.Logger.Warn("alertpush: record inline rejection", "push_id", p.ID, "err", err)
			}
		}
		submitted += int64(len(tokens) - len(res.Rejected))
	}
	return submitted, nil
}

// recordAttempt counts one transport failure and gives up at MaxAttempts
// (design spec §2.7). The push keeps its cursor either way.
func (d *Dispatcher) recordAttempt(ctx context.Context, log *slog.Logger, id int64, sendErr error) {
	attempts, err := d.Repo.RecordAttempt(ctx, id, sendErr.Error(), d.Now())
	if err != nil {
		log.Error("alertpush: record attempt", "err", err)
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
