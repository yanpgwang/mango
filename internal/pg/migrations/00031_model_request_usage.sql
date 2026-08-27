-- +goose Up
-- +goose StatementBegin

-- Provider usage is billed per completed model request, not per public turn.
-- The request event id is stable in Temporal history and makes Activity retries
-- idempotent while Session and Thread projections are updated atomically.
CREATE TABLE model_request_usage (
    session_id          text        NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
	thread_id           text        NOT NULL,
	request_event_id    text        NOT NULL,
	model_id            text        NOT NULL,
	stop_reason         text        NOT NULL DEFAULT '',
    usage               jsonb       NOT NULL,
    list_cost_nano_usd  bigint,
    created_at          timestamptz NOT NULL,
    PRIMARY KEY (session_id, request_event_id),
    FOREIGN KEY (session_id, thread_id)
        REFERENCES session_threads (session_id, id) ON DELETE CASCADE
);

CREATE INDEX model_request_usage_thread_idx
    ON model_request_usage (session_id, thread_id, created_at);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS model_request_usage;

-- +goose StatementEnd
