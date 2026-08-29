package sqlite_test

import (
	"context"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/alerts"
	"github.com/OneBusAway/sidecar/internal/regions"
	"github.com/OneBusAway/sidecar/internal/store/sqlite"
	"github.com/OneBusAway/sidecar/internal/store/sqlitetest"
)

// TestBumpSequences: the next id minted after a bump is above the floor,
// a lower bump is a no-op, and a table that has never had a row still gets
// its floor (sqlite_sequence has no row for it until then).
func TestBumpSequences(t *testing.T) {
	t.Parallel()
	store := sqlitetest.Open(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	if err := store.Regions().UpsertFromDirectory(ctx, []regions.Region{{ID: 1, Name: "R", OBABaseURL: "https://r.example/", Active: true}}, now); err != nil {
		t.Fatal(err)
	}
	newAlert := func(t *testing.T) int64 {
		t.Helper()
		a, err := store.Alerts().Create(ctx, alerts.NewAlert{RegionID: 1, AgencyID: "1", HeaderText: "h", StartTime: now}, now)
		if err != nil {
			t.Fatal(err)
		}
		return a.ID
	}
	first := newAlert(t) // 1

	before, err := store.Sequences(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if before["alerts"] != first || before["studies"] != 0 {
		t.Fatalf("Sequences before = %v", before)
	}

	const floor = 1_000_000
	prev, err := store.BumpSequences(ctx, floor)
	if err != nil {
		t.Fatalf("BumpSequences: %v", err)
	}
	if prev["alerts"] != first || prev["survey_questions"] != 0 {
		t.Errorf("previous values = %v", prev)
	}
	if got := newAlert(t); got != floor+1 {
		t.Errorf("next alert id = %d, want %d", got, floor+1)
	}

	after, err := store.Sequences(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range sqlite.SequenceTables {
		if after[name] < floor {
			t.Errorf("%s seq = %d, want >= %d", name, after[name], floor)
		}
	}

	// A lower floor changes nothing.
	if _, err := store.BumpSequences(ctx, 10); err != nil {
		t.Fatal(err)
	}
	if got := newAlert(t); got != floor+2 {
		t.Errorf("after a lower bump, next alert id = %d, want %d", got, floor+2)
	}
}
