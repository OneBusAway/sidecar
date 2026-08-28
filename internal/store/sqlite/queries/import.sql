-- Import statements (internal/export): every insert carries the source's
-- id or natural key and does nothing when THAT key already exists, so
-- re-running an import on a later export adds only what is new. The
-- conflict target is named per statement on purpose: any other constraint
-- (a CHECK, a second UNIQUE such as a question's position or a ghost bus
-- report's dedupe key) still fails loudly instead of being counted as
-- "already present". :execrows reports whether the row landed. Timestamps
-- are epoch seconds, as everywhere.

-- name: ImportAlert :execrows
INSERT INTO alerts (
  id, region_id, agency_id, header_text, description_text, url,
  cause, effect, severity_level, start_time, end_time,
  published, is_test, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (id) DO NOTHING;

-- name: ImportAlertTranslation :execrows
INSERT INTO alert_translations (
  alert_id, language, field, text, source_sha256, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (alert_id, language, field) DO NOTHING;

-- name: ImportStudy :execrows
INSERT INTO studies (id, region_id, name, description, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT (id) DO NOTHING;

-- name: ImportSurvey :execrows
INSERT INTO surveys (
  id, study_id, name, available, start_time, end_time,
  show_on_map, show_on_stops, always_visible, allows_multiple_responses,
  visible_stop_list, visible_route_list, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (id) DO NOTHING;

-- name: ImportSurveyQuestion :execrows
INSERT INTO survey_questions (
  id, survey_id, position, required, question_type, content, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (id) DO NOTHING;

-- name: ImportSurveyResponse :execrows
INSERT INTO survey_responses (
  survey_id, public_id, user_identifier, stop_identifier, stop_latitude, stop_longitude,
  answers, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (public_id) DO NOTHING;

-- name: ImportPushRegistration :execrows
INSERT INTO push_registrations (
  region_id, token, operating_system, apns_sandbox, locale, test_device, description,
  last_seen_at, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (region_id, token) DO NOTHING;

-- name: ImportGhostBusReport :execrows
INSERT INTO ghost_bus_reports (
  region_id, public_identifier, user_identifier, trip_identifier, service_date,
  route_identifier, stop_identifier, vehicle_identifier, stop_sequence,
  predicted, schedule_deviation_minutes, wait_duration_minutes, comment,
  user_latitude, user_longitude,
  scheduled_arrival_at, predicted_arrival_at, prediction_last_updated_at,
  snapshot_status, snapshot_json, snapshot_captured_at, snapshot_attempts,
  created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (public_identifier) DO NOTHING;

-- name: GetQuestionSurveyID :one
SELECT survey_id FROM survey_questions WHERE id = ?;
