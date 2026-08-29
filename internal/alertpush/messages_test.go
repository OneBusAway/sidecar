package alertpush_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/OneBusAway/sidecar/internal/alertpush"
	"github.com/OneBusAway/sidecar/internal/alerts"
)

func TestBuildMessagesEnglishAndFreshTranslations(t *testing.T) {
	a := alerts.Alert{
		HeaderText:      "Route 44 detour",
		DescriptionText: "Buses skip 3rd Ave this weekend.",
		Translations: []alerts.Translation{
			{Language: "es", Field: alerts.FieldHeader, Text: "Desvío ruta 44", SourceSHA256: alerts.SourceHash("Route 44 detour")},
			// Stale: hash of an older English description.
			{Language: "es", Field: alerts.FieldDescription, Text: "VIEJO", SourceSHA256: alerts.SourceHash("old text")},
			// Every field stale: language must not appear at all.
			{Language: "fr", Field: alerts.FieldHeader, Text: "VIEUX", SourceSHA256: alerts.SourceHash("old")},
		},
	}
	got := alertpush.BuildMessages(a)
	want := alertpush.Messages{
		"en": {Title: "Route 44 detour", Body: "Buses skip 3rd Ave this weekend."},
		"es": {Title: "Desvío ruta 44", Body: "Buses skip 3rd Ave this weekend."},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("BuildMessages = %+v, want %+v", got, want)
	}
	if cat := got.Catalog(); !reflect.DeepEqual(cat, []string{"es"}) {
		t.Errorf("Catalog = %v, want [es]", cat)
	}
	if m := got.For(""); m != want["en"] {
		t.Errorf("For(\"\") = %+v, want English", m)
	}
	if m := got.For("de"); m != want["en"] {
		t.Errorf("For(de) = %+v, want English fallback", m)
	}
	if m := got.For("es"); m != want["es"] {
		t.Errorf("For(es) = %+v, want Spanish", m)
	}
}

func TestBuildMessagesBlankDescriptionPromotesHeaderToBody(t *testing.T) {
	got := alertpush.BuildMessages(alerts.Alert{HeaderText: "Elevator out at Westlake"})
	if want := (alertpush.Message{Title: "", Body: "Elevator out at Westlake"}); got["en"] != want {
		t.Errorf("en = %+v, want %+v", got["en"], want)
	}
}

func TestBuildMessagesClamps(t *testing.T) {
	long := strings.Repeat("é", 200) // multi-byte: a byte-based clamp would split a rune
	got := alertpush.BuildMessages(alerts.Alert{HeaderText: long, DescriptionText: long})
	if n := len([]rune(got["en"].Title)); n != alertpush.TitleLimit {
		t.Errorf("title runes = %d, want %d", n, alertpush.TitleLimit)
	}
	if !strings.HasSuffix(got["en"].Title, "…") {
		t.Errorf("title %q lacks ellipsis", got["en"].Title)
	}
	if n := len([]rune(got["en"].Body)); n != alertpush.BodyLimit {
		t.Errorf("body runes = %d, want %d", n, alertpush.BodyLimit)
	}
	if s := alertpush.Clamp("short", 48); s != "short" {
		t.Errorf("Clamp(short) = %q", s)
	}
	if s := alertpush.Clamp(strings.Repeat("x", 48), 48); s != strings.Repeat("x", 48) {
		t.Errorf("Clamp at exactly the limit must not truncate: %q", s)
	}
	// A non-positive limit has no room for the ellipsis: the empty string,
	// never a panic on runes[:limit-1].
	for _, limit := range []int{0, -1} {
		if s := alertpush.Clamp("short", limit); s != "" {
			t.Errorf("Clamp(short, %d) = %q, want the empty string", limit, s)
		}
	}
}

func TestValidateMessages(t *testing.T) {
	t.Parallel()
	ok := alertpush.Message{Title: "Route 40", Body: "Detour until Friday"}
	for _, tc := range []struct {
		name string
		in   alertpush.Messages
		want bool
	}{
		{"english only", alertpush.Messages{"en": ok}, true},
		{"english plus a translation", alertpush.Messages{"en": ok, "es": {Title: "Ruta 40", Body: "Desvio"}}, true},
		{"empty title is fine (header promoted to body)", alertpush.Messages{"en": {Body: "Detour"}}, true},
		{"title at the cap", alertpush.Messages{"en": {Title: strings.Repeat("t", alertpush.TitleLimit), Body: "b"}}, true},
		{"body at the cap", alertpush.Messages{"en": {Body: strings.Repeat("b", alertpush.BodyLimit)}}, true},
		{"empty set", alertpush.Messages{}, false},
		{"no english", alertpush.Messages{"es": ok}, false},
		{"blank body", alertpush.Messages{"en": {Title: "t", Body: "   "}}, false},
		{"title over the cap", alertpush.Messages{"en": {Title: strings.Repeat("t", alertpush.TitleLimit+1), Body: "b"}}, false},
		{"body over the cap", alertpush.Messages{"en": {Body: strings.Repeat("b", alertpush.BodyLimit+1)}}, false},
		{"multi-byte body at the cap counts runes", alertpush.Messages{"en": {Body: strings.Repeat("中", alertpush.BodyLimit)}}, true},
		{"un-normalized language", alertpush.Messages{"en": ok, "ES": ok}, false},
		{"blank language", alertpush.Messages{"en": ok, "": ok}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := alertpush.ValidateMessages(tc.in)
			if (err == nil) != tc.want {
				t.Fatalf("err = %v, want ok=%v", err, tc.want)
			}
			if err != nil && !errors.Is(err, alertpush.ErrInvalidMessages) {
				t.Errorf("err = %v, want ErrInvalidMessages", err)
			}
		})
	}
}
