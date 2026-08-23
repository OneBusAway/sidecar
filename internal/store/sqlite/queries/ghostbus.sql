-- name: CreateGhostBusReport :one
INSERT INTO ghost_bus_reports (
  region_id, public_identifier, user_identifier, trip_identifier, service_date,
  route_identifier, stop_identifier, vehicle_identifier, stop_sequence,
  predicted, schedule_deviation_minutes, wait_duration_minutes, comment,
  user_latitude, user_longitude,
  scheduled_arrival_at, predicted_arrival_at, prediction_last_updated_at,
  created_at, updated_at
) VALUES (
  @region_id, @public_identifier, @user_identifier, @trip_identifier, @service_date,
  @route_identifier, @stop_identifier, @vehicle_identifier, @stop_sequence,
  @predicted, @schedule_deviation_minutes, @wait_duration_minutes, @comment,
  @user_latitude, @user_longitude,
  @scheduled_arrival_at, @predicted_arrival_at, @prediction_last_updated_at,
  @now, @now
)
RETURNING *;

-- name: ListPendingSnapshotReports :many
SELECT * FROM ghost_bus_reports
WHERE snapshot_status = 'pending' AND snapshot_attempts < 3
ORDER BY id
LIMIT @max_rows;

-- name: MarkGhostBusSnapshotCaptured :exec
UPDATE ghost_bus_reports
SET snapshot_json = @snapshot_json, snapshot_status = 'captured',
    snapshot_captured_at = @now, updated_at = @now
WHERE id = @id;

-- name: MarkGhostBusSnapshotUnavailable :exec
UPDATE ghost_bus_reports
SET snapshot_status = 'unavailable', updated_at = @now
WHERE id = @id;

-- The cap check and the status flip are one UPDATE deliberately: a crash
-- between "increment" and "mark unavailable" must not leave a row that is
-- both at the cap and still pending (the poll predicate would skip it, but
-- nothing would ever resolve it either).
-- name: RecordGhostBusSnapshotFailure :one
UPDATE ghost_bus_reports
SET snapshot_attempts = snapshot_attempts + 1,
    snapshot_status = CASE WHEN snapshot_attempts + 1 >= 3
                           THEN 'unavailable' ELSE snapshot_status END,
    updated_at = @now
WHERE id = @id
RETURNING snapshot_attempts;

-- name: ListGhostBusReportsForExport :many
SELECT * FROM ghost_bus_reports
WHERE region_id = @region_id AND created_at >= @since
ORDER BY id;
