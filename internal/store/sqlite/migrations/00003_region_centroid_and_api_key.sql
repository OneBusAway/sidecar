-- +goose Up
ALTER TABLE regions ADD COLUMN latitude    REAL;
ALTER TABLE regions ADD COLUMN longitude   REAL;
ALTER TABLE regions ADD COLUMN oba_api_key TEXT NOT NULL DEFAULT '';

-- Latitude and longitude are a pair or they are absent. The Go type makes a
-- half-set centroid unrepresentable; without these triggers the schema does
-- not, and a future writer could persist one.
--
-- The goose annotations are required, not decorative: goose splits a migration
-- on semicolons, and a trigger body contains one. Without them each trigger is
-- submitted as two fragments and the migration fails.
-- +goose StatementBegin
CREATE TRIGGER regions_centroid_paired_insert
AFTER INSERT ON regions
WHEN (NEW.latitude IS NULL) <> (NEW.longitude IS NULL)
BEGIN SELECT RAISE(ABORT, 'latitude and longitude must be set together'); END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER regions_centroid_paired_update
AFTER UPDATE OF latitude, longitude ON regions
WHEN (NEW.latitude IS NULL) <> (NEW.longitude IS NULL)
BEGIN SELECT RAISE(ABORT, 'latitude and longitude must be set together'); END;
-- +goose StatementEnd

-- +goose Down
-- The triggers must go first: SQLite refuses to DROP a column that a live
-- trigger still references.
DROP TRIGGER regions_centroid_paired_update;
DROP TRIGGER regions_centroid_paired_insert;
ALTER TABLE regions DROP COLUMN oba_api_key;
ALTER TABLE regions DROP COLUMN longitude;
ALTER TABLE regions DROP COLUMN latitude;
