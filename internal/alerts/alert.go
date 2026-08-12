// Package alerts holds the service alert domain model and the pure function
// that renders alerts into a GTFS-realtime feed.
//
// Nothing in this package performs I/O or reads the clock. Times are absolute
// instants; see the design spec §2.3.
package alerts

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
)

// Field names the translatable text fields of an alert. The GTFS-realtime
// url field is English-only and deliberately absent.
type Field string

const (
	// FieldHeader is the alert header text field.
	FieldHeader Field = "header"
	// FieldDescription is the alert description text field.
	FieldDescription Field = "description"
)

// Translation is a non-English rendering of one alert field.
//
// SourceSHA256 is the hash of the English text the translation was made from.
// A translation whose hash no longer matches the current English is stale and
// is withheld from the feed, so riders read accurate English rather than
// outdated translated text.
type Translation struct {
	Language     string
	Field        Field
	Text         string
	SourceSHA256 string
}

// Alert is an authored service alert. Cause, Effect, and Severity hold
// GTFS-realtime enum names such as "CONSTRUCTION", not numbers.
type Alert struct {
	ID              int64
	RegionID        int64
	AgencyID        string
	HeaderText      string
	DescriptionText string
	URL             string
	Cause           string
	Effect          string
	Severity        string
	StartTime       time.Time
	EndTime         *time.Time
	Published       bool
	IsTest          bool
	CreatedAt       time.Time
	UpdatedAt       time.Time
	Translations    []Translation
}

// SourceHash returns the hex SHA-256 of s, used for translation staleness.
func SourceHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// NormalizeLanguage lowercases and trims a BCP-47 tag. Normalizing in Go
// rather than with SQL collation keeps behavior identical across engines.
func NormalizeLanguage(tag string) string {
	return strings.ToLower(strings.TrimSpace(tag))
}
