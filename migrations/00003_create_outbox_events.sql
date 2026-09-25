-- +goose Up
CREATE TABLE cloudforge.outbox_events (
    id UUID PRIMARY KEY,
    event_type TEXT NOT NULL,
    aggregate_id UUID NOT NULL REFERENCES cloudforge.jobs (id),
    payload JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at TIMESTAMPTZ,
    CONSTRAINT outbox_events_type_check CHECK (event_type = 'job.ready'),
    CONSTRAINT outbox_events_payload_object_check CHECK (jsonb_typeof(payload) = 'object'),
    CONSTRAINT outbox_events_job_ready_unique UNIQUE (event_type, aggregate_id)
);

CREATE INDEX outbox_events_unpublished_created_idx
    ON cloudforge.outbox_events (created_at, id)
    WHERE published_at IS NULL;

-- +goose Down
DROP TABLE cloudforge.outbox_events;
