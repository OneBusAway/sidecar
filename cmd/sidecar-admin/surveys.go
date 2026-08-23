package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/OneBusAway/sidecar/internal/regions"
	"github.com/OneBusAway/sidecar/internal/store/sqlite"
	"github.com/OneBusAway/sidecar/internal/surveys"
)

// ---------------------------------------------------------------------------
// study
// ---------------------------------------------------------------------------

func runStudy(ctx context.Context, stdout io.Writer, store *sqlite.Store, now time.Time, args []string) error {
	if len(args) == 0 {
		return errors.New("study requires a subcommand: create, list")
	}
	switch args[0] {
	case "create":
		return studyCreate(ctx, stdout, store, now, args[1:])
	case "list":
		return studyList(ctx, stdout, store, args[1:])
	default:
		return fmt.Errorf("unknown study subcommand %q", args[0])
	}
}

func studyCreate(ctx context.Context, stdout io.Writer, store *sqlite.Store, now time.Time, args []string) error {
	fs := flag.NewFlagSet("study create", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	regionID := fs.Int64("region", 0, "region id (required)")
	name := fs.String("name", "", "study name (required)")
	description := fs.String("description", "", "study description")
	if err := fs.Parse(args); err != nil {
		return err
	}
	seen := visitedFlags(fs)
	if !seen["region"] || !seen["name"] {
		return errors.New("study create requires --region and --name")
	}
	if strings.TrimSpace(*name) == "" {
		return errors.New("study create: --name cannot be blank")
	}
	if _, err := store.Regions().Get(ctx, *regionID); err != nil {
		return fmt.Errorf("study create: %w", err)
	}
	st, err := store.Surveys().CreateStudy(ctx, *regionID, strings.TrimSpace(*name), strings.TrimSpace(*description), now)
	if err != nil {
		return fmt.Errorf("study create: %w", err)
	}
	fmt.Fprintf(stdout, "created study %d\n", st.ID)
	return nil
}

func studyList(ctx context.Context, stdout io.Writer, store *sqlite.Store, args []string) error {
	fs := flag.NewFlagSet("study list", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	regionID := fs.Int64("region", 0, "region id (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !visitedFlags(fs)["region"] {
		return errors.New("study list requires --region")
	}
	list, err := store.Surveys().ListStudies(ctx, *regionID)
	if err != nil {
		return fmt.Errorf("study list: %w", err)
	}
	for _, st := range list {
		fmt.Fprintf(stdout, "%d\tregion=%d\t%s\t%s\n", st.ID, st.RegionID, st.Name, st.Description)
	}
	return nil
}

// ---------------------------------------------------------------------------
// survey
// ---------------------------------------------------------------------------

func runSurvey(ctx context.Context, stdin io.Reader, stdout io.Writer, store *sqlite.Store, now time.Time, args []string) error {
	if len(args) == 0 {
		return errors.New("survey requires a subcommand: create, list, show, edit, delete, responses")
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "create":
		return surveyCreate(ctx, stdin, stdout, store, now, rest)
	case "list":
		return surveyList(ctx, stdout, store, rest)
	case "show":
		return surveyShow(ctx, stdout, store, rest)
	case "edit":
		return surveyEdit(ctx, stdin, store, now, rest)
	case "delete":
		return surveyDelete(ctx, store, rest)
	case "responses":
		return surveyResponses(ctx, stdout, store, rest)
	default:
		return fmt.Errorf("unknown survey subcommand %q", cmd)
	}
}

// surveyDocument is the authoring document of design spec 2.13: the wire
// Survey shape minus server-owned keys, plus available. The server-owned
// keys are declared (and ignored on input) so `show | edit --file -` round
// trips; everything else unknown is rejected so a typo like
// "show_on_maps" cannot silently become a hidden survey.
type surveyDocument struct {
	ID                      *int64             `json:"id,omitempty"`
	Name                    string             `json:"name"`
	Available               *bool              `json:"available"`
	StartDate               *string            `json:"start_date"`
	EndDate                 *string            `json:"end_date"`
	ShowOnMap               bool               `json:"show_on_map"`
	ShowOnStops             bool               `json:"show_on_stops"`
	AlwaysVisible           bool               `json:"always_visible"`
	AllowsMultipleResponses bool               `json:"allows_multiple_responses"`
	VisibleStopList         []string           `json:"visible_stop_list"`
	VisibleRouteList        []string           `json:"visible_route_list"`
	Questions               []questionDocument `json:"questions"`
	Study                   *studyDocument     `json:"study,omitempty"`
	CreatedAt               string             `json:"created_at,omitempty"`
	UpdatedAt               string             `json:"updated_at,omitempty"`
}

type questionDocument struct {
	ID       *int64          `json:"id,omitempty"`
	Position *int64          `json:"position,omitempty"`
	Required bool            `json:"required"`
	Content  surveys.Content `json:"content"`
}

type studyDocument struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// readDocument loads a survey document from path, or stdin for "-".
func readDocument(stdin io.Reader, path string) (surveyDocument, error) {
	r := stdin
	if path != "-" {
		f, err := os.Open(path)
		if err != nil {
			return surveyDocument{}, err
		}
		defer func() { _ = f.Close() }()
		r = f
	}
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	var doc surveyDocument
	if err := dec.Decode(&doc); err != nil {
		return surveyDocument{}, fmt.Errorf("parse survey document: %w", err)
	}
	return doc, nil
}

// toDefinition converts a document to a validated Definition. Dates go
// through parseInstant with the region, so the explicit-offset rule and
// its timezone hint carry over from alert create.
func (doc surveyDocument) toDefinition(region regions.Region) (surveys.Definition, error) {
	def := surveys.Definition{
		Name: doc.Name, Available: true,
		ShowOnMap: doc.ShowOnMap, ShowOnStops: doc.ShowOnStops, AlwaysVisible: doc.AlwaysVisible,
		AllowsMultipleResponses: doc.AllowsMultipleResponses,
		VisibleStopList:         doc.VisibleStopList, VisibleRouteList: doc.VisibleRouteList,
	}
	if doc.Available != nil {
		def.Available = *doc.Available
	}
	if doc.StartDate != nil {
		t, err := parseInstant(*doc.StartDate, region)
		if err != nil {
			return surveys.Definition{}, fmt.Errorf("start_date: %w", err)
		}
		def.StartTime = &t
	}
	if doc.EndDate != nil {
		t, err := parseInstant(*doc.EndDate, region)
		if err != nil {
			return surveys.Definition{}, fmt.Errorf("end_date: %w", err)
		}
		def.EndTime = &t
	}
	for _, q := range doc.Questions {
		def.Questions = append(def.Questions, surveys.QuestionDefinition{Required: q.Required, Content: q.Content})
	}
	if err := def.Validate(); err != nil {
		return surveys.Definition{}, err
	}
	return def, nil
}

func documentFromSurvey(s surveys.Survey) surveyDocument {
	available := s.Available
	doc := surveyDocument{
		ID: &s.ID, Name: s.Name, Available: &available,
		ShowOnMap: s.ShowOnMap, ShowOnStops: s.ShowOnStops, AlwaysVisible: s.AlwaysVisible,
		AllowsMultipleResponses: s.AllowsMultipleResponses,
		VisibleStopList:         s.VisibleStopList, VisibleRouteList: s.VisibleRouteList,
		Questions: make([]questionDocument, 0, len(s.Questions)),
		Study:     &studyDocument{ID: s.Study.ID, Name: s.Study.Name, Description: s.Study.Description},
		CreatedAt: surveys.FormatTime(s.CreatedAt), UpdatedAt: surveys.FormatTime(s.UpdatedAt),
	}
	if s.StartTime != nil {
		v := surveys.FormatTime(*s.StartTime)
		doc.StartDate = &v
	}
	if s.EndTime != nil {
		v := surveys.FormatTime(*s.EndTime)
		doc.EndDate = &v
	}
	for _, q := range s.Questions {
		id, pos := q.ID, q.Position
		doc.Questions = append(doc.Questions, questionDocument{ID: &id, Position: &pos, Required: q.Required, Content: q.Content})
	}
	return doc
}

func parseSurveyIDArg(op string, args []string) (int64, error) {
	if len(args) == 0 {
		return 0, fmt.Errorf("%s requires a survey id", op)
	}
	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid survey id %q: %w", op, args[0], err)
	}
	return id, nil
}

// regionForStudy resolves the region a study belongs to, for parseInstant's
// explicit-offset check and timezone hint.
func regionForStudy(ctx context.Context, store *sqlite.Store, studyID int64) (regions.Region, error) {
	st, err := store.Surveys().GetStudy(ctx, studyID)
	if err != nil {
		return regions.Region{}, fmt.Errorf("study %d: %w", studyID, err)
	}
	reg, err := store.Regions().Get(ctx, st.RegionID)
	if err != nil {
		return regions.Region{}, fmt.Errorf("region %d: %w", st.RegionID, err)
	}
	return reg, nil
}

func surveyCreate(ctx context.Context, stdin io.Reader, stdout io.Writer, store *sqlite.Store, now time.Time, args []string) error {
	fs := flag.NewFlagSet("survey create", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	studyID := fs.Int64("study", 0, "study id (required)")
	file := fs.String("file", "", "survey document path, or - for stdin (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	seen := visitedFlags(fs)
	if !seen["study"] || !seen["file"] {
		return errors.New("survey create requires --study and --file")
	}
	reg, err := regionForStudy(ctx, store, *studyID)
	if err != nil {
		return fmt.Errorf("survey create: %w", err)
	}
	doc, err := readDocument(stdin, *file)
	if err != nil {
		return fmt.Errorf("survey create: %w", err)
	}
	def, err := doc.toDefinition(reg)
	if err != nil {
		return fmt.Errorf("survey create: %w", err)
	}
	s, err := store.Surveys().CreateSurvey(ctx, *studyID, def, now)
	if err != nil {
		return fmt.Errorf("survey create: %w", err)
	}
	fmt.Fprintf(stdout, "created survey %d\n", s.ID)
	return nil
}

func surveyList(ctx context.Context, stdout io.Writer, store *sqlite.Store, args []string) error {
	fs := flag.NewFlagSet("survey list", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	regionID := fs.Int64("region", 0, "region id (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !visitedFlags(fs)["region"] {
		return errors.New("survey list requires --region")
	}
	reg, err := store.Regions().Get(ctx, *regionID)
	if err != nil {
		return fmt.Errorf("survey list: %w", err)
	}
	list, err := store.Surveys().ListSurveys(ctx, *regionID)
	if err != nil {
		return fmt.Errorf("survey list: %w", err)
	}
	for _, s := range list {
		var n int64
		n, err = store.Surveys().CountResponses(ctx, s.ID)
		if err != nil {
			return fmt.Errorf("survey list: %w", err)
		}
		window := "always"
		if s.StartTime != nil && s.EndTime != nil {
			window = formatInZone(*s.StartTime, reg.Timezone) + " .. " + formatInZone(*s.EndTime, reg.Timezone)
		}
		fmt.Fprintf(stdout, "%d\tstudy=%d\tavailable=%t\tresponses=%d\twindow=%s\t%s\n",
			s.ID, s.StudyID, s.Available, n, window, s.Name)
	}
	return nil
}

func surveyShow(ctx context.Context, stdout io.Writer, store *sqlite.Store, args []string) error {
	id, err := parseSurveyIDArg("survey show", args)
	if err != nil {
		return err
	}
	s, err := store.Surveys().GetSurvey(ctx, id)
	if err != nil {
		return fmt.Errorf("survey show %d: %w", id, err)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err = enc.Encode(documentFromSurvey(s)); err != nil {
		return fmt.Errorf("survey show %d: %w", id, err)
	}
	_, err = stdout.Write(buf.Bytes())
	return err
}

func surveyEdit(ctx context.Context, stdin io.Reader, store *sqlite.Store, now time.Time, args []string) error {
	id, err := parseSurveyIDArg("survey edit", args)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("survey edit", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	file := fs.String("file", "", "survey document path, or - for stdin (required)")
	if err = fs.Parse(args[1:]); err != nil {
		return err
	}
	if !visitedFlags(fs)["file"] {
		return errors.New("survey edit requires --file")
	}
	current, err := store.Surveys().GetSurvey(ctx, id)
	if err != nil {
		return fmt.Errorf("survey edit %d: %w", id, err)
	}
	reg, err := regionForStudy(ctx, store, current.StudyID)
	if err != nil {
		return fmt.Errorf("survey edit %d: %w", id, err)
	}
	doc, err := readDocument(stdin, *file)
	if err != nil {
		return fmt.Errorf("survey edit %d: %w", id, err)
	}
	def, err := doc.toDefinition(reg)
	if err != nil {
		return fmt.Errorf("survey edit %d: %w", id, err)
	}
	if _, err = store.Surveys().UpdateSurvey(ctx, id, def, now); err != nil {
		if errors.Is(err, surveys.ErrQuestionsFrozen) {
			// The count is informational only: if it also fails, the message
			// still names the real problem (frozen questions) without
			// asserting a response count the code could not confirm --
			// never "0 responses" when the count itself is unknown.
			n, countErr := store.Surveys().CountResponses(ctx, id)
			if countErr != nil {
				return fmt.Errorf("survey edit %d: survey has responses; its questions are frozen (edit only name, dates, flags, and targeting)", id)
			}
			return fmt.Errorf("survey edit %d: survey has %d responses; its questions are frozen (edit only name, dates, flags, and targeting)", id, n)
		}
		return fmt.Errorf("survey edit %d: %w", id, err)
	}
	return nil
}

func surveyDelete(ctx context.Context, store *sqlite.Store, args []string) error {
	id, err := parseSurveyIDArg("survey delete", args)
	if err != nil {
		return err
	}
	if err := store.Surveys().DeleteSurvey(ctx, id); err != nil {
		if errors.Is(err, surveys.ErrHasResponses) {
			// The count is informational only: if it also fails, the
			// message still names the real problem (responses exist) without
			// asserting a count the code could not confirm -- never "0
			// response(s)" when the count itself is unknown.
			n, countErr := store.Surveys().CountResponses(ctx, id)
			if countErr != nil {
				return fmt.Errorf("survey delete %d: survey has responses; responses are retained indefinitely, so the survey cannot be deleted", id)
			}
			return fmt.Errorf("survey delete %d: survey has %d response(s); responses are retained indefinitely, so the survey cannot be deleted", id, n)
		}
		return fmt.Errorf("survey delete %d: %w", id, err)
	}
	return nil
}

func surveyResponses(context.Context, io.Writer, *sqlite.Store, []string) error {
	return errors.New("survey responses: not implemented") // Task 10
}
