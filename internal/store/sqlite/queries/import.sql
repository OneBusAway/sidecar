-- Import statements (internal/export): every insert carries the source's
-- id or natural key and ignores a row that already exists, so re-running
-- an import on a later export adds only what is new. :execrows reports
-- whether the row landed. Timestamps are epoch seconds, as everywhere.

-- name: ImportAlert :execrows
INSERT OR IGNORE INTO alerts (
  id, region_id, agency_id, header_text, description_text, url,
  cause, effect, severity_level, start_time, end_time,
  published, is_test, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: ImportAlertTranslation :execrows
INSERT OR IGNORE INTO alert_translations (
  alert_id, language, field, text, source_sha256, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?);

-- name: ImportStudy :execrows
INSERT OR IGNORE INTO studies (id, region_id, name, description, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?);

-- name: ImportSurvey :execrows
INSERT OR IGNORE INTO surveys (
  id, study_id, name, available, start_time, end_time,
  show_on_map, show_on_stops, always_visible, allows_multiple_responses,
  visible_stop_list, visible_route_list, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: ImportSurveyQuestion :execrows
INSERT OR IGNORE INTO survey_questions (
  id, survey_id, position, required, question_type, content, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?);

-- name: ImportSurveyResponse :execrows
INSERT OR IGNORE INTO survey_responses (
  survey_id, public_id, user_identifier, stop_identifier, stop_latitude, stop_longitude,
  answers, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: ImportPushRegistration :execrows
INSERT OR IGNORE INTO push_registrations (
  region_id, token, operating_system, apns_sandbox, locale, test_device, description,
  last_seen_at, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?);

-- name: ImportGhostBusReport :execrows
INSERT OR IGNORE INTO ghost_bus_reports (
  region_id, public_identifier, user_identifier, trip_identifier, service_date,
  route_identifier, stop_identifier, vehicle_identifier, stop_sequence,
  predicted, schedule_deviation_minutes, wait_duration_minutes, comment,
  user_latitude, user_longitude,
  scheduled_arrival_at, predicted_arrival_at, prediction_last_updated_at,
  snapshot_status, snapshot_json, snapshot_captured_at, snapshot_attempts,
  created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);
