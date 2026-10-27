-- +goose Up
ALTER TABLE cloudforge.jobs DROP CONSTRAINT jobs_status_check;
ALTER TABLE cloudforge.jobs
    ADD CONSTRAINT jobs_status_check CHECK (status IN ('queued', 'running', 'retry_wait', 'succeeded', 'failed')),
    ADD COLUMN next_retry_at TIMESTAMPTZ,
    ADD CONSTRAINT jobs_retry_time_check CHECK ((status = 'retry_wait') = (next_retry_at IS NOT NULL));

CREATE INDEX jobs_expired_lease_idx ON cloudforge.jobs (lease_expires_at, id)
    WHERE status = 'running';
CREATE INDEX jobs_due_retry_idx ON cloudforge.jobs (next_retry_at, id)
    WHERE status = 'retry_wait';

ALTER TABLE cloudforge.job_attempts
    ADD COLUMN error_code TEXT,
    ADD COLUMN error_message TEXT,
    ADD COLUMN retry_decision TEXT CHECK (retry_decision IN ('scheduled', 'exhausted', 'permanent'));

-- The event ID remains the primary key. A job can have one initial ready
-- event (generation 0) and one event for each completed retry generation.
ALTER TABLE cloudforge.outbox_events
    ADD COLUMN delivery_generation SMALLINT NOT NULL DEFAULT 0
        CHECK (delivery_generation >= 0);
ALTER TABLE cloudforge.outbox_events DROP CONSTRAINT outbox_events_job_ready_unique;
ALTER TABLE cloudforge.outbox_events
    ADD CONSTRAINT outbox_events_job_ready_generation_unique
        UNIQUE (event_type, aggregate_id, delivery_generation);

-- +goose Down
-- Refuse a rollback that would discard active retry state or event history.
-- Re-adding the original uniqueness constraint also refuses duplicate jobs.
DO $$ BEGIN
    IF EXISTS (SELECT 1 FROM cloudforge.jobs WHERE status = 'retry_wait') THEN
        RAISE EXCEPTION 'resolve retry_wait jobs before reversing migration 00007';
    END IF;
END $$;
ALTER TABLE cloudforge.outbox_events DROP CONSTRAINT outbox_events_job_ready_generation_unique;
ALTER TABLE cloudforge.outbox_events
    ADD CONSTRAINT outbox_events_job_ready_unique UNIQUE (event_type, aggregate_id);
ALTER TABLE cloudforge.outbox_events DROP COLUMN delivery_generation;
ALTER TABLE cloudforge.job_attempts
    DROP COLUMN error_code, DROP COLUMN error_message, DROP COLUMN retry_decision;
DROP INDEX cloudforge.jobs_due_retry_idx;
DROP INDEX cloudforge.jobs_expired_lease_idx;
ALTER TABLE cloudforge.jobs DROP CONSTRAINT jobs_retry_time_check;
ALTER TABLE cloudforge.jobs DROP CONSTRAINT jobs_status_check;
ALTER TABLE cloudforge.jobs
    ADD CONSTRAINT jobs_status_check CHECK (status IN ('queued', 'running', 'succeeded', 'failed'));
ALTER TABLE cloudforge.jobs DROP COLUMN next_retry_at;
