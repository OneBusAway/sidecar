package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/OneBusAway/sidecar/internal/ghostbus"
	"github.com/OneBusAway/sidecar/internal/store/sqlite"
)

// ghostBusCmd dispatches the ghostbus subcommands (export only, this
// slice). The CSV is the agency-facing read surface -- there is
// deliberately no rider-facing read API (spec §8).
const ghostBusExportUsage = "usage: ghostbus export --region N [--since RFC3339]"

func ghostBusCmd(ctx context.Context, stdout io.Writer, store *sqlite.Store, args []string) error {
	if len(args) == 0 || args[0] != "export" {
		return errors.New(ghostBusExportUsage)
	}
	fs := flag.NewFlagSet("ghostbus export", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	regionID := fs.Int64("region", 0, "region id to export")
	since := fs.String("since", "", "only reports created at or after this RFC 3339 instant")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	// flag.Parse stops at the first non-flag argument, so trailing
	// positionals would otherwise be silently ignored -- and a typo like a
	// misspelled flag would export anyway.
	if fs.NArg() != 0 {
		return errors.New(ghostBusExportUsage)
	}
	if *regionID == 0 {
		return errors.New(ghostBusExportUsage)
	}
	region, err := store.Regions().Get(ctx, *regionID)
	if err != nil {
		return fmt.Errorf("region %d: %w", *regionID, err)
	}
	var sinceUnix int64
	if *since != "" {
		parsed, parseErr := time.Parse(time.RFC3339, *since)
		if parseErr != nil {
			return fmt.Errorf("--since must be RFC 3339 with an explicit UTC offset: %w", parseErr)
		}
		sinceUnix = parsed.Unix()
	}
	reports, err := store.GhostBus().ListForExport(ctx, *regionID, sinceUnix)
	if err != nil {
		return fmt.Errorf("list reports: %w", err)
	}
	return ghostbus.WriteReportsCSV(stdout, region, reports)
}
