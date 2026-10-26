-- +goose Up
-- Internal successful terminal receipts. Public Run admission remains absent.
CREATE TABLE workflow_run_terminal_results (
    run_id text PRIMARY KEY,
    session_id text NOT NULL,
    candidate_cursor bigint NOT NULL CHECK (candidate_cursor > 0),
    evaluator_epoch bigint NOT NULL CHECK (evaluator_epoch > 0),
    coordinator_execution_id text NOT NULL,
    request_id text NOT NULL,
    value jsonb NOT NULL,
    primary_event_id text NOT NULL UNIQUE REFERENCES events(id),
    consumed_trigger_id text REFERENCES events(id),
    consumed_at timestamptz,
    FOREIGN KEY (session_id,run_id) REFERENCES workflow_runs(session_id,id) ON DELETE CASCADE,
    CHECK ((consumed_trigger_id IS NULL) = (consumed_at IS NULL))
);
CREATE INDEX workflow_run_terminal_pending ON workflow_run_terminal_results(session_id) WHERE consumed_at IS NULL;

CREATE TABLE workflow_outcome_waits (
    session_id text NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    trigger_id text NOT NULL REFERENCES events(id),
    start_event_id text NOT NULL REFERENCES events(id),
    end_event_id text NOT NULL,
    iteration integer NOT NULL CHECK (iteration >= 0),
 evaluation_open boolean NOT NULL DEFAULT true,
 state text NOT NULL DEFAULT 'waiting' CHECK(state IN ('waiting','finished')),
 PRIMARY KEY(session_id,trigger_id)
);
CREATE UNIQUE INDEX workflow_outcome_one_wait ON workflow_outcome_waits(session_id) WHERE state='waiting';

-- +goose Down
DROP TABLE workflow_outcome_waits;
DROP TABLE workflow_run_terminal_results;
