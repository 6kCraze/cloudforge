package jobs

import (
	"context"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const MaxBatchImages = 4

// BatchInput describes an already finalized, validated input. Names are display
// metadata only; all filesystem operations use the generated InputID.
type BatchInput struct {
	InputID      string `json:"input_id"`
	OriginalName string `json:"original_name"`
	Format       string `json:"format"`
	Bytes        int64  `json:"bytes"`
	Width        int    `json:"width"`
	Height       int    `json:"height"`
}

type BatchJob struct {
	Job
	BatchInput
	OutputURL string `json:"output_url,omitempty"`
}

type Batch struct {
	ID        string     `json:"id"`
	CreatedAt time.Time  `json:"created_at"`
	JobCount  int        `json:"job_count"`
	Queued    int        `json:"queued"`
	Running   int        `json:"running"`
	RetryWait int        `json:"retry_wait"`
	Succeeded int        `json:"succeeded"`
	Failed    int        `json:"failed"`
	Jobs      []BatchJob `json:"jobs"`
}

type BatchStore interface {
	CreateBatch(context.Context, []BatchInput) (Batch, error)
	GetBatch(context.Context, string) (Batch, error)
}

func (s *Service) CreateBatch(ctx context.Context, inputs []BatchInput) (Batch, error) {
	store, ok := s.store.(BatchStore)
	if !ok {
		return Batch{}, errors.New("batch storage is unavailable")
	}
	return store.CreateBatch(ctx, inputs)
}

func (s *Service) GetBatch(ctx context.Context, id string) (Batch, error) {
	store, ok := s.store.(BatchStore)
	if !ok {
		return Batch{}, errors.New("batch storage is unavailable")
	}
	return store.GetBatch(ctx, id)
}

func (s *Store) CreateBatch(ctx context.Context, inputs []BatchInput) (Batch, error) {
	if len(inputs) < 1 || len(inputs) > MaxBatchImages {
		return Batch{}, &ValidationError{Field: "images", Message: "select between one and four images"}
	}
	seen := make(map[string]bool)
	for _, input := range inputs {
		id, err := uuid.Parse(input.InputID)
		if err != nil || id.String() != input.InputID || seen[input.InputID] ||
			!utf8.ValidString(input.OriginalName) || utf8.RuneCountInString(input.OriginalName) < 1 || utf8.RuneCountInString(input.OriginalName) > 200 ||
			(input.Format != "jpeg" && input.Format != "png") || input.Bytes < 1 || input.Bytes > 5<<20 ||
			input.Width < 1 || input.Height < 1 || input.Width > 8192 || input.Height > 8192 || int64(input.Width)*int64(input.Height) > 8_000_000 {
			return Batch{}, &ValidationError{Field: "images", Message: "invalid stored image metadata"}
		}
		seen[input.InputID] = true
	}
	batch := Batch{ID: uuid.NewString(), Jobs: make([]BatchJob, 0, len(inputs))}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Batch{}, fmt.Errorf("begin batch: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := tx.QueryRow(ctx, `INSERT INTO cloudforge.batches (id) VALUES ($1) RETURNING created_at`, batch.ID).Scan(&batch.CreatedAt); err != nil {
		return Batch{}, fmt.Errorf("insert batch: %w", err)
	}
	for position, input := range inputs {
		job, err := insertJob(ctx, tx, TypeImageThumbnail, ThumbnailPayload{InputID: input.InputID})
		if err != nil {
			return Batch{}, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO cloudforge.batch_images
            (batch_id, position, input_id, job_id, original_name, format, bytes, width, height)
            VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)`, batch.ID, position, input.InputID, job.ID, input.OriginalName, input.Format, input.Bytes, input.Width, input.Height); err != nil {
			return Batch{}, fmt.Errorf("insert batch image: %w", err)
		}
		batch.Jobs = append(batch.Jobs, BatchJob{Job: job, BatchInput: input})
	}
	summarizeBatch(&batch)
	if err := tx.Commit(ctx); err != nil {
		if commitOutcomeUncertain(err) {
			return batch, fmt.Errorf("%w: %w", ErrCommitUncertain, err)
		}
		return Batch{}, fmt.Errorf("commit batch: %w", err)
	}
	return batch, nil
}

func (s *Store) GetBatch(ctx context.Context, id string) (Batch, error) {
	// One statement gives metadata and all job states from the same PostgreSQL
	// snapshot. There are no mutable counters or status copies to drift.
	rows, err := s.pool.Query(ctx, `SELECT b.id::text, b.created_at,
        i.input_id::text, i.original_name, i.format, i.bytes, i.width, i.height,
        j.id::text, j.type, j.status, j.payload, j.result, j.error_code, j.error_message,
		j.attempt_count, j.claimed_by, j.created_at, j.updated_at, j.started_at, j.completed_at,
		j.attempt_id::text, j.lease_expires_at, j.next_retry_at
        FROM cloudforge.batches b JOIN cloudforge.batch_images i ON i.batch_id=b.id
        JOIN cloudforge.jobs j ON j.id=i.job_id WHERE b.id=$1 ORDER BY i.position`, id)
	if err != nil {
		return Batch{}, fmt.Errorf("get batch: %w", err)
	}
	defer rows.Close()
	batch := Batch{Jobs: make([]BatchJob, 0, MaxBatchImages)}
	for rows.Next() {
		var entry BatchJob
		j := &entry.Job
		i := &entry.BatchInput
		if err := rows.Scan(&batch.ID, &batch.CreatedAt, &i.InputID, &i.OriginalName, &i.Format, &i.Bytes, &i.Width, &i.Height,
			&j.ID, &j.Type, &j.Status, &j.Payload, &j.Result, &j.ErrorCode, &j.ErrorMessage, &j.AttemptCount, &j.ClaimedBy, &j.CreatedAt, &j.UpdatedAt, &j.StartedAt, &j.CompletedAt,
			&j.AttemptID, &j.LeaseExpiresAt, &j.NextRetryAt); err != nil {
			return Batch{}, fmt.Errorf("scan batch: %w", err)
		}
		batch.Jobs = append(batch.Jobs, entry)
	}
	if err := rows.Err(); err != nil {
		return Batch{}, err
	}
	if batch.ID == "" {
		return Batch{}, ErrNotFound
	}
	summarizeBatch(&batch)
	return batch, nil
}

func summarizeBatch(batch *Batch) {
	batch.JobCount = len(batch.Jobs)
	for index := range batch.Jobs {
		job := &batch.Jobs[index]
		switch job.Status {
		case StatusQueued:
			batch.Queued++
		case StatusRunning:
			batch.Running++
		case StatusRetryWait:
			batch.RetryWait++
		case StatusSucceeded:
			batch.Succeeded++
			job.OutputURL = "/v1/jobs/" + job.ID + "/thumbnail"
		case StatusFailed:
			batch.Failed++
		}
	}
}
