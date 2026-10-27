package dispatcher

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

type probeRecoveryStore struct {
	expired, released int
	stop              context.CancelFunc
}

func (p *probeRecoveryStore) RecoverExpired(context.Context) (int, error) { p.expired++; return 1, nil }
func (p *probeRecoveryStore) ReleaseDue(context.Context) (int, error) {
	p.released++
	p.stop()
	return 1, nil
}

func TestRecoveryLoopRunsWithoutBrokerConnection(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &probeRecoveryStore{stop: cancel}
	done := make(chan struct{})
	go func() {
		RunRecovery(ctx, store, time.Second, slog.New(slog.NewTextHandler(io.Discard, nil)))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("recovery loop did not stop")
	}
	if store.expired != 1 || store.released != 1 {
		t.Fatalf("sweeps=%d/%d", store.expired, store.released)
	}
}
