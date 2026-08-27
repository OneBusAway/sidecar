-- +goose Up
-- Region API keys and service principals (design spec section 3).
--
-- Both tables store only the hex SHA-256 of the raw key, the same posture
-- sessions take. A stolen backup therefore yields no usable credential --
-- though it still yields every plaintext secret the sidecar already keeps
-- (region OBA keys, push tokens), so this narrows that threat rather than
-- closing it.
CREATE TABLE service_principals (
    id           INTEGER PRIMARY KEY,
    name         TEXT    NOT NULL,
    key_hash     TEXT    NOT NULL UNIQUE,
    created_at   INTEGER NOT NULL,
    last_used_at INTEGER,
    revoked_at   INTEGER
);

-- Revoked rows are kept, never deleted: `key list` shows history, and a
-- revoked key's hash can never be re-minted by accident. created_by_* and
-- revoked_by_* are deliberately NOT foreign keys -- a deleted operator or a
-- revoked principal must not orphan the audit trail.
CREATE TABLE region_api_keys (
    id              INTEGER PRIMARY KEY,
    region_id       INTEGER NOT NULL REFERENCES regions(id) ON DELETE CASCADE,
    name            TEXT    NOT NULL,
    key_hash        TEXT    NOT NULL UNIQUE,
    created_by_kind TEXT    NOT NULL CHECK (created_by_kind IN ('operator', 'principal', 'cli')),
    created_by_id   INTEGER,
    created_at      INTEGER NOT NULL,
    last_used_at    INTEGER,
    revoked_at      INTEGER,
    revoked_by_kind TEXT    CHECK (revoked_by_kind IN ('operator', 'principal', 'cli')),
    revoked_by_id   INTEGER,
    CHECK ((created_by_kind = 'cli') = (created_by_id IS NULL)),
    CHECK ((revoked_at IS NULL) = (revoked_by_kind IS NULL)),
    CHECK ((revoked_by_kind IS NULL) OR ((revoked_by_kind = 'cli') = (revoked_by_id IS NULL)))
);
CREATE INDEX region_api_keys_region ON region_api_keys(region_id);
CREATE INDEX region_api_keys_creator ON region_api_keys(created_by_kind, created_by_id);

-- +goose Down
DROP INDEX region_api_keys_creator;
DROP INDEX region_api_keys_region;
DROP TABLE region_api_keys;
DROP TABLE service_principals;
