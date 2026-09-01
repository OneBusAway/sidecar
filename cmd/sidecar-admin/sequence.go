package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/OneBusAway/sidecar/internal/store/sqlite"
)

// runSequence dispatches `sidecar-admin sequence`'s subcommands: the
// id-sequence headroom tooling for migrating regions from OBACloud
// (migration design spec section 2.6; README, Migrating a region from
// OBACloud).
func runSequence(ctx context.Context, stdout io.Writer, store *sqlite.Store, args []string) error {
	if len(args) == 0 {
		return errors.New("sequence requires a subcommand: show, bump")
	}
	cmd, cmdArgs := args[0], args[1:]
	switch cmd {
	case "show":
		return sequenceShow(ctx, stdout, store, cmdArgs)
	case "bump":
		return sequenceBump(ctx, stdout, store, cmdArgs)
	default:
		return fmt.Errorf("unknown sequence subcommand %q; expected show or bump", cmd)
	}
}

// sequenceShow prints one "table<TAB>seq" line per id sequence the import
// preserves, in sqlite.SequenceTables order so the output is stable.
func sequenceShow(ctx context.Context, stdout io.Writer, store *sqlite.Store, args []string) error {
	fs := flag.NewFlagSet("sequence show", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil {
		return err
	}
	seqs, err := store.Sequences(ctx)
	if err != nil {
		return fmt.Errorf("sequence show: %w", err)
	}
	for _, name := range sqlite.SequenceTables {
		fmt.Fprintf(stdout, "%s\t%d\n", name, seqs[name])
	}
	return nil
}

// sequenceBump raises every preserved id sequence to at least --min and
// prints before -> after per table. Re-running with the same or a lower
// floor changes nothing, so the cutover runbook can call it unconditionally.
func sequenceBump(ctx context.Context, stdout io.Writer, store *sqlite.Store, args []string) error {
	fs := flag.NewFlagSet("sequence bump", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	min := fs.Int64("min", 0, "floor for every sequence (required; the runbook uses 1000000)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !visitedFlags(fs)["min"] {
		return errors.New("sequence bump requires --min")
	}
	if *min <= 0 {
		return fmt.Errorf("sequence bump: --min must be positive, got %d", *min)
	}
	before, err := store.BumpSequences(ctx, *min)
	if err != nil {
		return fmt.Errorf("sequence bump: %w", err)
	}
	after, err := store.Sequences(ctx)
	if err != nil {
		return fmt.Errorf("sequence bump: %w", err)
	}
	for _, name := range sqlite.SequenceTables {
		fmt.Fprintf(stdout, "%s: %d -> %d\n", name, before[name], after[name])
	}
	return nil
}
