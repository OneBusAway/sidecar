-- +goose Up
CREATE TABLE push_registrations (
  id               INTEGER PRIMARY KEY AUTOINCREMENT,
  region_id        INTEGER NOT NULL REFERENCES regions(id) ON DELETE CASCADE,
  token            TEXT    NOT NULL,
  operating_system TEXT    NOT NULL CHECK (operating_system IN ('ios', 'android')),
  apns_sandbox     BOOLEAN NOT NULL DEFAULT FALSE,
  locale           TEXT    NOT NULL DEFAULT '',
  test_device      BOOLEAN NOT NULL DEFAULT FALSE,
  description      TEXT    NOT NULL DEFAULT '',
  last_seen_at     INTEGER NOT NULL,
  created_at       INTEGER NOT NULL,
  updated_at       INTEGER NOT NULL,
  UNIQUE (region_id, token)
);

-- The daily reaper scans by staleness alone (spec §4: 180 days unseen).
CREATE INDEX push_registrations_prune_idx ON push_registrations (last_seen_at);

-- +goose Down
DROP TABLE push_registrations;
