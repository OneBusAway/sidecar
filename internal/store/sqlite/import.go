package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/OneBusAway/sidecar/internal/alerts"
	"github.com/OneBusAway/sidecar/internal/export"
	"github.com/OneBusAway/sidecar/internal/store/sqlite/gen"
	"github.com/OneBusAway/sidecar/internal/surveys"
)

// ErrRegionMissing is returned by Import when the document's region has
// not been synced into this database yet.
var ErrRegionMissing = errors.New("sqlite: import: region not found; sync the regions directory first")

// ErrImportConflict marks a row whose id or public id already exists in
// this database but belongs to something else -- another region, another
// study, another survey. Alerts, studies, surveys, and questions share one
// global id sequence, so the second region migrated into a database that
// has authored anything of its own can collide with a source id. Silently
// skipping such a row would drop it (or, for a study or survey, attach the
// rows beneath it to another region's content), so it is an error naming
// the row instead.
var ErrImportConflict = errors.New("sqlite: import: id already belongs to another region or parent")

// ValidateImport runs every check Import runs before it writes anything:
// the document's own invariants, the derivations that can fail (enum
// names, question content, answer shape, time windows), and the region's
// presence. `sidecar-admin import --dry-run` reports through this so a
// clean dry run means the real import will not stop on validation.
func (s *Store) ValidateImport(ctx context.Context, doc *export.Document, now time.Time) error {
	if _, err := prepareImport(doc, now); err != nil {
		return err
	}
	if _, err := s.q.GetRegion(ctx, doc.RegionID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w (region %d)", ErrRegionMissing, doc.RegionID)
		}
		return fmt.Errorf("sqlite: import: region %d: %w", doc.RegionID, err)
	}
	return nil
}

// Import loads one export document in a single transaction. Rows are
// inserted with their source ids (or natural keys); a row whose key already
// exists and belongs to this region is skipped and counted, which makes a
// second run on a later export a delta, while a key that exists under
// another region or parent is ErrImportConflict. Everything that can be
// checked without the database (Validate, prepareImport) runs before the
// transaction opens; anything the database itself rejects -- a foreign
// key, a CHECK, a second UNIQUE such as a question's position -- rolls the
// whole transaction back, so a bad document never leaves a partial import.
// Enum names, question content, answer shape, and time windows are
// re-validated with the domain packages' own rules; the HTTP layer's
// length caps and locale normalization are not, on the assumption the
// source enforced them.
func (s *Store) Import(ctx context.Context, doc *export.Document, now time.Time) (export.Summary, error) {
	var sum export.Summary
	prepared, err := prepareImport(doc, now)
	if err != nil {
		return sum, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return sum, fmt.Errorf("sqlite: import: begin: %w", err)
	}
	defer func() {
		// ErrTxDone is the normal outcome after Commit; anything else means
		// the rollback of a failed import did not happen cleanly.
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			s.logger.Warn("sqlite: import: rollback", "err", rbErr)
		}
	}()
	q := gen.New(tx)

	if _, err := q.GetRegion(ctx, doc.RegionID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return sum, fmt.Errorf("%w (region %d)", ErrRegionMissing, doc.RegionID)
		}
		return sum, fmt.Errorf("sqlite: import: region %d: %w", doc.RegionID, err)
	}

	nowUnix := now.Unix()
	for i, a := range doc.Alerts {
		p := prepared.alerts[i]
		n, err := q.ImportAlert(ctx, gen.ImportAlertParams{
			ID: a.ID, RegionID: doc.RegionID, AgencyID: a.AgencyID,
			HeaderText: a.HeaderText, DescriptionText: a.DescriptionText, Url: a.URL,
			Cause: p.cause, Effect: p.effect, SeverityLevel: p.severity,
			StartTime: a.StartTime.Unix(), EndTime: timeToNullUnix(a.EndTime),
			Published: a.Published, IsTest: a.IsTest,
			CreatedAt: orNow(a.CreatedAt, nowUnix), UpdatedAt: orNow(a.UpdatedAt, nowUnix),
		})
		if err != nil {
			return sum, fmt.Errorf("sqlite: import alert %d: %w", a.ID, err)
		}
		if !sum.Alerts.Tally(n) {
			existing, err := q.GetAlert(ctx, a.ID)
			if err != nil || existing.RegionID != doc.RegionID {
				return sum, conflict("alert", a.ID, err)
			}
		}
		// Translations are attempted whether or not the alert row was new:
		// a later export can add a language to an alert already migrated,
		// and that is exactly what the delta run is for.
		for _, t := range a.Translations {
			lang := alerts.NormalizeLanguage(t.Language)
			for _, f := range []struct {
				field        alerts.Field
				text, source string
			}{
				{alerts.FieldHeader, t.HeaderText, firstNonEmpty(t.SourceHeader, a.HeaderText)},
				{alerts.FieldDescription, t.DescriptionText, firstNonEmpty(t.SourceDescription, a.DescriptionText)},
			} {
				if f.text == "" {
					continue
				}
				n, err := q.ImportAlertTranslation(ctx, gen.ImportAlertTranslationParams{
					AlertID: a.ID, Language: lang, Field: string(f.field), Text: f.text,
					SourceSha256: alerts.SourceHash(f.source), CreatedAt: nowUnix, UpdatedAt: nowUnix,
				})
				if err != nil {
					return sum, fmt.Errorf("sqlite: import alert %d translation %s/%s: %w", a.ID, lang, f.field, err)
				}
				sum.Translations.Tally(n)
			}
		}
	}

	for _, st := range doc.Studies {
		n, err := q.ImportStudy(ctx, gen.ImportStudyParams{
			ID: st.ID, RegionID: doc.RegionID, Name: st.Name, Description: st.Description,
			CreatedAt: orNow(st.CreatedAt, nowUnix), UpdatedAt: orNow(st.UpdatedAt, nowUnix),
		})
		if err != nil {
			return sum, fmt.Errorf("sqlite: import study %d: %w", st.ID, err)
		}
		if !sum.Studies.Tally(n) {
			existing, err := q.GetStudy(ctx, st.ID)
			if err != nil || existing.RegionID != doc.RegionID {
				return sum, conflict("study", st.ID, err)
			}
		}
		for _, sv := range st.Surveys {
			sp := prepared.surveys[sv.ID]
			n, err := q.ImportSurvey(ctx, gen.ImportSurveyParams{
				ID: sv.ID, StudyID: st.ID, Name: sv.Name, Available: sv.Available,
				StartTime: timeToNullUnix(sv.StartTime), EndTime: timeToNullUnix(sv.EndTime),
				ShowOnMap: sv.ShowOnMap, ShowOnStops: sv.ShowOnStops, AlwaysVisible: sv.AlwaysVisible,
				AllowsMultipleResponses: sv.AllowsMultipleResponses,
				VisibleStopList:         sp.stops, VisibleRouteList: sp.routes,
				CreatedAt: orNow(sv.CreatedAt, nowUnix), UpdatedAt: orNow(sv.UpdatedAt, nowUnix),
			})
			if err != nil {
				return sum, fmt.Errorf("sqlite: import survey %d: %w", sv.ID, err)
			}
			if !sum.Surveys.Tally(n) {
				existing, err := q.GetSurvey(ctx, sv.ID)
				if err != nil || existing.StudyID != st.ID {
					return sum, conflict("survey", sv.ID, err)
				}
			}
			for _, qd := range sv.Questions {
				content := prepared.questions[qd.ID]
				n, err := q.ImportSurveyQuestion(ctx, gen.ImportSurveyQuestionParams{
					ID: qd.ID, SurveyID: sv.ID, Position: qd.Position, Required: qd.Required,
					QuestionType: content.typ, Content: content.json,
					CreatedAt: nowUnix, UpdatedAt: nowUnix,
				})
				if err != nil {
					return sum, fmt.Errorf("sqlite: import question %d (survey %d): %w", qd.ID, sv.ID, err)
				}
				if !sum.Questions.Tally(n) {
					owner, err := q.GetQuestionSurveyID(ctx, qd.ID)
					if err != nil || owner != sv.ID {
						return sum, conflict("question", qd.ID, err)
					}
				}
			}
		}
	}

	for i, r := range doc.SurveyResponses {
		// The survey a response names must be this region's, whether it
		// came with the document or was migrated earlier: the foreign key
		// alone would happily attach a rider's answers to another region's
		// survey of the same id.
		if err := surveyInRegion(ctx, q, r.SurveyID, doc.RegionID); err != nil {
			return sum, fmt.Errorf("survey response %s: %w", r.PublicID, err)
		}
		n, err := q.ImportSurveyResponse(ctx, gen.ImportSurveyResponseParams{
			SurveyID: r.SurveyID, PublicID: r.PublicID, UserIdentifier: r.UserIdentifier,
			StopIdentifier: nullString(r.StopIdentifier),
			StopLatitude:   floatToNull(r.StopLatitude), StopLongitude: floatToNull(r.StopLongitude),
			Answers:   prepared.answers[i],
			CreatedAt: orNow(r.CreatedAt, nowUnix), UpdatedAt: orNow(r.UpdatedAt, nowUnix),
		})
		if err != nil {
			return sum, fmt.Errorf("sqlite: import survey response %s: %w", r.PublicID, err)
		}
		if !sum.SurveyResponses.Tally(n) {
			existing, err := q.GetResponseByPublicID(ctx, r.PublicID)
			if err != nil || existing.SurveyID != r.SurveyID {
				return sum, fmt.Errorf("%w: survey response %s", ErrImportConflict, r.PublicID)
			}
		}
	}

	for _, p := range doc.PushRegistrations {
		// The natural key includes the region, so a skip is always this
		// region's own earlier row (or the device re-registering directly).
		n, err := q.ImportPushRegistration(ctx, gen.ImportPushRegistrationParams{
			RegionID: doc.RegionID, Token: p.Token, OperatingSystem: p.OperatingSystem,
			ApnsSandbox: p.APNSSandbox, Locale: p.Locale, TestDevice: p.TestDevice, Description: p.Description,
			LastSeenAt: orNow(p.LastSeenAt, nowUnix), CreatedAt: orNow(p.CreatedAt, nowUnix), UpdatedAt: nowUnix,
		})
		if err != nil {
			return sum, fmt.Errorf("sqlite: import push registration: %w", err)
		}
		sum.PushRegistrations.Tally(n)
	}

	for _, g := range doc.GhostBusReports {
		var attempts int64
		if g.SnapshotStatus == "unavailable" {
			attempts = 3 // matches a report the enrichment loop gave up on
		}
		snapshot := ""
		if g.SnapshotStatus == "captured" {
			snapshot = string(g.Snapshot)
		}
		n, err := q.ImportGhostBusReport(ctx, gen.ImportGhostBusReportParams{
			RegionID: doc.RegionID, PublicIdentifier: g.PublicID, UserIdentifier: g.UserIdentifier,
			TripIdentifier: g.TripIdentifier, ServiceDate: g.ServiceDateMS,
			RouteIdentifier: g.RouteIdentifier, StopIdentifier: g.StopIdentifier, VehicleIdentifier: g.VehicleIdentifier,
			StopSequence: int64ToNullInt64(g.StopSequence), Predicted: boolToNullInt64(g.Predicted),
			ScheduleDeviationMinutes: int64ToNullInt64(g.ScheduleDeviationMinutes), WaitDurationMinutes: g.WaitDurationMinutes,
			Comment: g.Comment, UserLatitude: floatToNull(g.UserLatitude), UserLongitude: floatToNull(g.UserLongitude),
			ScheduledArrivalAt: int64ToNullInt64(g.ScheduledArrivalMS), PredictedArrivalAt: int64ToNullInt64(g.PredictedArrivalMS),
			PredictionLastUpdatedAt: int64ToNullInt64(g.PredictionLastUpdatedMS),
			SnapshotStatus:          g.SnapshotStatus, SnapshotJson: snapshot,
			SnapshotCapturedAt: timeToNullUnix(g.SnapshotCapturedAt), SnapshotAttempts: attempts,
			CreatedAt: orNow(g.CreatedAt, nowUnix), UpdatedAt: nowUnix,
		})
		if err != nil {
			return sum, fmt.Errorf("sqlite: import ghost bus report %s: %w", g.PublicID, err)
		}
		if !sum.GhostBusReports.Tally(n) {
			// The lookup is region-scoped, so "not found" means the public
			// id exists under another region.
			if _, err := q.GetGhostBusReportByPublicID(ctx, gen.GetGhostBusReportByPublicIDParams{
				RegionID: doc.RegionID, PublicIdentifier: g.PublicID,
			}); err != nil {
				return sum, fmt.Errorf("%w: ghost bus report %s", ErrImportConflict, g.PublicID)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return sum, fmt.Errorf("sqlite: import: commit: %w", err)
	}
	return sum, nil
}

// surveyInRegion is ErrImportConflict unless survey id exists and its
// study belongs to regionID.
func surveyInRegion(ctx context.Context, q *gen.Queries, id, regionID int64) error {
	sv, err := q.GetSurvey(ctx, id)
	if err != nil {
		return conflict("survey", id, err)
	}
	st, err := q.GetStudy(ctx, sv.StudyID)
	if err != nil || st.RegionID != regionID {
		return conflict("survey", id, err)
	}
	return nil
}

// conflict wraps ErrImportConflict for an id-keyed row; a lookup error
// other than "not found" is reported as itself.
func conflict(kind string, id int64, lookupErr error) error {
	if lookupErr != nil && !errors.Is(lookupErr, sql.ErrNoRows) {
		return fmt.Errorf("sqlite: import %s %d: %w", kind, id, lookupErr)
	}
	return fmt.Errorf("%w: %s %d", ErrImportConflict, kind, id)
}

// preparedImport holds the per-row derivations that can fail, computed
// before the transaction opens so validation never leaves a partial import.
type preparedImport struct {
	alerts    []preparedAlert
	surveys   map[int64]preparedSurvey
	questions map[int64]preparedQuestion
	answers   []string
}

type preparedAlert struct{ cause, effect, severity string }

type preparedSurvey struct{ stops, routes sql.NullString }

type preparedQuestion struct{ typ, json string }

func prepareImport(doc *export.Document, now time.Time) (preparedImport, error) {
	p := preparedImport{
		alerts:    make([]preparedAlert, len(doc.Alerts)),
		surveys:   make(map[int64]preparedSurvey),
		questions: make(map[int64]preparedQuestion),
		answers:   make([]string, len(doc.SurveyResponses)),
	}
	if err := doc.Validate(); err != nil {
		return p, err
	}
	for i, a := range doc.Alerts {
		var err error
		if p.alerts[i].cause, err = alerts.ParseCause(a.Cause); err != nil {
			return p, fmt.Errorf("export: alert %d: %w", a.ID, err)
		}
		if p.alerts[i].effect, err = alerts.ParseEffect(a.Effect); err != nil {
			return p, fmt.Errorf("export: alert %d: %w", a.ID, err)
		}
		if p.alerts[i].severity, err = alerts.ParseSeverity(a.Severity); err != nil {
			return p, fmt.Errorf("export: alert %d: %w", a.ID, err)
		}
		if err := alerts.ValidateWindow(a.StartTime, a.EndTime, now); err != nil {
			return p, fmt.Errorf("export: alert %d: %w", a.ID, err)
		}
	}
	seenQuestions := make(map[int64]int64) // question id -> survey id
	for _, st := range doc.Studies {
		for _, sv := range st.Surveys {
			if err := surveys.ValidateWindow(sv.StartTime, sv.EndTime); err != nil {
				return p, fmt.Errorf("export: survey %d: %w", sv.ID, err)
			}
			stops, err := listToNull(sv.VisibleStopList)
			if err != nil {
				return p, fmt.Errorf("export: survey %d: %w", sv.ID, err)
			}
			routes, err := listToNull(sv.VisibleRouteList)
			if err != nil {
				return p, fmt.Errorf("export: survey %d: %w", sv.ID, err)
			}
			p.surveys[sv.ID] = preparedSurvey{stops: stops, routes: routes}
			for _, qd := range sv.Questions {
				if other, dup := seenQuestions[qd.ID]; dup {
					return p, fmt.Errorf("export: question %d appears in surveys %d and %d", qd.ID, other, sv.ID)
				}
				seenQuestions[qd.ID] = sv.ID
				var c surveys.Content
				if err := json.Unmarshal(qd.Content, &c); err != nil {
					return p, fmt.Errorf("export: question %d: content: %w", qd.ID, err)
				}
				if err := c.Validate(); err != nil {
					return p, fmt.Errorf("export: question %d: %w", qd.ID, err)
				}
				raw, err := json.Marshal(c)
				if err != nil {
					return p, fmt.Errorf("export: question %d: %w", qd.ID, err)
				}
				p.questions[qd.ID] = preparedQuestion{typ: c.Type, json: string(raw)}
			}
		}
	}
	for i, r := range doc.SurveyResponses {
		raw := "[]"
		if rawPresent(r.Answers) {
			raw = string(r.Answers)
		}
		// ParseAnswers is the rider API's own decoder: strict structure,
		// string coercion, and merge-by-question_id.
		answers, err := surveys.ParseAnswers(raw)
		if err != nil {
			return p, fmt.Errorf("export: survey response %s: answers: %w", r.PublicID, err)
		}
		encoded, err := encodeAnswers(answers)
		if err != nil {
			return p, fmt.Errorf("export: survey response %s: %w", r.PublicID, err)
		}
		p.answers[i] = encoded
	}
	return p, nil
}

// rawPresent reports whether a JSON value was supplied and is not null.
func rawPresent(raw json.RawMessage) bool {
	return len(raw) > 0 && string(raw) != "null"
}

func orNow(t time.Time, now int64) int64 {
	if t.IsZero() {
		return now
	}
	return t.Unix()
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func nullString(s string) sql.NullString {
	return sql.NullString{String: s, Valid: s != ""}
}
