-- +goose Up
-- An empty regions.timezone means "not configured". The column keeps its
-- DEFAULT 'UTC', but nothing relies on it any more: the directory upsert
-- writes '' explicitly on insert. A stored 'UTC' cannot be told apart from
-- that old default, so every 'UTC' row becomes unconfigured; an operator
-- whose region really is on UTC sets it again with region set.
UPDATE regions SET timezone = '' WHERE timezone = 'UTC';

-- +goose Down
-- length() = 0 rather than = '': same test on a NOT NULL TEXT column, and
-- static analysers that assume Oracle (where '' is NULL) do not misread it.
UPDATE regions SET timezone = 'UTC' WHERE length(timezone) = 0;
