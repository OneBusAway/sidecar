package storetest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/liveactivities"
	"github.com/OneBusAway/sidecar/internal/regions"
)

type newLiveActivityStoreFunc func(*testing.T) (liveactivities.Repository, regions.Repository)

// RunLiveActivityRepository exercises a liveactivities.Repository against
// the behavioral contract every engine must satisfy.
func RunLiveActivityRepository(t *testing.T, newStore newLiveActivityStoreFunc) {
	t.Helper()
	t.Run("UpsertInsertsAndRoundTrips", func(t *testing.T) { testLAUpsertInserts(t, newStore) })
	t.Run("UpsertUpdatesRegistrationAndPreservesBookkeeping", func(t *testing.T) { testLAUpsertUpdates(t, newStore) })
	t.Run("UpsertIsRegionScoped", func(t *testing.T) { testLAUpsertRegionScoped(t, newStore) })
	t.Run("DeleteByTokenReports204Contract", func(t *testing.T) { testLADeleteByToken(t, newStore) })
	t.Run("DeleteByPushTokenCountsRows", func(t *testing.T) { testLADeleteByPushToken(t, newStore) })
	t.Run("FailureCounterIncrementsAndResets", func(t *testing.T) { testLAFailureCounter(t, newStore) })
	t.Run("RecordPushRoundTripsStateAndInstant", func(t *testing.T) { testLARecordPush(t, newStore) })
	t.Run("DeleteByIDTreatsMissingAsSuccess", func(t *testing.T) { testLADeleteByIDMissing(t, newStore) })
	t.Run("ConcurrentFirstRegistrationRace", func(t *testing.T) { testLAConcurrentFirstRegistrationRace(t, newStore) })
	t.Run("DeleteByIDRemovesExistingRow", func(t *testing.T) { testLADeleteByIDRemovesExistingRow(t, newStore) })
}

func fullLAIn(token, activityID string) liveactivities.NewLiveActivity {
	return liveactivities.NewLiveActivity{
		RegionID: 1, Token: token, ExpiresAt: base.Add(8 * time.Hour),
		ActivityID: activityID, PushToken: "push-" + activityID, APNSSandbox: true,
		StopID: "1_570", RouteShortName: "44", TripHeadsign: "Ballard",
		TripID: "1_604370", ServiceDate: 1754809200000, VehicleID: "1_4361", StopSequence: ptr(int64(0)),
	}
}

// findLAByToken reads back the "tok-a" row every subtest registers via List,
// proving the store round trip rather than trusting Upsert's return value.
func findLAByToken(t *testing.T, repo liveactivities.Repository) liveactivities.LiveActivity {
	const token = "tok-a"
	t.Helper()
	list, err := repo.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, la := range list {
		if la.Token == token {
			return la
		}
	}
	t.Fatalf("List() missing token %q (got %+v)", token, list)
	return liveactivities.LiveActivity{}
}

func testLAUpsertInserts(t *testing.T, newStore newLiveActivityStoreFunc) {
	repo, regionRepo := newStore(t)
	ctx := context.Background()
	putStoretestRegion(t, regionRepo, 1)
	in := fullLAIn("tok-a", "act-a")
	created, err := repo.Upsert(ctx, in, base)
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	got := findLAByToken(t, repo)
	if got.ID != created.ID || got.Token != "tok-a" || got.ActivityID != "act-a" || got.PushToken != "push-act-a" ||
		!got.APNSSandbox || got.StopID != "1_570" || got.RouteShortName != "44" || got.TripHeadsign != "Ballard" ||
		got.TripID != "1_604370" || got.ServiceDate != 1754809200000 || got.VehicleID != "1_4361" ||
		got.StopSequence == nil || *got.StopSequence != 0 {
		t.Errorf("round trip lost a field: %+v", got)
	}
	if !got.ExpiresAt.Equal(base.Add(8*time.Hour)) || !got.CreatedAt.Equal(base) {
		t.Errorf("ExpiresAt=%v CreatedAt=%v", got.ExpiresAt, got.CreatedAt)
	}
	if got.LastPushedAt != nil || got.ConsecutiveFailures != 0 || len(got.LastContentState.Arrivals) != 0 {
		t.Errorf("fresh row bookkeeping: %+v", got)
	}
}

func testLAUpsertUpdates(t *testing.T, newStore newLiveActivityStoreFunc) {
	repo, regionRepo := newStore(t)
	ctx := context.Background()
	putStoretestRegion(t, regionRepo, 1)
	first, err := repo.Upsert(ctx, fullLAIn("tok-a", "act-a"), base)
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if _, failErr := repo.RecordFailure(ctx, first.ID); failErr != nil {
		t.Fatal(failErr)
	}
	state := liveactivities.ContentState{Arrivals: []liveactivities.ArrivalInfo{{DepartureTime: 1, ScheduleStatus: "on_time"}}}
	if pushErr := repo.RecordPush(ctx, first.ID, state, base.Add(time.Minute)); pushErr != nil {
		t.Fatal(pushErr)
	}

	in := fullLAIn("tok-IGNORED", "act-a") // same activity, rotated token
	in.PushToken = "push-rotated"
	in.APNSSandbox = false
	in.StopSequence = nil
	in.ExpiresAt = base.Add(99 * time.Hour)
	second, err := repo.Upsert(ctx, in, base.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("Upsert update: %v", err)
	}
	if second.ID != first.ID || second.Token != "tok-a" {
		t.Errorf("update must keep id/token: first=%+v second=%+v", first, second)
	}
	got := findLAByToken(t, repo)
	if got.PushToken != "push-rotated" || got.APNSSandbox || got.StopSequence != nil {
		t.Errorf("registration fields not rewritten: %+v", got)
	}
	if !got.ExpiresAt.Equal(base.Add(8*time.Hour)) || got.ConsecutiveFailures != 1 ||
		got.LastPushedAt == nil || !got.LastPushedAt.Equal(base.Add(time.Minute)) ||
		len(got.LastContentState.Arrivals) != 1 {
		t.Errorf("update must preserve expiry and bookkeeping: %+v", got)
	}
	list, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Errorf("update must not insert a second row: %d rows", len(list))
	}
}

func testLAUpsertRegionScoped(t *testing.T, newStore newLiveActivityStoreFunc) {
	repo, regionRepo := newStore(t)
	ctx := context.Background()
	putStoretestRegion(t, regionRepo, 1)
	putStoretestRegion(t, regionRepo, 2)
	if _, err := repo.Upsert(ctx, fullLAIn("tok-1", "act-a"), base); err != nil {
		t.Fatal(err)
	}
	in := fullLAIn("tok-2", "act-a")
	in.RegionID = 2
	if _, err := repo.Upsert(ctx, in, base); err != nil {
		t.Fatalf("same activity id in another region must insert: %v", err)
	}
	list, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 {
		t.Errorf("want 2 rows, got %d", len(list))
	}
}

func testLADeleteByToken(t *testing.T, newStore newLiveActivityStoreFunc) {
	repo, regionRepo := newStore(t)
	ctx := context.Background()
	putStoretestRegion(t, regionRepo, 1)
	putStoretestRegion(t, regionRepo, 2)
	if _, err := repo.Upsert(ctx, fullLAIn("tok-a", "act-a"), base); err != nil {
		t.Fatal(err)
	}
	if err := repo.Delete(ctx, 2, "tok-a"); !errors.Is(err, liveactivities.ErrNotFound) {
		t.Errorf("wrong region: err = %v, want ErrNotFound", err)
	}
	if err := repo.Delete(ctx, 1, "tok-a"); err != nil {
		t.Errorf("Delete: %v", err)
	}
	if err := repo.Delete(ctx, 1, "tok-a"); !errors.Is(err, liveactivities.ErrNotFound) {
		t.Errorf("second delete: err = %v, want ErrNotFound", err)
	}
}

func testLADeleteByPushToken(t *testing.T, newStore newLiveActivityStoreFunc) {
	repo, regionRepo := newStore(t)
	ctx := context.Background()
	putStoretestRegion(t, regionRepo, 1)
	a := fullLAIn("tok-a", "act-a")
	b := fullLAIn("tok-b", "act-b")
	b.PushToken = a.PushToken
	c := fullLAIn("tok-c", "act-c")
	for _, in := range []liveactivities.NewLiveActivity{a, b, c} {
		if _, err := repo.Upsert(ctx, in, base); err != nil {
			t.Fatal(err)
		}
	}
	n, err := repo.DeleteByPushToken(ctx, a.PushToken)
	if err != nil || n != 2 {
		t.Errorf("DeleteByPushToken = %d, %v; want 2, nil", n, err)
	}
	n, err = repo.DeleteByPushToken(ctx, "nope")
	if err != nil || n != 0 {
		t.Errorf("unknown token = %d, %v; want 0, nil", n, err)
	}
	list, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].Token != "tok-c" {
		t.Errorf("survivor: %+v", list)
	}
}

func testLAFailureCounter(t *testing.T, newStore newLiveActivityStoreFunc) {
	repo, regionRepo := newStore(t)
	ctx := context.Background()
	putStoretestRegion(t, regionRepo, 1)
	la, err := repo.Upsert(ctx, fullLAIn("tok-a", "act-a"), base)
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	for want := int64(1); want <= 3; want++ {
		got, err := repo.RecordFailure(ctx, la.ID)
		if err != nil || got != want {
			t.Fatalf("RecordFailure #%d = %d, %v", want, got, err)
		}
	}
	if err := repo.ResetFailures(ctx, la.ID); err != nil {
		t.Fatal(err)
	}
	if got := findLAByToken(t, repo); got.ConsecutiveFailures != 0 {
		t.Errorf("after reset: %d", got.ConsecutiveFailures)
	}
	if _, err := repo.RecordFailure(ctx, 999999); !errors.Is(err, liveactivities.ErrNotFound) {
		t.Errorf("missing row: err = %v, want ErrNotFound", err)
	}
}

func testLARecordPush(t *testing.T, newStore newLiveActivityStoreFunc) {
	repo, regionRepo := newStore(t)
	ctx := context.Background()
	putStoretestRegion(t, regionRepo, 1)
	la, err := repo.Upsert(ctx, fullLAIn("tok-a", "act-a"), base)
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	state := liveactivities.ContentState{Arrivals: []liveactivities.ArrivalInfo{
		{DepartureTime: 1767980460, ScheduleStatus: "on_time", ScheduleDeviation: 60, IsArrival: false},
		{DepartureTime: 1767981000, ScheduleStatus: "delayed", ScheduleDeviation: 240, IsArrival: true},
	}}
	at := base.Add(90 * time.Second)
	if err := repo.RecordPush(ctx, la.ID, state, at); err != nil {
		t.Fatal(err)
	}
	got := findLAByToken(t, repo)
	if got.LastPushedAt == nil || !got.LastPushedAt.Equal(at) {
		t.Errorf("LastPushedAt = %v, want %v", got.LastPushedAt, at)
	}
	if liveactivities.Changed(got.LastContentState, state) {
		t.Errorf("state round trip: got %+v want %+v", got.LastContentState, state)
	}
}

func testLADeleteByIDMissing(t *testing.T, newStore newLiveActivityStoreFunc) {
	repo, _ := newStore(t)
	if err := repo.DeleteByID(context.Background(), 424242); err != nil {
		t.Errorf("DeleteByID(missing) = %v, want nil", err)
	}
}

// countLAByActivity counts the rows in list whose ActivityID matches
// activityID -- the concurrent-race subtest needs to see exactly one
// survivor for the activity every goroutine raced to register.
func countLAByActivity(list []liveactivities.LiveActivity, activityID string) int {
	n := 0
	for _, la := range list {
		if la.ActivityID == activityID {
			n++
		}
	}
	return n
}

// testLAConcurrentFirstRegistrationRace models
// testGhostBusConcurrentDuplicate (ghostbustest.go): several goroutines race
// Upsert on the same brand-new (region, activity_id) pair, each with a
// distinct token/push token standing in for distinct ActivityKit
// registrations of the same activity. Exactly the constraint-violation loser
// side of the race must surface as ErrDuplicate -- never a bare error -- and
// the winner's insert must leave exactly one row behind, matching Upsert's
// documented single-retry contract (design spec §2.1). A follow-up
// sequential Upsert against the same activity id must then hit the update
// path and succeed, proving the row the race left behind is usable, not
// wedged.
func testLAConcurrentFirstRegistrationRace(t *testing.T, newStore newLiveActivityStoreFunc) {
	repo, regionRepo := newStore(t)
	ctx := context.Background()
	putStoretestRegion(t, regionRepo, 1)

	const n = 8
	errs := make(chan error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range n {
		in := fullLAIn(fmt.Sprintf("tok-race-%d", i), "act-race")
		in.PushToken = fmt.Sprintf("push-race-%d", i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := repo.Upsert(ctx, in, base)
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)

	var okCount int
	for err := range errs {
		switch {
		case err == nil:
			okCount++
		case errors.Is(err, liveactivities.ErrDuplicate):
			// expected loser outcome
		default:
			t.Fatalf("racing Upsert err = %v, want nil or ErrDuplicate", err)
		}
	}
	if okCount < 1 {
		t.Fatalf("race outcome ok=%d, want at least one winner", okCount)
	}

	list, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got := countLAByActivity(list, "act-race"); got != 1 {
		t.Fatalf("List() has %d rows for act-race, want 1", got)
	}

	// The row the race left behind must still take a normal update: this
	// Upsert must hit UpdateLiveActivityRegistration (the row already
	// exists), not mint a second row.
	if _, followupErr := repo.Upsert(ctx, fullLAIn("tok-race-followup", "act-race"), base); followupErr != nil {
		t.Fatalf("follow-up Upsert (update path) = %v, want nil", followupErr)
	}
	list, err = repo.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got := countLAByActivity(list, "act-race"); got != 1 {
		t.Fatalf("List() after follow-up has %d rows for act-race, want 1", got)
	}
}

// testLADeleteByIDRemovesExistingRow asserts DeleteByID actually removes a
// known row (not just reporting success for a row that was never there,
// which is all testLADeleteByIDMissing exercises), and that deleting the
// same id a second time still reports no error.
func testLADeleteByIDRemovesExistingRow(t *testing.T, newStore newLiveActivityStoreFunc) {
	repo, regionRepo := newStore(t)
	ctx := context.Background()
	putStoretestRegion(t, regionRepo, 1)

	created, err := repo.Upsert(ctx, fullLAIn("tok-delete-by-id", "act-delete-by-id"), base)
	if err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	if deleteErr := repo.DeleteByID(ctx, created.ID); deleteErr != nil {
		t.Fatalf("DeleteByID(known) = %v, want nil", deleteErr)
	}

	list, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, la := range list {
		if la.ID == created.ID {
			t.Fatalf("List() after DeleteByID still contains id %d: %+v", created.ID, list)
		}
	}

	if secondErr := repo.DeleteByID(ctx, created.ID); secondErr != nil {
		t.Errorf("DeleteByID(already gone) = %v, want nil", secondErr)
	}
}
