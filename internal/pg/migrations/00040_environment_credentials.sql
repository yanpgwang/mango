-- +goose Up
-- +goose StatementBegin
ALTER TABLE api_keys ADD COLUMN environment_id text;
ALTER TABLE api_keys ADD CONSTRAINT api_keys_environment_workspace_fk
    FOREIGN KEY (environment_id, workspace_id)
    REFERENCES environments (id, workspace_id) ON DELETE CASCADE;
CREATE INDEX api_keys_environment_idx ON api_keys (environment_id)
    WHERE environment_id IS NOT NULL;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Never promote scoped credentials to Workspace administrators on rollback.
DELETE FROM api_keys WHERE environment_id IS NOT NULL;
ALTER TABLE api_keys DROP COLUMN environment_id;
-- +goose StatementEnd
