package main

import (
	"context"
	"strings"
	"testing"

	"github.com/OneBusAway/sidecar/internal/store/sqlite"
)

// TestSequenceCommands: show lists every table; bump raises them to the
// floor and reports before -> after; a re-run is a no-op; the flag is
// required and must be positive.
func TestSequenceCommands(t *testing.T) {
	t.Parallel()
	dbPath, store := newDB(t)

	out, _, err := cli(t, dbPath, "sequence", "show")
	if err != nil {
		t.Fatalf("sequence show: %v", err)
	}
	for _, name := range sqlite.SequenceTables {
		if !strings.Contains(out, name+"\t0\n") {
			t.Errorf("show output lacks %q at 0:\n%s", name, out)
		}
	}

	out, _, err = cli(t, dbPath, "sequence", "bump", "--min", "1000000")
	if err != nil {
		t.Fatalf("sequence bump: %v", err)
	}
	for _, name := range sqlite.SequenceTables {
		if !strings.Contains(out, name+": 0 -> 1000000") {
			t.Errorf("bump output lacks %q:\n%s", name, out)
		}
	}
	seqs, err := store.Sequences(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range sqlite.SequenceTables {
		if seqs[name] != 1000000 {
			t.Errorf("%s = %d after bump", name, seqs[name])
		}
	}

	out, _, err = cli(t, dbPath, "sequence", "bump", "--min", "10")
	if err != nil || !strings.Contains(out, "alerts: 1000000 -> 1000000") {
		t.Errorf("lower bump: %v\n%s", err, out)
	}

	cliErrContains(t, dbPath, "requires --min", "sequence", "bump")
	cliErrContains(t, dbPath, "must be positive", "sequence", "bump", "--min", "0")
	cliErrContains(t, dbPath, "subcommand", "sequence")
	cliErrContains(t, dbPath, "unknown sequence subcommand", "sequence", "reset")
}
