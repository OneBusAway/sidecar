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
	want := export.Summary{Alerts: 1, Studies: 1, Surveys: 1, Questions: 2, SurveyResponses: 1, PushRegistrations: 2, GhostBusReports: 1}
	if sum != want {
		t.Fatalf("summary %+v, want %+v", sum, want)
	}

	// Ids survive: the feed entity id Alert_4242 is what riders dismissed.
	alert, err := store.Alerts().Get(ctx, 4242)
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

	// A second import of a superset is a delta: everything existing is
	// skipped, the new row lands, nothing is duplicated.
	doc := importDoc()
	doc.PushRegistrations = append(doc.PushRegistrations, export.PushRegistration{Token: "tok-new", OperatingSystem: "ios", LastSeenAt: importBase})
	sum, err = store.Import(ctx, doc, importBase.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	wantDelta := export.Summary{AlertsSkipped: 1, StudiesSkipped: 1, SurveysSkipped: 1, QuestionsSkipped: 2, SurveyResponsesSkipped: 1, PushRegistrations: 1, PushRegistrationsSkipped: 2, GhostBusReportsSkipped: 1}
	if sum != wantDelta {
		t.Fatalf("delta summary %+v, want %+v", sum, wantDelta)
	}
	count, countErr := store.PushRegs().CountAudience(ctx, 1, false)
	if countErr != nil || count.Total != 3 {
		t.Fatalf("audience %+v %v", count, countErr)
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
