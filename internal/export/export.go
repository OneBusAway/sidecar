// Package export defines the interchange document that moves one region's
// agency content and rider state between sidecar deployments -- in
// particular from OBACloud, which produces it with `rake sidecar:export`,
// into this server via `sidecar-admin import`. Every id the apps persist
// locally (alert ids inside feed entity ids, survey and question ids inside
// answered-survey bookkeeping) is carried verbatim so a migrated region
// looks unchanged to riders.
//
// Instants are RFC 3339 with an explicit offset; epoch-millisecond fields
// are named *_ms and mirror the wire contract they came from.
package export

import (
	"encoding/json"
	"fmt"
	"time"
)

// Format is the value of Document.Format this package reads and writes.
const Format = "sidecar-export/1"

// Document is one region's export.
type Document struct {
	Format     string    `json:"format"`
	ExportedAt time.Time `json:"exported_at"`
	// RegionID is the regions directory id (the {regionId} path segment).
	RegionID          int64              `json:"region_id"`
	Alerts            []Alert            `json:"alerts"`
	Studies           []Study            `json:"studies"`
	SurveyResponses   []SurveyResponse   `json:"survey_responses"`
	PushRegistrations []PushRegistration `json:"push_registrations"`
	GhostBusReports   []GhostBusReport   `json:"ghost_bus_reports"`
}

// Alert is one authored service alert with its translations. Cause,
// Effect, and Severity are GTFS-realtime enum names.
type Alert struct {
	ID              int64              `json:"id"`
	AgencyID        string             `json:"agency_id"`
	HeaderText      string             `json:"header_text"`
	DescriptionText string             `json:"description_text"`
	URL             string             `json:"url"`
	Cause           string             `json:"cause"`
	Effect          string             `json:"effect"`
	Severity        string             `json:"severity"`
	StartTime       time.Time          `json:"start_time"`
	EndTime         *time.Time         `json:"end_time"`
	Published       bool               `json:"published"`
	IsTest          bool               `json:"is_test"`
	CreatedAt       time.Time          `json:"created_at"`
	UpdatedAt       time.Time          `json:"updated_at"`
	Translations    []AlertTranslation `json:"translations"`
}

// AlertTranslation is one language's rendering of an alert. Source* hold
// the English text the translation was made from, so a translation whose
// source has since been edited imports as stale rather than current.
type AlertTranslation struct {
	Language          string `json:"language"`
	HeaderText        string `json:"header_text"`
	DescriptionText   string `json:"description_text"`
	SourceHeader      string `json:"source_header_text"`
	SourceDescription string `json:"source_description_text"`
}

// Study groups surveys.
type Study struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	Surveys     []Survey  `json:"surveys"`
}

// Survey is one questionnaire. VisibleStopList / VisibleRouteList nil
// means everywhere.
type Survey struct {
	ID                      int64      `json:"id"`
	Name                    string     `json:"name"`
	Available               bool       `json:"available"`
	StartTime               *time.Time `json:"start_time"`
	EndTime                 *time.Time `json:"end_time"`
	ShowOnMap               bool       `json:"show_on_map"`
	ShowOnStops             bool       `json:"show_on_stops"`
	AlwaysVisible           bool       `json:"always_visible"`
	AllowsMultipleResponses bool       `json:"allows_multiple_responses"`
	VisibleStopList         []string   `json:"visible_stop_list"`
	VisibleRouteList        []string   `json:"visible_route_list"`
	CreatedAt               time.Time  `json:"created_at"`
	UpdatedAt               time.Time  `json:"updated_at"`
	Questions               []Question `json:"questions"`
}

// Question is one survey question; Content is the wire content document
// (spec section 7) and carries the type.
type Question struct {
	ID       int64           `json:"id"`
	Position int64           `json:"position"`
	Required bool            `json:"required"`
	Content  json.RawMessage `json:"content"`
}

// SurveyResponse is one rider submission. Answers is the wire array of
// answer objects (question_id, question_type, question_label, answer).
type SurveyResponse struct {
	SurveyID       int64           `json:"survey_id"`
	PublicID       string          `json:"public_id"`
	UserIdentifier string          `json:"user_identifier"`
	StopIdentifier string          `json:"stop_identifier"`
	StopLatitude   *float64        `json:"stop_latitude"`
	StopLongitude  *float64        `json:"stop_longitude"`
	Answers        json.RawMessage `json:"answers"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
}

// PushRegistration is one device's registration in the region.
type PushRegistration struct {
	Token           string    `json:"token"`
	OperatingSystem string    `json:"operating_system"` // ios | android
	Locale          string    `json:"locale"`
	APNSSandbox     bool      `json:"apns_sandbox"`
	TestDevice      bool      `json:"test_device"`
	Description     string    `json:"description"`
	LastSeenAt      time.Time `json:"last_seen_at"`
	CreatedAt       time.Time `json:"created_at"`
}

// GhostBusReport is one rider report plus its enrichment snapshot, if any.
type GhostBusReport struct {
	PublicID                 string          `json:"public_id"`
	UserIdentifier           string          `json:"user_identifier"`
	TripIdentifier           string          `json:"trip_identifier"`
	ServiceDateMS            int64           `json:"service_date_ms"`
	RouteIdentifier          string          `json:"route_identifier"`
	StopIdentifier           string          `json:"stop_identifier"`
	VehicleIdentifier        string          `json:"vehicle_identifier"`
	StopSequence             *int64          `json:"stop_sequence"`
	Predicted                *bool           `json:"predicted"`
	ScheduleDeviationMinutes *int64          `json:"schedule_deviation_minutes"`
	WaitDurationMinutes      int64           `json:"wait_duration_minutes"`
	Comment                  string          `json:"comment"`
	UserLatitude             *float64        `json:"user_latitude"`
	UserLongitude            *float64        `json:"user_longitude"`
	ScheduledArrivalMS       *int64          `json:"scheduled_arrival_ms"`
	PredictedArrivalMS       *int64          `json:"predicted_arrival_ms"`
	PredictionLastUpdatedMS  *int64          `json:"prediction_last_updated_ms"`
	SnapshotStatus           string          `json:"snapshot_status"` // pending | captured | unavailable
	Snapshot                 json.RawMessage `json:"snapshot"`        // object when captured, else null
	SnapshotCapturedAt       *time.Time      `json:"snapshot_captured_at"`
	CreatedAt                time.Time       `json:"created_at"`
}

// Counts is one kind's import tally. Skipped rows already existed (same
// id, public id, or (region, token)), which is what makes re-running an
// import on a later export a delta rather than a conflict.
type Counts struct {
	Added, Skipped int
}

// Tally records one insert attempt: n is the rows-affected count of an
// INSERT OR IGNORE, so 0 means the row already existed. It reports whether
// the row landed.
func (c *Counts) Tally(n int64) bool {
	if n == 0 {
		c.Skipped++
		return false
	}
	c.Added++
	return true
}

// Summary counts what an import did, per kind.
type Summary struct {
	Alerts, Studies, Surveys, Questions, SurveyResponses, PushRegistrations, GhostBusReports Counts
}

// Lines renders the summary one kind per line, for the CLI.
func (s Summary) Lines() []string {
	kinds := []struct {
		label string
		c     Counts
	}{
		{"alerts", s.Alerts}, {"studies", s.Studies}, {"surveys", s.Surveys}, {"questions", s.Questions},
		{"survey responses", s.SurveyResponses}, {"push registrations", s.PushRegistrations}, {"ghost bus reports", s.GhostBusReports},
	}
	out := make([]string, len(kinds))
	for i, k := range kinds {
		out[i] = fmt.Sprintf("  %-18s %d added, %d already present", k.label, k.c.Added, k.c.Skipped)
	}
	return out
}

// Validate checks the document's framing and the invariants the stores
// enforce with CHECK constraints, naming the offending row so the operator
// can fix the source rather than decode a constraint error.
func (d *Document) Validate() error {
	if d.Format != Format {
		return fmt.Errorf("export: unsupported format %q (want %q)", d.Format, Format)
	}
	if d.RegionID <= 0 {
		return fmt.Errorf("export: region_id must be positive, got %d", d.RegionID)
	}
	for _, a := range d.Alerts {
		if a.ID <= 0 || a.HeaderText == "" || a.StartTime.IsZero() {
			return fmt.Errorf("export: alert %d needs a positive id, header_text, and start_time", a.ID)
		}
		for _, t := range a.Translations {
			if t.Language == "" || t.Language == "en" {
				return fmt.Errorf("export: alert %d translation language %q must be a non-English BCP-47 tag", a.ID, t.Language)
			}
		}
	}
	for _, s := range d.Studies {
		if s.ID <= 0 || s.Name == "" {
			return fmt.Errorf("export: study %d needs a positive id and a name", s.ID)
		}
		for _, sv := range s.Surveys {
			if sv.ID <= 0 || sv.Name == "" {
				return fmt.Errorf("export: survey %d (study %d) needs a positive id and a name", sv.ID, s.ID)
			}
			if (sv.StartTime == nil) != (sv.EndTime == nil) {
				return fmt.Errorf("export: survey %d has only one of start_time/end_time; set both or neither", sv.ID)
			}
			for _, q := range sv.Questions {
				if q.ID <= 0 || len(q.Content) == 0 {
					return fmt.Errorf("export: question %d (survey %d) needs a positive id and content", q.ID, sv.ID)
				}
			}
		}
	}
	for _, r := range d.SurveyResponses {
		if r.SurveyID <= 0 || r.PublicID == "" || r.UserIdentifier == "" {
			return fmt.Errorf("export: survey response %q needs survey_id, public_id, and user_identifier", r.PublicID)
		}
	}
	for _, p := range d.PushRegistrations {
		if p.Token == "" || (p.OperatingSystem != "ios" && p.OperatingSystem != "android") {
			return fmt.Errorf("export: push registration %q needs a token and operating_system ios|android", p.Token)
		}
	}
	for _, g := range d.GhostBusReports {
		if g.PublicID == "" || g.UserIdentifier == "" || g.TripIdentifier == "" {
			return fmt.Errorf("export: ghost bus report %q needs public_id, user_identifier, and trip_identifier", g.PublicID)
		}
		switch g.SnapshotStatus {
		case "pending", "captured", "unavailable":
		default:
			return fmt.Errorf("export: ghost bus report %q has snapshot_status %q", g.PublicID, g.SnapshotStatus)
		}
	}
	return nil
}
