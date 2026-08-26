-- +goose Up
-- Alert push fan-out (design spec §3). One row per send of one alert.
CREATE TABLE alert_pushes (
  id               INTEGER PRIMARY KEY AUTOINCREMENT,
  alert_id         INTEGER NOT NULL REFERENCES alerts(id) ON DELETE CASCADE,
  region_id        INTEGER NOT NULL REFERENCES regions(id) ON DELETE CASCADE,
  audience         TEXT    NOT NULL CHECK (audience IN ('all', 'test')),
  status           TEXT    NOT NULL CHECK (status IN ('queued', 'sending', 'sent', 'failed', 'canceled')),
  messages         TEXT    NOT NULL,
  batch_cursor     INTEGER NOT NULL DEFAULT 0,
  device_count     INTEGER NOT NULL DEFAULT 0,
  submitted_count  INTEGER NOT NULL DEFAULT 0,
  failed_count     INTEGER NOT NULL DEFAULT 0,
  attempts         INTEGER NOT NULL DEFAULT 0,
  last_error       TEXT    NOT NULL DEFAULT '',
  started_at       INTEGER,
  completed_at     INTEGER,
  created_at       INTEGER NOT NULL,
  updated_at       INTEGER NOT NULL
);
CREATE INDEX alert_pushes_alert_idx  ON alert_pushes (alert_id, id);
CREATE INDEX alert_pushes_status_idx ON alert_pushes (status, updated_at);
-- At most one queued/sending push per alert (design spec §2.2).
CREATE UNIQUE INDEX alert_pushes_inflight_idx ON alert_pushes (alert_id)
  WHERE status IN ('queued', 'sending');

-- Per-token failure accounting; (push_id, token_sha256) dedups replayed
-- feedback (design spec §2.8). Only a hash is stored: nothing reads the
-- token back, and a plaintext copy would outlive the registration's
-- retention (spec §13).
CREATE TABLE alert_push_failures (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  push_id      INTEGER NOT NULL REFERENCES alert_pushes(id) ON DELETE CASCADE,
  token_sha256 TEXT    NOT NULL,
  reason       TEXT    NOT NULL,
  created_at   INTEGER NOT NULL,
  UNIQUE (push_id, token_sha256)
);

-- The audience query pages by id within a region (spec §12 cursor).
CREATE INDEX push_registrations_audience_idx ON push_registrations (region_id, id);

-- +goose Down
DROP INDEX push_registrations_audience_idx;
DROP TABLE alert_push_failures;
DROP TABLE alert_pushes;
