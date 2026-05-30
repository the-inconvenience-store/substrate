-- name: AppendEvent :exec
INSERT INTO events (id, workspace_id, collection_id, record_id, type, revision, state_after, actor, idempotency_key)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9);

-- name: GetReplayEvent :one
SELECT record_id, collection_id, revision, state_after, actor, type
FROM events
WHERE workspace_id = $1 AND idempotency_key = $2
ORDER BY seq DESC
LIMIT 1;
