-- +goose Up
-- Region key scopes (migration design spec section 2.2). A JSON array of
-- scope names; the only defined scope is "push". Existing keys get [] and
-- so keep exactly the reach they had.
ALTER TABLE region_api_keys ADD COLUMN scopes TEXT NOT NULL DEFAULT '[]';

-- +goose Down
ALTER TABLE region_api_keys DROP COLUMN scopes;
