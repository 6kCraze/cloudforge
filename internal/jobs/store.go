package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/6kCraze/cloudforge/internal/outbox"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

func (s *Store) Create(ctx context.Context, input CreateInput) (Job, error) {
	return s.create(ctx, input.Type, input.Payload)
}

func (s *Store) CreateThumbnail(ctx context.Context, input ThumbnailPayload) (Job, error) {
	id, err := uuid.Parse(input.InputID)
	if err != nil || id.String() != input.InputID {
		return Job{}, &ValidationError{Field: "input_id", Message: "must be a canonical UUID"}
	}
	return s.create(ctx, TypeImageThumbnail, input)
}

func (s *Store) create(ctx context.Context, jobType string, payload any) (Job, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Job{}, fmt.Errorf("begin job creation transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	job, err := insertJob(ctx, tx, jobType, payload)
	if err != nil {
		return Job{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		if commitOutcomeUncertain(err) {
			// A lost connection may hide a successful commit. The upload handler
			// must retain its input instead of deleting a possibly referenced file.
			return job, fmt.Errorf("%w: %w", ErrCommitUncertain, err)
		}
		return Job{}, fmt.Errorf("commit job creation transaction: %w", err)
	}
	return job, nil
}

// insertJob registers a job and its publication intent in the caller's transaction.
func insertJob(ctx context.Context, tx pgx.Tx, jobType string, payload any) (Job, error) {
	jobID := uuid.NewString()
	jobPayload, err := json.Marshal(payload)
	if err != nil {
		return Job{}, fmt.Errorf("encode job payload: %w", err)
	}
	eventPayload, err := json.Marshal(outbox.JobReadyPayload{JobID: jobID})
	if err != nil {
		return Job{}, fmt.Errorf("encode outbox payload: %w", err)
	}
	job, err := scanJob(tx.QueryRow(ctx, `INSERT INTO cloudforge.jobs (id, type, payload)
		VALUES ($1, $2, $3) RETURNING `+jobColumns, jobID, jobType, jobPayload))
	if err != nil {
		return Job{}, fmt.Errorf("insert job: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO cloudforge.outbox_events (id, event_type, aggregate_id, payload)
		VALUES ($1, $2, $3, $4)`, uuid.NewString(), outbox.EventTypeJobReady, jobID, eventPayload); err != nil {
		return Job{}, fmt.Errorf("insert outbox event: %w", err)
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

// ClaimByID atomically transitions one specifically delivered queued job to
// running. RabbitMQ may deliver the same job more than once, so the status
// predicate is the database-backed deduplication guard.
func (s *Store) ClaimByID(ctx context.Context, id, workerID string) (Job, error) {
	row := s.pool.QueryRow(ctx, `
		UPDATE cloudforge.jobs
		SET status = 'running',
			attempt_count = attempt_count + 1,
			claimed_by = $2,
			started_at = now(),
			updated_at = now()
		WHERE id = $1
		  AND status = 'queued'
		RETURNING `+jobColumns,
		id, workerID,
	)
	job, err := scanJob(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, ErrNotClaimable
	}
	if err != nil {
		return Job{}, fmt.Errorf("claim job by ID: %w", err)
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

// Only explicit rollback replies justify deleting a finalized input. Transport
// errors and SQLSTATE statement-completion-unknown are conservatively retained.
func commitOutcomeUncertain(err error) bool {
	if err == nil || errors.Is(err, pgx.ErrTxCommitRollback) {
		return false
	}
	var serverError *pgconn.PgError
	if errors.As(err, &serverError) {
		if strings.HasPrefix(serverError.Code, "23") || (strings.HasPrefix(serverError.Code, "40") && serverError.Code != "40003") {
			return false
		}
	}
	return true
}
