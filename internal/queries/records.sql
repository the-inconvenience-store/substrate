-- name: InsertRecord :exec
INSERT INTO records (id, collection_id, workspace_id, data, revision, status, actor, schema_version)
VALUES ($1, $2, $3, $4, $5, 'active', $6, $7);

-- name: GetActiveRecord :one
SELECT id, collection_id, data, revision, status, actor
FROM records
WHERE workspace_id = $1 AND collection_id = $2 AND id = $3 AND status = 'active';

-- name: GetRecordRevisionForUpdate :one
SELECT revision
FROM records
WHERE workspace_id = $1 AND collection_id = $2 AND id = $3 AND status = 'active'
FOR UPDATE;

-- name: GetRecordForUpdate :one
SELECT revision, data
FROM records
WHERE workspace_id = $1 AND collection_id = $2 AND id = $3 AND status = 'active'
FOR UPDATE;

-- name: UpdateRecordData :exec
UPDATE records SET data = $4, revision = $5, actor = $6, schema_version = $7, updated_at = now()
WHERE workspace_id = $1 AND collection_id = $2 AND id = $3;

-- name: SoftDeleteRecord :exec
UPDATE records SET status = 'deleted', revision = $4, updated_at = now()
WHERE workspace_id = $1 AND collection_id = $2 AND id = $3;

-- name: GetAnyRecordRevisionForUpdate :one
SELECT revision
FROM records
WHERE workspace_id = $1 AND collection_id = $2 AND id = $3
FOR UPDATE;

-- name: RevertRecordData :exec
UPDATE records SET data = $4, revision = $5, status = 'active', updated_at = now()
WHERE workspace_id = $1 AND collection_id = $2 AND id = $3;
