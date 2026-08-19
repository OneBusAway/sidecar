// Package storetest is the shared conformance suite for the alerts, regions,
// and auth repositories. It runs against the SQLite adapter today; when a Postgres
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
	"strings"
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
	t.Run("CentroidRoundTrip", func(t *testing.T) { testCentroidRoundTrip(t, newStore) })
	t.Run("CentroidRejectsHalfSet", func(t *testing.T) { testCentroidRejectsHalfSet(t, newStore) })
	t.Run("SetLocalFieldsWritesAllThree", func(t *testing.T) { testSetLocalFieldsWritesAllThree(t, newStore) })
	t.Run("UpsertNeverDeletes", func(t *testing.T) { testUpsertNeverDeletes(t, newStore) })
	t.Run("TranslationUpsertReplaces", func(t *testing.T) { testTranslationUpsertReplaces(t, newStore) })
	t.Run("FeedAttachesTranslations", func(t *testing.T) { testFeedAttachesTranslations(t, newStore) })
	t.Run("DeleteCascadesToTranslations", func(t *testing.T) { testDeleteCascadesToTranslations(t, newStore) })
	t.Run("UpdatePatchSemantics", func(t *testing.T) { testUpdatePatchSemantics(t, newStore) })
	t.Run("SetPublishedAndDeleteReportUnknownID", func(t *testing.T) { testSetPublishedAndDeleteReportUnknownID(t, newStore) })
	t.Run("CreateRejectsInvalidWindow", func(t *testing.T) { testCreateRejectsInvalidWindow(t, newStore) })
	t.Run("CreateRejectsEmptyAgencyID", func(t *testing.T) { testCreateRejectsEmptyAgencyID(t, newStore) })
	t.Run("UpdateRejectsEmptyAgencyID", func(t *testing.T) { testUpdateRejectsEmptyAgencyID(t, newStore) })
	t.Run("CreateRejectsEmptyHeaderText", func(t *testing.T) { testCreateRejectsEmptyHeaderText(t, newStore) })
	t.Run("UpdateRejectsEmptyHeaderText", func(t *testing.T) { testUpdateRejectsEmptyHeaderText(t, newStore) })
	t.Run("UpsertTranslationNormalizesLanguage", func(t *testing.T) { testUpsertTranslationNormalizesLanguage(t, newStore) })
	t.Run("AlertTimestampsPopulated", func(t *testing.T) { testAlertTimestampsPopulated(t, newStore) })
	t.Run("DeleteTranslationRemovesBothFields", func(t *testing.T) { testDeleteTranslationRemovesBothFields(t, newStore) })
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
	// now is pinned near want, rather than the package's shared `base`
	// (2026), so this fixture -- whose whole point is exercising storage
	// past the 32-bit boundary -- doesn't also trip Create's unrelated
	// ValidateWindow "more than 10 years out" check.
	now := want.AddDate(-1, 0, 0)

	created, err := repo.Create(ctx, alerts.NewAlert{
		RegionID: 1, AgencyID: "40", HeaderText: "Far future",
		Cause: "UNKNOWN_CAUSE", Effect: "UNKNOWN_EFFECT", Severity: "INFO",
		StartTime: want,
	}, now)
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

	if err := regionRepo.SetLocalFields(ctx, 1, regions.LocalFields{
		DefaultAgencyID: "40", Timezone: "America/Los_Angeles", OBAAPIKey: "secret-key",
	}, base); err != nil {
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
	// This is the assertion that catches a full-row upsert wiping the key
	// specifically: DefaultAgencyID and Timezone were already covered above,
	// but OBAAPIKey is the newest of the three locally-managed columns and the
	// one most likely to be left out of a partial-upsert's column list.
	if got.OBAAPIKey != "secret-key" {
		t.Errorf("OBAAPIKey = %q, want %q (must survive directory refresh)", got.OBAAPIKey, "secret-key")
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

// testSetPublishedAndDeleteReportUnknownID asserts that SetPublished and
// Delete report alerts.ErrNotFound for an id that does not exist, rather than
// returning nil. Without this, `sidecar-admin alert publish 9999` exits 0 and
// prints nothing -- an author who mistypes an id believes the alert is live
// when nothing happened. A future Postgres adapter inherits this requirement
// because it runs against this same suite.
func testSetPublishedAndDeleteReportUnknownID(t *testing.T, newStore newStoreFunc) {
	repo, _ := newStore(t)
	ctx := context.Background()

	const unknownID = 999999

	if err := repo.SetPublished(ctx, unknownID, true, base); !errors.Is(err, alerts.ErrNotFound) {
		t.Errorf("SetPublished(unknown id) = %v, want alerts.ErrNotFound", err)
	}
	if err := repo.Delete(ctx, unknownID); !errors.Is(err, alerts.ErrNotFound) {
		t.Errorf("Delete(unknown id) = %v, want alerts.ErrNotFound", err)
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

// testCreateRejectsInvalidWindow asserts that Create itself enforces
// alerts.ValidateWindow rather than relying on every caller to have checked
// first. TimeRange.start/end are uint64 on the wire, so a pre-epoch start
// wraps to an enormous value instead of failing; a caller that reaches the
// repository directly (an HTTP admin API, a bulk importer) -- not through
// the CLI, which validates independently -- must get the same protection. A
// future Postgres adapter inherits this requirement because it runs against
// this same suite.
func testCreateRejectsInvalidWindow(t *testing.T, newStore newStoreFunc) {
	repo, regionRepo := newStore(t)
	ctx := context.Background()
	putRegion(t, regionRepo, 1)

	preEpoch := newAlertIn(1, time.Date(1969, 12, 31, 23, 0, 0, 0, time.UTC))
	if _, err := repo.Create(ctx, preEpoch, base); err == nil {
		t.Error("Create with a pre-2000 start: want error, got nil")
	}

	end := base
	invalidEnd := newAlertIn(1, base)
	invalidEnd.EndTime = &end // end == start, not after it
	if _, err := repo.Create(ctx, invalidEnd, base); err == nil {
		t.Error("Create with end <= start: want error, got nil")
	}
}

// testCreateRejectsEmptyAgencyID asserts that Create rejects an empty
// AgencyID rather than storing it: NewAlert.AgencyID is documented as
// pre-resolved by the caller, and that contract must be enforced by the
// repository, not merely by convention -- a caller that skips resolution
// would otherwise store an alert no app can match by agency.
func testCreateRejectsEmptyAgencyID(t *testing.T, newStore newStoreFunc) {
	repo, regionRepo := newStore(t)
	ctx := context.Background()
	putRegion(t, regionRepo, 1)

	in := newAlertIn(1, base)
	in.AgencyID = ""
	if _, err := repo.Create(ctx, in, base); err == nil {
		t.Error("Create with empty AgencyID: want error, got nil")
	}
}

// testUpdateRejectsEmptyAgencyID asserts that Update enforces the same
// non-empty-AgencyID invariant Create does. The check was originally added
// only to Create; Patch{AgencyID: &""} would otherwise succeed and write an
// empty agency_id, producing an informed_entity{agency_id:""} in the feed
// that no OBA app matches by agency. A future Postgres adapter inherits this
// requirement because it runs against this same suite.
func testUpdateRejectsEmptyAgencyID(t *testing.T, newStore newStoreFunc) {
	repo, regionRepo := newStore(t)
	ctx := context.Background()
	putRegion(t, regionRepo, 1)

	created, err := repo.Create(ctx, newAlertIn(1, base), base)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	empty := ""
	if _, updateErr := repo.Update(ctx, created.ID, alerts.Patch{AgencyID: &empty}, base.Add(time.Minute)); updateErr == nil {
		t.Error("Update with empty AgencyID: want error, got nil")
	}

	got, err := repo.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.AgencyID != created.AgencyID {
		t.Errorf("AgencyID = %q after a rejected Update, want unchanged %q", got.AgencyID, created.AgencyID)
	}
}

// testCreateRejectsEmptyHeaderText asserts that Create rejects an empty
// HeaderText rather than storing it: alerts.BuildFeed passes HeaderText
// straight through to the feed's alert_header_text with no fallback, so a
// caller that reaches the repository directly (an HTTP admin API, a bulk
// importer) -- not through the CLI, which validates independently -- must
// get the same protection, or riders receive an alert with no header text. A
// future Postgres adapter inherits this requirement because it runs against
// this same suite.
func testCreateRejectsEmptyHeaderText(t *testing.T, newStore newStoreFunc) {
	repo, regionRepo := newStore(t)
	ctx := context.Background()
	putRegion(t, regionRepo, 1)

	in := newAlertIn(1, base)
	in.HeaderText = ""
	if _, err := repo.Create(ctx, in, base); err == nil {
		t.Error("Create with empty HeaderText: want error, got nil")
	}
}

// testUpdateRejectsEmptyHeaderText asserts that Update enforces the same
// non-empty-HeaderText invariant Create does. The check was originally added
// only to Create; Patch{HeaderText: &""} would otherwise succeed and blank
// the header of an already-published alert riders are reading right now. A
// future Postgres adapter inherits this requirement because it runs against
// this same suite.
func testUpdateRejectsEmptyHeaderText(t *testing.T, newStore newStoreFunc) {
	repo, regionRepo := newStore(t)
	ctx := context.Background()
	putRegion(t, regionRepo, 1)

	created, err := repo.Create(ctx, newAlertIn(1, base), base)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	empty := ""
	if _, updateErr := repo.Update(ctx, created.ID, alerts.Patch{HeaderText: &empty}, base.Add(time.Minute)); updateErr == nil {
		t.Error("Update with empty HeaderText: want error, got nil")
	}

	got, err := repo.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.HeaderText != created.HeaderText {
		t.Errorf("HeaderText = %q after a rejected Update, want unchanged %q", got.HeaderText, created.HeaderText)
	}
}

// testUpsertTranslationNormalizesLanguage asserts that UpsertTranslation
// normalizes the language tag itself. The schema's UNIQUE(alert_id,
// language, field) is case-sensitive, so a caller that forgot to normalize
// would otherwise insert "ES" alongside an existing "es" -- two live rows
// for one language that the feed would both emit to riders.
func testUpsertTranslationNormalizesLanguage(t *testing.T, newStore newStoreFunc) {
	repo, regionRepo := newStore(t)
	ctx := context.Background()
	putRegion(t, regionRepo, 1)

	a, err := repo.Create(ctx, newAlertIn(1, base), base)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err = repo.UpsertTranslation(ctx, a.ID, alerts.Translation{
		Language: "ES", Field: alerts.FieldHeader, Text: "Primero",
		SourceSHA256: alerts.SourceHash("Alert"),
	}, base); err != nil {
		t.Fatalf("UpsertTranslation(ES): %v", err)
	}
	if err = repo.UpsertTranslation(ctx, a.ID, alerts.Translation{
		Language: "es", Field: alerts.FieldHeader, Text: "Segundo",
		SourceSHA256: alerts.SourceHash("Alert"),
	}, base.Add(time.Minute)); err != nil {
		t.Fatalf("UpsertTranslation(es): %v", err)
	}

	got, err := repo.Get(ctx, a.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	var matches []alerts.Translation
	for _, tt := range got.Translations {
		if tt.Field == alerts.FieldHeader {
			matches = append(matches, tt)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("translations = %d, want 1 (differently-cased language tags must collide after normalization)", len(matches))
	}
	if matches[0].Language != "es" {
		t.Errorf("Language = %q, want normalized %q", matches[0].Language, "es")
	}
	if matches[0].Text != "Segundo" {
		t.Errorf("Text = %q, want %q (second upsert should win)", matches[0].Text, "Segundo")
	}
}

// testAlertTimestampsPopulated asserts that Get surfaces CreatedAt/UpdatedAt
// read back from storage as UTC instants -- an instant-only comparison (e.g.
// Equal) would pass even if the adapter attached the wrong zone, so this
// checks .Location() explicitly -- and that Update advances UpdatedAt while
// leaving CreatedAt untouched. A future Postgres adapter inherits this
// requirement because it runs against this same suite.
func testAlertTimestampsPopulated(t *testing.T, newStore newStoreFunc) {
	repo, regionRepo := newStore(t)
	ctx := context.Background()
	putRegion(t, regionRepo, 1)

	created, err := repo.Create(ctx, newAlertIn(1, base), base)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := repo.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got.CreatedAt.Equal(base) {
		t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, base)
	}
	if got.CreatedAt.Location() != time.UTC {
		t.Errorf("CreatedAt.Location() = %v, want %v", got.CreatedAt.Location(), time.UTC)
	}
	if !got.UpdatedAt.Equal(base) {
		t.Errorf("UpdatedAt = %v, want %v", got.UpdatedAt, base)
	}
	if got.UpdatedAt.Location() != time.UTC {
		t.Errorf("UpdatedAt.Location() = %v, want %v", got.UpdatedAt.Location(), time.UTC)
	}

	later := base.Add(time.Hour)
	newHeader := "Updated"
	updated, err := repo.Update(ctx, created.ID, alerts.Patch{HeaderText: &newHeader}, later)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if !updated.UpdatedAt.Equal(later) {
		t.Errorf("UpdatedAt after Update = %v, want %v (must advance)", updated.UpdatedAt, later)
	}
	if !updated.CreatedAt.Equal(base) {
		t.Errorf("CreatedAt after Update = %v, want unchanged %v", updated.CreatedAt, base)
	}
}

// testDeleteTranslationRemovesBothFields asserts that DeleteTranslation
// removes every field row for one (alert, language) pair -- both header and
// description -- and normalizes the language it is given: the test passes
// "ES" specifically to prove that, since the schema stores the normalized
// "es" the earlier upserts wrote. A second delete against the same
// now-empty language must report alerts.ErrNotFound rather than silently
// succeeding a second time.
func testDeleteTranslationRemovesBothFields(t *testing.T, newStore newStoreFunc) {
	repo, regionRepo := newStore(t)
	ctx := context.Background()
	putRegion(t, regionRepo, 1)

	a, err := repo.Create(ctx, newAlertIn(1, base), base)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err = repo.UpsertTranslation(ctx, a.ID, alerts.Translation{
		Language: "es", Field: alerts.FieldHeader, Text: "Encabezado",
		SourceSHA256: alerts.SourceHash("Header"),
	}, base); err != nil {
		t.Fatalf("UpsertTranslation(header): %v", err)
	}
	if err = repo.UpsertTranslation(ctx, a.ID, alerts.Translation{
		Language: "es", Field: alerts.FieldDescription, Text: "Detalle",
		SourceSHA256: alerts.SourceHash("Description"),
	}, base); err != nil {
		t.Fatalf("UpsertTranslation(description): %v", err)
	}

	if err = repo.DeleteTranslation(ctx, a.ID, "ES"); err != nil {
		t.Fatalf("DeleteTranslation(ES): %v", err)
	}

	got, err := repo.Get(ctx, a.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got.Translations) != 0 {
		t.Fatalf("Translations after DeleteTranslation = %+v, want none", got.Translations)
	}

	if err = repo.DeleteTranslation(ctx, a.ID, "ES"); !errors.Is(err, alerts.ErrNotFound) {
		t.Errorf("second DeleteTranslation(ES) = %v, want alerts.ErrNotFound", err)
	}
}

// testCentroidRoundTrip pins the three states a centroid can be in. The 0,0
// case is the point of the nullable column: it is a real coordinate in the
// Gulf of Guinea, and must survive as a value rather than reading back as
// "unset".
func testCentroidRoundTrip(t *testing.T, newStore newStoreFunc) {
	_, repo := newStore(t)
	ctx := context.Background()

	in := []regions.Region{
		{ID: 1, Name: "Has Centroid", OBABaseURL: "https://a.example/", Active: true,
			Centroid: &regions.LatLon{Lat: 47.75, Lon: -122.49}},
		{ID: 2, Name: "No Centroid", OBABaseURL: "https://b.example/", Active: true},
		{ID: 3, Name: "Null Island", OBABaseURL: "https://c.example/", Active: true,
			Centroid: &regions.LatLon{Lat: 0, Lon: 0}},
	}
	if err := repo.UpsertFromDirectory(ctx, in, base); err != nil {
		t.Fatalf("UpsertFromDirectory: %v", err)
	}

	got1, err := repo.Get(ctx, 1)
	if err != nil {
		t.Fatalf("Get(1): %v", err)
	}
	if got1.Centroid == nil {
		t.Fatal("region 1 Centroid = nil, want a point")
	}
	if got1.Centroid.Lat != 47.75 || got1.Centroid.Lon != -122.49 {
		t.Errorf("region 1 Centroid = %+v, want {47.75 -122.49}", *got1.Centroid)
	}

	got2, err := repo.Get(ctx, 2)
	if err != nil {
		t.Fatalf("Get(2): %v", err)
	}
	if got2.Centroid != nil {
		t.Errorf("region 2 Centroid = %+v, want nil", *got2.Centroid)
	}

	got3, err := repo.Get(ctx, 3)
	if err != nil {
		t.Fatalf("Get(3): %v", err)
	}
	if got3.Centroid == nil {
		t.Fatal("region 3 Centroid = nil, want 0,0 -- Null Island is a real coordinate")
	}
	if got3.Centroid.Lat != 0 || got3.Centroid.Lon != 0 {
		t.Errorf("region 3 Centroid = %+v, want {0 0}", *got3.Centroid)
	}
}

// testCentroidRejectsHalfSet proves the invariant lives in the schema, not
// only in the Go type: it exercises both the INSERT and UPDATE triggers, and
// -- beyond just checking that some error came back -- confirms each abort
// actually rolled back the offending write, rather than merely proving that
// some unrelated statement failed. A Postgres adapter expresses this as a
// CHECK; both must refuse the same row the same way.
func testCentroidRejectsHalfSet(t *testing.T, newStore newStoreFunc) {
	_, repo := newStore(t)
	ctx := context.Background()

	// The type assertion happens here, not behind a helper that swallows a
	// missing implementation into an ordinary error: that shape would let
	// this subtest pass vacuously -- without exercising anything -- for any
	// future adapter that never implements the hook.
	w, ok := repo.(HalfSetCentroidWriter)
	if !ok {
		t.Skip("adapter does not implement HalfSetCentroidWriter")
	}

	if err := repo.UpsertFromDirectory(ctx, []regions.Region{
		{ID: 1, Name: "Whole", OBABaseURL: "https://a.example/", Active: true,
			Centroid: &regions.LatLon{Lat: 47.75, Lon: -122.49}},
	}, base); err != nil {
		t.Fatalf("UpsertFromDirectory: %v", err)
	}

	// UPDATE trigger: break the centroid of the row just written.
	const wantMsg = "latitude and longitude must be set together"
	updateErr := w.WriteHalfSetCentroidForTest(ctx, 1)
	if updateErr == nil {
		t.Fatal("UPDATE with latitude but no longitude succeeded, want a constraint failure")
	}
	if !strings.Contains(updateErr.Error(), wantMsg) {
		t.Errorf("UPDATE error = %q, want it to mention %q", updateErr.Error(), wantMsg)
	}
	// Any non-nil error -- a renamed column, a broken raw statement, a closed
	// connection -- would satisfy the checks above without proving the
	// trigger fired. Re-reading the row is what proves the ABORT rolled the
	// write back rather than merely failing for some unrelated reason.
	got, err := repo.Get(ctx, 1)
	if err != nil {
		t.Fatalf("Get(1) after rejected UPDATE: %v", err)
	}
	if got.Centroid == nil || got.Centroid.Lat != 47.75 || got.Centroid.Lon != -122.49 {
		t.Errorf("Centroid after rejected UPDATE = %+v, want unchanged {47.75 -122.49}", got.Centroid)
	}

	// INSERT trigger: UpsertFromDirectory only ever writes both coordinates
	// or neither, so nothing above -- or anywhere in the normal write path --
	// ever exercises this trigger.
	insertErr := w.InsertHalfSetCentroidForTest(ctx, 2)
	if insertErr == nil {
		t.Fatal("INSERT with latitude but no longitude succeeded, want a constraint failure")
	}
	if !strings.Contains(insertErr.Error(), wantMsg) {
		t.Errorf("INSERT error = %q, want it to mention %q", insertErr.Error(), wantMsg)
	}
	// Same rollback proof as above, applied to the INSERT case: the row must
	// not exist at all, not merely have failed to receive a centroid.
	if _, err := repo.Get(ctx, 2); !errors.Is(err, regions.ErrNotFound) {
		t.Errorf("Get(2) after rejected INSERT: err = %v, want regions.ErrNotFound (the row must not exist)", err)
	}
}

// testSetLocalFieldsWritesAllThree pins that the three locally-managed fields
// travel together and that a directory refresh leaves every one of them alone.
func testSetLocalFieldsWritesAllThree(t *testing.T, newStore newStoreFunc) {
	_, repo := newStore(t)
	ctx := context.Background()

	dir := []regions.Region{{ID: 1, Name: "R", OBABaseURL: "https://a.example/", Active: true}}
	if err := repo.UpsertFromDirectory(ctx, dir, base); err != nil {
		t.Fatalf("UpsertFromDirectory: %v", err)
	}

	want := regions.LocalFields{DefaultAgencyID: "40", Timezone: "America/Los_Angeles", OBAAPIKey: "secret-key"}
	if err := repo.SetLocalFields(ctx, 1, want, base); err != nil {
		t.Fatalf("SetLocalFields: %v", err)
	}

	// A refresh must not disturb any of them.
	if err := repo.UpsertFromDirectory(ctx, dir, base.Add(time.Hour)); err != nil {
		t.Fatalf("second UpsertFromDirectory: %v", err)
	}

	got, err := repo.Get(ctx, 1)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.DefaultAgencyID != want.DefaultAgencyID {
		t.Errorf("DefaultAgencyID = %q, want %q", got.DefaultAgencyID, want.DefaultAgencyID)
	}
	if got.Timezone != want.Timezone {
		t.Errorf("Timezone = %q, want %q", got.Timezone, want.Timezone)
	}
	if got.OBAAPIKey != want.OBAAPIKey {
		t.Errorf("OBAAPIKey = %q, want %q", got.OBAAPIKey, want.OBAAPIKey)
	}
}

// HalfSetCentroidWriter is implemented by adapters that can attempt an
// invalid half-set centroid write against both the UPDATE and INSERT paths,
// so the conformance suite can prove the storage engine rejects each one. An
// adapter that does not implement it skips that subtest via t.Skip rather
// than silently passing: see the type assertion in testCentroidRejectsHalfSet.
type HalfSetCentroidWriter interface {
	// WriteHalfSetCentroidForTest attempts to break an existing row's paired
	// centroid columns, exercising the UPDATE trigger.
	WriteHalfSetCentroidForTest(ctx context.Context, id int64) error
	// InsertHalfSetCentroidForTest attempts to insert a brand new row with a
	// half-set centroid, exercising the INSERT trigger.
	InsertHalfSetCentroidForTest(ctx context.Context, id int64) error
}
