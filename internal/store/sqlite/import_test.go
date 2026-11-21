package sqlite_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
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
			WaitDurationMinutes: 15, Comment: "never came",
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
	doc.Alerts[0].DescriptionText = " \t"
	doc.Alerts[0].Translations = append(doc.Alerts[0].Translations,
		export.AlertTranslation{Language: "de", HeaderText: " \t", DescriptionText: "Nehmen Sie die 43."})
	doc.GhostBusReports = append(doc.GhostBusReports,
		export.GhostBusReport{PublicID: "gb-pending", UserIdentifier: "u2", TripIdentifier: "1_t2", ServiceDateMS: 1756339200000, WaitDurationMinutes: 5, SnapshotStatus: "pending", Snapshot: json.RawMessage(`{"ignored":true}`)},
		export.GhostBusReport{PublicID: "gb-gone", UserIdentifier: "u3", TripIdentifier: "1_t3", ServiceDateMS: 1756339200000, WaitDurationMinutes: 5, SnapshotStatus: "unavailable"},
	)
	if _, err := store.Import(ctx, doc, importBase); err != nil {
		t.Fatal(err)
	}
	study, err := store.Surveys().GetStudy(ctx, 7)
	if err != nil || !study.CreatedAt.Equal(importBase) {
		t.Fatalf("zero created_at should fall back to now: %v %v", err, study.CreatedAt)
	}
	// Blank text imports as none: no description, and no de header row
	// beside es header, es description, and de description. The de
	// description has no source_description, so it is hashed against the
	// stored "" and is not stale.
	alert, err := store.Alerts().Get(ctx, 4242)
	de := slices.IndexFunc(alert.Translations, func(tr alerts.Translation) bool {
		return tr.Language == "de" && tr.Field == alerts.FieldDescription
	})
	if err != nil || alert.DescriptionText != "" || len(alert.Translations) != 3 || de < 0 || alert.TranslationStale(alert.Translations[de]) {
		t.Fatalf("blank text should import as none, de description fresh: description %q, translations %+v, err %v", alert.DescriptionText, alert.Translations, err)
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
		"blank header":        func(d *export.Document) { d.Alerts[0].HeaderText = " \t" },
		"empty agency id":     func(d *export.Document) { d.Alerts[0].AgencyID = "" },
		"blank agency id":     func(d *export.Document) { d.Alerts[0].AgencyID = " \t" },
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

// TestImport_RejectsIDAtSequenceFloor (SC-13): an alert id of MaxInt64
// would park the shared alerts sequence where SQLite has no id left to
// mint, breaking alert creation in every region. It is rejected before the
// transaction opens: nothing lands, the sequences do not move, and another
// region can still author.
func TestImport_RejectsIDAtSequenceFloor(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := sqlitetest.Open(t)
	seedImportRegion(t, store)
	if err := store.Regions().UpsertFromDirectory(ctx, []regions.Region{{
		ID: 2, Name: "Elsewhere", OBABaseURL: "https://example.org/", Active: true,
	}}, importBase); err != nil {
		t.Fatal(err)
	}
	before, err := store.Sequences(ctx)
	if err != nil {
		t.Fatal(err)
	}
	doc := importDoc()
	doc.Alerts[0].ID = math.MaxInt64
	if dryErr := store.ValidateImport(ctx, doc, importBase); dryErr == nil {
		t.Fatal("dry run accepted an id at MaxInt64")
	}
	if _, impErr := store.Import(ctx, doc, importBase); impErr == nil || !strings.Contains(impErr.Error(), "sequence floor") {
		t.Fatalf("import err %v, want a sequence-floor rejection", impErr)
	}
	after, err := store.Sequences(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(before) != fmt.Sprint(after) {
		t.Fatalf("sequences moved: %v -> %v", before, after)
	}
	if _, err := store.Surveys().GetStudy(ctx, 7); err == nil {
		t.Fatal("rejected import left the study behind")
	}
	if _, err := store.Alerts().Create(ctx, alerts.NewAlert{RegionID: 2, AgencyID: "1", HeaderText: "h", StartTime: importBase}, importBase); err != nil {
		t.Fatalf("another region can no longer author: %v", err)
	}
}

// TestImport_AlertURLValidated (SC-07): import applies the admin API's url
// rule. OBACloud validates http(s) on write, so this should never fire on
// real data; when it does, the error names the alert to fix in OBACloud.
func TestImport_AlertURLValidated(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := sqlitetest.Open(t)
	seedImportRegion(t, store)
	for _, u := range []string{"javascript:alert(1)", "data:text/html,x", "//evil.example/x", "/detours", "https://example.org/" + strings.Repeat("a", 5000)} {
		doc := importDoc()
		doc.Alerts[0].URL = u
		if err := store.ValidateImport(ctx, doc, importBase); err == nil || !strings.Contains(err.Error(), "alert 4242") || !strings.Contains(err.Error(), "url") {
			t.Errorf("url %.40q: dry run err %v, want one naming alert 4242's url", u, err)
		}
		if _, err := store.Import(ctx, doc, importBase); err == nil {
			t.Errorf("url %.40q imported", u)
		}
	}
	if _, err := store.Alerts().Get(ctx, 4242); err == nil {
		t.Fatal("rejected import left the alert behind")
	}
	doc := importDoc()
	doc.Alerts[0].URL = "https://example.org/detours"
	if _, err := store.Import(ctx, doc, importBase); err != nil {
		t.Fatalf("valid url: %v", err)
	}
}

// TestImport_HTTPLayerRules (SC-15): imported rows meet the rules the HTTP
// layer applies to the same fields -- length caps, coordinate ranges, the
// wait-duration choices, a URL-safe public id, a snapshot that is a JSON
// object -- and a row that breaks one is rejected by name before anything
// is written (the operator fixes or deletes it in OBACloud and re-exports).
func TestImport_HTTPLayerRules(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := sqlitetest.Open(t)
	seedImportRegion(t, store)
	long := strings.Repeat("x", 256)
	far := 91.0
	wide := -180.5
	cases := []struct {
		name, wantRow string
		mutate        func(*export.Document)
	}{
		{"response user_identifier too long", "survey response resp-1", func(d *export.Document) { d.SurveyResponses[0].UserIdentifier = long }},
		{"response stop_identifier too long", "survey response resp-1", func(d *export.Document) { d.SurveyResponses[0].StopIdentifier = long }},
		{"response latitude out of range", "survey response resp-1", func(d *export.Document) { d.SurveyResponses[0].StopLatitude = &far }},
		{"response longitude out of range", "survey response resp-1", func(d *export.Document) { d.SurveyResponses[0].StopLongitude = &wide }},
		{"response public id not URL-safe", "survey response", func(d *export.Document) { d.SurveyResponses[0].PublicID = "../admin" }},
		{"response public id too long", "survey response", func(d *export.Document) { d.SurveyResponses[0].PublicID = strings.Repeat("a", 65) }},
		{"token too long", "push registration", func(d *export.Document) { d.PushRegistrations[0].Token = strings.Repeat("t", 4097) }},
		{"test device description too long", "push registration", func(d *export.Document) { d.PushRegistrations[0].Description = long }},
		{"report user_identifier too long", "ghost bus report gb-1", func(d *export.Document) { d.GhostBusReports[0].UserIdentifier = long }},
		{"report trip_identifier too long", "ghost bus report gb-1", func(d *export.Document) { d.GhostBusReports[0].TripIdentifier = long }},
		{"report route_identifier too long", "ghost bus report gb-1", func(d *export.Document) { d.GhostBusReports[0].RouteIdentifier = long }},
		{"report stop_identifier too long", "ghost bus report gb-1", func(d *export.Document) { d.GhostBusReports[0].StopIdentifier = long }},
		{"report vehicle_identifier too long", "ghost bus report gb-1", func(d *export.Document) { d.GhostBusReports[0].VehicleIdentifier = long }},
		{"report comment too long", "ghost bus report gb-1", func(d *export.Document) { d.GhostBusReports[0].Comment = strings.Repeat("é", 1001) }},
		{"report latitude out of range", "ghost bus report gb-1", func(d *export.Document) { d.GhostBusReports[0].UserLatitude = &far }},
		{"report longitude out of range", "ghost bus report gb-1", func(d *export.Document) { d.GhostBusReports[0].UserLongitude = &wide }},
		{"report wait duration not a choice", "ghost bus report gb-1", func(d *export.Document) { d.GhostBusReports[0].WaitDurationMinutes = 12 }},
		{"report public id not URL-safe", "ghost bus report", func(d *export.Document) { d.GhostBusReports[0].PublicID = "gb 1" }},
		{"snapshot not an object", "ghost bus report gb-1", func(d *export.Document) { d.GhostBusReports[0].Snapshot = json.RawMessage(`[1,2]`) }},
		{"snapshot a string", "ghost bus report gb-1", func(d *export.Document) { d.GhostBusReports[0].Snapshot = json.RawMessage(`"x"`) }},
		{"snapshot too large", "ghost bus report gb-1", func(d *export.Document) {
			d.GhostBusReports[0].Snapshot = json.RawMessage(`{"pad":"` + strings.Repeat("p", 70_000) + `"}`)
		}},
	}
	for _, tc := range cases {
		doc := importDoc()
		tc.mutate(doc)
		err := store.ValidateImport(ctx, doc, importBase)
		if err == nil || !strings.Contains(err.Error(), tc.wantRow) {
			t.Errorf("%s: dry run err %v, want one naming %q", tc.name, err, tc.wantRow)
			continue
		}
		if _, err := store.Import(ctx, doc, importBase); err == nil {
			t.Errorf("%s: imported", tc.name)
		}
	}
	if _, err := store.Alerts().Get(ctx, 4242); err == nil {
		t.Fatal("a rejected import left the alert behind")
	}

	// The limits themselves are accepted: exactly 255 bytes, 1000 runes,
	// +/-90 and +/-180, a 4096-byte token, Rails's 20-hex public ids.
	doc := importDoc()
	edge := strings.Repeat("x", 255)
	lat, lon := -90.0, 180.0
	doc.SurveyResponses[0].UserIdentifier, doc.SurveyResponses[0].PublicID = edge, "b7e4483857d9af7702b9"
	doc.GhostBusReports[0].TripIdentifier, doc.GhostBusReports[0].Comment = edge, strings.Repeat("é", 1000)
	doc.GhostBusReports[0].UserLatitude, doc.GhostBusReports[0].UserLongitude = &lat, &lon
	doc.PushRegistrations[1].Token = strings.Repeat("t", 4096)
	if _, err := store.Import(ctx, doc, importBase); err != nil {
		t.Fatalf("rows at the limits rejected: %v", err)
	}
}

// TestImport_NonTestDeviceDescriptionDropped (SC-15): the HTTP layer keeps
// no description on a non-test registration (spec section 4), so a legacy
// one is normalized away on import instead of failing the document.
func TestImport_NonTestDeviceDescriptionDropped(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := sqlitetest.Open(t)
	seedImportRegion(t, store)
	doc := importDoc()
	doc.PushRegistrations[1].Description = strings.Repeat("d", 300)
	if _, err := store.Import(ctx, doc, importBase); err != nil {
		t.Fatal(err)
	}
	reg, err := store.PushRegs().Get(ctx, 1, "tok-android")
	if err != nil || reg.Description != "" {
		t.Fatalf("non-test description kept: %q %v", reg.Description, err)
	}
	reg, err = store.PushRegs().Get(ctx, 1, "tok-ios")
	if err != nil || reg.Description != "Aaron's phone" {
		t.Fatalf("test device description lost: %q %v", reg.Description, err)
	}
}
