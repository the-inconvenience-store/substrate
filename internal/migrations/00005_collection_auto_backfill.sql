-- +goose Up
ALTER TABLE collections ADD COLUMN auto_backfill boolean NOT NULL DEFAULT false;

-- +goose Down
ALTER TABLE collections DROP COLUMN IF EXISTS auto_backfill;
