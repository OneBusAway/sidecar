-- name: InsertLiveActivity :one
INSERT INTO live_activities (
  region_id, token, activity_id, push_token, apns_sandbox,
  stop_id, route_short_name, trip_headsign, trip_id, service_date, vehicle_id, stop_sequence,
  expires_at, created_at, updated_at
) VALUES (
  @region_id, @token, @activity_id, @push_token, @apns_sandbox,
  @stop_id, @route_short_name, @trip_headsign, @trip_id, @service_date, @vehicle_id, @stop_sequence,
  @expires_at, @now, @now
)
RETURNING *;

-- UpdateLiveActivityRegistration rewrites only the registration fields
-- (design spec section 2.1): token, expires_at, last_content_state,
-- last_pushed_at and consecutive_failures are deliberately untouched.
-- revision is bumped so an in-flight sweep's DeleteLiveActivityByID, which
-- compares on it, cannot remove the refreshed registration. It is a
-- re-registration counter, deliberately NOT a write counter: the sweep's own
-- writes (RecordLiveActivityPush/Failure, ResetLiveActivityFailures) must not
-- bump it, or the sweep could never delete the rows it just listed.

-- name: UpdateLiveActivityRegistration :one
UPDATE live_activities SET
  push_token = @push_token, apns_sandbox = @apns_sandbox,
  stop_id = @stop_id, route_short_name = @route_short_name, trip_headsign = @trip_headsign,
  trip_id = @trip_id, service_date = @service_date, vehicle_id = @vehicle_id, stop_sequence = @stop_sequence,
  revision = revision + 1, updated_at = @now
WHERE region_id = @region_id AND activity_id = @activity_id
RETURNING *;

-- name: DeleteLiveActivityByToken :execrows
DELETE FROM live_activities WHERE region_id = @region_id AND token = @token;

-- DeleteLiveActivityByID is a compare-and-delete: the row is removed only if
-- its revision still matches the one the caller listed.

-- name: DeleteLiveActivityByID :execrows
DELETE FROM live_activities WHERE id = @id AND revision = @revision;

-- name: DeleteLiveActivitiesByPushToken :execrows
DELETE FROM live_activities WHERE push_token = @push_token;

-- name: ListLiveActivities :many
SELECT * FROM live_activities ORDER BY id;

-- RecordLiveActivityFailure / ResetLiveActivityFailures stamp updated_at with
-- SQLite's own unixepoch(): the repository methods take no now (same
-- reasoning as queries/alarms.sql).

-- name: RecordLiveActivityFailure :one
UPDATE live_activities SET consecutive_failures = consecutive_failures + 1, updated_at = unixepoch()
WHERE id = @id
RETURNING consecutive_failures;

-- name: ResetLiveActivityFailures :exec
UPDATE live_activities SET consecutive_failures = 0, updated_at = unixepoch()
WHERE id = @id AND consecutive_failures <> 0;

-- name: RecordLiveActivityPush :exec
UPDATE live_activities SET last_content_state = @last_content_state, last_pushed_at = @pushed_at, updated_at = @pushed_at
WHERE id = @id;

-- LiveActivityUsage feeds the registration caps (SC-03): table-wide and
-- per-region row counts, distinct stops in the region, rows on this push
-- token and on this stop, and whether (region, activity_id) already exists
-- and on which stop. One round trip; the counts are a soft cap, so a race
-- between this read and the insert can overshoot by the concurrency.

-- name: LiveActivityUsage :one
SELECT
  (SELECT COUNT(*) FROM live_activities) AS total,
  (SELECT COUNT(*) FROM live_activities AS r WHERE r.region_id = sqlc.arg(region_id)) AS in_region,
  (SELECT COUNT(DISTINCT s.stop_id) FROM live_activities AS s WHERE s.region_id = sqlc.arg(region_id)) AS stops_in_region,
  (SELECT COUNT(*) FROM live_activities AS p WHERE p.push_token = sqlc.arg(push_token)) AS for_push_token,
  (SELECT COUNT(*) FROM live_activities AS o WHERE o.region_id = sqlc.arg(region_id) AND o.stop_id = sqlc.arg(stop_id)) AS on_stop,
  (SELECT COALESCE(MAX(e.stop_id), '') FROM live_activities AS e WHERE e.region_id = sqlc.arg(region_id) AND e.activity_id = sqlc.arg(activity_id)) AS existing_stop_id,
  (SELECT COUNT(*) FROM live_activities AS x WHERE x.region_id = sqlc.arg(region_id) AND x.activity_id = sqlc.arg(activity_id)) AS existing;
