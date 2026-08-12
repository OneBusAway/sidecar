-- name: CreateSession :exec
INSERT INTO sessions (token_hash, user_id, created_at, expires_at)
VALUES (?, ?, ?, ?);

-- Expiry is NOT filtered here: the adapter evaluates it against the
-- injected clock and deletes expired rows itself (delete-on-read contract).
-- name: GetSessionRow :one
SELECT * FROM sessions WHERE token_hash = ?;

-- name: DeleteSession :execrows
DELETE FROM sessions WHERE token_hash = ?;

-- name: DeleteUserSessions :execrows
DELETE FROM sessions WHERE user_id = ?;

-- name: DeleteExpiredSessions :execrows
DELETE FROM sessions WHERE expires_at <= ?;
