-- name: CreateWorkspace :one
INSERT INTO workspaces (id, name, policy_mode)
VALUES ($1, $2, $3)
RETURNING id, name, policy_mode, created_at;

-- name: GetWorkspace :one
SELECT id, name, policy_mode, created_at
FROM workspaces
WHERE id = $1;

-- name: GetWorkspacePolicyMode :one
SELECT policy_mode FROM workspaces WHERE id = $1;

-- name: SetWorkspacePolicyMode :exec
UPDATE workspaces SET policy_mode = $2 WHERE id = $1;
