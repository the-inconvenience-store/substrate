-- name: LockCollection :one
SELECT id, workspace_id, level, active_schema_version
FROM collections
WHERE id = $1
FOR UPDATE;

-- name: NextSchemaVersion :one
SELECT COALESCE(MAX(version), 0) + 1 AS next
FROM schemas
WHERE collection_id = $1;

-- name: InsertSchema :one
INSERT INTO schemas (id, collection_id, workspace_id, version, json_schema, lifecycle, indexed_fields, rationale, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING id, collection_id, version, lifecycle, indexed_fields, created_at, created_by;

-- name: GetSchema :one
SELECT id, collection_id, version, json_schema, lifecycle, indexed_fields, rationale, created_at, created_by
FROM schemas
WHERE collection_id = $1 AND version = $2;

-- name: ListSchemas :many
SELECT version, lifecycle, indexed_fields, created_at, created_by
FROM schemas
WHERE collection_id = $1
ORDER BY version ASC;

-- name: GetActiveSchema :one
SELECT s.version, s.json_schema
FROM schemas s
JOIN collections c ON c.id = s.collection_id AND c.active_schema_version = s.version
WHERE c.id = $1;

-- name: SetSchemaLifecycle :exec
UPDATE schemas SET lifecycle = $3
WHERE collection_id = $1 AND version = $2;

-- name: SetCollectionActiveVersion :exec
UPDATE collections SET active_schema_version = $2, level = 'typed', updated_at = now()
WHERE id = $1;
