package rabbitmq

import (
	"context"
	"fmt"
	"net"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

func openConnection(ctx context.Context, url, connectionName string) (*amqp.Connection, error) {
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	connection, err := amqp.DialConfig(url, amqp.Config{
		Heartbeat:  10 * time.Second,
		Properties: amqp.Table{"connection_name": connectionName},
		Dial: func(network, address string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, address)
		},
	})
	if err != nil {
		return nil, fmt.Errorf("connect to RabbitMQ: %w", err)
	}
	return connection, nil
}

func declareTopology(channel *amqp.Channel) error {
	if err := channel.ExchangeDeclare(ExchangeName, "direct", true, false, false, false, nil); err != nil {
		return fmt.Errorf("declare RabbitMQ exchange: %w", err)
	}
	if _, err := channel.QueueDeclare(QueueName, true, false, false, false, nil); err != nil {
		return fmt.Errorf("declare RabbitMQ queue: %w", err)
	}
	if err := channel.QueueBind(QueueName, RoutingKey, ExchangeName, false, nil); err != nil {
		return fmt.Errorf("bind RabbitMQ queue: %w", err)
	}
	return nil
}
