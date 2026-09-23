package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

func (s *Store) Create(ctx context.Context, input CreateInput) (Job, error) {
	payload, err := json.Marshal(input.Payload)
	if err != nil {
		return Job{}, fmt.Errorf("encode job payload: %w", err)
	}

	row := s.pool.QueryRow(ctx, `
		INSERT INTO cloudforge.jobs (type, payload)
		VALUES ($1, $2)
		RETURNING `+jobColumns,
		input.Type, payload,
	)
	job, err := scanJob(row)
	if err != nil {
		return Job{}, fmt.Errorf("insert job: %w", err)
	}
	return job, nil
}

func (s *Store) Get(ctx context.Context, id string) (Job, error) {
	row := s.pool.QueryRow(ctx, `SELECT `+jobColumns+` FROM cloudforge.jobs WHERE id = $1`, id)
	job, err := scanJob(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	if err != nil {
		return Job{}, fmt.Errorf("get job: %w", err)
	}
	return job, nil
}

// ClaimNext atomically locks the oldest queued row, changes it to running,
// records its owner, and increments the attempt count in one transaction.
func (s *Store) ClaimNext(ctx context.Context, workerID string) (Job, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Job{}, fmt.Errorf("begin claim transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	var id string
	err = tx.QueryRow(ctx, `
		SELECT id
		FROM cloudforge.jobs
		WHERE status = 'queued'
		ORDER BY created_at, id
		FOR UPDATE SKIP LOCKED
		LIMIT 1
	`).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, ErrNoJobs
	}
	if err != nil {
		return Job{}, fmt.Errorf("select queued job: %w", err)
	}

	row := tx.QueryRow(ctx, `
		UPDATE cloudforge.jobs
		SET status = 'running',
			attempt_count = attempt_count + 1,
			claimed_by = $2,
			started_at = now(),
			updated_at = now()
		WHERE id = $1
		RETURNING `+jobColumns,
		id, workerID,
	)
	job, err := scanJob(row)
	if err != nil {
		return Job{}, fmt.Errorf("mark job running: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return Job{}, fmt.Errorf("commit claim transaction: %w", err)
	}
	return job, nil
}

func (s *Store) MarkSucceeded(ctx context.Context, id, workerID string, result json.RawMessage) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE cloudforge.jobs
		SET status = 'succeeded',
			result = $3,
			error_code = NULL,
			error_message = NULL,
			completed_at = now(),
			updated_at = now()
		WHERE id = $1 AND status = 'running' AND claimed_by = $2
	`, id, workerID, result)
	if err != nil {
		return fmt.Errorf("mark job succeeded: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("mark job succeeded: job is not running for worker %q", workerID)
	}
	return nil
}

func (s *Store) MarkFailed(ctx context.Context, id, workerID, code, message string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE cloudforge.jobs
		SET status = 'failed',
			result = NULL,
			error_code = $3,
			error_message = $4,
			completed_at = now(),
			updated_at = now()
		WHERE id = $1 AND status = 'running' AND claimed_by = $2
	`, id, workerID, code, message)
	if err != nil {
		return fmt.Errorf("mark job failed: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("mark job failed: job is not running for worker %q", workerID)
	}
	return nil
}

const jobColumns = `
	id::text, type, status, payload, result, error_code, error_message,
	attempt_count, claimed_by, created_at, updated_at, started_at, completed_at`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanJob(row rowScanner) (Job, error) {
	var job Job
	err := row.Scan(
		&job.ID,
		&job.Type,
		&job.Status,
		&job.Payload,
		&job.Result,
		&job.ErrorCode,
		&job.ErrorMessage,
		&job.AttemptCount,
		&job.ClaimedBy,
		&job.CreatedAt,
		&job.UpdatedAt,
		&job.StartedAt,
		&job.CompletedAt,
	)
	return job, err
}
