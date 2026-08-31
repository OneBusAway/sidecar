-- +goose Up
-- Background-loop leases (spec section 12). One row per loop name; holder
-- is an opaque per-process id and expires_at is epoch seconds. A row past
-- its expiry is free for any holder to take; see queries/leases.sql.
CREATE TABLE leases (
  name       TEXT    PRIMARY KEY,
  holder     TEXT    NOT NULL,
  expires_at INTEGER NOT NULL
);

-- +goose Down
DROP TABLE leases;
