-- +goose Up
-- +goose StatementBegin

-- Approval authorizes external execution; only its later result resolves the
-- original action. Both receipts retain the same public tool-use reference.
ALTER TABLE pending_actions
    ADD COLUMN approval_event_id text REFERENCES events (id),
    ADD CONSTRAINT pending_actions_approval_unique UNIQUE (session_id, approval_event_id),
    ADD CONSTRAINT pending_actions_approval_kind CHECK (
        approval_event_id IS NULL OR kind = 'tool_result'
    );

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

ALTER TABLE pending_actions
    DROP CONSTRAINT pending_actions_approval_kind,
    DROP CONSTRAINT pending_actions_approval_unique,
    DROP COLUMN approval_event_id;

-- +goose StatementEnd
