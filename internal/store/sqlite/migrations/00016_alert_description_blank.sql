-- +goose Up
-- Create and Update now reject a whitespace-only description, and Update
-- checks the merged row, so an alert with one stored would fail every later
-- edit. Clear any stored.
-- trim() alone strips only spaces, hence tab, LF, VT, FF, CR and space.
UPDATE alerts SET description_text = ''
WHERE length(trim(description_text, char(9, 10, 11, 12, 13, 32))) = 0;

-- +goose Down
-- Nothing to undo: '' was always valid, and the cleared whitespace is gone.
