package sqlite_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/alerts"
	"github.com/OneBusAway/sidecar/internal/regions"
	"github.com/OneBusAway/sidecar/internal/store/sqlite"
)

func TestOpenMigrateAndRoundTrip(t *testing.T) {
	t.Parallel()

	store, openErr := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if openErr != nil {
		t.Fatalf("Open: %v", openErr)
	}
	t.Cleanup(func() { _ = store.Close() })

	if err := store.Migrate(); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	ctx := context.Background()
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)

	if err := store.Regions().UpsertFromDirectory(ctx, []regions.Region{{
		ID: 1, Name: "Puget Sound", OBABaseURL: "https://api.example.org/", Active: true,
	}}, now); err != nil {
		t.Fatalf("UpsertFromDirectory: %v", err)
	}

	got, getErr := store.Regions().Get(ctx, 1)
	if getErr != nil {
		t.Fatalf("Get: %v", getErr)
	}
	if got.Name != "Puget Sound" {
		t.Errorf("Name = %q, want Puget Sound", got.Name)
	}

	if _, err := store.Regions().Get(ctx, 999); !errors.Is(err, regions.ErrNotFound) {
		t.Errorf("Get(999) error = %v, want regions.ErrNotFound", err)
	}
	if _, err := store.Alerts().Get(ctx, 999); !errors.Is(err, alerts.ErrNotFound) {
		t.Errorf("Get(999) error = %v, want alerts.ErrNotFound", err)
	}
}

// TestFeed exercises Feed through the generated Go bindings rather than by
// hand-running SQL: an earlier review verified the FeedAlerts/FeedTranslations
// SQL by hand and missed that a bare `LIMIT ?` mixed with an
// explicitly-numbered sqlc.arg was numbered as a fourth, unfilled
// placeholder, so every real Feed call failed with "missing argument with
// index 4" even though the hand-run SQL was correct.
func TestFeed(t *testing.T) {
	t.Parallel()

	store, openErr := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if openErr != nil {
		t.Fatalf("Open: %v", openErr)
	}
	t.Cleanup(func() { _ = store.Close() })

	if err := store.Migrate(); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	ctx := context.Background()
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)

	if err := store.Regions().UpsertFromDirectory(ctx, []regions.Region{{
		ID: 1, Name: "Puget Sound", OBABaseURL: "https://api.example.org/", Active: true,
	}}, now); err != nil {
		t.Fatalf("UpsertFromDirectory: %v", err)
	}

	repo := store.Alerts()

	first, createErr := repo.Create(ctx, alerts.NewAlert{
		RegionID: 1, AgencyID: "40", HeaderText: "First alert",
		Cause: "UNKNOWN_CAUSE", Effect: "UNKNOWN_EFFECT", Severity: "WARNING",
		StartTime: now,
	}, now)
	if createErr != nil {
		t.Fatalf("Create(first): %v", createErr)
	}
	if err := repo.SetPublished(ctx, first.ID, true, now); err != nil {
		t.Fatalf("SetPublished(first): %v", err)
	}
	if err := repo.UpsertTranslation(ctx, first.ID, alerts.Translation{
		Language: "es", Field: alerts.FieldHeader, Text: "Primera alerta",
		SourceSHA256: alerts.SourceHash("First alert"),
	}, now); err != nil {
		t.Fatalf("UpsertTranslation(first): %v", err)
	}

	later := now.Add(time.Hour)
	second, createErr := repo.Create(ctx, alerts.NewAlert{
		RegionID: 1, AgencyID: "40", HeaderText: "Second alert",
		Cause: "UNKNOWN_CAUSE", Effect: "UNKNOWN_EFFECT", Severity: "WARNING",
		StartTime: later,
	}, later)
	if createErr != nil {
		t.Fatalf("Create(second): %v", createErr)
	}
	if err := repo.SetPublished(ctx, second.ID, true, later); err != nil {
		t.Fatalf("SetPublished(second): %v", err)
	}
	if err := repo.UpsertTranslation(ctx, second.ID, alerts.Translation{
		Language: "es", Field: alerts.FieldHeader, Text: "Segunda alerta",
		SourceSHA256: alerts.SourceHash("Second alert"),
	}, later); err != nil {
		t.Fatalf("UpsertTranslation(second): %v", err)
	}

	feed, feedErr := repo.Feed(ctx, 1, false, 10)
	if feedErr != nil {
		t.Fatalf("Feed: %v", feedErr)
	}
	if len(feed) != 2 {
		t.Fatalf("len(feed) = %d, want 2", len(feed))
	}

	// Newest first: second alert (later StartTime) precedes first.
	if feed[0].ID != second.ID {
		t.Errorf("feed[0].ID = %d, want %d (second, newest first)", feed[0].ID, second.ID)
	}
	if feed[1].ID != first.ID {
		t.Errorf("feed[1].ID = %d, want %d (first)", feed[1].ID, first.ID)
	}

	if len(feed[0].Translations) != 1 || feed[0].Translations[0].Text != "Segunda alerta" {
		t.Errorf("feed[0].Translations = %+v, want [{...Text: Segunda alerta}]", feed[0].Translations)
	}
	if len(feed[1].Translations) != 1 || feed[1].Translations[0].Text != "Primera alerta" {
		t.Errorf("feed[1].Translations = %+v, want [{...Text: Primera alerta}]", feed[1].Translations)
	}
}
