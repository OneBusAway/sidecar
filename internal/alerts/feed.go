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
			Alert: buildAlert(a, dur),
		})
	}
	return msg
}

func buildAlert(a Alert, dur time.Duration) *gtfs.Alert {
	start := uint64(a.StartTime.Unix())
	end := uint64(a.StartTime.Add(dur).Unix())
	if a.EndTime != nil {
		end = uint64(a.EndTime.Unix())
	}

	agencyID := a.AgencyID
	cause := CauseEnum(a.Cause)
	effect := EffectEnum(a.Effect)
	severity := SeverityEnum(a.Severity)

	out := &gtfs.Alert{
		ActivePeriod:    []*gtfs.TimeRange{{Start: &start, End: &end}},
		InformedEntity:  []*gtfs.EntitySelector{{AgencyId: &agencyID}},
		Cause:           &cause,
		Effect:          &effect,
		SeverityLevel:   &severity,
		HeaderText:      translated(a.HeaderText, a.Translations, FieldHeader),
		DescriptionText: translated(a.DescriptionText, a.Translations, FieldDescription),
	}
	if a.URL != "" {
		// url is English-only per the feed contract.
		out.Url = translated(a.URL, nil, FieldHeader)
	}
	return out
}

// translated builds a TranslatedString with English first, followed by any
// non-stale translations sorted by language tag.
//
// Sorting matters: ranging a map would vary the wire output between runs.
// Staleness is per-field — a translation made from an older English source is
// withheld so riders fall back to accurate English.
func translated(english string, all []Translation, field Field) *gtfs.TranslatedString {
	lang := englishTag
	text := english
	out := &gtfs.TranslatedString{
		Translation: []*gtfs.TranslatedString_Translation{{Language: &lang, Text: &text}},
	}

	want := SourceHash(english)
	fresh := make([]Translation, 0, len(all))
	for _, t := range all {
		if t.Field == field && t.SourceSHA256 == want {
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
