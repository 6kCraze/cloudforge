-- +goose Up
ALTER TABLE cloudforge.jobs
    ADD COLUMN attempt_id UUID,
    ADD COLUMN lease_expires_at TIMESTAMPTZ;

CREATE TABLE cloudforge.job_attempts (
    attempt_id UUID PRIMARY KEY,
    job_id UUID NOT NULL REFERENCES cloudforge.jobs(id),
    attempt_number SMALLINT NOT NULL CHECK (attempt_number > 0),
    worker_id TEXT NOT NULL CHECK (worker_id <> ''),
    started_at TIMESTAMPTZ NOT NULL,
    last_heartbeat_at TIMESTAMPTZ NOT NULL,
    lease_expires_at TIMESTAMPTZ NOT NULL,
    completed_at TIMESTAMPTZ,
    outcome TEXT NOT NULL DEFAULT 'running' CHECK (outcome IN ('running', 'succeeded', 'failed', 'expired')),
    UNIQUE (job_id, attempt_number)
);

-- Previously running V4 jobs have no live lease or token. Record their
-- ownership, but let the lease expire immediately; checkpoint 2 will recover them.
UPDATE cloudforge.jobs
SET attempt_id = gen_random_uuid(), lease_expires_at = clock_timestamp()
WHERE status = 'running';

INSERT INTO cloudforge.job_attempts
    (attempt_id, job_id, attempt_number, worker_id, started_at, last_heartbeat_at, lease_expires_at)
SELECT attempt_id, id, GREATEST(attempt_count, 1), COALESCE(NULLIF(claimed_by, ''), 'legacy-unknown'),
       COALESCE(started_at, updated_at), COALESCE(started_at, updated_at), lease_expires_at
FROM cloudforge.jobs WHERE status = 'running';

-- +goose Down
DROP TABLE cloudforge.job_attempts;
ALTER TABLE cloudforge.jobs DROP COLUMN lease_expires_at, DROP COLUMN attempt_id;
