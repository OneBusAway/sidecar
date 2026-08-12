package main

import (
	"bytes"
	"context"
	"errors"
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
	"github.com/OneBusAway/sidecar/internal/store/sqlitetest"
)

// newDB opens and migrates a temp database, returning its path (for the CLI's
// --db flag) and a store handle the test can use to seed fixtures and read
// back through the repositories directly -- the same interfaces the CLI
// itself writes through.
func newDB(t *testing.T) (string, *sqlite.Store) {
	t.Helper()
	return sqlitetest.OpenAt(t)
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
// -- no subprocess -- with empty stdin (unused by every command exercised in
// this file), and returns stdout, stderr, and the error.
func cli(t *testing.T, dbPath string, args ...string) (string, string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	full := append([]string{"--db", dbPath}, args...)
	err := run(strings.NewReader(""), &stdout, &stderr, full)
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
	// This is the single most likely first-run failure -- exactly what a user
	// hits by copying the README's `alert create` without its `region set`
	// predecessor -- so the message must name both fixes with commands the
	// user can actually run, not just state the problem.
	if !strings.Contains(err.Error(), "region set") {
		t.Errorf("error = %q, want it to name the `region set` fix", err.Error())
	}
	if !strings.Contains(err.Error(), "--agency-id") {
		t.Errorf("error = %q, want it to name the --agency-id fix", err.Error())
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

// TestEditTestFalseClearsTestFlag reproduces the finding that `alert edit ID
// --test=false` was a silent no-op: the old code branched on the flag's
// *value* (`if *test`) rather than on whether it was passed, so
// `--test=false` looked indistinguishable from the flag being absent
// entirely and IsTest never changed. An author promoting a verified test
// alert to a real one with the natural `--test=false` syntax believed it was
// live; riders would never see it.
func TestEditTestFalseClearsTestFlag(t *testing.T) {
	t.Parallel()
	dbPath, store := newDB(t)
	seedRegion(t, store.Regions(), 1)

	created, err := store.Alerts().Create(context.Background(), alerts.NewAlert{
		RegionID: 1, AgencyID: "40", HeaderText: "Test alert",
		Cause: "UNKNOWN_CAUSE", Effect: "UNKNOWN_EFFECT", Severity: "WARNING",
		StartTime: time.Date(2026, 8, 15, 14, 0, 0, 0, time.UTC), IsTest: true,
	}, time.Now())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !created.IsTest {
		t.Fatalf("fixture setup: created alert must start with IsTest = true")
	}

	if _, _, editErr := cli(t, dbPath, "alert", "edit", strconv.FormatInt(created.ID, 10), "--test=false"); editErr != nil {
		t.Fatalf("alert edit --test=false: %v", editErr)
	}

	got, err := store.Alerts().Get(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.IsTest {
		t.Error("IsTest = true after --test=false, want false")
	}
}

// TestEditTestAndNoTestTogetherRejected asserts that passing both --test and
// --no-test in one invocation is an error rather than one silently winning.
func TestEditTestAndNoTestTogetherRejected(t *testing.T) {
	t.Parallel()
	dbPath, store := newDB(t)
	seedRegion(t, store.Regions(), 1)

	created, err := store.Alerts().Create(context.Background(), alerts.NewAlert{
		RegionID: 1, AgencyID: "40", HeaderText: "Alert",
		Cause: "UNKNOWN_CAUSE", Effect: "UNKNOWN_EFFECT", Severity: "WARNING",
		StartTime: time.Date(2026, 8, 15, 14, 0, 0, 0, time.UTC),
	}, time.Now())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	_, _, err = cli(t, dbPath, "alert", "edit", strconv.FormatInt(created.ID, 10), "--test", "--no-test")
	if err == nil {
		t.Fatal("alert edit --test --no-test: want error, got nil")
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

// TestAlertCreateEmptyHeaderRejected pins the CLI-level guard against
// `--header ""`: the flag is seen (so the "requires --header" check does not
// catch it), and without this check it would reach the repository, which now
// rejects it too, but with a bare storage-layer message rather than one
// naming the flag the author typed.
func TestAlertCreateEmptyHeaderRejected(t *testing.T) {
	t.Parallel()
	dbPath, store := newDB(t)
	seedRegion(t, store.Regions(), 1)
	if _, _, err := cli(t, dbPath, "region", "set", "--id", "1", "--agency-id", "40", "--timezone", "UTC"); err != nil {
		t.Fatalf("region set: %v", err)
	}

	_, _, err := cli(t, dbPath, "alert", "create", "--region", "1", "--header", "",
		"--start", "2026-08-15T14:00:00Z")
	if err == nil {
		t.Fatal("alert create with empty --header: want error, got nil")
	}
	if !strings.Contains(err.Error(), "--header") {
		t.Errorf("error = %v, want it to name --header", err)
	}
	assertNoAlerts(t, store, 1)
}

// TestAlertEditEmptyHeaderRejected pins the CLI-level guard against
// `--header ""` blanking the header of an already-published alert, mirroring
// the existing --agency-id guard in alert edit.
func TestAlertEditEmptyHeaderRejected(t *testing.T) {
	t.Parallel()
	dbPath, store := newDB(t)
	seedRegion(t, store.Regions(), 1)
	if _, _, err := cli(t, dbPath, "region", "set", "--id", "1", "--agency-id", "40", "--timezone", "UTC"); err != nil {
		t.Fatalf("region set: %v", err)
	}
	stdout, _, err := cli(t, dbPath, "alert", "create", "--region", "1", "--header", "Original",
		"--start", "2026-08-15T14:00:00Z")
	if err != nil {
		t.Fatalf("alert create: %v", err)
	}
	id := parseCreatedID(t, stdout)

	_, _, editErr := cli(t, dbPath, "alert", "edit", strconv.FormatInt(id, 10), "--header", "")
	if editErr == nil {
		t.Fatal("alert edit with empty --header: want error, got nil")
	}
	if !strings.Contains(editErr.Error(), "--header") {
		t.Errorf("error = %v, want it to name --header", editErr)
	}

	got, gerr := store.Alerts().Get(context.Background(), id)
	if gerr != nil {
		t.Fatalf("Get: %v", gerr)
	}
	if got.HeaderText != "Original" {
		t.Errorf("HeaderText = %q after a rejected edit, want unchanged %q", got.HeaderText, "Original")
	}
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

// TestEditNoTestSpellings exercises all five --test/--no-test spellings
// against a fixed starting IsTest value, reproducing the finding that
// `alert edit --no-test=false` inverted the flag. An earlier fix changed the
// branch to key off which flags were *visited* rather than their values
// (see TestEditTestFalseClearsTestFlag above), but computed
// `v := !*noTest` for the --no-test arm: --no-test=false (the standard Go
// spelling for "don't do what this flag does") made seen["no-test"] true and
// *noTest false, so v became true and a published, rider-visible alert
// edited with `alert edit ID --no-test=false --header "..."` silently
// vanished from the public feed. --no-test=false is now a no-op: there is
// no reading of "decline to unmark this as test" that means "mark it as
// test".
func TestEditNoTestSpellings(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		startIsTest bool
		flag        string
		wantIsTest  bool
	}{
		{"--test marks as test", false, "--test", true},
		{"--test=true marks as test", false, "--test=true", true},
		{"--test=false clears test", true, "--test=false", false},
		{"--no-test clears test", true, "--no-test", false},
		{"--no-test=true clears test", true, "--no-test=true", false},
		// This is the regression: --no-test=false must be a no-op, not an
		// inversion that sets IsTest to true.
		{"--no-test=false is a no-op (regression)", false, "--no-test=false", false},
		{"--no-test=false is a no-op even starting true", true, "--no-test=false", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dbPath, store := newDB(t)
			seedRegion(t, store.Regions(), 1)

			created, err := store.Alerts().Create(context.Background(), alerts.NewAlert{
				RegionID: 1, AgencyID: "40", HeaderText: "Alert",
				Cause: "UNKNOWN_CAUSE", Effect: "UNKNOWN_EFFECT", Severity: "WARNING",
				StartTime: time.Date(2026, 8, 15, 14, 0, 0, 0, time.UTC), IsTest: tt.startIsTest,
			}, time.Now())
			if err != nil {
				t.Fatalf("Create: %v", err)
			}

			if _, _, editErr := cli(t, dbPath, "alert", "edit", strconv.FormatInt(created.ID, 10), tt.flag); editErr != nil {
				t.Fatalf("alert edit %s: %v", tt.flag, editErr)
			}

			got, err := store.Alerts().Get(context.Background(), created.ID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if got.IsTest != tt.wantIsTest {
				t.Errorf("after %s (starting IsTest=%t): IsTest = %t, want %t", tt.flag, tt.startIsTest, got.IsTest, tt.wantIsTest)
			}
		})
	}
}

// TestMigrateStatusDoesNotMigrate reproduces the finding that `run` migrated
// the database unconditionally before dispatch, so `migrate status` -- a
// read-only inspection command -- applied every pending migration and then
// reported "up to date": the opposite of the truth, having silently mutated
// the schema. Run against a database that has never been touched, this must
// report pending work and leave the schema untouched.
func TestMigrateStatusDoesNotMigrate(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "test.db")

	stdout, _, err := cli(t, dbPath, "migrate", "status")
	if err != nil {
		t.Fatalf("migrate status: %v", err)
	}
	if !strings.Contains(stdout, "pending") {
		t.Errorf("stdout = %q, want it to report pending migrations on a never-touched database", stdout)
	}
	if strings.Contains(stdout, "up to date") {
		t.Errorf("stdout = %q, want it to NOT report up to date -- migrate status must not migrate first", stdout)
	}

	store, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	defer func() { _ = store.Close() }()

	statuses, err := store.MigrationStatuses(context.Background())
	if err != nil {
		t.Fatalf("MigrationStatuses: %v", err)
	}
	pending := 0
	for _, s := range statuses {
		if s.Pending {
			pending++
		}
	}
	if pending == 0 {
		t.Error("pending migrations = 0 after `migrate status`, want > 0 (it must leave the schema untouched)")
	}
}

// TestMigrateUpStillMigrates guards against a fix that skips auto-migrate
// too broadly: every subcommand other than `migrate status` -- including
// `migrate up` itself -- must still run against a migrated schema.
func TestMigrateUpStillMigrates(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "test.db")

	stdout, _, err := cli(t, dbPath, "migrate", "up")
	if err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	if !strings.Contains(stdout, "up to date") {
		t.Errorf("stdout = %q, want it to report up to date after migrating", stdout)
	}

	statusOut, _, err := cli(t, dbPath, "migrate", "status")
	if err != nil {
		t.Fatalf("migrate status: %v", err)
	}
	if !strings.Contains(statusOut, "up to date") {
		t.Errorf("stdout = %q, want up to date after a prior `migrate up`", statusOut)
	}
}

// TestRegionSetRejectsEmptyTimezone and TestRegionSetRejectsLocalTimezone
// assert that region set rejects "" and "Local" explicitly. Both make
// time.LoadLocation return a nil error, so neither was caught by the
// pre-existing LoadLocation check: "" would silently blank a configured
// timezone, and "Local" resolves to whatever zone the invoking machine
// happens to have -- exactly the machine-local dependence this design bans
// everywhere else.
func TestRegionSetRejectsEmptyTimezone(t *testing.T) {
	t.Parallel()
	dbPath, store := newDB(t)
	seedRegion(t, store.Regions(), 1)

	_, _, err := cli(t, dbPath, "region", "set", "--id", "1", "--timezone", "")
	if err == nil {
		t.Fatal("region set --timezone \"\": want error, got nil")
	}

	got, gerr := store.Regions().Get(context.Background(), 1)
	if gerr != nil {
		t.Fatalf("Get: %v", gerr)
	}
	if got.Timezone != "UTC" {
		t.Errorf("Timezone = %q, want unchanged default %q (rejection must write nothing)", got.Timezone, "UTC")
	}
}

func TestRegionSetRejectsLocalTimezone(t *testing.T) {
	t.Parallel()
	dbPath, store := newDB(t)
	seedRegion(t, store.Regions(), 1)

	_, _, err := cli(t, dbPath, "region", "set", "--id", "1", "--timezone", "Local")
	if err == nil {
		t.Fatal("region set --timezone Local: want error, got nil")
	}

	got, gerr := store.Regions().Get(context.Background(), 1)
	if gerr != nil {
		t.Fatalf("Get: %v", gerr)
	}
	if got.Timezone != "UTC" {
		t.Errorf("Timezone = %q, want unchanged default %q (rejection must write nothing)", got.Timezone, "UTC")
	}
}

// erroringRegionRepo satisfies regions.Repository but fails every Get with
// something other than regions.ErrNotFound, letting a test exercise
// alertShow's "surface anything else" branch without needing a genuinely
// corrupt database.
type erroringRegionRepo struct {
	regions.Repository
}

var errRegionLookupBroken = errors.New("regions: simulated corrupt row")

func (erroringRegionRepo) Get(context.Context, int64) (regions.Region, error) {
	return regions.Region{}, errRegionLookupBroken
}

// notFoundRegionRepo satisfies regions.Repository but always reports
// regions.ErrNotFound, for asserting the other side of the same branch: a
// genuinely missing region is still tolerated.
type notFoundRegionRepo struct {
	regions.Repository
}

func (notFoundRegionRepo) Get(context.Context, int64) (regions.Region, error) {
	return regions.Region{}, regions.ErrNotFound
}

type fakeAlertShowStore struct {
	alertsRepo  alerts.Repository
	regionsRepo regions.Repository
}

func (f fakeAlertShowStore) Alerts() alerts.Repository   { return f.alertsRepo }
func (f fakeAlertShowStore) Regions() regions.Repository { return f.regionsRepo }

// TestAlertShowSurfacesNonNotFoundRegionError reproduces the finding that
// alertShow treated every region lookup failure -- a corrupt database, a
// cancelled context, not just a missing region -- identically: print the
// alert in UTC and exit 0. Anything other than regions.ErrNotFound must be
// surfaced, not swallowed.
func TestAlertShowSurfacesNonNotFoundRegionError(t *testing.T) {
	t.Parallel()
	_, store := newDB(t)
	seedRegion(t, store.Regions(), 1)

	created, err := store.Alerts().Create(context.Background(), alerts.NewAlert{
		RegionID: 1, AgencyID: "40", HeaderText: "H",
		Cause: "UNKNOWN_CAUSE", Effect: "UNKNOWN_EFFECT", Severity: "WARNING",
		StartTime: time.Date(2026, 8, 15, 14, 0, 0, 0, time.UTC),
	}, time.Now())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	fake := fakeAlertShowStore{alertsRepo: store.Alerts(), regionsRepo: erroringRegionRepo{}}

	var stdout bytes.Buffer
	err = alertShow(context.Background(), &stdout, fake, []string{strconv.FormatInt(created.ID, 10)})
	if err == nil {
		t.Fatal("alertShow with a non-ErrNotFound region lookup error: want error, got nil")
	}
	if !errors.Is(err, errRegionLookupBroken) {
		t.Errorf("error = %v, want it to wrap the underlying region lookup error", err)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want nothing written (exit 0 with UTC output would hide the real failure)", stdout.String())
	}
}

// TestAlertShowToleratesUnknownRegion is the other side of the same branch:
// a genuinely missing region must still degrade to UTC-only output rather
// than becoming an error.
func TestAlertShowToleratesUnknownRegion(t *testing.T) {
	t.Parallel()
	_, store := newDB(t)
	seedRegion(t, store.Regions(), 1)

	created, err := store.Alerts().Create(context.Background(), alerts.NewAlert{
		RegionID: 1, AgencyID: "40", HeaderText: "H",
		Cause: "UNKNOWN_CAUSE", Effect: "UNKNOWN_EFFECT", Severity: "WARNING",
		StartTime: time.Date(2026, 8, 15, 14, 0, 0, 0, time.UTC),
	}, time.Now())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	fake := fakeAlertShowStore{alertsRepo: store.Alerts(), regionsRepo: notFoundRegionRepo{}}

	var stdout bytes.Buffer
	if err := alertShow(context.Background(), &stdout, fake, []string{strconv.FormatInt(created.ID, 10)}); err != nil {
		t.Fatalf("alertShow with an unknown region: %v, want no error", err)
	}
	if !strings.Contains(stdout.String(), "id: ") {
		t.Errorf("stdout = %q, want the alert printed despite the missing region", stdout.String())
	}
}

// TestParseInstantWrapsUnderlyingError reproduces the finding that
// parseInstant checked time.Parse's error but never wrapped it, so a
// truncated offset and an out-of-range month produced the identical generic
// message -- indistinguishable to whoever is debugging a rejected --start.
func TestParseInstantWrapsUnderlyingError(t *testing.T) {
	t.Parallel()
	reg := regions.Region{ID: 1, Timezone: "America/Los_Angeles"}

	_, truncatedErr := parseInstant("2026-08-15T14:00:00-07:0", reg)
	_, monthErr := parseInstant("2026-13-15T14:00:00-07:00", reg)

	if truncatedErr == nil || monthErr == nil {
		t.Fatalf("want both malformed inputs to fail; got %v / %v", truncatedErr, monthErr)
	}
	if truncatedErr.Error() == monthErr.Error() {
		t.Errorf("truncated-offset and out-of-range-month errors are identical (%q); want the underlying time.Parse detail wrapped in with %%w so they're distinguishable", truncatedErr.Error())
	}
	for _, err := range []error{truncatedErr, monthErr} {
		if !strings.Contains(err.Error(), "RFC 3339") {
			t.Errorf("error = %q, want it to still mention the RFC 3339 guidance", err.Error())
		}
		if !strings.Contains(err.Error(), "America/Los_Angeles") {
			t.Errorf("error = %q, want it to still mention the region's configured timezone", err.Error())
		}
	}
}
