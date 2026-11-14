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
	"regexp"
	"strings"
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

	// IDFloor is the sidecar's id-sequence floor (`sidecar-admin sequence
	// bump --min`); Validate rejects any carried id at or above it. It is
	// the importer's setting, never part of the wire format (the CLI's
	// --id-floor), and zero means DefaultIDFloor.
	IDFloor int64 `json:"-"`
}

// DefaultIDFloor is the floor the cutover runbook bumps the alerts,
// studies, surveys, and survey_questions sequences to before the first
// region migrates, and OBACloud's Sidecar::Cutover::ID_HEADROOM: every id
// OBACloud exports sits below it, and every id this database mints after
// the bump sits above it. An imported id at or above the floor could only
// collide with sidecar-authored content -- or, at MaxInt64, leave a shared
// AUTOINCREMENT sequence with no id left to mint for any region.
const DefaultIDFloor int64 = 1_000_000

// floor is the effective IDFloor.
func (d *Document) floor() int64 {
	if d.IDFloor > 0 {
		return d.IDFloor
	}
	return DefaultIDFloor
}

// checkFloor rejects an id at or above the document's floor.
func (d *Document) checkFloor(kind string, id int64) error {
	if id >= d.floor() {
		return fmt.Errorf("export: %s id %d is at or above the sequence floor %d; OBACloud ids must stay below it (README, Id headroom)", kind, id, d.floor())
	}
	return nil
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

// Survey is one questionnaire. VisibleStopList / VisibleRouteList nil or
// empty means everywhere.
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
	Alerts, Translations, Studies, Surveys, Questions, SurveyResponses, PushRegistrations, GhostBusReports Counts
}

// Lines renders the summary one kind per line, for the CLI.
func (s Summary) Lines() []string {
	kinds := []struct {
		label string
		c     Counts
	}{
		{"alerts", s.Alerts}, {"translations", s.Translations}, {"studies", s.Studies}, {"surveys", s.Surveys}, {"questions", s.Questions},
		{"survey responses", s.SurveyResponses}, {"push registrations", s.PushRegistrations}, {"ghost bus reports", s.GhostBusReports},
	}
	out := make([]string, len(kinds))
	for i, k := range kinds {
		out[i] = fmt.Sprintf("  %-18s %d added, %d already present", k.label, k.c.Added, k.c.Skipped)
	}
	return out
}

// Validate checks the document's framing, the invariants the stores
// enforce with CHECK constraints, and that every persisted key (ids,
// public ids, tokens, translation languages) appears once, naming the
// offending row so the operator can fix the source rather than decode a
// constraint error. Domain rules that need the domain packages (enum
// names, question content, answers, time windows) are checked by the
// importer before its transaction opens.
func (d *Document) Validate() error {
	if d.Format != Format {
		return fmt.Errorf("export: unsupported format %q (want %q)", d.Format, Format)
	}
	if d.RegionID <= 0 {
		return fmt.Errorf("export: region_id must be positive, got %d", d.RegionID)
	}
	for _, check := range []func() error{d.validateAlerts, d.validateStudies, d.validateResponses, d.validateRegistrations, d.validateReports} {
		if err := check(); err != nil {
			return err
		}
	}
	return nil
}

// publicIDPattern is the shape of a survey response or ghost bus report
// public id: OBACloud mints 20 hex characters, this server 22 URL-safe
// base64 ones. Either appears as a path segment (a survey amend URL, an
// admin link), so anything outside the URL-safe alphabet or past 64
// characters is not a public id either side ever minted.
var publicIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// keySet rejects a repeated persisted key.
type keySet map[string]struct{}

func (k keySet) add(kind string, key any) error {
	s := fmt.Sprint(key)
	if _, dup := k[s]; dup {
		return fmt.Errorf("export: %s %s appears more than once", kind, s)
	}
	k[s] = struct{}{}
	return nil
}

func (d *Document) validateAlerts() error {
	ids := keySet{}
	for _, a := range d.Alerts {
		if a.ID <= 0 || strings.TrimSpace(a.HeaderText) == "" || a.StartTime.IsZero() || strings.TrimSpace(a.AgencyID) == "" {
			return fmt.Errorf("export: alert %d needs a positive id, header_text, start_time, and agency_id", a.ID)
		}
		if err := d.checkFloor("alert", a.ID); err != nil {
			return err
		}
		if err := ids.add("alert", a.ID); err != nil {
			return err
		}
		langs := keySet{}
		for _, t := range a.Translations {
			// Compared normalized, the way the store writes it, so "EN" is
			// caught here and not by the CHECK constraint mid-transaction.
			lang := strings.ToLower(strings.TrimSpace(t.Language))
			if lang == "" || lang == "en" {
				return fmt.Errorf("export: alert %d translation language %q must be a non-English BCP-47 tag", a.ID, t.Language)
			}
			if err := langs.add(fmt.Sprintf("alert %d translation", a.ID), lang); err != nil {
				return err
			}
		}
	}
	return nil
}

func (d *Document) validateStudies() error {
	studies, surveys, questions := keySet{}, keySet{}, keySet{}
	for _, s := range d.Studies {
		if s.ID <= 0 || s.Name == "" {
			return fmt.Errorf("export: study %d needs a positive id and a name", s.ID)
		}
		if err := d.checkFloor("study", s.ID); err != nil {
			return err
		}
		if err := studies.add("study", s.ID); err != nil {
			return err
		}
		for _, sv := range s.Surveys {
			if err := d.validateSurvey(s.ID, sv, surveys, questions); err != nil {
				return err
			}
		}
	}
	return nil
}

func (d *Document) validateSurvey(studyID int64, sv Survey, surveys, questions keySet) error {
	if sv.ID <= 0 || sv.Name == "" {
		return fmt.Errorf("export: survey %d (study %d) needs a positive id and a name", sv.ID, studyID)
	}
	if (sv.StartTime == nil) != (sv.EndTime == nil) {
		return fmt.Errorf("export: survey %d has only one of start_time/end_time; set both or neither", sv.ID)
	}
	if err := d.checkFloor("survey", sv.ID); err != nil {
		return err
	}
	if err := surveys.add("survey", sv.ID); err != nil {
		return err
	}
	for _, q := range sv.Questions {
		if q.ID <= 0 || len(q.Content) == 0 {
			return fmt.Errorf("export: question %d (survey %d) needs a positive id and content", q.ID, sv.ID)
		}
		if err := d.checkFloor("question", q.ID); err != nil {
			return err
		}
		if err := questions.add("question", q.ID); err != nil {
			return err
		}
	}
	return nil
}

func (d *Document) validateResponses() error {
	ids := keySet{}
	for _, r := range d.SurveyResponses {
		if r.SurveyID <= 0 || r.PublicID == "" || r.UserIdentifier == "" {
			return fmt.Errorf("export: survey response %q needs survey_id, public_id, and user_identifier", r.PublicID)
		}
		if !publicIDPattern.MatchString(r.PublicID) {
			return fmt.Errorf("export: survey response %.80q: public_id must be 1-64 URL-safe characters", r.PublicID)
		}
		// A response can name a survey an earlier run migrated, so its
		// survey_id is held to the floor on its own.
		if err := d.checkFloor(fmt.Sprintf("survey response %q survey", r.PublicID), r.SurveyID); err != nil {
			return err
		}
		if err := ids.add("survey response", r.PublicID); err != nil {
			return err
		}
	}
	return nil
}

func (d *Document) validateRegistrations() error {
	tokens := keySet{}
	for _, p := range d.PushRegistrations {
		if p.Token == "" || (p.OperatingSystem != "ios" && p.OperatingSystem != "android") {
			return fmt.Errorf("export: push registration %q needs a token and operating_system ios|android", p.Token)
		}
		if err := tokens.add("push registration", p.Token); err != nil {
			return err
		}
	}
	return nil
}

func (d *Document) validateReports() error {
	ids := keySet{}
	for _, g := range d.GhostBusReports {
		if g.PublicID == "" || g.UserIdentifier == "" || g.TripIdentifier == "" {
			return fmt.Errorf("export: ghost bus report %q needs public_id, user_identifier, and trip_identifier", g.PublicID)
		}
		if !publicIDPattern.MatchString(g.PublicID) {
			return fmt.Errorf("export: ghost bus report %.80q: public_id must be 1-64 URL-safe characters", g.PublicID)
		}
		if err := ids.add("ghost bus report", g.PublicID); err != nil {
			return err
		}
		switch g.SnapshotStatus {
		case "pending", "unavailable":
		case "captured":
			if len(g.Snapshot) == 0 || string(g.Snapshot) == "null" {
				return fmt.Errorf("export: ghost bus report %q is captured but has no snapshot", g.PublicID)
			}
		default:
			return fmt.Errorf("export: ghost bus report %q has snapshot_status %q", g.PublicID, g.SnapshotStatus)
		}
	}
	return nil
}
