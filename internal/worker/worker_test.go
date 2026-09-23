package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/6kCraze/cloudforge/internal/jobs"
)

type fakeStore struct {
	claim func(context.Context, string) (jobs.Job, error)
	done  func(context.Context, string, string, json.RawMessage) error
	fail  func(context.Context, string, string, string, string) error
}

func (f fakeStore) ClaimNext(ctx context.Context, id string) (jobs.Job, error) {
	return f.claim(ctx, id)
}

func (f fakeStore) MarkSucceeded(ctx context.Context, id, workerID string, result json.RawMessage) error {
	return f.done(ctx, id, workerID, result)
}

func (f fakeStore) MarkFailed(ctx context.Context, id, workerID, code, message string) error {
	return f.fail(ctx, id, workerID, code, message)
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestProcessOnePersistsFailure(t *testing.T) {
	var failed bool
	store := fakeStore{
		claim: func(context.Context, string) (jobs.Job, error) {
			return jobs.Job{ID: "job-1", Status: jobs.StatusRunning}, nil
		},
		done: func(context.Context, string, string, json.RawMessage) error {
			t.Fatal("success must not be recorded")
			return nil
		},
		fail: func(_ context.Context, id, workerID, code, message string) error {
			failed = true
			if id != "job-1" || workerID != "worker-a" || code != "execution_failed" || message != "boom" {
				t.Fatalf("failure values: %q %q %q %q", id, workerID, code, message)
			}
			return nil
		},
	}
	w := New(store, "worker-a", time.Millisecond, func(context.Context, jobs.Job) (json.RawMessage, error) {
		return nil, errors.New("boom")
	}, discardLogger())

	processed, err := w.ProcessOne(context.Background())
	if err != nil || !processed || !failed {
		t.Fatalf("processed=%v failed=%v err=%v", processed, failed, err)
	}
}

func TestRunFinishesClaimedJobAfterCancellation(t *testing.T) {
	processing := make(chan struct{})
	release := make(chan struct{})
	completed := make(chan struct{})
	claims := 0
	store := fakeStore{
		claim: func(context.Context, string) (jobs.Job, error) {
			claims++
			if claims > 1 {
				return jobs.Job{}, jobs.ErrNoJobs
			}
			return jobs.Job{ID: "job-1", Status: jobs.StatusRunning}, nil
		},
		done: func(context.Context, string, string, json.RawMessage) error {
			close(completed)
			return nil
		},
		fail: func(context.Context, string, string, string, string) error { return nil },
	}
	w := New(store, "worker-a", time.Millisecond, func(ctx context.Context, _ jobs.Job) (json.RawMessage, error) {
		close(processing)
		<-release
		if ctx.Err() != nil {
			t.Fatalf("processor context was cancelled: %v", ctx.Err())
		}
		return json.RawMessage(`{"ok":true}`), nil
	}, discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	<-processing
	cancel()
	close(release)
	select {
	case <-completed:
	case <-time.After(time.Second):
		t.Fatal("claimed job did not finish")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
