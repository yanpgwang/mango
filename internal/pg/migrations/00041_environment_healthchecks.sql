-- +goose Up
ALTER TABLE environment_work
    ALTER COLUMN session_id DROP NOT NULL,
    ALTER COLUMN activation_seq DROP NOT NULL,
    ADD COLUMN work_type text NOT NULL DEFAULT 'session',
    ADD COLUMN expires_at timestamptz,
    ADD COLUMN result jsonb,
    ADD CONSTRAINT environment_work_data_shape CHECK (
        (work_type = 'session' AND session_id IS NOT NULL AND activation_seq IS NOT NULL AND expires_at IS NULL AND result IS NULL) OR
        (work_type = 'healthcheck' AND session_id IS NULL AND activation_seq IS NULL AND expires_at IS NOT NULL
          AND ((state <> 'stopped' AND result IS NULL) OR
               (state = 'stopped' AND result IS NOT NULL AND result->>'status' IN ('succeeded', 'failed', 'timed_out', 'cancelled'))))
    );

-- +goose Down
DELETE FROM environment_work WHERE work_type = 'healthcheck';
ALTER TABLE environment_work
    DROP CONSTRAINT environment_work_data_shape,
    DROP COLUMN result,
    DROP COLUMN expires_at,
    DROP COLUMN work_type,
    ALTER COLUMN session_id SET NOT NULL,
    ALTER COLUMN activation_seq SET NOT NULL;
