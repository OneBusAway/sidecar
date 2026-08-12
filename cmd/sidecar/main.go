// Command sidecar is the OneBusAway sidecar server.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	// time/tzdata embeds the IANA timezone database into the binary, so
	// time.LoadLocation works in a scratch container with no system tzdata
	// -- PATCH /api/admin/v1/regions/{id} depends on it to reject a bad
	// timezone at the point of the mistake, and would otherwise reject every
	// valid one on such a host.
	_ "time/tzdata"

	"github.com/OneBusAway/sidecar/internal/httpapi"
	"github.com/OneBusAway/sidecar/internal/httpapi/adminui"
	"github.com/OneBusAway/sidecar/internal/regions"
	"github.com/OneBusAway/sidecar/internal/store/sqlite"
)

const (
	defaultDB         = "./sidecar.db"
	defaultAddr       = ":8080"
	defaultRegionsURL = "https://regions.onebusaway.org/regions-v3.json"
	defaultRefresh    = 60 * time.Minute

	// shutdownTimeout bounds how long a graceful shutdown waits for
	// in-flight requests to finish before giving up.
	shutdownTimeout = 10 * time.Second

	// adminFailDelay is the constant pause httpapi.Deps.FailDelay applies to
	// every failed login: a brake on online password guessing (design spec
	// §4.3). httpapi.NewRouter does not default this field -- an omission
	// here would silently disable the brake with no panic and no failing
	// test, so it is named and tested (see TestBuildDeps_WiresFailDelay)
	// rather than inlined into the Deps literal below.
	adminFailDelay = 500 * time.Millisecond
)

func main() {
	if err := run(os.Stdout, os.Stderr, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "sidecar:", err)
		os.Exit(1)
	}
}

// run holds main's logic so tests can supply their own streams and
// arguments. It returns an error rather than exiting so main owns the only
// exit path.
//
// Flag parsing errors are deliberately kept off stdout: flag.FlagSet's
// output is discarded during Parse, so a malformed flag produces only the
// returned error (which main reports on stderr). The one exception is
// -h/-help, which is a request for usage, not a failure, so it is printed to
// stdout and reported as success (nil), following the convention that
// --help is not an error condition.
func run(stdout, stderr io.Writer, args []string) error {
	fs := flag.NewFlagSet("sidecar", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	dbPath := fs.String("db", envOrDefault("SIDECAR_DB", defaultDB), "path to the sqlite database file")
	addr := fs.String("addr", defaultAddr, "address for the HTTP server to listen on")
	regionsURL := fs.String("regions-url", envOrDefault("SIDECAR_REGIONS_URL", defaultRegionsURL), "URL of the regions directory document")
	refresh := fs.Duration("refresh", defaultRefresh, "interval between regions directory refreshes")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fs.SetOutput(stdout)
			fs.Usage()
			return nil
		}
		return err
	}

	// time.NewTicker panics on a duration <= 0. --refresh=0 is a natural way
	// to try to disable the sync loop, and --refresh=nonsense is already
	// rejected by fs.Parse above; a non-positive value that parses cleanly
	// (0 or a negative duration) must be rejected the same clean way here,
	// before the sync loop's goroutine can panic and take the whole process
	// -- including the HTTP server, already serving by then -- down with it.
	if *refresh <= 0 {
		return fmt.Errorf("--refresh must be positive, got %s", refresh.String())
	}

	logger := slog.New(slog.NewTextHandler(stderr, nil))

	store, err := sqlite.Open(*dbPath)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer func() {
		if closeErr := store.Close(); closeErr != nil {
			logger.Error("sidecar: close database", "error", closeErr)
		}
	}()

	// Migrate before anything touches a table: a fresh database has no
	// regions table, so a directory sync that ran first would fail against
	// a missing relation. Never serve on an unknown schema.
	if err := store.Migrate(); err != nil {
		return fmt.Errorf("migrate database: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The sync loop runs in its own goroutine so boot never blocks on the
	// first directory fetch beyond the client's own timeout: the server
	// starts serving from whatever rows already exist while the loop keeps
	// trying in the background.
	client := regions.NewClient(*regionsURL, regions.DefaultClientOptions())
	go regions.RunSyncLoop(ctx, client, store.Regions(), *refresh, time.Now, logger)

	server := httpapi.NewServer(httpapi.ServerConfig{
		Addr: *addr,
		Deps: buildDeps(store, logger),
	})

	logger.Info("sidecar: listening", "addr", *addr)
	return serve(ctx, server, logger)
}

// buildDeps assembles the httpapi.Deps the router needs. It is factored out
// of run so a test can inspect the wired-up Deps directly -- in particular
// FailDelay (see adminFailDelay's comment) -- without standing up a listener
// or driving the process through signal handling. time.Now is read here,
// not in httpapi, because cmd/ is the one place in this repo allowed to
// touch the wall clock directly (design spec §2.3); everywhere else gets it
// injected.
func buildDeps(store *sqlite.Store, logger *slog.Logger) httpapi.Deps {
	return httpapi.Deps{
		Alerts:    store.Alerts(),
		Regions:   store.Regions(),
		Auth:      store.Auth(),
		Now:       time.Now,
		Logger:    logger,
		AdminUI:   adminui.FS(),
		FailDelay: adminFailDelay,
	}
}

// serve runs server until ctx is cancelled (by SIGINT/SIGTERM), then shuts
// it down gracefully, giving in-flight requests up to shutdownTimeout to
// finish.
func serve(ctx context.Context, server *http.Server, logger *slog.Logger) error {
	errCh := make(chan error, 1)
	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("listen and serve: %w", err)
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		logger.Info("sidecar: shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown: %w", err)
		}
		return <-errCh
	}
}

// envOrDefault returns the value of the environment variable key, or def if
// key is unset or empty.
func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
