package sqlite_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/alerts"
	"github.com/OneBusAway/sidecar/internal/export"
	"github.com/OneBusAway/sidecar/internal/store/sqlite"
	"github.com/OneBusAway/sidecar/internal/store/sqlitetest"
	"github.com/OneBusAway/sidecar/internal/surveys"
)

// SC-14: an id already present in this region is only "the same row" when
// that row came from an earlier import. These tests pin the three cases the
// cutover depends on.

// TestImport_IdenticalReimportIsNoOp: the cutover's bulk-then-delta runs
// and any re-run of either change nothing the second time.
func TestImport_IdenticalReimportIsNoOp(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := sqlitetest.Open(t)
	seedImportRegion(t, store)
	if _, err := store.Import(ctx, importDoc(), importBase); err != nil {
		t.Fatal(err)
	}
	before, err := store.Alerts().Get(ctx, 4242)
	if err != nil {
		t.Fatal(err)
	}
	sum, err := store.Import(ctx, importDoc(), importBase.Add(time.Hour))
	if err != nil {
		t.Fatalf("identical re-import: %v", err)
	}
	if sum.Alerts.Added+sum.Studies.Added+sum.Surveys.Added+sum.Questions.Added+sum.Translations.Added != 0 {
		t.Fatalf("identical re-import added rows: %+v", sum)
	}
	after, err := store.Alerts().Get(ctx, 4242)
	if err != nil || !after.UpdatedAt.Equal(before.UpdatedAt) || after.HeaderText != before.HeaderText {
		t.Fatalf("identical re-import touched the alert: %+v -> %+v (%v)", before, after, err)
	}
}

// TestImport_RailsEditedRowIsSkipped: OBACloud stays authoritative until
// the flip, so a row imported by the bulk run may be edited there before
// the delta. The delta is insert-if-absent: the edited row is skipped, not
// a conflict (OBACloud's cutover preflight, Sidecar::ContentComparison,
// is what reports that drift before any flip), and a translation added to
// it in the meantime still lands.
func TestImport_RailsEditedRowIsSkipped(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := sqlitetest.Open(t)
	seedImportRegion(t, store)
	if _, err := store.Import(ctx, importDoc(), importBase); err != nil {
		t.Fatal(err)
	}
	doc := importDoc()
	doc.Alerts[0].HeaderText = "Route 44 detour extended"
	doc.Alerts[0].UpdatedAt = importBase.Add(30 * time.Minute)
	doc.Alerts[0].Translations = append(doc.Alerts[0].Translations, export.AlertTranslation{Language: "fr", HeaderText: "Ligne 44"})
	doc.Studies[0].Name = "Renamed study"
	doc.Studies[0].Surveys[0].Name = "Renamed survey"
	sum, err := store.Import(ctx, doc, importBase.Add(time.Hour))
	if err != nil {
		t.Fatalf("delta over a Rails-edited row: %v", err)
	}
	if sum.Alerts.Skipped != 1 || sum.Studies.Skipped != 1 || sum.Surveys.Skipped != 1 || sum.Translations.Added != 1 {
		t.Fatalf("summary %+v", sum)
	}
	a, err := store.Alerts().Get(ctx, 4242)
	if err != nil || a.HeaderText != "Route 44 detoured" {
		t.Fatalf("insert-if-absent overwrote the stored alert: %q %v", a.HeaderText, err)
	}
}

// TestImport_SidecarAuthoredRowIsConflict: a row authored on this sidecar
// that happens to hold an exported id (an un-bumped sequence) is a
// different row. Skipping it would attach the export's translations,
// surveys, or questions to unrelated content, so it is ErrImportConflict
// and the whole document rolls back.
func TestImport_SidecarAuthoredRowIsConflict(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("alert", func(t *testing.T) {
		t.Parallel()
		store := sqlitetest.Open(t)
		seedImportRegion(t, store)
		local, err := store.Alerts().Create(ctx, alerts.NewAlert{RegionID: 1, AgencyID: "1", HeaderText: "Local alert", StartTime: importBase}, importBase)
		if err != nil {
			t.Fatal(err)
		}
		doc := importDoc()
		doc.Alerts[0].ID = local.ID
		_, err = store.Import(ctx, doc, importBase)
		if !errors.Is(err, sqlite.ErrImportConflict) {
			t.Fatalf("err %v, want ErrImportConflict", err)
		}
		got, err := store.Alerts().Get(ctx, local.ID)
		if err != nil || len(got.Translations) != 0 || got.HeaderText != "Local alert" {
			t.Fatalf("translations attached to the local alert: %+v %v", got, err)
		}
		if _, err := store.Surveys().GetStudy(ctx, 7); err == nil {
			t.Fatal("conflicted import left the study behind")
		}
	})

	t.Run("study", func(t *testing.T) {
		t.Parallel()
		store := sqlitetest.Open(t)
		seedImportRegion(t, store)
		local, err := store.Surveys().CreateStudy(ctx, 1, "Local study", "", importBase)
		if err != nil {
			t.Fatal(err)
		}
		doc := importDoc()
		doc.Studies[0].ID = local.ID
		if _, err := store.Import(ctx, doc, importBase); !errors.Is(err, sqlite.ErrImportConflict) {
			t.Fatalf("err %v, want ErrImportConflict", err)
		}
		if _, err := store.Surveys().GetSurvey(ctx, 70); err == nil {
			t.Fatal("survey attached to the local study")
		}
	})

	t.Run("survey", func(t *testing.T) {
		t.Parallel()
		store := sqlitetest.Open(t)
		seedImportRegion(t, store)
		// The study is imported; a survey authored under it here takes the
		// next id, which the export also uses.
		first := importDoc()
		first.Studies[0].Surveys = nil
		first.SurveyResponses = nil
		if _, err := store.Import(ctx, first, importBase); err != nil {
			t.Fatal(err)
		}
		local, err := store.Surveys().CreateSurvey(ctx, 7, surveys.Definition{Name: "Local survey"}, importBase)
		if err != nil {
			t.Fatal(err)
		}
		doc := importDoc()
		doc.Studies[0].Surveys[0].ID = local.ID
		doc.SurveyResponses[0].SurveyID = local.ID
		if _, impErr := store.Import(ctx, doc, importBase); !errors.Is(impErr, sqlite.ErrImportConflict) {
			t.Fatalf("err %v, want ErrImportConflict", impErr)
		}
		got, err := store.Surveys().GetSurvey(ctx, local.ID)
		if err != nil || len(got.Questions) != 0 {
			t.Fatalf("questions attached to the local survey: %+v %v", got, err)
		}
	})
}

// TestImport_LegacyImportedRowsStillReimport: rows imported by a binary
// that predates the provenance column carry none. The delta that follows a
// bulk import across that upgrade must still work, so an unmarked row whose
// created_at is the export's (OBACloud never changes created_at, and the
// import carries it verbatim) is recognized as the earlier import and
// stamped; a different created_at is a conflict.
func TestImport_LegacyImportedRowsStillReimport(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path, store := sqlitetest.OpenAt(t)
	seedImportRegion(t, store)
	// OBACloud always sends created_at; the fixture leaves the study's and
	// survey's zero to cover the fallback-to-now path elsewhere.
	legacyDoc := func() *export.Document {
		d := importDoc()
		d.Studies[0].CreatedAt = importBase.Add(-48 * time.Hour)
		d.Studies[0].Surveys[0].CreatedAt = importBase.Add(-47 * time.Hour)
		return d
	}
	if _, err := store.Import(ctx, legacyDoc(), importBase); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	clear := func(t *testing.T) {
		t.Helper()
		for _, table := range []string{"alerts", "studies", "surveys"} {
			if _, err := db.ExecContext(ctx, "UPDATE "+table+" SET imported_at = NULL"); err != nil {
				t.Fatal(err)
			}
		}
	}
	clear(t)
	doc := legacyDoc()
	doc.Alerts[0].HeaderText = "edited in Rails"
	if _, err := store.Import(ctx, doc, importBase.Add(time.Hour)); err != nil {
		t.Fatalf("delta over legacy-imported rows: %v", err)
	}
	var stamped int
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM alerts WHERE imported_at IS NOT NULL").Scan(&stamped); err != nil || stamped != 1 {
		t.Fatalf("legacy row not stamped: %d %v", stamped, err)
	}

	clear(t)
	doc = legacyDoc()
	doc.Alerts[0].CreatedAt = importBase.Add(-5 * time.Hour)
	if _, err := store.Import(ctx, doc, importBase); !errors.Is(err, sqlite.ErrImportConflict) {
		t.Fatalf("unmarked row with another created_at: err %v, want ErrImportConflict", err)
	}
}
