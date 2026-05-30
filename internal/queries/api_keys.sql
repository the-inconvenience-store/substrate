-- name: CreateAPIKey :one
INSERT INTO api_keys (id, workspace_id, hash, label)
VALUES ($1, $2, $3, $4)
RETURNING id;

-- name: GetWorkspaceIDByAPIKeyHash :one
SELECT workspace_id
FROM api_keys
WHERE hash = $1 AND revoked_at IS NULL;
