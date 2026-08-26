-- +goose Up
-- +goose StatementBegin

ALTER TABLE deployments
    ADD COLUMN schedule_claim_token text;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE deployments
    DROP COLUMN IF EXISTS schedule_claim_token;

-- +goose StatementEnd
