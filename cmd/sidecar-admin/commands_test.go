package main

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/MobilityData/gtfs-realtime-bindings/golang/gtfs"

	"github.com/OneBusAway/sidecar/internal/alerts"
	"github.com/OneBusAway/sidecar/internal/regions"
	"github.com/OneBusAway/sidecar/internal/store/sqlite"
)

// newDB opens and migrates a temp database, returning its path (for the CLI's
// --db flag) and a store handle the test can use to seed fixtures and read
// back through the repositories directly -- the same interfaces the CLI
// itself writes through.
func newDB(t *testing.T) (string, *sqlite.Store) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	store, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return dbPath, store
}

// seedRegion inserts a directory-sourced region row directly through the
// repository, bypassing `region sync`'s network fetch.
func seedRegion(t *testing.T, repo regions.Repository, id int64) {
	t.Helper()
	if err := repo.UpsertFromDirectory(context.Background(), []regions.Region{{
		ID: id, Name: "Test Region", OBABaseURL: "https://example.org/", Active: true,
	}}, time.Now()); err != nil {
		t.Fatalf("UpsertFromDirectory(%d): %v", id, err)
	}
}

// cli runs one sidecar-admin invocation against dbPath through the run seam
// -- no subprocess -- and returns stdout, stderr, and the error.
func cli(t *testing.T, dbPath string, args ...string) (string, string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	full := append([]string{"--db", dbPath}, args...)
	err := run(&stdout, &stderr, full)
	return stdout.String(), stderr.String(), err
}

// parseCreatedID extracts the id `alert create` prints on success.
func parseCreatedID(t *testing.T, stdout string) int64 {
	t.Helper()
	var id int64
	if _, err := fmt.Sscanf(strings.TrimSpace(stdout), "created alert %d", &id); err != nil {
		t.Fatalf("parse created id from %q: %v", stdout, err)
	}
	return id
}

// assertNoAlerts fails the test if region has any alert at all -- the
// assertion every rejection-path test uses to confirm an error left nothing
// written, rather than an error alongside a partial write.
func assertNoAlerts(t *testing.T, store *sqlite.Store, regionID int64) {
	t.Helper()
	rid := regionID
	list, err := store.Alerts().List(context.Background(), alerts.ListFilter{RegionID: &rid})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("List(region %d) = %d alerts, want 0 (rejection must write nothing)", regionID, len(list))
	}
}

func hasTranslation(msg *gtfs.FeedMessage, lang string) bool {
	for _, e := range msg.GetEntity() {
		for _, tr := range e.GetAlert().GetHeaderText().GetTranslation() {
			if tr.GetLanguage() == lang {
				return true
			}
		}
	}
	return false
}

func TestRoundTrip_CreatePublishAppearsInFeed(t *testing.T) {
	t.Parallel()
	dbPath, store := newDB(t)
	seedRegion(t, store.Regions(), 1)

	if _, _, err := cli(t, dbPath, "region", "set", "--id", "1", "--agency-id", "40", "--timezone", "America/Los_Angeles"); err != nil {
		t.Fatalf("region set: %v", err)
	}

	stdout, _, err := cli(t, dbPath, "alert", "create", "--region", "1",
		"--header", "Route 44 detoured", "--start", "2026-08-15T14:00:00-07:00",
		"--cause", "CONSTRUCTION", "--effect", "DETOUR")
	if err != nil {
		t.Fatalf("alert create: %v", err)
	}
	id := parseCreatedID(t, stdout)

	if _, _, pubErr := cli(t, dbPath, "alert", "publish", strconv.FormatInt(id, 10)); pubErr != nil {
		t.Fatalf("alert publish: %v", pubErr)
	}

	feed, err := store.Alerts().Feed(context.Background(), 1, false, alerts.FeedLimit)
	if err != nil {
		t.Fatalf("Feed: %v", err)
	}
	if len(feed) != 1 {
		t.Fatalf("Feed = %d alerts, want 1", len(feed))
	}
	if feed[0].AgencyID != "40" {
		t.Errorf("AgencyID = %q, want %q", feed[0].AgencyID, "40")
	}
	if feed[0].HeaderText != "Route 44 detoured" {
		t.Errorf("HeaderText = %q, want %q", feed[0].HeaderText, "Route 44 detoured")
	}
}

func TestDraftNotInFeed(t *testing.T) {
	t.Parallel()
	dbPath, store := newDB(t)
	seedRegion(t, store.Regions(), 1)
	if _, _, err := cli(t, dbPath, "region", "set", "--id", "1", "--agency-id", "40", "--timezone", "UTC"); err != nil {
		t.Fatalf("region set: %v", err)
	}

	if _, _, err := cli(t, dbPath, "alert", "create", "--region", "1", "--header", "Draft", "--start", "2026-08-15T14:00:00Z"); err != nil {
		t.Fatalf("alert create: %v", err)
	}

	feed, err := store.Alerts().Feed(context.Background(), 1, false, alerts.FeedLimit)
	if err != nil {
		t.Fatalf("Feed: %v", err)
	}
	if len(feed) != 0 {
		t.Fatalf("Feed = %d alerts, want 0 (draft never published)", len(feed))
	}
}

func TestNaiveStartRejected(t *testing.T) {
	t.Parallel()
	dbPath, store := newDB(t)
	seedRegion(t, store.Regions(), 1)
	if _, _, err := cli(t, dbPath, "region", "set", "--id", "1", "--agency-id", "40", "--timezone", "America/Los_Angeles"); err != nil {
		t.Fatalf("region set: %v", err)
	}

	_, _, err := cli(t, dbPath, "alert", "create", "--region", "1", "--header", "H", "--start", "2026-08-15 14:00:00")
	if err == nil {
		t.Fatal("alert create with a naive start: want error, got nil")
	}
	if !strings.Contains(err.Error(), "America/Los_Angeles") {
		t.Errorf("error = %v, want it to mention the region's timezone", err)
	}
	assertNoAlerts(t, store, 1)
}

func TestPre2000StartRejected(t *testing.T) {
	t.Parallel()
	dbPath, store := newDB(t)
	seedRegion(t, store.Regions(), 1)
	if _, _, err := cli(t, dbPath, "region", "set", "--id", "1", "--agency-id", "40", "--timezone", "UTC"); err != nil {
		t.Fatalf("region set: %v", err)
	}

	// A typo'd negative epoch: this is exactly the case that would wrap to
	// an enormous uint64 in the proto's TimeRange if it reached BuildFeed.
	_, _, err := cli(t, dbPath, "alert", "create", "--region", "1", "--header", "H", "--start", "1969-12-31T23:00:00Z")
	if err == nil {
		t.Fatal("alert create with a pre-2000 start: want error, got nil")
	}
	assertNoAlerts(t, store, 1)
}

func TestEndBeforeStartRejected(t *testing.T) {
	t.Parallel()
	dbPath, store := newDB(t)
	seedRegion(t, store.Regions(), 1)
	if _, _, err := cli(t, dbPath, "region", "set", "--id", "1", "--agency-id", "40", "--timezone", "UTC"); err != nil {
		t.Fatalf("region set: %v", err)
	}

	_, _, err := cli(t, dbPath, "alert", "create", "--region", "1", "--header", "H",
		"--start", "2026-08-15T14:00:00Z", "--end", "2026-08-15T13:00:00Z")
	if err == nil {
		t.Fatal("alert create with end <= start: want error, got nil")
	}
	assertNoAlerts(t, store, 1)
}

func TestMissingAgencyResolutionRejected(t *testing.T) {
	t.Parallel()
	dbPath, store := newDB(t)
	seedRegion(t, store.Regions(), 1)
	// Region has no default agency id and none is passed on the command line.

	_, _, err := cli(t, dbPath, "alert", "create", "--region", "1", "--header", "H", "--start", "2026-08-15T14:00:00Z")
	if err == nil {
		t.Fatal("alert create with no agency resolution: want error, got nil")
	}
	assertNoAlerts(t, store, 1)
}

func TestRegionDefaultAgencyApplied(t *testing.T) {
	t.Parallel()
	dbPath, store := newDB(t)
	seedRegion(t, store.Regions(), 1)
	if _, _, err := cli(t, dbPath, "region", "set", "--id", "1", "--agency-id", "77", "--timezone", "UTC"); err != nil {
		t.Fatalf("region set: %v", err)
	}

	stdout, _, err := cli(t, dbPath, "alert", "create", "--region", "1", "--header", "H", "--start", "2026-08-15T14:00:00Z")
	if err != nil {
		t.Fatalf("alert create: %v", err)
	}
	id := parseCreatedID(t, stdout)

	got, err := store.Alerts().Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.AgencyID != "77" {
		t.Errorf("AgencyID = %q, want the region default %q", got.AgencyID, "77")
	}
}

func TestUnknownCauseRejected(t *testing.T) {
	t.Parallel()
	dbPath, store := newDB(t)
	seedRegion(t, store.Regions(), 1)
	if _, _, err := cli(t, dbPath, "region", "set", "--id", "1", "--agency-id", "40", "--timezone", "UTC"); err != nil {
		t.Fatalf("region set: %v", err)
	}

	_, _, err := cli(t, dbPath, "alert", "create", "--region", "1", "--header", "H",
		"--start", "2026-08-15T14:00:00Z", "--cause", "BOGUS_CAUSE")
	if err == nil {
		t.Fatal("alert create with unknown cause: want error, got nil")
	}
	if !strings.Contains(err.Error(), "valid values") {
		t.Errorf("error = %v, want it to list valid values", err)
	}
	assertNoAlerts(t, store, 1)
}

func TestUnknownTimezoneRejected(t *testing.T) {
	t.Parallel()
	dbPath, store := newDB(t)
	seedRegion(t, store.Regions(), 1)

	_, _, err := cli(t, dbPath, "region", "set", "--id", "1", "--timezone", "America/Seatle")
	if err == nil {
		t.Fatal("region set with unknown timezone: want error, got nil")
	}

	got, gerr := store.Regions().Get(context.Background(), 1)
	if gerr != nil {
		t.Fatalf("Get: %v", gerr)
	}
	// The schema default ("UTC") is what a freshly-synced, never-configured
	// region carries; the assertion is that the rejected --timezone never
	// overwrote it.
	if got.Timezone != "UTC" {
		t.Errorf("Timezone = %q, want unchanged default %q (rejection must write nothing)", got.Timezone, "UTC")
	}
}

func TestRegionSetUnknownIDErrors(t *testing.T) {
	t.Parallel()
	dbPath, store := newDB(t)

	_, _, err := cli(t, dbPath, "region", "set", "--id", "999", "--agency-id", "1", "--timezone", "UTC")
	if err == nil {
		t.Fatal("region set on an unknown id: want error, got nil")
	}

	if _, gerr := store.Regions().Get(context.Background(), 999); gerr == nil {
		t.Fatal("region 999 exists after a rejected `region set`; want it never inserted")
	}
}

func TestAlertListRegionZeroFiltersToThatRegion(t *testing.T) {
	t.Parallel()
	dbPath, store := newDB(t)
	seedRegion(t, store.Regions(), 0) // Tampa Bay
	seedRegion(t, store.Regions(), 1)
	if _, _, err := cli(t, dbPath, "region", "set", "--id", "0", "--agency-id", "40", "--timezone", "UTC"); err != nil {
		t.Fatalf("region set 0: %v", err)
	}
	if _, _, err := cli(t, dbPath, "region", "set", "--id", "1", "--agency-id", "40", "--timezone", "UTC"); err != nil {
		t.Fatalf("region set 1: %v", err)
	}

	if _, _, err := cli(t, dbPath, "alert", "create", "--region", "0", "--header", "Region0Alert", "--start", "2026-08-15T14:00:00Z"); err != nil {
		t.Fatalf("alert create region 0: %v", err)
	}
	if _, _, err := cli(t, dbPath, "alert", "create", "--region", "1", "--header", "Region1Alert", "--start", "2026-08-15T14:00:00Z"); err != nil {
		t.Fatalf("alert create region 1: %v", err)
	}

	stdout, _, err := cli(t, dbPath, "alert", "list", "--region", "0")
	if err != nil {
		t.Fatalf("alert list --region 0: %v", err)
	}
	if !strings.Contains(stdout, "Region0Alert") {
		t.Errorf("stdout = %q, want it to contain region 0's alert", stdout)
	}
	if strings.Contains(stdout, "Region1Alert") {
		t.Errorf("stdout = %q, want it to NOT contain region 1's alert", stdout)
	}
}

func TestTranslateThenEditWithholdsStaleTranslation(t *testing.T) {
	t.Parallel()
	dbPath, store := newDB(t)
	seedRegion(t, store.Regions(), 1)
	if _, _, err := cli(t, dbPath, "region", "set", "--id", "1", "--agency-id", "40", "--timezone", "UTC"); err != nil {
		t.Fatalf("region set: %v", err)
	}

	stdout, _, err := cli(t, dbPath, "alert", "create", "--region", "1", "--header", "Original", "--start", "2026-08-15T14:00:00Z")
	if err != nil {
		t.Fatalf("alert create: %v", err)
	}
	idStr := strconv.FormatInt(parseCreatedID(t, stdout), 10)

	if _, _, pubErr := cli(t, dbPath, "alert", "publish", idStr); pubErr != nil {
		t.Fatalf("alert publish: %v", pubErr)
	}
	if _, _, trErr := cli(t, dbPath, "alert", "translate", idStr, "--language", "es", "--header", "Original ES"); trErr != nil {
		t.Fatalf("alert translate: %v", trErr)
	}

	ctx := context.Background()
	feed, err := store.Alerts().Feed(ctx, 1, false, alerts.FeedLimit)
	if err != nil {
		t.Fatalf("Feed: %v", err)
	}
	msg := alerts.BuildFeed(feed, alerts.FeedOptions{Now: time.Now()})
	if !hasTranslation(msg, "es") {
		t.Fatal("fresh es translation missing from feed before the English edit")
	}

	if _, _, editErr := cli(t, dbPath, "alert", "edit", idStr, "--header", "Updated"); editErr != nil {
		t.Fatalf("alert edit: %v", editErr)
	}

	feed2, err := store.Alerts().Feed(ctx, 1, false, alerts.FeedLimit)
	if err != nil {
		t.Fatalf("Feed: %v", err)
	}
	msg2 := alerts.BuildFeed(feed2, alerts.FeedOptions{Now: time.Now()})
	if hasTranslation(msg2, "es") {
		t.Fatal("stale es translation still present in feed after the English edit; want it withheld")
	}
}
