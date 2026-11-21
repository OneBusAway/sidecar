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

// maxImportBytes caps the export document the CLI will read. A region's
// export is mostly rider rows of a few hundred bytes each, so even a large
// region with tens of thousands of survey responses and ghost bus reports
// stays in the tens of MB; 256 MiB leaves an order of magnitude of room
// while still refusing a runaway or hostile input before it exhausts the
// host's memory.
const maxImportBytes = 256 << 20

// errDocumentTooLarge is what cappedReader returns past its limit.
var errDocumentTooLarge = errors.New("document is too large")

// cappedReader reads at most n bytes and then fails with
// errDocumentTooLarge rather than a plain EOF, so an oversized document is
// reported as such and not as truncated JSON.
type cappedReader struct {
	r io.Reader
	n int64
}

func (c *cappedReader) Read(p []byte) (int, error) {
	if c.n <= 0 {
		// One more byte would exceed the cap: probe for it.
		var one [1]byte
		n, err := c.r.Read(one[:])
		if n > 0 {
			return 0, errDocumentTooLarge
		}
		if err == nil {
			err = io.EOF // a zero-byte read with no error: treat as the end
		}
		return 0, err
	}
	if int64(len(p)) > c.n {
		p = p[:c.n]
	}
	n, err := c.r.Read(p)
	c.n -= int64(n)
	return n, err
}

// decodeDocument reads exactly one export document of at most limit bytes.
func decodeDocument(r io.Reader, source string, limit int64) (export.Document, error) {
	var doc export.Document
	dec := json.NewDecoder(&cappedReader{r: r, n: limit})
	dec.DisallowUnknownFields()
	if decodeErr := dec.Decode(&doc); decodeErr != nil {
		if errors.Is(decodeErr, errDocumentTooLarge) {
			return doc, fmt.Errorf("import: %s: document is larger than %d bytes", source, limit)
		}
		return doc, fmt.Errorf("import: %s: %w", source, decodeErr)
	}
	// One document per input: a second JSON value would otherwise be read
	// past silently and never imported.
	if _, trailingErr := dec.Token(); !errors.Is(trailingErr, io.EOF) {
		if errors.Is(trailingErr, errDocumentTooLarge) {
			return doc, fmt.Errorf("import: %s: document is larger than %d bytes", source, limit)
		}
		return doc, fmt.Errorf("import: %s: unexpected content after the document", source)
	}
	return doc, nil
}

// runImport loads an export document (internal/export; produced by
// OBACloud's `rake sidecar:export`) into this database. Rows that already
// exist are skipped, so the same command applied to a later export of the
// same region imports only what is new -- the "final delta" step of a
// region cutover (README, Migrating a region from OBACloud). `--file -`
// reads the document from stdin, so the cutover can pipe it over `render
// ssh` with no file transfer step (migration design spec section 2.5).
func runImport(ctx context.Context, stdin io.Reader, stdout io.Writer, store *sqlite.Store, now time.Time, args []string) error {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	file := fs.String("file", "", "path to the export document, or - for stdin (required)")
	dryRun := fs.Bool("dry-run", false, "validate the document and report what would be imported, without writing")
	idFloor := fs.Int64("id-floor", export.DefaultIDFloor, "refuse ids at or above this floor; match `sequence bump --min`")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *file == "" {
		return errors.New("import requires --file")
	}
	if *idFloor <= 0 {
		return fmt.Errorf("import: --id-floor must be positive, got %d", *idFloor)
	}
	r := stdin
	source := "stdin"
	if *file != "-" {
		f, err := os.Open(*file)
		if err != nil {
			return err
		}
		defer f.Close()
		r = f
		source = *file
	}
	doc, err := decodeDocument(r, source, maxImportBytes)
	if err != nil {
		return err
	}
	doc.IDFloor = *idFloor
	if *dryRun {
		// The same pre-checks the real import runs, so a clean dry run
		// means the import will not stop on validation (it can still
		// stop on a conflict, which needs the database).
		if validateErr := store.ValidateImport(ctx, &doc, now); validateErr != nil {
			return validateErr
		}
		fmt.Fprintf(stdout, "dry run: %s is a valid %s document for region %d: %d alerts, %d studies, %d survey responses, %d push registrations, %d ghost bus reports\n",
			source, doc.Format, doc.RegionID, len(doc.Alerts), len(doc.Studies), len(doc.SurveyResponses), len(doc.PushRegistrations), len(doc.GhostBusReports))
		return nil
	}
	sum, err := store.Import(ctx, &doc, now)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "imported region %d from %s\n", doc.RegionID, source)
	for _, line := range sum.Lines() {
		fmt.Fprintln(stdout, line)
	}
	return nil
}
