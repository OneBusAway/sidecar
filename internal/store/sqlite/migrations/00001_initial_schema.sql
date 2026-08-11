-- +goose Up
CREATE TABLE regions (
  id                INTEGER PRIMARY KEY,
  region_name       TEXT    NOT NULL,
  oba_base_url      TEXT    NOT NULL,
  sidecar_base_url  TEXT    NOT NULL DEFAULT '',
  language          TEXT    NOT NULL DEFAULT '',
  active            BOOLEAN NOT NULL DEFAULT TRUE,
  default_agency_id TEXT    NOT NULL DEFAULT '',
  timezone          TEXT    NOT NULL DEFAULT 'UTC',
  synced_at         INTEGER NOT NULL,
  created_at        INTEGER NOT NULL,
  updated_at        INTEGER NOT NULL
);

CREATE TABLE alerts (
  id               INTEGER PRIMARY KEY AUTOINCREMENT,
  region_id        INTEGER NOT NULL REFERENCES regions(id) ON DELETE CASCADE,
  agency_id        TEXT    NOT NULL,
  header_text      TEXT    NOT NULL,
  description_text TEXT    NOT NULL DEFAULT '',
  url              TEXT    NOT NULL DEFAULT '',
  cause            TEXT    NOT NULL DEFAULT 'UNKNOWN_CAUSE',
  effect           TEXT    NOT NULL DEFAULT 'UNKNOWN_EFFECT',
  severity_level   TEXT    NOT NULL DEFAULT 'UNKNOWN_SEVERITY',
  start_time       INTEGER NOT NULL,
  end_time         INTEGER,
  published        BOOLEAN NOT NULL DEFAULT FALSE,
  is_test          BOOLEAN NOT NULL DEFAULT FALSE,
  created_at       INTEGER NOT NULL,
  updated_at       INTEGER NOT NULL
);

CREATE INDEX alerts_feed_idx ON alerts (region_id, published, start_time DESC, id DESC);

CREATE TABLE alert_translations (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  alert_id      INTEGER NOT NULL REFERENCES alerts(id) ON DELETE CASCADE,
  language      TEXT    NOT NULL CHECK (language <> 'en'),
  field         TEXT    NOT NULL CHECK (field IN ('header', 'description')),
  text          TEXT    NOT NULL,
  source_sha256 TEXT    NOT NULL,
  created_at    INTEGER NOT NULL,
  updated_at    INTEGER NOT NULL,
  UNIQUE (alert_id, language, field)
);

-- +goose Down
DROP TABLE alert_translations;
DROP TABLE alerts;
DROP TABLE regions;
