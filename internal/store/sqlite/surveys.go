package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/OneBusAway/sidecar/internal/store/sqlite/gen"
	"github.com/OneBusAway/sidecar/internal/surveys"
)

// Error strings from this repo never embed user_identifier, stop
// identifiers, or answers: they are rider data (spec §13) and callers log
// errors verbatim. Ids and counts only.
type surveyRepo struct {
	db *sql.DB
	q  *gen.Queries
}

func studyFromRow(r gen.Study) surveys.Study {
	return surveys.Study{
		ID: r.ID, RegionID: r.RegionID, Name: r.Name, Description: r.Description,
		CreatedAt: unixToTime(r.CreatedAt), UpdatedAt: unixToTime(r.UpdatedAt),
	}
}

// listToNull encodes a targeting list as a JSON array, or NULL for nil
// ("everywhere", design spec 2.11). Validate has already collapsed empty
// to nil, but a caller that skipped it must not store "[]" and have the
// Android client treat the survey as targeting nothing.
func listToNull(in []string) (sql.NullString, error) {
	if len(in) == 0 {
		return sql.NullString{}, nil
	}
	b, err := json.Marshal(in)
	if err != nil {
		return sql.NullString{}, err
	}
	return sql.NullString{String: string(b), Valid: true}, nil
}

func nullToList(in sql.NullString) ([]string, error) {
	if !in.Valid {
		return nil, nil
	}
	var out []string
	if err := json.Unmarshal([]byte(in.String), &out); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

func surveyFromRow(r gen.Survey) (surveys.Survey, error) {
	stops, err := nullToList(r.VisibleStopList)
	if err != nil {
		return surveys.Survey{}, fmt.Errorf("survey %d: visible_stop_list: %w", r.ID, err)
	}
	routes, err := nullToList(r.VisibleRouteList)
	if err != nil {
		return surveys.Survey{}, fmt.Errorf("survey %d: visible_route_list: %w", r.ID, err)
	}
	return surveys.Survey{
		ID: r.ID, StudyID: r.StudyID, Name: r.Name, Available: r.Available,
		StartTime: nullUnixToTime(r.StartTime), EndTime: nullUnixToTime(r.EndTime),
		ShowOnMap: r.ShowOnMap, ShowOnStops: r.ShowOnStops, AlwaysVisible: r.AlwaysVisible,
		AllowsMultipleResponses: r.AllowsMultipleResponses,
		VisibleStopList:         stops, VisibleRouteList: routes,
		CreatedAt: unixToTime(r.CreatedAt), UpdatedAt: unixToTime(r.UpdatedAt),
	}, nil
}

func questionFromRow(r gen.SurveyQuestion) (surveys.Question, error) {
	var c surveys.Content
	if err := json.Unmarshal([]byte(r.Content), &c); err != nil {
		// A row whose content does not parse is a corrupt row, and emitting
		// a half-decoded question would fail the whole region's survey list
		// on iOS (design spec 2.12); refuse the read instead.
		return surveys.Question{}, fmt.Errorf("question %d: content: %w", r.ID, err)
	}
	return surveys.Question{ID: r.ID, Position: r.Position, Required: r.Required, Content: c}, nil
}

func (r *surveyRepo) CreateStudy(ctx context.Context, regionID int64, name, description string, now time.Time) (surveys.Study, error) {
	row, err := r.q.CreateStudy(ctx, gen.CreateStudyParams{RegionID: regionID, Name: name, Description: description, Now: now.Unix()})
	if err != nil {
		return surveys.Study{}, fmt.Errorf("sqlite: create study in region %d: %w", regionID, err)
	}
	return studyFromRow(row), nil
}

func (r *surveyRepo) GetStudy(ctx context.Context, id int64) (surveys.Study, error) {
	row, err := r.q.GetStudy(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return surveys.Study{}, fmt.Errorf("sqlite: get study %d: %w", id, surveys.ErrNotFound)
		}
		return surveys.Study{}, fmt.Errorf("sqlite: get study %d: %w", id, err)
	}
	return studyFromRow(row), nil
}

func (r *surveyRepo) ListStudies(ctx context.Context, regionID int64) ([]surveys.Study, error) {
	rows, err := r.q.ListStudiesByRegion(ctx, regionID)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list studies in region %d: %w", regionID, err)
	}
	out := make([]surveys.Study, len(rows))
	for i, row := range rows {
		out[i] = studyFromRow(row)
	}
	return out, nil
}

// surveyParams maps a validated Definition to the column form shared by
// CreateSurvey and UpdateSurvey.
type surveyParams struct {
	start, end    sql.NullInt64
	stops, routes sql.NullString
}

func paramsFromDefinition(def surveys.Definition) (surveyParams, error) {
	stops, err := listToNull(def.VisibleStopList)
	if err != nil {
		return surveyParams{}, err
	}
	routes, err := listToNull(def.VisibleRouteList)
	if err != nil {
		return surveyParams{}, err
	}
	return surveyParams{
		start: timeToNullUnix(def.StartTime), end: timeToNullUnix(def.EndTime),
		stops: stops, routes: routes,
	}, nil
}

// insertQuestions writes def.Questions in document order; position is the
// 1-based index (design spec 2.13).
func insertQuestions(ctx context.Context, q *gen.Queries, surveyID int64, defs []surveys.QuestionDefinition, now int64) error {
	for i, qd := range defs {
		content, err := json.Marshal(qd.Content)
		if err != nil {
			return fmt.Errorf("question %d: %w", i+1, err)
		}
		if _, err := q.InsertQuestion(ctx, gen.InsertQuestionParams{
			SurveyID: surveyID, Position: int64(i + 1), Required: qd.Required,
			QuestionType: qd.Content.Type, Content: string(content), Now: now,
		}); err != nil {
			return fmt.Errorf("question %d: %w", i+1, err)
		}
	}
	return nil
}

// loadSurvey assembles a Survey from its row plus questions and study using
// the given query set (a tx's or the db's).
func loadSurvey(ctx context.Context, q *gen.Queries, row gen.Survey) (surveys.Survey, error) {
	s, err := surveyFromRow(row)
	if err != nil {
		return surveys.Survey{}, err
	}
	study, err := q.GetStudy(ctx, row.StudyID)
	if err != nil {
		return surveys.Survey{}, fmt.Errorf("study %d for survey %d: %w", row.StudyID, row.ID, err)
	}
	s.Study = studyFromRow(study)
	qrows, err := q.ListQuestionsBySurvey(ctx, row.ID)
	if err != nil {
		return surveys.Survey{}, fmt.Errorf("questions for survey %d: %w", row.ID, err)
	}
	s.Questions = make([]surveys.Question, 0, len(qrows))
	for _, qr := range qrows {
		question, err := questionFromRow(qr)
		if err != nil {
			return surveys.Survey{}, err
		}
		s.Questions = append(s.Questions, question)
	}
	return s, nil
}

func (r *surveyRepo) CreateSurvey(ctx context.Context, studyID int64, def surveys.Definition, now time.Time) (surveys.Survey, error) {
	p, err := paramsFromDefinition(def)
	if err != nil {
		return surveys.Survey{}, fmt.Errorf("sqlite: create survey: %w", err)
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return surveys.Survey{}, fmt.Errorf("sqlite: create survey: begin tx: %w", err)
	}
	//nolint:errcheck // rollback after a successful commit is a documented no-op; the error is expected and safe to ignore
	defer func() { _ = tx.Rollback() }()
	q := r.q.WithTx(tx)

	if _, err = q.GetStudy(ctx, studyID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return surveys.Survey{}, fmt.Errorf("sqlite: create survey in study %d: %w", studyID, surveys.ErrNotFound)
		}
		return surveys.Survey{}, fmt.Errorf("sqlite: create survey in study %d: %w", studyID, err)
	}
	row, err := q.CreateSurvey(ctx, gen.CreateSurveyParams{
		StudyID: studyID, Name: def.Name, Available: def.Available,
		StartTime: p.start, EndTime: p.end,
		ShowOnMap: def.ShowOnMap, ShowOnStops: def.ShowOnStops, AlwaysVisible: def.AlwaysVisible,
		AllowsMultipleResponses: def.AllowsMultipleResponses,
		VisibleStopList:         p.stops, VisibleRouteList: p.routes, Now: now.Unix(),
	})
	if err != nil {
		return surveys.Survey{}, fmt.Errorf("sqlite: create survey in study %d: %w", studyID, err)
	}
	if err = insertQuestions(ctx, q, row.ID, def.Questions, now.Unix()); err != nil {
		return surveys.Survey{}, fmt.Errorf("sqlite: create survey %d: %w", row.ID, err)
	}
	s, err := loadSurvey(ctx, q, row)
	if err != nil {
		return surveys.Survey{}, fmt.Errorf("sqlite: create survey: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return surveys.Survey{}, fmt.Errorf("sqlite: create survey: commit: %w", err)
	}
	return s, nil
}

func (r *surveyRepo) GetSurvey(ctx context.Context, id int64) (surveys.Survey, error) {
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return surveys.Survey{}, fmt.Errorf("sqlite: get survey %d: begin tx: %w", id, err)
	}
	//nolint:errcheck // rollback after a successful commit is a documented no-op; the error is expected and safe to ignore
	defer func() { _ = tx.Rollback() }()
	q := r.q.WithTx(tx)
	row, err := q.GetSurvey(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return surveys.Survey{}, fmt.Errorf("sqlite: get survey %d: %w", id, surveys.ErrNotFound)
		}
		return surveys.Survey{}, fmt.Errorf("sqlite: get survey %d: %w", id, err)
	}
	s, err := loadSurvey(ctx, q, row)
	if err != nil {
		return surveys.Survey{}, fmt.Errorf("sqlite: get survey: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return surveys.Survey{}, fmt.Errorf("sqlite: get survey %d: commit: %w", id, err)
	}
	return s, nil
}

// listSurveys runs the row query inside one read transaction and loads
// each survey's questions and study, so a list is a consistent snapshot
// (design spec 3.3).
func (r *surveyRepo) listSurveys(ctx context.Context, op string, rows func(*gen.Queries) ([]gen.Survey, error)) ([]surveys.Survey, error) {
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("sqlite: %s: begin tx: %w", op, err)
	}
	//nolint:errcheck // rollback after a successful commit is a documented no-op; the error is expected and safe to ignore
	defer func() { _ = tx.Rollback() }()
	q := r.q.WithTx(tx)
	srows, err := rows(q)
	if err != nil {
		return nil, fmt.Errorf("sqlite: %s: %w", op, err)
	}
	out := make([]surveys.Survey, 0, len(srows))
	for _, row := range srows {
		s, err := loadSurvey(ctx, q, row)
		if err != nil {
			return nil, fmt.Errorf("sqlite: %s: %w", op, err)
		}
		out = append(out, s)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("sqlite: %s: commit: %w", op, err)
	}
	return out, nil
}

func (r *surveyRepo) ListSurveys(ctx context.Context, regionID int64) ([]surveys.Survey, error) {
	return r.listSurveys(ctx, fmt.Sprintf("list surveys in region %d", regionID), func(q *gen.Queries) ([]gen.Survey, error) {
		return q.ListSurveysByRegion(ctx, regionID)
	})
}

func (r *surveyRepo) ListActiveSurveys(ctx context.Context, regionID int64, now time.Time) ([]surveys.Survey, error) {
	return r.listSurveys(ctx, fmt.Sprintf("list active surveys in region %d", regionID), func(q *gen.Queries) ([]gen.Survey, error) {
		return q.ListActiveSurveysByRegion(ctx, gen.ListActiveSurveysByRegionParams{RegionID: regionID, Now: now.Unix()})
	})
}

// UpdateSurvey rewrites the scalars and replaces the question set
// wholesale -- unless responses exist, in which case the document's
// questions must equal the stored ones (design spec 2.13). The check and
// both writes share one immediate transaction, so a response arriving
// between the count and the delete cannot orphan its question ids.
func (r *surveyRepo) UpdateSurvey(ctx context.Context, id int64, def surveys.Definition, now time.Time) (surveys.Survey, error) {
	p, err := paramsFromDefinition(def)
	if err != nil {
		return surveys.Survey{}, fmt.Errorf("sqlite: update survey %d: %w", id, err)
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return surveys.Survey{}, fmt.Errorf("sqlite: update survey %d: begin tx: %w", id, err)
	}
	//nolint:errcheck // rollback after a successful commit is a documented no-op; the error is expected and safe to ignore
	defer func() { _ = tx.Rollback() }()
	q := r.q.WithTx(tx)

	current, err := q.GetSurvey(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return surveys.Survey{}, fmt.Errorf("sqlite: update survey %d: %w", id, surveys.ErrNotFound)
		}
		return surveys.Survey{}, fmt.Errorf("sqlite: update survey %d: %w", id, err)
	}
	responses, err := q.CountResponsesForSurvey(ctx, id)
	if err != nil {
		return surveys.Survey{}, fmt.Errorf("sqlite: update survey %d: count responses: %w", id, err)
	}
	replaceQuestions := true
	if responses > 0 {
		var stored surveys.Survey
		stored, err = loadSurvey(ctx, q, current)
		if err != nil {
			return surveys.Survey{}, fmt.Errorf("sqlite: update survey: %w", err)
		}
		if !surveys.QuestionsEqual(stored.Questions, def.Questions) {
			return surveys.Survey{}, fmt.Errorf("sqlite: update survey %d with %d responses: %w", id, responses, surveys.ErrQuestionsFrozen)
		}
		replaceQuestions = false
	}
	row, err := q.UpdateSurvey(ctx, gen.UpdateSurveyParams{
		ID: id, Name: def.Name, Available: def.Available,
		StartTime: p.start, EndTime: p.end,
		ShowOnMap: def.ShowOnMap, ShowOnStops: def.ShowOnStops, AlwaysVisible: def.AlwaysVisible,
		AllowsMultipleResponses: def.AllowsMultipleResponses,
		VisibleStopList:         p.stops, VisibleRouteList: p.routes, Now: now.Unix(),
	})
	if err != nil {
		return surveys.Survey{}, fmt.Errorf("sqlite: update survey %d: %w", id, err)
	}
	if replaceQuestions {
		if err = q.DeleteQuestionsForSurvey(ctx, id); err != nil {
			return surveys.Survey{}, fmt.Errorf("sqlite: update survey %d: delete questions: %w", id, err)
		}
		if err = insertQuestions(ctx, q, id, def.Questions, now.Unix()); err != nil {
			return surveys.Survey{}, fmt.Errorf("sqlite: update survey %d: %w", id, err)
		}
	}
	s, err := loadSurvey(ctx, q, row)
	if err != nil {
		return surveys.Survey{}, fmt.Errorf("sqlite: update survey: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return surveys.Survey{}, fmt.Errorf("sqlite: update survey %d: commit: %w", id, err)
	}
	return s, nil
}

// DeleteSurvey refuses while responses exist: they are indefinite agency
// data (spec §13, design spec 2.15). Count and delete share a transaction.
func (r *surveyRepo) DeleteSurvey(ctx context.Context, id int64) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: delete survey %d: begin tx: %w", id, err)
	}
	//nolint:errcheck // rollback after a successful commit is a documented no-op; the error is expected and safe to ignore
	defer func() { _ = tx.Rollback() }()
	q := r.q.WithTx(tx)
	n, err := q.CountResponsesForSurvey(ctx, id)
	if err != nil {
		return fmt.Errorf("sqlite: delete survey %d: count responses: %w", id, err)
	}
	if n > 0 {
		return fmt.Errorf("sqlite: delete survey %d with %d responses: %w", id, n, surveys.ErrHasResponses)
	}
	affected, err := q.DeleteSurvey(ctx, id)
	if err != nil {
		return fmt.Errorf("sqlite: delete survey %d: %w", id, err)
	}
	if affected == 0 {
		return fmt.Errorf("sqlite: delete survey %d: %w", id, surveys.ErrNotFound)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: delete survey %d: commit: %w", id, err)
	}
	return nil
}

func (r *surveyRepo) CountResponses(ctx context.Context, surveyID int64) (int64, error) {
	n, err := r.q.CountResponsesForSurvey(ctx, surveyID)
	if err != nil {
		return 0, fmt.Errorf("sqlite: count responses for survey %d: %w", surveyID, err)
	}
	return n, nil
}

func floatToNull(v *float64) sql.NullFloat64 {
	if v == nil {
		return sql.NullFloat64{}
	}
	return sql.NullFloat64{Float64: *v, Valid: true}
}

func nullToFloat(v sql.NullFloat64) *float64 {
	if !v.Valid {
		return nil
	}
	f := v.Float64
	return &f
}

func encodeAnswers(in []surveys.Answer) (string, error) {
	if in == nil {
		in = []surveys.Answer{}
	}
	b, err := json.Marshal(in)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func responseFromRow(r gen.SurveyResponse) (surveys.Response, error) {
	answers := []surveys.Answer{}
	if err := json.Unmarshal([]byte(r.Answers), &answers); err != nil {
		return surveys.Response{}, fmt.Errorf("response %d: answers: %w", r.ID, err)
	}
	if answers == nil {
		answers = []surveys.Answer{}
	}
	return surveys.Response{
		ID: r.ID, SurveyID: r.SurveyID, PublicID: r.PublicID, UserIdentifier: r.UserIdentifier,
		StopIdentifier: r.StopIdentifier.String,
		StopLatitude:   nullToFloat(r.StopLatitude), StopLongitude: nullToFloat(r.StopLongitude),
		Answers:   answers,
		CreatedAt: unixToTime(r.CreatedAt), UpdatedAt: unixToTime(r.UpdatedAt),
	}, nil
}

// CreateResponse checks survey existence and inserts the response in one
// transaction: without it, the insert's foreign key would be the only
// thing stopping a response from attaching to a survey deleted between the
// check and the insert, and that failure is a raw driver error, not
// surveys.ErrNotFound -- sharing a transaction keeps the interface's
// ErrNotFound contract true without string-matching an FK violation.
func (r *surveyRepo) CreateResponse(ctx context.Context, in surveys.NewResponse, now time.Time) (surveys.Response, error) {
	answers, err := encodeAnswers(in.Answers)
	if err != nil {
		return surveys.Response{}, fmt.Errorf("sqlite: create response for survey %d: %w", in.SurveyID, err)
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return surveys.Response{}, fmt.Errorf("sqlite: create response for survey %d: begin tx: %w", in.SurveyID, err)
	}
	//nolint:errcheck // rollback after a successful commit is a documented no-op; the error is expected and safe to ignore
	defer func() { _ = tx.Rollback() }()
	q := r.q.WithTx(tx)

	if _, err = q.GetSurvey(ctx, in.SurveyID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return surveys.Response{}, fmt.Errorf("sqlite: create response for survey %d: %w", in.SurveyID, surveys.ErrNotFound)
		}
		return surveys.Response{}, fmt.Errorf("sqlite: create response for survey %d: %w", in.SurveyID, err)
	}
	stop := sql.NullString{}
	if in.StopIdentifier != "" {
		stop = sql.NullString{String: in.StopIdentifier, Valid: true}
	}
	row, err := q.CreateResponse(ctx, gen.CreateResponseParams{
		SurveyID: in.SurveyID, PublicID: in.PublicID, UserIdentifier: in.UserIdentifier,
		StopIdentifier: stop, StopLatitude: floatToNull(in.StopLatitude), StopLongitude: floatToNull(in.StopLongitude),
		Answers: answers, Now: now.Unix(),
	})
	if err != nil {
		return surveys.Response{}, fmt.Errorf("sqlite: create response for survey %d: %w", in.SurveyID, err)
	}
	out, err := responseFromRow(row)
	if err != nil {
		return surveys.Response{}, fmt.Errorf("sqlite: create response: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return surveys.Response{}, fmt.Errorf("sqlite: create response for survey %d: commit: %w", in.SurveyID, err)
	}
	return out, nil
}

func (r *surveyRepo) GetResponse(ctx context.Context, publicID string) (surveys.Response, error) {
	row, err := r.q.GetResponseByPublicID(ctx, publicID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return surveys.Response{}, fmt.Errorf("sqlite: get response: %w", surveys.ErrNotFound)
		}
		return surveys.Response{}, fmt.Errorf("sqlite: get response: %w", err)
	}
	out, err := responseFromRow(row)
	if err != nil {
		return surveys.Response{}, fmt.Errorf("sqlite: get response: %w", err)
	}
	return out, nil
}

// AmendResponse is a read-merge-write inside one write transaction. With
// the store's _txlock=immediate DSN the transaction holds the write lock
// from BEGIN, so a concurrent amend waits on busy_timeout rather than
// failing after its read with SQLITE_BUSY_SNAPSHOT (design spec 2.6).
func (r *surveyRepo) AmendResponse(ctx context.Context, publicID string, incoming []surveys.Answer, now time.Time) (surveys.Response, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return surveys.Response{}, fmt.Errorf("sqlite: amend response: begin tx: %w", err)
	}
	//nolint:errcheck // rollback after a successful commit is a documented no-op; the error is expected and safe to ignore
	defer func() { _ = tx.Rollback() }()
	q := r.q.WithTx(tx)

	current, err := q.GetResponseByPublicID(ctx, publicID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return surveys.Response{}, fmt.Errorf("sqlite: amend response: %w", surveys.ErrNotFound)
		}
		return surveys.Response{}, fmt.Errorf("sqlite: amend response: %w", err)
	}
	stored, err := responseFromRow(current)
	if err != nil {
		return surveys.Response{}, fmt.Errorf("sqlite: amend response: %w", err)
	}
	merged := surveys.MergeAnswers(stored.Answers, incoming)
	if len(merged) > surveys.MaxAnswers {
		return surveys.Response{}, fmt.Errorf("sqlite: amend response %d: %w", stored.ID, surveys.ErrTooManyAnswers)
	}
	encoded, err := encodeAnswers(merged)
	if err != nil {
		return surveys.Response{}, fmt.Errorf("sqlite: amend response %d: %w", stored.ID, err)
	}
	row, err := q.UpdateResponseAnswers(ctx, gen.UpdateResponseAnswersParams{PublicID: publicID, Answers: encoded, Now: now.Unix()})
	if err != nil {
		return surveys.Response{}, fmt.Errorf("sqlite: amend response %d: %w", stored.ID, err)
	}
	out, err := responseFromRow(row)
	if err != nil {
		return surveys.Response{}, fmt.Errorf("sqlite: amend response: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return surveys.Response{}, fmt.Errorf("sqlite: amend response %d: commit: %w", stored.ID, err)
	}
	return out, nil
}

func (r *surveyRepo) ListResponses(ctx context.Context, surveyID int64) ([]surveys.Response, error) {
	rows, err := r.q.ListResponsesBySurvey(ctx, surveyID)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list responses for survey %d: %w", surveyID, err)
	}
	out := make([]surveys.Response, 0, len(rows))
	for _, row := range rows {
		resp, err := responseFromRow(row)
		if err != nil {
			return nil, fmt.Errorf("sqlite: list responses for survey %d: %w", surveyID, err)
		}
		out = append(out, resp)
	}
	return out, nil
}
