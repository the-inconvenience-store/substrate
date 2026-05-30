-- name: CreateWorkspace :one
INSERT INTO workspaces (id, name, policy_mode)
VALUES ($1, $2, $3)
RETURNING id, name, policy_mode, created_at;

-- name: GetWorkspace :one
SELECT id, name, policy_mode, created_at
FROM workspaces
WHERE id = $1;
