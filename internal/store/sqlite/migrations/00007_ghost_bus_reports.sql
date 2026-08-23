-- +goose Up
-- Spec §8. service_date and the *_arrival_at / prediction_last_updated_at
-- columns are epoch MILLISECONDS stored as received (service_date is a
-- dedupe key component; a lossy transform there would change dedupe
-- behavior). created_at / updated_at / snapshot_captured_at are epoch
-- seconds. INTEGER everywhere -- never DATETIME (modernc stores
-- time.Time.String() in DATETIME cells and ORDER BY sorts text).
CREATE TABLE ghost_bus_reports (
  id                         INTEGER PRIMARY KEY AUTOINCREMENT,
  region_id                  INTEGER NOT NULL REFERENCES regions(id) ON DELETE CASCADE,
  public_identifier          TEXT    NOT NULL,
  user_identifier            TEXT    NOT NULL,
  trip_identifier            TEXT    NOT NULL,
  service_date               INTEGER NOT NULL,
  route_identifier           TEXT    NOT NULL DEFAULT '',
  stop_identifier            TEXT    NOT NULL DEFAULT '',
  vehicle_identifier         TEXT    NOT NULL DEFAULT '',
  stop_sequence              INTEGER,
  predicted                  INTEGER,
  schedule_deviation_minutes INTEGER,
  wait_duration_minutes      INTEGER NOT NULL,
  comment                    TEXT    NOT NULL DEFAULT '',
  user_latitude              REAL,
  user_longitude             REAL,
  scheduled_arrival_at       INTEGER,
  predicted_arrival_at       INTEGER,
  prediction_last_updated_at INTEGER,
  snapshot_status            TEXT    NOT NULL DEFAULT 'pending'
    CHECK (snapshot_status IN ('pending', 'captured', 'unavailable')),
  snapshot_json              TEXT    NOT NULL DEFAULT '',
  snapshot_captured_at       INTEGER,
  snapshot_attempts          INTEGER NOT NULL DEFAULT 0,
  created_at                 INTEGER NOT NULL,
  updated_at                 INTEGER NOT NULL
);

CREATE UNIQUE INDEX ghost_bus_reports_public_identifier_idx
  ON ghost_bus_reports(public_identifier);

-- The §8 dedupe key. The adapter tells this index's violation apart from
-- the public_identifier one by the columns named in the error message;
-- region_id leading keeps that message distinctive.
CREATE UNIQUE INDEX ghost_bus_reports_dedupe_idx
  ON ghost_bus_reports(region_id, user_identifier, trip_identifier, service_date);

-- The snapshot worker's poll predicate, verbatim: the attempts guard is
-- what stops a crash-stranded row from being retried forever.
CREATE INDEX ghost_bus_reports_snapshot_pending_idx ON ghost_bus_reports(id)
  WHERE snapshot_status = 'pending' AND snapshot_attempts < 3;

CREATE INDEX ghost_bus_reports_region_created_idx
  ON ghost_bus_reports(region_id, created_at);

-- +goose Down
DROP TABLE ghost_bus_reports;
