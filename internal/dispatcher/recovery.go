package dispatcher

import (
	"context"
	"log/slog"
	"time"
)

type RecoveryStore interface {
	RecoverExpired(context.Context) (int, error)
	ReleaseDue(context.Context) (int, error)
}

// RunRecovery is a separate lightweight loop in the dispatcher process. It
// never needs a RabbitMQ connection; due work is handed to the normal outbox.
func RunRecovery(ctx context.Context, store RecoveryStore, interval time.Duration, logger *slog.Logger) {
	logger.Info("job recovery started", "poll_interval", interval)
	defer logger.Info("job recovery stopped")
	for ctx.Err() == nil {
		sweepCtx, cancelSweep := context.WithTimeout(ctx, 5*time.Second)
		expired, err := store.RecoverExpired(sweepCtx)
		cancelSweep()
		if err != nil && ctx.Err() == nil {
			logger.Error("expired-job recovery failed", "error", err)
		}
		if err == nil {
			if expired > 0 {
				logger.Info("expired jobs recovered", "count", expired)
			}
			releaseCtx, cancelRelease := context.WithTimeout(ctx, 5*time.Second)
			due, releaseErr := store.ReleaseDue(releaseCtx)
			cancelRelease()
			if releaseErr != nil && ctx.Err() == nil {
				logger.Error("due-retry release failed", "error", releaseErr)
			} else if due > 0 {
				logger.Info("retry jobs queued with outbox events", "count", due)
			}
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
