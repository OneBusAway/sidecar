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

// readDocument loads a survey document (surveys.Document, design spec 2.13)
// from path, or stdin for "-". Unknown fields are rejected so a typo like
// "show_on_maps" cannot silently become a hidden survey.
func readDocument(stdin io.Reader, path string) (surveys.Document, error) {
	r := stdin
	if path != "-" {
		f, err := os.Open(path)
		if err != nil {
			return surveys.Document{}, err
		}
		defer func() { _ = f.Close() }()
		r = f
	}
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	var doc surveys.Document
	if err := dec.Decode(&doc); err != nil {
		return surveys.Document{}, fmt.Errorf("parse survey document: %w", err)
	}
	return doc, nil
}

// definitionFromDocument converts a document to a validated Definition via
// the shared codec (internal/surveys/codec.go), so the CLI and POST/PUT
// /surveys cannot drift on defaults. Dates go through parseInstant with the
// region, so the explicit-offset rule and its timezone hint carry over from
// alert create.
func definitionFromDocument(doc surveys.Document, region regions.Region) (surveys.Definition, error) {
	return surveys.DefinitionFromDocument(doc, func(s string) (time.Time, error) {
		return parseInstant(s, region)
	})
}

func parseSurveyIDArg(op string, args []string) (int64, error) { return parseIDArg(op, "survey", args) }

// wrapSurveyErr is wrapAlertErr for surveys: "survey show 7: survey not
// found" rather than "survey show 7: sqlite: get survey 7: not found".
func wrapSurveyErr(op string, id int64, err error) error {
	return wrapNotFound(op, id, err, surveys.ErrNotFound, "survey ")
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
	def, err := definitionFromDocument(doc, reg)
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
		return wrapSurveyErr("survey show", id, err)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err = enc.Encode(surveys.DocumentFromSurvey(s)); err != nil {
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
		return wrapSurveyErr("survey edit", id, err)
	}
	reg, err := regionForStudy(ctx, store, current.StudyID)
	if err != nil {
		return fmt.Errorf("survey edit %d: %w", id, err)
	}
	doc, err := readDocument(stdin, *file)
	if err != nil {
		return fmt.Errorf("survey edit %d: %w", id, err)
	}
	def, err := definitionFromDocument(doc, reg)
	if err != nil {
		return fmt.Errorf("survey edit %d: %w", id, err)
	}
	if _, err = store.Surveys().UpdateSurvey(ctx, id, def, now); err != nil {
		if errors.Is(err, surveys.ErrQuestionsFrozen) {
			return fmt.Errorf("survey edit %d: survey has %s; its questions are frozen (edit only name, dates, flags, and targeting)",
				id, responseCountPhrase(ctx, store, id))
		}
		return wrapSurveyErr("survey edit", id, err)
	}
	return nil
}

// responseCountPhrase renders "N response(s)" for a refusal message, or
// just "responses" when the count itself cannot be read: the message must
// name the real problem (responses exist) without asserting a number the
// code could not confirm -- never "0 responses".
func responseCountPhrase(ctx context.Context, store *sqlite.Store, id int64) string {
	n, err := store.Surveys().CountResponses(ctx, id)
	if err != nil {
		return "responses"
	}
	return fmt.Sprintf("%d response(s)", n)
}

func surveyDelete(ctx context.Context, store *sqlite.Store, args []string) error {
	id, err := parseSurveyIDArg("survey delete", args)
	if err != nil {
		return err
	}
	if err := store.Surveys().DeleteSurvey(ctx, id); err != nil {
		if errors.Is(err, surveys.ErrHasResponses) {
			return fmt.Errorf("survey delete %d: survey has %s; responses are retained indefinitely, so the survey cannot be deleted",
				id, responseCountPhrase(ctx, store, id))
		}
		return wrapSurveyErr("survey delete", id, err)
	}
	return nil
}

// surveyResponses writes the survey's responses as long-format CSV (one row
// per answer; see surveys.WriteResponsesCSV for the format itself, so the
// admin CSV route can share it).
func surveyResponses(ctx context.Context, stdout io.Writer, store *sqlite.Store, args []string) error {
	id, err := parseSurveyIDArg("survey responses", args)
	if err != nil {
		return err
	}
	s, err := store.Surveys().GetSurvey(ctx, id)
	if err != nil {
		return wrapSurveyErr("survey responses", id, err)
	}
	list, err := store.Surveys().ListResponses(ctx, id)
	if err != nil {
		return fmt.Errorf("survey responses %d: %w", id, err)
	}
	return surveys.WriteResponsesCSV(stdout, s, list)
}
