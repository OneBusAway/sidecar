-- +goose Up
-- Due-window bookkeeping for the alarm scheduler (spec section 5.3, section
-- 12). check_after is the epoch-seconds instant before which a sweep skips
-- the row: 0 (the default, and what every new alarm starts with) means "due
-- now". The scheduler pushes it out after a Wait whose departure is far
-- off, so an alarm set hours ahead stops costing an OBA lookup per minute.
-- No index: the table stays small (fire-then-delete, 3-strike reaper), so
-- a scan per minute beats maintaining an index every deferral rewrites.
ALTER TABLE alarms ADD COLUMN check_after INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE alarms DROP COLUMN check_after;
