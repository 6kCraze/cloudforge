-- +goose Up
ALTER TABLE cloudforge.jobs DROP CONSTRAINT jobs_type_check;
ALTER TABLE cloudforge.jobs ADD CONSTRAINT jobs_type_check
    CHECK (type IN ('synthetic.sha256', 'image.thumbnail'));

-- +goose Down
-- Refuse rollback if image rows remain, rather than deleting application data.
ALTER TABLE cloudforge.jobs DROP CONSTRAINT jobs_type_check;
ALTER TABLE cloudforge.jobs ADD CONSTRAINT jobs_type_check
    CHECK (type = 'synthetic.sha256');
