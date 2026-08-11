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
