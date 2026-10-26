-- +goose Up
-- Unexposed frontier gate; no evaluator or public Run admission is registered.
ALTER TABLE workflow_runs ADD COLUMN evaluator_epoch bigint NOT NULL DEFAULT 0 CHECK (evaluator_epoch >= 0);
ALTER TABLE workflow_runs ADD COLUMN journal_cursor bigint NOT NULL DEFAULT 0 CHECK (journal_cursor BETWEEN 0 AND 256);
ALTER TABLE workflow_runs ADD COLUMN journal_bytes bigint NOT NULL DEFAULT 0 CHECK (journal_bytes BETWEEN 0 AND 8388608);
ALTER TABLE workflow_runs ADD COLUMN pause_reasons text[] NOT NULL DEFAULT '{}';

CREATE TABLE workflow_evaluator_claims (
    session_id text NOT NULL,
    run_id text NOT NULL,
    claim_id text NOT NULL,
    previous_epoch bigint NOT NULL,
    epoch bigint NOT NULL,
    journal_cursor bigint NOT NULL,
    PRIMARY KEY (run_id, claim_id),
    UNIQUE (run_id, epoch),
    FOREIGN KEY (session_id, run_id) REFERENCES workflow_runs(session_id, id) ON DELETE CASCADE
);

CREATE TABLE workflow_pump_decisions (
    session_id text NOT NULL,
    run_id text NOT NULL,
    request_id text NOT NULL,
    epoch bigint NOT NULL,
    expected_cursor bigint NOT NULL,
    command jsonb NOT NULL,
    decision jsonb NOT NULL,
    PRIMARY KEY (run_id, request_id),
    UNIQUE (run_id, expected_cursor),
    FOREIGN KEY (session_id, run_id) REFERENCES workflow_runs(session_id, id) ON DELETE CASCADE
);

CREATE TABLE outcome_grading_reservations (
    session_id text NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    thread_id text NOT NULL,
    trigger_id text NOT NULL,
    outcome_id text NOT NULL,
    iteration integer NOT NULL CHECK (iteration >= 0),
    start_event_id text NOT NULL,
    end_event_id text NOT NULL,
    state text NOT NULL DEFAULT 'active' CHECK (state IN ('active', 'finished', 'fenced')),
    terminal_event_id text,
    PRIMARY KEY (session_id, start_event_id),
    UNIQUE (session_id, end_event_id),
    FOREIGN KEY (session_id, thread_id, trigger_id) REFERENCES events(session_id, thread_id, id),
    FOREIGN KEY (session_id, thread_id, start_event_id) REFERENCES events(session_id, thread_id, id)
);
CREATE UNIQUE INDEX one_active_outcome_grader ON outcome_grading_reservations(session_id) WHERE state='active';

-- +goose Down
DROP TABLE outcome_grading_reservations;
DROP TABLE workflow_pump_decisions;
DROP TABLE workflow_evaluator_claims;
ALTER TABLE workflow_runs DROP COLUMN pause_reasons;
ALTER TABLE workflow_runs DROP COLUMN journal_cursor;
ALTER TABLE workflow_runs DROP COLUMN journal_bytes;
ALTER TABLE workflow_runs DROP COLUMN evaluator_epoch;
