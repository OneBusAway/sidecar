package alertpush_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/alertpush"
	"github.com/OneBusAway/sidecar/internal/alerts"
	"github.com/OneBusAway/sidecar/internal/pushreg"
	"github.com/OneBusAway/sidecar/internal/regions"
	"github.com/OneBusAway/sidecar/internal/store/sqlite"
	"github.com/OneBusAway/sidecar/internal/store/sqlitetest"
)

var base = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func ptr[T any](v T) *T { return &v }

type fixture struct {
	store *sqlite.Store
	enq   *alertpush.Enqueuer
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	store := sqlitetest.Open(t)
	ctx := context.Background()
	if err := store.Regions().UpsertFromDirectory(ctx, []regions.Region{{ID: 1, Name: "R", OBABaseURL: "https://x/", Active: true}}, base); err != nil {
		t.Fatal(err)
	}
	return fixture{store: store, enq: &alertpush.Enqueuer{Repo: store.AlertPushes(), Alerts: store.Alerts(), PushRegs: store.PushRegs()}}
}

func (f fixture) alert(t *testing.T, published, isTest bool) alerts.Alert {
	t.Helper()
	ctx := context.Background()
	a, err := f.store.Alerts().Create(ctx, alerts.NewAlert{RegionID: 1, AgencyID: "1", HeaderText: "Hdr", DescriptionText: "Desc",
		Cause: "CONSTRUCTION", Effect: "DETOUR", Severity: "WARNING", StartTime: base, IsTest: isTest}, base)
	if err != nil {
		t.Fatal(err)
	}
	if published {
		if err := f.store.Alerts().SetPublished(ctx, a.ID, true, base); err != nil {
			t.Fatal(err)
		}
	}
	return a
}

func (f fixture) register(t *testing.T, token string, test bool) {
	t.Helper()
	up := pushreg.Upsert{RegionID: 1, Token: token, OperatingSystem: pushreg.OSIOS}
	if test {
		up.TestDevice, up.Description = ptr(true), ptr("QA")
	}
	if err := f.store.PushRegs().Upsert(context.Background(), up, base); err != nil {
		t.Fatal(err)
	}
}

func TestEnqueueHappyPathSnapshotsCopy(t *testing.T) {
	f := newFixture(t)
	a := f.alert(t, true, false)
	f.register(t, "tok", false)
	p, err := f.enq.Enqueue(context.Background(), a.ID, alertpush.AudienceAll, nil, base)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if p.Status != alertpush.StatusQueued || p.Audience != alertpush.AudienceAll || p.RegionID != 1 {
		t.Errorf("push = %+v", p)
	}
	if p.Messages["en"] != (alertpush.Message{Title: "Hdr", Body: "Desc"}) {
		t.Errorf("Messages = %+v", p.Messages)
	}
}

func TestEnqueueRejectsUnpublished(t *testing.T) {
	f := newFixture(t)
	a := f.alert(t, false, false)
	f.register(t, "tok", false)
	if _, err := f.enq.Enqueue(context.Background(), a.ID, alertpush.AudienceAll, nil, base); !errors.Is(err, alertpush.ErrNotPublished) {
		t.Errorf("err = %v, want ErrNotPublished", err)
	}
}

func TestEnqueueRejectsUnknownAlert(t *testing.T) {
	f := newFixture(t)
	if _, err := f.enq.Enqueue(context.Background(), 999, alertpush.AudienceAll, nil, base); !errors.Is(err, alerts.ErrNotFound) {
		t.Errorf("err = %v, want alerts.ErrNotFound", err)
	}
}

func TestEnqueueRejectsEmptyAudience(t *testing.T) {
	f := newFixture(t)
	a := f.alert(t, true, false)
	f.register(t, "tok", false) // a non-test device: the test audience is empty
	if _, err := f.enq.Enqueue(context.Background(), a.ID, alertpush.AudienceTest, nil, base); !errors.Is(err, alertpush.ErrEmptyAudience) {
		t.Errorf("err = %v, want ErrEmptyAudience", err)
	}
}

func TestEnqueueTestAlertForcesTestAudience(t *testing.T) {
	f := newFixture(t)
	a := f.alert(t, true, true)
	f.register(t, "qa", true)
	p, err := f.enq.Enqueue(context.Background(), a.ID, alertpush.AudienceAll, nil, base)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if p.Audience != alertpush.AudienceTest {
		t.Errorf("Audience = %s, want test (forced for a test alert)", p.Audience)
	}
	rep, err := f.enq.AudienceFor(context.Background(), a.ID)
	if err != nil || !rep.ForcedTest || rep.Test.Total != 1 {
		t.Errorf("AudienceFor = %+v, %v", rep, err)
	}
}

func TestEnqueueRejectsInFlight(t *testing.T) {
	f := newFixture(t)
	a := f.alert(t, true, false)
	f.register(t, "tok", false)
	if _, err := f.enq.Enqueue(context.Background(), a.ID, alertpush.AudienceAll, nil, base); err != nil {
		t.Fatal(err)
	}
	if _, err := f.enq.Enqueue(context.Background(), a.ID, alertpush.AudienceAll, nil, base); !errors.Is(err, alertpush.ErrInFlight) {
		t.Errorf("second Enqueue = %v, want ErrInFlight", err)
	}
}

// finish moves every queued push to sent, so the in-flight rule (one push
// per alert at a time) is out of the way of the cooldown assertions.
func (f fixture) finish(t *testing.T, now time.Time, submitted bool) {
	t.Helper()
	ctx := context.Background()
	claimed, err := f.store.AlertPushes().Claim(ctx, now, now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range claimed {
		status := alertpush.StatusSent
		if !submitted {
			status = alertpush.StatusCanceled
		} else if _, err := f.store.AlertPushes().AdvanceCursor(ctx, p.ID, p.BatchCursor, p.BatchCursor+1, 1, now); err != nil {
			t.Fatal(err)
		}
		if _, err := f.store.AlertPushes().MarkCompleted(ctx, p.ID, status, "", now); err != nil {
			t.Fatal(err)
		}
	}
}

// TestEnqueueCooldown pins SC-27: a modest brake on "all" pushes that a
// compromised push-scoped key could otherwise loop, tuned so it never gets
// in an agency's way. A repeat of the same alert within AlertSpacing is
// refused (a double click, a script loop), but a correction minutes later
// goes through; more than RegionHourlyLimit "all" pushes in one region in
// an hour is refused; test-audience pushes are never throttled; and a push
// that was canceled or failed before reaching anyone does not count.
func TestEnqueueCooldown(t *testing.T) {
	f := newFixture(t)
	f.enq.AlertSpacing = time.Minute
	f.enq.RegionHourlyLimit = 3
	ctx := context.Background()
	f.register(t, "tok", false)
	f.register(t, "qa", true)
	a := f.alert(t, true, false)

	if _, err := f.enq.Enqueue(ctx, a.ID, alertpush.AudienceAll, nil, base); err != nil {
		t.Fatalf("first push: %v", err)
	}
	f.finish(t, base.Add(10*time.Second), true)
	if _, err := f.enq.Enqueue(ctx, a.ID, alertpush.AudienceAll, nil, base.Add(30*time.Second)); !errors.Is(err, alertpush.ErrCooldown) {
		t.Fatalf("same alert 30s later: err = %v, want ErrCooldown", err)
	}
	// Test-audience sends are how operators check copy; never throttled.
	for i := range 5 {
		if _, err := f.enq.Enqueue(ctx, a.ID, alertpush.AudienceTest, nil, base.Add(31*time.Second)); err != nil {
			t.Fatalf("test push %d: %v", i, err)
		}
		f.finish(t, base.Add(31*time.Second), true)
	}
	// A correction a few minutes later is a legitimate second send.
	if _, err := f.enq.Enqueue(ctx, a.ID, alertpush.AudienceAll, nil, base.Add(3*time.Minute)); err != nil {
		t.Fatalf("correction 3 minutes later: %v", err)
	}
	f.finish(t, base.Add(3*time.Minute), true)

	// Region budget: a and its correction are 2 of 3 this hour.
	b := f.alert(t, true, false)
	if _, err := f.enq.Enqueue(ctx, b.ID, alertpush.AudienceAll, nil, base.Add(5*time.Minute)); err != nil {
		t.Fatalf("third push in the hour: %v", err)
	}
	f.finish(t, base.Add(5*time.Minute), true)
	c := f.alert(t, true, false)
	if _, err := f.enq.Enqueue(ctx, c.ID, alertpush.AudienceAll, nil, base.Add(6*time.Minute)); !errors.Is(err, alertpush.ErrCooldown) {
		t.Fatalf("fourth push in the hour: err = %v, want ErrCooldown", err)
	}
	// A test alert is forced to the test audience, so it is unthrottled too.
	ta := f.alert(t, true, true)
	if _, err := f.enq.Enqueue(ctx, ta.ID, alertpush.AudienceAll, nil, base.Add(6*time.Minute)); err != nil {
		t.Fatalf("test alert over the region budget: %v", err)
	}
	f.finish(t, base.Add(6*time.Minute), true)
	// The window rolls: an hour after the first send, there is room again.
	if _, err := f.enq.Enqueue(ctx, c.ID, alertpush.AudienceAll, nil, base.Add(time.Hour+time.Second)); err != nil {
		t.Fatalf("after the hour: %v", err)
	}
	f.finish(t, base.Add(time.Hour+time.Second), false) // canceled before any batch

	// That canceled push reached nobody, so it neither spaces the alert nor
	// spends the region budget.
	if _, err := f.enq.Enqueue(ctx, c.ID, alertpush.AudienceAll, nil, base.Add(time.Hour+2*time.Second)); err != nil {
		t.Fatalf("resend after an unsent cancel: %v", err)
	}

	// The CLI's Enqueuer sets neither knob: operators with database access
	// are not throttled.
	cli := &alertpush.Enqueuer{Repo: f.store.AlertPushes(), Alerts: f.store.Alerts(), PushRegs: f.store.PushRegs()}
	f.finish(t, base.Add(time.Hour+3*time.Second), true)
	if _, err := cli.Enqueue(ctx, c.ID, alertpush.AudienceAll, nil, base.Add(time.Hour+4*time.Second)); err != nil {
		t.Fatalf("unthrottled Enqueuer: %v", err)
	}
}
