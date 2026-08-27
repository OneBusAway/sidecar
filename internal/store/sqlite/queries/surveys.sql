-- Every parameter is a named arg; a name used twice (@now below) binds
-- once. Parameters compared against NULLable columns are CAST so sqlc
-- types them int64 rather than sql.NullInt64.

-- name: CreateStudy :one
INSERT INTO studies (region_id, name, description, created_at, updated_at)
VALUES (@region_id, @name, @description, @now, @now)
RETURNING *;

-- name: GetStudy :one
SELECT * FROM studies WHERE id = @id;

-- name: ListStudiesByRegion :many
SELECT * FROM studies WHERE region_id = @region_id ORDER BY id ASC;

-- name: UpdateStudy :one
UPDATE studies SET name = @name, description = @description, updated_at = @now
WHERE id = @id AND region_id = @region_id
RETURNING *;

-- name: GetStudyInRegion :one
SELECT * FROM studies WHERE id = @id AND region_id = @region_id;

-- name: CreateSurvey :one
INSERT INTO surveys (
  study_id, name, available, start_time, end_time,
  show_on_map, show_on_stops, always_visible, allows_multiple_responses,
  visible_stop_list, visible_route_list, created_at, updated_at
) VALUES (
  @study_id, @name, @available, @start_time, @end_time,
  @show_on_map, @show_on_stops, @always_visible, @allows_multiple_responses,
  @visible_stop_list, @visible_route_list, @now, @now
)
RETURNING *;

-- name: GetSurvey :one
SELECT * FROM surveys WHERE id = @id;

-- name: ListSurveysByRegion :many
SELECT surveys.* FROM surveys
JOIN studies ON studies.id = surveys.study_id
WHERE studies.region_id = @region_id
ORDER BY surveys.id ASC;

-- The spec 7.1 filter (design spec 2.4): available, and inside the window
-- when one is set. Both bounds inclusive, matching the reference's BETWEEN.
-- name: ListActiveSurveysByRegion :many
SELECT surveys.* FROM surveys
JOIN studies ON studies.id = surveys.study_id
WHERE studies.region_id = @region_id
  AND surveys.available = TRUE
  AND (surveys.start_time IS NULL OR surveys.start_time <= CAST(@now AS INTEGER))
  AND (surveys.end_time   IS NULL OR surveys.end_time   >= CAST(@now AS INTEGER))
ORDER BY surveys.id ASC;

-- name: UpdateSurvey :one
UPDATE surveys SET
  name = @name, available = @available, start_time = @start_time, end_time = @end_time,
  show_on_map = @show_on_map, show_on_stops = @show_on_stops,
  always_visible = @always_visible, allows_multiple_responses = @allows_multiple_responses,
  visible_stop_list = @visible_stop_list, visible_route_list = @visible_route_list,
  updated_at = @now
WHERE id = @id
RETURNING *;

-- name: DeleteSurvey :execrows
DELETE FROM surveys WHERE id = @id;

-- name: CountResponsesForSurvey :one
SELECT COUNT(*) FROM survey_responses WHERE survey_id = @survey_id;

-- name: ListQuestionsBySurvey :many
SELECT * FROM survey_questions WHERE survey_id = @survey_id
ORDER BY position ASC, id ASC;

-- name: InsertQuestion :one
INSERT INTO survey_questions (survey_id, position, required, question_type, content, created_at, updated_at)
VALUES (@survey_id, @position, @required, @question_type, @content, @now, @now)
RETURNING *;

-- name: DeleteQuestionsForSurvey :exec
DELETE FROM survey_questions WHERE survey_id = @survey_id;

-- name: CreateResponse :one
INSERT INTO survey_responses (
  survey_id, public_id, user_identifier, stop_identifier, stop_latitude, stop_longitude,
  answers, created_at, updated_at
) VALUES (
  @survey_id, @public_id, @user_identifier, @stop_identifier, @stop_latitude, @stop_longitude,
  @answers, @now, @now
)
RETURNING *;

-- name: GetResponseByPublicID :one
SELECT * FROM survey_responses WHERE public_id = @public_id;

-- name: GetResponseByPublicIDInRegion :one
SELECT survey_responses.* FROM survey_responses
JOIN surveys ON surveys.id = survey_responses.survey_id
JOIN studies ON studies.id = surveys.study_id
WHERE survey_responses.public_id = @public_id
  AND studies.region_id = @region_id;

-- name: UpdateResponseAnswers :one
UPDATE survey_responses SET answers = @answers, updated_at = @now
WHERE public_id = @public_id
RETURNING *;

-- name: ListResponsesBySurvey :many
SELECT * FROM survey_responses WHERE survey_id = @survey_id
ORDER BY created_at ASC, id ASC;
