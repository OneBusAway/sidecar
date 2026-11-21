package sqlite_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/OneBusAway/sidecar/internal/store/sqlite"
)

// TestOpen_FileModesOwnerOnly (SC-16): the database holds password hashes,
// session and API-key digests, OBA keys, push tokens, and rider data, so
// the file and its -wal and -shm siblings are owner-only whatever the
// process umask -- both when Open creates them and when an older
// deployment left them 0644.
func TestOpen_FileModesOwnerOnly(t *testing.T) {
	t.Parallel()
	check := func(t *testing.T, path string) {
		t.Helper()
		for _, p := range []string{path, path + "-wal", path + "-shm"} {
			fi, err := os.Stat(p)
			if err != nil {
				t.Fatalf("stat %s: %v", filepath.Base(p), err)
			}
			if mode := fi.Mode().Perm(); mode&0o077 != 0 {
				t.Errorf("%s mode %o; want no group/other bits", filepath.Base(p), mode)
			}
		}
	}
	open := func(t *testing.T, path string) *sqlite.Store {
		t.Helper()
		store, err := sqlite.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close() })
		if err := store.Migrate(); err != nil {
			t.Fatal(err)
		}
		// A write keeps the WAL and shared-memory files in place while
		// the pool holds a connection.
		if _, err := store.Sequences(context.Background()); err != nil {
			t.Fatal(err)
		}
		return store
	}

	t.Run("fresh", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "fresh.db")
		open(t, path)
		check(t, path)
	})

	t.Run("existing 0644", func(t *testing.T) {
		t.Parallel()
		path := filepath.Join(t.TempDir(), "old.db")
		first := open(t, path)
		if err := first.Close(); err != nil {
			t.Fatal(err)
		}
		for _, p := range []string{path, path + "-wal", path + "-shm"} {
			if _, err := os.Stat(p); err == nil {
				if err := os.Chmod(p, 0o644); err != nil {
					t.Fatal(err)
				}
			}
		}
		open(t, path)
		check(t, path)
	})
}
