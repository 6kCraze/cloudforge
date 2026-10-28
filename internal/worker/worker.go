package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/6kCraze/cloudforge/internal/images"
	"github.com/6kCraze/cloudforge/internal/jobs"
	"github.com/6kCraze/cloudforge/internal/outbox"
	"github.com/google/uuid"
	amqp "github.com/rabbitmq/amqp091-go"
)

type JobStore interface {
	Get(context.Context, string) (jobs.Job, error)
	ClaimByID(context.Context, string, string) (jobs.Job, error)
	Heartbeat(context.Context, string, string, string) error
	MarkSucceeded(context.Context, string, string, string, json.RawMessage) error
	MarkFailed(context.Context, string, string, string, string, string) error
	ScheduleRetry(context.Context, string, string, string, string, string) (string, error)
}

type Processor func(context.Context, jobs.Job) (json.RawMessage, error)

type Outcome string

const (
	OutcomeExecuted          Outcome = "executed"
	OutcomeTerminalDuplicate Outcome = "terminal_duplicate"
	OutcomeRunningDeferred   Outcome = "running_deferred"
	OutcomeRetryDeferred     Outcome = "retry_deferred"
	OutcomeUnknownJob        Outcome = "unknown_job"
)

type Handler struct {
	store             JobStore
	workerID          string
	processor         Processor
	logger            *slog.Logger
	heartbeatInterval time.Duration
}

func NewHandler(store JobStore, workerID string, processor Processor, logger *slog.Logger, interval ...time.Duration) *Handler {
	heartbeatInterval := 5 * time.Second
	if len(interval) != 0 {
		heartbeatInterval = interval[0]
	}
	return &Handler{store: store, workerID: workerID, processor: processor, logger: logger, heartbeatInterval: heartbeatInterval}
}

// Handle reconciles a RabbitMQ delivery with PostgreSQL before doing work.
// PostgreSQL is authoritative: only queued jobs can be claimed and executed.
func (h *Handler) Handle(ctx context.Context, jobID string) (Outcome, error) {
	// The second pass handles the narrow race where another duplicate delivery
	// changes queued state between Get and ClaimByID.
	for pass := 0; pass < 2; pass++ {
		job, err := h.store.Get(ctx, jobID)
		if errors.Is(err, jobs.ErrNotFound) {
			h.logger.Warn("unknown job delivery", "job_id", jobID, "worker_id", h.workerID)
			return OutcomeUnknownJob, nil
		}
		if err != nil {
			return "", fmt.Errorf("load delivered job: %w", err)
		}

		switch job.Status {
		case jobs.StatusSucceeded, jobs.StatusFailed:
			h.logger.Info("terminal duplicate skipped", "job_id", jobID, "worker_id", h.workerID, "status", job.Status)
			return OutcomeTerminalDuplicate, nil
		case jobs.StatusRunning:
			h.logger.Warn("running job delivery deferred", "job_id", jobID, "worker_id", h.workerID, "status", job.Status, "claimed_by", job.ClaimedBy)
			return OutcomeRunningDeferred, nil
		case jobs.StatusRetryWait:
			h.logger.Info("retry waiting; duplicate delivery skipped", "job_id", jobID, "worker_id", h.workerID, "next_retry_at", job.NextRetryAt)
			return OutcomeRetryDeferred, nil
		case jobs.StatusQueued:
			claimed, claimErr := h.store.ClaimByID(ctx, jobID, h.workerID)
			if errors.Is(claimErr, jobs.ErrNotClaimable) {
				continue
			}
			if claimErr != nil {
				return "", fmt.Errorf("claim delivered job: %w", claimErr)
			}
			h.logger.Info("job claimed", "job_id", jobID, "worker_id", h.workerID, "status", claimed.Status, "attempt_count", claimed.AttemptCount, "attempt_id", claimed.AttemptID)
			return h.execute(ctx, claimed)
		default:
			return "", fmt.Errorf("job %s has unsupported status %q", jobID, job.Status)
		}
	}
	return "", fmt.Errorf("claim delivered job: state remained queued after a competing claim")
}

func (h *Handler) execute(ctx context.Context, job jobs.Job) (Outcome, error) {
	if job.AttemptID == nil || *job.AttemptID == "" {
		return "", fmt.Errorf("claimed job has no attempt ID")
	}
	attemptID := *job.AttemptID
	h.logger.Info("job started", "job_id", job.ID, "worker_id", h.workerID, "attempt_count", job.AttemptCount, "attempt_id", attemptID, "status", jobs.StatusRunning)
	processCtx, cancel := context.WithCancel(ctx)
	heartbeatDone := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(h.heartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-processCtx.Done():
				heartbeatDone <- nil
				return
			case <-ticker.C:
				renewCtx, stopRenew := context.WithTimeout(processCtx, h.heartbeatInterval)
				err := h.store.Heartbeat(renewCtx, job.ID, h.workerID, attemptID)
				stopRenew()
				if err != nil {
					cancel()
					heartbeatDone <- fmt.Errorf("renew attempt lease: %w", err)
					return
				}
			}
		}
	}()
	result, err := h.processor(processCtx, job)
	cancel()
	if heartbeatErr := <-heartbeatDone; heartbeatErr != nil {
		return "", heartbeatErr
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil {
		if permanentFailure(err) {
			if markErr := h.store.MarkFailed(ctx, job.ID, h.workerID, attemptID, permanentCode(err), err.Error()); markErr != nil {
				return "", fmt.Errorf("record permanent job failure: %w", markErr)
			}
			h.logger.Warn("job permanently failed", "job_id", job.ID, "worker_id", h.workerID, "attempt_id", attemptID, "attempt_count", job.AttemptCount, "status", jobs.StatusFailed, "error", err)
			return OutcomeExecuted, nil
		}
		status, markErr := h.store.ScheduleRetry(ctx, job.ID, h.workerID, attemptID, "execution_failed", err.Error())
		if markErr != nil {
			return "", fmt.Errorf("record recoverable job failure: %w", markErr)
		}
		h.logger.Warn("job attempt failed", "job_id", job.ID, "worker_id", h.workerID, "attempt_id", attemptID, "attempt_count", job.AttemptCount, "status", status, "error", err)
		return OutcomeExecuted, nil
	}
	if err := h.store.MarkSucceeded(ctx, job.ID, h.workerID, attemptID, result); err != nil {
		return "", fmt.Errorf("record job success: %w", err)
	}
	h.logger.Info("job completed", "job_id", job.ID, "worker_id", h.workerID, "attempt_id", attemptID, "attempt_count", job.AttemptCount, "status", jobs.StatusSucceeded)
	return OutcomeExecuted, nil
}

func permanentFailure(err error) bool {
	var validation *jobs.ValidationError
	return errors.Is(err, jobs.ErrPermanentJob) || errors.As(err, &validation) ||
		errors.Is(err, images.ErrInvalid) || errors.Is(err, images.ErrUnsupported) ||
		errors.Is(err, images.ErrDimensions) || errors.Is(err, images.ErrTooLarge)
}

func permanentCode(err error) string {
	if errors.Is(err, jobs.ErrPermanentJob) {
		return "invalid_job"
	}
	return "invalid_image"
}

type DeliveryConsumer interface {
	Deliveries() <-chan amqp.Delivery
	Done() <-chan error
	Cancel() error
	Close() error
}

type Connector func(context.Context) (DeliveryConsumer, error)

type RabbitWorker struct {
	handler    *Handler
	connect    Connector
	workerID   string
	backoffMin time.Duration
	backoffMax time.Duration
	logger     *slog.Logger
}

func NewRabbitWorker(handler *Handler, connect Connector, workerID string, backoffMin, backoffMax time.Duration, logger *slog.Logger) *RabbitWorker {
	return &RabbitWorker{handler: handler, connect: connect, workerID: workerID, backoffMin: backoffMin, backoffMax: backoffMax, logger: logger}
}

func (w *RabbitWorker) Run(ctx context.Context) error {
	w.logger.Info("worker started", "worker_id", w.workerID, "source", "rabbitmq")
	defer w.logger.Info("worker stopped", "worker_id", w.workerID)

	backoff := w.backoffMin
	hasConnected := false
	for ctx.Err() == nil {
		consumer, err := w.connect(ctx)
		if err != nil {
			w.logger.Error("broker connection failed", "worker_id", w.workerID, "error", err, "retry_in", backoff)
			if !wait(ctx, backoff) {
				return nil
			}
			backoff = nextBackoff(backoff, w.backoffMax)
			continue
		}
		if hasConnected {
			w.logger.Info("broker reconnected", "worker_id", w.workerID)
		} else {
			w.logger.Info("broker connected", "worker_id", w.workerID)
		}
		hasConnected = true

		cancelFinished := make(chan struct{})
		stopCancel := context.AfterFunc(ctx, func() {
			_ = consumer.Cancel()
			close(cancelFinished)
		})
		var handled bool
		handled, err = w.consume(ctx, consumer)
		if !stopCancel() {
			<-cancelFinished
		}
		_ = consumer.Close()
		if ctx.Err() != nil {
			return nil
		}
		if handled {
			// A successfully handled delivery proves the full broker/database path
			// is healthy. Merely reconnecting to RabbitMQ does not: PostgreSQL may
			// still be unavailable and the same requeued message may immediately fail.
			backoff = w.backoffMin
		}
		if err != nil {
			w.logger.Warn("consumer interrupted", "worker_id", w.workerID, "error", err, "retry_in", backoff)
		}
		if !wait(ctx, backoff) {
			return nil
		}
		backoff = nextBackoff(backoff, w.backoffMax)
	}
	return nil
}

func (w *RabbitWorker) consume(ctx context.Context, consumer DeliveryConsumer) (bool, error) {
	handled := false
	for {
		select {
		case <-ctx.Done():
			return handled, nil
		case err, ok := <-consumer.Done():
			if ok && err != nil {
				return handled, err
			}
			return handled, errors.New("RabbitMQ consumer closed")
		case delivery, ok := <-consumer.Deliveries():
			if !ok {
				return handled, errors.New("RabbitMQ deliveries closed")
			}
			// Once accepted locally, graceful cancellation must not interrupt the
			// database terminal update or the following acknowledgement.
			if err := w.handleDelivery(context.WithoutCancel(ctx), delivery); err != nil {
				return handled, err
			}
			handled = true
		}
	}
}

func (w *RabbitWorker) handleDelivery(ctx context.Context, delivery amqp.Delivery) error {
	jobID, validationErr := validateDelivery(delivery)
	w.logger.Info("message received", "worker_id", w.workerID, "job_id", jobID, "message_id", delivery.MessageId, "delivery_tag", delivery.DeliveryTag, "redelivered", delivery.Redelivered)
	if validationErr != nil {
		w.logger.Error("malformed message dropped", "worker_id", w.workerID, "message_id", delivery.MessageId, "delivery_tag", delivery.DeliveryTag, "error", validationErr)
		if err := delivery.Ack(false); err != nil {
			return fmt.Errorf("ack malformed message: %w", err)
		}
		return nil
	}

	outcome, err := w.handler.Handle(ctx, jobID)
	if err != nil {
		w.logger.Error("message processing deferred", "worker_id", w.workerID, "job_id", jobID, "message_id", delivery.MessageId, "error", err)
		if nackErr := delivery.Nack(false, true); nackErr != nil {
			return fmt.Errorf("process job: %v; nack delivery: %w", err, nackErr)
		}
		w.logger.Warn("message negatively acknowledged", "worker_id", w.workerID, "job_id", jobID, "message_id", delivery.MessageId, "requeue", true)
		return fmt.Errorf("process job %s: %w", jobID, err)
	}
	if err := delivery.Ack(false); err != nil {
		return fmt.Errorf("ack job %s: %w", jobID, err)
	}
	w.logger.Info("message acknowledged", "worker_id", w.workerID, "job_id", jobID, "message_id", delivery.MessageId, "outcome", outcome)
	return nil
}

func validateDelivery(delivery amqp.Delivery) (string, error) {
	if delivery.ContentType != "application/json" {
		return "", fmt.Errorf("content_type must be application/json")
	}
	if delivery.Type != outbox.EventTypeJobReady {
		return "", fmt.Errorf("type must be %s", outbox.EventTypeJobReady)
	}
	if _, err := uuid.Parse(delivery.MessageId); err != nil {
		return "", fmt.Errorf("message_id must be a UUID")
	}
	var payload outbox.JobReadyPayload
	if err := json.Unmarshal(delivery.Body, &payload); err != nil {
		return "", fmt.Errorf("decode body: %w", err)
	}
	if _, err := uuid.Parse(payload.JobID); err != nil {
		return "", fmt.Errorf("job_id must be a UUID")
	}
	return payload.JobID, nil
}

func wait(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func nextBackoff(current, maximum time.Duration) time.Duration {
	if current >= maximum/2 {
		return maximum
	}
	return current * 2
}
