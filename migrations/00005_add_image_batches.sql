-- +goose Up
CREATE TABLE cloudforge.batches (
    id UUID PRIMARY KEY,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE cloudforge.batch_images (
    batch_id UUID NOT NULL REFERENCES cloudforge.batches(id),
    position SMALLINT NOT NULL CHECK (position BETWEEN 0 AND 3),
    input_id UUID NOT NULL UNIQUE,
    job_id UUID NOT NULL UNIQUE REFERENCES cloudforge.jobs(id),
    original_name TEXT NOT NULL CHECK (char_length(original_name) BETWEEN 1 AND 200),
    format TEXT NOT NULL CHECK (format IN ('jpeg', 'png')),
    bytes BIGINT NOT NULL CHECK (bytes BETWEEN 1 AND 5242880),
    width INTEGER NOT NULL CHECK (width BETWEEN 1 AND 8192),
    height INTEGER NOT NULL CHECK (height BETWEEN 1 AND 8192),
    CHECK (width::bigint * height <= 8000000),
    PRIMARY KEY (batch_id, position)
);

-- +goose Down
-- Jobs and their outbox history survive removal of batch tracking.
DROP TABLE cloudforge.batch_images;
DROP TABLE cloudforge.batches;
