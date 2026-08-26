-- Comments in this file must stay ASCII-only. sqlc expands `*` and renumbers
-- sqlc.arg() by byte offset into each statement's text, and a multi-byte rune
-- anywhere in a preceding comment shifts those offsets: one section-sign
-- character above a query was enough to emit the garbage SQL "SELECTid ..."
-- and fail generation. Cite the design spec as "section N", not with the sign.

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

-- name: ListPushAudience :many
-- Pages a region's registrations by id (design spec section 2.3). The
-- predicate (test_device OR NOT test_only) selects everyone when
-- test_only is false and only test devices when it is true.
SELECT * FROM push_registrations
WHERE region_id = sqlc.arg(region_id)
  AND id > sqlc.arg(after_id)
  AND (test_device OR NOT CAST(sqlc.arg(test_only) AS BOOLEAN))
ORDER BY id
LIMIT sqlc.arg(limit);

-- name: CountPushAudience :many
-- The same audience as ListPushAudience, grouped by platform so the admin
-- API can report the iOS/Android split before a send (design spec
-- section 2.3).
SELECT operating_system, COUNT(*) AS n FROM push_registrations
WHERE region_id = sqlc.arg(region_id)
  AND (test_device OR NOT CAST(sqlc.arg(test_only) AS BOOLEAN))
GROUP BY operating_system;
