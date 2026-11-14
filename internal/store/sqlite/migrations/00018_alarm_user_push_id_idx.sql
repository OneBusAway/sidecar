-- +goose Up
-- The per-device live-alarm cap counts a device's alarms on every create
-- (CountAlarmsByUserPushID). The V1 dedupe index leads with region_id and
-- only covers api_version = 1, so it cannot serve that count.
CREATE INDEX alarms_user_push_id_idx ON alarms (user_push_id);

-- +goose Down
DROP INDEX alarms_user_push_id_idx;
