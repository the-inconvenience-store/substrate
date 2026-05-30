-- +goose Up
CREATE TABLE workspaces (
    id          uuid PRIMARY KEY,
    name        text NOT NULL,
    policy_mode text NOT NULL DEFAULT 'allow',
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE api_keys (
    id           uuid PRIMARY KEY,
    workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    hash         bytea NOT NULL,
    label        text,
    created_at   timestamptz NOT NULL DEFAULT now(),
    revoked_at   timestamptz
);
CREATE UNIQUE INDEX api_keys_hash_idx ON api_keys(hash);

CREATE TABLE collections (
    id                    uuid PRIMARY KEY,
    workspace_id          uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    name                  text NOT NULL,
    level                 text NOT NULL DEFAULT 'flexible',
    active_schema_version int,
    created_at            timestamptz NOT NULL DEFAULT now(),
    updated_at            timestamptz NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, name)
);

CREATE TABLE records (
    id             uuid NOT NULL,
    collection_id  uuid NOT NULL REFERENCES collections(id) ON DELETE CASCADE,
    workspace_id   uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    schema_version int,
    data           jsonb NOT NULL,
    revision       bigint NOT NULL,
    status         text NOT NULL DEFAULT 'active',
    actor          text,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (collection_id, id)
);

CREATE TABLE events (
    seq             bigserial PRIMARY KEY,
    id              uuid NOT NULL,
    workspace_id    uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    collection_id   uuid NOT NULL REFERENCES collections(id) ON DELETE CASCADE,
    record_id       uuid NOT NULL,
    type            text NOT NULL,
    revision        bigint NOT NULL,
    state_after     jsonb,
    actor           text,
    trace           jsonb,
    idempotency_key text,
    created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX events_record_idx ON events(collection_id, record_id, seq);
CREATE UNIQUE INDEX events_idempotency_idx
    ON events(workspace_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;

-- +goose Down
DROP TABLE IF EXISTS events;
DROP TABLE IF EXISTS records;
DROP TABLE IF EXISTS collections;
DROP TABLE IF EXISTS api_keys;
DROP TABLE IF EXISTS workspaces;
