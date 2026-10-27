package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/6kCraze/cloudforge/internal/outbox"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const MaxAttempts int16 = 3
const recoveryBatchSize = 16

// ScheduleRetry records a recoverable processing failure under the current
// ownership fence. The third processing attempt fails terminally instead.
func (s *Store) ScheduleRetry(ctx context.Context, id, workerID, attemptID, code, message string) (string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var status string
	var ended time.Time
	err = tx.QueryRow(ctx, `UPDATE cloudforge.jobs SET
		status = CASE WHEN attempt_count < $4 THEN 'retry_wait' ELSE 'failed' END,
		next_retry_at = CASE WHEN attempt_count < $4 THEN
			clock_timestamp() + LEAST($7::bigint * (1::bigint << LEAST(attempt_count::int - 1, 2)), $8::bigint) * interval '1 microsecond'
			ELSE NULL END,
		result = NULL, error_code = $5, error_message = $6,
		claimed_by = CASE WHEN attempt_count < $4 THEN NULL ELSE claimed_by END,
		attempt_id = CASE WHEN attempt_count < $4 THEN NULL ELSE attempt_id END,
		lease_expires_at = CASE WHEN attempt_count < $4 THEN NULL ELSE lease_expires_at END,
		completed_at = CASE WHEN attempt_count < $4 THEN NULL ELSE clock_timestamp() END,
		updated_at = clock_timestamp()
		WHERE id = $1 AND status = 'running' AND claimed_by = $2 AND attempt_id = $3
		AND lease_expires_at > clock_timestamp()
		RETURNING status, updated_at`, id, workerID, attemptID, MaxAttempts, code, message,
		s.retryBase.Microseconds(), s.retryMax.Microseconds()).Scan(&status, &ended)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrOwnershipLost
	}
	if err != nil {
		return "", fmt.Errorf("schedule processing retry: %w", err)
	}
	decision := "scheduled"
	if status == StatusFailed {
		decision = "exhausted"
	}
	tag, err := tx.Exec(ctx, `UPDATE cloudforge.job_attempts SET outcome = 'failed', completed_at = $2,
		error_code = $3, error_message = $4, retry_decision = $5
		WHERE attempt_id = $1 AND outcome = 'running'`, attemptID, ended, code, message, decision)
	if err != nil {
		return "", fmt.Errorf("record processing attempt: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return "", ErrOwnershipLost
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("commit processing retry: %w", err)
	}
	return status, nil
}

type expiredAttempt struct {
	jobID, attemptID string
	attemptCount     int16
}

// RecoverExpired moves each abandoned running job exactly once. The row lock
// excludes another recovery loop and terminal worker updates during the decision.
func (s *Store) RecoverExpired(ctx context.Context) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	rows, err := tx.Query(ctx, `SELECT id::text, attempt_id::text, attempt_count
		FROM cloudforge.jobs WHERE status = 'running' AND lease_expires_at <= clock_timestamp()
		ORDER BY lease_expires_at, id FOR UPDATE SKIP LOCKED LIMIT $1`, recoveryBatchSize)
	if err != nil {
		return 0, fmt.Errorf("select expired jobs: %w", err)
	}
	var selected []expiredAttempt
	for rows.Next() {
		var item expiredAttempt
		if err := rows.Scan(&item.jobID, &item.attemptID, &item.attemptCount); err != nil {
			rows.Close()
			return 0, err
		}
		selected = append(selected, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	for _, item := range selected {
		decision := "scheduled"
		if item.attemptCount >= MaxAttempts {
			decision = "exhausted"
		}
		tag, err := tx.Exec(ctx, `UPDATE cloudforge.job_attempts SET outcome = 'expired',
			completed_at = clock_timestamp(), error_code = 'lease_expired',
			error_message = 'worker lease expired before completion', retry_decision = $2
			WHERE attempt_id = $1 AND outcome = 'running'`, item.attemptID, decision)
		if err != nil {
			return 0, fmt.Errorf("record abandoned attempt: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return 0, fmt.Errorf("abandoned attempt %s has no active history", item.attemptID)
		}
		tag, err = tx.Exec(ctx, `UPDATE cloudforge.jobs SET
			status = CASE WHEN attempt_count < $3 THEN 'retry_wait' ELSE 'failed' END,
			next_retry_at = CASE WHEN attempt_count < $3 THEN
				clock_timestamp() + LEAST($4::bigint * (1::bigint << LEAST(attempt_count::int - 1, 2)), $5::bigint) * interval '1 microsecond'
				ELSE NULL END,
			error_code = 'lease_expired', error_message = 'worker lease expired before completion',
			claimed_by = CASE WHEN attempt_count < $3 THEN NULL ELSE claimed_by END,
			attempt_id = CASE WHEN attempt_count < $3 THEN NULL ELSE attempt_id END,
			lease_expires_at = CASE WHEN attempt_count < $3 THEN NULL ELSE lease_expires_at END,
			completed_at = CASE WHEN attempt_count < $3 THEN NULL ELSE clock_timestamp() END,
			updated_at = clock_timestamp()
			WHERE id = $1 AND status = 'running' AND attempt_id = $2 AND lease_expires_at <= clock_timestamp()`,
			item.jobID, item.attemptID, MaxAttempts, s.retryBase.Microseconds(), s.retryMax.Microseconds())
		if err != nil {
			return 0, fmt.Errorf("recover expired job: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return 0, fmt.Errorf("expired job %s changed during recovery", item.jobID)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit expired recovery: %w", err)
	}
	return len(selected), nil
}

type dueRetry struct {
	id         string
	generation int16
}

// ReleaseDue requeues eligible jobs and records publication intent together.
// RabbitMQ is never contacted while this transaction is open.
func (s *Store) ReleaseDue(ctx context.Context) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	rows, err := tx.Query(ctx, `SELECT id::text, attempt_count FROM cloudforge.jobs
		WHERE status = 'retry_wait' AND next_retry_at <= clock_timestamp()
		ORDER BY next_retry_at, id FOR UPDATE SKIP LOCKED LIMIT $1`, recoveryBatchSize)
	if err != nil {
		return 0, fmt.Errorf("select due retries: %w", err)
	}
	var selected []dueRetry
	for rows.Next() {
		var item dueRetry
		if err := rows.Scan(&item.id, &item.generation); err != nil {
			rows.Close()
			return 0, err
		}
		selected = append(selected, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	for _, item := range selected {
		tag, err := tx.Exec(ctx, `UPDATE cloudforge.jobs SET status = 'queued', next_retry_at = NULL,
			updated_at = clock_timestamp()
			WHERE id = $1 AND status = 'retry_wait' AND next_retry_at <= clock_timestamp()`, item.id)
		if err != nil {
			return 0, fmt.Errorf("release due job: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return 0, fmt.Errorf("due job %s changed during release", item.id)
		}
		payload, err := json.Marshal(outbox.JobReadyPayload{JobID: item.id})
		if err != nil {
			return 0, err
		}
		_, err = tx.Exec(ctx, `INSERT INTO cloudforge.outbox_events
			(id, event_type, aggregate_id, payload, delivery_generation)
			VALUES ($1, $2, $3, $4, $5)`, uuid.NewString(), outbox.EventTypeJobReady, item.id, payload, item.generation)
		if err != nil {
			return 0, fmt.Errorf("record retry publication intent: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit due retries: %w", err)
	}
	return len(selected), nil
}
