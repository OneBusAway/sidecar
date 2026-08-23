-- +goose Up
CREATE TABLE studies (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  region_id   INTEGER NOT NULL REFERENCES regions(id) ON DELETE CASCADE,
  name        TEXT    NOT NULL,
  description TEXT    NOT NULL DEFAULT '',
  created_at  INTEGER NOT NULL,
  updated_at  INTEGER NOT NULL
);
CREATE INDEX studies_region_idx ON studies (region_id);

CREATE TABLE surveys (
  id                        INTEGER PRIMARY KEY AUTOINCREMENT,
  study_id                  INTEGER NOT NULL REFERENCES studies(id) ON DELETE CASCADE,
  name                      TEXT    NOT NULL,
  available                 BOOLEAN NOT NULL DEFAULT TRUE,
  -- Both set or both NULL; enforced at authoring (design spec 2.4), which
  -- is what lets the active-window predicate stay two simple clauses.
  start_time                INTEGER,
  end_time                  INTEGER,
  show_on_map               BOOLEAN NOT NULL DEFAULT FALSE,
  show_on_stops             BOOLEAN NOT NULL DEFAULT FALSE,
  always_visible            BOOLEAN NOT NULL DEFAULT FALSE,
  allows_multiple_responses BOOLEAN NOT NULL DEFAULT FALSE,
  -- NULL = everywhere; otherwise a JSON array of ids (design spec 2.11).
  -- JSON rather than CSV because GTFS ids are free-form text.
  visible_stop_list         TEXT,
  visible_route_list        TEXT,
  created_at                INTEGER NOT NULL,
  updated_at                INTEGER NOT NULL
);
CREATE INDEX surveys_study_idx ON surveys (study_id);

CREATE TABLE survey_questions (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  survey_id     INTEGER NOT NULL REFERENCES surveys(id) ON DELETE CASCADE,
  position      INTEGER NOT NULL,
  required      BOOLEAN NOT NULL DEFAULT FALSE,
  -- iOS decodes content.type as a closed enum and drops the whole region's
  -- survey list on an unknown value, so the invariant holds in SQL too.
  question_type TEXT    NOT NULL
    CHECK (question_type IN ('text', 'label', 'radio', 'checkbox', 'external_survey')),
  -- The full content document (design spec 2.12); question_type is
  -- duplicated out of it so the CHECK above can hold.
  content       TEXT    NOT NULL,
  created_at    INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL,
  UNIQUE (survey_id, position)
);

CREATE TABLE survey_responses (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  survey_id       INTEGER NOT NULL REFERENCES surveys(id) ON DELETE CASCADE,
  public_id       TEXT    NOT NULL UNIQUE,
  user_identifier TEXT    NOT NULL,
  stop_identifier TEXT,
  stop_latitude   REAL,
  stop_longitude  REAL,
  -- JSON array of answer objects in the server's canonical serialization
  -- (design spec 2.5), merged by question_id on amend (2.6).
  answers         TEXT    NOT NULL DEFAULT '[]',
  created_at      INTEGER NOT NULL,
  updated_at      INTEGER NOT NULL
);
CREATE INDEX survey_responses_survey_idx ON survey_responses (survey_id);
CREATE INDEX survey_responses_user_idx   ON survey_responses (user_identifier);

-- +goose Down
DROP TABLE survey_responses;
DROP TABLE survey_questions;
DROP TABLE surveys;
DROP TABLE studies;
