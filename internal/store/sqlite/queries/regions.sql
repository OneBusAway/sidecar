-- name: GetRegion :one
SELECT * FROM regions WHERE id = ?;

-- name: ListRegions :many
SELECT * FROM regions ORDER BY id;

-- name: UpsertRegionFromDirectory :exec
-- Partial upsert: default_agency_id, timezone, and oba_api_key are locally
-- managed and must survive every refresh. A full-row upsert would wipe them
-- hourly, after which alerts emit an empty agency_id and vehicle search loses
-- its key.
INSERT INTO regions (
  id, region_name, oba_base_url, sidecar_base_url, language, active,
  latitude, longitude, synced_at, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (id) DO UPDATE SET
  region_name      = excluded.region_name,
  oba_base_url     = excluded.oba_base_url,
  sidecar_base_url = excluded.sidecar_base_url,
  language         = excluded.language,
  active           = excluded.active,
  latitude         = excluded.latitude,
  longitude        = excluded.longitude,
  synced_at        = excluded.synced_at,
  updated_at       = excluded.updated_at;

-- name: SetRegionLocalFields :exec
UPDATE regions
SET default_agency_id = ?, timezone = ?, oba_api_key = ?, updated_at = ?
WHERE id = ?;
