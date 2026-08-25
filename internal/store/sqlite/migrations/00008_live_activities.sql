-- +goose Up
CREATE TABLE live_activities (
  id                   INTEGER PRIMARY KEY AUTOINCREMENT,
  region_id            INTEGER NOT NULL REFERENCES regions(id) ON DELETE CASCADE,
  -- The public address (spec section 2.4). Globally unique like alarms.token.
  token                TEXT    NOT NULL UNIQUE,
  -- ActivityKit activity id: the upsert key with region_id (spec section 6.1).
  activity_id          TEXT    NOT NULL,
  -- ActivityKit push token; rotates over the activity's lifetime. Not the
  -- device alert token.
  push_token           TEXT    NOT NULL,
  apns_sandbox         BOOLEAN NOT NULL DEFAULT FALSE,
  stop_id              TEXT    NOT NULL,
  route_short_name     TEXT    NOT NULL,
  trip_headsign        TEXT    NOT NULL,
  -- Optional trip metadata, stored as sent. '' / 0 mean omitted;
  -- stop_sequence needs NULL because 0 is a real value. service_date is
  -- epoch MILLISECONDS exactly as OBA sends it -- the one INTEGER column
  -- here that is not epoch seconds; it is never converted to a time.Time.
  trip_id              TEXT    NOT NULL DEFAULT '',
  service_date         INTEGER NOT NULL DEFAULT 0,
  vehicle_id           TEXT    NOT NULL DEFAULT '',
  stop_sequence        INTEGER,
  -- Canonical JSON of the last pushed content state (design spec section 3).
  last_content_state   TEXT    NOT NULL DEFAULT '{"arrivals":[]}',
  last_pushed_at       INTEGER,
  consecutive_failures INTEGER NOT NULL DEFAULT 0,
  -- Bumped on every re-registration (token rotation). The updater deletes
  -- with a compare on this value, so a sweep that decided to end a row it
  -- listed a moment ago cannot remove a registration refreshed since.
  revision             INTEGER NOT NULL DEFAULT 0,
  expires_at           INTEGER NOT NULL,
  created_at           INTEGER NOT NULL,
  updated_at           INTEGER NOT NULL
);

-- UNIQUE (not just an index) so the concurrent first-registration race
-- surfaces as a constraint violation the adapter maps to ErrDuplicate.
CREATE UNIQUE INDEX live_activities_activity_idx ON live_activities (region_id, activity_id);
-- The feedback webhook deletes by push token.
CREATE INDEX live_activities_push_token_idx ON live_activities (push_token);

-- +goose Down
DROP TABLE live_activities;
