-- +goose Up
-- Internal ownership/relay gate only; startup still does not enable Run admission.
ALTER TABLE workflow_runs ADD COLUMN coordinator_chain_id text NOT NULL DEFAULT '';
ALTER TABLE workflow_runs ADD COLUMN coordinator_execution_id text NOT NULL DEFAULT '';
ALTER TABLE workflow_runs ADD CONSTRAINT workflow_coordinator_pair CHECK ((coordinator_chain_id='')=(coordinator_execution_id=''));

ALTER TABLE workflow_run_outbox ADD COLUMN delivery_attempts integer NOT NULL DEFAULT 0 CHECK (delivery_attempts BETWEEN 0 AND 8);
ALTER TABLE workflow_run_outbox ADD COLUMN available_at timestamptz NOT NULL DEFAULT now();
ALTER TABLE workflow_run_outbox ADD COLUMN lease_token text NOT NULL DEFAULT '';
CREATE INDEX workflow_run_outbox_due ON workflow_run_outbox(available_at,enqueued_at,run_id) WHERE delivery_attempts<8;

-- +goose Down
DROP INDEX workflow_run_outbox_due;
ALTER TABLE workflow_run_outbox DROP COLUMN lease_token;
ALTER TABLE workflow_run_outbox DROP COLUMN available_at;
ALTER TABLE workflow_run_outbox DROP COLUMN delivery_attempts;
ALTER TABLE workflow_runs DROP CONSTRAINT workflow_coordinator_pair;
ALTER TABLE workflow_runs DROP COLUMN coordinator_execution_id;
ALTER TABLE workflow_runs DROP COLUMN coordinator_chain_id;
