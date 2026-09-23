package worker

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/6kCraze/cloudforge/internal/jobs"
)

type JobStore interface {
	ClaimNext(context.Context, string) (jobs.Job, error)
	MarkSucceeded(context.Context, string, string, json.RawMessage) error
	MarkFailed(context.Context, string, string, string, string) error
}

type Processor func(context.Context, jobs.Job) (json.RawMessage, error)

type Worker struct {
	store        JobStore
	workerID     string
	pollInterval time.Duration
	processor    Processor
	logger       *slog.Logger
}

func New(store JobStore, workerID string, pollInterval time.Duration, processor Processor, logger *slog.Logger) *Worker {
	return &Worker{store: store, workerID: workerID, pollInterval: pollInterval, processor: processor, logger: logger}
}

func (w *Worker) Run(ctx context.Context) error {
	w.logger.Info("worker started", "worker_id", w.workerID, "poll_interval", w.pollInterval)
	defer w.logger.Info("worker stopped", "worker_id", w.workerID)

	for {
		if ctx.Err() != nil {
			return nil
		}

		processed, err := w.ProcessOne(ctx)
		if err != nil {
			w.logger.Error("worker cycle failed", "worker_id", w.workerID, "error", err)
		}
		if processed {
			continue
		}

		timer := time.NewTimer(w.pollInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil
		case <-timer.C:
		}
	}
}

func (w *Worker) ProcessOne(ctx context.Context) (bool, error) {
	job, err := w.store.ClaimNext(ctx, w.workerID)
	if errors.Is(err, jobs.ErrNoJobs) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	// Once claimed, graceful cancellation must not interrupt the terminal update.
	workCtx := context.WithoutCancel(ctx)
	result, err := w.processor(workCtx, job)
	if err != nil {
		if markErr := w.store.MarkFailed(workCtx, job.ID, w.workerID, "execution_failed", err.Error()); markErr != nil {
			return true, markErr
		}
		w.logger.Warn("job failed", "job_id", job.ID, "worker_id", w.workerID, "error", err)
		return true, nil
	}
	if err := w.store.MarkSucceeded(workCtx, job.ID, w.workerID, result); err != nil {
		return true, err
	}
	w.logger.Info("job succeeded", "job_id", job.ID, "worker_id", w.workerID)
	return true, nil
}
