package storetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/alarms"
	"github.com/OneBusAway/sidecar/internal/regions"
)

// newAlarmStoreFunc is shorthand for the callback every alarm subtest
// receives: a fresh, migrated pair of repositories backed by the same
// underlying store.
type newAlarmStoreFunc func(*testing.T) (alarms.Repository, regions.Repository)

// RunAlarmRepository exercises an alarms.Repository/regions.Repository pair
// against the behavioral contract both engines must satisfy. Each subtest
// gets a fresh store from newStore.
func RunAlarmRepository(t *testing.T, newStore newAlarmStoreFunc) {
	t.Helper()

	t.Run("CreateGetRoundTrip", func(t *testing.T) { testAlarmCreateGetRoundTrip(t, newStore) })
	t.Run("StopSequenceZeroDistinctFromAbsent", func(t *testing.T) { testStopSequenceZeroDistinctFromAbsent(t, newStore) })
	t.Run("V1FindMatchesExactKey", func(t *testing.T) { testV1FindMatchesExactKey(t, newStore) })
	t.Run("V1DuplicateInsertReturnsErrDuplicate", func(t *testing.T) { testV1DuplicateInsertReturnsErrDuplicate(t, newStore) })
	t.Run("V2NeverDeduplicates", func(t *testing.T) { testV2NeverDeduplicates(t, newStore) })
	t.Run("DeleteByTokenReports204Contract", func(t *testing.T) { testDeleteByTokenReports204Contract(t, newStore) })
	t.Run("FailureCounterIncrementsAndResets", func(t *testing.T) { testFailureCounterIncrementsAndResets(t, newStore) })
	t.Run("ServiceDateBeyond32Bit", func(t *testing.T) { testServiceDateBeyond32Bit(t, newStore) })
	t.Run("RegionCascade", func(t *testing.T) { testAlarmRegionCascade(t, newStore) })
	t.Run("DeleteByIDTreatsMissingAsSuccess", func(t *testing.T) { testDeleteByIDTreatsMissingAsSuccess(t, newStore) })
	t.Run("RegionScopedReads", func(t *testing.T) { testAlarmRegionScopedReads(t, newStore) })
	t.Run("DeferHidesAlarmFromListDueUntilItsInstant", func(t *testing.T) { testDeferHidesAlarmFromListDue(t, newStore) })
}

// seedAlarmRegions upserts the two regions testAlarmRegionScopedReads uses.
// Region 0 is deliberately one of them: it is a real region (Tampa Bay), so
// a repository that treats 0 as "no region" fails here.
func seedAlarmRegions(t *testing.T, repo regions.Repository) {
	t.Helper()
	err := repo.UpsertFromDirectory(context.Background(), []regions.Region{
		{ID: 0, Name: "Tampa Bay", OBABaseURL: "https://tampa.example/", Language: "en", Active: true},
		{ID: 1, Name: "Puget Sound", OBABaseURL: "https://puget.example/", Language: "en", Active: true},
	}, base)
	if err != nil {
		t.Fatalf("seed regions: %v", err)
	}
}

// fullAlarmIn builds a NewAlarm with every field set, for the subtests that
// need a fully-populated starting row. RegionID is always 1: subtests that
// need a different region call putStoretestRegion for the extra id and set
// RegionID on the returned value themselves. StopSequence defaults to
// ptr(0): the zero value is a real stop sequence (the trip's first stop)
// and must be distinguishable from an absent one throughout the suite
// unless a subtest overrides it.
func fullAlarmIn(token string, apiVersion int) alarms.NewAlarm {
	return alarms.NewAlarm{
		RegionID:        1,
		Token:           token,
		APIVersion:      apiVersion,
		UserPushID:      "push-1",
		OperatingSystem: "ios",
		APNSSandbox:     true,
		StopID:          "stop-1",
		TripID:          "trip-1",
		ServiceDate:     1755657600123,
		VehicleID:       "vehicle-1",
		StopSequence:    ptr(int64(0)),
		SecondsBefore:   600,
		Message:         "The 44 to Ballard leaves in 10 minutes",
	}
}

// findAlarmByToken locates an alarm by token in a List() result, failing the
// test if it is not present.
func findAlarmByToken(t *testing.T, list []alarms.Alarm, token string) alarms.Alarm {
	t.Helper()
	for _, a := range list {
		if a.Token == token {
			return a
		}
	}
	t.Fatalf("List() missing alarm with token %q (got %+v)", token, list)
	return alarms.Alarm{}
}

// testAlarmCreateGetRoundTrip asserts that Create with every field set
// stores and returns every field, that List surfaces the same row (V2
// alarms have no dedicated Get, so List is how they are read back), and that
// a fresh alarm starts with FailureCount 0.
func testAlarmCreateGetRoundTrip(t *testing.T, newStore newAlarmStoreFunc) {
	repo, regionRepo := newStore(t)
	ctx := context.Background()
	putStoretestRegion(t, regionRepo, 1)

	in := fullAlarmIn("tok-round-trip", 2)
	created, err := repo.Create(ctx, in, base)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.Token != in.Token {
		t.Errorf("Create() Token = %q, want %q (token echoed)", created.Token, in.Token)
	}
	if created.FailureCount != 0 {
		t.Errorf("Create() FailureCount = %d, want 0", created.FailureCount)
	}

	list, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	got := findAlarmByToken(t, list, "tok-round-trip")

	if got.RegionID != in.RegionID {
		t.Errorf("RegionID = %d, want %d", got.RegionID, in.RegionID)
	}
	if got.APIVersion != in.APIVersion {
		t.Errorf("APIVersion = %d, want %d", got.APIVersion, in.APIVersion)
	}
	if got.UserPushID != in.UserPushID {
		t.Errorf("UserPushID = %q, want %q", got.UserPushID, in.UserPushID)
	}
	if got.OperatingSystem != in.OperatingSystem {
		t.Errorf("OperatingSystem = %q, want %q", got.OperatingSystem, in.OperatingSystem)
	}
	if got.APNSSandbox != in.APNSSandbox {
		t.Errorf("APNSSandbox = %v, want %v", got.APNSSandbox, in.APNSSandbox)
	}
	if got.StopID != in.StopID {
		t.Errorf("StopID = %q, want %q", got.StopID, in.StopID)
	}
	if got.TripID != in.TripID {
		t.Errorf("TripID = %q, want %q", got.TripID, in.TripID)
	}
	if got.ServiceDate != in.ServiceDate {
		t.Errorf("ServiceDate = %d, want %d", got.ServiceDate, in.ServiceDate)
	}
	if got.VehicleID != in.VehicleID {
		t.Errorf("VehicleID = %q, want %q", got.VehicleID, in.VehicleID)
	}
	if got.StopSequence == nil || *got.StopSequence != *in.StopSequence {
		t.Errorf("StopSequence = %v, want %v", got.StopSequence, in.StopSequence)
	}
	if got.SecondsBefore != in.SecondsBefore {
		t.Errorf("SecondsBefore = %d, want %d", got.SecondsBefore, in.SecondsBefore)
	}
	if got.Message != in.Message {
		t.Errorf("Message = %q, want %q", got.Message, in.Message)
	}
	if got.FailureCount != 0 {
		t.Errorf("FailureCount = %d, want 0", got.FailureCount)
	}
	if !got.CreatedAt.Equal(base) {
		t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, base)
	}
}

// testStopSequenceZeroDistinctFromAbsent asserts that StopSequence: ptr(0)
// and StopSequence: nil round-trip as distinguishable values -- 0 is a real
// stop sequence (the trip's first stop), not a signal that the field was
// omitted.
func testStopSequenceZeroDistinctFromAbsent(t *testing.T, newStore newAlarmStoreFunc) {
	repo, regionRepo := newStore(t)
	ctx := context.Background()
	putStoretestRegion(t, regionRepo, 1)

	zeroIn := fullAlarmIn("tok-zero", 2)
	zeroIn.StopSequence = ptr(int64(0))
	if _, err := repo.Create(ctx, zeroIn, base); err != nil {
		t.Fatalf("Create(zero): %v", err)
	}

	absentIn := fullAlarmIn("tok-absent", 2)
	absentIn.StopSequence = nil
	if _, err := repo.Create(ctx, absentIn, base); err != nil {
		t.Fatalf("Create(absent): %v", err)
	}

	list, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	zeroGot := findAlarmByToken(t, list, "tok-zero")
	absentGot := findAlarmByToken(t, list, "tok-absent")

	if zeroGot.StopSequence == nil {
		t.Fatal("tok-zero StopSequence = nil, want *0 (a real stop sequence, not absent)")
	}
	if *zeroGot.StopSequence != 0 {
		t.Errorf("tok-zero StopSequence = %d, want 0", *zeroGot.StopSequence)
	}
	if absentGot.StopSequence != nil {
		t.Errorf("tok-absent StopSequence = %v, want nil", *absentGot.StopSequence)
	}
}

// testV1FindMatchesExactKey asserts that FindV1 hits only when every one of
// the five key fields (region, user push id, trip id, stop id, service date)
// matches, and returns alarms.ErrNotFound when any single field differs.
func testV1FindMatchesExactKey(t *testing.T, newStore newAlarmStoreFunc) {
	repo, regionRepo := newStore(t)
	ctx := context.Background()
	putStoretestRegion(t, regionRepo, 1)
	putStoretestRegion(t, regionRepo, 2)

	in := fullAlarmIn("tok-v1-key", 1)
	created, err := repo.Create(ctx, in, base)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	key := alarms.V1Key{
		RegionID:    in.RegionID,
		UserPushID:  in.UserPushID,
		TripID:      in.TripID,
		StopID:      in.StopID,
		ServiceDate: in.ServiceDate,
	}
	got, err := repo.FindV1(ctx, key)
	if err != nil {
		t.Fatalf("FindV1(exact key): %v", err)
	}
	if got.ID != created.ID {
		t.Errorf("FindV1(exact key) ID = %d, want %d", got.ID, created.ID)
	}

	variants := []struct {
		name string
		key  alarms.V1Key
	}{
		{"RegionID", func() alarms.V1Key { k := key; k.RegionID = 2; return k }()},
		{"UserPushID", func() alarms.V1Key { k := key; k.UserPushID = "other-push"; return k }()},
		{"TripID", func() alarms.V1Key { k := key; k.TripID = "other-trip"; return k }()},
		{"StopID", func() alarms.V1Key { k := key; k.StopID = "other-stop"; return k }()},
		{"ServiceDate", func() alarms.V1Key { k := key; k.ServiceDate = key.ServiceDate + 1; return k }()},
	}
	for _, v := range variants {
		if _, err := repo.FindV1(ctx, v.key); !errors.Is(err, alarms.ErrNotFound) {
			t.Errorf("FindV1(%s changed) = %v, want alarms.ErrNotFound", v.name, err)
		}
	}
}

// testV1DuplicateInsertReturnsErrDuplicate asserts that a second V1 Create
// sharing the same idempotency key (region, user push id, trip id, stop id,
// service date) as an existing V1 alarm loses the dedupe race and returns
// alarms.ErrDuplicate, rather than minting a second alarm that fires twice.
func testV1DuplicateInsertReturnsErrDuplicate(t *testing.T, newStore newAlarmStoreFunc) {
	repo, regionRepo := newStore(t)
	ctx := context.Background()
	putStoretestRegion(t, regionRepo, 1)

	first := fullAlarmIn("tok-v1-first", 1)
	if _, err := repo.Create(ctx, first, base); err != nil {
		t.Fatalf("Create(first): %v", err)
	}

	second := fullAlarmIn("tok-v1-second", 1) // different token, identical V1 key
	if _, err := repo.Create(ctx, second, base); !errors.Is(err, alarms.ErrDuplicate) {
		t.Fatalf("Create(duplicate V1 key) = %v, want alarms.ErrDuplicate", err)
	}

	list, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("List() = %d alarms, want 1 (the duplicate must not have been inserted)", len(list))
	}
}

// testV2NeverDeduplicates asserts that two V2 Creates sharing what would be
// a V1 dedupe key both succeed and leave two independent rows: the partial
// unique index only applies WHERE api_version = 1.
func testV2NeverDeduplicates(t *testing.T, newStore newAlarmStoreFunc) {
	repo, regionRepo := newStore(t)
	ctx := context.Background()
	putStoretestRegion(t, regionRepo, 1)

	first := fullAlarmIn("tok-v2-first", 2)
	if _, err := repo.Create(ctx, first, base); err != nil {
		t.Fatalf("Create(first): %v", err)
	}
	second := fullAlarmIn("tok-v2-second", 2) // different token, identical trip-identity fields
	if _, err := repo.Create(ctx, second, base); err != nil {
		t.Fatalf("Create(second): %v, want no error (V2 never dedupes)", err)
	}

	list, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("List() = %d alarms, want 2", len(list))
	}
}

// testDeleteByTokenReports204Contract asserts Delete succeeds and actually
// removes a known (region, token) row, and reports alarms.ErrNotFound for an
// unknown one -- the distinction the HTTP handler needs to return 204 vs 404.
func testDeleteByTokenReports204Contract(t *testing.T, newStore newAlarmStoreFunc) {
	repo, regionRepo := newStore(t)
	ctx := context.Background()
	putStoretestRegion(t, regionRepo, 1)

	if err := repo.Delete(ctx, 1, "ghost"); !errors.Is(err, alarms.ErrNotFound) {
		t.Errorf("Delete(unknown) = %v, want alarms.ErrNotFound", err)
	}

	in := fullAlarmIn("tok-delete", 2)
	if _, err := repo.Create(ctx, in, base); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := repo.Delete(ctx, 1, "tok-delete"); err != nil {
		t.Fatalf("Delete(known) = %v, want nil", err)
	}

	list, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("List() after Delete = %d alarms, want 0 (really gone)", len(list))
	}
}

// testFailureCounterIncrementsAndResets asserts RecordFailure returns the
// new streak on each call (1, 2, 3, ...) and that ResetFailures brings the
// next RecordFailure back to 1.
func testFailureCounterIncrementsAndResets(t *testing.T, newStore newAlarmStoreFunc) {
	repo, regionRepo := newStore(t)
	ctx := context.Background()
	putStoretestRegion(t, regionRepo, 1)

	in := fullAlarmIn("tok-failures", 2)
	created, err := repo.Create(ctx, in, base)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	for i, want := range []int64{1, 2, 3} {
		got, recErr := repo.RecordFailure(ctx, created.ID)
		if recErr != nil {
			t.Fatalf("RecordFailure(%d): %v", i, recErr)
		}
		if got != want {
			t.Fatalf("RecordFailure(%d) = %d, want %d", i, got, want)
		}
	}

	if resetErr := repo.ResetFailures(ctx, created.ID); resetErr != nil {
		t.Fatalf("ResetFailures: %v", resetErr)
	}

	got, err := repo.RecordFailure(ctx, created.ID)
	if err != nil {
		t.Fatalf("RecordFailure(after reset): %v", err)
	}
	if got != 1 {
		t.Fatalf("RecordFailure(after reset) = %d, want 1", got)
	}
}

// testServiceDateBeyond32Bit asserts that a ServiceDate value beyond the
// 32-bit signed boundary round-trips exactly: service_date is epoch
// MILLISECONDS client data, so ordinary present-day values already exceed
// 1<<31, and any adapter storing it in anything narrower than a 64-bit
// column silently truncates.
func testServiceDateBeyond32Bit(t *testing.T, newStore newAlarmStoreFunc) {
	repo, regionRepo := newStore(t)
	ctx := context.Background()
	putStoretestRegion(t, regionRepo, 1)

	const want = (int64(1) << 32) + 123456789 // well beyond the 32-bit signed boundary

	in := fullAlarmIn("tok-service-date", 2)
	in.ServiceDate = want
	if _, err := repo.Create(ctx, in, base); err != nil {
		t.Fatalf("Create: %v", err)
	}

	list, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	got := findAlarmByToken(t, list, "tok-service-date")
	if got.ServiceDate != want {
		t.Errorf("ServiceDate = %d, want %d", got.ServiceDate, want)
	}
}

// testAlarmRegionCascade asserts Delete is scoped by region_id: a token
// registered in region 1 cannot be deleted by naming region 2, since
// regions.Repository has no delete operation to exercise FK cascade
// directly -- this is the region-scoping guarantee Delete must uphold
// instead.
func testAlarmRegionCascade(t *testing.T, newStore newAlarmStoreFunc) {
	repo, regionRepo := newStore(t)
	ctx := context.Background()
	putStoretestRegion(t, regionRepo, 1)
	putStoretestRegion(t, regionRepo, 2)

	in := fullAlarmIn("tok-cascade", 2)
	if _, err := repo.Create(ctx, in, base); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := repo.Delete(ctx, 2, "tok-cascade"); !errors.Is(err, alarms.ErrNotFound) {
		t.Errorf("Delete(wrong region) = %v, want alarms.ErrNotFound (must not delete across regions)", err)
	}

	list, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("List() after cross-region Delete attempt = %d alarms, want 1 (row must survive)", len(list))
	}

	if err := repo.Delete(ctx, 1, "tok-cascade"); err != nil {
		t.Fatalf("Delete(correct region) = %v, want nil", err)
	}
}

// testDeleteByIDTreatsMissingAsSuccess asserts DeleteByID reports no error
// for an id that does not exist: the scheduler may race a rider's cancel
// against its own sweep, and the row being gone is the goal either way, not
// an error condition.
func testDeleteByIDTreatsMissingAsSuccess(t *testing.T, newStore newAlarmStoreFunc) {
	repo, regionRepo := newStore(t)
	ctx := context.Background()
	putStoretestRegion(t, regionRepo, 1)

	const unknownID = 999999
	if err := repo.DeleteByID(ctx, unknownID); err != nil {
		t.Errorf("DeleteByID(unknown) = %v, want nil", err)
	}

	in := fullAlarmIn("tok-delete-by-id", 2)
	created, err := repo.Create(ctx, in, base)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if deleteErr := repo.DeleteByID(ctx, created.ID); deleteErr != nil {
		t.Fatalf("DeleteByID(known): %v, want nil", deleteErr)
	}
	list, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("List() after DeleteByID = %d alarms, want 0 (really gone)", len(list))
	}

	// Deleting the same id again must still report no error.
	if err := repo.DeleteByID(ctx, created.ID); err != nil {
		t.Errorf("DeleteByID(already gone) = %v, want nil", err)
	}
}

// testAlarmRegionScopedReads pins the tenancy fence the admin API leans on:
// the region is a query condition, not something a handler compares after
// the fact (design spec section 3.2).
func testAlarmRegionScopedReads(t *testing.T, newStore newAlarmStoreFunc) {
	repo, regionRepo := newStore(t)
	ctx := context.Background()
	seedAlarmRegions(t, regionRepo) // reuse this file's existing region seeder

	inA, err := repo.Create(ctx, alarms.NewAlarm{
		RegionID: 0, Token: "tok-a", APIVersion: 2, UserPushID: "u1",
		OperatingSystem: "ios", StopID: "s1", SecondsBefore: 600, Message: "a",
	}, base)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	inB, err := repo.Create(ctx, alarms.NewAlarm{
		RegionID: 1, Token: "tok-b", APIVersion: 2, UserPushID: "u2",
		OperatingSystem: "android", StopID: "s2", SecondsBefore: 600, Message: "b",
	}, base.Add(time.Minute))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	listed, err := repo.ListByRegion(ctx, 0)
	if err != nil {
		t.Fatalf("ListByRegion: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != inA.ID {
		t.Fatalf("ListByRegion(0) = %+v, want only alarm %d", listed, inA.ID)
	}

	got, err := repo.GetInRegion(ctx, 0, inA.ID)
	if err != nil {
		t.Fatalf("GetInRegion: %v", err)
	}
	if got.Token != "tok-a" {
		t.Errorf("GetInRegion token = %q, want tok-a", got.Token)
	}
	// The whole point: an alarm that exists, addressed through the wrong
	// region, is indistinguishable from one that does not exist.
	if _, err := repo.GetInRegion(ctx, 0, inB.ID); !errors.Is(err, alarms.ErrNotFound) {
		t.Errorf("GetInRegion across regions: err = %v, want ErrNotFound", err)
	}
	if _, err := repo.GetInRegion(ctx, 0, 99999); !errors.Is(err, alarms.ErrNotFound) {
		t.Errorf("GetInRegion unknown id: err = %v, want ErrNotFound", err)
	}
}

// testDeferHidesAlarmFromListDue pins the due-window contract the scheduler
// relies on (spec section 5.3, section 12): a fresh alarm is due at once
// (CheckAfter is the zero instant, so ListDue at any now returns it); after
// Defer(until) it is absent from ListDue for every now before until and
// present again for now == until and later; and List still returns it
// throughout, because deferral is scheduler bookkeeping, not a lifecycle
// state the admin API should hide.
func testDeferHidesAlarmFromListDue(t *testing.T, newStore newAlarmStoreFunc) {
	const token = "tok-defer"
	repo, regionRepo := newStore(t)
	ctx := context.Background()
	putStoretestRegion(t, regionRepo, 1)

	created, err := repo.Create(ctx, fullAlarmIn(token, 2), base)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !created.CheckAfter.IsZero() {
		t.Errorf("Create() CheckAfter = %v, want the zero instant (due at once)", created.CheckAfter)
	}

	due, err := repo.ListDue(ctx, base)
	if err != nil {
		t.Fatalf("ListDue(base): %v", err)
	}
	findAlarmByToken(t, due, token)

	until := base.Add(90 * time.Minute)
	if deferErr := repo.Defer(ctx, created.ID, until); deferErr != nil {
		t.Fatalf("Defer: %v", deferErr)
	}

	for _, now := range []time.Time{base, until.Add(-time.Second)} {
		got, listErr := repo.ListDue(ctx, now)
		if listErr != nil {
			t.Fatalf("ListDue(%v): %v", now, listErr)
		}
		if len(got) != 0 {
			t.Errorf("ListDue(%v) = %d alarms, want 0 (deferred until %v)", now, len(got), until)
		}
	}
	for _, now := range []time.Time{until, until.Add(time.Hour)} {
		got, listErr := repo.ListDue(ctx, now)
		if listErr != nil {
			t.Fatalf("ListDue(%v): %v", now, listErr)
		}
		a := findAlarmByToken(t, got, token)
		if !a.CheckAfter.Equal(until) {
			t.Errorf("CheckAfter = %v, want %v", a.CheckAfter, until)
		}
	}

	all, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	findAlarmByToken(t, all, token)

	// Defer on a vanished row is not an error: the sweep can race the
	// rider's own cancel, same as DeleteByID.
	if deferErr := repo.Defer(ctx, created.ID+1000, until); deferErr != nil {
		t.Errorf("Defer(missing) = %v, want nil", deferErr)
	}
}
