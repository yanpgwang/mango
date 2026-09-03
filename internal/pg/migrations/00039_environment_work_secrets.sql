-- +goose Up
-- +goose StatementBegin

ALTER TABLE environment_work
    ADD COLUMN sessions_token_hash bytea;

-- A healthy worker renews continuously. Bound the maximum stale-owner window
-- and keep PostgreSQL interval arithmetic within a small, predictable range.
UPDATE environment_work
SET ttl_seconds = 300
WHERE ttl_seconds > 300;

ALTER TABLE environment_work
    ADD CONSTRAINT environment_work_ttl_seconds_bounded
    CHECK (ttl_seconds BETWEEN 1 AND 300);

CREATE UNIQUE INDEX environment_work_sessions_token_hash_idx
    ON environment_work (sessions_token_hash)
    WHERE sessions_token_hash IS NOT NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS environment_work_sessions_token_hash_idx;

ALTER TABLE environment_work
    DROP CONSTRAINT IF EXISTS environment_work_ttl_seconds_bounded;

ALTER TABLE environment_work
    DROP COLUMN IF EXISTS sessions_token_hash;

-- +goose StatementEnd
