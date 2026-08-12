// Package sqlitetest collapses the Open-Migrate-Cleanup boilerplate that
// tests across the repo repeat every time they need a real, freshly
// migrated SQLite-backed store.
//
// This is deliberately separate from internal/store/storetest: that package
// stays engine-agnostic (it takes a newStore callback and must not import
// any specific adapter) so a future Postgres adapter can import it
// unmodified. A helper that imports the sqlite adapter directly, like this
// one, cannot live there.
package sqlitetest

import (
	"path/filepath"
	"testing"

	"github.com/OneBusAway/sidecar/internal/store/sqlite"
)

// OpenAt returns the file path and a freshly opened, migrated *sqlite.Store
// backed by that path, closed automatically via t.Cleanup. It exists
// alongside Open for tests that need to open a second, independent
// connection to the same database file -- the sidecar-admin CLI tests do
// this, opening their own handle through the CLI's --db flag while also
// holding a handle for fixture setup and assertions.
func OpenAt(t *testing.T) (string, *sqlite.Store) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	store, err := sqlite.Open(path)
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return path, store
}

// Open returns a freshly opened, migrated *sqlite.Store backed by a file in
// t.TempDir(), closed automatically via t.Cleanup.
func Open(t *testing.T) *sqlite.Store {
	t.Helper()
	_, store := OpenAt(t)
	return store
}
