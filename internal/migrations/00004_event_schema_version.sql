-- +goose Up
ALTER TABLE events ADD COLUMN schema_version int;

-- +goose Down
ALTER TABLE events DROP COLUMN IF EXISTS schema_version;
