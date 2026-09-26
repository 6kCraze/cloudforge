package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/6kCraze/cloudforge/internal/jobs"
	"github.com/6kCraze/cloudforge/internal/outbox"
	"github.com/google/uuid"
	amqp "github.com/rabbitmq/amqp091-go"
)

type fakeStore struct {
	get   func(context.Context, string) (jobs.Job, error)
	claim func(context.Context, string, string) (jobs.Job, error)
	done  func(context.Context, string, string, json.RawMessage) error
	fail  func(context.Context, string, string, string, string) error
}

func (f fakeStore) Get(ctx context.Context, id string) (jobs.Job, error) {
	return f.get(ctx, id)
}
func (f fakeStore) ClaimByID(ctx context.Context, id, workerID string) (jobs.Job, error) {
	return f.claim(ctx, id, workerID)
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

func TestHandlerExecutesQueuedJobAndPersistsSuccess(t *testing.T) {
	jobID := uuid.NewString()
	claimed := false
	completed := false
	store := fakeStore{
		get: func(context.Context, string) (jobs.Job, error) {
			return jobs.Job{ID: jobID, Status: jobs.StatusQueued}, nil
		},
		claim: func(_ context.Context, id, workerID string) (jobs.Job, error) {
			claimed = true
			if id != jobID || workerID != "worker-a" {
				t.Fatalf("claim values = %q/%q", id, workerID)
			}
			return jobs.Job{ID: jobID, Status: jobs.StatusRunning, AttemptCount: 1}, nil
		},
		done: func(_ context.Context, id, workerID string, result json.RawMessage) error {
			completed = true
			if id != jobID || workerID != "worker-a" || string(result) != `{"ok":true}` {
				t.Fatalf("success values = %q/%q/%s", id, workerID, result)
			}
			return nil
		},
		fail: func(context.Context, string, string, string, string) error {
			t.Fatal("failure must not be recorded")
			return nil
		},
	}
	handler := NewHandler(store, "worker-a", func(context.Context, jobs.Job) (json.RawMessage, error) {
		return json.RawMessage(`{"ok":true}`), nil
	}, discardLogger())

	outcome, err := handler.Handle(context.Background(), jobID)
	if err != nil || outcome != OutcomeExecuted || !claimed || !completed {
		t.Fatalf("outcome=%q claimed=%v completed=%v err=%v", outcome, claimed, completed, err)
	}
}

func TestHandlerPersistsProcessorFailure(t *testing.T) {
	jobID := uuid.NewString()
	failed := false
	store := fakeStore{
		get: func(context.Context, string) (jobs.Job, error) {
			return jobs.Job{ID: jobID, Status: jobs.StatusQueued}, nil
		},
		claim: func(context.Context, string, string) (jobs.Job, error) {
			return jobs.Job{ID: jobID, Status: jobs.StatusRunning}, nil
		},
		done: func(context.Context, string, string, json.RawMessage) error {
			t.Fatal("success must not be recorded")
			return nil
		},
		fail: func(_ context.Context, id, workerID, code, message string) error {
			failed = true
			if id != jobID || workerID != "worker-a" || code != "execution_failed" || message != "boom" {
				t.Fatalf("failure values = %q/%q/%q/%q", id, workerID, code, message)
			}
			return nil
		},
	}
	handler := NewHandler(store, "worker-a", func(context.Context, jobs.Job) (json.RawMessage, error) {
		return nil, errors.New("boom")
	}, discardLogger())

	outcome, err := handler.Handle(context.Background(), jobID)
	if err != nil || outcome != OutcomeExecuted || !failed {
		t.Fatalf("outcome=%q failed=%v err=%v", outcome, failed, err)
	}
}

func TestHandlerSkipsNonExecutableDatabaseStates(t *testing.T) {
	tests := []struct {
		name    string
		status  string
		getErr  error
		outcome Outcome
	}{
		{name: "succeeded duplicate", status: jobs.StatusSucceeded, outcome: OutcomeTerminalDuplicate},
		{name: "failed duplicate", status: jobs.StatusFailed, outcome: OutcomeTerminalDuplicate},
		{name: "running remains stuck", status: jobs.StatusRunning, outcome: OutcomeRunningDeferred},
		{name: "unknown is dropped", getErr: jobs.ErrNotFound, outcome: OutcomeUnknownJob},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := fakeStore{
				get: func(context.Context, string) (jobs.Job, error) {
					return jobs.Job{ID: uuid.NewString(), Status: test.status}, test.getErr
				},
				claim: func(context.Context, string, string) (jobs.Job, error) {
					t.Fatal("non-queued job must not be claimed")
					return jobs.Job{}, nil
				},
				done: func(context.Context, string, string, json.RawMessage) error { return nil },
				fail: func(context.Context, string, string, string, string) error { return nil },
			}
			handler := NewHandler(store, "worker-a", func(context.Context, jobs.Job) (json.RawMessage, error) {
				t.Fatal("non-queued job must not execute")
				return nil, nil
			}, discardLogger())
			outcome, err := handler.Handle(context.Background(), uuid.NewString())
			if err != nil || outcome != test.outcome {
				t.Fatalf("outcome=%q err=%v, want %q", outcome, err, test.outcome)
			}
		})
	}
}

type fakeAcknowledger struct {
	mu        sync.Mutex
	acks      int
	nacks     int
	requeue   bool
	ackSignal chan struct{}
}

func (f *fakeAcknowledger) Ack(uint64, bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acks++
	if f.ackSignal != nil {
		select {
		case f.ackSignal <- struct{}{}:
		default:
		}
	}
	return nil
}
func (f *fakeAcknowledger) Nack(_ uint64, _ bool, requeue bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nacks++
	f.requeue = requeue
	return nil
}
func (f *fakeAcknowledger) Reject(uint64, bool) error { return nil }

func validDelivery(ack amqp.Acknowledger, jobID string) amqp.Delivery {
	body, _ := json.Marshal(outbox.JobReadyPayload{JobID: jobID})
	return amqp.Delivery{
		Acknowledger: ack,
		DeliveryTag:  1,
		ContentType:  "application/json",
		Type:         outbox.EventTypeJobReady,
		MessageId:    uuid.NewString(),
		Body:         body,
	}
}

func TestMalformedDeliveryIsAcknowledgedWithoutDatabaseAccess(t *testing.T) {
	ack := &fakeAcknowledger{}
	delivery := validDelivery(ack, uuid.NewString())
	delivery.Body = []byte(`{"job_id":`)
	store := fakeStore{get: func(context.Context, string) (jobs.Job, error) {
		t.Fatal("malformed delivery must not access database")
		return jobs.Job{}, nil
	}}
	runner := NewRabbitWorker(NewHandler(store, "worker-a", nil, discardLogger()), nil, "worker-a", time.Millisecond, time.Second, discardLogger())

	if err := runner.handleDelivery(context.Background(), delivery); err != nil {
		t.Fatal(err)
	}
	if ack.acks != 1 || ack.nacks != 0 {
		t.Fatalf("acks=%d nacks=%d", ack.acks, ack.nacks)
	}
}

func TestTemporaryDatabaseFailureNacksAndRequeues(t *testing.T) {
	ack := &fakeAcknowledger{}
	store := fakeStore{get: func(context.Context, string) (jobs.Job, error) {
		return jobs.Job{}, errors.New("database unavailable")
	}}
	runner := NewRabbitWorker(NewHandler(store, "worker-a", nil, discardLogger()), nil, "worker-a", time.Millisecond, time.Second, discardLogger())

	if err := runner.handleDelivery(context.Background(), validDelivery(ack, uuid.NewString())); err == nil {
		t.Fatal("expected processing error")
	}
	if ack.acks != 0 || ack.nacks != 1 || !ack.requeue {
		t.Fatalf("acks=%d nacks=%d requeue=%v", ack.acks, ack.nacks, ack.requeue)
	}
}

type fakeConsumer struct {
	deliveries chan amqp.Delivery
	done       chan error
	cancelled  chan struct{}
	once       sync.Once
}

func (f *fakeConsumer) Deliveries() <-chan amqp.Delivery { return f.deliveries }
func (f *fakeConsumer) Done() <-chan error               { return f.done }
func (f *fakeConsumer) Cancel() error {
	f.once.Do(func() { close(f.cancelled) })
	return nil
}
func (f *fakeConsumer) Close() error { return nil }

func TestGracefulShutdownStopsIntakeAndFinishesCurrentDelivery(t *testing.T) {
	jobID := uuid.NewString()
	processing := make(chan struct{})
	release := make(chan struct{})
	ackSignal := make(chan struct{}, 1)
	ack := &fakeAcknowledger{ackSignal: ackSignal}
	store := fakeStore{
		get: func(context.Context, string) (jobs.Job, error) {
			return jobs.Job{ID: jobID, Status: jobs.StatusQueued}, nil
		},
		claim: func(context.Context, string, string) (jobs.Job, error) {
			return jobs.Job{ID: jobID, Status: jobs.StatusRunning}, nil
		},
		done: func(ctx context.Context, _, _ string, _ json.RawMessage) error {
			if ctx.Err() != nil {
				t.Fatalf("terminal update context was cancelled: %v", ctx.Err())
			}
			return nil
		},
		fail: func(context.Context, string, string, string, string) error { return nil },
	}
	handler := NewHandler(store, "worker-a", func(ctx context.Context, _ jobs.Job) (json.RawMessage, error) {
		close(processing)
		<-release
		if ctx.Err() != nil {
			t.Fatalf("processor context was cancelled: %v", ctx.Err())
		}
		return json.RawMessage(`{"ok":true}`), nil
	}, discardLogger())
	consumer := &fakeConsumer{
		deliveries: make(chan amqp.Delivery, 1),
		done:       make(chan error),
		cancelled:  make(chan struct{}),
	}
	consumer.deliveries <- validDelivery(ack, jobID)
	runner := NewRabbitWorker(handler, func(context.Context) (DeliveryConsumer, error) { return consumer, nil }, "worker-a", time.Millisecond, time.Second, discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	go func() { finished <- runner.Run(ctx) }()

	<-processing
	cancel()
	select {
	case <-consumer.cancelled:
	case <-time.After(time.Second):
		t.Fatal("consumer intake was not cancelled")
	}
	close(release)
	select {
	case <-ackSignal:
	case <-time.After(time.Second):
		t.Fatal("current delivery was not acknowledged")
	}
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
}

func TestWorkerReconnectsAfterConsumerFailure(t *testing.T) {
	first := &fakeConsumer{deliveries: make(chan amqp.Delivery), done: make(chan error, 1), cancelled: make(chan struct{})}
	second := &fakeConsumer{deliveries: make(chan amqp.Delivery), done: make(chan error), cancelled: make(chan struct{})}
	first.done <- errors.New("broker restarted")
	connectedAgain := make(chan struct{})
	attempts := 0
	connect := func(context.Context) (DeliveryConsumer, error) {
		attempts++
		if attempts == 1 {
			return first, nil
		}
		close(connectedAgain)
		return second, nil
	}
	store := fakeStore{}
	runner := NewRabbitWorker(NewHandler(store, "worker-a", nil, discardLogger()), connect, "worker-a", time.Millisecond, 5*time.Millisecond, discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()

	select {
	case <-connectedAgain:
	case <-time.After(time.Second):
		t.Fatal("worker did not reconnect")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatalf("connection attempts=%d, want 2", attempts)
	}
}
