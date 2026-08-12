package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/OneBusAway/sidecar/internal/alerts"
	"github.com/OneBusAway/sidecar/internal/regions"
	"github.com/OneBusAway/sidecar/internal/store/sqlite"
)

const (
	defaultDB         = "./sidecar.db"
	defaultRegionsURL = "https://regions.onebusaway.org/regions-v3.json"
)

// run holds main's logic so tests can supply their own streams, arguments,
// and a temp database, entirely in-process -- no subprocess. It returns an
// error rather than exiting so main owns the only exit path.
//
// stdin feeds `user create`/`user passwd`'s --password-stdin and interactive
// prompt; every other command ignores it.
//
// Every command runs against a freshly migrated schema: like cmd/sidecar,
// this never operates against an unknown schema. `migrate up` is still
// useful on its own for scripted, explicit use (e.g. a deploy step) even
// though it is redundant here.
func run(stdin io.Reader, stdout, stderr io.Writer, args []string) error {
	fs := flag.NewFlagSet("sidecar-admin", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	dbPath := fs.String("db", envOrDefault("SIDECAR_DB", defaultDB), "path to the sqlite database file")
	regionsURL := fs.String("regions-url", envOrDefault("SIDECAR_REGIONS_URL", defaultRegionsURL), "URL of the regions directory document, used by 'region sync'")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fs.SetOutput(stdout)
			fs.Usage()
			return nil
		}
		return err
	}

	rest := fs.Args()
	if len(rest) == 0 {
		return errors.New("missing command; expected region, alert, migrate, or user")
	}

	store, err := sqlite.Open(*dbPath)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer func() {
		if closeErr := store.Close(); closeErr != nil {
			fmt.Fprintln(stderr, "sidecar-admin: close database:", closeErr)
		}
	}()

	cmd, cmdArgs := rest[0], rest[1:]

	// Every other command runs against a freshly migrated schema (see the
	// doc comment above). `migrate status` is the one exception: it exists
	// to report the database's real pre-migration state, so auto-migrating
	// first would make it apply every pending migration and then report
	// "up to date" -- the opposite of the truth, having silently mutated the
	// schema a read-only inspection command was never supposed to touch.
	if cmd != "migrate" || len(cmdArgs) == 0 || cmdArgs[0] != "status" {
		if err := store.Migrate(); err != nil {
			return fmt.Errorf("migrate database: %w", err)
		}
	}

	ctx := context.Background()
	now := time.Now()

	switch cmd {
	case "region":
		return runRegion(ctx, stdout, store, *regionsURL, now, cmdArgs)
	case "alert":
		return runAlert(ctx, stdout, store, now, cmdArgs)
	case "migrate":
		return runMigrate(ctx, stdout, store, cmdArgs)
	case "user":
		return runUser(ctx, stdin, stdout, stderr, store, now, cmdArgs)
	default:
		return fmt.Errorf("unknown command %q; expected region, alert, migrate, or user", cmd)
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

// visitedFlags reports which flags on fs were actually set by the caller,
// distinguishing "absent" from "set to the zero value" -- the mechanism
// every partial-update command (region set, alert edit) depends on.
func visitedFlags(fs *flag.FlagSet) map[string]bool {
	seen := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { seen[f.Name] = true })
	return seen
}

// parseInstant requires an explicit UTC offset. A naive datetime is rejected
// rather than guessed: interpreting it in the server's local zone would
// place an alert hours from where the author meant, and the regions
// directory carries no timezone to fall back on.
func parseInstant(s string, region regions.Region) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf(
			"%q must be RFC 3339 with an explicit offset (e.g. 2026-08-15T14:00:00-07:00); "+
				"region %d is configured as %s: %w", s, region.ID, region.Timezone, err)
	}
	return t.UTC(), nil
}

// parseAlertIDArg extracts and parses the alert id positional argument
// shared by every `alert` subcommand that operates on a single existing
// alert (show, edit, publish/unpublish, delete, translate). op names the
// subcommand for the error message, e.g. "alert show".
func parseAlertIDArg(op string, args []string) (int64, error) {
	if len(args) == 0 {
		return 0, fmt.Errorf("%s requires an alert id", op)
	}
	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid alert id %q: %w", op, args[0], err)
	}
	return id, nil
}

// wrapAlertErr frames a store-layer error about an existing alert in terms of
// the subcommand and id the user supplied (e.g. "alert publish 1: alert not
// found"), rather than a second copy of the store's own operation name or the
// storage layer's internal wrapping (e.g. "sqlite: set published for alert
// 1"). errors.Is(result, alerts.ErrNotFound) still succeeds, since the
// sentinel is re-wrapped unchanged; any other error keeps the store's full
// detail, since that detail -- not a restatement of "op id" -- is what's
// needed to diagnose something unexpected.
func wrapAlertErr(op string, id int64, err error) error {
	if errors.Is(err, alerts.ErrNotFound) {
		return fmt.Errorf("%s %d: %w", op, id, alerts.ErrNotFound)
	}
	return fmt.Errorf("%s %d: %w", op, id, err)
}

// formatInZone renders t in tz alongside UTC, so `alert list`/`alert show`
// never leave the reader to do zone math by hand. An empty or unrecognised
// tz falls back to UTC only.
func formatInZone(t time.Time, tz string) string {
	utc := t.UTC().Format(time.RFC3339)
	if tz == "" {
		return utc
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return utc
	}
	return fmt.Sprintf("%s (%s)", t.In(loc).Format(time.RFC3339), utc)
}

// ---------------------------------------------------------------------------
// region
// ---------------------------------------------------------------------------

func runRegion(ctx context.Context, stdout io.Writer, store *sqlite.Store, regionsURL string, now time.Time, args []string) error {
	if len(args) == 0 {
		return errors.New("region requires a subcommand: list, set, sync")
	}
	switch args[0] {
	case "list":
		return regionList(ctx, stdout, store.Regions())
	case "set":
		return regionSet(ctx, store.Regions(), now, args[1:])
	case "sync":
		return regionSync(ctx, store.Regions(), regionsURL, now)
	default:
		return fmt.Errorf("unknown region subcommand %q; expected list, set, or sync", args[0])
	}
}

func regionList(ctx context.Context, stdout io.Writer, repo regions.Repository) error {
	list, err := repo.List(ctx)
	if err != nil {
		return fmt.Errorf("region list: %w", err)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	for _, r := range list {
		fmt.Fprintf(stdout, "%d\t%s\tactive=%t\tagency=%s\ttz=%s\n", r.ID, r.Name, r.Active, r.DefaultAgencyID, r.Timezone)
	}
	return nil
}

// regionSet updates an existing region row only: regions come from the
// directory, so an unknown --id is an error, not an implicit insert. An
// omitted --agency-id/--timezone leaves that field unchanged, which is why
// the current row is fetched and merged before the (full-row) write.
func regionSet(ctx context.Context, repo regions.Repository, now time.Time, args []string) error {
	fs := flag.NewFlagSet("region set", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	id := fs.Int64("id", 0, "region id (required)")
	agencyID := fs.String("agency-id", "", "default agency id for this region")
	timezone := fs.String("timezone", "", "IANA timezone for this region, e.g. America/Los_Angeles")
	if err := fs.Parse(args); err != nil {
		return err
	}
	seen := visitedFlags(fs)
	if !seen["id"] {
		return errors.New("region set requires --id")
	}

	current, err := repo.Get(ctx, *id)
	if err != nil {
		return fmt.Errorf("region set: %w", err)
	}

	newAgencyID := current.DefaultAgencyID
	if seen["agency-id"] {
		newAgencyID = *agencyID
	}

	newTimezone := current.Timezone
	if seen["timezone"] {
		// Validated here, at the point of the mistake, rather than later
		// inside `alert list`/`alert create` where a typo would be far
		// less obvious.
		//
		// time.LoadLocation returns a nil error for both "" and "Local", so
		// neither is caught by the call below: "" would silently blank a
		// configured timezone, and "Local" would resolve to whatever zone
		// the invoking machine happens to have -- exactly the
		// machine-local dependence this design bans everywhere else. Both
		// are rejected explicitly rather than trusted to LoadLocation.
		if *timezone == "" {
			return errors.New("region set: --timezone must not be empty")
		}
		if *timezone == "Local" {
			return errors.New("region set: --timezone \"Local\" is machine-dependent; use an explicit IANA zone name")
		}
		if _, err := time.LoadLocation(*timezone); err != nil {
			return fmt.Errorf("region set: invalid --timezone %q: %w", *timezone, err)
		}
		newTimezone = *timezone
	}

	if err := repo.SetLocalFields(ctx, *id, newAgencyID, newTimezone, now); err != nil {
		return fmt.Errorf("region set: %w", err)
	}
	return nil
}

func regionSync(ctx context.Context, repo regions.Repository, url string, now time.Time) error {
	client := regions.NewClient(url, regions.DefaultClientOptions())
	if err := regions.Sync(ctx, client, repo, func() time.Time { return now }); err != nil {
		return fmt.Errorf("region sync: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// alert
// ---------------------------------------------------------------------------

func runAlert(ctx context.Context, stdout io.Writer, store *sqlite.Store, now time.Time, args []string) error {
	if len(args) == 0 {
		return errors.New("alert requires a subcommand: create, list, show, edit, publish, unpublish, delete, translate")
	}
	cmd, cmdArgs := args[0], args[1:]
	switch cmd {
	case "create":
		return alertCreate(ctx, stdout, store, now, cmdArgs)
	case "list":
		return alertList(ctx, stdout, store, cmdArgs)
	case "show":
		return alertShow(ctx, stdout, store, cmdArgs)
	case "edit":
		return alertEdit(ctx, store, now, cmdArgs)
	case "publish":
		return alertSetPublished(ctx, store, now, cmdArgs, true)
	case "unpublish":
		return alertSetPublished(ctx, store, now, cmdArgs, false)
	case "delete":
		return alertDelete(ctx, store, cmdArgs)
	case "translate":
		return alertTranslate(ctx, store, now, cmdArgs)
	default:
		return fmt.Errorf("unknown alert subcommand %q", cmd)
	}
}

// alertCreate validates the whole request -- region, timestamps, window,
// resolved agency id, enums -- before making any repository call, so a
// rejected create never leaves a partial row behind.
func alertCreate(ctx context.Context, stdout io.Writer, store *sqlite.Store, now time.Time, args []string) error {
	fs := flag.NewFlagSet("alert create", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	regionID := fs.Int64("region", 0, "region id (required)")
	header := fs.String("header", "", "alert header text (required)")
	start := fs.String("start", "", "alert start time, RFC 3339 with an explicit offset (required)")
	description := fs.String("description", "", "alert description text")
	url := fs.String("url", "", "informational URL")
	end := fs.String("end", "", "alert end time, RFC 3339 with an explicit offset")
	agencyID := fs.String("agency-id", "", "agency id; defaults to the region's configured default")
	cause := fs.String("cause", "", "GTFS-realtime cause, e.g. CONSTRUCTION")
	effect := fs.String("effect", "", "GTFS-realtime effect, e.g. DETOUR")
	severity := fs.String("severity", "", "GTFS-realtime severity, e.g. WARNING")
	isTest := fs.Bool("test", false, "mark this alert as a test alert")
	if err := fs.Parse(args); err != nil {
		return err
	}
	seen := visitedFlags(fs)

	var missing []string
	for _, name := range []string{"region", "header", "start"} {
		if !seen[name] {
			missing = append(missing, "--"+name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("alert create requires %s", strings.Join(missing, ", "))
	}
	if *header == "" {
		return errors.New("alert create: --header cannot be empty")
	}

	reg, err := store.Regions().Get(ctx, *regionID)
	if err != nil {
		return fmt.Errorf("alert create: %w", err)
	}

	startTime, err := parseInstant(*start, reg)
	if err != nil {
		return fmt.Errorf("alert create: %w", err)
	}

	var endTime *time.Time
	if seen["end"] {
		endInstant, endErr := parseInstant(*end, reg)
		if endErr != nil {
			return fmt.Errorf("alert create: %w", endErr)
		}
		endTime = &endInstant
	}

	if winErr := alerts.ValidateWindow(startTime, endTime, now); winErr != nil {
		return fmt.Errorf("alert create: %w", winErr)
	}

	// agency_id resolves at author time: --agency-id, else the region's
	// default. The resolved value is what gets stored, so the stored alert
	// never changes underneath a later directory sync or region default
	// change.
	resolvedAgencyID := reg.DefaultAgencyID
	if seen["agency-id"] {
		resolvedAgencyID = *agencyID
	}
	if resolvedAgencyID == "" {
		return fmt.Errorf(
			"alert create: region %d has no default agency id.\n"+
				"  Set one:              sidecar-admin --db <db> region set --id %d --agency-id <agency>\n"+
				"  Or pass it per-alert: --agency-id <agency>",
			*regionID, *regionID)
	}

	causeName, err := alerts.ParseCause(*cause)
	if err != nil {
		return fmt.Errorf("alert create: %w", err)
	}
	effectName, err := alerts.ParseEffect(*effect)
	if err != nil {
		return fmt.Errorf("alert create: %w", err)
	}
	severityName, err := alerts.ParseSeverity(*severity)
	if err != nil {
		return fmt.Errorf("alert create: %w", err)
	}

	a, err := store.Alerts().Create(ctx, alerts.NewAlert{
		RegionID:        *regionID,
		AgencyID:        resolvedAgencyID,
		HeaderText:      *header,
		DescriptionText: *description,
		URL:             *url,
		Cause:           causeName,
		Effect:          effectName,
		Severity:        severityName,
		StartTime:       startTime,
		EndTime:         endTime,
		IsTest:          *isTest,
	}, now)
	if err != nil {
		return fmt.Errorf("alert create: %w", err)
	}

	fmt.Fprintf(stdout, "created alert %d\n", a.ID)
	return nil
}

// alertList requires exactly one of --region or --all: region 0 is a real
// region (Tampa Bay), so a default that silently means "all" would make
// `alert list` with no flags an easy way to think you scoped to a region
// when you didn't, and there is no zero value that safely means "all".
func alertList(ctx context.Context, stdout io.Writer, store *sqlite.Store, args []string) error {
	fs := flag.NewFlagSet("alert list", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	regionID := fs.Int64("region", 0, "list only this region's alerts")
	all := fs.Bool("all", false, "list alerts across every region")
	if err := fs.Parse(args); err != nil {
		return err
	}
	seen := visitedFlags(fs)

	if seen["region"] && *all {
		return errors.New("alert list: specify only one of --region or --all")
	}
	if !seen["region"] && !*all {
		return errors.New("alert list: specify --region N or --all")
	}

	filter := alerts.ListFilter{}
	if seen["region"] {
		filter.RegionID = regionID
	}

	list, err := store.Alerts().List(ctx, filter)
	if err != nil {
		return fmt.Errorf("alert list: %w", err)
	}

	regionCache := map[int64]regions.Region{}
	for _, a := range list {
		reg, ok := regionCache[a.RegionID]
		if !ok {
			reg, err = store.Regions().Get(ctx, a.RegionID)
			if err != nil {
				return fmt.Errorf("alert list: region %d: %w", a.RegionID, err)
			}
			regionCache[a.RegionID] = reg
		}
		fmt.Fprintf(stdout, "%d\tregion=%d\tpublished=%t\ttest=%t\tstart=%s\t%s\n",
			a.ID, a.RegionID, a.Published, a.IsTest, formatInZone(a.StartTime, reg.Timezone), a.HeaderText)
	}
	return nil
}

// alertShowStore is the slice of *sqlite.Store that alertShow depends on,
// factored out so a test can supply a Regions() repository that fails with
// something other than regions.ErrNotFound -- exercising the branch that
// must surface an unexpected failure rather than swallow it. *sqlite.Store
// satisfies this implicitly.
type alertShowStore interface {
	Alerts() alerts.Repository
	Regions() regions.Repository
}

func alertShow(ctx context.Context, stdout io.Writer, store alertShowStore, args []string) error {
	id, err := parseAlertIDArg("alert show", args)
	if err != nil {
		return err
	}

	a, err := store.Alerts().Get(ctx, id)
	if err != nil {
		return wrapAlertErr("alert show", id, err)
	}

	// A missing region is tolerated (formatInZone degrades to UTC-only), but
	// any other failure -- a corrupt database, a cancelled context -- must
	// not be treated the same way: silently printing the alert in UTC and
	// exiting 0 would hide the real problem from whoever is looking at this
	// output.
	tz := ""
	reg, err := store.Regions().Get(ctx, a.RegionID)
	switch {
	case err == nil:
		tz = reg.Timezone
	case errors.Is(err, regions.ErrNotFound):
		// fall through with tz == "" (UTC only).
	default:
		return fmt.Errorf("alert show: region %d: %w", a.RegionID, err)
	}

	fmt.Fprintf(stdout, "id: %d\n", a.ID)
	fmt.Fprintf(stdout, "region: %d\n", a.RegionID)
	fmt.Fprintf(stdout, "agency: %s\n", a.AgencyID)
	fmt.Fprintf(stdout, "published: %t\n", a.Published)
	fmt.Fprintf(stdout, "test: %t\n", a.IsTest)
	fmt.Fprintf(stdout, "header: %s\n", a.HeaderText)
	fmt.Fprintf(stdout, "description: %s\n", a.DescriptionText)
	fmt.Fprintf(stdout, "url: %s\n", a.URL)
	fmt.Fprintf(stdout, "cause: %s\n", a.Cause)
	fmt.Fprintf(stdout, "effect: %s\n", a.Effect)
	fmt.Fprintf(stdout, "severity: %s\n", a.Severity)
	fmt.Fprintf(stdout, "start: %s\n", formatInZone(a.StartTime, tz))
	if a.EndTime != nil {
		fmt.Fprintf(stdout, "end: %s\n", formatInZone(*a.EndTime, tz))
	} else {
		fmt.Fprintf(stdout, "end: (none; feed falls back to %s after start)\n", alerts.DefaultDuration)
	}
	for _, t := range a.Translations {
		fmt.Fprintf(stdout, "translation[%s/%s]: %s\n", t.Language, t.Field, t.Text)
	}
	return nil
}

// alertEdit applies patch semantics: an omitted flag leaves the value
// unchanged, --url "" clears the URL, and --no-end reverts to the default
// fallback duration (distinct from omitting --end, which leaves the end
// time alone). The resulting window is validated against the *merged*
// view -- current row plus patch -- before any write, so e.g. editing only
// --end still gets checked against the alert's existing start.
func alertEdit(ctx context.Context, store *sqlite.Store, now time.Time, args []string) error {
	id, err := parseAlertIDArg("alert edit", args)
	if err != nil {
		return err
	}

	fs := flag.NewFlagSet("alert edit", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	header := fs.String("header", "", "alert header text")
	description := fs.String("description", "", "alert description text")
	url := fs.String("url", "", "informational URL; pass an empty string to clear it")
	start := fs.String("start", "", "alert start time, RFC 3339 with an explicit offset")
	end := fs.String("end", "", "alert end time, RFC 3339 with an explicit offset")
	noEnd := fs.Bool("no-end", false, "clear the end time, reverting to the default fallback duration")
	agencyID := fs.String("agency-id", "", "agency id")
	cause := fs.String("cause", "", "GTFS-realtime cause")
	effect := fs.String("effect", "", "GTFS-realtime effect")
	severity := fs.String("severity", "", "GTFS-realtime severity")
	test := fs.Bool("test", false, "mark this alert as a test alert")
	noTest := fs.Bool("no-test", false, "unmark this alert as a test alert")
	if parseErr := fs.Parse(args[1:]); parseErr != nil {
		return parseErr
	}
	seen := visitedFlags(fs)

	if seen["end"] && *noEnd {
		return errors.New("alert edit: specify only one of --end or --no-end")
	}
	if seen["test"] && seen["no-test"] {
		return errors.New("alert edit: specify only one of --test or --no-test")
	}

	current, err := store.Alerts().Get(ctx, id)
	if err != nil {
		return wrapAlertErr("alert edit", id, err)
	}
	reg, err := store.Regions().Get(ctx, current.RegionID)
	if err != nil {
		return fmt.Errorf("alert edit: %w", err)
	}

	var patch alerts.Patch

	effectiveStart := current.StartTime
	if seen["start"] {
		startInstant, startErr := parseInstant(*start, reg)
		if startErr != nil {
			return fmt.Errorf("alert edit: %w", startErr)
		}
		patch.StartTime = &startInstant
		effectiveStart = startInstant
	}

	effectiveEnd := current.EndTime
	switch {
	case *noEnd:
		patch.ClearEndTime = true
		effectiveEnd = nil
	case seen["end"]:
		endInstant, endErr := parseInstant(*end, reg)
		if endErr != nil {
			return fmt.Errorf("alert edit: %w", endErr)
		}
		patch.EndTime = &endInstant
		effectiveEnd = &endInstant
	}

	if winErr := alerts.ValidateWindow(effectiveStart, effectiveEnd, now); winErr != nil {
		return fmt.Errorf("alert edit: %w", winErr)
	}

	if seen["header"] {
		if *header == "" {
			return errors.New("alert edit: --header cannot be empty")
		}
		patch.HeaderText = header
	}
	if seen["description"] {
		patch.DescriptionText = description
	}
	if seen["url"] {
		patch.URL = url
	}
	if seen["agency-id"] {
		if *agencyID == "" {
			return errors.New("alert edit: --agency-id cannot be empty")
		}
		patch.AgencyID = agencyID
	}
	if seen["cause"] {
		name, causeErr := alerts.ParseCause(*cause)
		if causeErr != nil {
			return fmt.Errorf("alert edit: %w", causeErr)
		}
		patch.Cause = &name
	}
	if seen["effect"] {
		name, effectErr := alerts.ParseEffect(*effect)
		if effectErr != nil {
			return fmt.Errorf("alert edit: %w", effectErr)
		}
		patch.Effect = &name
	}
	if seen["severity"] {
		name, severityErr := alerts.ParseSeverity(*severity)
		if severityErr != nil {
			return fmt.Errorf("alert edit: %w", severityErr)
		}
		patch.Severity = &name
	}
	// Branch on whether --test/--no-test were passed at all, not on their
	// values: `--test=false` must clear IsTest, not be mistaken for the flag
	// being absent (which is what `if *test` did -- a silent no-op that let
	// a verified test alert stay flagged as test after an author explicitly
	// tried to promote it to real).
	//
	// --no-test's whole job is to clear IsTest, so --no-test/--no-test=true
	// do that. --no-test=false is the regression this replaces: an earlier
	// fix computed `v := !*noTest` here, so --no-test=false (the standard Go
	// boolean-flag spelling for "don't do what this flag does") evaluated to
	// v=true and *set* IsTest instead of leaving it alone -- a published,
	// rider-visible alert edited with `--no-test=false` would silently
	// vanish from the public feed. There is no reading of "decline to
	// unmark this as test" that means "mark it as test", so --no-test=false
	// is treated as a no-op (leaves IsTest whatever it already was) rather
	// than inverted.
	if seen["test"] {
		v := *test
		patch.IsTest = &v
	} else if seen["no-test"] && *noTest {
		v := false
		patch.IsTest = &v
	}

	if _, err := store.Alerts().Update(ctx, id, patch, now); err != nil {
		return wrapAlertErr("alert edit", id, err)
	}
	return nil
}

func alertSetPublished(ctx context.Context, store *sqlite.Store, now time.Time, args []string, published bool) error {
	op := "alert publish"
	if !published {
		op = "alert unpublish"
	}
	id, err := parseAlertIDArg(op, args)
	if err != nil {
		return err
	}
	if err := store.Alerts().SetPublished(ctx, id, published, now); err != nil {
		return wrapAlertErr(op, id, err)
	}
	return nil
}

func alertDelete(ctx context.Context, store *sqlite.Store, args []string) error {
	id, err := parseAlertIDArg("alert delete", args)
	if err != nil {
		return err
	}
	if err := store.Alerts().Delete(ctx, id); err != nil {
		return wrapAlertErr("alert delete", id, err)
	}
	return nil
}

// alertTranslate stores SourceSHA256 of the *current* English text for each
// field being translated. Editing the English afterwards changes its hash,
// which makes the translation stale and the feed withholds it -- see
// internal/alerts/feed.go's translated().
func alertTranslate(ctx context.Context, store *sqlite.Store, now time.Time, args []string) error {
	id, err := parseAlertIDArg("alert translate", args)
	if err != nil {
		return err
	}

	fs := flag.NewFlagSet("alert translate", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	language := fs.String("language", "", "BCP-47 language tag (required)")
	header := fs.String("header", "", "translated header text")
	description := fs.String("description", "", "translated description text")
	if parseErr := fs.Parse(args[1:]); parseErr != nil {
		return parseErr
	}
	seen := visitedFlags(fs)

	if !seen["language"] || strings.TrimSpace(*language) == "" {
		return errors.New("alert translate requires --language")
	}
	if !seen["header"] && !seen["description"] {
		return errors.New("alert translate requires --header and/or --description")
	}

	current, err := store.Alerts().Get(ctx, id)
	if err != nil {
		return wrapAlertErr("alert translate", id, err)
	}

	lang := alerts.NormalizeLanguage(*language)

	if seen["header"] {
		t := alerts.Translation{
			Language:     lang,
			Field:        alerts.FieldHeader,
			Text:         *header,
			SourceSHA256: alerts.SourceHash(current.HeaderText),
		}
		if upsertErr := store.Alerts().UpsertTranslation(ctx, id, t, now); upsertErr != nil {
			return fmt.Errorf("alert translate: header: %w", upsertErr)
		}
	}
	if seen["description"] {
		t := alerts.Translation{
			Language:     lang,
			Field:        alerts.FieldDescription,
			Text:         *description,
			SourceSHA256: alerts.SourceHash(current.DescriptionText),
		}
		if upsertErr := store.Alerts().UpsertTranslation(ctx, id, t, now); upsertErr != nil {
			return fmt.Errorf("alert translate: description: %w", upsertErr)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// migrate
// ---------------------------------------------------------------------------

func runMigrate(ctx context.Context, stdout io.Writer, store *sqlite.Store, args []string) error {
	if len(args) == 0 {
		return errors.New("migrate requires a subcommand: up, status")
	}
	switch args[0] {
	case "up":
		// run already migrated to latest before dispatch; this is here for
		// explicit, scriptable use (e.g. a deploy step) and is a no-op when
		// already current.
		if err := store.Migrate(); err != nil {
			return fmt.Errorf("migrate up: %w", err)
		}
		fmt.Fprintln(stdout, "database is up to date")
		return nil
	case "status":
		statuses, err := store.MigrationStatuses(ctx)
		if err != nil {
			return fmt.Errorf("migrate status: %w", err)
		}
		pending := 0
		for _, s := range statuses {
			state := "applied"
			if s.Pending {
				state = "pending"
				pending++
			}
			fmt.Fprintf(stdout, "%d\t%s\n", s.Version, state)
		}
		if pending == 0 {
			fmt.Fprintln(stdout, "up to date")
		}
		return nil
	default:
		return fmt.Errorf("unknown migrate subcommand %q; expected up or status", args[0])
	}
}
