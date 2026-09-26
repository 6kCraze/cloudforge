package rabbitmq

import (
	"context"
	"errors"
	"fmt"
	"sync"

	amqp "github.com/rabbitmq/amqp091-go"
)

const WorkerPrefetch = 1

type Consumer struct {
	connection *amqp.Connection
	channel    *amqp.Channel
	deliveries <-chan amqp.Delivery
	done       chan error
	tag        string
	closeOnce  sync.Once
}

func OpenConsumer(ctx context.Context, url, workerID string) (*Consumer, error) {
	connection, err := openConnection(ctx, url, "cloudforge-worker-"+workerID)
	if err != nil {
		return nil, err
	}
	channel, err := connection.Channel()
	if err != nil {
		_ = connection.Close()
		return nil, fmt.Errorf("open RabbitMQ consumer channel: %w", err)
	}
	cleanup := func() {
		_ = channel.Close()
		_ = connection.Close()
	}
	if err := declareTopology(channel); err != nil {
		cleanup()
		return nil, err
	}
	if err := channel.Qos(WorkerPrefetch, 0, false); err != nil {
		cleanup()
		return nil, fmt.Errorf("configure RabbitMQ prefetch: %w", err)
	}
	tag := "cloudforge-worker-" + workerID
	deliveries, err := channel.Consume(QueueName, tag, false, false, false, false, nil)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("start RabbitMQ consumer: %w", err)
	}

	closeNotifications := channel.NotifyClose(make(chan *amqp.Error, 1))
	cancelNotifications := channel.NotifyCancel(make(chan string, 1))
	done := make(chan error, 1)
	go func() {
		select {
		case closeErr, ok := <-closeNotifications:
			if ok && closeErr != nil {
				done <- closeErr
			}
		case cancelledTag, ok := <-cancelNotifications:
			if ok {
				done <- fmt.Errorf("RabbitMQ consumer %s was cancelled by broker", cancelledTag)
			}
		}
		close(done)
	}()

	return &Consumer{
		connection: connection,
		channel:    channel,
		deliveries: deliveries,
		done:       done,
		tag:        tag,
	}, nil
}

func (c *Consumer) Deliveries() <-chan amqp.Delivery { return c.deliveries }
func (c *Consumer) Done() <-chan error               { return c.done }

func (c *Consumer) Cancel() error {
	if err := c.channel.Cancel(c.tag, false); err != nil && !errors.Is(err, amqp.ErrClosed) {
		return err
	}
	return nil
}

func (c *Consumer) Close() error {
	var closeErr error
	c.closeOnce.Do(func() {
		if err := c.channel.Close(); err != nil && !errors.Is(err, amqp.ErrClosed) {
			closeErr = err
		}
		if err := c.connection.Close(); err != nil && !errors.Is(err, amqp.ErrClosed) && closeErr == nil {
			closeErr = err
		}
	})
	return closeErr
}
