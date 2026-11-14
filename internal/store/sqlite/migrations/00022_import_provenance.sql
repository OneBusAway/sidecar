-- +goose Up
-- Import provenance (security audit SC-14). imported_at is the epoch-seconds
-- instant `sidecar-admin import` first wrote the row; NULL means the row was
-- authored on this sidecar. A later import that finds a row already holding
-- its id treats it as the same row (skip, the documented insert-if-absent
-- delta) only when it was imported; a sidecar-authored row at that id is a
-- different row and a hard conflict. Rows imported before this column
-- existed are NULL too: the importer recognizes them by the created_at the
-- export carries verbatim and stamps them then, so no backfill guesses here.
-- Questions need no column: a question is only reached under a survey that
-- already passed this check, and its survey_id is compared as before.
ALTER TABLE alerts ADD COLUMN imported_at INTEGER;
ALTER TABLE studies ADD COLUMN imported_at INTEGER;
ALTER TABLE surveys ADD COLUMN imported_at INTEGER;

-- +goose Down
ALTER TABLE surveys DROP COLUMN imported_at;
ALTER TABLE studies DROP COLUMN imported_at;
ALTER TABLE alerts DROP COLUMN imported_at;
