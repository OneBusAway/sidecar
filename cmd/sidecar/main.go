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
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	// time/tzdata embeds the IANA timezone database into the binary, so
	// time.LoadLocation works in a scratch container with no system tzdata
	// -- PATCH /api/admin/v1/regions/{id} depends on it to reject a bad
	// timezone at the point of the mistake, and would otherwise reject every
	// valid one on such a host.
	_ "time/tzdata"

	"github.com/google/uuid"

	"github.com/OneBusAway/sidecar/internal/alarms"
	"github.com/OneBusAway/sidecar/internal/alertpush"
	"github.com/OneBusAway/sidecar/internal/cache"
	"github.com/OneBusAway/sidecar/internal/clientip"
	"github.com/OneBusAway/sidecar/internal/donations"
	"github.com/OneBusAway/sidecar/internal/dotenv"
	"github.com/OneBusAway/sidecar/internal/errreport"
	"github.com/OneBusAway/sidecar/internal/ghostbus"
	"github.com/OneBusAway/sidecar/internal/httpapi"
	"github.com/OneBusAway/sidecar/internal/httpapi/adminui"
	"github.com/OneBusAway/sidecar/internal/liveactivities"
	"github.com/OneBusAway/sidecar/internal/obaapi"
	"github.com/OneBusAway/sidecar/internal/push"
	"github.com/OneBusAway/sidecar/internal/pushreg"
	"github.com/OneBusAway/sidecar/internal/regions"
	"github.com/OneBusAway/sidecar/internal/store/sqlite"
	"github.com/OneBusAway/sidecar/internal/vehicles"
	"github.com/OneBusAway/sidecar/internal/weather"
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

	// Cache sizing for the upstream proxies. The TTLs come from the spec
	// (fleet 30 minutes, per-query results 5 minutes); the budgets sit under
	// the server's 15s WriteTimeout, and the query budget exceeds the fleet
	// budget because a cold query fetch nests a fleet fetch inside it.
	fleetTTL     = 30 * time.Minute
	fleetEntries = 256
	fleetBudget  = 12 * time.Second
	queryTTL     = 5 * time.Minute
	queryEntries = 4096
	queryBudget  = 13 * time.Second

	// Weather cache sizing: the TTL comes from the spec (30 minutes), the
	// budget sits under the server's 15s WriteTimeout, and entries is sized
	// per coordinate rather than per region since the cache key is the
	// rounded centroid.
	weatherTTL     = 30 * time.Minute
	weatherEntries = 256
	weatherBudget  = 5 * time.Second

	// alarmCheckInterval is the alarm scheduler's cycle cadence (spec §5.3).
	alarmCheckInterval = time.Minute
	// alertPushInterval is the alert push dispatcher's tick (design spec
	// §2.6). The admin API wakes the dispatcher directly after every
	// enqueue, so this ticker is only the at-least-once safety net: it
	// picks up rows the CLI created (it has no handle on a running server)
	// and rows a crash orphaned mid-send.
	alertPushInterval = 15 * time.Second
	// pushRegPruneEvery and pushRegMaxAge bound the push_registrations
	// table: prune once a day (spec §12) any row unseen for 180 days (spec
	// §4).
	pushRegPruneEvery = 24 * time.Hour
	pushRegMaxAge     = 180 * 24 * time.Hour
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
func run(stdout, stderr io.Writer, args []string) (err error) {
	// Before the flag definitions, not just before Parse: the envOrDefault
	// calls below read the environment at flag-registration time, so a .env
	// loaded any later could never reach a flag default. Real environment
	// variables win over the file (dotenv.Load never overwrites), keeping
	// platform-provided production configuration unaffected.
	if loadErr := dotenv.Load(".env"); loadErr != nil {
		return loadErr
	}

	fs := flag.NewFlagSet("sidecar", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	dbPath := fs.String("db", envOrDefault("SIDECAR_DB", defaultDB), "path to the sqlite database file")
	addr := fs.String("addr", defaultListenAddr(), "address for the HTTP server to listen on; "+
		"defaults to SIDECAR_ADDR, then :$PORT (what Render and similar hosts set), then "+defaultAddr)
	regionsURL := fs.String("regions-url", envOrDefault("SIDECAR_REGIONS_URL", defaultRegionsURL), "URL of the regions directory document")
	refresh := fs.Duration("refresh", defaultRefresh, "interval between regions directory refreshes")
	obaAPIKey := fs.String("oba-api-key", envOrDefault("SIDECAR_OBA_API_KEY", ""),
		"default OneBusAway REST API key, used for regions with no key of their own")
	pirateKey := fs.String("pirate-weather-key", envOrDefault("SIDECAR_PIRATE_WEATHER_KEY", ""),
		"Pirate Weather API key; without it the weather endpoint returns 403")
	webhookSecret := fs.String("gorush-webhook-secret", envOrDefault("SIDECAR_GORUSH_WEBHOOK_SECRET", ""),
		"shared secret gorush must send as a bearer token on POST /webhooks/gorush; "+
			"unset leaves the webhook open but rate limited")
	gorushURL := fs.String("gorush-url", envOrDefault("SIDECAR_GORUSH_URL", ""),
		"base URL of the gorush push gateway; without it alarms are stored but never fire")
	trustedProxy := fs.String("trusted-proxy", envOrDefault("SIDECAR_TRUSTED_PROXY", ""),
		"proxy whose client-address header the per-IP throttles trust: off (default: the TCP peer), "+
			"cloudflare (CF-Connecting-IP), render (True-Client-IP), or header:<Name>; "+
			"set only when the proxy overwrites that header on every request")
	logFormat := fs.String("log-format", envOrDefault("SIDECAR_LOG_FORMAT", "text"),
		"log line format: text (default) or json (one object per line, for log aggregators)")
	sentryDSN := fs.String("sentry-dsn", envOrDefault("SIDECAR_SENTRY_DSN", ""),
		"Sentry DSN; when set, every error-level log line is also reported there. Unset disables reporting")
	sentryEnv := fs.String("sentry-environment", envOrDefault("SIDECAR_SENTRY_ENVIRONMENT", ""),
		"environment tag on Sentry events (e.g. production, staging)")
	stripeKey := fs.String("stripe-secret-key", envOrDefault("SIDECAR_STRIPE_SECRET_KEY", ""),
		"Stripe live secret key; enables POST /api/v1/payment_intents (donations). Unset leaves the route unregistered")
	stripeTestKey := fs.String("stripe-test-secret-key", envOrDefault("SIDECAR_STRIPE_TEST_SECRET_KEY", ""),
		"Stripe test secret key, used for requests with test_mode=1")
	stripeProduct := fs.String("stripe-recurring-product-id", envOrDefault("SIDECAR_STRIPE_RECURRING_PRODUCT_ID", ""),
		"id of the live Stripe product recurring donations bill against (prod_...)")
	stripeTestProduct := fs.String("stripe-test-recurring-product-id", envOrDefault("SIDECAR_STRIPE_TEST_RECURRING_PRODUCT_ID", ""),
		"id of the test-mode Stripe product recurring donations bill against")
	apnsTopic := fs.String("apns-topic", envOrDefault("SIDECAR_APNS_TOPIC", ""),
		"APNs topic (the iOS app's bundle id) stamped on every iOS push; required for pushes to be accepted under .p8 token auth")

	if parseErr := fs.Parse(args); parseErr != nil {
		if errors.Is(parseErr, flag.ErrHelp) {
			fs.SetOutput(stdout)
			fs.Usage()
			return nil
		}
		return parseErr
	}

	resolveClientIP, err := clientip.Parse(*trustedProxy)
	if err != nil {
		return fmt.Errorf("--trusted-proxy/SIDECAR_TRUSTED_PROXY: %w", err)
	}

	// gorush builds its feedback header from "name:value" by splitting on
	// every ':' and silently sends no header at all when the split yields
	// more than two parts, so a secret containing ':' (or whitespace, which
	// header values cannot carry) would make every prune 401 with nothing
	// in the sidecar's logs to say why. Reject it here, where it is visible.
	if strings.ContainsAny(*webhookSecret, ": \t\r\n") {
		return errors.New("--gorush-webhook-secret/SIDECAR_GORUSH_WEBHOOK_SECRET must not contain ':' or whitespace (gorush splits its header setting on ':')")
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

	var handler slog.Handler
	switch strings.ToLower(*logFormat) {
	case "text":
		handler = slog.NewTextHandler(stderr, nil)
	case "json":
		handler = slog.NewJSONHandler(stderr, nil)
	default:
		return fmt.Errorf("--log-format/SIDECAR_LOG_FORMAT must be text or json, got %q", *logFormat)
	}
	if *sentryDSN != "" {
		// Sentry's own diagnostics go to the bare handler, never through
		// the tee, or a delivery failure would try to report itself.
		reporter, sentryErr := errreport.NewSentry(*sentryDSN, *sentryEnv, buildVersion(), slog.New(handler))
		if sentryErr != nil {
			return fmt.Errorf("--sentry-dsn/SIDECAR_SENTRY_DSN: %w", sentryErr)
		}
		// Flushed on the way out so errors logged during shutdown -- and
		// the fatal-error line below for a failed boot -- reach Sentry.
		defer reporter.Flush(2 * time.Second)
		handler = errreport.New(handler, reporter)
	}
	logger := slog.New(handler)
	// Boot failures from here on (open, migrate, listen) are returned to
	// main, which prints them; log them too so they reach Sentry and the
	// structured log rather than only the process's stderr line.
	defer func() {
		if err != nil {
			logger.Error("sidecar: fatal", "err", err)
		}
	}()

	store, err := sqlite.Open(*dbPath)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	store.SetLogger(logger)
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

	go pushreg.RunPruneLoop(ctx, store.PushRegs(), pushRegPruneEvery, pushRegMaxAge, time.Now, logger)

	// The scheduler always runs, even with no push transport: its Expire
	// branch and 3-strike reaping are what bound the alarms table (spec
	// §13); only the fire step needs a sender.
	var sender push.Sender
	var laSender push.LiveActivitySender
	// batchSender is the same *push.Gorush as sender, kept separately
	// because the dispatcher wants the batch interface, and assigned only
	// inside the branch that builds one: assigning a nil *push.Gorush would
	// leave a non-nil interface wrapping a nil pointer, defeating both the
	// dispatcher's own Sender == nil check and the waker gate below.
	var batchSender push.BatchSender
	if *gorushURL == "" {
		logger.Warn("no --gorush-url/SIDECAR_GORUSH_URL set; departure alarms and Live Activities will be stored and reaped but never pushed, and alert pushes will fail immediately")
	} else {
		g := push.NewGorush(*gorushURL, *apnsTopic, http.DefaultClient)
		sender = g
		batchSender = g
		if *apnsTopic == "" {
			// Gorush.SendLiveActivity refuses every call without a topic, and
			// the updater treats a refused send as a transport blip it must
			// retry next minute -- one Error log line per subscription per
			// minute for eight hours. Run store-only instead (design spec
			// §2.5): rows still expire and reap, nothing is attempted.
			logger.Warn("no --apns-topic/SIDECAR_APNS_TOPIC set; iOS alarm pushes will be rejected by APNs with MissingTopic and Live Activities will be stored and reaped but never pushed")
		} else {
			laSender = g
		}
	}

	// Alert push fan-out (spec §4, §12 row 3). The dispatcher runs even with
	// no transport (design spec §2.6): the CLI can enqueue a push without a
	// server, and those rows must be resolved -- failed with a reason an
	// operator can read -- rather than left queued forever. Only the admin
	// routes are gated on a transport, via Deps.AlertPushWaker below.
	dispatcher := &alertpush.Dispatcher{
		Repo:     store.AlertPushes(),
		Alerts:   store.Alerts(),
		PushRegs: store.PushRegs(),
		Sender:   batchSender,
		Now:      time.Now,
		Logger:   logger,
	}
	go dispatcher.RunLoop(ctx, alertPushInterval)

	// The waker, unlike the dispatcher itself, is set only when a transport
	// exists: it is what registers the admin push routes (design spec
	// §2.9), and an operator must not be offered a queue button that can
	// only produce failed pushes.
	var waker alertpush.Waker
	if batchSender != nil {
		waker = dispatcher
	}

	// Hoisted out of the ServerConfig literal so the alarm scheduler below
	// shares the same obaapi.Client that vehicle search uses, rather than
	// constructing a second one.
	deps := buildDeps(store, logger, *obaAPIKey, *pirateKey, *webhookSecret, waker)
	deps.ClientIP = resolveClientIP
	deps.Donations = newDonations(logger, *stripeKey, *stripeTestKey, *stripeProduct, *stripeTestProduct)
	if setting := strings.ToLower(strings.TrimSpace(*trustedProxy)); setting != "" && setting != "off" {
		// In the boot log on purpose: a wrong value here mis-keys every
		// throttle bucket.
		logger.Info("per-IP throttles key on the proxy's client-address header", "trusted_proxy", setting)
	}

	sched := &alarms.Scheduler{
		Repo:    store.Alarms(),
		Regions: store.Regions(),
		OBA:     deps.OBA,
		Sender:  sender,
		Now:     time.Now,
		Logger:  logger,
	}
	go sched.RunLoop(ctx, alarmCheckInterval)

	// Live Activities share the alarm cadence (spec §6.3: once per minute)
	// and the same store-only rule: without a sender, rows still expire and
	// reap (design spec §2.5).
	updater := liveactivities.NewUpdater(store.LiveActivities(), store.Regions(), deps.OBA, laSender, time.Now, logger)
	go updater.RunLoop(ctx, alarmCheckInterval)

	// Always runs, mirroring the alarm scheduler above: a region that
	// resolves no OBA key just yields per-report 'unavailable' snapshots
	// (spec §8), which is designed degraded behavior, not a reason to skip
	// starting the loop.
	snapSched := &ghostbus.SnapshotScheduler{
		Repo:    store.GhostBus(),
		Regions: store.Regions(),
		OBA:     deps.OBA,
		Now:     time.Now,
		Logger:  logger,
	}
	go snapSched.RunLoop(ctx, ghostbus.SnapshotInterval)

	server := httpapi.NewServer(httpapi.ServerConfig{
		Addr: *addr,
		Deps: deps,
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
// A nil waker means no push transport is configured, which is what keeps the
// admin alert-push routes unregistered (design spec §2.9); Deps.AlertPushes
// is wired either way, because the feedback webhook must keep accounting
// failures against pushes the CLI enqueued (design spec §2.12).
func buildDeps(store *sqlite.Store, logger *slog.Logger, obaAPIKey, pirateKey, webhookSecret string, waker alertpush.Waker) httpapi.Deps {
	if webhookSecret == "" {
		logger.Warn("no --gorush-webhook-secret/SIDECAR_GORUSH_WEBHOOK_SECRET set; " +
			"POST /webhooks/gorush is open to anyone (rate limited); restrict it at the proxy")
	}
	if obaAPIKey == "" {
		logger.Warn("no --oba-api-key/SIDECAR_OBA_API_KEY set; " +
			"vehicle search returns 502 for regions with no key of their own")
	}
	obaClient := obaapi.New(obaAPIKey, http.DefaultClient, logger)
	vehicleSvc := vehicles.NewService(
		obaClient,
		cache.New[[]obaapi.Vehicle](fleetTTL, fleetEntries, fleetBudget, time.Now),
		cache.New[[]vehicles.Match](queryTTL, queryEntries, queryBudget, time.Now),
		logger,
	)

	// A nil provider is the "not configured" signal: weather.Service turns
	// it into ErrNoProvider, and the handler turns that into a 403, without
	// ever attempting a network call.
	var provider weather.Provider
	if pirateKey == "" {
		logger.Warn("no --pirate-weather-key/SIDECAR_PIRATE_WEATHER_KEY set; the weather endpoint returns 403")
	} else {
		provider = weather.NewPirateWeather(pirateKey, http.DefaultClient, time.Now)
	}
	// Built unconditionally, even when provider is nil: cache.New only
	// allocates (no goroutine, no I/O), and weather.Service.Snapshot checks
	// s.provider == nil before ever touching the cache, so an unconfigured
	// deployment pays a small allocation, not a resource leak. Skipping the
	// construction would need its own nil-Cache branch inside Service for no
	// behavioural gain.
	weatherSvc := weather.NewService(provider,
		cache.New[weather.Snapshot](weatherTTL, weatherEntries, weatherBudget, time.Now))

	return httpapi.Deps{
		Alerts:           store.Alerts(),
		Regions:          store.Regions(),
		Auth:             store.Auth(),
		APIKeys:          store.APIKeys(),
		Now:              time.Now,
		Logger:           logger,
		AdminUI:          adminui.FS(),
		FailDelay:        adminFailDelay,
		Vehicles:         vehicleSvc,
		Weather:          weatherSvc,
		OBADefaultKeySet: obaAPIKey != "",
		PushRegs:         store.PushRegs(),
		Alarms:           store.Alarms(),
		OBA:              obaClient,
		FeedbackSecret:   webhookSecret,
		Surveys:          store.Surveys(),
		GhostBus:         store.GhostBus(),
		LiveActivities:   store.LiveActivities(),
		AlertPushes:      store.AlertPushes(),
		AlertPushWaker:   waker,
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
// defaultListenAddr resolves the --addr default from the environment:
// SIDECAR_ADDR verbatim, else ":"+PORT so a host that assigns the port via
// PORT (Render, Heroku-style platforms) and the binary cannot silently
// disagree on where the health check should look, else defaultAddr.
func defaultListenAddr() string {
	if v := os.Getenv("SIDECAR_ADDR"); v != "" {
		return v
	}
	if p := os.Getenv("PORT"); p != "" {
		return ":" + p
	}
	return defaultAddr
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// version is stamped by the image build (`-ldflags -X main.version=<git
// sha>`; the Docker build has no .git to read). It tags Sentry events so a
// report can be tied to the image that produced it.
var version string

// buildVersion is the stamped version, else the VCS revision `go build`
// records when building inside a checkout, else empty.
func buildVersion() string {
	if version != "" {
		return version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, kv := range info.Settings {
		if kv.Key == "vcs.revision" {
			return kv.Value
		}
	}
	return ""
}

// newDonations wires spec section 11 when a live Stripe key is present and
// returns nil (route unregistered, apps hide the UI) otherwise. A live key
// without a product id still serves one-time donations; recurring ones
// then fail at Stripe and surface as 500s, so the gap is logged at boot.
func newDonations(logger *slog.Logger, liveKey, testKey, liveProduct, testProduct string) *donations.Service {
	if liveKey == "" {
		if testKey != "" {
			logger.Warn("SIDECAR_STRIPE_TEST_SECRET_KEY set without SIDECAR_STRIPE_SECRET_KEY; donations stay disabled")
		}
		return nil
	}
	if liveProduct == "" {
		logger.Warn("no --stripe-recurring-product-id/SIDECAR_STRIPE_RECURRING_PRODUCT_ID set; recurring donations will fail")
	}
	svc := &donations.Service{
		Live:  donations.NewStripeGateway(liveKey, liveProduct, logger),
		NewID: uuid.NewString,
	}
	if testKey == "" {
		logger.Warn("no --stripe-test-secret-key/SIDECAR_STRIPE_TEST_SECRET_KEY set; test_mode donation requests will fail")
		return svc
	}
	if testProduct == "" {
		logger.Warn("no --stripe-test-recurring-product-id/SIDECAR_STRIPE_TEST_RECURRING_PRODUCT_ID set; recurring test_mode donations will fail")
	}
	svc.Test = donations.NewStripeGateway(testKey, testProduct, logger)
	return svc
}
