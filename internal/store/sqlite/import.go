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

// Import loads one export document in a single transaction. Rows are
// inserted with their source ids (or natural keys) and rows that already
// exist are skipped, so importing a later export of the same region is a
// delta. Nothing is written when any row fails validation: the whole
// document is checked before the first insert, and the transaction rolls
// back on any error after it. Every row is re-validated the way the
// authoring paths validate it -- enum names, question content, answer
// shape -- so a document cannot smuggle in a row the API would refuse.
func (s *Store) Import(ctx context.Context, doc *export.Document, now time.Time) (export.Summary, error) {
	var sum export.Summary
	if err := doc.Validate(); err != nil {
		return sum, err
	}
	prepared, err := prepareImport(doc)
	if err != nil {
		return sum, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return sum, fmt.Errorf("sqlite: import: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // rollback after commit is a no-op error
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
			continue
		}
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
				if _, err := q.ImportAlertTranslation(ctx, gen.ImportAlertTranslationParams{
					AlertID: a.ID, Language: lang, Field: string(f.field), Text: f.text,
					SourceSha256: alerts.SourceHash(f.source), CreatedAt: nowUnix, UpdatedAt: nowUnix,
				}); err != nil {
					return sum, fmt.Errorf("sqlite: import alert %d translation %s/%s: %w", a.ID, lang, f.field, err)
				}
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
		sum.Studies.Tally(n)
		for _, sv := range st.Surveys {
			stops, err := listToNull(sv.VisibleStopList)
			if err != nil {
				return sum, fmt.Errorf("sqlite: import survey %d: %w", sv.ID, err)
			}
			routes, err := listToNull(sv.VisibleRouteList)
			if err != nil {
				return sum, fmt.Errorf("sqlite: import survey %d: %w", sv.ID, err)
			}
			n, err := q.ImportSurvey(ctx, gen.ImportSurveyParams{
				ID: sv.ID, StudyID: st.ID, Name: sv.Name, Available: sv.Available,
				StartTime: timeToNullUnix(sv.StartTime), EndTime: timeToNullUnix(sv.EndTime),
				ShowOnMap: sv.ShowOnMap, ShowOnStops: sv.ShowOnStops, AlwaysVisible: sv.AlwaysVisible,
				AllowsMultipleResponses: sv.AllowsMultipleResponses,
				VisibleStopList:         stops, VisibleRouteList: routes,
				CreatedAt: orNow(sv.CreatedAt, nowUnix), UpdatedAt: orNow(sv.UpdatedAt, nowUnix),
			})
			if err != nil {
				return sum, fmt.Errorf("sqlite: import survey %d: %w", sv.ID, err)
			}
			sum.Surveys.Tally(n)
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
				sum.Questions.Tally(n)
			}
		}
	}

	for i, r := range doc.SurveyResponses {
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
		sum.SurveyResponses.Tally(n)
	}

	for _, p := range doc.PushRegistrations {
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
		if g.SnapshotStatus == "captured" && rawPresent(g.Snapshot) {
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
		sum.GhostBusReports.Tally(n)
	}

	if err := tx.Commit(); err != nil {
		return sum, fmt.Errorf("sqlite: import: commit: %w", err)
	}
	return sum, nil
}

// preparedImport holds the per-row derivations that can fail, computed
// before the transaction opens so validation never leaves a partial import.
type preparedImport struct {
	alerts    []preparedAlert
	questions map[int64]preparedQuestion
	answers   []string
}

type preparedQuestion struct{ typ, json string }

type preparedAlert struct{ cause, effect, severity string }

func prepareImport(doc *export.Document) (preparedImport, error) {
	p := preparedImport{
		alerts:    make([]preparedAlert, len(doc.Alerts)),
		questions: make(map[int64]preparedQuestion),
		answers:   make([]string, len(doc.SurveyResponses)),
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
	}
	for _, st := range doc.Studies {
		for _, sv := range st.Surveys {
			for _, qd := range sv.Questions {
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
		var answers []surveys.Answer
		if rawPresent(r.Answers) {
			if err := json.Unmarshal(r.Answers, &answers); err != nil {
				return p, fmt.Errorf("export: survey response %s: answers: %w", r.PublicID, err)
			}
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
