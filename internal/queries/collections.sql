-- name: CreateCollection :one
INSERT INTO collections (id, workspace_id, name, level)
VALUES ($1, $2, $3, $4)
RETURNING id, workspace_id, name, level;

-- name: GetCollectionByName :one
SELECT id, workspace_id, name, level, auto_backfill
FROM collections
WHERE workspace_id = $1 AND name = $2;

-- name: SetAutoBackfill :exec
UPDATE collections SET auto_backfill = $3, updated_at = now()
WHERE workspace_id = $1 AND id = $2;

-- name: GetCollectionAutoBackfill :one
SELECT auto_backfill FROM collections WHERE id = $1;
