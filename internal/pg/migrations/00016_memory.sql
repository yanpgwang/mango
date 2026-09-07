-- +goose Up
-- +goose StatementBegin

CREATE TABLE memory_stores (
    id          text        PRIMARY KEY,
    name        text        NOT NULL,
    description text        NOT NULL DEFAULT '',
    metadata    jsonb       NOT NULL DEFAULT '{}'::jsonb,
    created_at  timestamptz NOT NULL,
    updated_at  timestamptz NOT NULL,
    archived_at timestamptz
);

CREATE INDEX memory_stores_list_idx
    ON memory_stores (created_at DESC, id DESC);

CREATE TABLE memories (
    id                 text        PRIMARY KEY,
    memory_store_id    text        NOT NULL REFERENCES memory_stores (id) ON DELETE CASCADE,
    memory_version_id  text        NOT NULL,
    path               text        NOT NULL,
    content            text        NOT NULL,
    content_size_bytes bigint      NOT NULL,
    content_sha256     text        NOT NULL,
    created_at         timestamptz NOT NULL,
    updated_at         timestamptz NOT NULL,
    UNIQUE (memory_store_id, path)
);

CREATE INDEX memories_store_path_idx
    ON memories (memory_store_id, path, id);

CREATE TABLE memory_versions (
    id                 text        PRIMARY KEY,
    memory_store_id    text        NOT NULL REFERENCES memory_stores (id) ON DELETE CASCADE,
    memory_id          text        NOT NULL,
    operation          text        NOT NULL CHECK (operation IN ('created', 'modified', 'deleted')),
    path               text,
    content            text,
    content_size_bytes bigint,
    content_sha256     text,
    created_at         timestamptz NOT NULL,
    created_by_type    text        NOT NULL CHECK (created_by_type IN ('api_actor', 'session_actor', 'user_actor')),
    created_by_id      text        NOT NULL,
    redacted_at        timestamptz,
    redacted_by_type   text,
    redacted_by_id     text,
    CHECK ((redacted_by_type IS NULL) = (redacted_by_id IS NULL)),
    CHECK (redacted_by_type IS NULL OR redacted_by_type IN ('api_actor', 'session_actor', 'user_actor'))
);

CREATE INDEX memory_versions_store_list_idx
    ON memory_versions (memory_store_id, created_at DESC, id DESC);
CREATE INDEX memory_versions_memory_list_idx
    ON memory_versions (memory_store_id, memory_id, created_at DESC, id DESC);

CREATE TABLE session_resources (
    id                       text        PRIMARY KEY,
    session_id               text        NOT NULL REFERENCES sessions (id),
    resource_type            text        NOT NULL CHECK (resource_type = 'memory_store'),
    memory_store_id          text        NOT NULL REFERENCES memory_stores (id) ON DELETE RESTRICT,
    memory_access            text        NOT NULL CHECK (memory_access IN ('read_write', 'read_only')),
    memory_instructions      text        NOT NULL,
    memory_store_name        text        NOT NULL,
    memory_store_description text        NOT NULL,
    mount_path               text        NOT NULL,
    state                    text        NOT NULL CHECK (state IN ('active', 'deleting')),
    created_at               timestamptz NOT NULL,
    updated_at               timestamptz NOT NULL
);

CREATE INDEX session_resources_active_list_idx
    ON session_resources (session_id, created_at, id)
    WHERE state = 'active';
CREATE INDEX session_resources_deleting_idx
    ON session_resources (session_id, updated_at, id)
    WHERE state = 'deleting';
CREATE INDEX session_resources_session_idx
    ON session_resources (session_id);
CREATE UNIQUE INDEX session_resources_active_mount_idx
    ON session_resources (session_id, mount_path)
    WHERE state = 'active';

CREATE UNIQUE INDEX session_resources_memory_store_idx
    ON session_resources (session_id, memory_store_id)
    WHERE resource_type = 'memory_store';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS session_resources;
DROP TABLE IF EXISTS memory_versions;
DROP TABLE IF EXISTS memories;
DROP TABLE IF EXISTS memory_stores;
-- +goose StatementEnd
