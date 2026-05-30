-- name: AppendEvent :exec
INSERT INTO events (id, workspace_id, collection_id, record_id, type, revision, state_after, actor, idempotency_key)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9);

-- name: GetReplayEvent :one
SELECT record_id, collection_id, revision, state_after, actor, type
FROM events
WHERE workspace_id = $1 AND idempotency_key = $2
ORDER BY seq DESC
LIMIT 1;

-- name: ListRecordEvents :many
SELECT revision, type, actor, state_after, created_at
FROM events
WHERE workspace_id = $1 AND collection_id = $2 AND record_id = $3
ORDER BY seq ASC;

-- name: GetStateAtRevision :one
SELECT state_after, revision, type
FROM events
WHERE workspace_id = $1 AND collection_id = $2 AND record_id = $3 AND revision <= $4
ORDER BY seq DESC
LIMIT 1;

-- name: GetStateAtEvent :one
SELECT state_after, revision, type
FROM events e
WHERE e.workspace_id = $1 AND e.collection_id = $2 AND e.record_id = $3
  AND e.seq <= (SELECT sub.seq FROM events sub WHERE sub.id = $4)
ORDER BY e.seq DESC
LIMIT 1;

-- name: GetStateAtTimestamp :one
SELECT state_after, revision, type
FROM events
WHERE workspace_id = $1 AND collection_id = $2 AND record_id = $3 AND created_at <= $4
ORDER BY seq DESC
LIMIT 1;
