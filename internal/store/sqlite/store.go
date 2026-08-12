// Package sqlite is the SQLite adapter for the alerts, regions, and auth
// repositories. It maps generated sqlc rows to the domain types defined in
// internal/alerts, internal/regions, and internal/auth — nothing outside this
// package ever sees a gen.* struct, which is what lets a Postgres adapter
// satisfy the same interfaces later without touching any other package.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/pressly/goose/v3"
	_ "modernc.org/sqlite"

	"github.com/OneBusAway/sidecar/internal/alerts"
	"github.com/OneBusAway/sidecar/internal/auth"
	"github.com/OneBusAway/sidecar/internal/regions"
	"github.com/OneBusAway/sidecar/internal/store/sqlite/gen"
	"github.com/OneBusAway/sidecar/internal/store/sqlite/migrations"
)

// gooseConfigureOnce guards goose.SetBaseFS/goose.SetDialect, which mutate
// goose's own package-level globals rather than anything scoped to a *Store.
// Calling them from concurrent Migrate calls -- as parallel tests routinely
// do, each against its own database -- is a data race on those globals even
// though the migrations themselves target independent *sql.DB handles.
// goose.Up itself is not guarded here: it operates on the caller's own db
// and needs no serializing once the globals are set once.
var (
	gooseConfigureOnce sync.Once
	gooseConfigureErr  error
)

func configureGoose() error {
	gooseConfigureOnce.Do(func() {
		goose.SetBaseFS(migrations.FS)
		goose.SetLogger(goose.NopLogger())
		gooseConfigureErr = goose.SetDialect("sqlite3")
	})
	return gooseConfigureErr
}

// Store owns the database connection pool and hands out the two
// repositories. It is safe for concurrent use.
type Store struct {
	db *sql.DB
	q  *gen.Queries
}

// Open connects with the pragmas this design depends on:
//
//	_pragma=journal_mode(WAL)   server reads and CLI writes coexist
//	_pragma=busy_timeout(5000)  block briefly rather than failing on a lock
//	_pragma=foreign_keys(ON)    SQLite disables FK enforcement by default,
//	                            without which ON DELETE CASCADE silently
//	                            does nothing
func Open(path string) (*Store, error) {
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlite: open %s: %w", path, err)
	}
	return &Store{db: db, q: gen.New(db)}, nil
}

// Migrate runs the embedded goose migrations.
func (s *Store) Migrate() error {
	if err := configureGoose(); err != nil {
		return fmt.Errorf("sqlite: set dialect: %w", err)
	}
	if err := goose.Up(s.db, "."); err != nil {
		return fmt.Errorf("sqlite: migrate: %w", err)
	}
	return nil
}

// Close releases the underlying connection pool.
func (s *Store) Close() error {
	return s.db.Close()
}

// MigrationStatus describes one embedded migration's state, for
// `sidecar-admin migrate status`.
type MigrationStatus struct {
	Version int64
	Pending bool
}

// MigrationStatuses reports the state of every embedded migration, applied
// or pending, ordered by version.
//
// This builds its own goose.Provider scoped to s.db rather than going
// through the package-level goose.Status, whose backing Provider.Close would
// close s.db out from under every other Store method -- Provider is
// otherwise a plain read against s.db and needs no such cleanup here.
func (s *Store) MigrationStatuses(ctx context.Context) ([]MigrationStatus, error) {
	provider, err := goose.NewProvider(goose.DialectSQLite3, s.db, migrations.FS)
	if err != nil {
		return nil, fmt.Errorf("sqlite: migration status: new provider: %w", err)
	}

	statuses, err := provider.Status(ctx)
	if err != nil {
		return nil, fmt.Errorf("sqlite: migration status: %w", err)
	}

	out := make([]MigrationStatus, len(statuses))
	for i, st := range statuses {
		out[i] = MigrationStatus{
			Version: st.Source.Version,
			Pending: st.State == goose.StatePending,
		}
	}
	return out, nil
}

// Alerts returns the alerts.Repository backed by this store.
func (s *Store) Alerts() alerts.Repository {
	return &alertRepo{db: s.db, q: s.q}
}

// Regions returns the regions.Repository backed by this store.
func (s *Store) Regions() regions.Repository {
	return &regionRepo{db: s.db, q: s.q}
}

// Auth returns the auth.Repository backed by this store.
func (s *Store) Auth() auth.Repository {
	return &authRepo{db: s.db, q: s.q}
}

// unixToTime converts a stored epoch-seconds value to an absolute instant in
// UTC. Skipping .UTC() here would attach the machine's local zone to every
// value read from the database.
func unixToTime(n int64) time.Time {
	return time.Unix(n, 0).UTC()
}

// nullUnixToTime converts an optional epoch-seconds column to *time.Time.
func nullUnixToTime(n sql.NullInt64) *time.Time {
	if !n.Valid {
		return nil
	}
	t := unixToTime(n.Int64)
	return &t
}

// timeToNullUnix converts an optional instant to the NULLable column form.
func timeToNullUnix(t *time.Time) sql.NullInt64 {
	if t == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: t.Unix(), Valid: true}
}

// ---------------------------------------------------------------------------
// regions
// ---------------------------------------------------------------------------

type regionRepo struct {
	db *sql.DB
	q  *gen.Queries
}

func regionFromRow(r gen.Region) regions.Region {
	return regions.Region{
		ID:              r.ID,
		Name:            r.RegionName,
		OBABaseURL:      r.ObaBaseUrl,
		SidecarBaseURL:  r.SidecarBaseUrl,
		Language:        r.Language,
		Active:          r.Active,
		DefaultAgencyID: r.DefaultAgencyID,
		Timezone:        r.Timezone,
	}
}

func (r *regionRepo) Get(ctx context.Context, id int64) (regions.Region, error) {
	row, err := r.q.GetRegion(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return regions.Region{}, fmt.Errorf("sqlite: get region %d: %w", id, regions.ErrNotFound)
		}
		return regions.Region{}, fmt.Errorf("sqlite: get region %d: %w", id, err)
	}
	return regionFromRow(row), nil
}

func (r *regionRepo) List(ctx context.Context) ([]regions.Region, error) {
	rows, err := r.q.ListRegions(ctx)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list regions: %w", err)
	}
	out := make([]regions.Region, len(rows))
	for i, row := range rows {
		out[i] = regionFromRow(row)
	}
	return out, nil
}

// UpsertFromDirectory runs the whole batch in one write transaction: a
// per-row upsert with no enclosing transaction would let a mid-loop failure
// (e.g. SQLITE_BUSY past the busy_timeout) leave the table half-refreshed
// while Sync logs the refresh as failed -- an operator reading that log
// would believe nothing changed when rows are actually a mix of new and
// stale. Wrapping the loop makes it all-or-nothing.
func (r *regionRepo) UpsertFromDirectory(ctx context.Context, in []regions.Region, now time.Time) error {
	ts := now.Unix()

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: upsert regions: begin tx: %w", err)
	}
	//nolint:errcheck // rollback after a successful commit is a documented no-op; the error is expected and safe to ignore
	defer func() { _ = tx.Rollback() }()

	q := r.q.WithTx(tx)
	for _, reg := range in {
		if err := q.UpsertRegionFromDirectory(ctx, gen.UpsertRegionFromDirectoryParams{
			ID:             reg.ID,
			RegionName:     reg.Name,
			ObaBaseUrl:     reg.OBABaseURL,
			SidecarBaseUrl: reg.SidecarBaseURL,
			Language:       reg.Language,
			Active:         reg.Active,
			SyncedAt:       ts,
			CreatedAt:      ts,
			UpdatedAt:      ts,
		}); err != nil {
			return fmt.Errorf("sqlite: upsert region %d: %w", reg.ID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: upsert regions: commit: %w", err)
	}
	return nil
}

func (r *regionRepo) SetLocalFields(ctx context.Context, id int64, agencyID, timezone string, now time.Time) error {
	if err := r.q.SetRegionLocalFields(ctx, gen.SetRegionLocalFieldsParams{
		DefaultAgencyID: agencyID,
		Timezone:        timezone,
		UpdatedAt:       now.Unix(),
		ID:              id,
	}); err != nil {
		return fmt.Errorf("sqlite: set local fields for region %d: %w", id, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// alerts
// ---------------------------------------------------------------------------

type alertRepo struct {
	db *sql.DB
	q  *gen.Queries
}

func alertFromRow(a gen.Alert) alerts.Alert {
	return alerts.Alert{
		ID:              a.ID,
		RegionID:        a.RegionID,
		AgencyID:        a.AgencyID,
		HeaderText:      a.HeaderText,
		DescriptionText: a.DescriptionText,
		URL:             a.Url,
		Cause:           a.Cause,
		Effect:          a.Effect,
		Severity:        a.SeverityLevel,
		StartTime:       unixToTime(a.StartTime),
		EndTime:         nullUnixToTime(a.EndTime),
		Published:       a.Published,
		IsTest:          a.IsTest,
		CreatedAt:       unixToTime(a.CreatedAt),
		UpdatedAt:       unixToTime(a.UpdatedAt),
	}
}

func translationFromRow(t gen.AlertTranslation) alerts.Translation {
	return alerts.Translation{
		Language:     t.Language,
		Field:        alerts.Field(t.Field),
		Text:         t.Text,
		SourceSHA256: t.SourceSha256,
	}
}

func (r *alertRepo) Create(ctx context.Context, in alerts.NewAlert, now time.Time) (alerts.Alert, error) {
	// AgencyID is documented as pre-resolved by the caller (NewAlert), but
	// that contract was enforced only by comment: the column is NOT NULL but
	// not non-empty, so a caller that skips the resolve step (an HTTP admin
	// API, a bulk importer) would silently store an alert no app can match by
	// agency. Enforcing it here, rather than only in the CLI, protects every
	// caller of this Repository.
	if in.AgencyID == "" {
		return alerts.Alert{}, errors.New("sqlite: create alert: agency id must not be empty")
	}
	if err := alerts.ValidateWindow(in.StartTime, in.EndTime, now); err != nil {
		return alerts.Alert{}, fmt.Errorf("sqlite: create alert: %w", err)
	}

	ts := now.Unix()
	row, err := r.q.CreateAlert(ctx, gen.CreateAlertParams{
		RegionID:        in.RegionID,
		AgencyID:        in.AgencyID,
		HeaderText:      in.HeaderText,
		DescriptionText: in.DescriptionText,
		Url:             in.URL,
		Cause:           in.Cause,
		Effect:          in.Effect,
		SeverityLevel:   in.Severity,
		StartTime:       in.StartTime.Unix(),
		EndTime:         timeToNullUnix(in.EndTime),
		Published:       false,
		IsTest:          in.IsTest,
		CreatedAt:       ts,
		UpdatedAt:       ts,
	})
	if err != nil {
		return alerts.Alert{}, fmt.Errorf("sqlite: create alert: %w", err)
	}
	return alertFromRow(row), nil
}

func (r *alertRepo) Get(ctx context.Context, id int64) (alerts.Alert, error) {
	row, err := r.q.GetAlert(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return alerts.Alert{}, fmt.Errorf("sqlite: get alert %d: %w", id, alerts.ErrNotFound)
		}
		return alerts.Alert{}, fmt.Errorf("sqlite: get alert %d: %w", id, err)
	}
	a := alertFromRow(row)
	translations, err := r.q.ListAlertTranslations(ctx, id)
	if err != nil {
		return alerts.Alert{}, fmt.Errorf("sqlite: list translations for alert %d: %w", id, err)
	}
	for _, t := range translations {
		a.Translations = append(a.Translations, translationFromRow(t))
	}
	return a, nil
}

// Update wraps its read-modify-write in a single write transaction: GetAlert
// and UpdateAlert with no enclosing transaction would let two concurrent
// edits both read the same pre-edit row, each merge only its own Patch
// field, and both write every column back -- silently discarding whichever
// edit wrote second. The Repository doc comment promises implementations are
// safe for concurrent use; this is what makes that true. Follows the same
// BeginTx/WithTx pattern Feed already uses for its own two-query
// consistency.
func (r *alertRepo) Update(ctx context.Context, id int64, p alerts.Patch, now time.Time) (alerts.Alert, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return alerts.Alert{}, fmt.Errorf("sqlite: update alert %d: begin tx: %w", id, err)
	}
	//nolint:errcheck // rollback after a successful commit is a documented no-op; the error is expected and safe to ignore
	defer func() { _ = tx.Rollback() }()

	q := r.q.WithTx(tx)

	current, err := q.GetAlert(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return alerts.Alert{}, fmt.Errorf("sqlite: update alert %d: %w", id, alerts.ErrNotFound)
		}
		return alerts.Alert{}, fmt.Errorf("sqlite: update alert %d: %w", id, err)
	}

	if p.AgencyID != nil {
		current.AgencyID = *p.AgencyID
	}
	if p.HeaderText != nil {
		current.HeaderText = *p.HeaderText
	}
	if p.DescriptionText != nil {
		current.DescriptionText = *p.DescriptionText
	}
	if p.URL != nil {
		current.Url = *p.URL
	}
	if p.Cause != nil {
		current.Cause = *p.Cause
	}
	if p.Effect != nil {
		current.Effect = *p.Effect
	}
	if p.Severity != nil {
		current.SeverityLevel = *p.Severity
	}
	if p.StartTime != nil {
		current.StartTime = p.StartTime.Unix()
	}
	if p.ClearEndTime {
		current.EndTime = sql.NullInt64{}
	} else if p.EndTime != nil {
		current.EndTime = sql.NullInt64{Int64: p.EndTime.Unix(), Valid: true}
	}
	if p.IsTest != nil {
		current.IsTest = *p.IsTest
	}

	// Same invariant Create enforces (see the comment there): the column is
	// NOT NULL but not non-empty, so without this check Patch{AgencyID: &""}
	// would succeed and write agency_id = '', producing an
	// informed_entity{agency_id:""} in the feed that no OBA app matches.
	if current.AgencyID == "" {
		return alerts.Alert{}, fmt.Errorf("sqlite: update alert %d: agency id must not be empty", id)
	}

	if err = alerts.ValidateWindow(unixToTime(current.StartTime), nullUnixToTime(current.EndTime), now); err != nil {
		return alerts.Alert{}, fmt.Errorf("sqlite: update alert %d: %w", id, err)
	}

	row, err := q.UpdateAlert(ctx, gen.UpdateAlertParams{
		AgencyID:        current.AgencyID,
		HeaderText:      current.HeaderText,
		DescriptionText: current.DescriptionText,
		Url:             current.Url,
		Cause:           current.Cause,
		Effect:          current.Effect,
		SeverityLevel:   current.SeverityLevel,
		StartTime:       current.StartTime,
		EndTime:         current.EndTime,
		IsTest:          current.IsTest,
		UpdatedAt:       now.Unix(),
		ID:              id,
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return alerts.Alert{}, fmt.Errorf("sqlite: update alert %d: %w", id, alerts.ErrNotFound)
		}
		return alerts.Alert{}, fmt.Errorf("sqlite: update alert %d: %w", id, err)
	}

	if err := tx.Commit(); err != nil {
		return alerts.Alert{}, fmt.Errorf("sqlite: update alert %d: commit: %w", id, err)
	}
	return alertFromRow(row), nil
}

func (r *alertRepo) SetPublished(ctx context.Context, id int64, published bool, now time.Time) error {
	if _, err := r.q.SetAlertPublished(ctx, gen.SetAlertPublishedParams{
		Published: published,
		UpdatedAt: now.Unix(),
		ID:        id,
	}); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("sqlite: set published for alert %d: %w", id, alerts.ErrNotFound)
		}
		return fmt.Errorf("sqlite: set published for alert %d: %w", id, err)
	}
	return nil
}

func (r *alertRepo) Delete(ctx context.Context, id int64) error {
	if _, err := r.q.DeleteAlert(ctx, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("sqlite: delete alert %d: %w", id, alerts.ErrNotFound)
		}
		return fmt.Errorf("sqlite: delete alert %d: %w", id, err)
	}
	return nil
}

func (r *alertRepo) List(ctx context.Context, f alerts.ListFilter) ([]alerts.Alert, error) {
	var rows []gen.Alert
	var err error
	if f.RegionID == nil {
		rows, err = r.q.ListAlerts(ctx)
	} else {
		rows, err = r.q.ListAlertsByRegion(ctx, *f.RegionID)
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: list alerts: %w", err)
	}
	out := make([]alerts.Alert, len(rows))
	for i, row := range rows {
		out[i] = alertFromRow(row)
	}
	return out, nil
}

// Feed returns published alerts for one region, newest first, capped at
// limit, with translations attached. Both queries run inside a single read
// transaction: on separate pool connections a concurrent publish could shift
// the top-N set between the two queries, silently dropping translations from
// an alert in the response.
func (r *alertRepo) Feed(ctx context.Context, regionID int64, includeTest bool, limit int) ([]alerts.Alert, error) {
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("sqlite: feed: begin tx: %w", err)
	}
	//nolint:errcheck // rollback after a successful commit is a documented no-op; the error is expected and safe to ignore
	defer func() { _ = tx.Rollback() }()

	q := r.q.WithTx(tx)

	alertRows, err := q.FeedAlerts(ctx, gen.FeedAlertsParams{
		RegionID:    regionID,
		IncludeTest: includeTest,
		Limit:       int64(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("sqlite: feed: alerts: %w", err)
	}

	translationRows, err := q.FeedTranslations(ctx, gen.FeedTranslationsParams{
		RegionID:    regionID,
		IncludeTest: includeTest,
		Limit:       int64(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("sqlite: feed: translations: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("sqlite: feed: commit: %w", err)
	}

	byAlert := make(map[int64][]alerts.Translation, len(translationRows))
	for _, t := range translationRows {
		byAlert[t.AlertID] = append(byAlert[t.AlertID], translationFromRow(t))
	}

	out := make([]alerts.Alert, len(alertRows))
	for i, row := range alertRows {
		a := alertFromRow(row)
		a.Translations = byAlert[a.ID]
		out[i] = a
	}
	return out, nil
}

// UpsertTranslation normalizes t.Language itself rather than trusting the
// caller to have done so: the schema's UNIQUE(alert_id, language, field) is
// case-sensitive, so a caller that forgot to normalize (or a future caller
// that never knew to) would insert "ES" alongside an existing "es" -- two
// live rows for one language that the feed would both emit.
func (r *alertRepo) UpsertTranslation(ctx context.Context, alertID int64, t alerts.Translation, now time.Time) error {
	ts := now.Unix()
	if err := r.q.UpsertAlertTranslation(ctx, gen.UpsertAlertTranslationParams{
		AlertID:      alertID,
		Language:     alerts.NormalizeLanguage(t.Language),
		Field:        string(t.Field),
		Text:         t.Text,
		SourceSha256: t.SourceSHA256,
		CreatedAt:    ts,
		UpdatedAt:    ts,
	}); err != nil {
		return fmt.Errorf("sqlite: upsert translation for alert %d: %w", alertID, err)
	}
	return nil
}

// DeleteTranslation removes every field row for one (alert, language) pair.
// It normalizes language itself for the same reason UpsertTranslation does:
// the schema's language column is case-sensitive, so an un-normalized
// caller-supplied tag would silently match zero rows against the normalized
// values UpsertTranslation actually wrote.
func (r *alertRepo) DeleteTranslation(ctx context.Context, alertID int64, language string) error {
	n, err := r.q.DeleteAlertTranslations(ctx, gen.DeleteAlertTranslationsParams{
		AlertID: alertID, Language: alerts.NormalizeLanguage(language),
	})
	if err != nil {
		return fmt.Errorf("sqlite: delete translations for alert %d: %w", alertID, err)
	}
	if n == 0 {
		return fmt.Errorf("sqlite: delete translations for alert %d: %w", alertID, alerts.ErrNotFound)
	}
	return nil
}
