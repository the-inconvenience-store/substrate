-- +goose Up
CREATE TABLE policies (
    id            uuid PRIMARY KEY,
    workspace_id  uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    actor         text NOT NULL DEFAULT '*',
    collection_id uuid REFERENCES collections(id) ON DELETE CASCADE,  -- NULL = any collection
    operation     text NOT NULL DEFAULT '*',
    effect        text NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX policies_ws_idx ON policies(workspace_id);

-- +goose Down
DROP TABLE IF EXISTS policies;
