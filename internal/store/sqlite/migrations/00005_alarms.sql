-- +goose Up
CREATE TABLE alarms (
  id               INTEGER PRIMARY KEY AUTOINCREMENT,
  region_id        INTEGER NOT NULL REFERENCES regions(id) ON DELETE CASCADE,
  -- The public address (spec section 2.4). Globally unique, not per-region:
  -- tokens are 128 random bits, and a global uniqueness constraint means a
  -- token can never resolve to two riders' alarms.
  token            TEXT    NOT NULL UNIQUE,
  api_version      INTEGER NOT NULL CHECK (api_version IN (1, 2)),
  user_push_id     TEXT    NOT NULL,
  operating_system TEXT    NOT NULL CHECK (operating_system IN ('ios', 'android')),
  apns_sandbox     BOOLEAN NOT NULL DEFAULT FALSE,
  -- Trip-identity fields are stored as the client sent them, unvalidated
  -- (spec section 5.2). '' / 0 mean omitted; stop_sequence needs NULL because
  -- 0 is a real value (the trip's first stop).
  stop_id          TEXT    NOT NULL DEFAULT '',
  trip_id          TEXT    NOT NULL DEFAULT '',
  service_date     INTEGER NOT NULL DEFAULT 0,
  vehicle_id       TEXT    NOT NULL DEFAULT '',
  stop_sequence    INTEGER,
  seconds_before   INTEGER NOT NULL,
  message          TEXT    NOT NULL,
  failure_count    INTEGER NOT NULL DEFAULT 0,
  created_at       INTEGER NOT NULL,
  updated_at       INTEGER NOT NULL
);

-- V1 idempotency (spec section 5.1): re-POSTs of the same registration return
-- the existing alarm. UNIQUE (not just an index) so the concurrent-duplicate
-- race surfaces as a constraint violation the adapter maps to ErrDuplicate
-- instead of quietly minting a second alarm that fires twice.
CREATE UNIQUE INDEX alarms_v1_dedupe_idx
  ON alarms (region_id, user_push_id, trip_id, stop_id, service_date)
  WHERE api_version = 1;

-- +goose Down
DROP TABLE alarms;
