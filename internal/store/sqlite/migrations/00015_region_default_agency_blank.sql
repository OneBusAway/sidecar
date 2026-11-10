-- +goose Up
-- SetLocalFields now rejects a whitespace-only default, and every region
-- PATCH and region set writes the current one back, so clear any stored.
-- trim() alone strips only spaces, hence tab, LF, VT, FF, CR and space.
UPDATE regions SET default_agency_id = ''
WHERE length(trim(default_agency_id, char(9, 10, 11, 12, 13, 32))) = 0;

-- +goose Down
-- Nothing to undo: '' was always valid, and the cleared whitespace is gone.
