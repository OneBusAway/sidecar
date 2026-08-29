-- name: CreateAlarm :one
INSERT INTO alarms (
  region_id, token, api_version, user_push_id, operating_system, apns_sandbox,
  stop_id, trip_id, service_date, vehicle_id, stop_sequence,
  seconds_before, message, created_at, updated_at
) VALUES (
  @region_id, @token, @api_version, @user_push_id, @operating_system, @apns_sandbox,
  @stop_id, @trip_id, @service_date, @vehicle_id, @stop_sequence,
  @seconds_before, @message, @now, @now
)
RETURNING *;

-- name: FindV1Alarm :one
SELECT * FROM alarms
WHERE api_version = 1 AND region_id = @region_id AND user_push_id = @user_push_id
  AND trip_id = @trip_id AND stop_id = @stop_id AND service_date = @service_date;

-- name: DeleteAlarmByToken :execrows
DELETE FROM alarms WHERE region_id = @region_id AND token = @token;

-- name: DeleteAlarmByID :execrows
DELETE FROM alarms WHERE id = @id;

-- name: ListAlarms :many
SELECT * FROM alarms ORDER BY id;

-- ListDueAlarms is deliberately unordered and unindexed: the sweep fans
-- out concurrently, most rows match (every fresh alarm is due), and the
-- 3-strike reaper bounds the table, so a plain scan is the cheap path.

-- name: ListDueAlarms :many
SELECT * FROM alarms WHERE check_after <= @now;

-- name: DeferAlarm :exec
UPDATE alarms SET check_after = @check_after WHERE id = @id;

-- RecordAlarmFailure and ResetAlarmFailures use SQLite's own unixepoch(),
-- not a bound @now, because alarms.Repository's RecordFailure/ResetFailures
-- methods take no now time.Time parameter (Task 7's interface, fixed) -- the
-- scheduler calls these mid-sweep with no caller-supplied clock to thread
-- through. Reading the engine's own clock here keeps the repo-wide
-- "no time.Now() outside cmd/" rule intact: it is SQLite's clock, not Go's.

-- name: RecordAlarmFailure :one
UPDATE alarms SET failure_count = failure_count + 1, updated_at = unixepoch()
WHERE id = @id
RETURNING failure_count;

-- name: ResetAlarmFailures :exec
UPDATE alarms SET failure_count = 0, updated_at = unixepoch()
WHERE id = @id AND failure_count <> 0;

-- name: ListAlarmsByRegion :many
SELECT * FROM alarms WHERE region_id = @region_id ORDER BY id;

-- name: GetAlarmInRegion :one
SELECT * FROM alarms WHERE id = @id AND region_id = @region_id;
