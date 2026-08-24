-- +goose Up
-- +goose StatementBegin

-- Webhook endpoints are Workspace-owned management resources. Signing secrets
-- use the same authenticated keyring envelope as Vault credentials but have an
-- independent AAD namespace in the application layer.
CREATE TABLE webhooks (
    id                    text        PRIMARY KEY,
    workspace_id          text        NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    url                   text        NOT NULL,
    event_types           text[]      NOT NULL,
    status                text        NOT NULL CHECK (status IN ('enabled', 'disabled')),
    disabled_reason       text,
    secret_version        integer     NOT NULL,
    secret_algorithm      text        NOT NULL,
    secret_key_id         text        NOT NULL,
    secret_nonce          bytea       NOT NULL,
    secret_ciphertext     bytea       NOT NULL,
    failure_started_at    timestamptz,
    last_success_at       timestamptz,
    created_at            timestamptz NOT NULL,
    updated_at            timestamptz NOT NULL,
    CHECK (cardinality(event_types) BETWEEN 1 AND 64),
    CHECK (status = 'disabled' OR disabled_reason IS NULL)
);

CREATE INDEX webhooks_workspace_list_idx
    ON webhooks (workspace_id, created_at DESC, id DESC);

-- One logical event is shared by every endpoint that was subscribed at the
-- instant the source transaction committed. The serialized payload is stored
-- once so every retry signs and sends identical bytes.
CREATE TABLE webhook_events (
    id            text        PRIMARY KEY,
    workspace_id  text        NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    event_type    text        NOT NULL,
    resource_id   text        NOT NULL,
    payload       bytea       NOT NULL,
    created_at    timestamptz NOT NULL
);

CREATE INDEX webhook_events_retention_idx ON webhook_events (created_at);

-- Delivery is at-least-once. Claims are leased because unlike Temporal wakeups
-- an HTTP callback is not naturally deduplicated by the receiver. A stable
-- event ID remains the consumer's final idempotency boundary if a worker loses
-- acknowledgement after the receiver accepted the request.
CREATE TABLE webhook_deliveries (
    webhook_id          text        NOT NULL REFERENCES webhooks (id) ON DELETE CASCADE,
    event_id            text        NOT NULL REFERENCES webhook_events (id) ON DELETE CASCADE,
    state               text        NOT NULL DEFAULT 'pending'
                                     CHECK (state IN ('pending', 'succeeded', 'failed')),
    attempt_count       integer     NOT NULL DEFAULT 0 CHECK (attempt_count BETWEEN 0 AND 3),
    next_attempt_at     timestamptz NOT NULL,
    claimed_at          timestamptz,
    claim_id            text,
    last_attempt_at     timestamptz,
    delivered_at        timestamptz,
    response_status     integer,
    last_error          text,
    created_at          timestamptz NOT NULL,
    PRIMARY KEY (webhook_id, event_id),
    CHECK ((claimed_at IS NULL) = (claim_id IS NULL)),
    CHECK (state = 'pending' OR claim_id IS NULL)
);

CREATE INDEX webhook_deliveries_due_idx
    ON webhook_deliveries (next_attempt_at, created_at, webhook_id, event_id)
    WHERE state = 'pending';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS webhook_deliveries;
DROP TABLE IF EXISTS webhook_events;
DROP TABLE IF EXISTS webhooks;
-- +goose StatementEnd
