# Service Alerts Feed Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Serve the region-scoped GTFS-realtime service alerts feed, with a `sidecar-admin` CLI for authoring the alerts it publishes.

**Architecture:** SQL selects which alerts (region, published, test flag, order, cap); a pure Go function renders them to protobuf. Storage is SQLite behind a repository interface over domain types, exercised by a shared conformance suite so a future Postgres adapter can prove equivalence. Regions are ingested from a configurable `regions-v3.json` URL and cached in the database.

**Tech Stack:** Go 1.26, `modernc.org/sqlite` (pure Go, no cgo), sqlc, goose, `MobilityData/gtfs-realtime-bindings`, `google.golang.org/protobuf`.

**Design spec:** `docs/superpowers/specs/2026-08-11-service-alerts-feed-design.md` — normative for every decision here. `specification/specification.md` §3 is normative for the feed contract.

## Global Constraints

Every task's requirements implicitly include this section.

- **Every timestamp is epoch seconds in an `INTEGER` column.** Never `DATETIME`, never `TEXT`. Storing a `time.Time` in a datetime column makes `ORDER BY` sort wall-clock text and inverts the feed's required newest-first ordering.
- **`time.Local` and `time.Now` are forbidden outside `cmd/`.** Enforced by `forbidigo` (Task 1). Clocks are read once at an entrypoint and injected downward.
- **All ordering is `ORDER BY start_time DESC, id DESC`** — never `start_time` alone. Ties would otherwise be resolved arbitrarily and differently per engine, so the 20-row cap would select different alerts.
- **Postgres columns, when that adapter is written, are `BIGINT`** — never `INTEGER`, which is 32-bit in Postgres and caps timestamps at 2038.
- **Wire assertions never use golden files.** `protojson` deliberately randomizes whitespace. Compare with `protocmp.Transform()`.
- **No `ORDER BY` on a nullable column** — SQLite and Postgres disagree on NULL placement.
- **Portable SQL only**: `ON CONFLICT … DO UPDATE`; no `INSERT OR REPLACE`, no rowid tricks, no `strftime` logic. Hashing and language-tag normalization happen in Go.
- Feed cap: **20**. Default alert duration when no end time: **8 hours**. Directory refresh: **60 minutes**.
- Module path is `github.com/OneBusAway/sidecar`.
- Every task ends with `make check` passing.

## File Structure

| File | Responsibility |
|---|---|
| `internal/alerts/alert.go` | `Alert`, `Translation`, `Field`, `SourceHash` — domain types only |
| `internal/alerts/enums.go` | GTFS-RT enum name validation and protobuf mapping |
| `internal/alerts/feed.go` | `BuildFeed` — pure render, no I/O |
| `internal/alerts/repository.go` | `Repository` interface, `NewAlert`, `Patch`, `ListFilter` |
| `internal/regions/region.go` | `Region`, `Repository` interface |
| `internal/regions/directory.go` | Directory client — fetch, validate, hostile-input limits |
| `internal/regions/sync.go` | Periodic refresh loop |
| `internal/store/sqlite/migrations/` | goose SQL, embedded |
| `internal/store/sqlite/queries/` | sqlc source SQL |
| `internal/store/sqlite/gen/` | sqlc output (committed, never hand-edited) |
| `internal/store/sqlite/store.go` | `Repository` implementations; maps generated rows to domain types |
| `internal/store/storetest/storetest.go` | Shared conformance suite, parameterized over implementations |
| `internal/httpapi/router.go` | Routes and server construction |
| `internal/httpapi/alerts.go` | Feed handlers, region-segment parsing, error shapes |
| `cmd/sidecar/main.go` | Server: migrate, sync, serve |
| `cmd/sidecar-admin/main.go` | CLI entrypoint |
| `cmd/sidecar-admin/commands.go` | Command dispatch and flag parsing |

---

### Task 1: Scaffolding — dependencies, sqlc, lint rules, Makefile targets

Sets up everything later tasks depend on, and widens the `run` seam so both binaries can return errors.

**Files:**
- Modify: `go.mod` (via `go get`)
- Create: `sqlc.yaml`
- Modify: `.golangci.yml`
- Modify: `Makefile`
- Modify: `cmd/sidecar/main.go`
- Modify: `cmd/sidecar/main_test.go`

**Interfaces:**
- Consumes: nothing
- Produces: `run(stdout, stderr io.Writer, args []string) error` seam pattern; `make generate`, `make generate-check`, `make test-tz`

- [ ] **Step 1: Add dependencies**

```bash
go get github.com/MobilityData/gtfs-realtime-bindings/golang/gtfs@v1.0.0
go get google.golang.org/protobuf@v1.36.12
go get modernc.org/sqlite@v1.56.0
go get github.com/pressly/goose/v3@v3.27.3
go get github.com/google/go-cmp@latest
go mod tidy
```

- [ ] **Step 2: Create `sqlc.yaml`**

```yaml
version: "2"
sql:
  - engine: "sqlite"
    schema: "internal/store/sqlite/migrations"
    queries: "internal/store/sqlite/queries"
    gen:
      go:
        package: "gen"
        out: "internal/store/sqlite/gen"
        emit_empty_slices: true
```

- [ ] **Step 3: Add `forbidigo` to `.golangci.yml`**

Add `forbidigo` to `linters.enable`, then add to `linters.settings`:

```yaml
    forbidigo:
      analyze-types: true
      forbid:
        - pattern: '^time\.Local$'
          msg: "time.Local is banned outside cmd/: timestamps are absolute instants (see design spec §2.3)"
        - pattern: '^time\.Now$'
          msg: "time.Now is banned outside cmd/: inject the clock so tests are deterministic (see design spec §2.3)"
```

And to `linters.exclusions.rules`:

```yaml
      # Entrypoints legitimately read the clock once and inject it downward.
      - path: '^cmd/'
        linters:
          - forbidigo
      # Tests construct fixed instants and occasionally need a real clock.
      - path: '_test\.go$'
        linters:
          - forbidigo
```

- [ ] **Step 4: Add Makefile targets**

Add to the tooling section, and add `test-tz` to the `check` target's prerequisites (`check: fmt-check vet lint test test-tz`):

```makefile
.PHONY: generate
generate: ## Regenerate sqlc code
	sqlc generate

.PHONY: generate-check
generate-check: ## Fail if committed sqlc output is stale
	sqlc diff

.PHONY: test-tz
test-tz: ## Run tests under two timezones to catch local-time leaks
	TZ=UTC go test ./...
	TZ=Asia/Kathmandu go test ./...
```

- [ ] **Step 5: Widen the `run` seam — write the failing test**

Replace `cmd/sidecar/main_test.go` entirely:

```go
package main

import (
	"bytes"
	"testing"
)

func TestRun(t *testing.T) {
	t.Parallel()

	var stdout, stderr bytes.Buffer
	if err := run(&stdout, &stderr, nil); err != nil {
		t.Fatalf("run() returned %v, want nil", err)
	}
	if got, want := stdout.String(), "Hello, world!\n"; got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty", stderr.String())
	}
}
```

- [ ] **Step 6: Run test to verify it fails**

Run: `go test ./cmd/sidecar/ -run TestRun -v`
Expected: FAIL — compile error, `too many arguments in call to run`.

- [ ] **Step 7: Widen the seam**

Replace `cmd/sidecar/main.go`:

```go
// Command sidecar is the OneBusAway sidecar server.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/OneBusAway/sidecar/internal/greeting"
)

func main() {
	if err := run(os.Stdout, os.Stderr, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "sidecar:", err)
		os.Exit(1)
	}
}

// run holds main's logic so tests can supply their own streams and arguments.
// It returns an error rather than exiting so main owns the only exit path.
func run(stdout, _ io.Writer, args []string) error {
	var name string
	if len(args) > 0 {
		name = args[0]
	}
	fmt.Fprintln(stdout, greeting.Greet(name))
	return nil
}
```

- [ ] **Step 8: Run the full check**

Run: `make check`
Expected: PASS, including both timezone runs.

- [ ] **Step 9: Commit**

```bash
git add -A
git commit -m "Add alerts feed dependencies, sqlc config, and lint rules

Widens the run() seam to return an error so both binaries can map
failures to exit codes. Adds a forbidigo rule banning time.Local and
time.Now outside cmd/, which makes the absolute-instant invariant a
build failure rather than a convention."
```

---

### Task 2: Domain types and GTFS-RT enum mapping

**Files:**
- Create: `internal/alerts/alert.go`
- Create: `internal/alerts/enums.go`
- Test: `internal/alerts/enums_test.go`

**Interfaces:**
- Consumes: nothing
- Produces:
  - `type Field string`; `FieldHeader`, `FieldDescription`
  - `type Translation struct { Language string; Field Field; Text, SourceSHA256 string }`
  - `type Alert struct { ID, RegionID int64; AgencyID, HeaderText, DescriptionText, URL string; Cause, Effect, Severity string; StartTime time.Time; EndTime *time.Time; Published, IsTest bool; Translations []Translation }`
  - `func SourceHash(s string) string`
  - `func ParseCause(string) (string, error)`, `ParseEffect`, `ParseSeverity`
  - `func CauseEnum(string) gtfs.Alert_Cause`, `EffectEnum`, `SeverityEnum`
  - `func NormalizeLanguage(string) string`

- [ ] **Step 1: Write the failing test**

Create `internal/alerts/enums_test.go`:

```go
package alerts_test

import (
	"testing"

	"github.com/MobilityData/gtfs-realtime-bindings/golang/gtfs"

	"github.com/OneBusAway/sidecar/internal/alerts"
)

func TestParseCause(t *testing.T) {
	t.Parallel()

	if got, err := alerts.ParseCause("construction"); err != nil || got != "CONSTRUCTION" {
		t.Errorf("ParseCause(construction) = %q, %v; want CONSTRUCTION, nil", got, err)
	}
	if got, err := alerts.ParseCause(""); err != nil || got != "UNKNOWN_CAUSE" {
		t.Errorf("ParseCause(empty) = %q, %v; want UNKNOWN_CAUSE, nil", got, err)
	}
	if _, err := alerts.ParseCause("NOT_A_CAUSE"); err == nil {
		t.Error("ParseCause(NOT_A_CAUSE) = nil error, want error listing valid values")
	}
}

func TestEnumMapping(t *testing.T) {
	t.Parallel()

	if got := alerts.CauseEnum("CONSTRUCTION"); got != gtfs.Alert_CONSTRUCTION {
		t.Errorf("CauseEnum(CONSTRUCTION) = %v, want CONSTRUCTION", got)
	}
	// An unmappable name must degrade, never panic: one bad row would
	// otherwise darken an entire region's feed.
	if got := alerts.CauseEnum("GARBAGE"); got != gtfs.Alert_UNKNOWN_CAUSE {
		t.Errorf("CauseEnum(GARBAGE) = %v, want UNKNOWN_CAUSE", got)
	}
	if got := alerts.EffectEnum("GARBAGE"); got != gtfs.Alert_UNKNOWN_EFFECT {
		t.Errorf("EffectEnum(GARBAGE) = %v, want UNKNOWN_EFFECT", got)
	}
	if got := alerts.SeverityEnum("GARBAGE"); got != gtfs.Alert_UNKNOWN_SEVERITY {
		t.Errorf("SeverityEnum(GARBAGE) = %v, want UNKNOWN_SEVERITY", got)
	}
}

func TestNormalizeLanguage(t *testing.T) {
	t.Parallel()

	for in, want := range map[string]string{"ES": "es", " es-MX ": "es-mx", "zh-Hans": "zh-hans"} {
		if got := alerts.NormalizeLanguage(in); got != want {
			t.Errorf("NormalizeLanguage(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSourceHash(t *testing.T) {
	t.Parallel()

	if alerts.SourceHash("a") == alerts.SourceHash("b") {
		t.Error("SourceHash collided on different inputs")
	}
	if alerts.SourceHash("a") != alerts.SourceHash("a") {
		t.Error("SourceHash is not deterministic")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/alerts/ -v`
Expected: FAIL — package `alerts` does not exist.

- [ ] **Step 3: Write `internal/alerts/alert.go`**

```go
// Package alerts holds the service alert domain model and the pure function
// that renders alerts into a GTFS-realtime feed.
//
// Nothing in this package performs I/O or reads the clock. Times are absolute
// instants; see the design spec §2.3.
package alerts

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
)

// Field names the translatable text fields of an alert. The GTFS-realtime
// url field is English-only and deliberately absent.
type Field string

const (
	FieldHeader      Field = "header"
	FieldDescription Field = "description"
)

// Translation is a non-English rendering of one alert field.
//
// SourceSHA256 is the hash of the English text the translation was made from.
// A translation whose hash no longer matches the current English is stale and
// is withheld from the feed, so riders read accurate English rather than
// outdated translated text.
type Translation struct {
	Language     string
	Field        Field
	Text         string
	SourceSHA256 string
}

// Alert is an authored service alert. Cause, Effect, and Severity hold
// GTFS-realtime enum names such as "CONSTRUCTION", not numbers.
type Alert struct {
	ID              int64
	RegionID        int64
	AgencyID        string
	HeaderText      string
	DescriptionText string
	URL             string
	Cause           string
	Effect          string
	Severity        string
	StartTime       time.Time
	EndTime         *time.Time
	Published       bool
	IsTest          bool
	Translations    []Translation
}

// SourceHash returns the hex SHA-256 of s, used for translation staleness.
func SourceHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// NormalizeLanguage lowercases and trims a BCP-47 tag. Normalizing in Go
// rather than with SQL collation keeps behavior identical across engines.
func NormalizeLanguage(tag string) string {
	return strings.ToLower(strings.TrimSpace(tag))
}
```

- [ ] **Step 4: Write `internal/alerts/enums.go`**

```go
package alerts

import (
	"fmt"
	"sort"
	"strings"

	"github.com/MobilityData/gtfs-realtime-bindings/golang/gtfs"
)

// Enum names are stored as text rather than numbers: portable across database
// engines, legible in `sidecar-admin alert list`, and stable if the protobuf
// numbering ever changes.

const (
	UnknownCause    = "UNKNOWN_CAUSE"
	UnknownEffect   = "UNKNOWN_EFFECT"
	UnknownSeverity = "UNKNOWN_SEVERITY"
)

var (
	causes = map[string]gtfs.Alert_Cause{
		"UNKNOWN_CAUSE":       gtfs.Alert_UNKNOWN_CAUSE,
		"OTHER_CAUSE":         gtfs.Alert_OTHER_CAUSE,
		"TECHNICAL_PROBLEM":   gtfs.Alert_TECHNICAL_PROBLEM,
		"STRIKE":              gtfs.Alert_STRIKE,
		"DEMONSTRATION":       gtfs.Alert_DEMONSTRATION,
		"ACCIDENT":            gtfs.Alert_ACCIDENT,
		"HOLIDAY":             gtfs.Alert_HOLIDAY,
		"WEATHER":             gtfs.Alert_WEATHER,
		"MAINTENANCE":         gtfs.Alert_MAINTENANCE,
		"CONSTRUCTION":        gtfs.Alert_CONSTRUCTION,
		"POLICE_ACTIVITY":     gtfs.Alert_POLICE_ACTIVITY,
		"MEDICAL_EMERGENCY":   gtfs.Alert_MEDICAL_EMERGENCY,
	}
	effects = map[string]gtfs.Alert_Effect{
		"NO_SERVICE":          gtfs.Alert_NO_SERVICE,
		"REDUCED_SERVICE":     gtfs.Alert_REDUCED_SERVICE,
		"SIGNIFICANT_DELAYS":  gtfs.Alert_SIGNIFICANT_DELAYS,
		"DETOUR":              gtfs.Alert_DETOUR,
		"ADDITIONAL_SERVICE":  gtfs.Alert_ADDITIONAL_SERVICE,
		"MODIFIED_SERVICE":    gtfs.Alert_MODIFIED_SERVICE,
		"OTHER_EFFECT":        gtfs.Alert_OTHER_EFFECT,
		"UNKNOWN_EFFECT":      gtfs.Alert_UNKNOWN_EFFECT,
		"STOP_MOVED":          gtfs.Alert_STOP_MOVED,
	}
	severities = map[string]gtfs.Alert_SeverityLevel{
		"UNKNOWN_SEVERITY": gtfs.Alert_UNKNOWN_SEVERITY,
		"INFO":             gtfs.Alert_INFO,
		"WARNING":          gtfs.Alert_WARNING,
		"SEVERE":           gtfs.Alert_SEVERE,
	}
)

func parseEnum[T any](kind, in, fallback string, table map[string]T) (string, error) {
	name := strings.ToUpper(strings.TrimSpace(in))
	if name == "" {
		return fallback, nil
	}
	if _, ok := table[name]; !ok {
		valid := make([]string, 0, len(table))
		for k := range table {
			valid = append(valid, k)
		}
		sort.Strings(valid)
		return "", fmt.Errorf("unknown %s %q; valid values: %s", kind, in, strings.Join(valid, ", "))
	}
	return name, nil
}

// ParseCause validates an author-supplied cause name. Empty means unknown.
func ParseCause(in string) (string, error) {
	return parseEnum("cause", in, UnknownCause, causes)
}

// ParseEffect validates an author-supplied effect name. Empty means unknown.
func ParseEffect(in string) (string, error) {
	return parseEnum("effect", in, UnknownEffect, effects)
}

// ParseSeverity validates an author-supplied severity name. Empty means unknown.
func ParseSeverity(in string) (string, error) {
	return parseEnum("severity", in, UnknownSeverity, severities)
}

// CauseEnum maps a stored name to its protobuf value. An unmappable name
// degrades to UNKNOWN_CAUSE rather than failing: names are validated at author
// time, so a bad value here means schema drift or a hand-edited row, and one
// such row must not darken the whole region's feed.
func CauseEnum(name string) gtfs.Alert_Cause {
	if v, ok := causes[name]; ok {
		return v
	}
	return gtfs.Alert_UNKNOWN_CAUSE
}

// EffectEnum maps a stored name to its protobuf value, degrading as CauseEnum does.
func EffectEnum(name string) gtfs.Alert_Effect {
	if v, ok := effects[name]; ok {
		return v
	}
	return gtfs.Alert_UNKNOWN_EFFECT
}

// SeverityEnum maps a stored name to its protobuf value, degrading as CauseEnum does.
func SeverityEnum(name string) gtfs.Alert_SeverityLevel {
	if v, ok := severities[name]; ok {
		return v
	}
	return gtfs.Alert_UNKNOWN_SEVERITY
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/alerts/ -v`
Expected: PASS. If a `gtfs.Alert_*` constant does not exist, run `go doc github.com/MobilityData/gtfs-realtime-bindings/golang/gtfs.Alert_Cause` and correct the table to the constants the pinned version actually defines.

- [ ] **Step 6: Commit**

```bash
git add internal/alerts/
git commit -m "Add alert domain types and GTFS-RT enum mapping

Enum names are stored as text for portability and legibility. Author-time
parsing rejects unknown names; render-time mapping degrades them to
UNKNOWN_* so a single drifted row cannot darken a region's feed."
```

---

### Task 3: The feed builder

The richest logic in the system, and testable with zero setup.

**Files:**
- Create: `internal/alerts/feed.go`
- Test: `internal/alerts/feed_test.go`

**Interfaces:**
- Consumes: Task 2's `Alert`, `Translation`, `SourceHash`, `*Enum` functions
- Produces: `func BuildFeed(alerts []Alert, opts FeedOptions) *gtfs.FeedMessage`; `type FeedOptions struct { Now time.Time; DefaultDuration time.Duration }`; `const DefaultDuration = 8 * time.Hour`; `const FeedLimit = 20`

- [ ] **Step 1: Write the failing test**

Create `internal/alerts/feed_test.go`:

```go
package alerts_test

import (
	"testing"
	"time"

	"github.com/MobilityData/gtfs-realtime-bindings/golang/gtfs"
	"google.golang.org/protobuf/proto"

	"github.com/OneBusAway/sidecar/internal/alerts"
)

var (
	now   = time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	start = time.Date(2026, 8, 15, 9, 0, 0, 0, time.UTC)
)

func opts() alerts.FeedOptions {
	return alerts.FeedOptions{Now: now, DefaultDuration: alerts.DefaultDuration}
}

func base() alerts.Alert {
	return alerts.Alert{
		ID: 7, RegionID: 1, AgencyID: "40",
		HeaderText: "Link delayed", DescriptionText: "Signal problem",
		Cause: "TECHNICAL_PROBLEM", Effect: "SIGNIFICANT_DELAYS", Severity: "WARNING",
		StartTime: start, Published: true,
	}
}

func TestBuildFeedHeader(t *testing.T) {
	t.Parallel()

	msg := alerts.BuildFeed(nil, opts())

	if got := msg.GetHeader().GetGtfsRealtimeVersion(); got != "1.0" {
		t.Errorf("version = %q, want 1.0", got)
	}
	if got := msg.GetHeader().GetIncrementality(); got != gtfs.FeedHeader_FULL_DATASET {
		t.Errorf("incrementality = %v, want FULL_DATASET", got)
	}
	if got := msg.GetHeader().GetTimestamp(); got != uint64(now.Unix()) {
		t.Errorf("timestamp = %d, want %d", got, now.Unix())
	}
	// An empty feed is still a valid feed; spec §15 requires the endpoint to
	// conform even when it always returns empty.
	if len(msg.GetEntity()) != 0 {
		t.Errorf("entity count = %d, want 0", len(msg.GetEntity()))
	}
}

func TestBuildFeedEntity(t *testing.T) {
	t.Parallel()

	msg := alerts.BuildFeed([]alerts.Alert{base()}, opts())

	if len(msg.GetEntity()) != 1 {
		t.Fatalf("entity count = %d, want 1", len(msg.GetEntity()))
	}
	e := msg.GetEntity()[0]
	if got := e.GetId(); got != "Alert_7" {
		t.Errorf("entity id = %q, want Alert_7", got)
	}

	a := e.GetAlert()
	if got := a.GetCause(); got != gtfs.Alert_TECHNICAL_PROBLEM {
		t.Errorf("cause = %v", got)
	}
	if got := a.GetEffect(); got != gtfs.Alert_SIGNIFICANT_DELAYS {
		t.Errorf("effect = %v", got)
	}
	if got := a.GetSeverityLevel(); got != gtfs.Alert_WARNING {
		t.Errorf("severity = %v", got)
	}

	if len(a.GetInformedEntity()) != 1 {
		t.Fatalf("informed_entity count = %d, want exactly 1", len(a.GetInformedEntity()))
	}
	if got := a.GetInformedEntity()[0].GetAgencyId(); got != "40" {
		t.Errorf("agency_id = %q, want 40", got)
	}
}

func TestActivePeriodDefaultsToEightHours(t *testing.T) {
	t.Parallel()

	msg := alerts.BuildFeed([]alerts.Alert{base()}, opts())
	tr := msg.GetEntity()[0].GetAlert().GetActivePeriod()

	if len(tr) != 1 {
		t.Fatalf("active_period count = %d, want 1", len(tr))
	}
	if got := tr[0].GetStart(); got != uint64(start.Unix()) {
		t.Errorf("start = %d, want %d", got, start.Unix())
	}
	// Absolute arithmetic on an instant: DST cannot affect this.
	if got, want := tr[0].GetEnd(), uint64(start.Add(8*time.Hour).Unix()); got != want {
		t.Errorf("end = %d, want %d (start + 8h)", got, want)
	}
}

func TestActivePeriodUsesExplicitEnd(t *testing.T) {
	t.Parallel()

	end := start.Add(30 * time.Minute)
	a := base()
	a.EndTime = &end

	tr := alerts.BuildFeed([]alerts.Alert{a}, opts()).GetEntity()[0].GetAlert().GetActivePeriod()
	if got := tr[0].GetEnd(); got != uint64(end.Unix()) {
		t.Errorf("end = %d, want %d", got, end.Unix())
	}
}

func TestEnglishFirstAndFreshTranslationsEmitted(t *testing.T) {
	t.Parallel()

	a := base()
	a.Translations = []alerts.Translation{
		{Language: "fr", Field: alerts.FieldHeader, Text: "Link retarde", SourceSHA256: alerts.SourceHash(a.HeaderText)},
		{Language: "es", Field: alerts.FieldHeader, Text: "Link retrasado", SourceSHA256: alerts.SourceHash(a.HeaderText)},
	}

	got := alerts.BuildFeed([]alerts.Alert{a}, opts()).GetEntity()[0].GetAlert().GetHeaderText().GetTranslation()

	want := []struct{ lang, text string }{
		{"en", "Link delayed"},
		{"es", "Link retrasado"}, // sorted by tag, so output is byte-stable
		{"fr", "Link retarde"},
	}
	if len(got) != len(want) {
		t.Fatalf("translation count = %d, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].GetLanguage() != w.lang || got[i].GetText() != w.text {
			t.Errorf("translation[%d] = (%q, %q), want (%q, %q)",
				i, got[i].GetLanguage(), got[i].GetText(), w.lang, w.text)
		}
	}
}

func TestStaleTranslationWithheld(t *testing.T) {
	t.Parallel()

	a := base()
	a.Translations = []alerts.Translation{{
		Language: "es", Field: alerts.FieldHeader, Text: "Texto viejo",
		SourceSHA256: alerts.SourceHash("an older English header"),
	}}

	got := alerts.BuildFeed([]alerts.Alert{a}, opts()).GetEntity()[0].GetAlert().GetHeaderText().GetTranslation()

	if len(got) != 1 || got[0].GetLanguage() != "en" {
		t.Fatalf("got %d translations, want only English; stale translation must be withheld", len(got))
	}
}

func TestTranslationsAreFieldScoped(t *testing.T) {
	t.Parallel()

	a := base()
	a.Translations = []alerts.Translation{{
		Language: "es", Field: alerts.FieldDescription, Text: "Problema de senal",
		SourceSHA256: alerts.SourceHash(a.DescriptionText),
	}}

	msg := alerts.BuildFeed([]alerts.Alert{a}, opts()).GetEntity()[0].GetAlert()
	if n := len(msg.GetHeaderText().GetTranslation()); n != 1 {
		t.Errorf("header translations = %d, want 1 (English only)", n)
	}
	if n := len(msg.GetDescriptionText().GetTranslation()); n != 2 {
		t.Errorf("description translations = %d, want 2", n)
	}
}

func TestURLIsEnglishOnlyAndOmittedWhenEmpty(t *testing.T) {
	t.Parallel()

	a := base()
	if u := alerts.BuildFeed([]alerts.Alert{a}, opts()).GetEntity()[0].GetAlert().GetUrl(); u != nil {
		t.Errorf("url = %v, want nil when empty", u)
	}

	a.URL = "https://example.org/alert"
	tr := alerts.BuildFeed([]alerts.Alert{a}, opts()).GetEntity()[0].GetAlert().GetUrl().GetTranslation()
	if len(tr) != 1 || tr[0].GetLanguage() != "en" {
		t.Errorf("url translations = %+v, want exactly one English entry", tr)
	}
}

func TestBuilderPreservesInputOrderAndDoesNotFilter(t *testing.T) {
	t.Parallel()

	// SQL owns filtering, ordering, and the cap. The builder renders what it
	// is given, in the order given — including unpublished or test alerts,
	// which SQL is responsible for excluding.
	a1, a2 := base(), base()
	a2.ID, a2.IsTest, a2.Published = 8, true, false

	msg := alerts.BuildFeed([]alerts.Alert{a1, a2}, opts())
	if len(msg.GetEntity()) != 2 {
		t.Fatalf("entity count = %d, want 2", len(msg.GetEntity()))
	}
	if msg.GetEntity()[0].GetId() != "Alert_7" || msg.GetEntity()[1].GetId() != "Alert_8" {
		t.Error("builder reordered its input")
	}
}

func TestFeedMarshalsBothEncodings(t *testing.T) {
	t.Parallel()

	msg := alerts.BuildFeed([]alerts.Alert{base()}, opts())
	if _, err := proto.Marshal(msg); err != nil {
		t.Fatalf("proto.Marshal: %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/alerts/ -run 'TestBuildFeed|TestActive|TestEnglish|TestStale|TestTranslations|TestURL|TestBuilder|TestFeedMarshals' -v`
Expected: FAIL — `undefined: alerts.BuildFeed`.

- [ ] **Step 3: Write `internal/alerts/feed.go`**

```go
package alerts

import (
	"fmt"
	"sort"
	"time"

	"github.com/MobilityData/gtfs-realtime-bindings/golang/gtfs"
)

const (
	// FeedLimit caps the feed. This is a "current conditions" feed, not an
	// archive; the apps re-fetch frequently.
	FeedLimit = 20

	// DefaultDuration is advertised as the active period when an author set
	// no end time. Without it an open-ended alert pins itself to the top of
	// riders' feeds forever.
	DefaultDuration = 8 * time.Hour

	feedVersion  = "1.0"
	englishTag   = "en"
	entityPrefix = "Alert_"
)

// FeedOptions carries everything BuildFeed needs from the outside world, so
// the function itself stays pure and deterministic.
type FeedOptions struct {
	Now             time.Time
	DefaultDuration time.Duration
}

// BuildFeed renders alerts into a GTFS-realtime FeedMessage.
//
// It applies no filtering, ordering, or capping: SQL decides which alerts
// appear and in what order, this function decides what they look like on the
// wire. Callers pass rows already filtered to published (plus the test flag),
// ordered start_time DESC then id DESC, and capped at FeedLimit.
func BuildFeed(in []Alert, opts FeedOptions) *gtfs.FeedMessage {
	dur := opts.DefaultDuration
	if dur <= 0 {
		dur = DefaultDuration
	}

	version := feedVersion
	incrementality := gtfs.FeedHeader_FULL_DATASET
	timestamp := uint64(opts.Now.Unix())

	msg := &gtfs.FeedMessage{
		Header: &gtfs.FeedHeader{
			GtfsRealtimeVersion: &version,
			Incrementality:      &incrementality,
			Timestamp:           &timestamp,
		},
		Entity: make([]*gtfs.FeedEntity, 0, len(in)),
	}

	for i := range in {
		a := in[i]
		id := entityPrefix + fmt.Sprint(a.ID)
		msg.Entity = append(msg.Entity, &gtfs.FeedEntity{
			Id:    &id,
			Alert: buildAlert(a, dur),
		})
	}
	return msg
}

func buildAlert(a Alert, dur time.Duration) *gtfs.Alert {
	start := uint64(a.StartTime.Unix())
	end := uint64(a.StartTime.Add(dur).Unix())
	if a.EndTime != nil {
		end = uint64(a.EndTime.Unix())
	}

	agencyID := a.AgencyID
	cause := CauseEnum(a.Cause)
	effect := EffectEnum(a.Effect)
	severity := SeverityEnum(a.Severity)

	out := &gtfs.Alert{
		ActivePeriod:    []*gtfs.TimeRange{{Start: &start, End: &end}},
		InformedEntity:  []*gtfs.EntitySelector{{AgencyId: &agencyID}},
		Cause:           &cause,
		Effect:          &effect,
		SeverityLevel:   &severity,
		HeaderText:      translated(a.HeaderText, a.Translations, FieldHeader),
		DescriptionText: translated(a.DescriptionText, a.Translations, FieldDescription),
	}
	if a.URL != "" {
		// url is English-only per the feed contract.
		out.Url = translated(a.URL, nil, FieldHeader)
	}
	return out
}

// translated builds a TranslatedString with English first, followed by any
// non-stale translations sorted by language tag.
//
// Sorting matters: ranging a map would vary the wire output between runs.
// Staleness is per-field — a translation made from an older English source is
// withheld so riders fall back to accurate English.
func translated(english string, all []Translation, field Field) *gtfs.TranslatedString {
	lang := englishTag
	text := english
	out := &gtfs.TranslatedString{
		Translation: []*gtfs.TranslatedString_Translation{{Language: &lang, Text: &text}},
	}

	want := SourceHash(english)
	fresh := make([]Translation, 0, len(all))
	for _, t := range all {
		if t.Field == field && t.SourceSHA256 == want {
			fresh = append(fresh, t)
		}
	}
	sort.Slice(fresh, func(i, j int) bool { return fresh[i].Language < fresh[j].Language })

	for i := range fresh {
		out.Translation = append(out.Translation, &gtfs.TranslatedString_Translation{
			Language: &fresh[i].Language,
			Text:     &fresh[i].Text,
		})
	}
	return out
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/alerts/ -v`
Expected: PASS, all cases.

- [ ] **Step 5: Run the full check**

Run: `make check`
Expected: PASS under both timezones.

- [ ] **Step 6: Commit**

```bash
git add internal/alerts/
git commit -m "Add pure GTFS-realtime feed builder

BuildFeed renders alerts to protobuf and does nothing else: no filtering,
ordering, or capping, all of which belong to SQL. Translations are sorted
by language tag so wire output is byte-stable, and stale ones are withheld
per field so riders fall back to accurate English."
```

---

### Task 4: Schema, migrations, and sqlc queries

**Files:**
- Create: `internal/store/sqlite/migrations/00001_initial_schema.sql`
- Create: `internal/store/sqlite/migrations/embed.go`
- Create: `internal/store/sqlite/queries/regions.sql`
- Create: `internal/store/sqlite/queries/alerts.sql`
- Create: `internal/store/sqlite/gen/` (generated)

**Interfaces:**
- Consumes: nothing
- Produces: `migrations.FS` (an `embed.FS`); generated package `gen` with `gen.New(db)`, `gen.Queries`, and the query methods named below

- [ ] **Step 1: Write the migration**

Create `internal/store/sqlite/migrations/00001_initial_schema.sql`:

```sql
-- +goose Up
CREATE TABLE regions (
  id                INTEGER PRIMARY KEY,
  region_name       TEXT    NOT NULL,
  oba_base_url      TEXT    NOT NULL,
  sidecar_base_url  TEXT    NOT NULL DEFAULT '',
  language          TEXT    NOT NULL DEFAULT '',
  active            BOOLEAN NOT NULL DEFAULT TRUE,
  default_agency_id TEXT    NOT NULL DEFAULT '',
  timezone          TEXT    NOT NULL DEFAULT 'UTC',
  synced_at         INTEGER NOT NULL,
  created_at        INTEGER NOT NULL,
  updated_at        INTEGER NOT NULL
);

CREATE TABLE alerts (
  id               INTEGER PRIMARY KEY AUTOINCREMENT,
  region_id        INTEGER NOT NULL REFERENCES regions(id) ON DELETE CASCADE,
  agency_id        TEXT    NOT NULL,
  header_text      TEXT    NOT NULL,
  description_text TEXT    NOT NULL DEFAULT '',
  url              TEXT    NOT NULL DEFAULT '',
  cause            TEXT    NOT NULL DEFAULT 'UNKNOWN_CAUSE',
  effect           TEXT    NOT NULL DEFAULT 'UNKNOWN_EFFECT',
  severity_level   TEXT    NOT NULL DEFAULT 'UNKNOWN_SEVERITY',
  start_time       INTEGER NOT NULL,
  end_time         INTEGER,
  published        BOOLEAN NOT NULL DEFAULT FALSE,
  is_test          BOOLEAN NOT NULL DEFAULT FALSE,
  created_at       INTEGER NOT NULL,
  updated_at       INTEGER NOT NULL
);

CREATE INDEX alerts_feed_idx ON alerts (region_id, published, start_time DESC, id DESC);

CREATE TABLE alert_translations (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  alert_id      INTEGER NOT NULL REFERENCES alerts(id) ON DELETE CASCADE,
  language      TEXT    NOT NULL CHECK (language <> 'en'),
  field         TEXT    NOT NULL CHECK (field IN ('header', 'description')),
  text          TEXT    NOT NULL,
  source_sha256 TEXT    NOT NULL,
  created_at    INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL,
  UNIQUE (alert_id, language, field)
);

-- +goose Down
DROP TABLE alert_translations;
DROP TABLE alerts;
DROP TABLE regions;
```

Note `url` and `default_agency_id` are `NOT NULL DEFAULT ''` rather than nullable: empty
string and NULL would render identically in the feed, and avoiding `sql.NullString` keeps
the generated types simpler. `end_time` stays nullable because NULL is meaningful there —
it selects the 8-hour fallback.

- [ ] **Step 2: Embed the migrations**

Create `internal/store/sqlite/migrations/embed.go`:

```go
// Package migrations holds the embedded goose migration files for SQLite.
package migrations

import "embed"

// FS holds every migration, embedded so the binary needs no files on disk.
//
//go:embed *.sql
var FS embed.FS
```

- [ ] **Step 3: Write the region queries**

Create `internal/store/sqlite/queries/regions.sql`:

```sql
-- name: GetRegion :one
SELECT * FROM regions WHERE id = ?;

-- name: ListRegions :many
SELECT * FROM regions ORDER BY id;

-- name: UpsertRegionFromDirectory :exec
-- Partial upsert: default_agency_id and timezone are locally managed and must
-- survive every refresh. A full-row upsert would wipe them hourly, after which
-- alerts emit an empty agency_id.
INSERT INTO regions (
  id, region_name, oba_base_url, sidecar_base_url, language, active,
  synced_at, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (id) DO UPDATE SET
  region_name      = excluded.region_name,
  oba_base_url     = excluded.oba_base_url,
  sidecar_base_url = excluded.sidecar_base_url,
  language         = excluded.language,
  active           = excluded.active,
  synced_at        = excluded.synced_at,
  updated_at       = excluded.updated_at;

-- name: SetRegionLocalFields :exec
UPDATE regions
SET default_agency_id = ?, timezone = ?, updated_at = ?
WHERE id = ?;
```

- [ ] **Step 4: Write the alert queries**

Create `internal/store/sqlite/queries/alerts.sql`:

```sql
-- name: CreateAlert :one
INSERT INTO alerts (
  region_id, agency_id, header_text, description_text, url,
  cause, effect, severity_level, start_time, end_time,
  published, is_test, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING *;

-- name: GetAlert :one
SELECT * FROM alerts WHERE id = ?;

-- name: UpdateAlert :one
UPDATE alerts SET
  agency_id = ?, header_text = ?, description_text = ?, url = ?,
  cause = ?, effect = ?, severity_level = ?, start_time = ?, end_time = ?,
  is_test = ?, updated_at = ?
WHERE id = ?
RETURNING *;

-- name: SetAlertPublished :exec
UPDATE alerts SET published = ?, updated_at = ? WHERE id = ?;

-- name: DeleteAlert :exec
DELETE FROM alerts WHERE id = ?;

-- name: ListAlerts :many
SELECT * FROM alerts
WHERE (CAST(? AS INTEGER) = 0 OR region_id = CAST(? AS INTEGER))
ORDER BY start_time DESC, id DESC;

-- name: FeedAlerts :many
-- The test predicate is (is_test = FALSE OR :include_test). Writing
-- is_test = :include_test instead would return ONLY test alerts when
-- ?test=1, hiding every real alert from an agency verifying delivery.
SELECT * FROM alerts
WHERE region_id = ?
  AND published = TRUE
  AND (is_test = FALSE OR CAST(? AS BOOLEAN))
ORDER BY start_time DESC, id DESC
LIMIT ?;

-- name: FeedTranslations :many
-- The subquery repeats the feed predicate including ORDER BY and LIMIT so it
-- matches the same rows. Both statements run in one read transaction; without
-- that, a publish between them can shift the top-20 set and an alert in the
-- response silently loses its translations.
SELECT * FROM alert_translations WHERE alert_id IN (
  SELECT id FROM alerts
  WHERE region_id = ?
    AND published = TRUE
    AND (is_test = FALSE OR CAST(? AS BOOLEAN))
  ORDER BY start_time DESC, id DESC
  LIMIT ?
);

-- name: ListAlertTranslations :many
SELECT * FROM alert_translations WHERE alert_id = ? ORDER BY language, field;

-- name: UpsertAlertTranslation :exec
INSERT INTO alert_translations (
  alert_id, language, field, text, source_sha256, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (alert_id, language, field) DO UPDATE SET
  text          = excluded.text,
  source_sha256 = excluded.source_sha256,
  updated_at    = excluded.updated_at;
```

- [ ] **Step 5: Generate and verify it compiles**

```bash
sqlc generate
go build ./...
```

Expected: `internal/store/sqlite/gen/` is populated and the module builds. If sqlc
rejects a `CAST(? AS …)` form, replace that parameter with `sqlc.arg(name)` and re-run;
the goal is a typed `bool`/`int64` parameter, not the specific syntax.

- [ ] **Step 6: Commit**

```bash
git add internal/store/ sqlc.yaml
git commit -m "Add SQLite schema, migrations, and sqlc queries

Timestamps are epoch-second INTEGERs throughout. The region upsert is
partial so the hourly directory refresh cannot wipe locally-managed
columns, and the feed query's test predicate is (is_test = FALSE OR ?)
rather than an equality test that would return only test alerts."
```

---

### Task 5: Repository interfaces and the SQLite adapter

**Files:**
- Create: `internal/alerts/repository.go`
- Create: `internal/regions/region.go`
- Create: `internal/store/sqlite/store.go`
- Test: `internal/store/sqlite/store_test.go`

**Interfaces:**
- Consumes: Task 2 domain types; Task 4 `gen` package and `migrations.FS`
- Produces:
  - `alerts.Repository` with `Create(ctx, NewAlert) (Alert, error)`, `Get(ctx, int64) (Alert, error)`, `Update(ctx, int64, Patch) (Alert, error)`, `SetPublished(ctx, int64, bool) error`, `Delete(ctx, int64) error`, `List(ctx, ListFilter) ([]Alert, error)`, `Feed(ctx, regionID int64, includeTest bool, limit int) ([]Alert, error)`, `UpsertTranslation(ctx, int64, Translation) error`
  - `alerts.ErrNotFound`, `regions.ErrNotFound`
  - `regions.Repository` with `Get(ctx, int64) (Region, error)`, `List(ctx) ([]Region, error)`, `UpsertFromDirectory(ctx, []Region, time.Time) error`, `SetLocalFields(ctx, id int64, agencyID, timezone string, now time.Time) error`
  - `sqlite.Open(path string) (*sqlite.Store, error)`, `(*Store).Migrate() error`, `(*Store).Alerts() alerts.Repository`, `(*Store).Regions() regions.Repository`, `(*Store).Close() error`

- [ ] **Step 1: Define the alert repository contract**

Create `internal/alerts/repository.go`:

```go
package alerts

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound is returned when a lookup finds no row. Callers distinguish it
// with errors.Is; the HTTP layer maps it to 404.
var ErrNotFound = errors.New("alert not found")

// NewAlert is the input to Create. AgencyID is already resolved — the CLI
// applies the region default before calling, so the stored value never changes
// underneath a published alert.
type NewAlert struct {
	RegionID        int64
	AgencyID        string
	HeaderText      string
	DescriptionText string
	URL             string
	Cause           string
	Effect          string
	Severity        string
	StartTime       time.Time
	EndTime         *time.Time
	IsTest          bool
}

// Patch carries an edit. A nil field means "leave unchanged"; this is why
// every field is a pointer rather than a value.
type Patch struct {
	AgencyID        *string
	HeaderText      *string
	DescriptionText *string
	URL             *string
	Cause           *string
	Effect          *string
	Severity        *string
	StartTime       *time.Time
	EndTime         *time.Time
	ClearEndTime    bool // distinct from EndTime == nil, which means "unchanged"
	IsTest          *bool
}

// ListFilter selects alerts for administrative listing. RegionID of 0 means
// every region — note this is safe only because ListFilter is never used for
// the feed, where region 0 (Tampa Bay) is a real region.
type ListFilter struct {
	RegionID int64
}

// Repository stores alerts. Implementations must be safe for concurrent use.
type Repository interface {
	Create(ctx context.Context, in NewAlert, now time.Time) (Alert, error)
	Get(ctx context.Context, id int64) (Alert, error)
	Update(ctx context.Context, id int64, p Patch, now time.Time) (Alert, error)
	SetPublished(ctx context.Context, id int64, published bool, now time.Time) error
	Delete(ctx context.Context, id int64) error
	List(ctx context.Context, f ListFilter) ([]Alert, error)

	// Feed returns published alerts for one region, newest first, capped at
	// limit, with translations attached. Implementations run both queries in a
	// single read transaction.
	Feed(ctx context.Context, regionID int64, includeTest bool, limit int) ([]Alert, error)

	UpsertTranslation(ctx context.Context, alertID int64, t Translation, now time.Time) error
}
```

- [ ] **Step 2: Define the region domain and contract**

Create `internal/regions/region.go`:

```go
// Package regions holds the region domain model, the directory client that
// populates it, and the periodic refresh loop.
package regions

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound is returned when no region has the requested id. The HTTP layer
// maps it to the 404 contract.
var ErrNotFound = errors.New("region not found")

// Region is one entry from the regions directory, plus locally-managed fields
// the directory does not supply.
type Region struct {
	ID             int64
	Name           string
	OBABaseURL     string
	SidecarBaseURL string
	Language       string
	Active         bool

	// Locally managed. The directory carries neither, and the refresh must
	// never overwrite them.
	DefaultAgencyID string
	Timezone        string
}

// Repository stores regions. Implementations must be safe for concurrent use.
type Repository interface {
	Get(ctx context.Context, id int64) (Region, error)
	List(ctx context.Context) ([]Region, error)

	// UpsertFromDirectory writes directory-sourced columns only, leaving
	// DefaultAgencyID and Timezone untouched. It never deletes rows: alerts
	// cascade from regions, so removing a region that vanished upstream would
	// destroy every alert authored for it.
	UpsertFromDirectory(ctx context.Context, in []Region, now time.Time) error

	SetLocalFields(ctx context.Context, id int64, agencyID, timezone string, now time.Time) error
}
```

- [ ] **Step 3: Write the failing adapter test**

Create `internal/store/sqlite/store_test.go`:

```go
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

	store, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
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

	got, err := store.Regions().Get(ctx, 1)
	if err != nil {
		t.Fatalf("Get: %v", err)
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
```

- [ ] **Step 4: Run test to verify it fails**

Run: `go test ./internal/store/sqlite/ -v`
Expected: FAIL — `undefined: sqlite.Open`.

- [ ] **Step 5: Write the adapter**

Create `internal/store/sqlite/store.go`. Implement `Open`, `Migrate`, `Alerts`, `Regions`,
`Close`, plus the two repository implementations mapping `gen` rows to domain types.

Key requirements:

```go
// Open connects with the pragmas this design depends on:
//   _pragma=journal_mode(WAL)   server reads and CLI writes coexist
//   _pragma=busy_timeout(5000)  block briefly rather than failing on a lock
//   _pragma=foreign_keys(ON)    SQLite disables FK enforcement by default
func Open(path string) (*Store, error) {
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	...
}

// Migrate runs the embedded goose migrations.
func (s *Store) Migrate() error {
	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("sqlite3"); err != nil { return err }
	return goose.Up(s.db, ".")
}
```

Conversion rules, applied consistently in both directions:

- `time.Time` ↔ `int64` via `.Unix()` and `time.Unix(n, 0).UTC()`. **Always `.UTC()`** on
  the way out, so no value ever carries the machine's local zone.
- `*time.Time` ↔ `sql.NullInt64` for `end_time`.
- `sql.ErrNoRows` → the package's `ErrNotFound`, wrapped with `%w`.
- `Feed` opens a read transaction, runs `FeedAlerts` then `FeedTranslations`, groups
  translations by `alert_id`, attaches them, and returns alerts in query order.
- `Update` reads the current row, applies non-nil `Patch` fields, honours
  `ClearEndTime`, and writes back.

- [ ] **Step 6: Run tests to verify they pass**

Run: `go test ./internal/store/sqlite/ -v`
Expected: PASS.

- [ ] **Step 7: Run the full check**

Run: `make check`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/alerts/repository.go internal/regions/region.go internal/store/
git commit -m "Add repository interfaces and SQLite adapter

Domain types cross the boundary, not generated structs, so a Postgres
adapter can satisfy the same interfaces. Feed runs both its queries in one
read transaction, and every time value is converted through UTC so no
machine-local zone can leak into stored data."
```

---

### Task 6: Shared conformance suite

The tests that make portability real rather than aspirational.

**Files:**
- Create: `internal/store/storetest/storetest.go`
- Modify: `internal/store/sqlite/store_test.go`

**Interfaces:**
- Consumes: Task 5 repository interfaces
- Produces: `func RunAlertRepository(t *testing.T, newStore func(*testing.T) (alerts.Repository, regions.Repository))`

- [ ] **Step 1: Write the suite**

Create `internal/store/storetest/storetest.go`. It is a normal (non-`_test`) package so
other engines can import it. Cover, each as its own `t.Run` subtest:

1. **Create/Get round trip** — every field survives, including `EndTime == nil`.
2. **Drafts are invisible to the feed** — create, do not publish, `Feed` returns empty; publish, `Feed` returns it; unpublish, empty again.
3. **Test alerts excluded by default** — one normal and one test alert; `includeTest=false` returns one, `includeTest=true` returns **both**. Assert the normal alert is present in the `includeTest=true` result — this is the assertion that catches an `is_test = ?` predicate.
4. **Ordering is newest-first with a deterministic tie-break** — three alerts, two sharing a `start_time`; assert exact id order, and that repeated calls return the identical order.
5. **The cap holds** — insert 25, assert `Feed(limit=20)` returns exactly 20 and they are the 20 newest.
6. **Region scoping** — alerts in region 1 never appear in region 2's feed. Include a region with **id 0** to prove zero is a real region rather than a sentinel.
7. **`start_time > 2^31`** — store `time.Unix(1<<31+86400, 0)`, read it back, assert exact equality. This fails loudly if a future Postgres schema uses `INTEGER` instead of `BIGINT`.
8. **Partial upsert preserves local fields** — set `DefaultAgencyID` and `Timezone`, run `UpsertFromDirectory` with changed directory fields, assert directory fields updated and local fields unchanged.
9. **Upsert never deletes** — upsert regions {1,2}, then upsert {1} alone; region 2 and its alerts still exist.
10. **Translation upsert replaces by (alert, language, field)** — two upserts for the same triple leave one row with the newer text.
11. **Feed attaches translations** to the right alerts.
12. **Delete cascades** — deleting an alert removes its translations.
13. **`Update` patch semantics** — a nil field leaves the value alone; `ClearEndTime` sets NULL.

Each subtest gets a fresh store from `newStore`.

- [ ] **Step 2: Wire SQLite into the suite**

Append to `internal/store/sqlite/store_test.go`:

```go
func TestConformance(t *testing.T) {
	t.Parallel()

	storetest.RunAlertRepository(t, func(t *testing.T) (alerts.Repository, regions.Repository) {
		t.Helper()
		store, err := sqlite.Open(filepath.Join(t.TempDir(), "conformance.db"))
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		t.Cleanup(func() { _ = store.Close() })
		if err := store.Migrate(); err != nil {
			t.Fatalf("Migrate: %v", err)
		}
		return store.Alerts(), store.Regions()
	})
}
```

- [ ] **Step 3: Run and fix until green**

Run: `go test ./internal/store/... -v`
Expected: PASS. Failures here are real adapter bugs — fix the adapter, not the suite.

- [ ] **Step 4: Run the full check**

Run: `make check`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/store/
git commit -m "Add shared store conformance suite

Parameterized over implementations so a future Postgres adapter proves
equivalence against the same tests. Includes a start_time > 2^31 case that
fails loudly if that schema uses INTEGER, which is 32-bit in Postgres, and
asserts non-test alerts survive an includeTest query."
```

---

### Task 7: Region directory client and refresh loop

**Files:**
- Create: `internal/regions/directory.go`
- Create: `internal/regions/sync.go`
- Test: `internal/regions/directory_test.go`
- Create: `internal/regions/testdata/regions-v3.json`

**Interfaces:**
- Consumes: Task 5 `regions.Repository`
- Produces: `func NewClient(url string, opts ClientOptions) *Client`; `func (*Client) Fetch(ctx) ([]Region, error)`; `type ClientOptions struct { Timeout time.Duration; MaxBytes int64; MaxEntries int; MaxFieldLen int; HTTPClient *http.Client }`; `func DefaultClientOptions() ClientOptions`; `func Sync(ctx, *Client, Repository, func() time.Time) error`; `func RunSyncLoop(ctx, *Client, Repository, time.Duration, func() time.Time, *slog.Logger)`

- [ ] **Step 1: Capture a fixture**

```bash
mkdir -p internal/regions/testdata
curl -s --max-time 20 https://regions.onebusaway.org/regions-v3.json \
  -o internal/regions/testdata/regions-v3.json
```

If the fetch fails, hand-write a fixture with the same shape:
`{"version":3,"code":200,"text":"OK","data":{"list":[{"id":0,"regionName":"Tampa Bay","obaBaseUrl":"https://api.tampa.example.org/","sidecarBaseUrl":"https://sidecar.example.org","language":"en_US","active":true}]}}`

- [ ] **Step 2: Write the failing test**

Create `internal/regions/directory_test.go` covering:

- **Parses the real fixture** — at least one region; region id 0 present with name "Tampa Bay"; `Active` decoded.
- **Rejects an oversized body** — handler writes `MaxBytes+1` bytes; `Fetch` errors and does not hang.
- **Rejects too many entries** — a generated document with `MaxEntries+1` entries errors.
- **Skips invalid entries, keeps valid ones** — a document containing a negative id, a duplicate id, and an entry whose `regionName` exceeds `MaxFieldLen`, alongside two good entries: `Fetch` returns exactly the two good ones.
- **Honours the timeout** — handler sleeps past `Timeout`; `Fetch` returns an error promptly. Use `ClientOptions.Timeout = 50 * time.Millisecond`.
- **`Sync` preserves local fields** — seed a region, set local fields, sync a changed directory, assert directory fields updated and local fields intact.
- **`Sync` never removes** — sync {1,2}, then sync {1}; region 2 survives.
- **A failed fetch leaves rows untouched** — point the client at a closed server; `Sync` errors and existing rows are unchanged.

- [ ] **Step 3: Run test to verify it fails**

Run: `go test ./internal/regions/ -v`
Expected: FAIL — `undefined: regions.NewClient`.

- [ ] **Step 4: Implement the client**

Create `internal/regions/directory.go`. Requirements, each traceable to a specific failure:

```go
// DefaultClientOptions are sized for the real directory (~100 KB, ~50 entries).
//
// These are not tuning knobs, they are the defence. This is an unauthenticated
// ingest path from infrastructure the operator does not control, running at
// boot and hourly, whose contents populate the serving path.
func DefaultClientOptions() ClientOptions {
	return ClientOptions{
		Timeout:     30 * time.Second, // Go's default client has NO timeout
		MaxBytes:    5 << 20,          // an unbounded read is a memory amplifier
		MaxEntries:  10_000,
		MaxFieldLen: 512,
	}
}
```

- Body reads go through `io.LimitReader(resp.Body, MaxBytes+1)`; if the result is longer
  than `MaxBytes`, error rather than parse.
- Non-200 responses are an error.
- Entry validation: `id >= 0`; reject duplicates within one document; `regionName` and
  `obaBaseUrl` non-empty; every stored string within `MaxFieldLen`. An invalid entry is
  **skipped**, not fatal, so one bad row cannot block a refresh.
- Strip ASCII control characters from `regionName` — it is later printed to an admin's
  terminal by `region list`.
- `Sync` calls `Fetch`, then `UpsertFromDirectory`. It never deletes.
- `RunSyncLoop` runs `Sync` on a ticker, logging failures and continuing. It returns when
  the context is cancelled.

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/regions/ -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/regions/
git commit -m "Add region directory client with hostile-input limits

Timeout, body-size cap, entry cap, and per-field length caps, because this
is an unauthenticated ingest path from infrastructure the operator does not
control. Sync never deletes rows: alerts cascade from regions, so dropping
a region that vanished upstream would destroy its alerts."
```

---

### Task 8: HTTP handlers

**Files:**
- Create: `internal/httpapi/router.go`
- Create: `internal/httpapi/alerts.go`
- Test: `internal/httpapi/alerts_test.go`

**Interfaces:**
- Consumes: Task 3 `BuildFeed`; Task 5 repositories
- Produces: `func NewServer(cfg ServerConfig) *http.Server`; `func NewRouter(deps Deps) http.Handler`; `type Deps struct { Alerts alerts.Repository; Regions regions.Repository; Now func() time.Time; Logger *slog.Logger }`; `func ParseRegionSegment(string) (int64, bool)`

- [ ] **Step 1: Write the failing test**

Create `internal/httpapi/alerts_test.go` covering:

**Region segment parsing** — a table driven directly from the design spec:

```go
func TestParseRegionSegment(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"1", 1, true},
		{"1-puget-sound", 1, true},
		{"0", 0, true},   // Tampa Bay: zero is a real region
		{"007", 7, true}, // leading zeros
		{"92233720368547758081-x", 0, false}, // overflows int64
		{"abc", 0, false},
		{"-1", 0, false},
		{"+1", 0, false},
		{"", 0, false},
	}
	for _, tt := range tests {
		got, ok := httpapi.ParseRegionSegment(tt.in)
		if got != tt.want || ok != tt.ok {
			t.Errorf("ParseRegionSegment(%q) = (%d, %v), want (%d, %v)", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}
```

**Handler behavior** — against a real store seeded with fixtures:

- `GET …/1/alerts` → 200, `Content-Type: application/octet-stream`; body unmarshals into a `FeedMessage` whose entity ids match the published alerts.
- `GET …/1-puget-sound/alerts` → 200, same body.
- `GET …/999/alerts` → 404, `Content-Type: application/json`, body exactly `{"error":"Couldn't find Region"}`.
- **Every malformed segment returns 404, never 500** — drive the parse table's failing cases through the handler.
- `GET …/1/alerts.pbtext` → 200, `text/plain`; body unmarshals via `protojson` into the same message. Compare with `protocmp.Transform()`, never against a golden string.
- **`?test=` semantics**, four cases: absent → test alerts excluded; `?test=1` → included **and the non-test alert is still present**; `?test=0` → included (any non-blank value); `?test=%20` → excluded (whitespace is blank).
- Unpublished alerts never appear.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/httpapi/ -v`
Expected: FAIL — package does not exist.

- [ ] **Step 3: Implement**

`ParseRegionSegment` takes the leading run of digits, parses with `strconv.ParseInt`, and
returns `false` on no-digits or overflow. **Callers map `false` to 404, never 500** —
spec §1.2 makes unrecognised identifiers a normal condition.

```go
// includeTest reports whether test alerts should appear. Any non-blank value
// enables them, so ?test=0 includes them; blank means empty or whitespace-only,
// matching the reference implementation's Rails `blank?`.
func includeTest(r *http.Request) bool {
	return strings.TrimSpace(r.URL.Query().Get("test")) != ""
}
```

`NewServer` sets the timeouts this endpoint needs as an unauthenticated public service —
Go's defaults are all zero, and a trivial slowloris otherwise holds goroutines forever:

```go
&http.Server{
	Handler:           handler,
	ReadHeaderTimeout: 5 * time.Second,
	ReadTimeout:       10 * time.Second,
	WriteTimeout:      15 * time.Second,
	IdleTimeout:       60 * time.Second,
}
```

Routes, using stdlib patterns:

```go
mux.HandleFunc("GET /api/v1/regions/{regionId}/alerts", h.feedBinary)
mux.HandleFunc("GET /api/v1/regions/{regionId}/alerts.pbtext", h.feedText)
```

Both handlers: parse the segment → 404 on failure; `Regions().Get` → 404 on
`regions.ErrNotFound`; `Alerts().Feed(ctx, id, includeTest(r), alerts.FeedLimit)`;
`BuildFeed`; marshal. A store error is 500 with an empty body and a logged region id.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/httpapi/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/httpapi/
git commit -m "Add alerts feed HTTP handlers

Region segments parse to their leading integer and every malformed form
resolves to 404 rather than 500, since the spec treats unrecognised ids as
a normal condition. Explicit server timeouts, because this endpoint is
unauthenticated by design and Go's defaults are all zero."
```

---

### Task 9: Wire up the sidecar server

**Files:**
- Modify: `cmd/sidecar/main.go`
- Modify: `cmd/sidecar/main_test.go`
- Delete: `internal/greeting/` (superseded)

**Interfaces:**
- Consumes: Tasks 5, 7, 8
- Produces: a runnable server

- [ ] **Step 1: Write the failing test**

Rewrite `cmd/sidecar/main_test.go` to assert `run` returns an error for an unparseable
`--addr`, and that `--help` writes usage to stdout and returns nil. Keep it thin — the
behaviour lives in tested packages.

- [ ] **Step 2: Implement**

`run(stdout, stderr, args)`:

1. Parse flags: `--db` (env `SIDECAR_DB`, default `./sidecar.db`), `--addr` (default `:8080`), `--regions-url` (env `SIDECAR_REGIONS_URL`, default `https://regions.onebusaway.org/regions-v3.json`), `--refresh` (default `60m`).
2. `sqlite.Open`, then **`store.Migrate()` — before anything touches a table.** On failure, return the error so `main` exits non-zero: never serve on an unknown schema.
3. Start `RunSyncLoop` in a goroutine. Boot does **not** block on the first fetch beyond the client timeout; the server serves from existing rows meanwhile.
4. Build the router and `NewServer`, listen, and shut down gracefully on SIGINT/SIGTERM.

`main` is the only place that reads the clock: pass `time.Now` down as the `Now` function.

- [ ] **Step 3: Verify it runs end to end**

```bash
go run ./cmd/sidecar --db /tmp/sc.db --addr :8080 &
sleep 2
curl -si localhost:8080/api/v1/regions/1/alerts | head -5
curl -s localhost:8080/api/v1/regions/999/alerts
kill %1
```

Expected: 200 with `application/octet-stream` for a known region once the directory has
synced; `{"error":"Couldn't find Region"}` for 999.

- [ ] **Step 4: Remove the placeholder package**

```bash
git rm -r internal/greeting
```

- [ ] **Step 5: Run the full check**

Run: `make check`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add -A
git commit -m "Wire up the sidecar server

Migrations run at boot before the first directory fetch, so a fresh
database cannot have its first upsert fail against a missing table, and a
migration failure exits non-zero rather than serving on an unknown schema.
Removes the hello-world placeholder package."
```

---

### Task 10: `sidecar-admin` CLI

**Files:**
- Create: `cmd/sidecar-admin/main.go`
- Create: `cmd/sidecar-admin/commands.go`
- Test: `cmd/sidecar-admin/commands_test.go`

**Interfaces:**
- Consumes: Tasks 2, 5
- Produces: the `sidecar-admin` binary

- [ ] **Step 1: Write the failing test**

Create `cmd/sidecar-admin/commands_test.go` covering, each against a temp database via
the `run(stdout, stderr, args)` seam:

- **Round trip** — `region set`, `alert create`, `alert publish`, then read back through the repository and confirm the alert is in the feed with the right agency id.
- **A draft is not in the feed** — create without publish; `Feed` is empty.
- **Naive `--start` is rejected** — `--start "2026-08-15 14:00:00"` returns an error mentioning the region's timezone, and writes nothing.
- **`--start` before 2000 is rejected** — catches typo'd years and every negative epoch, which wrap to enormous values in the proto's `uint64` `TimeRange`.
- **`end <= start` is rejected** — otherwise publish succeeds and riders never see the alert, with no error anywhere.
- **Missing agency resolution is rejected** — no `--agency-id`, region has no default; error, nothing written.
- **Region default is applied** — `--agency-id` omitted with a region default set stores the default.
- **Unknown `--cause` is rejected** with a message listing valid values.
- **Unknown `--timezone` is rejected** at `region set` — validated with `time.LoadLocation` at the point of the mistake.
- **`region set` on an unknown id errors** rather than inserting.
- **`alert translate` then feed** — a fresh translation appears; editing the English header afterwards withholds it.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/sidecar-admin/ -v`
Expected: FAIL — package does not exist.

- [ ] **Step 3: Implement**

`main.go` mirrors `cmd/sidecar`: `run(stdout, stderr io.Writer, args []string) error`,
with `main` mapping a non-nil error to exit 1. Import `_ "time/tzdata"` so
`time.LoadLocation` works in a scratch container with no system zone database.

`commands.go` dispatches `region` / `alert` / `migrate` subcommands with `flag.FlagSet`
per command.

Timestamp parsing is the subtle part:

```go
// parseInstant requires an explicit UTC offset. A naive datetime is rejected
// rather than guessed: interpreting it in the server's local zone would place
// an alert hours from where the author meant, and the directory carries no
// timezone to fall back on.
func parseInstant(s string, region regions.Region) (time.Time, error) {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf(
			"%q must be RFC 3339 with an explicit offset (e.g. 2026-08-15T14:00:00-07:00); "+
				"region %d is configured as %s", s, region.ID, region.Timezone)
	}
	return t.UTC(), nil
}
```

Validation before any write:

```go
var minStart = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

func validateWindow(start time.Time, end *time.Time, now time.Time) error {
	// TimeRange.start/end are uint64 in the proto, so a negative epoch wraps to
	// an enormous value instead of failing.
	if start.Before(minStart) {
		return fmt.Errorf("start %s is before %s; check the year", start, minStart.Format("2006"))
	}
	if start.After(now.AddDate(10, 0, 0)) {
		return fmt.Errorf("start %s is more than 10 years out; check the year", start)
	}
	if end != nil && !end.After(start) {
		// Publishing this would succeed and the alert would appear in the feed,
		// but apps hide out-of-window alerts, so riders would never see it and
		// nothing would report an error.
		return fmt.Errorf("end %s must be after start %s", end, start)
	}
	return nil
}
```

`alert list` renders each time in the region's zone plus UTC.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./cmd/sidecar-admin/ -v`
Expected: PASS.

- [ ] **Step 5: Verify end to end**

```bash
go build -o bin/sidecar-admin ./cmd/sidecar-admin
./bin/sidecar-admin --db /tmp/sc.db region list
./bin/sidecar-admin --db /tmp/sc.db region set --id 1 --agency-id 1 --timezone America/Los_Angeles
./bin/sidecar-admin --db /tmp/sc.db alert create --region 1 \
  --header "Route 44 detoured" --start 2026-08-15T14:00:00-07:00 --cause CONSTRUCTION --effect DETOUR
./bin/sidecar-admin --db /tmp/sc.db alert publish 1
./bin/sidecar-admin --db /tmp/sc.db alert list --region 1
```

Then start the server against the same database and confirm the alert appears in
`…/1/alerts.pbtext`.

- [ ] **Step 6: Run the full check**

Run: `make check`
Expected: PASS.

- [ ] **Step 7: Update the README**

Add a "Service alerts" section documenting the two endpoints and the `sidecar-admin`
workflow above.

- [ ] **Step 8: Commit**

```bash
git add -A
git commit -m "Add sidecar-admin CLI for authoring service alerts

Timestamps require an explicit RFC 3339 offset; a naive datetime is
rejected rather than guessed, since the regions directory carries no
timezone to interpret it against. Author-time validation rejects
pre-2000 starts, which wrap in the proto's uint64 TimeRange, and
end <= start, which would publish an alert riders can never see."
```

---

## Self-Review

**Spec coverage.** Every design section maps to a task: §2.1/§2.2 directory and partial
upsert → Tasks 4, 7; §2.3 epoch storage and the `time.Local` ban → Tasks 1, 4, 5; §2.4
author-time agency resolution → Tasks 5, 10; §2.5 lifecycle → Tasks 5, 6, 10; §2.6
translation staleness → Tasks 3, 4, 10; §2.7 portability → Tasks 4, 5, 6; §2.8 direct DB
access → Task 10; §3 data model → Task 4; §4.1–4.3 feed and HTTP → Tasks 3, 4, 8; §5
packages → all; §6 CLI → Task 10; §7 error handling → Tasks 7, 8, 9, 10; §8 testing →
every task; §9 dependencies → Task 1.

**Type consistency.** `Alert`, `Translation`, `Field`, `NewAlert`, `Patch`, `ListFilter`,
`FeedOptions`, and `Region` are defined once (Tasks 2, 3, 5) and referenced with those
exact names afterwards. `Repository` method signatures in Task 5 match their use in
Tasks 6–10, including the `now time.Time` parameter that keeps the clock injected.

**Known follow-ups**, deliberately excluded: alert push fan-out (spec §4), feed
ETag/caching, a CI workflow calling `make check`, and the Postgres adapter itself — for
which Task 6's conformance suite is the acceptance test.
