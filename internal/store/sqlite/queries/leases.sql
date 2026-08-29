-- AcquireLease is one atomic statement. The SELECT yields a row only when
-- the name is free, expired, or already the caller's, so in every other
-- case nothing is inserted and RETURNING yields no row -- which the adapter
-- reads as "not acquired". When it does yield, the INSERT either takes a
-- free name or hits the existing row and the conflict branch renews /
-- takes it over. Expiry is inclusive (expires_at > now blocks; equal is
-- free).
--
-- The guard is a NOT EXISTS on the SELECT rather than a WHERE on the DO
-- UPDATE because sqlc does not extract a parameter written inside the
-- ON CONFLICT ... WHERE clause: it hands the literal text to the driver,
-- the same silent shape as the IN (...) trap (see CLAUDE.md). The CASTs
-- give sqlc column types for a SELECT that has no table to infer from.

-- name: AcquireLease :one
INSERT INTO leases (name, holder, expires_at)
SELECT CAST(@name AS TEXT), CAST(@holder AS TEXT), CAST(@expires_at AS INTEGER)
WHERE NOT EXISTS (
  SELECT 1 FROM leases
  WHERE name = @name AND holder <> @holder AND expires_at > CAST(@now AS INTEGER)
)
ON CONFLICT (name) DO UPDATE
  SET holder = excluded.holder, expires_at = excluded.expires_at
RETURNING holder;

-- name: ReleaseLease :exec
DELETE FROM leases WHERE name = @name AND holder = @holder;
