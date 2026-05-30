-- name: InsertPolicy :one
INSERT INTO policies (id, workspace_id, actor, collection_id, operation, effect)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id, workspace_id, actor, collection_id, operation, effect, created_at;

-- name: ListPolicies :many
SELECT p.id, p.actor, p.collection_id, c.name AS collection_name, p.operation, p.effect, p.created_at
FROM policies p
LEFT JOIN collections c ON c.id = p.collection_id
WHERE p.workspace_id = $1
ORDER BY p.created_at ASC, p.id ASC;

-- name: ListPoliciesForRequest :many
SELECT id, actor, collection_id, operation, effect
FROM policies
WHERE workspace_id = $1
  AND (collection_id = $2 OR collection_id IS NULL);

-- name: DeletePolicy :execrows
DELETE FROM policies WHERE id = $1 AND workspace_id = $2;
