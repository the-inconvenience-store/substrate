-- name: CreateCollection :one
INSERT INTO collections (id, workspace_id, name, level)
VALUES ($1, $2, $3, $4)
RETURNING id, workspace_id, name, level;

-- name: GetCollectionByName :one
SELECT id, workspace_id, name, level
FROM collections
WHERE workspace_id = $1 AND name = $2;
