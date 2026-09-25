package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/6kCraze/cloudforge/internal/config"
	"github.com/6kCraze/cloudforge/internal/database"
	"github.com/6kCraze/cloudforge/internal/dispatcher"
	"github.com/6kCraze/cloudforge/internal/outbox"
	cloudrabbit "github.com/6kCraze/cloudforge/internal/rabbitmq"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("dispatcher stopped", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}
	if cfg.RabbitMQURL == "" {
		return errors.New("RABBITMQ_URL is required")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := database.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	store := outbox.NewStore(pool)
	connect := func(connectCtx context.Context) (dispatcher.Publisher, error) {
		return cloudrabbit.Open(connectCtx, cfg.RabbitMQURL, cfg.DispatcherConfirmTimeout)
	}
	runner := dispatcher.New(
		store,
		connect,
		cfg.DispatcherPollInterval,
		cfg.DispatcherBackoffMin,
		cfg.DispatcherBackoffMax,
		logger,
	)
	return runner.Run(ctx)
}
