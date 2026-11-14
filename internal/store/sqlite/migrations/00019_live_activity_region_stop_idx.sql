-- +goose Up
-- The registration caps (SC-03) count a region's distinct stops and the
-- rows on one stop on every new registration; neither existing index
-- (region_id, activity_id) / (push_token) serves those.
CREATE INDEX live_activities_region_stop_idx ON live_activities (region_id, stop_id);

-- +goose Down
DROP INDEX live_activities_region_stop_idx;
