-- name: CreateAlert :one
INSERT INTO alerts (
  region_id, agency_id, header_text, description_text, url,
  cause, effect, severity_level, start_time, end_time,
  published, is_test, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
RETURNING *;

-- name: GetAlert :one
SELECT * FROM alerts WHERE id = ?;

-- name: UpdateAlert :one
UPDATE alerts SET
  agency_id = ?, header_text = ?, description_text = ?, url = ?,
  cause = ?, effect = ?, severity_level = ?, start_time = ?, end_time = ?,
  is_test = ?, updated_at = ?
WHERE id = ?
RETURNING *;

-- name: SetAlertPublished :one
-- RETURNING id turns a no-op update against a nonexistent id into
-- sql.ErrNoRows instead of a silent, unreported success -- see
-- alertRepo.SetPublished in store.go, which maps that into alerts.ErrNotFound.
UPDATE alerts SET published = ?, updated_at = ? WHERE id = ?
RETURNING id;

-- name: DeleteAlert :one
-- RETURNING id, for the same reason as SetAlertPublished above: deleting a
-- nonexistent id must be reported, not silently accepted.
DELETE FROM alerts WHERE id = ?
RETURNING id;

-- name: ListAlerts :many
-- Every region. Region id 0 (Tampa Bay) is a real region, so "all regions"
-- cannot be expressed as a region_id parameter value: it is a separate
-- query from ListAlertsByRegion rather than a sentinel.
SELECT * FROM alerts
ORDER BY start_time DESC, id DESC;

-- name: ListAlertsByRegion :many
SELECT * FROM alerts
WHERE region_id = ?
ORDER BY start_time DESC, id DESC;

-- name: FeedAlerts :many
-- The test predicate is (is_test = FALSE OR :include_test). Writing
-- is_test = :include_test instead would return ONLY test alerts when
-- ?test=1, hiding every real alert from an agency verifying delivery.
--
-- Every parameter is an explicit sqlc.arg so numbering stays uniform. A bare
-- `?` mixed with an explicitly-numbered sqlc.arg is numbered by SQLite
-- positionally after the highest explicit index, not by argument order:
-- that mismatch made the third Go argument (limit) silently target a
-- nonexistent fourth placeholder and every Feed call fail.
SELECT * FROM alerts
WHERE region_id = sqlc.arg(region_id)
  AND published = TRUE
  AND (is_test = FALSE OR CAST(sqlc.arg(include_test) AS BOOLEAN))
ORDER BY start_time DESC, id DESC
LIMIT sqlc.arg(limit);

-- name: FeedTranslations :many
-- The subquery repeats the feed predicate including ORDER BY and LIMIT so it
-- matches the same rows. Both statements run in one read transaction; without
-- that, a publish between them can shift the top-20 set and an alert in the
-- response silently loses its translations.
--
-- As in FeedAlerts, every parameter is an explicit sqlc.arg so none is left
-- to be numbered positionally.
SELECT * FROM alert_translations WHERE alert_id IN (
  SELECT id FROM alerts
  WHERE region_id = sqlc.arg(region_id)
    AND published = TRUE
    AND (is_test = FALSE OR CAST(sqlc.arg(include_test) AS BOOLEAN))
  ORDER BY start_time DESC, id DESC
  LIMIT sqlc.arg(limit)
);

-- name: ListAlertTranslations :many
SELECT * FROM alert_translations WHERE alert_id = ? ORDER BY language, field;

-- name: UpsertAlertTranslation :exec
INSERT INTO alert_translations (
  alert_id, language, field, text, source_sha256, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (alert_id, language, field) DO UPDATE SET
  text          = excluded.text,
  source_sha256 = excluded.source_sha256,
  updated_at    = excluded.updated_at;
