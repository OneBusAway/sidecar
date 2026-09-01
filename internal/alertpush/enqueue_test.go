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
