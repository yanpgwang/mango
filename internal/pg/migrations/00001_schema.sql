-- +goose Up
-- +goose StatementBegin

-- Current development schema. Historical pre-release upgrades and data backfills
-- are intentionally squashed; initialize a fresh development database.
-- Keep this schema as the source of truth for both PostgreSQL and sqlc.

-- Workspace ownership and credentials.

CREATE TABLE workspaces (
    id text NOT NULL,
    name text NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    CONSTRAINT workspaces_pkey PRIMARY KEY (id)
);

CREATE TABLE api_keys (
    id text NOT NULL,
    workspace_id text NOT NULL,
    secret_hash bytea NOT NULL,
    label text NOT NULL,
    created_at timestamptz NOT NULL,
    revoked_at timestamptz,
    last_used_at timestamptz,
    environment_id text,
    CONSTRAINT api_keys_pkey PRIMARY KEY (id),
    CONSTRAINT api_keys_secret_hash_key UNIQUE (secret_hash)
);

CREATE INDEX api_keys_environment_idx ON api_keys USING btree (environment_id) WHERE (environment_id IS NOT NULL);

CREATE INDEX api_keys_workspace_idx ON api_keys USING btree (workspace_id, created_at, id);

-- Agent definitions and Environments.

CREATE TABLE agents (
    id text NOT NULL,
    version integer NOT NULL,
    name text NOT NULL,
    body jsonb NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    archived_at timestamptz,
    workspace_id text NOT NULL,
    CONSTRAINT agents_version_check CHECK ((version > 0)),
    CONSTRAINT agents_pkey PRIMARY KEY (id, version),
    CONSTRAINT agents_workspace_identity_unique UNIQUE (id, version, workspace_id)
);

CREATE INDEX agents_latest_idx ON agents USING btree (id, version DESC);

CREATE INDEX agents_workspace_list_idx ON agents USING btree (workspace_id, created_at DESC, id, version DESC);

CREATE TABLE environments (
    id text NOT NULL,
    name text NOT NULL,
    config_type text NOT NULL,
    body jsonb NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    archived_at timestamptz,
    workspace_id text NOT NULL,
    CONSTRAINT environments_pkey PRIMARY KEY (id),
    CONSTRAINT environments_workspace_identity_unique UNIQUE (id, workspace_id)
);

CREATE INDEX environments_workspace_list_idx ON environments USING btree (workspace_id, created_at DESC, id DESC);

-- Sessions, Threads, events, and durable execution.

CREATE TABLE sessions (
    id text NOT NULL,
    status text NOT NULL,
    body jsonb NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    agent_id text,
    agent_version integer,
    environment_id text,
    archived_at timestamptz,
    deleting_at timestamptz,
    deployment_id text,
    workspace_id text NOT NULL,
    CONSTRAINT sessions_pkey PRIMARY KEY (id)
);

CREATE INDEX sessions_active_idx ON sessions USING btree (created_at, id) WHERE (archived_at IS NULL);

CREATE INDEX sessions_agent_idx ON sessions USING btree (agent_id, agent_version, created_at, id);

CREATE INDEX sessions_deployment_idx ON sessions USING btree (deployment_id, created_at, id) WHERE (deployment_id IS NOT NULL);

CREATE INDEX sessions_status_idx ON sessions USING btree (status, created_at, id);

CREATE INDEX sessions_workspace_list_idx ON sessions USING btree (workspace_id, created_at DESC, id DESC);

CREATE TABLE session_threads (
    id text NOT NULL,
    session_id text NOT NULL,
    parent_thread_id text,
    kind text NOT NULL,
    created_at timestamptz NOT NULL,
    archived_at timestamptz,
    status text NOT NULL,
    body jsonb NOT NULL,
    updated_at timestamptz NOT NULL,
    CONSTRAINT session_threads_kind_check CHECK ((kind = ANY (ARRAY['primary'::text, 'child'::text, 'advisor'::text]))),
    CONSTRAINT session_threads_parent_check CHECK (
        (((kind = 'primary'::text)
        AND (parent_thread_id IS NULL))
        OR ((kind = ANY (ARRAY['child'::text, 'advisor'::text]))
        AND (parent_thread_id IS NOT NULL)))),
    CONSTRAINT session_threads_status_check CHECK ((status = ANY (ARRAY['idle'::text, 'running'::text, 'rescheduling'::text, 'terminated'::text]))),
    CONSTRAINT session_threads_pkey PRIMARY KEY (id),
    CONSTRAINT session_threads_session_id_id_key UNIQUE (session_id, id)
);

CREATE INDEX session_threads_order_idx ON session_threads USING btree (session_id, kind, created_at, id);

CREATE UNIQUE INDEX session_threads_primary_idx ON session_threads USING btree (session_id) WHERE (kind = 'primary'::text);

COMMENT ON COLUMN session_threads.kind IS 'primary and persistent child Workflows, plus automatically terminating advisor consultations';

COMMENT ON COLUMN session_threads.archived_at IS 'Independent Thread archive time; primary archive currently follows Session archive';

COMMENT ON COLUMN session_threads.body IS 'Authoritative per-Thread agent, usage, and timing projection';

CREATE TABLE events (
    id text NOT NULL,
    session_id text NOT NULL,
    seq bigint NOT NULL,
    type text NOT NULL,
    payload jsonb NOT NULL,
    turn_event_id text,
    created_at timestamptz NOT NULL,
    processed_at timestamptz,
    thread_id text NOT NULL,
    CONSTRAINT events_pkey PRIMARY KEY (id),
    CONSTRAINT events_session_id_seq_key UNIQUE (session_id, seq),
    CONSTRAINT events_session_thread_id_key UNIQUE (session_id, thread_id, id)
);

CREATE INDEX events_processed_idx ON events USING btree (session_id, processed_at, seq);

CREATE INDEX events_session_seq_idx ON events USING btree (session_id, seq);

CREATE INDEX events_thread_seq_idx ON events USING btree (session_id, thread_id, seq);

CREATE INDEX events_turn_idx ON events USING btree (session_id, turn_event_id) WHERE (turn_event_id IS NOT NULL);

COMMENT ON COLUMN events.thread_id IS 'Owning Session Thread ledger; Session-wide seq remains the total-order cursor';

CREATE TABLE orchestration_outbox (
    session_id text NOT NULL,
    max_event_seq bigint NOT NULL,
    enqueued_at timestamptz NOT NULL,
    attempts integer DEFAULT 0 NOT NULL,
    last_attempt_at timestamptz,
    last_error text,
    CONSTRAINT orchestration_outbox_pkey PRIMARY KEY (session_id)
);

CREATE INDEX orchestration_outbox_enqueued_idx ON orchestration_outbox USING btree (enqueued_at);

CREATE TABLE thread_orchestration_outbox (
    session_id text NOT NULL,
    thread_id text NOT NULL,
    max_event_seq bigint NOT NULL,
    enqueued_at timestamptz NOT NULL,
    attempts integer DEFAULT 0 NOT NULL,
    last_attempt_at timestamptz,
    last_error text,
    intent text DEFAULT 'wake'::text NOT NULL,
    CONSTRAINT thread_orchestration_outbox_intent_check CHECK ((intent = ANY (ARRAY['wake'::text, 'terminate'::text]))),
    CONSTRAINT thread_orchestration_outbox_pkey PRIMARY KEY (session_id, thread_id)
);

CREATE INDEX thread_orchestration_outbox_enqueued_idx ON thread_orchestration_outbox USING btree (enqueued_at);

CREATE TABLE turn_attempts (
    id text NOT NULL,
    session_id text NOT NULL,
    trigger_event_id text NOT NULL,
    attempt_no integer NOT NULL,
    state text NOT NULL,
    error text,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    finished_at timestamptz,
    CONSTRAINT turn_attempts_attempt_no_check CHECK ((attempt_no > 0)),
    CONSTRAINT turn_attempts_state_check CHECK ((state = ANY (ARRAY['active'::text, 'completed'::text, 'failed'::text, 'interrupted'::text]))),
    CONSTRAINT turn_attempts_pkey PRIMARY KEY (id),
    CONSTRAINT turn_attempts_session_id_trigger_event_id_attempt_no_key UNIQUE (session_id, trigger_event_id, attempt_no)
);

CREATE UNIQUE INDEX turn_attempts_one_active ON turn_attempts USING btree (session_id, trigger_event_id) WHERE (state = 'active'::text);

CREATE TABLE tool_steps (
    id text NOT NULL,
    attempt_id text NOT NULL,
    ordinal integer NOT NULL,
    tool_use_event_id text NOT NULL,
    tool_name text NOT NULL,
    input jsonb NOT NULL,
    state text NOT NULL,
    result jsonb,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    started_at timestamptz,
    finished_at timestamptz,
    CONSTRAINT tool_steps_ordinal_check CHECK ((ordinal >= 0)),
    CONSTRAINT tool_steps_state_check CHECK ((state = ANY (ARRAY['prepared'::text, 'started'::text, 'completed'::text, 'ambiguous'::text]))),
    CONSTRAINT tool_steps_attempt_id_ordinal_key UNIQUE (attempt_id, ordinal),
    CONSTRAINT tool_steps_pkey PRIMARY KEY (id),
    CONSTRAINT tool_steps_tool_use_event_id_key UNIQUE (tool_use_event_id)
);

CREATE INDEX tool_steps_attempt_idx ON tool_steps USING btree (attempt_id, ordinal);

CREATE TABLE pending_actions (
    id text NOT NULL,
    session_id text NOT NULL,
    action_event_id text NOT NULL,
    kind text NOT NULL,
    resolving_event_id text,
    created_at timestamptz NOT NULL,
    resolved_at timestamptz,
    thread_id text NOT NULL,
    client_action_event_id text NOT NULL,
    approval_event_id text,
    CONSTRAINT pending_actions_approval_kind CHECK (((approval_event_id IS NULL) OR (kind = 'tool_result'::text))),
    CONSTRAINT pending_actions_check CHECK (((resolved_at IS NULL) OR (resolving_event_id IS NOT NULL))),
    CONSTRAINT pending_actions_kind_check CHECK ((kind = ANY (ARRAY['custom_tool_result'::text, 'tool_confirmation'::text, 'tool_result'::text]))),
    CONSTRAINT pending_actions_approval_unique UNIQUE (session_id, approval_event_id),
    CONSTRAINT pending_actions_client_event_unique UNIQUE (session_id, client_action_event_id),
    CONSTRAINT pending_actions_pkey PRIMARY KEY (id),
    CONSTRAINT pending_actions_session_id_action_event_id_key UNIQUE (session_id, action_event_id),
    CONSTRAINT pending_actions_session_id_resolving_event_id_key UNIQUE (session_id, resolving_event_id)
);

CREATE INDEX pending_actions_unresolved_idx ON pending_actions USING btree (session_id, thread_id, created_at, id) WHERE (resolved_at IS NULL);

-- Model transcripts, context, usage, and MCP discovery.

CREATE TABLE provider_transcript_turns (
    session_id text NOT NULL,
    trigger_event_id text NOT NULL,
    turn_ordinal bigint GENERATED ALWAYS AS IDENTITY NOT NULL,
    committed_through_seq bigint NOT NULL,
    represented_event_ids jsonb NOT NULL,
    messages jsonb NOT NULL,
    tool_use_mappings jsonb DEFAULT '[]'::jsonb NOT NULL,
    created_at timestamptz NOT NULL,
    CONSTRAINT provider_transcript_turns_pkey PRIMARY KEY (session_id, trigger_event_id)
);

CREATE INDEX provider_transcript_turns_order_idx ON provider_transcript_turns USING btree (session_id, turn_ordinal);

CREATE TABLE thread_context_snapshots (
    id text NOT NULL,
    session_id text NOT NULL,
    thread_id text NOT NULL,
    trigger_event_id text NOT NULL,
    parent_snapshot_id text,
    snapshot_ordinal bigint GENERATED ALWAYS AS IDENTITY NOT NULL,
    transcript_trigger_event_ids jsonb NOT NULL,
    messages jsonb NOT NULL,
    projection jsonb NOT NULL,
    context_policy_version integer NOT NULL,
    created_at timestamptz NOT NULL,
    CONSTRAINT thread_context_snapshots_context_policy_version_check CHECK ((context_policy_version > 0)),
    CONSTRAINT thread_context_snapshots_pkey PRIMARY KEY (id),
    CONSTRAINT thread_context_snapshots_session_id_thread_id_id_key UNIQUE (session_id, thread_id, id),
    CONSTRAINT thread_context_snapshots_session_id_thread_id_trigger_event_key UNIQUE (session_id, thread_id, trigger_event_id)
);

CREATE INDEX thread_context_snapshots_order_idx ON thread_context_snapshots USING btree (session_id, thread_id, snapshot_ordinal DESC);

COMMENT ON TABLE thread_context_snapshots IS 'Private immutable compacted message projections; never a public Session resource';

CREATE TABLE model_request_usage (
    session_id text NOT NULL,
    thread_id text NOT NULL,
    request_event_id text NOT NULL,
    model_id text NOT NULL,
    stop_reason text DEFAULT ''::text NOT NULL,
    usage jsonb NOT NULL,
    list_cost_nano_usd bigint,
    created_at timestamptz NOT NULL,
    CONSTRAINT model_request_usage_pkey PRIMARY KEY (session_id, request_event_id)
);

CREATE INDEX model_request_usage_thread_idx ON model_request_usage USING btree (session_id, thread_id, created_at);

CREATE TABLE mcp_discovery_snapshots (
    session_id text NOT NULL,
    server_name text NOT NULL,
    server_url text NOT NULL,
    tools jsonb NOT NULL,
    created_at timestamptz NOT NULL,
    thread_id text NOT NULL,
    CONSTRAINT mcp_discovery_snapshots_pkey PRIMARY KEY (session_id, thread_id, server_name)
);

-- Files and immutable Skills.

CREATE TABLE files (
    session_id text REFERENCES sessions(id) ON DELETE SET NULL,
    id text NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    filename text NOT NULL,
    mime_type text NOT NULL,
    size_bytes bigint DEFAULT 0 NOT NULL,
    blob_key text NOT NULL,
    checksum_sha256 text DEFAULT ''::text NOT NULL,
    state text NOT NULL,
    workspace_id text NOT NULL,
    CONSTRAINT files_size_bytes_check CHECK ((size_bytes >= 0)),
    CONSTRAINT files_state_check CHECK ((state = ANY (ARRAY['uploading'::text, 'ready'::text, 'deleting'::text]))),
    CONSTRAINT files_blob_key_key UNIQUE (blob_key),
    CONSTRAINT files_pkey PRIMARY KEY (id)
);

CREATE INDEX files_incomplete_idx ON files USING btree (updated_at, id) WHERE (state <> 'ready'::text);

CREATE INDEX files_ready_list_idx ON files USING btree (created_at DESC, id DESC) WHERE (state = 'ready'::text);

CREATE INDEX files_workspace_list_idx ON files USING btree (workspace_id, created_at DESC, id DESC);

CREATE TABLE skills (
    id text NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    display_title text NOT NULL,
    latest_version text,
    source text NOT NULL,
    display_title_explicit boolean DEFAULT false NOT NULL,
    ready boolean DEFAULT false NOT NULL,
    workspace_id text NOT NULL,
    CONSTRAINT skills_source_check CHECK ((source = 'custom'::text)),
    CONSTRAINT skills_pkey PRIMARY KEY (id)
);

CREATE UNIQUE INDEX skills_explicit_display_title_idx ON skills USING btree (lower(display_title)) WHERE display_title_explicit;

CREATE INDEX skills_ready_list_idx ON skills USING btree (created_at DESC, id DESC) WHERE ready;

CREATE INDEX skills_workspace_list_idx ON skills USING btree (workspace_id, created_at DESC, id DESC);

CREATE TABLE skill_versions (
    skill_id text NOT NULL,
    version text NOT NULL,
    created_at timestamptz NOT NULL,
    description text NOT NULL,
    directory text NOT NULL,
    name text NOT NULL,
    blob_key text NOT NULL,
    size_bytes bigint DEFAULT 0 NOT NULL,
    checksum_sha256 text DEFAULT ''::text NOT NULL,
    state text NOT NULL,
    initial boolean DEFAULT false NOT NULL,
    uncompressed_size_bytes bigint NOT NULL,
    CONSTRAINT skill_versions_size_bytes_check CHECK ((size_bytes >= 0)),
    CONSTRAINT skill_versions_state_check CHECK ((state = ANY (ARRAY['uploading'::text, 'ready'::text, 'deleting'::text]))),
    CONSTRAINT skill_versions_uncompressed_size_bytes_check CHECK (((uncompressed_size_bytes >= 0) AND (uncompressed_size_bytes < 30000000))),
    CONSTRAINT skill_versions_version_check CHECK ((version ~ '^[0-9]+$'::text)),
    CONSTRAINT skill_versions_blob_key_key UNIQUE (blob_key),
    CONSTRAINT skill_versions_pkey PRIMARY KEY (skill_id, version)
);

CREATE INDEX skill_versions_incomplete_idx ON skill_versions USING btree (created_at, skill_id, version) WHERE (state <> 'ready'::text);

CREATE INDEX skill_versions_ready_list_idx ON skill_versions USING btree (skill_id, created_at DESC, version DESC) WHERE (state = 'ready'::text);

CREATE TABLE agent_skill_versions (
    agent_id text NOT NULL,
    agent_version integer NOT NULL,
    "position" integer NOT NULL,
    skill_id text NOT NULL,
    skill_version text NOT NULL,
    CONSTRAINT agent_skill_versions_position_check CHECK (("position" >= 0)),
    CONSTRAINT agent_skill_versions_pkey PRIMARY KEY (agent_id, "position")
);

CREATE INDEX agent_skill_versions_version_idx ON agent_skill_versions USING btree (skill_id, skill_version, agent_id);

CREATE TABLE session_skill_versions (
    session_id text NOT NULL,
    "position" integer NOT NULL,
    skill_id text NOT NULL,
    skill_version text NOT NULL,
    agent_id text NOT NULL,
    agent_version integer NOT NULL,
    CONSTRAINT session_skill_versions_position_check CHECK (("position" >= 0)),
    CONSTRAINT session_skill_versions_pkey PRIMARY KEY (session_id, agent_id, agent_version, "position")
);

CREATE INDEX session_skill_versions_version_idx ON session_skill_versions USING btree (skill_id, skill_version, session_id, agent_id, agent_version);

-- Memory and Session resources.

CREATE TABLE memory_stores (
    id text NOT NULL,
    name text NOT NULL,
    description text DEFAULT ''::text NOT NULL,
    metadata jsonb DEFAULT '{}'::jsonb NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    archived_at timestamptz,
    workspace_id text NOT NULL,
    CONSTRAINT memory_stores_pkey PRIMARY KEY (id)
);

CREATE INDEX memory_stores_list_idx ON memory_stores USING btree (created_at DESC, id DESC);

CREATE INDEX memory_stores_workspace_list_idx ON memory_stores USING btree (workspace_id, created_at DESC, id DESC);

CREATE TABLE memories (
    id text NOT NULL,
    memory_store_id text NOT NULL,
    memory_version_id text NOT NULL,
    path text NOT NULL,
    content text NOT NULL,
    content_size_bytes bigint NOT NULL,
    content_sha256 text NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    CONSTRAINT memories_memory_store_id_path_key UNIQUE (memory_store_id, path),
    CONSTRAINT memories_pkey PRIMARY KEY (id)
);

CREATE INDEX memories_store_path_idx ON memories USING btree (memory_store_id, path, id);

CREATE TABLE memory_versions (
    id text NOT NULL,
    memory_store_id text NOT NULL,
    memory_id text NOT NULL,
    operation text NOT NULL,
    path text,
    content text,
    content_size_bytes bigint,
    content_sha256 text,
    created_at timestamptz NOT NULL,
    created_by_type text NOT NULL,
    created_by_id text NOT NULL,
    redacted_at timestamptz,
    redacted_by_type text,
    redacted_by_id text,
    CONSTRAINT memory_versions_check CHECK (((redacted_by_type IS NULL) = (redacted_by_id IS NULL))),
    CONSTRAINT memory_versions_created_by_type_check CHECK ((created_by_type = ANY (ARRAY['api_actor'::text, 'session_actor'::text, 'user_actor'::text]))),
    CONSTRAINT memory_versions_operation_check CHECK ((operation = ANY (ARRAY['created'::text, 'modified'::text, 'deleted'::text]))),
    CONSTRAINT memory_versions_redacted_by_type_check CHECK (
        ((redacted_by_type IS NULL)
        OR (redacted_by_type = ANY (ARRAY['api_actor'::text, 'session_actor'::text, 'user_actor'::text])))),
    CONSTRAINT memory_versions_pkey PRIMARY KEY (id)
);

CREATE INDEX memory_versions_memory_list_idx ON memory_versions USING btree (memory_store_id, memory_id, created_at DESC, id DESC);

CREATE INDEX memory_versions_store_list_idx ON memory_versions USING btree (memory_store_id, created_at DESC, id DESC);

CREATE TABLE session_resources (
    id text NOT NULL,
    session_id text NOT NULL,
    resource_type text NOT NULL,
    memory_store_id text NOT NULL,
    memory_access text NOT NULL,
    memory_instructions text NOT NULL,
    memory_store_name text NOT NULL,
    memory_store_description text NOT NULL,
    mount_path text NOT NULL,
    state text NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    CONSTRAINT session_resources_memory_access_check CHECK ((memory_access = ANY (ARRAY['read_write'::text, 'read_only'::text]))),
    CONSTRAINT session_resources_resource_type_check CHECK ((resource_type = 'memory_store'::text)),
    CONSTRAINT session_resources_state_check CHECK ((state = ANY (ARRAY['active'::text, 'deleting'::text]))),
    CONSTRAINT session_resources_pkey PRIMARY KEY (id)
);

CREATE INDEX session_resources_active_list_idx ON session_resources USING btree (session_id, created_at, id) WHERE (state = 'active'::text);

CREATE UNIQUE INDEX session_resources_active_mount_idx ON session_resources USING btree (session_id, mount_path) WHERE (state = 'active'::text);

CREATE INDEX session_resources_deleting_idx ON session_resources USING btree (session_id, updated_at, id) WHERE (state = 'deleting'::text);

CREATE UNIQUE INDEX session_resources_memory_store_idx ON session_resources USING btree (session_id, memory_store_id) WHERE (resource_type = 'memory_store'::text);

CREATE INDEX session_resources_session_idx ON session_resources USING btree (session_id);

-- Vaults and Session attachments.

CREATE TABLE vaults (
    id text NOT NULL,
    display_name text NOT NULL,
    metadata jsonb DEFAULT '{}'::jsonb NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    archived_at timestamptz,
    workspace_id text NOT NULL,
    CONSTRAINT vaults_pkey PRIMARY KEY (id)
);

CREATE INDEX vaults_list_idx ON vaults USING btree (created_at DESC, id DESC);

CREATE INDEX vaults_workspace_list_idx ON vaults USING btree (workspace_id, created_at DESC, id DESC);

CREATE TABLE vault_credentials (
    id text NOT NULL,
    vault_id text NOT NULL,
    display_name text,
    metadata jsonb DEFAULT '{}'::jsonb NOT NULL,
    auth_type text NOT NULL,
    credential_key text NOT NULL,
    public_auth jsonb NOT NULL,
    secret_version integer,
    secret_algorithm text,
    secret_key_id text,
    secret_nonce bytea,
    secret_ciphertext bytea,
    version bigint DEFAULT 1 NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    archived_at timestamptz,
    CONSTRAINT vault_credentials_auth_type_check CHECK ((auth_type = ANY (ARRAY['mcp_oauth'::text, 'static_bearer'::text]))),
    CONSTRAINT vault_credentials_check CHECK (
        (((archived_at IS NULL)
        AND (secret_version IS NOT NULL)
        AND (secret_algorithm IS NOT NULL)
        AND (secret_key_id IS NOT NULL)
        AND (secret_nonce IS NOT NULL)
        AND (secret_ciphertext IS NOT NULL))
        OR ((archived_at IS NOT NULL)
        AND (secret_version IS NULL)
        AND (secret_algorithm IS NULL)
        AND (secret_key_id IS NULL)
        AND (secret_nonce IS NULL)
        AND (secret_ciphertext IS NULL)))),
    CONSTRAINT vault_credentials_version_check CHECK ((version > 0)),
    CONSTRAINT vault_credentials_pkey PRIMARY KEY (id)
);

CREATE UNIQUE INDEX vault_credentials_active_key_idx ON vault_credentials USING btree (vault_id, credential_key) WHERE (archived_at IS NULL);

CREATE INDEX vault_credentials_list_idx ON vault_credentials USING btree (vault_id, created_at DESC, id DESC);

CREATE TABLE session_vaults (
    session_id text NOT NULL,
    "position" integer NOT NULL,
    vault_id text NOT NULL,
    CONSTRAINT session_vaults_position_check CHECK (("position" >= 0)),
    CONSTRAINT session_vaults_pkey PRIMARY KEY (session_id, "position"),
    CONSTRAINT session_vaults_session_id_vault_id_key UNIQUE (session_id, vault_id)
);

CREATE INDEX session_vaults_vault_idx ON session_vaults USING btree (vault_id);

-- Scheduled Deployments.

CREATE TABLE deployments (
    id text NOT NULL,
    agent_id text NOT NULL,
    agent_version integer NOT NULL,
    environment_id text NOT NULL,
    status text NOT NULL,
    body jsonb NOT NULL,
    next_run_at timestamptz,
    schedule_claimed_at timestamptz,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    archived_at timestamptz,
    workspace_id text NOT NULL,
    schedule_claim_token text,
    CONSTRAINT deployments_status_check CHECK ((status = ANY (ARRAY['active'::text, 'paused'::text]))),
    CONSTRAINT deployments_pkey PRIMARY KEY (id),
    CONSTRAINT deployments_workspace_identity_unique UNIQUE (id, workspace_id)
);

CREATE INDEX deployments_agent_idx ON deployments USING btree (agent_id, created_at, id);

CREATE INDEX deployments_due_idx ON deployments USING btree (next_run_at, id) WHERE ((archived_at IS NULL) AND (status = 'active'::text) AND (next_run_at IS NOT NULL));

CREATE INDEX deployments_list_idx ON deployments USING btree (created_at, id);

CREATE INDEX deployments_workspace_list_idx ON deployments USING btree (workspace_id, created_at DESC, id DESC);

CREATE TABLE deployment_runs (
    id text NOT NULL,
    deployment_id text NOT NULL,
    session_id text,
    error_type text,
    trigger_type text NOT NULL,
    scheduled_at timestamptz,
    body jsonb NOT NULL,
    created_at timestamptz NOT NULL,
    CONSTRAINT deployment_runs_check CHECK (((session_id IS NOT NULL) <> (error_type IS NOT NULL))),
    CONSTRAINT deployment_runs_check1 CHECK (((trigger_type = 'schedule'::text) = (scheduled_at IS NOT NULL))),
    CONSTRAINT deployment_runs_trigger_type_check CHECK ((trigger_type = ANY (ARRAY['manual'::text, 'schedule'::text]))),
    CONSTRAINT deployment_runs_pkey PRIMARY KEY (id)
);

CREATE INDEX deployment_runs_deployment_idx ON deployment_runs USING btree (deployment_id, created_at DESC, id DESC);

CREATE INDEX deployment_runs_list_idx ON deployment_runs USING btree (created_at DESC, id DESC);

CREATE UNIQUE INDEX deployment_runs_scheduled_once_idx ON deployment_runs USING btree (deployment_id, scheduled_at) WHERE (trigger_type = 'schedule'::text);

-- Self-hosted Environment Work and execution healthchecks.

CREATE TABLE environment_work (
    id text NOT NULL,
    environment_id text NOT NULL,
    session_id text,
    activation_seq bigint,
    state text NOT NULL,
    metadata jsonb DEFAULT '{}'::jsonb NOT NULL,
    created_at timestamptz NOT NULL,
    acknowledged_at timestamptz,
    started_at timestamptz,
    latest_heartbeat_at timestamptz,
    ttl_seconds bigint DEFAULT 30 NOT NULL,
    stop_requested_at timestamptz,
    stopped_at timestamptz,
    polled_at timestamptz,
    poll_worker_id text,
    sessions_token_hash bytea,
    work_type text DEFAULT 'session'::text NOT NULL,
    expires_at timestamptz,
    result jsonb,
    CONSTRAINT environment_work_data_shape CHECK (
        (((work_type = 'session'::text)
        AND (session_id IS NOT NULL)
        AND (activation_seq IS NOT NULL)
        AND (expires_at IS NULL)
        AND (result IS NULL))
        OR ((work_type = 'healthcheck'::text)
        AND (session_id IS NULL)
        AND (activation_seq IS NULL)
        AND (expires_at IS NOT NULL)
        AND (((state <> 'stopped'::text)
        AND (result IS NULL))
        OR ((state = 'stopped'::text)
        AND (result IS NOT NULL)
        AND ((result ->> 'status'::text) = ANY (ARRAY['succeeded'::text, 'failed'::text, 'timed_out'::text, 'cancelled'::text]))))))),
    CONSTRAINT environment_work_state_check CHECK ((state = ANY (ARRAY['queued'::text, 'starting'::text, 'active'::text, 'stopping'::text, 'stopped'::text]))),
    CONSTRAINT environment_work_ttl_seconds_bounded CHECK (((ttl_seconds >= 1) AND (ttl_seconds <= 300))),
    CONSTRAINT environment_work_ttl_seconds_check CHECK ((ttl_seconds > 0)),
    CONSTRAINT environment_work_pkey PRIMARY KEY (id)
);

CREATE INDEX environment_work_list_idx ON environment_work USING btree (environment_id, created_at DESC, id DESC);

CREATE UNIQUE INDEX environment_work_live_session_idx ON environment_work USING btree (session_id) WHERE (state = ANY (ARRAY['queued'::text, 'starting'::text, 'active'::text]));

CREATE INDEX environment_work_queue_idx ON environment_work USING btree (environment_id, created_at, id) WHERE (state = 'queued'::text);

CREATE UNIQUE INDEX environment_work_sessions_token_hash_idx ON environment_work USING btree (sessions_token_hash) WHERE (sessions_token_hash IS NOT NULL);

CREATE TABLE environment_work_pollers (
    environment_id text NOT NULL,
    worker_id text NOT NULL,
    polled_at timestamptz NOT NULL,
    CONSTRAINT environment_work_pollers_pkey PRIMARY KEY (environment_id, worker_id)
);

CREATE INDEX environment_work_pollers_recent_idx ON environment_work_pollers USING btree (environment_id, polled_at);

-- Webhook delivery.

CREATE TABLE webhooks (
    id text NOT NULL,
    workspace_id text NOT NULL,
    url text NOT NULL,
    event_types text[] NOT NULL,
    status text NOT NULL,
    disabled_reason text,
    secret_version integer NOT NULL,
    secret_algorithm text NOT NULL,
    secret_key_id text NOT NULL,
    secret_nonce bytea NOT NULL,
    secret_ciphertext bytea NOT NULL,
    failure_started_at timestamptz,
    last_success_at timestamptz,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    CONSTRAINT webhooks_check CHECK (((status = 'disabled'::text) OR (disabled_reason IS NULL))),
    CONSTRAINT webhooks_event_types_check CHECK (((cardinality(event_types) >= 1) AND (cardinality(event_types) <= 64))),
    CONSTRAINT webhooks_status_check CHECK ((status = ANY (ARRAY['enabled'::text, 'disabled'::text]))),
    CONSTRAINT webhooks_pkey PRIMARY KEY (id)
);

CREATE INDEX webhooks_workspace_list_idx ON webhooks USING btree (workspace_id, created_at DESC, id DESC);

CREATE TABLE webhook_events (
    id text NOT NULL,
    workspace_id text NOT NULL,
    event_type text NOT NULL,
    resource_id text NOT NULL,
    payload bytea NOT NULL,
    created_at timestamptz NOT NULL,
    CONSTRAINT webhook_events_pkey PRIMARY KEY (id)
);

CREATE INDEX webhook_events_retention_idx ON webhook_events USING btree (created_at);

CREATE TABLE webhook_deliveries (
    webhook_id text NOT NULL,
    event_id text NOT NULL,
    state text DEFAULT 'pending'::text NOT NULL,
    attempt_count integer DEFAULT 0 NOT NULL,
    next_attempt_at timestamptz NOT NULL,
    claimed_at timestamptz,
    claim_id text,
    last_attempt_at timestamptz,
    delivered_at timestamptz,
    completed_at timestamptz,
    response_status integer,
    last_error text,
    created_at timestamptz NOT NULL,
    CONSTRAINT webhook_deliveries_attempt_count_check CHECK (((attempt_count >= 0) AND (attempt_count <= 3))),
    CONSTRAINT webhook_deliveries_check CHECK (((claimed_at IS NULL) = (claim_id IS NULL))),
    CONSTRAINT webhook_deliveries_check1 CHECK (((state = 'pending'::text) OR (claim_id IS NULL))),
    CONSTRAINT webhook_deliveries_check2 CHECK (((state = 'pending'::text) = (completed_at IS NULL))),
    CONSTRAINT webhook_deliveries_state_check CHECK ((state = ANY (ARRAY['pending'::text, 'succeeded'::text, 'failed'::text]))),
    CONSTRAINT webhook_deliveries_pkey PRIMARY KEY (webhook_id, event_id)
);

CREATE INDEX webhook_deliveries_due_idx ON webhook_deliveries USING btree (next_attempt_at, created_at, webhook_id, event_id) WHERE (state = 'pending'::text);

CREATE INDEX webhook_deliveries_event_retention_idx ON webhook_deliveries USING btree (event_id, completed_at);

-- Cross-resource references are installed after all tables exist.
ALTER TABLE agent_skill_versions
    ADD CONSTRAINT agent_skill_versions_agent_id_agent_version_fkey FOREIGN KEY (agent_id, agent_version) REFERENCES agents(id, version) ON DELETE CASCADE;

ALTER TABLE agent_skill_versions
    ADD CONSTRAINT agent_skill_versions_skill_id_skill_version_fkey FOREIGN KEY (skill_id, skill_version) REFERENCES skill_versions(skill_id, version) ON DELETE RESTRICT;

ALTER TABLE agents
    ADD CONSTRAINT agents_workspace_fk FOREIGN KEY (workspace_id) REFERENCES workspaces(id) ON DELETE RESTRICT;

ALTER TABLE api_keys
    ADD CONSTRAINT api_keys_environment_workspace_fk FOREIGN KEY (environment_id, workspace_id) REFERENCES environments(id, workspace_id) ON DELETE CASCADE;

ALTER TABLE api_keys
    ADD CONSTRAINT api_keys_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES workspaces(id) ON DELETE CASCADE;

ALTER TABLE deployment_runs
    ADD CONSTRAINT deployment_runs_deployment_id_fkey FOREIGN KEY (deployment_id) REFERENCES deployments(id);

ALTER TABLE deployments
    ADD CONSTRAINT deployments_agent_id_agent_version_fkey FOREIGN KEY (agent_id, agent_version) REFERENCES agents(id, version);

ALTER TABLE deployments
    ADD CONSTRAINT deployments_agent_workspace_fk FOREIGN KEY (agent_id, agent_version, workspace_id) REFERENCES agents(id, version, workspace_id);

ALTER TABLE deployments
    ADD CONSTRAINT deployments_environment_id_fkey FOREIGN KEY (environment_id) REFERENCES environments(id);

ALTER TABLE deployments
    ADD CONSTRAINT deployments_environment_workspace_fk FOREIGN KEY (environment_id, workspace_id) REFERENCES environments(id, workspace_id);

ALTER TABLE deployments
    ADD CONSTRAINT deployments_workspace_fk FOREIGN KEY (workspace_id) REFERENCES workspaces(id) ON DELETE RESTRICT;

ALTER TABLE environment_work
    ADD CONSTRAINT environment_work_environment_id_fkey FOREIGN KEY (environment_id) REFERENCES environments(id) ON DELETE CASCADE;

ALTER TABLE environment_work_pollers
    ADD CONSTRAINT environment_work_pollers_environment_id_fkey FOREIGN KEY (environment_id) REFERENCES environments(id) ON DELETE CASCADE;

ALTER TABLE environment_work
    ADD CONSTRAINT environment_work_session_id_fkey FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE;

ALTER TABLE environments
    ADD CONSTRAINT environments_workspace_fk FOREIGN KEY (workspace_id) REFERENCES workspaces(id) ON DELETE RESTRICT;

ALTER TABLE events
    ADD CONSTRAINT events_session_id_fkey FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE;

ALTER TABLE events
    ADD CONSTRAINT events_session_thread_fkey FOREIGN KEY (session_id, thread_id) REFERENCES session_threads(session_id, id);

ALTER TABLE files
    ADD CONSTRAINT files_workspace_fk FOREIGN KEY (workspace_id) REFERENCES workspaces(id) ON DELETE RESTRICT;

ALTER TABLE mcp_discovery_snapshots
    ADD CONSTRAINT mcp_discovery_snapshots_session_id_fkey FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE;

ALTER TABLE mcp_discovery_snapshots
    ADD CONSTRAINT mcp_discovery_snapshots_thread_fk FOREIGN KEY (session_id, thread_id) REFERENCES session_threads(session_id, id) ON DELETE CASCADE;

ALTER TABLE memories
    ADD CONSTRAINT memories_memory_store_id_fkey FOREIGN KEY (memory_store_id) REFERENCES memory_stores(id) ON DELETE CASCADE;

ALTER TABLE memory_stores
    ADD CONSTRAINT memory_stores_workspace_fk FOREIGN KEY (workspace_id) REFERENCES workspaces(id) ON DELETE RESTRICT;

ALTER TABLE memory_versions
    ADD CONSTRAINT memory_versions_memory_store_id_fkey FOREIGN KEY (memory_store_id) REFERENCES memory_stores(id) ON DELETE CASCADE;

ALTER TABLE model_request_usage
    ADD CONSTRAINT model_request_usage_session_id_fkey FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE;

ALTER TABLE model_request_usage
    ADD CONSTRAINT model_request_usage_session_id_thread_id_fkey FOREIGN KEY (session_id, thread_id) REFERENCES session_threads(session_id, id) ON DELETE CASCADE;

ALTER TABLE orchestration_outbox
    ADD CONSTRAINT orchestration_outbox_session_id_fkey FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE;

ALTER TABLE pending_actions
    ADD CONSTRAINT pending_actions_action_event_id_fkey FOREIGN KEY (action_event_id) REFERENCES events(id) ON DELETE CASCADE;

ALTER TABLE pending_actions
    ADD CONSTRAINT pending_actions_approval_event_id_fkey FOREIGN KEY (approval_event_id) REFERENCES events(id);

ALTER TABLE pending_actions
    ADD CONSTRAINT pending_actions_client_event_fk FOREIGN KEY (client_action_event_id) REFERENCES events(id) ON DELETE CASCADE;

ALTER TABLE pending_actions
    ADD CONSTRAINT pending_actions_resolving_event_id_fkey FOREIGN KEY (resolving_event_id) REFERENCES events(id);

ALTER TABLE pending_actions
    ADD CONSTRAINT pending_actions_session_id_fkey FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE;

ALTER TABLE pending_actions
    ADD CONSTRAINT pending_actions_thread_fk FOREIGN KEY (session_id, thread_id) REFERENCES session_threads(session_id, id) ON DELETE CASCADE;

ALTER TABLE provider_transcript_turns
    ADD CONSTRAINT provider_transcript_turns_session_id_fkey FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE;

ALTER TABLE provider_transcript_turns
    ADD CONSTRAINT provider_transcript_turns_trigger_event_id_fkey FOREIGN KEY (trigger_event_id) REFERENCES events(id) ON DELETE CASCADE;

ALTER TABLE session_resources
    ADD CONSTRAINT session_resources_memory_store_id_fkey FOREIGN KEY (memory_store_id) REFERENCES memory_stores(id) ON DELETE RESTRICT;

ALTER TABLE session_resources
    ADD CONSTRAINT session_resources_session_id_fkey FOREIGN KEY (session_id) REFERENCES sessions(id);

ALTER TABLE session_skill_versions
    ADD CONSTRAINT session_skill_versions_session_id_fkey FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE;

ALTER TABLE session_skill_versions
    ADD CONSTRAINT session_skill_versions_skill_id_skill_version_fkey FOREIGN KEY (skill_id, skill_version) REFERENCES skill_versions(skill_id, version) ON DELETE RESTRICT;

ALTER TABLE session_threads
    ADD CONSTRAINT session_threads_session_id_fkey FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE;

ALTER TABLE session_threads
    ADD CONSTRAINT session_threads_session_id_parent_thread_id_fkey FOREIGN KEY (session_id, parent_thread_id) REFERENCES session_threads(session_id, id);

ALTER TABLE session_vaults
    ADD CONSTRAINT session_vaults_session_id_fkey FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE;

ALTER TABLE sessions
    ADD CONSTRAINT sessions_deployment_id_fkey FOREIGN KEY (deployment_id) REFERENCES deployments(id);

ALTER TABLE sessions
    ADD CONSTRAINT sessions_deployment_workspace_fk FOREIGN KEY (deployment_id, workspace_id) REFERENCES deployments(id, workspace_id);

ALTER TABLE sessions
    ADD CONSTRAINT sessions_workspace_fk FOREIGN KEY (workspace_id) REFERENCES workspaces(id) ON DELETE RESTRICT;

ALTER TABLE skill_versions
    ADD CONSTRAINT skill_versions_skill_id_fkey FOREIGN KEY (skill_id) REFERENCES skills(id) ON DELETE RESTRICT;

ALTER TABLE skills
    ADD CONSTRAINT skills_workspace_fk FOREIGN KEY (workspace_id) REFERENCES workspaces(id) ON DELETE RESTRICT;

ALTER TABLE thread_context_snapshots
    ADD CONSTRAINT thread_context_snapshots_session_id_thread_id_fkey FOREIGN KEY (session_id, thread_id) REFERENCES session_threads(session_id, id) ON DELETE CASCADE;

ALTER TABLE thread_context_snapshots
    ADD CONSTRAINT thread_context_snapshots_session_id_thread_id_parent_snaps_fkey FOREIGN KEY (session_id, thread_id, parent_snapshot_id)
        REFERENCES thread_context_snapshots(session_id, thread_id, id);

ALTER TABLE thread_context_snapshots
    ADD CONSTRAINT thread_context_snapshots_session_id_thread_id_trigger_even_fkey FOREIGN KEY (session_id, thread_id, trigger_event_id)
        REFERENCES events(session_id, thread_id, id) ON DELETE CASCADE;

ALTER TABLE thread_orchestration_outbox
    ADD CONSTRAINT thread_orchestration_outbox_session_id_fkey FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE;

ALTER TABLE thread_orchestration_outbox
    ADD CONSTRAINT thread_orchestration_outbox_session_id_thread_id_fkey FOREIGN KEY (session_id, thread_id) REFERENCES session_threads(session_id, id) ON DELETE CASCADE;

ALTER TABLE tool_steps
    ADD CONSTRAINT tool_steps_attempt_id_fkey FOREIGN KEY (attempt_id) REFERENCES turn_attempts(id) ON DELETE CASCADE;

ALTER TABLE turn_attempts
    ADD CONSTRAINT turn_attempts_session_id_fkey FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE;

ALTER TABLE vault_credentials
    ADD CONSTRAINT vault_credentials_vault_id_fkey FOREIGN KEY (vault_id) REFERENCES vaults(id) ON DELETE CASCADE;

ALTER TABLE vaults
    ADD CONSTRAINT vaults_workspace_fk FOREIGN KEY (workspace_id) REFERENCES workspaces(id) ON DELETE RESTRICT;

ALTER TABLE webhook_deliveries
    ADD CONSTRAINT webhook_deliveries_event_id_fkey FOREIGN KEY (event_id) REFERENCES webhook_events(id) ON DELETE CASCADE;

ALTER TABLE webhook_deliveries
    ADD CONSTRAINT webhook_deliveries_webhook_id_fkey FOREIGN KEY (webhook_id) REFERENCES webhooks(id) ON DELETE CASCADE;

ALTER TABLE webhook_events
    ADD CONSTRAINT webhook_events_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES workspaces(id) ON DELETE CASCADE;

ALTER TABLE webhooks
    ADD CONSTRAINT webhooks_workspace_id_fkey FOREIGN KEY (workspace_id) REFERENCES workspaces(id) ON DELETE CASCADE;

-- The default Workspace is runtime bootstrap data, not a legacy-data backfill.
INSERT INTO workspaces (id, name, created_at, updated_at)
VALUES ('wrkspc_default', 'Default Workspace',
        '1970-01-01 00:00:00+00', '1970-01-01 00:00:00+00');

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- Drop the complete development schema as one dependency-checked operation.
DROP TABLE
    webhook_deliveries,
    webhook_events,
    webhooks,
    environment_work_pollers,
    environment_work,
    deployment_runs,
    deployments,
    session_vaults,
    vault_credentials,
    vaults,
    session_resources,
    memory_versions,
    memories,
    memory_stores,
    session_skill_versions,
    agent_skill_versions,
    skill_versions,
    skills,
    files,
    mcp_discovery_snapshots,
    model_request_usage,
    thread_context_snapshots,
    provider_transcript_turns,
    pending_actions,
    tool_steps,
    turn_attempts,
    thread_orchestration_outbox,
    orchestration_outbox,
    events,
    session_threads,
    sessions,
    environments,
    agents,
    api_keys,
    workspaces;

-- +goose StatementEnd
