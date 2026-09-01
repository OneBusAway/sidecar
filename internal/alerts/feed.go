package alerts

import (
	"fmt"
	"sort"
	"time"

	"github.com/MobilityData/gtfs-realtime-bindings/golang/gtfs"
)

const (
	// FeedLimit caps the feed. This is a "current conditions" feed, not an
	// archive; the apps re-fetch frequently.
	FeedLimit = 20

	// DefaultDuration is advertised as the active period when an author set
	// no end time. Without it an open-ended alert pins itself to the top of
	// riders' feeds forever.
	DefaultDuration = 8 * time.Hour

	feedVersion  = "1.0"
	englishTag   = "en"
	entityPrefix = "Alert_"
)

// FeedOptions carries everything BuildFeed needs from the outside world, so
// the function itself stays pure and deterministic.
type FeedOptions struct {
	Now             time.Time
	DefaultDuration time.Duration

	// OnUnknownEnum, if non-nil, is invoked once for each stored cause,
	// effect, or severity name that does not map to a known GTFS-realtime
	// enum value (kind is "cause", "effect", or "severity"; name is the
	// offending stored value). This package performs no I/O and imports no
	// logger itself -- callers that want the warning logged (design spec
	// §4.2, §7: "log warn, keep serving") wire this to their own logger.
	// A nil callback is safe: the degradation to UNKNOWN_* still happens,
	// just silently.
	OnUnknownEnum func(kind, name string)
}

// BuildFeed renders alerts into a GTFS-realtime FeedMessage.
//
// It applies no filtering, ordering, or capping: SQL decides which alerts
// appear and in what order, this function decides what they look like on the
// wire. Callers pass rows already filtered to published (plus the test flag),
// ordered start_time DESC then id DESC, and capped at FeedLimit.
func BuildFeed(in []Alert, opts FeedOptions) *gtfs.FeedMessage {
	dur := opts.DefaultDuration
	if dur <= 0 {
		dur = DefaultDuration
	}

	version := feedVersion
	incrementality := gtfs.FeedHeader_FULL_DATASET
	timestamp := uint64(opts.Now.Unix())

	msg := &gtfs.FeedMessage{
		Header: &gtfs.FeedHeader{
			GtfsRealtimeVersion: &version,
			Incrementality:      &incrementality,
			Timestamp:           &timestamp,
		},
		Entity: make([]*gtfs.FeedEntity, 0, len(in)),
	}

	for i := range in {
		a := in[i]
		id := entityPrefix + fmt.Sprint(a.ID)
		msg.Entity = append(msg.Entity, &gtfs.FeedEntity{
			Id:    &id,
			Alert: buildAlert(a, dur, opts),
		})
	}
	return msg
}

func buildAlert(a Alert, dur time.Duration, opts FeedOptions) *gtfs.Alert {
	start := uint64(a.StartTime.Unix())
	end := uint64(a.StartTime.Add(dur).Unix())
	if a.EndTime != nil {
		end = uint64(a.EndTime.Unix())
	}

	agencyID := a.AgencyID
	cause, causeOK := causeLookup(a.Cause)
	effect, effectOK := effectLookup(a.Effect)
	severity, severityOK := severityLookup(a.Severity)
	if opts.OnUnknownEnum != nil {
		if !causeOK {
			opts.OnUnknownEnum("cause", a.Cause)
		}
		if !effectOK {
			opts.OnUnknownEnum("effect", a.Effect)
		}
		if !severityOK {
			opts.OnUnknownEnum("severity", a.Severity)
		}
	}

	out := &gtfs.Alert{
		ActivePeriod:   []*gtfs.TimeRange{{Start: &start, End: &end}},
		InformedEntity: []*gtfs.EntitySelector{{AgencyId: &agencyID}},
		Cause:          &cause,
		Effect:         &effect,
		SeverityLevel:  &severity,
		HeaderText:     a.translated(a.HeaderText, FieldHeader),
	}
	if a.DescriptionText != "" {
		// description_text is optional in the CLI and NOT NULL DEFAULT ''
		// in storage, so "" means "no description", not "a blank one" --
		// guarded the same way url is guarded below. Emitting an empty
		// TranslatedString here would make a consumer that branches on
		// presence see a description that exists but is blank, rather than
		// correctly seeing none.
		out.DescriptionText = a.translated(a.DescriptionText, FieldDescription)
	}
	if a.URL != "" {
		// url is English-only per the feed contract, so it goes out with no
		// translation candidates at all rather than through translated().
		out.Url = englishOnly(a.URL)
	}
	return out
}

// englishOnly builds a TranslatedString carrying just the English text, for a
// field the feed never translates.
func englishOnly(english string) *gtfs.TranslatedString {
	lang := englishTag
	text := english
	return &gtfs.TranslatedString{
		Translation: []*gtfs.TranslatedString_Translation{{Language: &lang, Text: &text}},
	}
}

// translated builds a TranslatedString with English first, followed by any
// non-stale translation of field, sorted by language tag. english must be the
// alert's own text for field.
//
// Sorting matters: ranging a map would vary the wire output between runs.
// Whether a translation is fresh is TranslationStale's judgement and only its
// judgement: what the feed withholds and what the admin API reports as "stale"
// are one rule, so they cannot drift apart.
func (a Alert) translated(english string, field Field) *gtfs.TranslatedString {
	out := englishOnly(english)

	fresh := make([]Translation, 0, len(a.Translations))
	for _, t := range a.Translations {
		if t.Field == field && !a.TranslationStale(t) {
			fresh = append(fresh, t)
		}
	}
	sort.Slice(fresh, func(i, j int) bool { return fresh[i].Language < fresh[j].Language })

	for i := range fresh {
		out.Translation = append(out.Translation, &gtfs.TranslatedString_Translation{
			Language: &fresh[i].Language,
			Text:     &fresh[i].Text,
		})
	}
	return out
}
