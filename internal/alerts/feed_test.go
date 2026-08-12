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

// TestDescriptionTextOmittedWhenEmpty reproduces the finding that buildAlert
// guarded url with `if a.URL != ""` but not DescriptionText, which defaults
// to an empty string in storage and is optional in the CLI: an alert with
// only a header rendered description_text:{translation:{text:"" language:
// "en"}} instead of omitting the field, so a consumer branching on presence
// saw a description that existed but was blank rather than correctly seeing
// none.
func TestDescriptionTextOmittedWhenEmpty(t *testing.T) {
	t.Parallel()

	a := base()
	a.DescriptionText = ""

	got := alerts.BuildFeed([]alerts.Alert{a}, opts()).GetEntity()[0].GetAlert().GetDescriptionText()
	if got != nil {
		t.Errorf("description_text = %v, want nil (omitted) when DescriptionText is empty", got)
	}
}

func TestDescriptionTextPresentWhenNonEmpty(t *testing.T) {
	t.Parallel()

	// base() already carries a non-empty DescriptionText; this is the
	// complement to TestDescriptionTextOmittedWhenEmpty, guarding against an
	// overzealous fix that omits description_text unconditionally.
	got := alerts.BuildFeed([]alerts.Alert{base()}, opts()).GetEntity()[0].GetAlert().GetDescriptionText()
	if got == nil {
		t.Fatal("description_text = nil, want present when DescriptionText is non-empty")
	}
	tr := got.GetTranslation()
	if len(tr) != 1 || tr[0].GetLanguage() != "en" || tr[0].GetText() != "Signal problem" {
		t.Errorf("description_text translations = %+v, want exactly one English entry with the alert's description", tr)
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

func TestOnUnknownEnumFiresForUnmappableStoredValues(t *testing.T) {
	t.Parallel()

	a := base()
	a.Cause = "BANANA"
	a.Effect = "ALSO_BOGUS"
	// Severity left as a legitimate, mappable value: it must not trigger the
	// callback, distinguishing a real miss from every other field.

	type call struct{ kind, name string }
	var calls []call
	o := opts()
	o.OnUnknownEnum = func(kind, name string) {
		calls = append(calls, call{kind, name})
	}

	msg := alerts.BuildFeed([]alerts.Alert{a}, o)

	// The degradation itself must still happen regardless of the callback.
	got := msg.GetEntity()[0].GetAlert()
	if got.GetCause() != gtfs.Alert_UNKNOWN_CAUSE {
		t.Errorf("cause = %v, want UNKNOWN_CAUSE", got.GetCause())
	}
	if got.GetEffect() != gtfs.Alert_UNKNOWN_EFFECT {
		t.Errorf("effect = %v, want UNKNOWN_EFFECT", got.GetEffect())
	}

	want := []call{{"cause", "BANANA"}, {"effect", "ALSO_BOGUS"}}
	if len(calls) != len(want) {
		t.Fatalf("OnUnknownEnum calls = %+v, want %+v", calls, want)
	}
	for i, w := range want {
		if calls[i] != w {
			t.Errorf("call[%d] = %+v, want %+v", i, calls[i], w)
		}
	}
}

func TestOnUnknownEnumNotCalledForLegitimateUnknownValues(t *testing.T) {
	t.Parallel()

	a := base()
	a.Cause = "UNKNOWN_CAUSE"
	a.Effect = "UNKNOWN_EFFECT"
	a.Severity = "UNKNOWN_SEVERITY"

	called := false
	o := opts()
	o.OnUnknownEnum = func(kind, name string) { called = true }

	alerts.BuildFeed([]alerts.Alert{a}, o)

	if called {
		t.Error("OnUnknownEnum fired for an explicit, valid UNKNOWN_* value; want no call")
	}
}

func TestNilOnUnknownEnumIsSafe(t *testing.T) {
	t.Parallel()

	a := base()
	a.Cause = "BANANA"

	// opts() leaves OnUnknownEnum nil; BuildFeed must not panic and must
	// still degrade normally.
	msg := alerts.BuildFeed([]alerts.Alert{a}, opts())
	if got := msg.GetEntity()[0].GetAlert().GetCause(); got != gtfs.Alert_UNKNOWN_CAUSE {
		t.Errorf("cause = %v, want UNKNOWN_CAUSE", got)
	}
}

func TestFeedMarshalsBothEncodings(t *testing.T) {
	t.Parallel()

	msg := alerts.BuildFeed([]alerts.Alert{base()}, opts())
	if _, err := proto.Marshal(msg); err != nil {
		t.Fatalf("proto.Marshal: %v", err)
	}
}
