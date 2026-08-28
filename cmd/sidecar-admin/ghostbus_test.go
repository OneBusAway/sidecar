package main

import (
	"context"
	"encoding/csv"
	"strings"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/ghostbus"
	"github.com/OneBusAway/sidecar/internal/regions"
)

// seedGhostBusRegion inserts a directory-sourced region row and then sets its
// timezone through the same repository path `region set` uses -- ghost bus
// export needs a real IANA zone to render reported_at_local/service_date.
func seedGhostBusRegion(t *testing.T, repo regions.Repository, id int64, tz string) {
	t.Helper()
	seedRegion(t, repo, id)
	if err := repo.SetLocalFields(context.Background(), id, regions.LocalFields{Timezone: tz}, time.Now()); err != nil {
		t.Fatalf("SetLocalFields(%d): %v", id, err)
	}
}

// TestGhostBusExportCSV is the end-to-end CLI test: flag parsing, region
// resolution, and store lookup wire correctly into ghostbus.WriteReportsCSV.
// The CSV's own column contract (row rendering, injection guards, the
// staleness divisor, the malformed-snapshot fallback) is now pinned in
// internal/ghostbus/csv_test.go, against WriteReportsCSV directly -- this
// test only has to prove the CLI reaches it with the right arguments.
func TestGhostBusExportCSV(t *testing.T) {
	t.Parallel()
	dbPath, store := newDB(t)
	seedGhostBusRegion(t, store.Regions(), 1, "America/Los_Angeles")

	created := time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC)
	r1, err := store.GhostBus().Create(context.Background(), ghostbus.NewReport{
		RegionID:            1,
		PublicID:            "tok_full_000000000001",
		UserIdentifier:      "user-a",
		TripIdentifier:      "trip-common",
		ServiceDate:         created.UnixMilli(),
		WaitDurationMinutes: 15,
	}, created)
	if err != nil {
		t.Fatalf("create report: %v", err)
	}
	createdLate := created.Add(2 * time.Hour)
	if _, secondErr := store.GhostBus().Create(context.Background(), ghostbus.NewReport{
		RegionID:            1,
		PublicID:            "tok_late_000000000001",
		UserIdentifier:      "user-b",
		TripIdentifier:      "trip-other",
		ServiceDate:         createdLate.UnixMilli(),
		WaitDurationMinutes: 10,
	}, createdLate); secondErr != nil {
		t.Fatalf("create second report: %v", secondErr)
	}

	stdout, _, err := cli(t, dbPath, "ghostbus", "export", "--region", "1")
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	rows, err := csv.NewReader(strings.NewReader(stdout)).ReadAll()
	if err != nil {
		t.Fatalf("parse csv: %v (stdout=%q)", err, stdout)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3 (header + 2 reports); stdout=%q", len(rows), stdout)
	}
	if rows[0][0] != "public_identifier" {
		t.Fatalf("header[0] = %q, want public_identifier", rows[0][0])
	}
	var gotIDs []string
	for _, row := range rows[1:] {
		gotIDs = append(gotIDs, row[0])
	}
	if len(gotIDs) != 2 || gotIDs[0] != r1.PublicID {
		t.Errorf("public ids = %v, want [%s tok_late_000000000001]", gotIDs, r1.PublicID)
	}

	// --since between the two reports' created_at: only the later one
	// exported. This exercises the CLI's own --since -> Unix() wiring,
	// distinct from ListForExport's own boundary semantics.
	since := created.Add(time.Hour).Format(time.RFC3339)
	stdoutSince, _, err := cli(t, dbPath, "ghostbus", "export", "--region", "1", "--since", since)
	if err != nil {
		t.Fatalf("export --since: %v", err)
	}
	rowsSince, err := csv.NewReader(strings.NewReader(stdoutSince)).ReadAll()
	if err != nil {
		t.Fatalf("parse csv: %v", err)
	}
	if len(rowsSince) != 2 || rowsSince[1][0] != "tok_late_000000000001" {
		t.Fatalf("--since rows = %v, want header + the later report only", rowsSince)
	}

	// Unknown region: run() must return an error mentioning the region.
	if _, _, err := cli(t, dbPath, "ghostbus", "export", "--region", "999"); err == nil || !strings.Contains(err.Error(), "region") {
		t.Errorf("unknown region err = %v, want an error mentioning region", err)
	}
}

// TestGhostBusExportRejectsNaiveSince pins design §2.8/§5: --since is an
// instant, and (like every other CLI timestamp in this repo) a naive
// datetime with no UTC offset must be rejected rather than silently
// interpreted in some default zone.
func TestGhostBusExportRejectsNaiveSince(t *testing.T) {
	t.Parallel()
	dbPath, store := newDB(t)
	seedGhostBusRegion(t, store.Regions(), 1, "America/Los_Angeles")

	_, _, err := cli(t, dbPath, "ghostbus", "export", "--region", "1", "--since", "2026-09-01T00:00:00")
	if err == nil || !strings.Contains(err.Error(), "explicit UTC offset") {
		t.Fatalf("naive --since err = %v, want an explicit UTC offset error", err)
	}
}

// TestGhostBusExportMissingRegionFlag pins that a missing (or explicit
// zero) --region is a usage error, not a "region 0: not found" store
// lookup -- --since alone must not satisfy the flag-parsing check that
// used to let this fall through to the store.
func TestGhostBusExportMissingRegionFlag(t *testing.T) {
	t.Parallel()
	dbPath, store := newDB(t)
	seedGhostBusRegion(t, store.Regions(), 1, "America/Los_Angeles")

	const wantUsage = "usage: ghostbus export --region N [--since RFC3339]"

	if _, _, err := cli(t, dbPath, "ghostbus", "export"); err == nil || err.Error() != wantUsage {
		t.Errorf("no flags err = %v, want %q", err, wantUsage)
	}
	if _, _, err := cli(t, dbPath, "ghostbus", "export", "--since", "2026-09-01T00:00:00Z"); err == nil || err.Error() != wantUsage {
		t.Errorf("--since with no --region err = %v, want %q", err, wantUsage)
	}
	// flag.Parse stops at the first non-flag argument, so without the NArg
	// guard a trailing positional (a typo, a misplaced flag) would be
	// silently ignored and the export would run anyway.
	if _, _, err := cli(t, dbPath, "ghostbus", "export", "--region", "1", "unexpected"); err == nil || err.Error() != wantUsage {
		t.Errorf("trailing argument err = %v, want %q", err, wantUsage)
	}
	if _, _, err := cli(t, dbPath, "ghostbus", "export", "--region", "0"); err == nil || err.Error() != wantUsage {
		t.Errorf("--region 0 err = %v, want %q", err, wantUsage)
	}

	// A nonzero, nonexistent region must still surface the store's
	// not-found error, not the usage message.
	if _, _, err := cli(t, dbPath, "ghostbus", "export", "--region", "999"); err == nil || !strings.Contains(err.Error(), "region") {
		t.Errorf("unknown region err = %v, want an error mentioning region", err)
	}
}
