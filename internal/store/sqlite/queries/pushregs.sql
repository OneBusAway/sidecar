-- UpsertPushRegistration is NOT generated here. sqlc v1.31.1's SQLite engine
-- does not extract bind parameters that appear on the right-hand side of an
-- ON CONFLICT ... DO UPDATE SET assignment (confirmed for @name, sqlc.arg(),
-- and bare ? alike -- in every form the parameter is silently dropped: the
-- generated Params struct is missing the field, and the literal parameter
-- text is left untouched in the SQL string handed to the driver, which then
-- fails at execution with a bind-count mismatch). The sticky-flag CASE WHEN
-- expressions this table's upsert needs live entirely in that position, so
-- the statement is hand-written in sqlite/pushregs.go and run directly
-- against *sql.DB instead -- the same escape hatch store.go's
-- WriteHalfSetCentroidForTest/InsertHalfSetCentroidForTest already use for
-- statements sqlc cannot express.

-- name: GetPushRegistration :one
SELECT * FROM push_registrations
WHERE region_id = @region_id AND token = @token;

-- name: DeletePushRegistration :execrows
DELETE FROM push_registrations
WHERE region_id = @region_id AND token = @token;

-- name: DeletePushRegistrationsByToken :execrows
DELETE FROM push_registrations WHERE token = @token;

-- name: PrunePushRegistrations :execrows
DELETE FROM push_registrations WHERE last_seen_at < @cutoff;
