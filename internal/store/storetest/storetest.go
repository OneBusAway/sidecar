// Package storetest is the shared conformance suite for alerts and regions
// repositories. It runs against the SQLite adapter today; when a Postgres
// adapter is added it runs unchanged against that store too, which is what
// makes database portability real rather than aspirational.
//
// It is a normal package, not a _test.go file, so other engine adapters can
// import RunAlertRepository from their own tests. Because it is not a test
// file, the time.Now/time.Local lint ban still applies here: every clock
// value tests need is derived from the fixed instant base below rather than
// read from the wall clock.
package storetest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/alerts"
	"github.com/OneBusAway/sidecar/internal/regions"
)

// base is the fixed instant every subtest builds its timestamps from.
var base = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// newStoreFunc is shorthand for the callback every subtest receives: it
// hands back a fresh, migrated pair of repositories backed by the same
// underlying store.
type newStoreFunc func(*testing.T) (alerts.Repository, regions.Repository)

// RunAlertRepository exercises an alerts.Repository/regions.Repository pair
// against the behavioral contract both engines must satisfy. Each subtest
// gets a fresh store from newStore.
func RunAlertRepository(t *testing.T, newStore newStoreFunc) {
	t.Helper()

	t.Run("CreateGetRoundTrip", func(t *testing.T) { testCreateGetRoundTrip(t, newStore) })
	t.Run("DraftsInvisibleUntilPublished", func(t *testing.T) { testDraftsInvisibleUntilPublished(t, newStore) })
	t.Run("TestAlertsExcludedByDefault", func(t *testing.T) { testTestAlertsExcludedByDefault(t, newStore) })
	t.Run("OrderingIsDeterministic", func(t *testing.T) { testOrderingIsDeterministic(t, newStore) })
	t.Run("FeedCapReturnsNewest20", func(t *testing.T) { testFeedCapReturnsNewest20(t, newStore) })
	t.Run("RegionScoping", func(t *testing.T) { testRegionScoping(t, newStore) })
	t.Run("StartTimeBeyond32Bit", func(t *testing.T) { testStartTimeBeyond32Bit(t, newStore) })
	t.Run("PartialUpsertPreservesLocalFields", func(t *testing.T) { testPartialUpsertPreservesLocalFields(t, newStore) })
	t.Run("UpsertNeverDeletes", func(t *testing.T) { testUpsertNeverDeletes(t, newStore) })
	t.Run("TranslationUpsertReplaces", func(t *testing.T) { testTranslationUpsertReplaces(t, newStore) })
	t.Run("FeedAttachesTranslations", func(t *testing.T) { testFeedAttachesTranslations(t, newStore) })
	t.Run("DeleteCascadesToTranslations", func(t *testing.T) { testDeleteCascadesToTranslations(t, newStore) })
	t.Run("UpdatePatchSemantics", func(t *testing.T) { testUpdatePatchSemantics(t, newStore) })
}

// putRegion inserts a minimal directory-sourced region with the given id.
// Every alert-focused subtest needs at least one region to satisfy the
// alerts.region_id foreign key before it can create an alert.
func putRegion(t *testing.T, repo regions.Repository, id int64) {
	t.Helper()
	if err := repo.UpsertFromDirectory(context.Background(), []regions.Region{{
		ID:         id,
		Name:       fmt.Sprintf("Region %d", id),
		OBABaseURL: "https://example.org/",
		Active:     true,
	}}, base); err != nil {
		t.Fatalf("UpsertFromDirectory(%d): %v", id, err)
	}
}

// newAlertIn builds the minimal valid NewAlert for regionID starting at
// start. Tests override fields as needed.
func newAlertIn(regionID int64, start time.Time) alerts.NewAlert {
	return alerts.NewAlert{
		RegionID:   regionID,
		AgencyID:   "40",
		HeaderText: "Alert",
		Cause:      "UNKNOWN_CAUSE",
		Effect:     "UNKNOWN_EFFECT",
		Severity:   "WARNING",
		StartTime:  start,
	}
}

func testCreateGetRoundTrip(t *testing.T, newStore newStoreFunc) {
	repo, regionRepo := newStore(t)
	ctx := context.Background()
	putRegion(t, regionRepo, 1)

	end := base.Add(2 * time.Hour)
	in := alerts.NewAlert{
		RegionID:        1,
		AgencyID:        "40",
		HeaderText:      "Bus delayed",
		DescriptionText: "Signal problem near downtown",
		URL:             "https://example.org/alert/1",
		Cause:           "TECHNICAL_PROBLEM",
		Effect:          "SIGNIFICANT_DELAYS",
		Severity:        "WARNING",
		StartTime:       base,
		EndTime:         &end,
		IsTest:          true,
	}
	created, err := repo.Create(ctx, in, base)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := repo.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if got.RegionID != in.RegionID || got.AgencyID != in.AgencyID || got.HeaderText != in.HeaderText ||
		got.DescriptionText != in.DescriptionText || got.URL != in.URL || got.Cause != in.Cause ||
		got.Effect != in.Effect || got.Severity != in.Severity || got.IsTest != in.IsTest {
		t.Errorf("Get() = %+v, want fields matching %+v", got, in)
	}
	if !got.StartTime.Equal(in.StartTime) {
		t.Errorf("StartTime = %v, want %v", got.StartTime, in.StartTime)
	}
	if got.EndTime == nil || !got.EndTime.Equal(end) {
		t.Errorf("EndTime = %v, want %v", got.EndTime, end)
	}
	if got.Published {
		t.Error("newly created alert is Published, want false")
	}

	// A second alert with a nil EndTime must round-trip as nil, not a zero
	// time or some other sentinel.
	noEnd, err := repo.Create(ctx, alerts.NewAlert{
		RegionID: 1, AgencyID: "40", HeaderText: "No end", Cause: "UNKNOWN_CAUSE",
		Effect: "UNKNOWN_EFFECT", Severity: "INFO", StartTime: base,
	}, base)
	if err != nil {
		t.Fatalf("Create(no end): %v", err)
	}
	got2, err := repo.Get(ctx, noEnd.ID)
	if err != nil {
		t.Fatalf("Get(no end): %v", err)
	}
	if got2.EndTime != nil {
		t.Errorf("EndTime = %v, want nil", got2.EndTime)
	}
}

func testDraftsInvisibleUntilPublished(t *testing.T, newStore newStoreFunc) {
	repo, regionRepo := newStore(t)
	ctx := context.Background()
	putRegion(t, regionRepo, 1)

	a, err := repo.Create(ctx, newAlertIn(1, base), base)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	feed, err := repo.Feed(ctx, 1, true, 20)
	if err != nil {
		t.Fatalf("Feed (draft): %v", err)
	}
	if len(feed) != 0 {
		t.Fatalf("Feed (draft) = %d alerts, want 0", len(feed))
	}

	if err = repo.SetPublished(ctx, a.ID, true, base); err != nil {
		t.Fatalf("SetPublished(true): %v", err)
	}
	feed, err = repo.Feed(ctx, 1, true, 20)
	if err != nil {
		t.Fatalf("Feed (published): %v", err)
	}
	if len(feed) != 1 || feed[0].ID != a.ID {
		t.Fatalf("Feed (published) = %+v, want [%d]", feed, a.ID)
	}

	if err = repo.SetPublished(ctx, a.ID, false, base); err != nil {
		t.Fatalf("SetPublished(false): %v", err)
	}
	feed, err = repo.Feed(ctx, 1, true, 20)
	if err != nil {
		t.Fatalf("Feed (unpublished): %v", err)
	}
	if len(feed) != 0 {
		t.Fatalf("Feed (unpublished) = %d alerts, want 0", len(feed))
	}
}

func testTestAlertsExcludedByDefault(t *testing.T, newStore newStoreFunc) {
	repo, regionRepo := newStore(t)
	ctx := context.Background()
	putRegion(t, regionRepo, 1)

	normalIn := newAlertIn(1, base)
	normalIn.HeaderText = "Normal alert"
	normal, err := repo.Create(ctx, normalIn, base)
	if err != nil {
		t.Fatalf("Create(normal): %v", err)
	}
	if err = repo.SetPublished(ctx, normal.ID, true, base); err != nil {
		t.Fatalf("SetPublished(normal): %v", err)
	}

	testIn := newAlertIn(1, base)
	testIn.HeaderText = "Test alert"
	testIn.IsTest = true
	testAlert, err := repo.Create(ctx, testIn, base)
	if err != nil {
		t.Fatalf("Create(test): %v", err)
	}
	if err = repo.SetPublished(ctx, testAlert.ID, true, base); err != nil {
		t.Fatalf("SetPublished(test): %v", err)
	}

	excluded, err := repo.Feed(ctx, 1, false, 20)
	if err != nil {
		t.Fatalf("Feed(includeTest=false): %v", err)
	}
	if len(excluded) != 1 || excluded[0].ID != normal.ID {
		t.Fatalf("Feed(includeTest=false) = %+v, want only [%d]", excluded, normal.ID)
	}

	included, err := repo.Feed(ctx, 1, true, 20)
	if err != nil {
		t.Fatalf("Feed(includeTest=true): %v", err)
	}
	if len(included) != 2 {
		t.Fatalf("Feed(includeTest=true) = %d alerts, want 2", len(included))
	}
	var sawNormal, sawTest bool
	for _, a := range included {
		if a.ID == normal.ID {
			sawNormal = true
		}
		if a.ID == testAlert.ID {
			sawTest = true
		}
	}
	// This is the assertion that catches a broken predicate such as
	// `is_test = :include_test`, which returns ONLY test alerts when
	// includeTest=true and hides every real alert from an agency verifying
	// delivery.
	if !sawNormal {
		t.Error("Feed(includeTest=true) is missing the normal alert")
	}
	if !sawTest {
		t.Error("Feed(includeTest=true) is missing the test alert")
	}
}

func testOrderingIsDeterministic(t *testing.T, newStore newStoreFunc) {
	repo, regionRepo := newStore(t)
	ctx := context.Background()
	putRegion(t, regionRepo, 1)

	// a and b share a start_time; c starts an hour later. Newest-first with
	// start_time DESC alone leaves a and b in unspecified order -- the id
	// DESC tie-break must put b (created after a) ahead of a.
	a, err := repo.Create(ctx, newAlertIn(1, base), base)
	if err != nil {
		t.Fatalf("Create(a): %v", err)
	}
	b, err := repo.Create(ctx, newAlertIn(1, base), base)
	if err != nil {
		t.Fatalf("Create(b): %v", err)
	}
	c, err := repo.Create(ctx, newAlertIn(1, base.Add(time.Hour)), base)
	if err != nil {
		t.Fatalf("Create(c): %v", err)
	}
	for _, id := range []int64{a.ID, b.ID, c.ID} {
		if err := repo.SetPublished(ctx, id, true, base); err != nil {
			t.Fatalf("SetPublished(%d): %v", id, err)
		}
	}

	want := []int64{c.ID, b.ID, a.ID}
	for i := range 2 {
		feed, err := repo.Feed(ctx, 1, false, 20)
		if err != nil {
			t.Fatalf("Feed (call %d): %v", i, err)
		}
		got := make([]int64, len(feed))
		for j, al := range feed {
			got[j] = al.ID
		}
		if !slices.Equal(got, want) {
			t.Fatalf("Feed (call %d) order = %v, want %v", i, got, want)
		}
	}
}

func testFeedCapReturnsNewest20(t *testing.T, newStore newStoreFunc) {
	repo, regionRepo := newStore(t)
	ctx := context.Background()
	putRegion(t, regionRepo, 1)

	const total = 25
	ids := make([]int64, total)
	for i := range total {
		start := base.Add(time.Duration(i) * time.Minute)
		a, err := repo.Create(ctx, newAlertIn(1, start), base)
		if err != nil {
			t.Fatalf("Create(%d): %v", i, err)
		}
		if err := repo.SetPublished(ctx, a.ID, true, base); err != nil {
			t.Fatalf("SetPublished(%d): %v", i, err)
		}
		ids[i] = a.ID
	}

	feed, err := repo.Feed(ctx, 1, false, 20)
	if err != nil {
		t.Fatalf("Feed: %v", err)
	}
	if len(feed) != 20 {
		t.Fatalf("len(Feed) = %d, want 20", len(feed))
	}

	// Newest first: the last 20 created, in reverse order.
	want := make([]int64, 20)
	for i := range 20 {
		want[i] = ids[total-1-i]
	}
	got := make([]int64, len(feed))
	for i, a := range feed {
		got[i] = a.ID
	}
	if !slices.Equal(got, want) {
		t.Fatalf("Feed IDs = %v, want %v (20 newest)", got, want)
	}
}

func testRegionScoping(t *testing.T, newStore newStoreFunc) {
	repo, regionRepo := newStore(t)
	ctx := context.Background()
	// Region 0 (Tampa Bay) must behave as an ordinary region, not a
	// sentinel meaning "all regions".
	putRegion(t, regionRepo, 0)
	putRegion(t, regionRepo, 1)

	zeroAlert, err := repo.Create(ctx, newAlertIn(0, base), base)
	if err != nil {
		t.Fatalf("Create(region 0): %v", err)
	}
	if err = repo.SetPublished(ctx, zeroAlert.ID, true, base); err != nil {
		t.Fatalf("SetPublished(region 0): %v", err)
	}

	oneAlert, err := repo.Create(ctx, newAlertIn(1, base), base)
	if err != nil {
		t.Fatalf("Create(region 1): %v", err)
	}
	if err = repo.SetPublished(ctx, oneAlert.ID, true, base); err != nil {
		t.Fatalf("SetPublished(region 1): %v", err)
	}

	zeroFeed, err := repo.Feed(ctx, 0, true, 20)
	if err != nil {
		t.Fatalf("Feed(0): %v", err)
	}
	if len(zeroFeed) != 1 || zeroFeed[0].ID != zeroAlert.ID {
		t.Fatalf("Feed(0) = %+v, want only [%d]", zeroFeed, zeroAlert.ID)
	}

	oneFeed, err := repo.Feed(ctx, 1, true, 20)
	if err != nil {
		t.Fatalf("Feed(1): %v", err)
	}
	if len(oneFeed) != 1 || oneFeed[0].ID != oneAlert.ID {
		t.Fatalf("Feed(1) = %+v, want only [%d]", oneFeed, oneAlert.ID)
	}

	// ListFilter.RegionID is a *int64, and nil means "all regions" -- 0 is
	// a real region id, not usable as a zero-sentinel for "all".
	zero := int64(0)
	one := int64(1)

	zeroList, err := repo.List(ctx, alerts.ListFilter{RegionID: &zero})
	if err != nil {
		t.Fatalf("List(region 0): %v", err)
	}
	if len(zeroList) != 1 || zeroList[0].ID != zeroAlert.ID {
		t.Fatalf("List(region 0) = %+v, want only [%d]", zeroList, zeroAlert.ID)
	}

	oneList, err := repo.List(ctx, alerts.ListFilter{RegionID: &one})
	if err != nil {
		t.Fatalf("List(region 1): %v", err)
	}
	if len(oneList) != 1 || oneList[0].ID != oneAlert.ID {
		t.Fatalf("List(region 1) = %+v, want only [%d]", oneList, oneAlert.ID)
	}

	allList, err := repo.List(ctx, alerts.ListFilter{})
	if err != nil {
		t.Fatalf("List(nil): %v", err)
	}
	if len(allList) != 2 {
		t.Fatalf("List(nil) = %d alerts, want 2 (all regions)", len(allList))
	}
}

func testStartTimeBeyond32Bit(t *testing.T, newStore newStoreFunc) {
	repo, regionRepo := newStore(t)
	ctx := context.Background()
	putRegion(t, regionRepo, 1)

	// 1<<31 is the 32-bit signed overflow boundary (2038-01-19); add a day
	// past it. A future Postgres schema using INTEGER (32-bit there) instead
	// of BIGINT would silently truncate or reject this.
	want := time.Unix((1<<31)+86400, 0).UTC()

	created, err := repo.Create(ctx, alerts.NewAlert{
		RegionID: 1, AgencyID: "40", HeaderText: "Far future",
		Cause: "UNKNOWN_CAUSE", Effect: "UNKNOWN_EFFECT", Severity: "INFO",
		StartTime: want,
	}, base)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := repo.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got.StartTime.Equal(want) {
		t.Fatalf("StartTime = %v (unix %d), want %v (unix %d)", got.StartTime, got.StartTime.Unix(), want, want.Unix())
	}
}

func testPartialUpsertPreservesLocalFields(t *testing.T, newStore newStoreFunc) {
	_, regionRepo := newStore(t)
	ctx := context.Background()
	putRegion(t, regionRepo, 1)

	if err := regionRepo.SetLocalFields(ctx, 1, "40", "America/Los_Angeles", base); err != nil {
		t.Fatalf("SetLocalFields: %v", err)
	}

	// A later directory refresh with changed directory fields must not touch
	// the locally-managed fields just set above.
	if err := regionRepo.UpsertFromDirectory(ctx, []regions.Region{{
		ID: 1, Name: "Puget Sound (renamed)", OBABaseURL: "https://new.example.org/",
		SidecarBaseURL: "https://sidecar.example.org/", Language: "en", Active: false,
	}}, base.Add(time.Hour)); err != nil {
		t.Fatalf("UpsertFromDirectory: %v", err)
	}

	got, err := regionRepo.Get(ctx, 1)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Name != "Puget Sound (renamed)" {
		t.Errorf("Name = %q, want %q", got.Name, "Puget Sound (renamed)")
	}
	if got.OBABaseURL != "https://new.example.org/" {
		t.Errorf("OBABaseURL = %q, want %q", got.OBABaseURL, "https://new.example.org/")
	}
	if got.Active {
		t.Error("Active = true, want false (directory field should have updated)")
	}
	// This is the assertion that catches a full-row upsert: it would wipe
	// these on the first hourly refresh, and every alert would then emit an
	// empty agency_id.
	if got.DefaultAgencyID != "40" {
		t.Errorf("DefaultAgencyID = %q, want %q (must survive directory refresh)", got.DefaultAgencyID, "40")
	}
	if got.Timezone != "America/Los_Angeles" {
		t.Errorf("Timezone = %q, want %q (must survive directory refresh)", got.Timezone, "America/Los_Angeles")
	}
}

func testUpsertNeverDeletes(t *testing.T, newStore newStoreFunc) {
	repo, regionRepo := newStore(t)
	ctx := context.Background()
	putRegion(t, regionRepo, 1)
	putRegion(t, regionRepo, 2)

	a, err := repo.Create(ctx, newAlertIn(2, base), base)
	if err != nil {
		t.Fatalf("Create(region 2): %v", err)
	}

	// The directory now reports only region 1: region 2 has vanished
	// upstream. Upsert must not delete it, or its alert would be destroyed
	// along with it by the FK cascade.
	if err := regionRepo.UpsertFromDirectory(ctx, []regions.Region{{
		ID: 1, Name: "Region 1", OBABaseURL: "https://example.org/", Active: true,
	}}, base.Add(time.Hour)); err != nil {
		t.Fatalf("UpsertFromDirectory({1}): %v", err)
	}

	if _, err := regionRepo.Get(ctx, 2); err != nil {
		t.Fatalf("Get(region 2) after upsert without it: %v, want no error", err)
	}
	if _, err := repo.Get(ctx, a.ID); err != nil {
		t.Fatalf("Get(alert in region 2) after upsert: %v, want no error", err)
	}
}

func testTranslationUpsertReplaces(t *testing.T, newStore newStoreFunc) {
	repo, regionRepo := newStore(t)
	ctx := context.Background()
	putRegion(t, regionRepo, 1)

	a, err := repo.Create(ctx, newAlertIn(1, base), base)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	tr := alerts.Translation{Language: "es", Field: alerts.FieldHeader, Text: "Primero", SourceSHA256: alerts.SourceHash("First")}
	if err = repo.UpsertTranslation(ctx, a.ID, tr, base); err != nil {
		t.Fatalf("UpsertTranslation(1): %v", err)
	}
	tr.Text = "Segundo"
	tr.SourceSHA256 = alerts.SourceHash("Second")
	if err = repo.UpsertTranslation(ctx, a.ID, tr, base.Add(time.Minute)); err != nil {
		t.Fatalf("UpsertTranslation(2): %v", err)
	}

	got, err := repo.Get(ctx, a.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	var matches []alerts.Translation
	for _, tt := range got.Translations {
		if tt.Language == "es" && tt.Field == alerts.FieldHeader {
			matches = append(matches, tt)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("translations matching (alert, es, header) = %d, want 1 (upsert must replace, not accumulate)", len(matches))
	}
	if matches[0].Text != "Segundo" {
		t.Errorf("Text = %q, want %q (newer upsert should win)", matches[0].Text, "Segundo")
	}
}

func testFeedAttachesTranslations(t *testing.T, newStore newStoreFunc) {
	repo, regionRepo := newStore(t)
	ctx := context.Background()
	putRegion(t, regionRepo, 1)

	a, err := repo.Create(ctx, newAlertIn(1, base), base)
	if err != nil {
		t.Fatalf("Create(a): %v", err)
	}
	if err = repo.SetPublished(ctx, a.ID, true, base); err != nil {
		t.Fatalf("SetPublished(a): %v", err)
	}
	if err = repo.UpsertTranslation(ctx, a.ID, alerts.Translation{
		Language: "es", Field: alerts.FieldHeader, Text: "Alerta A",
		SourceSHA256: alerts.SourceHash("a"),
	}, base); err != nil {
		t.Fatalf("UpsertTranslation(a): %v", err)
	}

	b, err := repo.Create(ctx, newAlertIn(1, base.Add(time.Hour)), base)
	if err != nil {
		t.Fatalf("Create(b): %v", err)
	}
	if err = repo.SetPublished(ctx, b.ID, true, base); err != nil {
		t.Fatalf("SetPublished(b): %v", err)
	}
	if err = repo.UpsertTranslation(ctx, b.ID, alerts.Translation{
		Language: "es", Field: alerts.FieldHeader, Text: "Alerta B",
		SourceSHA256: alerts.SourceHash("b"),
	}, base); err != nil {
		t.Fatalf("UpsertTranslation(b): %v", err)
	}

	feed, err := repo.Feed(ctx, 1, false, 20)
	if err != nil {
		t.Fatalf("Feed: %v", err)
	}
	if len(feed) != 2 {
		t.Fatalf("len(Feed) = %d, want 2", len(feed))
	}
	byID := map[int64]alerts.Alert{feed[0].ID: feed[0], feed[1].ID: feed[1]}

	aGot, ok := byID[a.ID]
	if !ok || len(aGot.Translations) != 1 || aGot.Translations[0].Text != "Alerta A" {
		t.Errorf("alert a translations = %+v, want [Alerta A]", aGot.Translations)
	}
	bGot, ok := byID[b.ID]
	if !ok || len(bGot.Translations) != 1 || bGot.Translations[0].Text != "Alerta B" {
		t.Errorf("alert b translations = %+v, want [Alerta B]", bGot.Translations)
	}
}

func testDeleteCascadesToTranslations(t *testing.T, newStore newStoreFunc) {
	repo, regionRepo := newStore(t)
	ctx := context.Background()
	putRegion(t, regionRepo, 1)

	a, err := repo.Create(ctx, newAlertIn(1, base), base)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := repo.UpsertTranslation(ctx, a.ID, alerts.Translation{
		Language: "es", Field: alerts.FieldHeader, Text: "Alerta",
		SourceSHA256: alerts.SourceHash("x"),
	}, base); err != nil {
		t.Fatalf("UpsertTranslation: %v", err)
	}

	// If translations were not configured to cascade, this delete would fail
	// with a foreign key constraint violation -- the store enables foreign
	// key enforcement -- rather than silently leaving an orphaned row.
	if err := repo.Delete(ctx, a.ID); err != nil {
		t.Fatalf("Delete: %v, want no error (translations must cascade)", err)
	}

	if _, err := repo.Get(ctx, a.ID); !errors.Is(err, alerts.ErrNotFound) {
		t.Errorf("Get after delete: err = %v, want alerts.ErrNotFound", err)
	}
}

func testUpdatePatchSemantics(t *testing.T, newStore newStoreFunc) {
	repo, regionRepo := newStore(t)
	ctx := context.Background()
	putRegion(t, regionRepo, 1)

	end := base.Add(8 * time.Hour)
	created, err := repo.Create(ctx, alerts.NewAlert{
		RegionID: 1, AgencyID: "40", HeaderText: "Original header",
		DescriptionText: "Original description", URL: "https://example.org/1",
		Cause: "WEATHER", Effect: "DETOUR", Severity: "WARNING",
		StartTime: base, EndTime: &end,
	}, base)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// A patch with every field nil except one must leave the others alone.
	newHeader := "Updated header"
	updated, err := repo.Update(ctx, created.ID, alerts.Patch{HeaderText: &newHeader}, base.Add(time.Minute))
	if err != nil {
		t.Fatalf("Update(header only): %v", err)
	}
	if updated.HeaderText != newHeader {
		t.Errorf("HeaderText = %q, want %q", updated.HeaderText, newHeader)
	}
	if updated.DescriptionText != "Original description" {
		t.Errorf("DescriptionText = %q, want unchanged %q", updated.DescriptionText, "Original description")
	}
	if updated.URL != "https://example.org/1" {
		t.Errorf("URL = %q, want unchanged", updated.URL)
	}
	if updated.Cause != "WEATHER" || updated.Effect != "DETOUR" || updated.Severity != "WARNING" {
		t.Errorf("Cause/Effect/Severity = %q/%q/%q, want unchanged WEATHER/DETOUR/WARNING", updated.Cause, updated.Effect, updated.Severity)
	}
	if !updated.StartTime.Equal(base) {
		t.Errorf("StartTime = %v, want unchanged %v", updated.StartTime, base)
	}
	// EndTime nil in the patch, with ClearEndTime false, means unchanged --
	// distinct from ClearEndTime, which sets NULL.
	if updated.EndTime == nil || !updated.EndTime.Equal(end) {
		t.Errorf("EndTime = %v, want unchanged %v", updated.EndTime, end)
	}

	// ClearEndTime must set NULL even though EndTime is nil in the same patch.
	cleared, err := repo.Update(ctx, created.ID, alerts.Patch{ClearEndTime: true}, base.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("Update(ClearEndTime): %v", err)
	}
	if cleared.EndTime != nil {
		t.Errorf("EndTime = %v, want nil after ClearEndTime", cleared.EndTime)
	}
	// The header change from the previous patch must have persisted.
	if cleared.HeaderText != newHeader {
		t.Errorf("HeaderText = %q, want %q to persist across the clear", cleared.HeaderText, newHeader)
	}

	// An explicit EndTime patch (ClearEndTime false) must set exactly that
	// value -- this is how an author reinstates the 8-hour fallback after a
	// previous clear.
	newEnd := base.Add(4 * time.Hour)
	restored, err := repo.Update(ctx, created.ID, alerts.Patch{EndTime: &newEnd}, base.Add(3*time.Minute))
	if err != nil {
		t.Fatalf("Update(EndTime): %v", err)
	}
	if restored.EndTime == nil || !restored.EndTime.Equal(newEnd) {
		t.Errorf("EndTime = %v, want %v", restored.EndTime, newEnd)
	}
}
