package main

import (
	"bytes"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/store/sqlitetest"
)

// TestRun_ArgHandling covers flag parsing outcomes that don't require
// standing up the server: help, and malformed flag values. The behaviour of
// a successfully-parsed run (open, migrate, serve, shut down) lives in the
// packages it delegates to (internal/store/sqlite, internal/regions,
// internal/httpapi), so it isn't re-tested here — this keeps the table
// thin instead of re-binding a real port.
func TestRun_ArgHandling(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		args          []string
		wantErr       bool
		wantStdout    string // substring expected in stdout
		wantStdoutLen int    // if non-negative, an exact expected length (used to assert "nothing written")
	}{
		{
			name:          "help flag writes usage and returns nil",
			args:          []string{"--help"},
			wantErr:       false,
			wantStdout:    "Usage of sidecar",
			wantStdoutLen: -1,
		},
		{
			name:          "unparseable refresh duration returns an error and writes nothing to stdout",
			args:          []string{"--refresh=nonsense"},
			wantErr:       true,
			wantStdoutLen: 0,
		},
		{
			// time.NewTicker panics on a duration <= 0. --refresh=0 is a
			// natural way to try to disable the sync loop; it must be
			// rejected here, cleanly, rather than reaching RunSyncLoop's
			// goroutine (which has no recover) and taking the whole
			// process -- including the already-serving HTTP server -- down
			// with it.
			name:          "non-positive refresh (zero) returns an error and writes nothing to stdout",
			args:          []string{"--refresh=0"},
			wantErr:       true,
			wantStdoutLen: 0,
		},
		{
			name:          "non-positive refresh (negative) returns an error and writes nothing to stdout",
			args:          []string{"--refresh=-1h"},
			wantErr:       true,
			wantStdoutLen: 0,
		},
		{
			name:          "unknown flag returns an error and writes nothing to stdout",
			args:          []string{"--bogus-flag"},
			wantErr:       true,
			wantStdoutLen: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var stdout, stderr bytes.Buffer
			err := run(&stdout, &stderr, tt.args)

			if tt.wantErr && err == nil {
				t.Fatalf("run(%v) returned nil error, want non-nil", tt.args)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("run(%v) returned %v, want nil", tt.args, err)
			}
			if tt.wantStdoutLen == 0 && stdout.Len() != 0 {
				t.Errorf("stdout = %q, want empty", stdout.String())
			}
			if tt.wantStdout != "" && !strings.Contains(stdout.String(), tt.wantStdout) {
				t.Errorf("stdout = %q, want substring %q", stdout.String(), tt.wantStdout)
			}
		})
	}
}

// TestRun_MigrationFailure points --db at a path inside a directory that
// does not exist. sqlite.Open succeeds (the connection is opened lazily),
// but store.Migrate() fails when it actually tries to touch the database
// file, and that failure must propagate out of run as a non-nil error
// rather than falling through to start serving on an unknown schema.
func TestRun_MigrationFailure(t *testing.T) {
	t.Parallel()

	badPath := filepath.Join(t.TempDir(), "does-not-exist", "sidecar.db")

	var stdout, stderr bytes.Buffer
	err := run(&stdout, &stderr, []string{"--db", badPath})
	if err == nil {
		t.Fatalf("run() with unmigratable --db returned nil error, want non-nil")
	}
}

// TestBuildDeps_WiresFailDelay pins the one value httpapi.NewRouter will
// never catch for us: Deps.FailDelay is not defaulted there (unlike Logger,
// Sleep, and VerifyPassword), so a binary that forgot to set it would wire
// up Sleep(0) -- the brake on online login guessing (design spec §4.3)
// silently absent, with no panic and no other failing test. buildDeps is
// exercised directly, against a real migrated store, rather than through
// run/serve, because asserting on the constructed Deps needs no listening
// socket and no signal handling.
func TestBuildDeps_WiresFailDelay(t *testing.T) {
	t.Parallel()

	store := sqlitetest.Open(t)
	deps := buildDeps(store, slog.New(slog.DiscardHandler))

	if deps.FailDelay != 500*time.Millisecond {
		t.Errorf("Deps.FailDelay = %v, want 500ms", deps.FailDelay)
	}
}

// TestBuildDeps_WiresAdminSurface checks the fields Task 7 adds: without
// Auth or AdminUI, httpapi.NewRouter either panics (Auth set, admin deps
// missing) or never registers the admin surface at all (AdminUI nil); and
// Now must be the real wall clock -- forbidigo does not catch a wrong clock
// here, since .golangci.yml explicitly excludes the time.Now/time.Local
// rule for path '^cmd/' (rightly so: cmd/ is the one place allowed to read
// it). A clock pinned to some fixed instant would compile and lint clean
// while minting every session already-expired, so Now is checked by bounds
// rather than by nil-ness: two func values can never be compared with ==,
// and a nil check alone would let a wrong-but-non-nil clock through.
// None of these failure modes would be visible from run's own tests, which
// never reach a request handler.
func TestBuildDeps_WiresAdminSurface(t *testing.T) {
	t.Parallel()

	store := sqlitetest.Open(t)
	deps := buildDeps(store, slog.New(slog.DiscardHandler))

	if deps.Auth == nil {
		t.Error("Deps.Auth = nil, want store.Auth()")
	}
	if deps.AdminUI == nil {
		t.Error("Deps.AdminUI = nil, want adminui.FS()")
	}
	if deps.Now == nil {
		t.Fatal("Deps.Now = nil, want time.Now")
	}
	before := time.Now()
	got := deps.Now()
	after := time.Now()
	if got.Before(before) || got.After(after) {
		t.Errorf("Deps.Now() = %v, want a value between %v and %v (i.e. the real wall clock)", got, before, after)
	}
}
