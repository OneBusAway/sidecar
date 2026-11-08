-- +goose Up
-- An empty regions.timezone means "not configured". The column keeps its
-- DEFAULT 'UTC', but nothing relies on it any more: the directory upsert
-- writes '' explicitly on insert. A stored 'UTC' cannot be told apart from
-- that old default, so every 'UTC' row becomes unconfigured; an operator
-- whose region really is on UTC sets it again with region set.
UPDATE regions SET timezone = '' WHERE timezone = 'UTC';

-- +goose Down
UPDATE regions SET timezone = 'UTC' WHERE timezone = '';
