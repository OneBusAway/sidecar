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
	p, err := f.enq.Enqueue(context.Background(), a.ID, alertpush.AudienceAll, base)
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
	}
	got := map[key][]string{}
	for _, c := range sender.calls {
		if c.notifID != alertpush.NotifID(p.ID) {
			t.Errorf("notif_id = %q, want %q", c.notifID, alertpush.NotifID(p.ID))
		}
		got[key{c.n.Platform, c.n.Sandbox, c.n.Title}] = c.n.Tokens
	}
	want := map[key][]string{
		{push.PlatformIOS, false, "Hdr"}:        {"ios-en-prod"},
		{push.PlatformIOS, true, "Hdr"}:         {"ios-en-sandbox"},
		{push.PlatformIOS, false, "Título"}:     {"ios-es-mx"},
		{push.PlatformAndroid, false, "Título"}: {"android-es"},
		{push.PlatformAndroid, false, "Hdr"}:    {"android-de", "android-de-sandbox"},
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
	p, err := f.enq.Enqueue(context.Background(), a.ID, alertpush.AudienceAll, base)
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
	p, _ := f.enq.Enqueue(context.Background(), a.ID, alertpush.AudienceAll, base)
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
	p, _ := f.enq.Enqueue(context.Background(), a.ID, alertpush.AudienceAll, base)
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
	p, _ := f.enq.Enqueue(context.Background(), a.ID, alertpush.AudienceAll, base)
	if err := f.store.AlertPushes().Cancel(context.Background(), p.ID, base); err != nil {
		t.Fatal(err)
	}
	sender := &fakeSender{}
	now := base
	newDispatcher(f, sender, &now).RunOnce(context.Background())
	if len(sender.calls) != 0 {
		t.Errorf("calls = %d, want 0 for a canceled push", len(sender.calls))
	}
}

func TestDispatcherUnpublishedAlertCancelsPush(t *testing.T) {
	f := newFixture(t)
	a := f.alert(t, true, false)
	f.register(t, "tok", false)
	p, _ := f.enq.Enqueue(context.Background(), a.ID, alertpush.AudienceAll, base)
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
	p, _ := f.enq.Enqueue(context.Background(), a.ID, alertpush.AudienceAll, base)
	now := base
	newDispatcher(f, nil, &now).RunOnce(context.Background())
	final, _ := f.store.AlertPushes().Get(context.Background(), p.ID)
	if final.Status != alertpush.StatusFailed || final.LastError == "" {
		t.Errorf("final = %+v", final)
	}
}

func TestDispatcherWakeTriggersRunWithoutTick(t *testing.T) {
	f := newFixture(t)
	a := f.alert(t, true, false)
	f.register(t, "tok", false)
	p, _ := f.enq.Enqueue(context.Background(), a.ID, alertpush.AudienceAll, base)
	sender := &fakeSender{}
	now := base
	d := newDispatcher(f, sender, &now)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { d.RunLoop(ctx, time.Hour); close(done) }()
	d.Wake()
	deadline := time.Now().Add(5 * time.Second)
	for {
		final, _ := f.store.AlertPushes().Get(context.Background(), p.ID)
		if final.Status == alertpush.StatusSent {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("push not sent after Wake; status %s", final.Status)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	// Wake before RunLoop and repeated Wakes must never block.
	d2 := newDispatcher(f, sender, &now)
	d2.Wake()
	d2.Wake()
}
