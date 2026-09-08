-- +goose Up
-- +goose StatementBegin

-- Files use a durable intent state around object-store I/O. Only ready rows are
-- public. Uploading and deleting rows survive API-process crashes so startup
-- reconciliation can remove the object and the intent deterministically.
CREATE TABLE files (
    id              text        PRIMARY KEY,
    created_at      timestamptz NOT NULL,
    updated_at      timestamptz NOT NULL,
    filename        text        NOT NULL,
    mime_type       text        NOT NULL,
    size_bytes      bigint      NOT NULL DEFAULT 0 CHECK (size_bytes >= 0),
    blob_key        text        NOT NULL UNIQUE,
    checksum_sha256 text        NOT NULL DEFAULT '',
    state           text        NOT NULL CHECK (state IN ('uploading', 'ready', 'deleting'))
);

CREATE INDEX files_ready_list_idx
    ON files (created_at DESC, id DESC)
    WHERE state = 'ready';
CREATE INDEX files_incomplete_idx
    ON files (updated_at, id)
    WHERE state <> 'ready';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS files;
-- +goose StatementEnd
