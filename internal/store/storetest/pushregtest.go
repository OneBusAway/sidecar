package storetest

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/pushreg"
	"github.com/OneBusAway/sidecar/internal/regions"
)

// ptr returns a pointer to v, for building pushreg.Upsert literals inline.
func ptr[T any](v T) *T { return &v }

// newPushRegStoreFunc is shorthand for the callback every push registration
// subtest receives: a fresh, migrated pair of repositories backed by the
// same underlying store.
type newPushRegStoreFunc func(*testing.T) (pushreg.Repository, regions.Repository)

// RunPushRegistrationRepository exercises a pushreg.Repository/
// regions.Repository pair against the behavioral contract both engines must
// satisfy. Each subtest gets a fresh store from newStore.
func RunPushRegistrationRepository(t *testing.T, newStore newPushRegStoreFunc) {
	t.Helper()

	t.Run("UpsertInsertsAndGetRoundTrips", func(t *testing.T) { testUpsertInsertsAndGetRoundTrips(t, newStore) })
	t.Run("ReRegistrationRefreshesLastSeen", func(t *testing.T) { testReRegistrationRefreshesLastSeen(t, newStore) })
	t.Run("NilPointersKeepStoredValues", func(t *testing.T) { testNilPointersKeepStoredValues(t, newStore) })
	t.Run("ExplicitFalseDemotesAndClearsDescription", func(t *testing.T) { testExplicitFalseDemotesAndClearsDescription(t, newStore) })
	t.Run("OperatingSystemAndSandboxAlwaysOverwritten", func(t *testing.T) { testOperatingSystemAndSandboxAlwaysOverwritten(t, newStore) })
	t.Run("LocaleOverwrittenOnlyWhenSet", func(t *testing.T) { testLocaleOverwrittenOnlyWhenSet(t, newStore) })
	t.Run("DescriptionOverwrittenOnlyWhenSet", func(t *testing.T) { testDescriptionOverwrittenOnlyWhenSet(t, newStore) })
	t.Run("DeleteReportsNotFound", func(t *testing.T) { testPushRegDeleteReportsNotFound(t, newStore) })
	t.Run("DeleteByTokenSpansRegions", func(t *testing.T) { testDeleteByTokenSpansRegions(t, newStore) })
	t.Run("PruneRemovesOnlyStale", func(t *testing.T) { testPruneRemovesOnlyStale(t, newStore) })
	t.Run("RegionScoping", func(t *testing.T) { testPushRegRegionScoping(t, newStore) })
	t.Run("ConcurrentFirstRegistrationRaces", func(t *testing.T) { testConcurrentFirstRegistrationRaces(t, newStore) })
}

// putPushRegRegion inserts a minimal directory-sourced region with the given
// id: push_registrations.region_id is a foreign key, so every subtest needs
// at least one region to satisfy it before it can register a device.
func putPushRegRegion(t *testing.T, repo regions.Repository, id int64) {
	t.Helper()
	if err := repo.UpsertFromDirectory(context.Background(), []regions.Region{{
		ID:         id,
		Name:       "Region",
		OBABaseURL: "https://example.org/",
		Active:     true,
	}}, base); err != nil {
		t.Fatalf("UpsertFromDirectory(%d): %v", id, err)
	}
}

// fullUpsert builds an Upsert with every sticky pointer set, for the
// subtests that need a fully-populated starting row.
func fullUpsert(regionID int64, token string) pushreg.Upsert {
	return pushreg.Upsert{
		RegionID:        regionID,
		Token:           token,
		OperatingSystem: pushreg.OSIOS,
		APNSSandbox:     false,
		Locale:          ptr("es-MX"),
		TestDevice:      ptr(true),
		Description:     ptr("Aaron's iPhone"),
	}
}

func assertPushRegistration(t *testing.T, label string, got, want pushreg.Registration) {
	t.Helper()
	if got.RegionID != want.RegionID {
		t.Errorf("%s RegionID = %d, want %d", label, got.RegionID, want.RegionID)
	}
	if got.Token != want.Token {
		t.Errorf("%s Token = %q, want %q", label, got.Token, want.Token)
	}
	if got.OperatingSystem != want.OperatingSystem {
		t.Errorf("%s OperatingSystem = %q, want %q", label, got.OperatingSystem, want.OperatingSystem)
	}
	if got.APNSSandbox != want.APNSSandbox {
		t.Errorf("%s APNSSandbox = %v, want %v", label, got.APNSSandbox, want.APNSSandbox)
	}
	if got.Locale != want.Locale {
		t.Errorf("%s Locale = %q, want %q", label, got.Locale, want.Locale)
	}
	if got.TestDevice != want.TestDevice {
		t.Errorf("%s TestDevice = %v, want %v", label, got.TestDevice, want.TestDevice)
	}
	if got.Description != want.Description {
		t.Errorf("%s Description = %q, want %q", label, got.Description, want.Description)
	}
	if !got.LastSeenAt.Equal(want.LastSeenAt) {
		t.Errorf("%s LastSeenAt = %v, want %v", label, got.LastSeenAt, want.LastSeenAt)
	}
	if !got.CreatedAt.Equal(want.CreatedAt) {
		t.Errorf("%s CreatedAt = %v, want %v", label, got.CreatedAt, want.CreatedAt)
	}
}

// testUpsertInsertsAndGetRoundTrips asserts that a first-time Upsert with
// every sticky pointer set stores and returns every field, including
// LastSeenAt read back exactly as the injected clock.
func testUpsertInsertsAndGetRoundTrips(t *testing.T, newStore newPushRegStoreFunc) {
	repo, regionRepo := newStore(t)
	ctx := context.Background()
	putPushRegRegion(t, regionRepo, 1)

	in := fullUpsert(1, "tok-1")
	if err := repo.Upsert(ctx, in, base); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	got, err := repo.Get(ctx, 1, "tok-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	assertPushRegistration(t, "Get", got, pushreg.Registration{
		RegionID: 1, Token: "tok-1", OperatingSystem: pushreg.OSIOS, APNSSandbox: false,
		Locale: "es-MX", TestDevice: true, Description: "Aaron's iPhone",
		LastSeenAt: base, CreatedAt: base,
	})
	if !got.LastSeenAt.Equal(base) {
		t.Errorf("LastSeenAt = %v, want %v", got.LastSeenAt, base)
	}
}

// testReRegistrationRefreshesLastSeen asserts that a second Upsert with
// every pointer nil advances LastSeenAt while leaving the sticky fields
// (locale, test_device, description) untouched -- the defining behavior of
// "sticky": absence of a value on a write means "keep what's there", not
// "clear it".
func testReRegistrationRefreshesLastSeen(t *testing.T, newStore newPushRegStoreFunc) {
	repo, regionRepo := newStore(t)
	ctx := context.Background()
	putPushRegRegion(t, regionRepo, 1)

	if err := repo.Upsert(ctx, fullUpsert(1, "tok-1"), base); err != nil {
		t.Fatalf("Upsert(1): %v", err)
	}

	later := base.Add(time.Hour)
	if err := repo.Upsert(ctx, pushreg.Upsert{
		RegionID: 1, Token: "tok-1", OperatingSystem: pushreg.OSIOS, APNSSandbox: false,
		// Locale, TestDevice, Description all nil.
	}, later); err != nil {
		t.Fatalf("Upsert(2, all nil): %v", err)
	}

	got, err := repo.Get(ctx, 1, "tok-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got.LastSeenAt.Equal(later) {
		t.Fatalf("LastSeenAt = %v, want %v (must advance on every registration)", got.LastSeenAt, later)
	}
	// CreatedAt must survive the re-upsert unchanged: this is the assertion
	// that catches a swapped-column mapping (e.g. CreatedAt read from
	// last_seen_at) -- LastSeenAt alone advances to `later` on every write,
	// so a swap would still pass a LastSeenAt-only check.
	if !got.CreatedAt.Equal(base) {
		t.Errorf("CreatedAt = %v, want unchanged %v (must survive re-registration; only LastSeenAt advances)", got.CreatedAt, base)
	}
	if got.Locale != "es-MX" {
		t.Errorf("Locale = %q, want unchanged %q (nil pointer must not clear it)", got.Locale, "es-MX")
	}
	if !got.TestDevice {
		t.Errorf("TestDevice = %v, want unchanged true (nil pointer must not clear it)", got.TestDevice)
	}
	if got.Description != "Aaron's iPhone" {
		t.Errorf("Description = %q, want unchanged %q (nil pointer must not clear it)", got.Description, "Aaron's iPhone")
	}
}

// testNilPointersKeepStoredValues is the same assertion as
// testReRegistrationRefreshesLastSeen but isolates the claim per the brief's
// naming: every sticky field independently survives an Upsert that leaves
// its pointer nil, proving each field is gated by its own flag rather than
// one shared "any field present" flag that would wrongly clear the others.
func testNilPointersKeepStoredValues(t *testing.T, newStore newPushRegStoreFunc) {
	repo, regionRepo := newStore(t)
	ctx := context.Background()
	putPushRegRegion(t, regionRepo, 1)

	if err := repo.Upsert(ctx, fullUpsert(1, "tok-1"), base); err != nil {
		t.Fatalf("Upsert(1): %v", err)
	}
	// Only Locale is set on the second write; TestDevice and Description
	// stay nil. If they shared one flag with Locale, this write would wrongly
	// clear them too.
	if err := repo.Upsert(ctx, pushreg.Upsert{
		RegionID: 1, Token: "tok-1", OperatingSystem: pushreg.OSIOS, APNSSandbox: false,
		Locale: ptr("fr"),
	}, base.Add(time.Hour)); err != nil {
		t.Fatalf("Upsert(2, locale only): %v", err)
	}

	got, err := repo.Get(ctx, 1, "tok-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Locale != "fr" {
		t.Errorf("Locale = %q, want %q (explicitly set)", got.Locale, "fr")
	}
	if !got.TestDevice {
		t.Errorf("TestDevice = %v, want unchanged true (its pointer was nil)", got.TestDevice)
	}
	if got.Description != "Aaron's iPhone" {
		t.Errorf("Description = %q, want unchanged %q (its pointer was nil)", got.Description, "Aaron's iPhone")
	}
}

// testExplicitFalseDemotesAndClearsDescription asserts that an explicit
// non-nil pointer to a zero value (false, "") overwrites, distinct from a
// nil pointer: TestDevice: ptr(false) must actually clear a previously-true
// flag, and Description: ptr("") must actually clear previous text. A set
// flag computed as `in.TestDevice != nil && *in.TestDevice` (rather than
// just `in.TestDevice != nil`) would fail this by leaving the old value in
// place instead of writing false/"".
func testExplicitFalseDemotesAndClearsDescription(t *testing.T, newStore newPushRegStoreFunc) {
	repo, regionRepo := newStore(t)
	ctx := context.Background()
	putPushRegRegion(t, regionRepo, 1)

	if err := repo.Upsert(ctx, fullUpsert(1, "tok-1"), base); err != nil {
		t.Fatalf("Upsert(1): %v", err)
	}
	if err := repo.Upsert(ctx, pushreg.Upsert{
		RegionID: 1, Token: "tok-1", OperatingSystem: pushreg.OSIOS, APNSSandbox: false,
		TestDevice: ptr(false), Description: ptr(""),
	}, base.Add(time.Hour)); err != nil {
		t.Fatalf("Upsert(2, demote): %v", err)
	}

	got, err := repo.Get(ctx, 1, "tok-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.TestDevice {
		t.Errorf("TestDevice = true, want false (explicit demotion)")
	}
	if got.Description != "" {
		t.Errorf("Description = %q, want cleared %q", got.Description, "")
	}
	// Locale's pointer was nil throughout this write; it must still be
	// unaffected by the other two fields' explicit overwrite.
	if got.Locale != "es-MX" {
		t.Errorf("Locale = %q, want unchanged %q", got.Locale, "es-MX")
	}
}

// testOperatingSystemAndSandboxAlwaysOverwritten asserts operating_system
// and apns_sandbox have no sticky flag at all: every registration restates
// its own build's platform and APNs environment, so a later registration
// from a different build (e.g. a device switching from a sandboxed TestFlight
// build to a production one) must always win, with no pointer to suppress it.
func testOperatingSystemAndSandboxAlwaysOverwritten(t *testing.T, newStore newPushRegStoreFunc) {
	repo, regionRepo := newStore(t)
	ctx := context.Background()
	putPushRegRegion(t, regionRepo, 1)

	if err := repo.Upsert(ctx, pushreg.Upsert{
		RegionID: 1, Token: "tok-1", OperatingSystem: pushreg.OSIOS, APNSSandbox: true,
	}, base); err != nil {
		t.Fatalf("Upsert(1, ios/sandbox): %v", err)
	}
	if err := repo.Upsert(ctx, pushreg.Upsert{
		RegionID: 1, Token: "tok-1", OperatingSystem: pushreg.OSAndroid, APNSSandbox: false,
	}, base.Add(time.Hour)); err != nil {
		t.Fatalf("Upsert(2, android/production): %v", err)
	}

	got, err := repo.Get(ctx, 1, "tok-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.OperatingSystem != pushreg.OSAndroid {
		t.Errorf("OperatingSystem = %q, want %q (must always follow the latest registration)", got.OperatingSystem, pushreg.OSAndroid)
	}
	if got.APNSSandbox {
		t.Errorf("APNSSandbox = true, want false (must always follow the latest registration)")
	}
}

// testLocaleOverwrittenOnlyWhenSet asserts Locale's set-flag is independent
// of the other two sticky fields: an Upsert with only Locale set must
// overwrite locale and nothing else, and a following Upsert with Locale nil
// must leave the just-written value alone.
func testLocaleOverwrittenOnlyWhenSet(t *testing.T, newStore newPushRegStoreFunc) {
	repo, regionRepo := newStore(t)
	ctx := context.Background()
	putPushRegRegion(t, regionRepo, 1)

	if err := repo.Upsert(ctx, fullUpsert(1, "tok-1"), base); err != nil {
		t.Fatalf("Upsert(1): %v", err)
	}
	if err := repo.Upsert(ctx, pushreg.Upsert{
		RegionID: 1, Token: "tok-1", OperatingSystem: pushreg.OSIOS, APNSSandbox: false,
		Locale: ptr("fr"),
	}, base.Add(time.Hour)); err != nil {
		t.Fatalf("Upsert(2, locale=fr): %v", err)
	}
	got, err := repo.Get(ctx, 1, "tok-1")
	if err != nil {
		t.Fatalf("Get(after set): %v", err)
	}
	if got.Locale != "fr" {
		t.Fatalf("Locale = %q, want %q", got.Locale, "fr")
	}

	if err := repo.Upsert(ctx, pushreg.Upsert{
		RegionID: 1, Token: "tok-1", OperatingSystem: pushreg.OSIOS, APNSSandbox: false,
	}, base.Add(2*time.Hour)); err != nil {
		t.Fatalf("Upsert(3, locale nil): %v", err)
	}
	got, err = repo.Get(ctx, 1, "tok-1")
	if err != nil {
		t.Fatalf("Get(after nil): %v", err)
	}
	if got.Locale != "fr" {
		t.Errorf("Locale = %q, want unchanged %q (pointer was nil)", got.Locale, "fr")
	}
}

// testDescriptionOverwrittenOnlyWhenSet asserts Description's set-flag is
// independent of TestDevice's: an Upsert that sets Description while
// leaving TestDevice nil must overwrite the description and keep the stored
// test_device -- the merged-row invariant (a test device carries a
// description; a non-test row carries none) belongs to the handler, not the
// store, so the store must not couple the two flags together.
func testDescriptionOverwrittenOnlyWhenSet(t *testing.T, newStore newPushRegStoreFunc) {
	repo, regionRepo := newStore(t)
	ctx := context.Background()
	putPushRegRegion(t, regionRepo, 1)

	if err := repo.Upsert(ctx, fullUpsert(1, "tok-1"), base); err != nil {
		t.Fatalf("Upsert(1): %v", err)
	}
	if err := repo.Upsert(ctx, pushreg.Upsert{
		RegionID: 1, Token: "tok-1", OperatingSystem: pushreg.OSIOS, APNSSandbox: false,
		Description: ptr("new"),
	}, base.Add(time.Hour)); err != nil {
		t.Fatalf("Upsert(2, description=new, test_device nil): %v", err)
	}

	got, err := repo.Get(ctx, 1, "tok-1")
	if err != nil {
		t.Fatalf("Get(after set): %v", err)
	}
	if got.Description != "new" {
		t.Errorf("Description = %q, want %q", got.Description, "new")
	}
	if !got.TestDevice {
		t.Errorf("TestDevice = %v, want unchanged true (its pointer was nil, must not follow Description's flag)", got.TestDevice)
	}

	if err := repo.Upsert(ctx, pushreg.Upsert{
		RegionID: 1, Token: "tok-1", OperatingSystem: pushreg.OSIOS, APNSSandbox: false,
	}, base.Add(2*time.Hour)); err != nil {
		t.Fatalf("Upsert(3, description nil): %v", err)
	}
	got, err = repo.Get(ctx, 1, "tok-1")
	if err != nil {
		t.Fatalf("Get(after nil): %v", err)
	}
	if got.Description != "new" {
		t.Errorf("Description = %q, want unchanged %q (pointer was nil)", got.Description, "new")
	}
}

// testPushRegDeleteReportsNotFound asserts Delete reports pushreg.ErrNotFound
// for an unknown (region, token) pair and succeeds for a known one, after
// which the row is actually gone.
func testPushRegDeleteReportsNotFound(t *testing.T, newStore newPushRegStoreFunc) {
	repo, regionRepo := newStore(t)
	ctx := context.Background()
	putPushRegRegion(t, regionRepo, 1)

	if err := repo.Delete(ctx, 1, "ghost"); !errors.Is(err, pushreg.ErrNotFound) {
		t.Errorf("Delete(unknown) = %v, want pushreg.ErrNotFound", err)
	}

	if err := repo.Upsert(ctx, fullUpsert(1, "tok-1"), base); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := repo.Delete(ctx, 1, "tok-1"); err != nil {
		t.Fatalf("Delete(known) = %v, want nil", err)
	}
	if _, err := repo.Get(ctx, 1, "tok-1"); !errors.Is(err, pushreg.ErrNotFound) {
		t.Errorf("Get after Delete = %v, want pushreg.ErrNotFound", err)
	}
}

// testDeleteByTokenSpansRegions asserts DeleteByToken removes every row for
// a token regardless of region: terminal APNs feedback names only the
// device token, not a region, so a device registered from two regions must
// lose both rows in one call.
func testDeleteByTokenSpansRegions(t *testing.T, newStore newPushRegStoreFunc) {
	repo, regionRepo := newStore(t)
	ctx := context.Background()
	putPushRegRegion(t, regionRepo, 1)
	putPushRegRegion(t, regionRepo, 2)

	if err := repo.Upsert(ctx, fullUpsert(1, "shared-tok"), base); err != nil {
		t.Fatalf("Upsert(region 1): %v", err)
	}
	if err := repo.Upsert(ctx, fullUpsert(2, "shared-tok"), base); err != nil {
		t.Fatalf("Upsert(region 2): %v", err)
	}
	// A different token in region 1 must survive: DeleteByToken must not
	// over-match every row in a region it touches.
	if err := repo.Upsert(ctx, fullUpsert(1, "other-tok"), base); err != nil {
		t.Fatalf("Upsert(region 1, other token): %v", err)
	}

	n, err := repo.DeleteByToken(ctx, "shared-tok")
	if err != nil {
		t.Fatalf("DeleteByToken: %v", err)
	}
	if n != 2 {
		t.Fatalf("DeleteByToken = %d, want 2", n)
	}

	if _, err := repo.Get(ctx, 1, "shared-tok"); !errors.Is(err, pushreg.ErrNotFound) {
		t.Errorf("Get(region 1, shared-tok) after DeleteByToken = %v, want pushreg.ErrNotFound", err)
	}
	if _, err := repo.Get(ctx, 2, "shared-tok"); !errors.Is(err, pushreg.ErrNotFound) {
		t.Errorf("Get(region 2, shared-tok) after DeleteByToken = %v, want pushreg.ErrNotFound", err)
	}
	if _, err := repo.Get(ctx, 1, "other-tok"); err != nil {
		t.Errorf("Get(region 1, other-tok) after DeleteByToken = %v, want no error (must not be over-matched)", err)
	}
}

// testPruneRemovesOnlyStale asserts Prune removes rows whose last_seen_at is
// strictly before cutoff and leaves rows at or after it, reporting the
// number removed. The at-cutoff row is what actually pins the boundary as
// strict "<" rather than "<=": a fresh row 24h clear of the cutoff would
// still survive under either comparison, so it alone cannot distinguish
// them.
func testPruneRemovesOnlyStale(t *testing.T, newStore newPushRegStoreFunc) {
	repo, regionRepo := newStore(t)
	ctx := context.Background()
	putPushRegRegion(t, regionRepo, 1)

	cutoff := base.Add(time.Hour)

	stale := fullUpsert(1, "stale-tok")
	if err := repo.Upsert(ctx, stale, base); err != nil {
		t.Fatalf("Upsert(stale): %v", err)
	}
	atCutoff := fullUpsert(1, "at-cutoff-tok")
	if err := repo.Upsert(ctx, atCutoff, cutoff); err != nil {
		t.Fatalf("Upsert(at-cutoff): %v", err)
	}
	fresh := fullUpsert(1, "fresh-tok")
	if err := repo.Upsert(ctx, fresh, base.Add(24*time.Hour)); err != nil {
		t.Fatalf("Upsert(fresh): %v", err)
	}

	n, err := repo.Prune(ctx, cutoff)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if n != 1 {
		t.Fatalf("Prune = %d, want 1", n)
	}

	if _, err := repo.Get(ctx, 1, "stale-tok"); !errors.Is(err, pushreg.ErrNotFound) {
		t.Errorf("Get(stale) after Prune = %v, want pushreg.ErrNotFound", err)
	}
	if _, err := repo.Get(ctx, 1, "at-cutoff-tok"); err != nil {
		t.Errorf("Get(at-cutoff) after Prune = %v, want no error (a row exactly at the cutoff must survive: the boundary is strict <)", err)
	}
	if _, err := repo.Get(ctx, 1, "fresh-tok"); err != nil {
		t.Errorf("Get(fresh) after Prune = %v, want no error (must survive)", err)
	}
}

// testPushRegRegionScoping asserts the same token registered from two
// different regions is stored as two independent rows: writing or deleting
// one must not affect the other.
func testPushRegRegionScoping(t *testing.T, newStore newPushRegStoreFunc) {
	repo, regionRepo := newStore(t)
	ctx := context.Background()
	putPushRegRegion(t, regionRepo, 1)
	putPushRegRegion(t, regionRepo, 2)

	in1 := fullUpsert(1, "shared-tok")
	in1.Locale = ptr("es-MX")
	if err := repo.Upsert(ctx, in1, base); err != nil {
		t.Fatalf("Upsert(region 1): %v", err)
	}
	in2 := fullUpsert(2, "shared-tok")
	in2.Locale = ptr("fr")
	if err := repo.Upsert(ctx, in2, base); err != nil {
		t.Fatalf("Upsert(region 2): %v", err)
	}

	got1, err := repo.Get(ctx, 1, "shared-tok")
	if err != nil {
		t.Fatalf("Get(region 1): %v", err)
	}
	if got1.Locale != "es-MX" {
		t.Errorf("region 1 Locale = %q, want %q", got1.Locale, "es-MX")
	}
	got2, err := repo.Get(ctx, 2, "shared-tok")
	if err != nil {
		t.Fatalf("Get(region 2): %v", err)
	}
	if got2.Locale != "fr" {
		t.Errorf("region 2 Locale = %q, want %q", got2.Locale, "fr")
	}

	if err := repo.Delete(ctx, 1, "shared-tok"); err != nil {
		t.Fatalf("Delete(region 1): %v", err)
	}
	if _, err := repo.Get(ctx, 2, "shared-tok"); err != nil {
		t.Errorf("Get(region 2) after deleting region 1's row = %v, want no error (rows are independent)", err)
	}
}

// testConcurrentFirstRegistrationRaces asserts the Upsert contract's core
// promise: many goroutines racing to register the same brand-new (region,
// token) pair for the first time must all succeed with no error, and must
// leave exactly one row behind. This is what pins Upsert to a single atomic
// INSERT ... ON CONFLICT DO UPDATE rather than a check-then-write pattern
// that could double-insert or return a raw UNIQUE constraint violation to
// the loser of the race.
func testConcurrentFirstRegistrationRaces(t *testing.T, newStore newPushRegStoreFunc) {
	repo, regionRepo := newStore(t)
	ctx := context.Background()
	putPushRegRegion(t, regionRepo, 1)

	const n = 8
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})

	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = repo.Upsert(ctx, fullUpsert(1, "race-tok"), base)
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: Upsert = %v, want nil", i, err)
		}
	}

	got, err := repo.Get(ctx, 1, "race-tok")
	if err != nil {
		t.Fatalf("Get after race: %v", err)
	}
	if got.Token != "race-tok" {
		t.Errorf("Get after race Token = %q, want %q", got.Token, "race-tok")
	}

	// DeleteByToken's row count is what actually proves "exactly one row",
	// rather than merely "at least one": a broken adapter that let two
	// goroutines both INSERT (violating the UNIQUE index only one of them
	// would hit) is ruled out by every Upsert call reporting nil above, and a
	// duplicate-row adapter that somehow bypassed the unique index entirely
	// is ruled out here.
	n2, err := repo.DeleteByToken(ctx, "race-tok")
	if err != nil {
		t.Fatalf("DeleteByToken after race: %v", err)
	}
	if n2 != 1 {
		t.Fatalf("DeleteByToken after race = %d, want 1 (exactly one row must exist)", n2)
	}
}
