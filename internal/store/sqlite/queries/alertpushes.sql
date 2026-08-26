-- Comments in this file must stay ASCII-only. sqlc expands `*` and renumbers
-- sqlc.arg() by byte offset into each statement's text, and a multi-byte rune
-- anywhere in a preceding comment shifts those offsets, emitting garbage SQL.
-- Cite the design spec as "section N", not with the section sign.
--
-- sqlc 1.31.1 also does not extract a parameter written inside an IN (...)
-- list: `sqlc.arg(x) IN ('a', 'b')` compiles, diffs clean, and leaves the
-- literal text "sqlc.arg(x)" in the SQL handed to the driver, which then
-- fails at execution. Spell such an allowlist out as OR comparisons (see
-- CompleteAlertPush) -- the same class of silent bug as the ON CONFLICT one
-- documented at the top of pushregs.sql.

-- name: CreateAlertPush :one
INSERT INTO alert_pushes (
  alert_id, region_id, audience, status, messages, created_at, updated_at
) VALUES (
  sqlc.arg(alert_id), sqlc.arg(region_id), sqlc.arg(audience), 'queued',
  sqlc.arg(messages), sqlc.arg(created_at), sqlc.arg(updated_at)
)
RETURNING *;

-- name: GetAlertPush :one
SELECT * FROM alert_pushes WHERE id = sqlc.arg(id);

-- name: ListAlertPushesByAlert :many
SELECT * FROM alert_pushes WHERE alert_id = sqlc.arg(alert_id) ORDER BY id DESC;

-- name: CountInFlightAlertPushes :one
SELECT COUNT(*) FROM alert_pushes
WHERE alert_id = sqlc.arg(alert_id) AND status IN ('queued', 'sending');

-- name: ClaimAlertPushes :many
-- started_at is preserved on a reclaim so the record still shows when the
-- send first began. Two named parameters carry the same instant because
-- sqlc's SQLite engine binds each sqlc.arg occurrence separately.
UPDATE alert_pushes SET
  status     = 'sending',
  started_at = COALESCE(started_at, sqlc.arg(started_now)),
  updated_at = sqlc.arg(now)
WHERE status = 'queued'
   OR (status = 'sending' AND updated_at < sqlc.arg(stuck_before))
RETURNING *;

-- name: SetAlertPushDeviceCount :execrows
UPDATE alert_pushes SET device_count = sqlc.arg(device_count), updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id);

-- name: AdvanceAlertPushCursor :execrows
-- A committed page also clears the failure streak (design spec section 2.6).
UPDATE alert_pushes SET
  batch_cursor    = sqlc.arg(new_cursor),
  submitted_count = submitted_count + sqlc.arg(submitted),
  attempts        = 0,
  last_error      = '',
  updated_at      = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND batch_cursor = sqlc.arg(prev_cursor) AND status = 'sending';

-- name: InsertAlertPushFailure :execrows
-- Never rewrite this as ON CONFLICT DO UPDATE SET failed_count = ...: a
-- parameter on the right-hand side of that clause is the sqlc bug
-- documented at the top of pushregs.sql. The increment is a second
-- statement in the same transaction (alertPushRepo.RecordFailure).
INSERT OR IGNORE INTO alert_push_failures (push_id, token_sha256, reason, created_at)
VALUES (sqlc.arg(push_id), sqlc.arg(token_sha256), sqlc.arg(reason), sqlc.arg(created_at));

-- name: IncrementAlertPushFailed :execrows
UPDATE alert_pushes SET failed_count = failed_count + 1, updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id);

-- name: ListAlertPushFailureReasons :many
SELECT reason, COUNT(*) AS n FROM alert_push_failures
WHERE push_id = sqlc.arg(push_id)
GROUP BY reason ORDER BY n DESC, reason LIMIT 10;

-- name: RecordAlertPushAttempt :one
UPDATE alert_pushes SET attempts = attempts + 1, last_error = sqlc.arg(last_error), updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id)
RETURNING attempts;

-- name: CompleteAlertPush :execrows
-- Only a terminal status may be written here: the guard makes a caller bug
-- (MarkCompleted with 'queued' or 'sending') a no-op rather than a row that
-- is back in flight with completed_at stamped. The status value is named
-- twice because sqlc's SQLite engine binds each sqlc.arg occurrence
-- separately; the adapter passes the same value to both.
UPDATE alert_pushes SET
  status = sqlc.arg(status), last_error = sqlc.arg(last_error),
  completed_at = sqlc.arg(completed_at), updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND status = 'sending'
  AND (sqlc.arg(terminal_status) = 'sent'
       OR sqlc.arg(terminal_status) = 'failed'
       OR sqlc.arg(terminal_status) = 'canceled');

-- name: CancelAlertPush :execrows
UPDATE alert_pushes SET status = 'canceled', completed_at = sqlc.arg(completed_at), updated_at = sqlc.arg(now)
WHERE id = sqlc.arg(id) AND status IN ('queued', 'sending');
