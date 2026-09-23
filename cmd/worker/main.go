package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/6kCraze/cloudforge/internal/config"
	"github.com/6kCraze/cloudforge/internal/database"
	"github.com/6kCraze/cloudforge/internal/jobs"
	"github.com/6kCraze/cloudforge/internal/worker"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("worker stopped", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}

	workerID := cfg.WorkerID
	if workerID == "" {
		hostname, err := os.Hostname()
		if err != nil {
			return fmt.Errorf("read hostname: %w", err)
		}
		workerID = fmt.Sprintf("%s-%d", hostname, os.Getpid())
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := database.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	store := jobs.NewStore(pool)
	runner := worker.New(store, workerID, cfg.WorkerPollInterval, jobs.Execute, logger)
	return runner.Run(ctx)
}
