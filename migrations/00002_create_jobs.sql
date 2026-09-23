-- +goose Up
CREATE TABLE cloudforge.jobs (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    type TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'queued',
    payload JSONB NOT NULL,
    result JSONB,
    error_code TEXT,
    error_message TEXT,
    attempt_count SMALLINT NOT NULL DEFAULT 0,
    claimed_by TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    CONSTRAINT jobs_type_check CHECK (type = 'synthetic.sha256'),
    CONSTRAINT jobs_status_check CHECK (status IN ('queued', 'running', 'succeeded', 'failed')),
    CONSTRAINT jobs_payload_object_check CHECK (jsonb_typeof(payload) = 'object'),
    CONSTRAINT jobs_result_object_check CHECK (result IS NULL OR jsonb_typeof(result) = 'object'),
    CONSTRAINT jobs_attempt_count_check CHECK (attempt_count >= 0)
);

CREATE INDEX jobs_queued_created_idx
    ON cloudforge.jobs (created_at, id)
    WHERE status = 'queued';

-- +goose Down
DROP TABLE cloudforge.jobs;
