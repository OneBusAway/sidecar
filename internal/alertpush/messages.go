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
	m := Messages{EnglishKey: clampMessage(promoteHeader(sourceMessage(a)))}

	headerHash := alerts.SourceHash(a.HeaderText)
	descHash := alerts.SourceHash(a.DescriptionText)
	for _, t := range a.Translations {
		lang := alerts.NormalizeLanguage(t.Language)
		if lang == "" || lang == EnglishKey || !freshTranslation(t, headerHash, descHash) {
			continue
		}
		base, ok := m[lang]
		if !ok {
			base = sourceMessage(a)
		}
		m[lang] = applyTranslation(base, t)
	}

	for lang, msg := range m {
		if lang == EnglishKey {
			continue
		}
		m[lang] = clampMessage(promoteHeader(msg))
	}
	return m
}

// sourceMessage is the untranslated copy every language starts from: the
// alert's own English text, which each fresh translation then overwrites
// field by field.
func sourceMessage(a alerts.Alert) Message {
	return Message{Title: a.HeaderText, Body: a.DescriptionText}
}

// freshTranslation reports whether t still describes the English field it
// was translated from: the feed's staleness rule is that the stored source
// hash equals the hash of the current English text. A translation of an
// unknown field is never fresh.
func freshTranslation(t alerts.Translation, headerHash, descHash string) bool {
	switch t.Field {
	case alerts.FieldHeader:
		return t.SourceSHA256 == headerHash
	case alerts.FieldDescription:
		return t.SourceSHA256 == descHash
	default:
		return false
	}
}

// applyTranslation overwrites the one field t translates, leaving the other
// at its English fallback.
func applyTranslation(msg Message, t alerts.Translation) Message {
	switch t.Field {
	case alerts.FieldHeader:
		msg.Title = t.Text
	case alerts.FieldDescription:
		msg.Body = t.Text
	}
	return msg
}

// promoteHeader moves the title into an empty body, because a notification
// with no body is invisible on both platforms.
func promoteHeader(msg Message) Message {
	if msg.Body != "" {
		return msg
	}
	return Message{Title: "", Body: msg.Title}
}

func clampMessage(msg Message) Message {
	return Message{Title: Clamp(msg.Title, TitleLimit), Body: Clamp(msg.Body, BodyLimit)}
}

// Clamp truncates s to at most limit runes, replacing the tail with a single
// "…" when it had to cut. It counts runes, never bytes, so multi-byte
// text is never split mid-code-point. A limit of zero or less leaves no room
// for even the ellipsis, so it yields the empty string rather than panicking.
func Clamp(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	runes := []rune(s)
	return string(runes[:limit-1]) + "…"
}
