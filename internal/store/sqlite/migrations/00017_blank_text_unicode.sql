-- +goose Up
-- Migrations 15 and 16 cleared only ASCII whitespace, but the Go checks use
-- strings.TrimSpace, which also strips Unicode spaces, so a value such as a
-- lone no-break space still fails every later edit. Clear any stored.
-- The char() list is the set strings.TrimSpace strips: tab, LF, VT, FF, CR,
-- space, NEL, no-break space and the rest of Unicode Z (Zs, Zl, Zp).
UPDATE regions SET default_agency_id = ''
WHERE length(trim(default_agency_id, char(9, 10, 11, 12, 13, 32, 133, 160, 5760, 8192, 8193, 8194, 8195, 8196, 8197, 8198, 8199, 8200, 8201, 8202, 8232, 8233, 8239, 8287, 12288))) = 0;

UPDATE alerts SET description_text = ''
WHERE length(trim(description_text, char(9, 10, 11, 12, 13, 32, 133, 160, 5760, 8192, 8193, 8194, 8195, 8196, 8197, 8198, 8199, 8200, 8201, 8202, 8232, 8233, 8239, 8287, 12288))) = 0;

-- UpsertTranslation now rejects a blank translation, and the feed would
-- serve one as an empty string. Delete any stored.
DELETE FROM alert_translations
WHERE length(trim(text, char(9, 10, 11, 12, 13, 32, 133, 160, 5760, 8192, 8193, 8194, 8195, 8196, 8197, 8198, 8199, 8200, 8201, 8202, 8232, 8233, 8239, 8287, 12288))) = 0;

-- +goose Down
-- Nothing to undo: '' was always valid, and the cleared whitespace is gone.
-- Deleted translations are not restored: they held no text to serve.
