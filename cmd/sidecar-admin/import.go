package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/OneBusAway/sidecar/internal/export"
	"github.com/OneBusAway/sidecar/internal/store/sqlite"
)

// runImport loads an export document (internal/export; produced by
// OBACloud's `rake sidecar:export`) into this database. Rows that already
// exist are skipped, so the same command applied to a later export of the
// same region imports only what is new -- the "final delta" step of a
// region cutover (README, Migrating a region from OBACloud).
func runImport(ctx context.Context, stdout io.Writer, store *sqlite.Store, now time.Time, args []string) error {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	file := fs.String("file", "", "path to the export document (required)")
	dryRun := fs.Bool("dry-run", false, "validate the document and report what would be imported, without writing")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *file == "" {
		return errors.New("import requires --file")
	}
	f, err := os.Open(*file)
	if err != nil {
		return err
	}
	defer f.Close()
	var doc export.Document
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	if decodeErr := dec.Decode(&doc); decodeErr != nil {
		return fmt.Errorf("import: %s: %w", *file, decodeErr)
	}
	if *dryRun {
		// The same pre-checks the real import runs, so a clean dry run
		// means the import will not stop on validation (it can still
		// stop on a conflict, which needs the database).
		if validateErr := store.ValidateImport(ctx, &doc, now); validateErr != nil {
			return validateErr
		}
		fmt.Fprintf(stdout, "dry run: %s is a valid %s document for region %d: %d alerts, %d studies, %d survey responses, %d push registrations, %d ghost bus reports\n",
			*file, doc.Format, doc.RegionID, len(doc.Alerts), len(doc.Studies), len(doc.SurveyResponses), len(doc.PushRegistrations), len(doc.GhostBusReports))
		return nil
	}
	sum, err := store.Import(ctx, &doc, now)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "imported region %d from %s\n", doc.RegionID, *file)
	for _, line := range sum.Lines() {
		fmt.Fprintln(stdout, line)
	}
	return nil
}
