package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// SequenceTables are the AUTOINCREMENT tables whose ids an export document
// carries verbatim (internal/export): alerts, studies, surveys, and
// survey questions. After one region migrates, this database mints those
// ids from that region's maximum upward while OBACloud keeps minting for
// un-migrated regions from its own sequences, so a later region's import
// would collide with content authored here in between. BumpSequences moves
// every one of these above any id OBACloud could still hand out (migration
// design spec section 2.6).
var SequenceTables = []string{"alerts", "studies", "surveys", "survey_questions"}

// Sequences reports sqlite_sequence for every SequenceTables entry: the
// last id minted, or 0 for a table that has never had a row (SQLite creates
// the sqlite_sequence row on first insert).
//
// sqlite_sequence is not in the sqlc schema -- it is SQLite's own table --
// so this is one of the few hand-written statements in the adapter.
func (s *Store) Sequences(ctx context.Context) (map[string]int64, error) {
	out := make(map[string]int64, len(SequenceTables))
	for _, name := range SequenceTables {
		seq, _, err := readSequence(ctx, s.db, name)
		if err != nil {
			return nil, err
		}
		out[name] = seq
	}
	return out, nil
}

// BumpSequences raises every SequenceTables sequence to at least min, in
// one write transaction, and returns the value each had before. A sequence
// already at or above min is left alone, so the call is idempotent and
// can never move an id backwards.
func (s *Store) BumpSequences(ctx context.Context, min int64) (map[string]int64, error) {
	if min <= 0 {
		return nil, fmt.Errorf("sqlite: bump sequences: min must be positive, got %d", min)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("sqlite: bump sequences: begin tx: %w", err)
	}
	//nolint:errcheck // rollback after a successful commit is a documented no-op; the error is expected and safe to ignore
	defer func() { _ = tx.Rollback() }()

	before := make(map[string]int64, len(SequenceTables))
	for _, name := range SequenceTables {
		seq, found, err := readSequence(ctx, tx, name)
		if err != nil {
			return nil, err
		}
		before[name] = seq
		switch {
		case !found:
			// No row yet: sqlite_sequence has no unique index, so this is an
			// insert keyed on the row's absence, not an upsert -- a second
			// row for the same name would make SQLite's lookup ambiguous.
			if _, err := tx.ExecContext(ctx, `INSERT INTO sqlite_sequence (name, seq) VALUES (?, ?)`, name, min); err != nil {
				return nil, fmt.Errorf("sqlite: bump sequences: insert %s: %w", name, err)
			}
		case seq < min:
			if _, err := tx.ExecContext(ctx, `UPDATE sqlite_sequence SET seq = ? WHERE name = ?`, min, name); err != nil {
				return nil, fmt.Errorf("sqlite: bump sequences: update %s: %w", name, err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("sqlite: bump sequences: commit: %w", err)
	}
	return before, nil
}

// querier is the one method readSequence needs from *sql.DB and *sql.Tx.
type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// readSequence returns the sqlite_sequence value for name. found is false
// when the table has never had a row (SQLite creates the sqlite_sequence
// row on first insert); seq is 0 then.
func readSequence(ctx context.Context, q querier, name string) (seq int64, found bool, err error) {
	err = q.QueryRowContext(ctx, `SELECT seq FROM sqlite_sequence WHERE name = ?`, name).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("sqlite: read sequence %s: %w", name, err)
	}
	return seq, true, nil
}
