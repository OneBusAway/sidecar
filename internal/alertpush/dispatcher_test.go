package alertpush_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/alertpush"
	"github.com/OneBusAway/sidecar/internal/alerts"
	"github.com/OneBusAway/sidecar/internal/push"
	"github.com/OneBusAway/sidecar/internal/pushreg"
)

// fakeSender records every batch. failOn returns an error for the nth call
// (1-based) when non-zero; rejectTokens are reported as inline rejections.
type fakeSender struct {
	mu           sync.Mutex
	calls        []sentBatch
	failOn       int
	rejectTokens map[string]string
}

type sentBatch struct {
	notifID string
	n       push.Notification
}

func (s *fakeSender) SendBatch(_ context.Context, n push.Notification, notifID string) (push.BatchResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, sentBatch{notifID: notifID, n: n})
	if s.failOn == len(s.calls) {
		return push.BatchResult{}, errors.New("gorush: 502")
	}
	var res push.BatchResult
	for _, tok := range n.Tokens {
		if reason, ok := s.rejectTokens[tok]; ok {
			res.Rejected = append(res.Rejected, push.Rejection{Token: tok, Reason: reason})
		}
	}
	return res, nil
}

// cancelingSender cancels the push out from under the dispatcher on its
// first batch, standing in for an operator hitting cancel mid-send.
type cancelingSender struct {
	t      *testing.T
	repo   alertpush.Repository
	pushID int64
	calls  int
}

func (s *cancelingSender) SendBatch(ctx context.Context, _ push.Notification, _ string) (push.BatchResult, error) {
	s.calls++
	if s.calls == 1 {
		if err := s.repo.Cancel(ctx, s.pushID, base); err != nil {
			s.t.Errorf("cancel mid-send: %v", err)
		}
	}
	return push.BatchResult{}, nil
}

type alwaysFail struct{}

func (alwaysFail) SendBatch(context.Context, push.Notification, string) (push.BatchResult, error) {
	return push.BatchResult{}, errors.New("down")
}

func newDispatcher(f fixture, sender push.BatchSender, now *time.Time) *alertpush.Dispatcher {
	return &alertpush.Dispatcher{
		Repo: f.store.AlertPushes(), Alerts: f.store.Alerts(), PushRegs: f.store.PushRegs(),
		Sender: sender, Now: func() time.Time { return *now },
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func (f fixture) registerFull(t *testing.T, token, os, locale string, sandbox, test bool) {
	t.Helper()
	up := pushreg.Upsert{RegionID: 1, Token: token, OperatingSystem: os, APNSSandbox: sandbox, Locale: ptr(locale)}
	if test {
		up.TestDevice, up.Description = ptr(true), ptr("QA")
	}
	if err := f.store.PushRegs().Upsert(context.Background(), up, base); err != nil {
		t.Fatal(err)
	}
}

func (f fixture) translate(t *testing.T, a alerts.Alert, lang string, field alerts.Field, text string) {
	t.Helper()
	src := a.HeaderText
	if field == alerts.FieldDescription {
		src = a.DescriptionText
	}
	if err := f.store.Alerts().UpsertTranslation(context.Background(), a.ID, alerts.Translation{
		Language: lang, Field: field, Text: text, SourceSHA256: alerts.SourceHash(src)}, base); err != nil {
		t.Fatal(err)
	}
}

func TestDispatcherGroupsByPlatformLocaleAndSandbox(t *testing.T) {
	f := newFixture(t)
	a := f.alert(t, true, false)
	f.translate(t, a, "es", alerts.FieldHeader, "Título")
	f.registerFull(t, "ios-en-prod", pushreg.OSIOS, "en-US", false, false)
	f.registerFull(t, "ios-en-sandbox", pushreg.OSIOS, "", true, false)
	f.registerFull(t, "ios-es-mx", pushreg.OSIOS, "es-MX", false, false) // bare-subtag match → es
	f.registerFull(t, "android-es", pushreg.OSAndroid, "es", false, false)
	f.registerFull(t, "android-de", pushreg.OSAndroid, "de", false, false) // no catalog match → English
	// Registration writes apns_sandbox for every platform, so an Android row
	// can carry true; it must not split the FCM batch (design spec §2.6).
	f.registerFull(t, "android-de-sandbox", pushreg.OSAndroid, "de", true, false)
	p, err := f.enq.Enqueue(context.Background(), a.ID, alertpush.AudienceAll, nil, base)
	if err != nil {
		t.Fatal(err)
	}

	sender := &fakeSender{}
	now := base.Add(time.Second)
	d := newDispatcher(f, sender, &now)
	d.RunOnce(context.Background())

	// Expect exactly five groups; the order of the groups within a page is
	// not contractual, so match by content. Token order within a group is:
	// ListAudience pages ascending by registration id.
	type key struct {
		platform push.Platform
		sandbox  bool
		title    string
		body     string
	}
	got := map[key][]string{}
	for _, c := range sender.calls {
		if c.notifID != alertpush.NotifID(p.ID) {
			t.Errorf("notif_id = %q, want %q", c.notifID, alertpush.NotifID(p.ID))
		}
		// The fan-out carries copy only; the structured payload is the
		// alarm/Live Activity path, not this one.
		if c.n.Data != nil {
			t.Errorf("Data = %v, want nil", c.n.Data)
		}
		got[key{c.n.Platform, c.n.Sandbox, c.n.Title, c.n.Message}] = c.n.Tokens
	}
	// Only the header is translated, so every group's body is the alert's
	// English description.
	want := map[key][]string{
		{push.PlatformIOS, false, "Hdr", "Desc"}:        {"ios-en-prod"},
		{push.PlatformIOS, true, "Hdr", "Desc"}:         {"ios-en-sandbox"},
		{push.PlatformIOS, false, "Título", "Desc"}:     {"ios-es-mx"},
		{push.PlatformAndroid, false, "Título", "Desc"}: {"android-es"},
		{push.PlatformAndroid, false, "Hdr", "Desc"}:    {"android-de", "android-de-sandbox"},
	}
	if len(got) != len(want) {
		t.Fatalf("groups = %v, want %v", got, want)
	}
	for k, toks := range want {
		if !slices.Equal(got[k], toks) {
			t.Errorf("group %+v tokens = %v, want %v", k, got[k], toks)
		}
	}
	final, _ := f.store.AlertPushes().Get(context.Background(), p.ID)
	if final.Status != alertpush.StatusSent || final.DeviceCount != 6 || final.SubmittedCount != 6 || final.FailedCount != 0 {
		t.Errorf("final push = %+v", final)
	}
	if final.CompletedAt == nil || !final.CompletedAt.Equal(now) {
		t.Errorf("CompletedAt = %v, want %v", final.CompletedAt, now)
	}
}

func TestDispatcherResumesFromCursorAfterTransportError(t *testing.T) {
	f := newFixture(t)
	a := f.alert(t, true, false)
	// Exercise paging with one page of BatchSize plus a short remainder:
	// every registration shares a group, so page N is exactly one batch.
	for i := 0; i < alertpush.BatchSize+3; i++ {
		f.registerFull(t, "tok-"+strconv.Itoa(i), pushreg.OSAndroid, "", false, false)
	}
	p, err := f.enq.Enqueue(context.Background(), a.ID, alertpush.AudienceAll, nil, base)
	if err != nil {
		t.Fatal(err)
	}
	sender := &fakeSender{failOn: 2} // page 1 succeeds, page 2's (only) group fails
	now := base
	d := newDispatcher(f, sender, &now)
	d.RunOnce(context.Background())

	mid, _ := f.store.AlertPushes().Get(context.Background(), p.ID)
	if mid.Status != alertpush.StatusSending || mid.Attempts != 1 || mid.LastError != "gorush: 502" {
		t.Fatalf("after failure: %+v", mid)
	}
	if mid.SubmittedCount != alertpush.BatchSize || mid.BatchCursor == 0 {
		t.Fatalf("page 1 not committed: submitted %d cursor %d", mid.SubmittedCount, mid.BatchCursor)
	}

	// Not yet stuck: a cycle 1 minute later leaves it alone.
	now = base.Add(time.Minute)
	d.RunOnce(context.Background())
	if len(sender.calls) != 2 {
		t.Fatalf("calls after non-stuck cycle = %d, want 2", len(sender.calls))
	}

	// Stuck: reclaimed, resumes at the cursor and sends ONLY the last page.
	now = base.Add(alertpush.StuckAfter + time.Minute)
	d.RunOnce(context.Background())
	if len(sender.calls) != 3 {
		t.Fatalf("calls after reclaim = %d, want 3", len(sender.calls))
	}
	if got := len(sender.calls[2].n.Tokens); got != 3 {
		t.Errorf("resumed page size = %d, want 3 (the remainder, not the whole audience)", got)
	}
	final, _ := f.store.AlertPushes().Get(context.Background(), p.ID)
	if final.Status != alertpush.StatusSent || final.SubmittedCount != int64(alertpush.BatchSize+3) {
		t.Errorf("final = %+v", final)
	}
	if final.Attempts != 0 || final.LastError != "" {
		t.Errorf("a committed page must clear the streak: attempts %d last_error %q", final.Attempts, final.LastError)
	}
}

func TestDispatcherMarksFailedAfterMaxAttempts(t *testing.T) {
	f := newFixture(t)
	a := f.alert(t, true, false)
	f.register(t, "tok", false)
	p, _ := f.enq.Enqueue(context.Background(), a.ID, alertpush.AudienceAll, nil, base)
	now := base
	d := newDispatcher(f, &alwaysFail{}, &now)
	for i := 0; i < alertpush.MaxAttempts; i++ {
		d.RunOnce(context.Background())
		now = now.Add(alertpush.StuckAfter + time.Minute)
	}
	final, _ := f.store.AlertPushes().Get(context.Background(), p.ID)
	if final.Status != alertpush.StatusFailed || final.Attempts != alertpush.MaxAttempts || final.CompletedAt == nil {
		t.Errorf("final = %+v", final)
	}
}

func TestDispatcherCountsInlineRejections(t *testing.T) {
	f := newFixture(t)
	a := f.alert(t, true, false)
	f.register(t, "good", false)
	f.register(t, "bad", false)
	p, _ := f.enq.Enqueue(context.Background(), a.ID, alertpush.AudienceAll, nil, base)
	now := base
	d := newDispatcher(f, &fakeSender{rejectTokens: map[string]string{"bad": "BadDeviceToken"}}, &now)
	d.RunOnce(context.Background())
	final, _ := f.store.AlertPushes().Get(context.Background(), p.ID)
	if final.SubmittedCount != 1 || final.FailedCount != 1 || final.Status != alertpush.StatusSent {
		t.Errorf("final = %+v", final)
	}
	if len(final.FailureReasons) != 1 || final.FailureReasons[0] != (alertpush.FailureReason{Reason: "BadDeviceToken", Count: 1}) {
		t.Errorf("FailureReasons = %v", final.FailureReasons)
	}
}

func TestDispatcherCanceledPushIsNotSent(t *testing.T) {
	f := newFixture(t)
	a := f.alert(t, true, false)
	f.register(t, "tok", false)
	p, _ := f.enq.Enqueue(context.Background(), a.ID, alertpush.AudienceAll, nil, base)
	if err := f.store.AlertPushes().Cancel(context.Background(), p.ID, base); err != nil {
		t.Fatal(err)
	}
	sender := &fakeSender{}
	now := base
	newDispatcher(f, sender, &now).RunOnce(context.Background())
	if len(sender.calls) != 0 {
		t.Errorf("calls = %d, want 0 for a canceled push", len(sender.calls))
	}
	final, _ := f.store.AlertPushes().Get(context.Background(), p.ID)
	if final.Status != alertpush.StatusCanceled {
		t.Errorf("Status = %s, want canceled (Claim must not resurrect it)", final.Status)
	}
}

func TestDispatcherCancelMidSendYieldsWithoutCommittingPage(t *testing.T) {
	f := newFixture(t)
	a := f.alert(t, true, false)
	// Two pages: if the yield does not fire, the dispatcher walks on to the
	// second one and the batch count gives it away.
	for i := 0; i < alertpush.BatchSize+3; i++ {
		f.registerFull(t, "tok-"+strconv.Itoa(i), pushreg.OSAndroid, "", false, false)
	}
	p, err := f.enq.Enqueue(context.Background(), a.ID, alertpush.AudienceAll, nil, base)
	if err != nil {
		t.Fatal(err)
	}
	sender := &cancelingSender{t: t, repo: f.store.AlertPushes(), pushID: p.ID}
	now := base
	newDispatcher(f, sender, &now).RunOnce(context.Background())

	if sender.calls != 1 {
		t.Errorf("batches = %d, want 1 (AdvanceCursor must report the cancel and stop the send)", sender.calls)
	}
	final, _ := f.store.AlertPushes().Get(context.Background(), p.ID)
	if final.Status != alertpush.StatusCanceled {
		t.Errorf("Status = %s, want canceled (the dispatcher must not overwrite the operator's cancel)", final.Status)
	}
	// The row stopped being ours the moment it left sending, so the
	// in-flight page's progress is deliberately dropped (design spec §2.6).
	if final.SubmittedCount != 0 || final.BatchCursor != 0 {
		t.Errorf("uncommitted page leaked: submitted %d cursor %d, want 0 and 0", final.SubmittedCount, final.BatchCursor)
	}
}

func TestDispatcherTestAudienceReachesOnlyTestDevices(t *testing.T) {
	f := newFixture(t)
	a := f.alert(t, true, false)
	f.register(t, "qa", true)
	f.register(t, "rider", false)
	p, err := f.enq.Enqueue(context.Background(), a.ID, alertpush.AudienceTest, nil, base)
	if err != nil {
		t.Fatal(err)
	}
	sender := &fakeSender{}
	now := base
	newDispatcher(f, sender, &now).RunOnce(context.Background())

	if len(sender.calls) != 1 {
		t.Fatalf("batches = %d, want 1", len(sender.calls))
	}
	if got := sender.calls[0].n.Tokens; !slices.Equal(got, []string{"qa"}) {
		t.Errorf("tokens = %v, want only the test device", got)
	}
	final, _ := f.store.AlertPushes().Get(context.Background(), p.ID)
	if final.Status != alertpush.StatusSent || final.DeviceCount != 1 || final.SubmittedCount != 1 {
		t.Errorf("final = %+v, want sent with a 1-device test audience", final)
	}
}

func TestDispatcherUnpublishedAlertCancelsPush(t *testing.T) {
	f := newFixture(t)
	a := f.alert(t, true, false)
	f.register(t, "tok", false)
	p, _ := f.enq.Enqueue(context.Background(), a.ID, alertpush.AudienceAll, nil, base)
	if err := f.store.Alerts().SetPublished(context.Background(), a.ID, false, base); err != nil {
		t.Fatal(err)
	}
	sender := &fakeSender{}
	now := base
	newDispatcher(f, sender, &now).RunOnce(context.Background())
	final, _ := f.store.AlertPushes().Get(context.Background(), p.ID)
	if final.Status != alertpush.StatusCanceled || len(sender.calls) != 0 || final.LastError == "" {
		t.Errorf("final = %+v, calls %d", final, len(sender.calls))
	}
}

func TestDispatcherNoSenderFailsPush(t *testing.T) {
	f := newFixture(t)
	a := f.alert(t, true, false)
	f.register(t, "tok", false)
	p, _ := f.enq.Enqueue(context.Background(), a.ID, alertpush.AudienceAll, nil, base)
	now := base
	newDispatcher(f, nil, &now).RunOnce(context.Background())
	final, _ := f.store.AlertPushes().Get(context.Background(), p.ID)
	if final.Status != alertpush.StatusFailed {
		t.Errorf("Status = %s, want failed", final.Status)
	}
	// Operators read this string in the SPA; it must name both knobs.
	const want = "no push transport configured (--gorush-url/SIDECAR_GORUSH_URL)"
	if final.LastError != want {
		t.Errorf("LastError = %q, want %q", final.LastError, want)
	}
}

// TestDispatcherWakeSignalsWakeC pins the dispatcher's side of the wake
// contract: Wake makes WakeC readable, never blocks, and coalesces --
// repeated Wakes before the runner reads yield one signal. That a signal
// on Loop.Wake produces a RunOnce is lease.Runner's contract, pinned in
// its own tests.
func TestDispatcherWakeSignalsWakeC(t *testing.T) {
	d := &alertpush.Dispatcher{}
	d.Wake()
	d.Wake()
	d.Wake()
	select {
	case <-d.WakeC():
	default:
		t.Fatal("WakeC not readable after Wake")
	}
	select {
	case <-d.WakeC():
		t.Fatal("second signal readable; want repeated Wakes coalesced into one")
	default:
	}
}

// failingSender always errors and counts its calls, standing in for a
// transport that is down for the whole test.
type failingSender struct{ calls int }

func (s *failingSender) SendBatch(context.Context, push.Notification, string) (push.BatchResult, error) {
	s.calls++
	return push.BatchResult{}, errors.New("down")
}

func TestDispatcherFirstCycleAdoptsOrphanedSends(t *testing.T) {
	f := newFixture(t)
	a := f.alert(t, true, false)
	f.register(t, "tok", false)
	p, err := f.enq.Enqueue(context.Background(), a.ID, alertpush.AudienceAll, nil, base)
	if err != nil {
		t.Fatal(err)
	}

	// A process that claimed the push and then died leaves the row `sending`
	// with a fresh updated_at -- exactly what a SIGTERM mid-cycle produces.
	crashed := &failingSender{}
	now := base
	d1 := newDispatcher(f, crashed, &now)
	d1.RunOnce(context.Background())
	orphan, _ := f.store.AlertPushes().Get(context.Background(), p.ID)
	if orphan.Status != alertpush.StatusSending || !orphan.UpdatedAt.Equal(now) {
		t.Fatalf("orphan = status %s updated %v, want sending at %v", orphan.Status, orphan.UpdatedAt, now)
	}

	// A second cycle of the SAME dispatcher must not re-adopt: adoption is
	// first-cycle-only, and this row is not yet stuck.
	d1.RunOnce(context.Background())
	if crashed.calls != 1 {
		t.Errorf("sends by the same dispatcher = %d, want 1 (a fresh sending row is not re-adopted)", crashed.calls)
	}

	// The restarted process adopts it at once -- no StuckAfter wait, and the
	// clock has not moved.
	sender := &fakeSender{}
	d2 := newDispatcher(f, sender, &now)
	d2.RunOnce(context.Background())
	if len(sender.calls) != 1 {
		t.Fatalf("sends after restart = %d, want 1 (first cycle adopts every sending row)", len(sender.calls))
	}
	final, _ := f.store.AlertPushes().Get(context.Background(), p.ID)
	if final.Status != alertpush.StatusSent || final.SubmittedCount != 1 {
		t.Errorf("final = %+v, want sent with 1 submitted", final)
	}
}

// cursorFailRepo is a real repository whose cursor commit always fails,
// standing in for a store that has gone read-only mid-send.
type cursorFailRepo struct {
	alertpush.Repository
	err error
}

func (r cursorFailRepo) AdvanceCursor(context.Context, int64, int64, int64, int64, time.Time) (bool, error) {
	return false, r.err
}

func TestDispatcherCursorWriteFailureCountsAsAttempt(t *testing.T) {
	f := newFixture(t)
	a := f.alert(t, true, false)
	f.register(t, "tok", false)
	p, err := f.enq.Enqueue(context.Background(), a.ID, alertpush.AudienceAll, nil, base)
	if err != nil {
		t.Fatal(err)
	}
	sender := &fakeSender{}
	now := base
	d := newDispatcher(f, sender, &now)
	d.Repo = cursorFailRepo{Repository: f.store.AlertPushes(), err: errors.New("disk full")}

	d.RunOnce(context.Background())
	mid, _ := f.store.AlertPushes().Get(context.Background(), p.ID)
	if mid.Attempts != 1 || mid.Status != alertpush.StatusSending {
		t.Fatalf("after a failed cursor commit: attempts %d status %s, want 1 and sending", mid.Attempts, mid.Status)
	}
	if mid.LastError != "disk full" {
		t.Errorf("LastError = %q, want the store error verbatim", mid.LastError)
	}

	// Without the attempt count this page would be re-sent on every reclaim
	// forever; MaxAttempts bounds the duplicates instead.
	for i := 1; i < alertpush.MaxAttempts; i++ {
		now = now.Add(alertpush.StuckAfter + time.Minute)
		d.RunOnce(context.Background())
	}
	final, _ := f.store.AlertPushes().Get(context.Background(), p.ID)
	if final.Status != alertpush.StatusFailed || final.Attempts != alertpush.MaxAttempts {
		t.Errorf("final = status %s attempts %d, want failed and %d", final.Status, final.Attempts, alertpush.MaxAttempts)
	}
	if len(sender.calls) != alertpush.MaxAttempts {
		t.Errorf("batches = %d, want %d (one duplicate per attempt, then no more)", len(sender.calls), alertpush.MaxAttempts)
	}
}
