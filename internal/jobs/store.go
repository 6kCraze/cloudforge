package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/6kCraze/cloudforge/internal/outbox"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct {
	pool          *pgxpool.Pool
	leaseDuration time.Duration
	retryBase     time.Duration
	retryMax      time.Duration
}

func NewStore(pool *pgxpool.Pool) *Store {
	return NewStoreWithLease(pool, 30*time.Second)
}

func NewStoreWithLease(pool *pgxpool.Pool, lease time.Duration) *Store {
	return NewStoreWithPolicy(pool, lease, time.Second, 4*time.Second)
}

func NewStoreWithPolicy(pool *pgxpool.Pool, lease, retryBase, retryMax time.Duration) *Store {
	return &Store{pool: pool, leaseDuration: lease, retryBase: retryBase, retryMax: retryMax}
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
		WHERE status = 'queued' AND next_retry_at IS NULL
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

	job, err := s.claimInTransaction(ctx, tx, id, workerID)
	if err != nil {
		return Job{}, err
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
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Job{}, fmt.Errorf("begin claim transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	job, err := s.claimInTransaction(ctx, tx, id, workerID)
	if err != nil {
		return Job{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Job{}, fmt.Errorf("commit claim transaction: %w", err)
	}
	return job, nil
}

func (s *Store) claimInTransaction(ctx context.Context, tx pgx.Tx, id, workerID string) (Job, error) {
	if workerID == "" {
		return Job{}, errors.New("worker ID is required")
	}
	attemptID := uuid.NewString()
	row := tx.QueryRow(ctx, `
		UPDATE cloudforge.jobs
		SET status = 'running',
			attempt_count = attempt_count + 1,
			claimed_by = $2,
			attempt_id = $3,
			lease_expires_at = clock_timestamp() + $4::bigint * interval '1 microsecond',
			next_retry_at = NULL,
			result = NULL, error_code = NULL, error_message = NULL, completed_at = NULL,
			started_at = clock_timestamp(),
			updated_at = clock_timestamp()
		WHERE id = $1
		  AND status = 'queued' AND next_retry_at IS NULL
		RETURNING `+jobColumns,
		id, workerID, attemptID, s.leaseDuration.Microseconds(),
	)
	job, err := scanJob(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, ErrNotClaimable
	}
	if err != nil {
		return Job{}, fmt.Errorf("claim job by ID: %w", err)
	}
	_, err = tx.Exec(ctx, `INSERT INTO cloudforge.job_attempts
		(attempt_id, job_id, attempt_number, worker_id, started_at, last_heartbeat_at, lease_expires_at)
		VALUES ($1, $2, $3, $4, $5, $5, $6)`, attemptID, id, job.AttemptCount, workerID, job.StartedAt, job.LeaseExpiresAt)
	if err != nil {
		return Job{}, fmt.Errorf("record claim attempt: %w", err)
	}
	return job, nil
}

func (s *Store) Heartbeat(ctx context.Context, id, workerID, attemptID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var expires time.Time
	err = tx.QueryRow(ctx, `UPDATE cloudforge.jobs SET
		lease_expires_at = clock_timestamp() + $4::bigint * interval '1 microsecond',
		updated_at = clock_timestamp()
		WHERE id = $1 AND status = 'running' AND claimed_by = $2 AND attempt_id = $3
		AND lease_expires_at > clock_timestamp()
		RETURNING lease_expires_at`, id, workerID, attemptID, s.leaseDuration.Microseconds()).Scan(&expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrOwnershipLost
	}
	if err != nil {
		return fmt.Errorf("renew job lease: %w", err)
	}
	tag, err := tx.Exec(ctx, `UPDATE cloudforge.job_attempts SET
		lease_expires_at = $2, last_heartbeat_at = clock_timestamp()
		WHERE attempt_id = $1 AND outcome = 'running'`, attemptID, expires)
	if err != nil {
		return fmt.Errorf("record heartbeat: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrOwnershipLost
	}
	return tx.Commit(ctx)
}

func (s *Store) MarkSucceeded(ctx context.Context, id, workerID, attemptID string, result json.RawMessage) error {
	return s.markTerminal(ctx, id, workerID, attemptID, "succeeded", result, "", "")
}

func (s *Store) MarkFailed(ctx context.Context, id, workerID, attemptID, code, message string) error {
	return s.markTerminal(ctx, id, workerID, attemptID, "failed", nil, code, message)
}

func (s *Store) markTerminal(ctx context.Context, id, workerID, attemptID, outcome string, result json.RawMessage, code, message string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var completed time.Time
	err = tx.QueryRow(ctx, `UPDATE cloudforge.jobs SET status = $4,
		result = $5, error_code = NULLIF($6, ''), error_message = NULLIF($7, ''),
		completed_at = clock_timestamp(), updated_at = clock_timestamp()
		WHERE id = $1 AND status = 'running' AND claimed_by = $2 AND attempt_id = $3
		AND lease_expires_at > clock_timestamp()
		AND (type <> 'image.thumbnail' OR $4 <> 'succeeded' OR
			($5::jsonb->>'attempt_id' = attempt_id::text AND $5::jsonb->>'output_id' = id::text))
		RETURNING completed_at`, id, workerID, attemptID, outcome, result, code, message).Scan(&completed)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrOwnershipLost
	}
	if err != nil {
		return fmt.Errorf("mark job %s: %w", outcome, err)
	}
	decision := ""
	if outcome == "failed" {
		decision = "permanent"
	}
	tag, err := tx.Exec(ctx, `UPDATE cloudforge.job_attempts SET outcome = $2, completed_at = $3,
		error_code = NULLIF($4, ''), error_message = NULLIF($5, ''), retry_decision = NULLIF($6, '')
		WHERE attempt_id = $1 AND outcome = 'running'`, attemptID, outcome, completed, code, message, decision)
	if err != nil {
		return fmt.Errorf("record attempt %s: %w", outcome, err)
	}
	if tag.RowsAffected() != 1 {
		return ErrOwnershipLost
	}
	return tx.Commit(ctx)
}

const jobColumns = `
	id::text, type, status, payload, result, error_code, error_message,
	attempt_count, claimed_by, created_at, updated_at, started_at, completed_at,
	attempt_id::text, lease_expires_at, next_retry_at`

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
		&job.AttemptID,
		&job.LeaseExpiresAt,
		&job.NextRetryAt,
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
