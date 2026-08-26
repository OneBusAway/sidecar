package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/OneBusAway/sidecar/internal/alertpush"
	"github.com/OneBusAway/sidecar/internal/store/sqlite/gen"
)

// alertPushRepo implements alertpush.Repository. Error strings never embed
// tokens (alert_push_failures stores only their SHA-256; errors from that
// table name only the push id).
type alertPushRepo struct {
	db *sql.DB
	q  *gen.Queries
}

// alertPushFromRow maps one generated row onto the domain type.
// FailureReasons is left nil: only Get and ListByAlert pay for the rollup.
func alertPushFromRow(row gen.AlertPush) (alertpush.Push, error) {
	var msgs alertpush.Messages
	if err := json.Unmarshal([]byte(row.Messages), &msgs); err != nil {
		return alertpush.Push{}, fmt.Errorf("sqlite: alert push %d: decode messages: %w", row.ID, err)
	}
	return alertpush.Push{
		ID: row.ID, AlertID: row.AlertID, RegionID: row.RegionID,
		Audience: alertpush.Audience(row.Audience), Status: alertpush.Status(row.Status),
		Messages: msgs, BatchCursor: row.BatchCursor, DeviceCount: row.DeviceCount,
		SubmittedCount: row.SubmittedCount, FailedCount: row.FailedCount, Attempts: row.Attempts,
		LastError: row.LastError, StartedAt: nullUnixToTime(row.StartedAt),
		CompletedAt: nullUnixToTime(row.CompletedAt),
		CreatedAt:   unixToTime(row.CreatedAt), UpdatedAt: unixToTime(row.UpdatedAt),
	}, nil
}

// loadAlertPushFailureReasons attaches the grouped failure rollup one push
// at a time, so both Get and ListByAlert report the same shape.
func loadAlertPushFailureReasons(ctx context.Context, q *gen.Queries, id int64) ([]alertpush.FailureReason, error) {
	rows, err := q.ListAlertPushFailureReasons(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("sqlite: alert push %d: failure reasons: %w", id, err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	out := make([]alertpush.FailureReason, len(rows))
	for i, row := range rows {
		out[i] = alertpush.FailureReason{Reason: row.Reason, Count: row.N}
	}
	return out, nil
}

func (r *alertPushRepo) Create(ctx context.Context, in alertpush.NewPush, now time.Time) (alertpush.Push, error) {
	msgs, err := json.Marshal(in.Messages)
	if err != nil {
		return alertpush.Push{}, fmt.Errorf("sqlite: encode alert push messages: %w", err)
	}
	row, err := r.q.CreateAlertPush(ctx, gen.CreateAlertPushParams{
		AlertID: in.AlertID, RegionID: in.RegionID, Audience: string(in.Audience),
		Messages: string(msgs), CreatedAt: now.Unix(), UpdatedAt: now.Unix(),
	})
	if err != nil {
		// The partial unique index alert_pushes_inflight_idx is what makes
		// "one in-flight push per alert" a database invariant rather than a
		// check-then-insert race (design spec section 2.2).
		if isUniqueViolation(err, "alert_pushes.alert_id") {
			return alertpush.Push{}, fmt.Errorf("sqlite: create alert push for alert %d: %w", in.AlertID, alertpush.ErrInFlight)
		}
		return alertpush.Push{}, fmt.Errorf("sqlite: create alert push for alert %d: %w", in.AlertID, err)
	}
	return alertPushFromRow(row)
}

func (r *alertPushRepo) Get(ctx context.Context, id int64) (alertpush.Push, error) {
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return alertpush.Push{}, fmt.Errorf("sqlite: get alert push %d: begin tx: %w", id, err)
	}
	//nolint:errcheck // rollback after a successful commit is a documented no-op; the error is expected and safe to ignore
	defer func() { _ = tx.Rollback() }()
	q := r.q.WithTx(tx)

	row, err := q.GetAlertPush(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return alertpush.Push{}, fmt.Errorf("sqlite: get alert push %d: %w", id, alertpush.ErrNotFound)
		}
		return alertpush.Push{}, fmt.Errorf("sqlite: get alert push %d: %w", id, err)
	}
	push, err := alertPushFromRow(row)
	if err != nil {
		return alertpush.Push{}, err
	}
	if push.FailureReasons, err = loadAlertPushFailureReasons(ctx, q, id); err != nil {
		return alertpush.Push{}, err
	}
	if err := tx.Commit(); err != nil {
		return alertpush.Push{}, fmt.Errorf("sqlite: get alert push %d: commit: %w", id, err)
	}
	return push, nil
}

func (r *alertPushRepo) ListByAlert(ctx context.Context, alertID int64) ([]alertpush.Push, error) {
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("sqlite: list alert pushes for alert %d: begin tx: %w", alertID, err)
	}
	//nolint:errcheck // rollback after a successful commit is a documented no-op; the error is expected and safe to ignore
	defer func() { _ = tx.Rollback() }()
	q := r.q.WithTx(tx)

	rows, err := q.ListAlertPushesByAlert(ctx, alertID)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list alert pushes for alert %d: %w", alertID, err)
	}
	out := make([]alertpush.Push, 0, len(rows))
	for _, row := range rows {
		push, convErr := alertPushFromRow(row)
		if convErr != nil {
			return nil, convErr
		}
		// The admin push history renders each row's failure breakdown, so
		// the rollup is attached per row here too (design spec section 2.9).
		if push.FailureReasons, err = loadAlertPushFailureReasons(ctx, q, push.ID); err != nil {
			return nil, err
		}
		out = append(out, push)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("sqlite: list alert pushes for alert %d: commit: %w", alertID, err)
	}
	return out, nil
}

func (r *alertPushRepo) InFlightForAlert(ctx context.Context, alertID int64) (bool, error) {
	n, err := r.q.CountInFlightAlertPushes(ctx, alertID)
	if err != nil {
		return false, fmt.Errorf("sqlite: count in-flight alert pushes for alert %d: %w", alertID, err)
	}
	return n > 0, nil
}

func (r *alertPushRepo) Claim(ctx context.Context, now, stuckBefore time.Time) ([]alertpush.Push, error) {
	rows, err := r.q.ClaimAlertPushes(ctx, gen.ClaimAlertPushesParams{
		StartedNow:  sql.NullInt64{Int64: now.Unix(), Valid: true},
		Now:         now.Unix(),
		StuckBefore: stuckBefore.Unix(),
	})
	if err != nil {
		return nil, fmt.Errorf("sqlite: claim alert pushes: %w", err)
	}
	out := make([]alertpush.Push, 0, len(rows))
	for _, row := range rows {
		push, convErr := alertPushFromRow(row)
		if convErr != nil {
			return nil, convErr
		}
		out = append(out, push)
	}
	// UPDATE ... RETURNING makes no ordering promise; the interface does.
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (r *alertPushRepo) SetDeviceCount(ctx context.Context, id, n int64, now time.Time) error {
	rows, err := r.q.SetAlertPushDeviceCount(ctx, gen.SetAlertPushDeviceCountParams{
		DeviceCount: n, Now: now.Unix(), ID: id,
	})
	if err != nil {
		return fmt.Errorf("sqlite: set alert push %d device count: %w", id, err)
	}
	if rows == 0 {
		return fmt.Errorf("sqlite: set alert push %d device count: %w", id, alertpush.ErrNotFound)
	}
	return nil
}

func (r *alertPushRepo) AdvanceCursor(ctx context.Context, id, prevCursor, newCursor, submitted int64, now time.Time) (bool, error) {
	rows, err := r.q.AdvanceAlertPushCursor(ctx, gen.AdvanceAlertPushCursorParams{
		NewCursor: newCursor, Submitted: submitted, Now: now.Unix(),
		ID: id, PrevCursor: prevCursor,
	})
	if err != nil {
		return false, fmt.Errorf("sqlite: advance alert push %d cursor: %w", id, err)
	}
	// Zero rows is not an error: another worker moved the cursor, or the
	// push left sending (canceled). The caller stops (design spec section 2.6).
	return rows == 1, nil
}

func (r *alertPushRepo) RecordFailure(ctx context.Context, id int64, token, reason string, now time.Time) (bool, error) {
	sum := sha256.Sum256([]byte(token))
	hash := hex.EncodeToString(sum[:])

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("sqlite: record alert push %d failure: begin tx: %w", id, err)
	}
	//nolint:errcheck // rollback after a successful commit is a documented no-op; the error is expected and safe to ignore
	defer func() { _ = tx.Rollback() }()
	q := r.q.WithTx(tx)

	// INSERT OR IGNORE absorbs the (push_id, token_sha256) replay, but not a
	// foreign key violation: an unknown push id still errors here.
	inserted, err := q.InsertAlertPushFailure(ctx, gen.InsertAlertPushFailureParams{
		PushID: id, TokenSha256: hash, Reason: reason, CreatedAt: now.Unix(),
	})
	if err != nil {
		return false, fmt.Errorf("sqlite: record alert push %d failure: %w", id, err)
	}
	if inserted == 0 {
		// A replayed feedback row: already counted, so failed_count stands.
		return false, nil
	}
	// Note the missing `now`: the counter moves, updated_at does not. See the
	// query's comment -- it is the dispatcher's stuck clock, not this path's.
	if _, err := q.IncrementAlertPushFailed(ctx, id); err != nil {
		return false, fmt.Errorf("sqlite: record alert push %d failure: increment: %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("sqlite: record alert push %d failure: commit: %w", id, err)
	}
	return true, nil
}

func (r *alertPushRepo) RecordAttempt(ctx context.Context, id int64, errMsg string, now time.Time) (int64, error) {
	attempts, err := r.q.RecordAlertPushAttempt(ctx, gen.RecordAlertPushAttemptParams{
		LastError: errMsg, Now: now.Unix(), ID: id,
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("sqlite: record alert push %d attempt: %w", id, alertpush.ErrNotFound)
		}
		return 0, fmt.Errorf("sqlite: record alert push %d attempt: %w", id, err)
	}
	return attempts, nil
}

func (r *alertPushRepo) MarkCompleted(ctx context.Context, id int64, status alertpush.Status, lastError string, now time.Time) (bool, error) {
	rows, err := r.q.CompleteAlertPush(ctx, gen.CompleteAlertPushParams{
		Status: string(status), LastError: lastError,
		CompletedAt: sql.NullInt64{Int64: now.Unix(), Valid: true},
		Now:         now.Unix(), ID: id,
		// The same value the SET writes, re-bound for the query's
		// terminal-status allowlist (sqlc types a parameter compared only
		// against literals as interface{}).
		TerminalStatus: string(status),
	})
	if err != nil {
		return false, fmt.Errorf("sqlite: complete alert push %d: %w", id, err)
	}
	// Zero rows means the push was not sending -- an operator canceled it
	// mid-flight, or another worker already finished it -- or the caller
	// asked for a non-terminal status, which the query refuses. Not an error.
	return rows == 1, nil
}

func (r *alertPushRepo) Cancel(ctx context.Context, id int64, now time.Time) error {
	rows, err := r.q.CancelAlertPush(ctx, gen.CancelAlertPushParams{
		CompletedAt: sql.NullInt64{Int64: now.Unix(), Valid: true},
		Now:         now.Unix(), ID: id,
	})
	if err != nil {
		return fmt.Errorf("sqlite: cancel alert push %d: %w", id, err)
	}
	if rows == 1 {
		return nil
	}
	// Nothing moved: either the push does not exist or it is already
	// terminal. One extra read tells the caller which (404 vs 409).
	if _, err := r.q.GetAlertPush(ctx, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("sqlite: cancel alert push %d: %w", id, alertpush.ErrNotFound)
		}
		return fmt.Errorf("sqlite: cancel alert push %d: %w", id, err)
	}
	return fmt.Errorf("sqlite: cancel alert push %d: %w", id, alertpush.ErrTerminal)
}
