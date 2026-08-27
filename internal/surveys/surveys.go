// Package surveys is the domain for spec §7: agency-authored rider
// questionnaires and the incremental responses riders submit to them. The
// package is pure -- types, validation, answer parsing and merging, wire
// formatting -- so every contract in the design spec is testable without a
// database or an HTTP server. Storage lives behind Repository.
package surveys

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"
)

var (
	// ErrNotFound is returned for an unknown study, survey, or response id.
	// Callers know which entity they asked for; the HTTP layer maps it to
	// the entity-specific 404 body (design spec §2.7).
	ErrNotFound = errors.New("not found")
	// ErrHasResponses blocks DeleteSurvey: responses are indefinite agency
	// data (spec §13; design spec §2.15).
	ErrHasResponses = errors.New("survey has responses")
	// ErrQuestionsFrozen blocks an UpdateSurvey that would change the
	// question set once responses exist, since stored answers reference
	// question ids (design spec §2.13).
	ErrQuestionsFrozen = errors.New("survey questions are frozen")
	// ErrMalformedAnswers is the reference implementation's exact message
	// for a responses parameter that is not a JSON-array string of objects
	// (design spec §2.5); the HTTP layer writes err.Error() to the wire.
	ErrMalformedAnswers = errors.New("responses must be a JSON-encoded array of answer objects")
	// ErrTooManyAnswers is the MaxAnswers cap (design spec §2.5, §2.6).
	ErrTooManyAnswers = errors.New("responses has too many answers")
	// ErrAnswerTooLong is the per-field byte cap on one answer element
	// (design spec §2.5, §2.6).
	ErrAnswerTooLong = errors.New("responses contains an answer that is too long")
)

// Study is a questionnaire collection authored by an agency for a region.
type Study struct {
	ID          int64
	RegionID    int64
	Name        string
	Description string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Survey is one questionnaire as stored. Questions is in position order
// and Study is populated on every read, because the iOS client rejects a
// survey without a study object (design spec §2.3).
type Survey struct {
	ID                      int64
	StudyID                 int64
	Name                    string
	Available               bool
	StartTime               *time.Time // both nil or both set (design spec §2.4)
	EndTime                 *time.Time
	ShowOnMap               bool
	ShowOnStops             bool
	AlwaysVisible           bool
	AllowsMultipleResponses bool
	VisibleStopList         []string // nil = everywhere; never empty (design spec §2.11)
	VisibleRouteList        []string
	Questions               []Question
	Study                   Study
	CreatedAt               time.Time
	UpdatedAt               time.Time
}

// Question is a single question in a survey, with its content and metadata.
type Question struct {
	ID       int64
	Position int64
	Required bool
	Content  Content
}

// Answer is one element of the responses array, stored verbatim as
// strings: the two apps disagree on the checkbox answer format and the
// server never interprets it (design spec §2.5).
type Answer struct {
	QuestionID    int64  `json:"question_id"`
	QuestionType  string `json:"question_type"`
	QuestionLabel string `json:"question_label"`
	Answer        string `json:"answer"`
}

// Response is a rider's submission of answers to a survey.
type Response struct {
	ID             int64
	SurveyID       int64
	PublicID       string
	UserIdentifier string
	StopIdentifier string   // "" = absent
	StopLatitude   *float64 // nil = absent
	StopLongitude  *float64
	Answers        []Answer
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// NewResponse is what the create endpoint hands the repository. PublicID is
// minted by the caller (securetoken) so the repository never needs an
// entropy source.
type NewResponse struct {
	SurveyID       int64
	PublicID       string
	UserIdentifier string
	StopIdentifier string
	StopLatitude   *float64
	StopLongitude  *float64
	Answers        []Answer
}

// QuestionDefinition is one question in an authoring document; position is
// implied by array order (design spec §2.13).
type QuestionDefinition struct {
	Required bool    `json:"required"`
	Content  Content `json:"content"`
}

// Definition is the authoring document of design spec §2.13 -- the wire
// Survey minus every server-owned key, plus Available.
type Definition struct {
	Name                    string
	Available               bool
	StartTime               *time.Time
	EndTime                 *time.Time
	ShowOnMap               bool
	ShowOnStops             bool
	AlwaysVisible           bool
	AllowsMultipleResponses bool
	VisibleStopList         []string
	VisibleRouteList        []string
	Questions               []QuestionDefinition
}

// Repository is the storage contract. Implementations are safe for
// concurrent use; CreateSurvey, UpdateSurvey, DeleteSurvey and
// AmendResponse each run in one write transaction (design spec §2.6, §3.2).
type Repository interface {
	CreateStudy(ctx context.Context, regionID int64, name, description string, now time.Time) (Study, error)
	GetStudy(ctx context.Context, id int64) (Study, error) // ErrNotFound
	ListStudies(ctx context.Context, regionID int64) ([]Study, error)
	// UpdateStudy renames a study. The region is a query condition, so a
	// study in another region is ErrNotFound and nothing is written
	// (design spec section 3.2).
	UpdateStudy(ctx context.Context, regionID, id int64, name, description string, now time.Time) (Study, error)

	CreateSurvey(ctx context.Context, studyID int64, def Definition, now time.Time) (Survey, error) // ErrNotFound for the study
	// CreateSurveyInRegion is CreateSurvey with the study's region as a
	// JOIN condition: a study_id that arrived in a request body but belongs
	// to another region is ErrNotFound. Body-borne ids never go through a
	// load-then-compare.
	CreateSurveyInRegion(ctx context.Context, regionID, studyID int64, def Definition, now time.Time) (Survey, error)
	GetSurvey(ctx context.Context, id int64) (Survey, error)                                   // ErrNotFound; with Questions and Study
	ListSurveys(ctx context.Context, regionID int64) ([]Survey, error)                         // every survey, authoring
	ListActiveSurveys(ctx context.Context, regionID int64, now time.Time) ([]Survey, error)    // spec §7.1 filter
	UpdateSurvey(ctx context.Context, id int64, def Definition, now time.Time) (Survey, error) // ErrNotFound, ErrQuestionsFrozen
	DeleteSurvey(ctx context.Context, id int64) error                                          // ErrNotFound, ErrHasResponses
	CountResponses(ctx context.Context, surveyID int64) (int64, error)

	CreateResponse(ctx context.Context, in NewResponse, now time.Time) (Response, error)                    // ErrNotFound for the survey
	GetResponse(ctx context.Context, publicID string) (Response, error)                                     // ErrNotFound
	AmendResponse(ctx context.Context, publicID string, incoming []Answer, now time.Time) (Response, error) // ErrNotFound, ErrTooManyAnswers
	ListResponses(ctx context.Context, surveyID int64) ([]Response, error)
	// GetResponseInRegion resolves one response through its survey's study's
	// region, in a single query.
	GetResponseInRegion(ctx context.Context, regionID int64, publicID string) (Response, error)
}

// canonicalJSON is the one place json.RawMessage values are normalized, so
// equality comparisons (QuestionsEqual) and stored bytes never depend on
// the author's whitespace or escaping. It produces exactly the bytes
// encoding/json will emit for the value when the row is stored (compact,
// with <, >, & HTML-escaped), which is what a later read hands back to
// ContentEqual. Empty or a JSON null in, nil out: "sdk_configuration_values":
// null is how a document spells "unset", the same as the targeting lists.
func canonicalJSON(raw json.RawMessage) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	return b, nil
}
