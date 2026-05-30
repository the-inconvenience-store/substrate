-- +goose Up
CREATE TABLE schemas (
    id             uuid PRIMARY KEY,
    collection_id  uuid NOT NULL REFERENCES collections(id) ON DELETE CASCADE,
    workspace_id   uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    version        int NOT NULL,
    json_schema    jsonb NOT NULL,
    lifecycle      text NOT NULL DEFAULT 'draft',
    indexed_fields jsonb NOT NULL DEFAULT '[]',
    rationale      text,
    created_by     text,
    created_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (collection_id, version)
);

-- +goose Down
DROP TABLE IF EXISTS schemas;
