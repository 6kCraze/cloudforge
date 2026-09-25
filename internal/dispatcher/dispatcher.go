package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/6kCraze/cloudforge/internal/outbox"
	cloudrabbit "github.com/6kCraze/cloudforge/internal/rabbitmq"
)

var ErrInvalidEvent = errors.New("invalid outbox event")

const batchSize = 32

type EventStore interface {
	ListUnpublished(context.Context, int) ([]outbox.Event, error)
	MarkPublished(context.Context, string) error
}

type Publisher interface {
	Publish(context.Context, outbox.Event) error
	Done() <-chan error
	Close() error
}

type Connector func(context.Context) (Publisher, error)

type Dispatcher struct {
	store        EventStore
	connect      Connector
	pollInterval time.Duration
	backoffMin   time.Duration
	backoffMax   time.Duration
	logger       *slog.Logger
}

func New(
	store EventStore,
	connect Connector,
	pollInterval time.Duration,
	backoffMin time.Duration,
	backoffMax time.Duration,
	logger *slog.Logger,
) *Dispatcher {
	return &Dispatcher{
		store:        store,
		connect:      connect,
		pollInterval: pollInterval,
		backoffMin:   backoffMin,
		backoffMax:   backoffMax,
		logger:       logger,
	}
}

func (d *Dispatcher) Run(ctx context.Context) error {
	d.logger.Info("dispatcher started", "poll_interval", d.pollInterval)
	defer d.logger.Info("dispatcher stopped")

	var publisher Publisher
	defer func() {
		if publisher != nil {
			_ = publisher.Close()
		}
	}()

	backoff := d.backoffMin
	hasConnected := false
	for {
		if ctx.Err() != nil {
			return nil
		}

		if publisher == nil {
			if hasConnected {
				d.logger.Info("broker reconnecting", "backoff", backoff)
			}
			connected, err := d.connect(ctx)
			if err != nil {
				d.logger.Error("broker connection failed", "error", err, "retry_in", backoff)
				if !wait(ctx, backoff) {
					return nil
				}
				backoff = nextBackoff(backoff, d.backoffMax)
				continue
			}
			publisher = connected
			if hasConnected {
				d.logger.Info("broker reconnected")
			} else {
				d.logger.Info("broker connected")
			}
			hasConnected = true
			backoff = d.backoffMin
		}

		published, err := d.DispatchBatch(ctx, publisher)
		if err != nil {
			var unroutable *cloudrabbit.UnroutableError
			if errors.As(err, &unroutable) {
				d.logger.Error("publication returned/unroutable", "message_id", unroutable.MessageID, "reply_code", unroutable.ReplyCode, "reply_text", unroutable.ReplyText)
			} else {
				d.logger.Error("publication cycle failed", "error", err)
			}
			_ = publisher.Close()
			publisher = nil
			if !wait(ctx, backoff) {
				return nil
			}
			backoff = nextBackoff(backoff, d.backoffMax)
			continue
		}
		if published > 0 {
			backoff = d.backoffMin
			continue
		}

		if !waitWithPublisher(ctx, d.pollInterval, publisher.Done(), d.logger) {
			_ = publisher.Close()
			publisher = nil
		}
	}
}

func (d *Dispatcher) DispatchBatch(ctx context.Context, publisher Publisher) (int, error) {
	events, err := d.store.ListUnpublished(ctx, batchSize)
	if err != nil {
		return 0, err
	}
	published := 0
	for _, event := range events {
		if ctx.Err() != nil {
			return published, nil
		}

		jobID, err := validate(event)
		if err != nil {
			d.logger.Error("malformed outbox event", "event_id", event.ID, "error", err)
			continue
		}
		if err := d.dispatchEvent(ctx, publisher, event, jobID); err != nil {
			return published, err
		}
		published++
	}
	return published, nil
}

func (d *Dispatcher) dispatchEvent(ctx context.Context, publisher Publisher, event outbox.Event, jobID string) error {
	d.logger.Info("event selected", "event_id", event.ID, "job_id", jobID, "event_type", event.EventType)
	d.logger.Info("publish started", "event_id", event.ID, "job_id", jobID, "routing_key", event.EventType)
	if err := publisher.Publish(ctx, event); err != nil {
		return fmt.Errorf("publish event %s: %w", event.ID, err)
	}
	d.logger.Info("publish confirmed", "event_id", event.ID, "job_id", jobID)

	// Once RabbitMQ has confirmed the message, make a short best effort to
	// record that fact even if graceful shutdown was requested concurrently.
	markCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := d.store.MarkPublished(markCtx, event.ID); err != nil {
		return err
	}
	d.logger.Info("published_at recorded", "event_id", event.ID, "job_id", jobID)
	return nil
}

func validate(event outbox.Event) (string, error) {
	if event.ID == "" || event.EventType != outbox.EventTypeJobReady || event.AggregateID == "" {
		return "", fmt.Errorf("%w: unsupported event metadata for %q", ErrInvalidEvent, event.ID)
	}
	var payload outbox.JobReadyPayload
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		return "", fmt.Errorf("%w: decode payload for %s: %v", ErrInvalidEvent, event.ID, err)
	}
	if payload.JobID == "" || payload.JobID != event.AggregateID {
		return "", fmt.Errorf("%w: event %s job_id does not match aggregate_id", ErrInvalidEvent, event.ID)
	}
	return payload.JobID, nil
}

func waitWithPublisher(ctx context.Context, duration time.Duration, done <-chan error, logger *slog.Logger) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return true
	case <-timer.C:
		return true
	case closeErr, ok := <-done:
		if ok && closeErr != nil {
			logger.Warn("broker disconnected", "error", closeErr)
		} else {
			logger.Warn("broker disconnected")
		}
		return false
	}
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
