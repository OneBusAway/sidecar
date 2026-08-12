-- name: CreateUser :one
INSERT INTO users (username, password_hash, created_at, updated_at)
VALUES (?, ?, ?, ?)
RETURNING *;

-- name: GetUserByUsername :one
SELECT * FROM users WHERE username = ?;

-- name: GetUserByID :one
SELECT * FROM users WHERE id = ?;

-- name: ListUsers :many
SELECT * FROM users ORDER BY username;

-- name: DeleteUser :execrows
DELETE FROM users WHERE username = ?;

-- name: UpdateUserPassword :execrows
UPDATE users SET password_hash = ?, updated_at = ? WHERE username = ?;
