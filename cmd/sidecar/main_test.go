package main

import (
	"bytes"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/httpapi"
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
	deps := buildDeps(store, slog.New(slog.DiscardHandler), "", "")

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
	deps := buildDeps(store, slog.New(slog.DiscardHandler), "", "")

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

// TestBuildDeps_WiresOBADefaultKeySet pins the one line in buildDeps nothing
// else exercises: httpapi's own tests inject OBADefaultKeySet directly on
// Deps, so a binary that deleted this assignment (or hard-coded it to
// false/true) would still pass every httpapi test and every other test in
// this file. This is what lets a region relying on the process-default key
// report "default" instead of "none" to an operator.
func TestBuildDeps_WiresOBADefaultKeySet(t *testing.T) {
	t.Parallel()

	t.Run("set when a key is configured", func(t *testing.T) {
		t.Parallel()
		store := sqlitetest.Open(t)
		deps := buildDeps(store, slog.New(slog.DiscardHandler), "some-key", "")
		if !deps.OBADefaultKeySet {
			t.Error("Deps.OBADefaultKeySet = false, want true when --oba-api-key is non-empty")
		}
	})

	t.Run("unset when no key is configured", func(t *testing.T) {
		t.Parallel()
		store := sqlitetest.Open(t)
		deps := buildDeps(store, slog.New(slog.DiscardHandler), "", "")
		if deps.OBADefaultKeySet {
			t.Error("Deps.OBADefaultKeySet = true, want false when --oba-api-key is empty")
		}
	})
}

// TestBuildDeps_WiresVehicles covers the fields Task 5 adds. Deps.Vehicles
// must always be set, even with an empty key -- a feed-only region with its
// own OBAAPIKey still needs to search -- and an empty key must be flagged at
// startup, on the theory that a silently-broken vehicle search (every region
// with no key of its own gets 502 forever) is far harder to diagnose in
// production than a boot-time log line.
func TestBuildDeps_WiresVehicles(t *testing.T) {
	t.Parallel()

	t.Run("Vehicles is always wired", func(t *testing.T) {
		t.Parallel()
		store := sqlitetest.Open(t)
		deps := buildDeps(store, slog.New(slog.DiscardHandler), "some-key", "")
		if deps.Vehicles == nil {
			t.Fatal("Deps.Vehicles = nil, want a *vehicles.Service")
		}
	})

	t.Run("empty key logs a warning", func(t *testing.T) {
		t.Parallel()
		store := sqlitetest.Open(t)
		var buf bytes.Buffer
		buildDeps(store, slog.New(slog.NewTextHandler(&buf, nil)), "", "")
		if !strings.Contains(buf.String(), "oba-api-key") {
			t.Errorf("log output = %q, want a warning mentioning oba-api-key", buf.String())
		}
	})

	t.Run("non-empty key logs no warning", func(t *testing.T) {
		t.Parallel()
		store := sqlitetest.Open(t)
		var buf bytes.Buffer
		// A real pirate key too, so the weather warning this task adds can't
		// contaminate an assertion scoped to the OBA-key warning.
		buildDeps(store, slog.New(slog.NewTextHandler(&buf, nil)), "a-real-key", "a-real-pirate-key")
		if buf.Len() != 0 {
			t.Errorf("log output = %q, want nothing logged when a key is configured", buf.String())
		}
	})
}

// TestBuildDeps_WiresWeather covers the fields Task 7 adds. Deps.Weather must
// always be set, even with an empty Pirate Weather key -- weather.Service
// turns a nil provider into ErrNoProvider itself, so the handler still needs
// wiring to answer 403 rather than never being registered at all -- and an
// empty key must be flagged at startup the same way a missing OBA key is,
// since a silently-403 weather endpoint is far harder to diagnose in
// production than a boot-time log line.
func TestBuildDeps_WiresWeather(t *testing.T) {
	t.Parallel()

	t.Run("Weather is always wired", func(t *testing.T) {
		t.Parallel()
		store := sqlitetest.Open(t)
		deps := buildDeps(store, slog.New(slog.DiscardHandler), "some-key", "some-pirate-key")
		if deps.Weather == nil {
			t.Fatal("Deps.Weather = nil, want a *weather.Service")
		}
	})

	t.Run("empty key logs a warning", func(t *testing.T) {
		t.Parallel()
		store := sqlitetest.Open(t)
		var buf bytes.Buffer
		buildDeps(store, slog.New(slog.NewTextHandler(&buf, nil)), "some-key", "")
		if !strings.Contains(buf.String(), "pirate-weather-key") {
			t.Errorf("log output = %q, want a warning mentioning pirate-weather-key", buf.String())
		}
	})

	t.Run("non-empty key logs no warning", func(t *testing.T) {
		t.Parallel()
		store := sqlitetest.Open(t)
		var buf bytes.Buffer
		buildDeps(store, slog.New(slog.NewTextHandler(&buf, nil)), "some-key", "a-real-pirate-key")
		if strings.Contains(buf.String(), "pirate-weather-key") {
			t.Errorf("log output = %q, want no pirate-weather-key warning when a key is configured", buf.String())
		}
	})
}

// TestCacheBudgetsNestCorrectly pins the ordering the comment above the
// budget constants asserts but nothing previously checked: a cold query
// fetch nests a fleet fetch inside it (vehicles.Service.Search calls
// s.fleet.Get from within s.result.Get's fetch callback), so the fleet
// fetch must be able to finish inside the query fetch's own budget. If
// fleetBudget ever meets or exceeds queryBudget, the outer fetch can give up
// while the inner one -- still within its own budget -- is still running.
func TestCacheBudgetsNestCorrectly(t *testing.T) {
	t.Parallel()

	if fleetBudget >= queryBudget {
		t.Errorf("fleetBudget (%v) must be < queryBudget (%v): a cold query fetch nests a fleet fetch inside it",
			fleetBudget, queryBudget)
	}
}

// TestCacheBudgetsUnderWriteTimeout pins the other half of the claim the
// comment above the budget constants makes ("the budgets sit under the
// server's 15s WriteTimeout") but that, before this test, nothing checked:
// a fetch that runs past WriteTimeout can't finish writing its response
// even if it eventually succeeds, so every cache budget in this package must
// stay strictly under whatever httpapi.NewServer actually configures. Reading
// WriteTimeout off a real *http.Server, rather than repeating the literal
// 15*time.Second here, means a change to NewServer's timeout is what this
// test tracks -- not a copy of it that could drift out of sync.
func TestCacheBudgetsUnderWriteTimeout(t *testing.T) {
	t.Parallel()

	writeTimeout := httpapi.NewServer(httpapi.ServerConfig{}).WriteTimeout
	for _, b := range []struct {
		name   string
		budget time.Duration
	}{
		{"fleetBudget", fleetBudget},
		{"queryBudget", queryBudget},
		{"weatherBudget", weatherBudget},
	} {
		if b.budget >= writeTimeout {
			t.Errorf("%s (%v) must be < the server's WriteTimeout (%v)", b.name, b.budget, writeTimeout)
		}
	}
}
