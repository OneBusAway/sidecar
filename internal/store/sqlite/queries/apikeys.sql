-- Comments in this file must stay ASCII-only. sqlc renumbers sqlc.arg() by
-- byte offset into each statement's text, and a multi-byte rune anywhere in
-- a preceding comment shifts those offsets, emitting garbage SQL. Cite the
-- design spec as "spec section N", not with the section sign.

-- name: CreateRegionAPIKey :one
INSERT INTO region_api_keys (
  region_id, name, key_hash, created_by_kind, created_by_id, created_at
) VALUES (
  sqlc.arg(region_id), sqlc.arg(name), sqlc.arg(key_hash),
  sqlc.arg(created_by_kind), sqlc.arg(created_by_id), sqlc.arg(created_at)
)
RETURNING *;

-- name: GetRegionAPIKeyByHash :one
SELECT * FROM region_api_keys WHERE key_hash = sqlc.arg(key_hash);

-- name: GetRegionAPIKey :one
SELECT * FROM region_api_keys
WHERE id = sqlc.arg(id) AND region_id = sqlc.arg(region_id);

-- name: ListRegionAPIKeys :many
SELECT * FROM region_api_keys
WHERE region_id = sqlc.arg(region_id)
ORDER BY id DESC;

-- name: ListRegionAPIKeysByCreator :many
-- The CLI case has created_by_id IS NULL and lives in its own query below.
-- "created_by_id = ?" with a NULL bind matches no row in SQL, so folding the
-- two cases into one statement would silently return nothing for the CLI --
-- the failure mode the design spec calls out.
SELECT * FROM region_api_keys
WHERE created_by_kind = sqlc.arg(created_by_kind)
  AND created_by_id = sqlc.arg(created_by_id)
ORDER BY id DESC;

-- name: ListRegionAPIKeysByCLI :many
SELECT * FROM region_api_keys
WHERE created_by_kind = 'cli' AND created_by_id IS NULL
ORDER BY id DESC;

-- name: RevokeRegionAPIKey :execrows
UPDATE region_api_keys SET
  revoked_at      = sqlc.arg(revoked_at),
  revoked_by_kind = sqlc.arg(revoked_by_kind),
  revoked_by_id   = sqlc.arg(revoked_by_id)
WHERE id = sqlc.arg(id) AND region_id = sqlc.arg(region_id) AND revoked_at IS NULL;

-- name: RevokeRegionAPIKeysByCreator :many
UPDATE region_api_keys SET
  revoked_at      = sqlc.arg(revoked_at),
  revoked_by_kind = sqlc.arg(revoked_by_kind),
  revoked_by_id   = sqlc.arg(revoked_by_id)
WHERE created_by_kind = sqlc.arg(created_by_kind)
  AND created_by_id = sqlc.arg(created_by_id)
  AND revoked_at IS NULL
RETURNING id;

-- name: RevokeRegionAPIKeysByCLI :many
UPDATE region_api_keys SET
  revoked_at      = sqlc.arg(revoked_at),
  revoked_by_kind = sqlc.arg(revoked_by_kind),
  revoked_by_id   = sqlc.arg(revoked_by_id)
WHERE created_by_kind = 'cli' AND created_by_id IS NULL AND revoked_at IS NULL
RETURNING id;

-- name: TouchRegionAPIKey :exec
UPDATE region_api_keys SET last_used_at = sqlc.arg(last_used_at) WHERE id = sqlc.arg(id);

-- name: CreateServicePrincipal :one
INSERT INTO service_principals (name, key_hash, created_at)
VALUES (sqlc.arg(name), sqlc.arg(key_hash), sqlc.arg(created_at))
RETURNING *;

-- name: GetServicePrincipalByHash :one
SELECT * FROM service_principals WHERE key_hash = sqlc.arg(key_hash);

-- name: GetServicePrincipal :one
SELECT * FROM service_principals WHERE id = sqlc.arg(id);

-- name: ListServicePrincipals :many
SELECT * FROM service_principals ORDER BY id DESC;

-- name: RevokeServicePrincipal :execrows
UPDATE service_principals SET revoked_at = sqlc.arg(revoked_at)
WHERE id = sqlc.arg(id) AND revoked_at IS NULL;

-- name: TouchServicePrincipal :exec
UPDATE service_principals SET last_used_at = sqlc.arg(last_used_at) WHERE id = sqlc.arg(id);
