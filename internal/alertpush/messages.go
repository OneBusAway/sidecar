package alertpush

import (
	"unicode/utf8"

	"github.com/OneBusAway/sidecar/internal/alerts"
)

// BuildMessages derives the push copy snapshot from an alert (design spec
// §2.4): English from the alert's own text, plus every language with at
// least one non-stale translation (the feed's staleness rule: the stored
// source hash must equal the hash of the current English field), each
// missing field falling back to English. A blank description promotes the
// header to the body, because an empty-bodied notification is invisible.
func BuildMessages(a alerts.Alert) Messages {
	english := Message{Title: a.HeaderText, Body: a.DescriptionText}
	if english.Body == "" {
		english = Message{Title: "", Body: a.HeaderText}
	}
	m := Messages{EnglishKey: clampMessage(english)}

	headerHash := alerts.SourceHash(a.HeaderText)
	descHash := alerts.SourceHash(a.DescriptionText)
	for _, t := range a.Translations {
		lang := alerts.NormalizeLanguage(t.Language)
		if lang == "" || lang == EnglishKey {
			continue
		}
		var fresh bool
		switch t.Field {
		case alerts.FieldHeader:
			fresh = t.SourceSHA256 == headerHash
		case alerts.FieldDescription:
			fresh = t.SourceSHA256 == descHash
		}
		if !fresh {
			continue
		}
		msg, ok := m[lang]
		if !ok {
			msg = Message{Title: a.HeaderText, Body: a.DescriptionText}
		}
		switch t.Field {
		case alerts.FieldHeader:
			msg.Title = t.Text
		case alerts.FieldDescription:
			msg.Body = t.Text
		}
		m[lang] = msg
	}
	for lang, msg := range m {
		if lang == EnglishKey {
			continue
		}
		if msg.Body == "" {
			msg = Message{Title: "", Body: msg.Title}
		}
		m[lang] = clampMessage(msg)
	}
	return m
}

func clampMessage(msg Message) Message {
	return Message{Title: Clamp(msg.Title, TitleLimit), Body: Clamp(msg.Body, BodyLimit)}
}

// Clamp truncates s to at most limit runes, replacing the tail with a single
// "…" when it had to cut. It counts runes, never bytes, so multi-byte
// text is never split mid-code-point.
func Clamp(s string, limit int) string {
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	runes := []rune(s)
	return string(runes[:limit-1]) + "…"
}
