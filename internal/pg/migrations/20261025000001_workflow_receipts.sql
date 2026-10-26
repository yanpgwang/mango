-- +goose Up
-- Internal receipt kernel only. No public Run admission or evaluator is enabled.
CREATE TABLE workflow_runs (
    id text PRIMARY KEY,
    session_id text NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    admission_key text NOT NULL,
    request jsonb NOT NULL,
    roster jsonb NOT NULL,
    deadline timestamptz NOT NULL,
    max_invocations integer NOT NULL CHECK (max_invocations BETWEEN 1 AND 25),
    max_result_bytes integer NOT NULL CHECK (max_result_bytes BETWEEN 1 AND 65536),
    state text NOT NULL DEFAULT 'open' CHECK (state IN ('open', 'closed')),
    version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
    created_at timestamptz NOT NULL,
    UNIQUE (session_id, admission_key),
    UNIQUE (session_id, id)
);

CREATE TABLE workflow_invocations (
    id text PRIMARY KEY,
    session_id text NOT NULL,
    run_id text NOT NULL,
    ordinal integer NOT NULL CHECK (ordinal >= 0),
    command jsonb NOT NULL,
    agent_snapshot jsonb NOT NULL,
    UNIQUE (run_id, ordinal),
    UNIQUE (session_id, run_id, id),
    FOREIGN KEY (session_id, run_id) REFERENCES workflow_runs(session_id, id) ON DELETE CASCADE
);

CREATE TABLE workflow_invocation_attempts (
    id text PRIMARY KEY,
    session_id text NOT NULL,
    run_id text NOT NULL,
    invocation_id text NOT NULL,
    attempt_no integer NOT NULL CHECK (attempt_no > 0),
    thread_id text NOT NULL UNIQUE,
    root_trigger_id text NOT NULL,
    state text NOT NULL DEFAULT 'active' CHECK (state IN ('active', 'settled')),
    receipt jsonb,
    terminal_trigger_id text,
    terminal_event_id text,
    turn_attempt_id text REFERENCES turn_attempts(id),
    UNIQUE (invocation_id, attempt_no),
    FOREIGN KEY (session_id, run_id, invocation_id) REFERENCES workflow_invocations(session_id, run_id, id) ON DELETE CASCADE,
    FOREIGN KEY (session_id, thread_id) REFERENCES session_threads(session_id, id) ON DELETE CASCADE,
    FOREIGN KEY (session_id, thread_id, root_trigger_id) REFERENCES events(session_id, thread_id, id),
    FOREIGN KEY (session_id, thread_id, terminal_trigger_id) REFERENCES events(session_id, thread_id, id),
    FOREIGN KEY (session_id, thread_id, terminal_event_id) REFERENCES events(session_id, thread_id, id),
    CHECK ((state = 'active' AND receipt IS NULL AND terminal_trigger_id IS NULL AND terminal_event_id IS NULL)
        OR (state = 'settled' AND receipt IS NOT NULL AND terminal_trigger_id IS NOT NULL AND terminal_event_id IS NOT NULL))
);

CREATE TABLE workflow_run_outbox (
    session_id text NOT NULL,
    run_id text PRIMARY KEY,
    version bigint NOT NULL CHECK (version > 0),
    enqueued_at timestamptz NOT NULL,
    FOREIGN KEY (session_id, run_id) REFERENCES workflow_runs(session_id, id) ON DELETE CASCADE
);

-- +goose Down
DROP TABLE workflow_run_outbox;
DROP TABLE workflow_invocation_attempts;
DROP TABLE workflow_invocations;
DROP TABLE workflow_runs;
