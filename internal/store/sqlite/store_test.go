package sqlite_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/alarms"
	"github.com/OneBusAway/sidecar/internal/alertpush"
	"github.com/OneBusAway/sidecar/internal/alerts"
	"github.com/OneBusAway/sidecar/internal/apikey"
	"github.com/OneBusAway/sidecar/internal/auth"
	"github.com/OneBusAway/sidecar/internal/liveactivities"
	"github.com/OneBusAway/sidecar/internal/pushreg"
	"github.com/OneBusAway/sidecar/internal/regions"
	"github.com/OneBusAway/sidecar/internal/store/sqlitetest"
	"github.com/OneBusAway/sidecar/internal/store/storetest"
	"github.com/OneBusAway/sidecar/internal/surveys"

	_ "modernc.org/sqlite"
)

func TestOpenMigrateAndRoundTrip(t *testing.T) {
	t.Parallel()

	store := sqlitetest.Open(t)

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

// TestMigrateCreatesAuthTables checks that migrating creates the users and
// sessions tables. It opens its own *sql.DB on the path OpenAt returns
// rather than going through store.Regions()/store.Alerts(): this test file
// is package sqlite_test (external), so it cannot reach the Store's
// unexported db field, and Store intentionally has no public DB() accessor.
func TestMigrateCreatesAuthTables(t *testing.T) {
	t.Parallel()

	path, _ := sqlitetest.OpenAt(t)

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	for _, table := range []string{"users", "sessions"} {
		var n int
		err := db.QueryRowContext(ctx,
			"SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&n)
		if err != nil || n != 1 {
			t.Fatalf("table %s missing after migrate (err=%v)", table, err)
		}
	}
}

// TestMigrateDeclaresTimeColumnsAsInteger pins the declared type of every
// column the migrations use to hold a time value -- epoch seconds everywhere
// except alarms.service_date, which is epoch milliseconds. All of them are
// INTEGER, never DATETIME or TEXT.
//
// Nothing else in the suite can hold this: SQLite's dynamic typing round-trips
// an int64 unchanged through a TEXT- or DATETIME-declared column, so every
// storetest conformance suite stays green against a mis-declared schema and
// cannot be the check. Reading the declared types back out of the catalog is
// the only assertion that bites, and it is engine-specific, which is why it
// lives here rather than in storetest.
//
// Every table in the schema is listed. A new table with a time column and no
// entry here is unpinned, so add it when you add the migration.
func TestMigrateDeclaresTimeColumnsAsInteger(t *testing.T) {
	t.Parallel()

	path, _ := sqlitetest.OpenAt(t)

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	wantIntegerColumns := map[string][]string{
		"regions":             {"synced_at", "created_at", "updated_at"},
		"alerts":              {"start_time", "end_time", "created_at", "updated_at"},
		"alert_translations":  {"created_at", "updated_at"},
		"users":               {"created_at", "updated_at"},
		"sessions":            {"created_at", "expires_at"},
		"push_registrations":  {"last_seen_at", "created_at", "updated_at"},
		"alarms":              {"service_date", "created_at", "updated_at"},
		"live_activities":     {"service_date", "last_pushed_at", "expires_at", "created_at", "updated_at"},
		"studies":             {"created_at", "updated_at"},
		"surveys":             {"start_time", "end_time", "created_at", "updated_at"},
		"survey_questions":    {"created_at", "updated_at"},
		"survey_responses":    {"created_at", "updated_at"},
		"alert_pushes":        {"started_at", "completed_at", "created_at", "updated_at"},
		"alert_push_failures": {"created_at"},
	}
	for table, columns := range wantIntegerColumns {
		types, err := columnTypes(ctx, db, table)
		if err != nil {
			t.Fatalf("PRAGMA table_info(%s): %v", table, err)
		}
		for _, column := range columns {
			got, ok := types[column]
			if !ok {
				t.Errorf("%s.%s missing after migrate", table, column)
				continue
			}
			if got != "INTEGER" {
				t.Errorf("%s.%s declared type = %q, want INTEGER (never DATETIME/TEXT)", table, column, got)
			}
		}
	}
}

// columnTypes returns each column's declared type for one table, as SQLite
// recorded it at CREATE TABLE time.
func columnTypes(ctx context.Context, db *sql.DB, table string) (map[string]string, error) {
	// PRAGMA does not accept a bound parameter for the table name; the values
	// here are test-local literals, not input.
	rows, err := db.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]string)
	for rows.Next() {
		var (
			cid        int
			name       string
			declType   string
			notNull    int
			defaultVal sql.NullString
			primaryKey int
		)
		if err := rows.Scan(&cid, &name, &declType, &notNull, &defaultVal, &primaryKey); err != nil {
			return nil, err
		}
		out[name] = declType
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// TestFeed exercises Feed through the generated Go bindings rather than by
// hand-running SQL: an earlier review verified the FeedAlerts/FeedTranslations
// SQL by hand and missed that a bare `LIMIT ?` mixed with an
// explicitly-numbered sqlc.arg was numbered as a fourth, unfilled
// placeholder, so every real Feed call failed with "missing argument with
// index 4" even though the hand-run SQL was correct.
func TestFeed(t *testing.T) {
	t.Parallel()

	store := sqlitetest.Open(t)

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

// TestUpdate_ConcurrentEditsDoNotLoseWrites reproduces the finding that
// alertRepo.Update performed GetAlert then UpdateAlert with no enclosing
// transaction: two concurrent `alert edit` invocations could both read the
// same pre-edit row, each merge only its own Patch field, and both write
// every column back, so the second silently discarded the first's edit --
// violating the Repository doc comment's promise that implementations are
// safe for concurrent use. Update now wraps the read-modify-write in one
// transaction; SQLite is single-writer, so what must never happen is both
// concurrent calls reporting success while the final row is missing one of
// the two edits -- a call reporting an error (lost the race to a lock or a
// stale snapshot) is an acceptable, non-silent outcome.
//
// The race this guards against is real but narrow (the unguarded version
// loses roughly 1-2 edits per 100 attempts in local measurement): a single
// pair of concurrent calls passes even against the old, unguarded code often
// enough to be useless as a regression test. Running many independent
// attempts and failing on the first lost update makes the test a reliable
// detector while staying well under a second.
func TestUpdate_ConcurrentEditsDoNotLoseWrites(t *testing.T) {
	t.Parallel()

	const attempts = 100

	for i := range attempts {
		store := sqlitetest.Open(t)
		ctx := context.Background()
		now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)

		if err := store.Regions().UpsertFromDirectory(ctx, []regions.Region{{
			ID: 1, Name: "Region 1", OBABaseURL: "https://example.org/", Active: true,
		}}, now); err != nil {
			t.Fatalf("attempt %d: UpsertFromDirectory: %v", i, err)
		}

		created, err := store.Alerts().Create(ctx, alerts.NewAlert{
			RegionID: 1, AgencyID: "40", HeaderText: "Original header",
			DescriptionText: "Original description",
			Cause:           "UNKNOWN_CAUSE", Effect: "UNKNOWN_EFFECT", Severity: "WARNING",
			StartTime: now,
		}, now)
		if err != nil {
			t.Fatalf("attempt %d: Create: %v", i, err)
		}

		newHeader := "Updated header"
		newDescription := "Updated description"

		var headerErr, descErr error
		var wg sync.WaitGroup
		start := make(chan struct{})

		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, headerErr = store.Alerts().Update(ctx, created.ID, alerts.Patch{HeaderText: &newHeader}, now.Add(time.Minute))
		}()
		go func() {
			defer wg.Done()
			<-start
			_, descErr = store.Alerts().Update(ctx, created.ID, alerts.Patch{DescriptionText: &newDescription}, now.Add(time.Minute))
		}()
		close(start)
		wg.Wait()

		if headerErr != nil && descErr != nil {
			t.Fatalf("attempt %d: both concurrent Update calls failed: header=%v, description=%v", i, headerErr, descErr)
		}

		got, err := store.Alerts().Get(ctx, created.ID)
		if err != nil {
			t.Fatalf("attempt %d: Get: %v", i, err)
		}

		// A call reporting an error legitimately did not apply its edit --
		// an acceptable outcome. What must never happen is a call reporting
		// SUCCESS while its edit is nonetheless missing from the final row.
		if headerErr == nil && got.HeaderText != newHeader {
			t.Fatalf("attempt %d: Update(header) reported success but final HeaderText = %q, want %q -- lost update", i, got.HeaderText, newHeader)
		}
		if descErr == nil && got.DescriptionText != newDescription {
			t.Fatalf("attempt %d: Update(description) reported success but final DescriptionText = %q, want %q -- lost update", i, got.DescriptionText, newDescription)
		}
	}
}

// TestConformance runs the shared store conformance suite against the
// SQLite adapter. When a Postgres adapter is added, it runs the same suite
// unchanged to prove behavioral equivalence.
func TestConformance(t *testing.T) {
	t.Parallel()

	storetest.RunAlertRepository(t, func(t *testing.T) (alerts.Repository, regions.Repository) {
		t.Helper()
		store := sqlitetest.Open(t)
		return store.Alerts(), store.Regions()
	})
}

// TestAuthRepositoryConformance runs the shared auth conformance suite against
// the SQLite adapter. The users/sessions migration test above asserts only
// that the tables exist; this suite is what pins the behavior that depends on
// their column types, foreign key, and expiry semantics.
func TestAuthRepositoryConformance(t *testing.T) {
	t.Parallel()

	storetest.RunAuthRepository(t, func(t *testing.T) auth.Repository {
		t.Helper()
		return sqlitetest.Open(t).Auth()
	})
}

// TestPushRegistrationConformance runs the shared push registration
// conformance suite against the SQLite adapter. When a Postgres adapter is
// added, it runs the same suite unchanged to prove behavioral equivalence.
func TestPushRegistrationConformance(t *testing.T) {
	t.Parallel()

	storetest.RunPushRegistrationRepository(t, func(t *testing.T) (pushreg.Repository, regions.Repository) {
		s := sqlitetest.Open(t)
		return s.PushRegs(), s.Regions()
	})
}

// TestAlarmConformance runs the shared alarm conformance suite against the
// SQLite adapter. When a Postgres adapter is added, it runs the same suite
// unchanged to prove behavioral equivalence.
func TestAlarmConformance(t *testing.T) {
	t.Parallel()

	storetest.RunAlarmRepository(t, func(t *testing.T) (alarms.Repository, regions.Repository) {
		s := sqlitetest.Open(t)
		return s.Alarms(), s.Regions()
	})
}

// TestLiveActivityRepositoryConformance runs the shared live activity suite
// against the SQLite adapter (design spec §8).
func TestLiveActivityRepositoryConformance(t *testing.T) {
	t.Parallel()
	storetest.RunLiveActivityRepository(t, func(t *testing.T) (liveactivities.Repository, regions.Repository) {
		t.Helper()
		store := sqlitetest.Open(t)
		return store.LiveActivities(), store.Regions()
	})
}

// TestSurveyConformance runs the shared survey conformance suite against
// the SQLite adapter through the production Open, so the _txlock=immediate
// DSN the concurrency subtest depends on is the one production uses.
func TestSurveyConformance(t *testing.T) {
	t.Parallel()

	storetest.RunSurveyRepository(t, func(t *testing.T) (surveys.Repository, regions.Repository) {
		s := sqlitetest.Open(t)
		return s.Surveys(), s.Regions()
	})
}

// TestAPIKeyRepositoryConformance runs the shared API key conformance suite
// against the SQLite adapter (design spec section 8).
func TestAPIKeyRepositoryConformance(t *testing.T) {
	t.Parallel()

	storetest.RunAPIKeyRepository(t, func(t *testing.T) (apikey.Repository, regions.Repository) {
		t.Helper()
		store := sqlitetest.Open(t)
		return store.APIKeys(), store.Regions()
	})
}

// TestAlertPushRepository runs the shared alert push conformance suite
// against the SQLite adapter (design spec sections 2.6-2.9).
func TestAlertPushRepository(t *testing.T) {
	t.Parallel()

	storetest.RunAlertPushRepository(t, func(t *testing.T) (alertpush.Repository, alerts.Repository, regions.Repository) {
		t.Helper()
		store := sqlitetest.Open(t)
		return store.AlertPushes(), store.Alerts(), store.Regions()
	})
}

// TestAlertPushFailuresStoreOnlyHashes pins design spec section 2.8: the
// failure table must never hold a plaintext device token. Only reading the
// stored column back through a raw connection can show this -- the
// conformance suite never sees the column, and RecordFailure would behave
// identically if it stored the token itself. This test file is package
// sqlite_test, so it opens its own *sql.DB on the path OpenAt returns, as
// TestMigrateCreatesAuthTables does.
func TestAlertPushFailuresStoreOnlyHashes(t *testing.T) {
	t.Parallel()

	const token = "PLAINTEXT-TOKEN"

	path, store := sqlitetest.OpenAt(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)

	if err := store.Regions().UpsertFromDirectory(ctx, []regions.Region{{
		ID: 1, Name: "Region 1", OBABaseURL: "https://example.org/", Active: true,
	}}, now); err != nil {
		t.Fatalf("UpsertFromDirectory: %v", err)
	}
	alert, alertErr := store.Alerts().Create(ctx, alerts.NewAlert{
		RegionID: 1, AgencyID: "40", HeaderText: "Route 44 detour",
		Cause: "CONSTRUCTION", Effect: "DETOUR", Severity: "WARNING", StartTime: now,
	}, now)
	if alertErr != nil {
		t.Fatalf("Create alert: %v", alertErr)
	}
	push, pushErr := store.AlertPushes().Create(ctx, alertpush.NewPush{
		AlertID: alert.ID, RegionID: 1, Audience: alertpush.AudienceAll,
		Messages: alertpush.Messages{"en": {Title: "Route 44 detour", Body: "Buses skip 3rd Ave."}},
	}, now)
	if pushErr != nil {
		t.Fatalf("Create push: %v", pushErr)
	}
	if _, err := store.AlertPushes().RecordFailure(ctx, push.ID, token, "Unregistered", now); err != nil {
		t.Fatalf("RecordFailure: %v", err)
	}

	db, openErr := sql.Open("sqlite", path)
	if openErr != nil {
		t.Fatalf("sql.Open: %v", openErr)
	}
	defer db.Close()

	var stored string
	if err := db.QueryRowContext(ctx, "SELECT token_sha256 FROM alert_push_failures").Scan(&stored); err != nil {
		t.Fatalf("SELECT token_sha256: %v", err)
	}
	if stored == token {
		t.Fatal("alert_push_failures.token_sha256 holds the plaintext token")
	}
	sum := sha256.Sum256([]byte(token))
	if want := hex.EncodeToString(sum[:]); stored != want {
		t.Errorf("token_sha256 = %q, want %q (hex sha256 of the token)", stored, want)
	}
}
