package rabbitmq

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/6kCraze/cloudforge/internal/outbox"
	amqp "github.com/rabbitmq/amqp091-go"
)

const (
	ExchangeName = "cloudforge.jobs"
	QueueName    = "cloudforge.jobs.v1"
	RoutingKey   = outbox.EventTypeJobReady
)

var (
	ErrNack            = errors.New("RabbitMQ negatively acknowledged publication")
	ErrConfirmTimeout  = errors.New("RabbitMQ publisher confirmation timed out")
	ErrPublisherClosed = errors.New("RabbitMQ publisher connection closed")
)

type UnroutableError struct {
	ReplyCode int
	ReplyText string
	MessageID string
}

func (e *UnroutableError) Error() string {
	return fmt.Sprintf("RabbitMQ returned unroutable message %s: %d %s", e.MessageID, e.ReplyCode, e.ReplyText)
}

type Publisher struct {
	connection     *amqp.Connection
	channel        *amqp.Channel
	returns        <-chan amqp.Return
	done           chan error
	confirmTimeout time.Duration
	closeOnce      sync.Once
}

func Open(ctx context.Context, url string, confirmTimeout time.Duration) (*Publisher, error) {
	connection, err := openConnection(ctx, url, "cloudforge-dispatcher")
	if err != nil {
		return nil, fmt.Errorf("connect to RabbitMQ: %w", err)
	}

	channel, err := connection.Channel()
	if err != nil {
		_ = connection.Close()
		return nil, fmt.Errorf("open RabbitMQ channel: %w", err)
	}
	cleanup := func() {
		_ = channel.Close()
		_ = connection.Close()
	}

	if err := declareTopology(channel); err != nil {
		cleanup()
		return nil, err
	}
	if err := channel.Confirm(false); err != nil {
		cleanup()
		return nil, fmt.Errorf("enable RabbitMQ publisher confirms: %w", err)
	}

	returns := channel.NotifyReturn(make(chan amqp.Return, 1))
	closeNotifications := channel.NotifyClose(make(chan *amqp.Error, 1))
	done := make(chan error, 1)
	go func() {
		if closeErr, ok := <-closeNotifications; ok && closeErr != nil {
			done <- closeErr
		}
		close(done)
	}()

	return &Publisher{
		connection:     connection,
		channel:        channel,
		returns:        returns,
		done:           done,
		confirmTimeout: confirmTimeout,
	}, nil
}

// Publish sends one persistent message with mandatory routing and waits for the
// broker's publisher confirmation. The dispatcher uses one serial publisher, so
// returned-message correlation remains intentionally simple.
func (p *Publisher) Publish(ctx context.Context, event outbox.Event) error {
	// The AMQP library's PublishWithDeferredConfirmWithContext method does not
	// itself honor context cancellation. Closing the connection unblocks a
	// stalled network write during shutdown.
	stopCloseOnCancel := context.AfterFunc(ctx, func() {
		_ = p.connection.Close()
	})
	confirmation, err := p.channel.PublishWithDeferredConfirmWithContext(
		ctx,
		ExchangeName,
		event.EventType,
		true,
		false,
		amqp.Publishing{
			Headers:      amqp.Table{"schema_version": int32(1)},
			ContentType:  "application/json",
			DeliveryMode: amqp.Persistent,
			MessageId:    event.ID,
			Timestamp:    event.CreatedAt,
			Type:         event.EventType,
			AppId:        "cloudforge-dispatcher",
			Body:         event.Payload,
		},
	)
	stopCloseOnCancel()
	if err != nil {
		return fmt.Errorf("publish RabbitMQ message: %w", err)
	}

	waitCtx, cancel := context.WithTimeout(ctx, p.confirmTimeout)
	defer cancel()

	var returned *amqp.Return
	for {
		select {
		case result, ok := <-p.returns:
			if ok && result.MessageId == event.ID {
				copy := result
				returned = &copy
			}
		case <-confirmation.Done():
			// RabbitMQ sends basic.return before basic.ack for mandatory,
			// unroutable publications. Drain the already-enqueued return before
			// accepting the confirmation.
			select {
			case result, ok := <-p.returns:
				if ok && result.MessageId == event.ID {
					copy := result
					returned = &copy
				}
			default:
			}
			if !confirmation.Acked() {
				return ErrNack
			}
			if returned != nil {
				return &UnroutableError{
					ReplyCode: int(returned.ReplyCode),
					ReplyText: returned.ReplyText,
					MessageID: returned.MessageId,
				}
			}
			return nil
		case closeErr, ok := <-p.done:
			if ok && closeErr != nil {
				return fmt.Errorf("%w: %v", ErrPublisherClosed, closeErr)
			}
			return ErrPublisherClosed
		case <-waitCtx.Done():
			if errors.Is(waitCtx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
				return ErrConfirmTimeout
			}
			return ctx.Err()
		}
	}
}

func (p *Publisher) Done() <-chan error {
	return p.done
}

func (p *Publisher) Close() error {
	var closeErr error
	p.closeOnce.Do(func() {
		if err := p.channel.Close(); err != nil && !errors.Is(err, amqp.ErrClosed) {
			closeErr = err
		}
		if err := p.connection.Close(); err != nil && !errors.Is(err, amqp.ErrClosed) && closeErr == nil {
			closeErr = err
		}
	})
	return closeErr
}
