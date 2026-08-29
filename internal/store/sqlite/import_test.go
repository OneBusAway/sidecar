package sqlite_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/OneBusAway/sidecar/internal/alerts"
	"github.com/OneBusAway/sidecar/internal/export"
	"github.com/OneBusAway/sidecar/internal/regions"
	"github.com/OneBusAway/sidecar/internal/store/sqlite"
	"github.com/OneBusAway/sidecar/internal/store/sqlitetest"
)

var importBase = time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)

func importDoc() *export.Document {
	end := importBase.Add(48 * time.Hour)
	lat, lon := 47.6, -122.3
	seq := int64(4)
	predicted := true
	captured := importBase.Add(-time.Hour)
	return &export.Document{
		Format: export.Format, ExportedAt: importBase, RegionID: 1,
		Alerts: []export.Alert{{
			ID: 4242, AgencyID: "1", HeaderText: "Route 44 detoured", DescriptionText: "Use 43rd.",
			Cause: "construction", Effect: "DETOUR", Severity: "WARNING",
			StartTime: importBase.Add(-time.Hour), EndTime: &end, Published: true,
			CreatedAt: importBase.Add(-2 * time.Hour), UpdatedAt: importBase.Add(-time.Hour),
			Translations: []export.AlertTranslation{{
				Language: "ES", HeaderText: "Ruta 44 desviada", DescriptionText: "Use la 43.",
				SourceHeader: "Route 44 detoured", SourceDescription: "Old description",
			}},
		}},
		Studies: []export.Study{{
			ID: 7, Name: "Rider study", Description: "d",
			Surveys: []export.Survey{{
				ID: 70, Name: "How was your trip?", Available: true, ShowOnStops: true,
				VisibleStopList: []string{"1_100", "1_200"},
				Questions: []export.Question{
					{ID: 700, Position: 1, Required: true, Content: json.RawMessage(`{"type":"radio","label_text":"Rate it","options":["good","bad"]}`)},
					{ID: 701, Position: 2, Content: json.RawMessage(`{"type":"text","label_text":"Why?"}`)},
				},
			}},
		}},
		SurveyResponses: []export.SurveyResponse{{
			SurveyID: 70, PublicID: "resp-1", UserIdentifier: "user-1", StopIdentifier: "1_100",
			StopLatitude: &lat, StopLongitude: &lon,
			Answers:   json.RawMessage(`[{"question_id":700,"question_type":"radio","question_label":"Rate it","answer":"good"}]`),
			CreatedAt: importBase.Add(-30 * time.Minute),
		}},
		PushRegistrations: []export.PushRegistration{
			{Token: "tok-ios", OperatingSystem: "ios", Locale: "es", TestDevice: true, Description: "Aaron's phone", LastSeenAt: importBase.Add(-24 * time.Hour), CreatedAt: importBase.Add(-100 * 24 * time.Hour)},
			{Token: "tok-android", OperatingSystem: "android", LastSeenAt: importBase},
		},
		GhostBusReports: []export.GhostBusReport{{
			PublicID: "gb-1", UserIdentifier: "user-1", TripIdentifier: "1_trip", ServiceDateMS: 1756339200000,
			RouteIdentifier: "1_44", StopIdentifier: "1_100", StopSequence: &seq, Predicted: &predicted,
			WaitDurationMinutes: 12, Comment: "never came",
			SnapshotStatus: "captured", Snapshot: json.RawMessage(`{"status":{"phase":"in_progress"},"display":{"route_short_name":"44"}}`),
			SnapshotCapturedAt: &captured, CreatedAt: importBase.Add(-3 * time.Hour),
		}},
	}
}

func seedImportRegion(t *testing.T, store *sqlite.Store) {
	t.Helper()
	if err := store.Regions().UpsertFromDirectory(context.Background(), []regions.Region{{
		ID: 1, Name: "Puget Sound", OBABaseURL: "https://example.org/", Active: true,
	}}, importBase); err != nil {
		t.Fatal(err)
	}
}

func TestImport_RoundTripAndDelta(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := sqlitetest.Open(t)
	seedImportRegion(t, store)

	sum, err := store.Import(ctx, importDoc(), importBase)
	if err != nil {
		t.Fatal(err)
	}
	one := export.Counts{Added: 1}
	want := export.Summary{Alerts: one, Translations: export.Counts{Added: 2}, Studies: one, Surveys: one, Questions: export.Counts{Added: 2}, SurveyResponses: one, PushRegistrations: export.Counts{Added: 2}, GhostBusReports: one}
	if sum != want {
		t.Fatalf("summary %+v, want %+v", sum, want)
	}
	assertImportedAlert(t, store)
	assertImportedSurvey(t, store)
	assertImportedRiderState(t, store)

	// A second import of a superset is a delta: everything existing is
	// skipped, the new rows land, nothing is duplicated.
	doc := importDoc()
	doc.PushRegistrations = append(doc.PushRegistrations, export.PushRegistration{Token: "tok-new", OperatingSystem: "ios", LastSeenAt: importBase})
	// A translation added to an already-migrated alert must land on the
	// delta run; that is the case the delta exists for.
	doc.Alerts[0].Translations = append(doc.Alerts[0].Translations, export.AlertTranslation{Language: "fr", HeaderText: "Ligne 44", DescriptionText: "Prenez la 43e."})
	sum, err = store.Import(ctx, doc, importBase.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	skipped := export.Counts{Skipped: 1}
	wantDelta := export.Summary{Alerts: skipped, Translations: export.Counts{Added: 2, Skipped: 2}, Studies: skipped, Surveys: skipped, Questions: export.Counts{Skipped: 2}, SurveyResponses: skipped, PushRegistrations: export.Counts{Added: 1, Skipped: 2}, GhostBusReports: skipped}
	if sum != wantDelta {
		t.Fatalf("delta summary %+v, want %+v", sum, wantDelta)
	}
	count, countErr := store.PushRegs().CountAudience(ctx, 1, false)
	if countErr != nil || count.Total != 3 {
		t.Fatalf("audience %+v %v", count, countErr)
	}
	alert, err := store.Alerts().Get(ctx, 4242)
	if err != nil || len(alert.Translations) != 4 {
		t.Fatalf("fr translation not imported on the delta run: %v %+v", err, alert.Translations)
	}
}

// assertImportedAlert: ids survive (the feed entity id Alert_4242 is what
// riders dismissed) and translation staleness follows the source text.
func assertImportedAlert(t *testing.T, store *sqlite.Store) {
	t.Helper()
	alert, err := store.Alerts().Get(context.Background(), 4242)
	if err != nil {
		t.Fatal(err)
	}
	if alert.RegionID != 1 || alert.Cause != "CONSTRUCTION" || !alert.Published || alert.EndTime == nil {
		t.Fatalf("alert %+v", alert)
	}
	var sawHeader, sawDescription bool
	for _, tr := range alert.Translations {
		if tr.Language != "es" {
			t.Fatalf("translation language %q", tr.Language)
		}
		switch tr.Field {
		case "header":
			sawHeader = tr.SourceSHA256 == alerts.SourceHash("Route 44 detoured")
		case "description":
			// Source text differs from the current description: stale.
			sawDescription = tr.SourceSHA256 == alerts.SourceHash("Old description")
		}
	}
	if !sawHeader || !sawDescription {
		t.Fatalf("translations %+v", alert.Translations)
	}
}

func assertImportedSurvey(t *testing.T, store *sqlite.Store) {
	t.Helper()
	ctx := context.Background()
	survey, err := store.Surveys().GetSurvey(ctx, 70)
	if err != nil {
		t.Fatal(err)
	}
	if survey.Study.ID != 7 || len(survey.Questions) != 2 || survey.Questions[0].ID != 700 || survey.Questions[0].Content.Type != "radio" {
		t.Fatalf("survey %+v", survey)
	}
	if len(survey.VisibleStopList) != 2 {
		t.Fatalf("visible stops %v", survey.VisibleStopList)
	}
	resp, err := store.Surveys().GetResponse(ctx, "resp-1")
	if err != nil {
		t.Fatal(err)
	}
	if resp.SurveyID != 70 || len(resp.Answers) != 1 || resp.Answers[0].QuestionID != 700 || resp.StopLatitude == nil {
		t.Fatalf("response %+v", resp)
	}
}

func assertImportedRiderState(t *testing.T, store *sqlite.Store) {
	t.Helper()
	ctx := context.Background()
	reg, err := store.PushRegs().Get(ctx, 1, "tok-ios")
	if err != nil {
		t.Fatal(err)
	}
	if !reg.TestDevice || reg.Locale != "es" || !reg.LastSeenAt.Equal(importBase.Add(-24*time.Hour)) {
		t.Fatalf("registration %+v", reg)
	}
	if _, androidErr := store.PushRegs().Get(ctx, 1, "tok-android"); androidErr != nil {
		t.Fatal(androidErr)
	}
	report, err := store.GhostBus().GetByPublicID(ctx, 1, "gb-1")
	if err != nil {
		t.Fatal(err)
	}
	if report.SnapshotStatus != "captured" || !strings.Contains(report.SnapshotJSON, `"route_short_name":"44"`) || report.StopSequence == nil || *report.StopSequence != 4 {
		t.Fatalf("report %+v", report)
	}
	pending, pendErr := store.GhostBus().ListPendingSnapshots(ctx, 10)
	if pendErr != nil || len(pending) != 0 {
		t.Fatalf("captured report re-queued for enrichment: %v %v", pending, pendErr)
	}
}

// TestImport_CrossRegionConflict pins that a source id already used by
// another region's content is an error, never a silent skip or a merge:
// alerts, studies, surveys, and questions share one id sequence.
func TestImport_CrossRegionConflict(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := sqlitetest.Open(t)
	seedImportRegion(t, store)
	if err := store.Regions().UpsertFromDirectory(ctx, []regions.Region{{
		ID: 2, Name: "Elsewhere", OBABaseURL: "https://example.org/", Active: true,
	}}, importBase); err != nil {
		t.Fatal(err)
	}
	// Region 2 gets the same document first, so every id now belongs to it.
	other := importDoc()
	other.RegionID = 2
	if _, err := store.Import(ctx, other, importBase); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*export.Document){
		"alert":           func(d *export.Document) { d.Studies, d.SurveyResponses, d.GhostBusReports = nil, nil, nil },
		"study":           func(d *export.Document) { d.Alerts, d.SurveyResponses, d.GhostBusReports = nil, nil, nil },
		"ghost bus":       func(d *export.Document) { d.Alerts, d.Studies, d.SurveyResponses = nil, nil, nil },
		"survey response": func(d *export.Document) { d.Alerts, d.Studies, d.GhostBusReports = nil, nil, nil },
	}
	for name, trim := range cases {
		doc := importDoc()
		trim(doc)
		_, err := store.Import(ctx, doc, importBase)
		if !errors.Is(err, sqlite.ErrImportConflict) {
			t.Errorf("%s owned by another region: err %v, want ErrImportConflict", name, err)
		}
	}
	// A survey under a different study than the one it already belongs to
	// is a conflict too, even within the region.
	doc := importDoc()
	doc.RegionID = 2
	doc.Studies[0].ID = 8
	_, err := store.Import(ctx, doc, importBase)
	if !errors.Is(err, sqlite.ErrImportConflict) {
		t.Fatalf("survey re-parented: err %v", err)
	}
	if _, err := store.Alerts().Feed(ctx, 1, true, 10); err != nil {
		t.Fatal(err)
	}
}

// TestImport_SecondaryRows covers the branches the round trip does not:
// timestamp fallback, the unavailable/pending snapshot bookkeeping, and
// the pre-transaction rejections that only prepareImport performs.
func TestImport_SecondaryRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := sqlitetest.Open(t)
	seedImportRegion(t, store)
	doc := importDoc()
	doc.GhostBusReports = append(doc.GhostBusReports,
		export.GhostBusReport{PublicID: "gb-pending", UserIdentifier: "u2", TripIdentifier: "1_t2", ServiceDateMS: 1756339200000, WaitDurationMinutes: 1, SnapshotStatus: "pending", Snapshot: json.RawMessage(`{"ignored":true}`)},
		export.GhostBusReport{PublicID: "gb-gone", UserIdentifier: "u3", TripIdentifier: "1_t3", ServiceDateMS: 1756339200000, WaitDurationMinutes: 1, SnapshotStatus: "unavailable"},
	)
	if _, err := store.Import(ctx, doc, importBase); err != nil {
		t.Fatal(err)
	}
	study, err := store.Surveys().GetStudy(ctx, 7)
	if err != nil || !study.CreatedAt.Equal(importBase) {
		t.Fatalf("zero created_at should fall back to now: %v %v", err, study.CreatedAt)
	}
	pending, err := store.GhostBus().ListPendingSnapshots(ctx, 10)
	if err != nil || len(pending) != 1 || pending[0].PublicID != "gb-pending" || pending[0].SnapshotJSON != "" {
		t.Fatalf("pending queue %+v %v", pending, err)
	}
	gone, err := store.GhostBus().GetByPublicID(ctx, 1, "gb-gone")
	if err != nil || gone.SnapshotAttempts != 3 {
		t.Fatalf("unavailable report %+v %v", gone, err)
	}

	rejected := map[string]func(*export.Document){
		"uppercase english":    func(d *export.Document) { d.Alerts[0].Translations[0].Language = " EN " },
		"captured no snapshot": func(d *export.Document) { d.GhostBusReports[0].Snapshot = nil },
		"alert end before start": func(d *export.Document) {
			early := d.Alerts[0].StartTime.Add(-time.Minute)
			d.Alerts[0].EndTime = &early
		},
		"survey end before start": func(d *export.Document) {
			a, b := importBase, importBase.Add(-time.Hour)
			d.Studies[0].Surveys[0].StartTime, d.Studies[0].Surveys[0].EndTime = &a, &b
		},
		"malformed answers": func(d *export.Document) { d.SurveyResponses[0].Answers = json.RawMessage(`[{"question_id":"x"}]`) },
		"duplicate question id": func(d *export.Document) {
			d.Studies[0].Surveys[0].Questions[1].ID = d.Studies[0].Surveys[0].Questions[0].ID
		},
		"duplicate alert id":  func(d *export.Document) { d.Alerts = append(d.Alerts, d.Alerts[0]) },
		"duplicate token":     func(d *export.Document) { d.PushRegistrations = append(d.PushRegistrations, d.PushRegistrations[0]) },
		"duplicate public id": func(d *export.Document) { d.GhostBusReports = append(d.GhostBusReports, d.GhostBusReports[0]) },
		"duplicate translation": func(d *export.Document) {
			d.Alerts[0].Translations = append(d.Alerts[0].Translations, d.Alerts[0].Translations[0])
		},
	}
	for name, mutate := range rejected {
		d := importDoc()
		mutate(d)
		if err := store.ValidateImport(ctx, d, importBase); err == nil {
			t.Errorf("%s: ValidateImport accepted it", name)
		}
	}
	if err := store.ValidateImport(ctx, importDoc(), importBase); err != nil {
		t.Fatalf("valid document rejected: %v", err)
	}
	empty := sqlitetest.Open(t)
	if err := empty.ValidateImport(ctx, importDoc(), importBase); !errors.Is(err, sqlite.ErrRegionMissing) {
		t.Fatalf("dry run must notice the missing region: %v", err)
	}
}

func TestImport_ValidatesBeforeWriting(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := sqlitetest.Open(t)
	seedImportRegion(t, store)

	cases := map[string]func(*export.Document){
		"bad cause":           func(d *export.Document) { d.Alerts[0].Cause = "GREMLINS" },
		"english translation": func(d *export.Document) { d.Alerts[0].Translations[0].Language = "en" },
		"one-sided window":    func(d *export.Document) { d.Studies[0].Surveys[0].StartTime = &importBase },
		"radio without options": func(d *export.Document) {
			d.Studies[0].Surveys[0].Questions[0].Content = json.RawMessage(`{"type":"radio","label_text":"x"}`)
		},
		"unknown snapshot status": func(d *export.Document) { d.GhostBusReports[0].SnapshotStatus = "lost" },
		"wrong format":            func(d *export.Document) { d.Format = "sidecar-export/9" },
	}
	for name, mutate := range cases {
		doc := importDoc()
		mutate(doc)
		if _, err := store.Import(ctx, doc, importBase); err == nil {
			t.Errorf("%s: import succeeded", name)
		}
	}
	// Nothing landed from any of the rejected documents.
	if _, err := store.Alerts().Get(ctx, 4242); err == nil {
		t.Fatal("rejected import left the alert behind")
	}

	empty := sqlitetest.Open(t)
	if _, err := empty.Import(ctx, importDoc(), importBase); !errors.Is(err, sqlite.ErrRegionMissing) {
		t.Fatalf("missing region: %v", err)
	}
}
