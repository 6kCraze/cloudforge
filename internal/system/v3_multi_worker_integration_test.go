//go:build integration

package system_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/6kCraze/cloudforge/internal/config"
	"github.com/6kCraze/cloudforge/internal/database"
	"github.com/6kCraze/cloudforge/internal/jobs"
	"github.com/6kCraze/cloudforge/internal/outbox"
	cloudrabbit "github.com/6kCraze/cloudforge/internal/rabbitmq"
	"github.com/6kCraze/cloudforge/internal/testenv"
	"github.com/6kCraze/cloudforge/internal/worker"
	"github.com/jackc/pgx/v5/pgxpool"
	amqp "github.com/rabbitmq/amqp091-go"
)

type v3Fixture struct {
	cfg       config.Config
	pool      *pgxpool.Pool
	jobs      *jobs.Store
	outbox    *outbox.Store
	publisher *cloudrabbit.Publisher
	admin     *amqp.Channel
	logger    *slog.Logger
	acks      chan struct{}
}

type runningWorker struct {
	cancel context.CancelFunc
	done   chan struct{}
	err    error
}

type workStart struct {
	workerID string
	jobID    string
}

// TestV3TwoWorkersShareQueueSafely proves multi-worker behavior with two
// consumers and deterministic blocking processors. It uses no CPU-heavy SHA
// work; synchronization, PostgreSQL state, and RabbitMQ consumer state provide
// the evidence.
func TestV3TwoWorkersShareQueueSafely(t *testing.T) {
	fixture := newV3Fixture(t)

	t.Run("different workers execute different jobs concurrently", func(t *testing.T) {
		started := make(chan workStart, 2)
		release, unblock := newGate()
		processor := func(workerID string) worker.Processor {
			return func(ctx context.Context, job jobs.Job) (json.RawMessage, error) {
				select {
				case started <- workStart{workerID: workerID, jobID: job.ID}:
				case <-time.After(5 * time.Second):
					return nil, errors.New("test start observation timed out")
				}
				if err := waitGate(ctx, release); err != nil {
					return nil, err
				}
				return json.RawMessage(fmt.Sprintf(`{"worker_id":%q}`, workerID)), nil
			}
		}

		worker01 := fixture.startWorker(t, "worker-01", fixture.jobs, processor("worker-01"), unblock)
		worker02 := fixture.startWorker(t, "worker-02", fixture.jobs, processor("worker-02"), unblock)
		fixture.waitForConsumers(t, 2)

		first := fixture.createAndPublish(t, "v3 concurrent first", 1)
		second := fixture.createAndPublish(t, "v3 concurrent second", 1)
		firstStart := receiveStart(t, started)
		secondStart := receiveStart(t, started)
		if firstStart.workerID == secondStart.workerID {
			t.Fatalf("both jobs started on %s, want two workers", firstStart.workerID)
		}
		if firstStart.jobID == secondStart.jobID {
			t.Fatalf("job %s started twice", firstStart.jobID)
		}

		for _, observed := range []workStart{firstStart, secondStart} {
			got := fixture.getJob(t, observed.jobID)
			if got.Status != jobs.StatusRunning || got.ClaimedBy == nil || *got.ClaimedBy != observed.workerID || got.AttemptCount != 1 {
				t.Fatalf("running job = %#v, want worker=%s attempt=1", got, observed.workerID)
			}
		}

		unblock()
		completedFirst := fixture.waitForStatus(t, first.ID, jobs.StatusSucceeded)
		completedSecond := fixture.waitForStatus(t, second.ID, jobs.StatusSucceeded)
		for _, completed := range []jobs.Job{completedFirst, completedSecond} {
			if completed.AttemptCount != 1 || len(completed.Result) == 0 {
				t.Fatalf("durable result/attempt missing: %#v", completed)
			}
		}
		assertOverlap(t, completedFirst, completedSecond)
		fixture.waitForACKs(t, 2)
		stopWorker(t, worker02)
		stopWorker(t, worker01)
		fixture.assertSettled(t)
	})

	t.Run("duplicate deliveries compete but execute once", func(t *testing.T) {
		created, event := fixture.createJob(t, "v3 duplicate competition", 1)
		seen := make(chan string, 2)
		claimGate, allowClaims := newGate()
		claims := make(chan claimOutcome, 2)
		started := make(chan workStart, 1)
		release, unblock := newGate()
		cleanup := func() { allowClaims(); unblock() }
		var executions atomic.Int32

		workerStore01 := &observingStore{Store: fixture.jobs, workerID: "worker-01", watchJobID: created.ID, seen: seen, claimGate: claimGate, claims: claims}
		workerStore02 := &observingStore{Store: fixture.jobs, workerID: "worker-02", watchJobID: created.ID, seen: seen, claimGate: claimGate, claims: claims}
		processor := func(workerID string) worker.Processor {
			return func(ctx context.Context, job jobs.Job) (json.RawMessage, error) {
				executions.Add(1)
				select {
				case started <- workStart{workerID: workerID, jobID: job.ID}:
				case <-time.After(5 * time.Second):
					return nil, errors.New("test start observation timed out")
				}
				if err := waitGate(ctx, release); err != nil {
					return nil, err
				}
				return json.RawMessage(`{"ok":true}`), nil
			}
		}

		worker01 := fixture.startWorker(t, "worker-01", workerStore01, processor("worker-01"), cleanup)
		worker02 := fixture.startWorker(t, "worker-02", workerStore02, processor("worker-02"), cleanup)
		fixture.waitForConsumers(t, 2)
		fixture.publish(t, event)
		fixture.publish(t, event)
		if err := fixture.outbox.MarkPublished(context.Background(), event.ID); err != nil {
			t.Fatal(err)
		}

		seenBy := receiveDistinctWorkers(t, seen, 2)
		allowClaims()
		successes, conflicts := 0, 0
		for i := 0; i < 2; i++ {
			select {
			case claim := <-claims:
				switch {
				case claim.err == nil:
					successes++
				case errors.Is(claim.err, jobs.ErrNotClaimable):
					conflicts++
				default:
					t.Fatalf("claim from %s: %v", claim.workerID, claim.err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("timed out waiting for both real claims")
			}
		}
		if successes != 1 || conflicts != 1 {
			t.Fatalf("claims: successes=%d conflicts=%d, want 1/1", successes, conflicts)
		}
		t.Log("actual conditional claims: one success, one ErrNotClaimable")
		executor := receiveStart(t, started)
		if _, ok := seenBy["worker-01"]; !ok {
			t.Fatalf("worker-01 never inspected duplicate job; seen=%v", seenBy)
		}
		if _, ok := seenBy["worker-02"]; !ok {
			t.Fatalf("worker-02 never inspected duplicate job; seen=%v", seenBy)
		}
		if executions.Load() != 1 {
			t.Fatalf("executions=%d, want 1 while winner is blocked", executions.Load())
		}
		running := fixture.getJob(t, created.ID)
		if running.Status != jobs.StatusRunning || running.AttemptCount != 1 || running.ClaimedBy == nil || *running.ClaimedBy != executor.workerID {
			t.Fatalf("running duplicate job = %#v, executor=%s", running, executor.workerID)
		}

		unblock()
		completed := fixture.waitForStatus(t, created.ID, jobs.StatusSucceeded)
		fixture.waitForACKs(t, 2)
		if executions.Load() != 1 || completed.AttemptCount != 1 || len(completed.Result) == 0 {
			t.Fatalf("executions=%d attempt_count=%d, want 1/1", executions.Load(), completed.AttemptCount)
		}
		stopWorker(t, worker02)
		stopWorker(t, worker01)
		fixture.assertSettled(t)
	})

	t.Run("stopping one worker leaves the other operational", func(t *testing.T) {
		processor := func(_ context.Context, _ jobs.Job) (json.RawMessage, error) {
			return json.RawMessage(`{"ok":true}`), nil
		}
		worker01 := fixture.startWorker(t, "worker-01", fixture.jobs, processor, func() {})
		worker02 := fixture.startWorker(t, "worker-02", fixture.jobs, processor, func() {})
		fixture.waitForConsumers(t, 2)

		stopWorker(t, worker02)
		fixture.waitForConsumers(t, 1)
		created := fixture.createAndPublish(t, "v3 remaining worker", 1)
		completed := fixture.waitForStatus(t, created.ID, jobs.StatusSucceeded)
		if completed.ClaimedBy == nil || *completed.ClaimedBy != "worker-01" || completed.AttemptCount != 1 || len(completed.Result) == 0 {
			t.Fatalf("completed job = %#v, want worker-01 attempt=1", completed)
		}
		fixture.waitForACKs(t, 1)
		stopWorker(t, worker01)
		fixture.assertSettled(t)
	})
}

func newV3Fixture(t *testing.T) *v3Fixture {
	t.Helper()
	cfg, err := testenv.Load()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool, err := database.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	lock, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lock.Exec(ctx, `SELECT pg_advisory_lock($1)`, integrationLockID); err != nil {
		lock.Release()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = lock.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, integrationLockID)
		lock.Release()
	})
	if _, err := pool.Exec(ctx, `TRUNCATE cloudforge.batch_images, cloudforge.batches, cloudforge.outbox_events, cloudforge.jobs`); err != nil {
		t.Fatal(err)
	}

	adminConnection, err := amqp.Dial(cfg.RabbitMQURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = adminConnection.Close() })
	admin, err := adminConnection.Channel()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	publisher, err := cloudrabbit.Open(ctx, cfg.RabbitMQURL, cfg.DispatcherConfirmTimeout)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = publisher.Close() })
	if _, err := admin.QueuePurge(cloudrabbit.QueueName, false); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.QueuePurge(cloudrabbit.QueueName, false)
		_, _ = pool.Exec(context.Background(), `TRUNCATE cloudforge.batch_images, cloudforge.batches, cloudforge.outbox_events, cloudforge.jobs`)
	})

	return &v3Fixture{
		cfg:       cfg,
		pool:      pool,
		jobs:      jobs.NewStore(pool),
		outbox:    outbox.NewStore(pool),
		publisher: publisher,
		admin:     admin,
		acks:      make(chan struct{}, 8),
		logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func (f *v3Fixture) startWorker(t *testing.T, workerID string, store worker.JobStore, processor worker.Processor, unblock func()) *runningWorker {
	t.Helper()
	handler := worker.NewHandler(store, workerID, processor, f.logger)
	runner := worker.NewRabbitWorker(handler, func(ctx context.Context) (worker.DeliveryConsumer, error) {
		consumer, err := cloudrabbit.OpenConsumer(ctx, f.cfg.RabbitMQURL, workerID)
		if err != nil {
			return nil, err
		}
		return observeACKs(consumer, f.acks), nil
	}, workerID, 10*time.Millisecond, 100*time.Millisecond, f.logger)
	ctx, cancel := context.WithCancel(context.Background())
	running := &runningWorker{cancel: cancel, done: make(chan struct{})}
	// Production finishes accepted work without cancellation: unblock first.
	t.Cleanup(func() { unblock(); stopWorker(t, running) })
	go func() { running.err = runner.Run(ctx); close(running.done) }()
	return running
}

func (f *v3Fixture) createJob(t *testing.T, text string, iterations int) (jobs.Job, outbox.Event) {
	t.Helper()
	created, err := f.jobs.Create(context.Background(), jobs.CreateInput{
		Type: jobs.TypeSyntheticSHA256, Payload: jobs.SHA256Payload{Text: text, Iterations: iterations},
	})
	if err != nil {
		t.Fatal(err)
	}
	events, err := f.outbox.ListUnpublished(context.Background(), 32)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.AggregateID == created.ID {
			return created, event
		}
	}
	t.Fatalf("outbox event for job %s not found", created.ID)
	return jobs.Job{}, outbox.Event{}
}

func (f *v3Fixture) createAndPublish(t *testing.T, text string, iterations int) jobs.Job {
	t.Helper()
	created, event := f.createJob(t, text, iterations)
	f.publish(t, event)
	if err := f.outbox.MarkPublished(context.Background(), event.ID); err != nil {
		t.Fatal(err)
	}
	return created
}

func (f *v3Fixture) publish(t *testing.T, event outbox.Event) {
	t.Helper()
	if err := f.publisher.Publish(context.Background(), event); err != nil {
		t.Fatal(err)
	}
}

func (f *v3Fixture) waitForConsumers(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		queue, err := f.admin.QueueInspect(cloudrabbit.QueueName)
		if err != nil {
			t.Fatal(err)
		}
		if queue.Consumers == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("consumers=%d, want %d", queue.Consumers, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (f *v3Fixture) waitForReady(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		queue, err := f.admin.QueueInspect(cloudrabbit.QueueName)
		if err != nil {
			t.Fatal(err)
		}
		if queue.Messages == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("ready=%d consumers=%d, want ready=%d", queue.Messages, queue.Consumers, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (f *v3Fixture) getJob(t *testing.T, id string) jobs.Job {
	t.Helper()
	job, err := f.jobs.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func (f *v3Fixture) waitForStatus(t *testing.T, id, status string) jobs.Job {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		job := f.getJob(t, id)
		if job.Status == status {
			return job
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s status=%s, want %s", id, job.Status, status)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func receiveStart(t *testing.T, started <-chan workStart) workStart {
	t.Helper()
	select {
	case observed := <-started:
		return observed
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for worker to start job")
		return workStart{}
	}
}

func receiveDistinctWorkers(t *testing.T, seen <-chan string, want int) map[string]struct{} {
	t.Helper()
	workers := make(map[string]struct{}, want)
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for len(workers) < want {
		select {
		case workerID := <-seen:
			workers[workerID] = struct{}{}
		case <-deadline.C:
			t.Fatalf("workers that inspected delivery=%v, want %d distinct workers", workers, want)
		}
	}
	return workers
}

func stopWorker(t *testing.T, running *runningWorker) {
	t.Helper()
	running.cancel()
	select {
	case <-running.done:
		if running.err != nil {
			t.Error(running.err)
		}
	case <-time.After(5 * time.Second):
		t.Error("worker did not stop within 5 seconds")
	}
}

func assertOverlap(t *testing.T, first, second jobs.Job) {
	t.Helper()
	if first.StartedAt == nil || first.CompletedAt == nil || second.StartedAt == nil || second.CompletedAt == nil {
		t.Fatalf("jobs lack timing evidence: first=%#v second=%#v", first, second)
	}
	latestStart := *first.StartedAt
	if second.StartedAt.After(latestStart) {
		latestStart = *second.StartedAt
	}
	earliestCompletion := *first.CompletedAt
	if second.CompletedAt.Before(earliestCompletion) {
		earliestCompletion = *second.CompletedAt
	}
	if !latestStart.Before(earliestCompletion) {
		t.Fatalf("jobs did not overlap: first=%s..%s second=%s..%s", first.StartedAt, first.CompletedAt, second.StartedAt, second.CompletedAt)
	}
}

// Synchronize the existing production Get -> ClaimByID path, not a new read.
type claimOutcome struct {
	workerID string
	err      error
}
type observingStore struct {
	*jobs.Store
	workerID   string
	watchJobID string
	seen       chan<- string
	claimGate  <-chan struct{}
	claims     chan<- claimOutcome
}

func (s *observingStore) Get(ctx context.Context, id string) (jobs.Job, error) {
	job, err := s.Store.Get(ctx, id)
	if err == nil && id == s.watchJobID && job.Status == jobs.StatusQueued {
		select {
		case s.seen <- s.workerID:
		case <-time.After(5 * time.Second):
			return jobs.Job{}, errors.New("test read observation timed out")
		}
		if gateErr := waitGate(ctx, s.claimGate); gateErr != nil {
			return jobs.Job{}, gateErr
		}
	}
	return job, err
}
func (s *observingStore) ClaimByID(ctx context.Context, id, workerID string) (jobs.Job, error) {
	job, err := s.Store.ClaimByID(ctx, id, workerID)
	select {
	case s.claims <- claimOutcome{workerID: workerID, err: err}:
	case <-time.After(5 * time.Second):
		return jobs.Job{}, errors.New("test claim observation timed out")
	}
	return job, err
}
func newGate() (chan struct{}, func()) {
	gate := make(chan struct{})
	var once sync.Once
	return gate, func() { once.Do(func() { close(gate) }) }
}
func waitGate(ctx context.Context, gate <-chan struct{}) error {
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	select {
	case <-gate:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return errors.New("test gate timed out")
	}
}

// ACK itself has no broker confirm. Closing actual channels and checking for
// requeued work after observing successful ACK calls completes the proof.
type ackObserver struct {
	amqp.Acknowledger
	acks chan<- struct{}
}

func (a ackObserver) Ack(tag uint64, multiple bool) error {
	if err := a.Acknowledger.Ack(tag, multiple); err != nil {
		return err
	}
	select {
	case a.acks <- struct{}{}:
	case <-time.After(5 * time.Second):
		return errors.New("test ACK observation timed out")
	}
	return nil
}

type observedConsumer struct {
	*cloudrabbit.Consumer
	deliveries chan amqp.Delivery
	stopped    chan struct{}
	once       sync.Once
}

func observeACKs(consumer *cloudrabbit.Consumer, acks chan<- struct{}) *observedConsumer {
	c := &observedConsumer{Consumer: consumer, deliveries: make(chan amqp.Delivery, 1), stopped: make(chan struct{})}
	go func() {
		defer close(c.deliveries)
		for {
			select {
			case <-c.stopped:
				return
			case delivery, ok := <-consumer.Deliveries():
				if !ok {
					return
				}
				delivery.Acknowledger = ackObserver{Acknowledger: delivery.Acknowledger, acks: acks}
				select {
				case c.deliveries <- delivery:
				case <-c.stopped:
					return
				}
			}
		}
	}()
	return c
}
func (c *observedConsumer) Deliveries() <-chan amqp.Delivery { return c.deliveries }
func (c *observedConsumer) Close() error {
	c.once.Do(func() { close(c.stopped) })
	return c.Consumer.Close()
}
func (f *v3Fixture) waitForACKs(t *testing.T, want int) {
	t.Helper()
	for i := 0; i < want; i++ {
		select {
		case <-f.acks:
		case <-time.After(5 * time.Second):
			t.Fatalf("ACK calls completed=%d,want %d", i, want)
		}
	}
	t.Logf("observed %d successful real ACK calls", want)
}
func (f *v3Fixture) assertSettled(t *testing.T) {
	t.Helper()
	f.waitForConsumers(t, 0)
	f.waitForReady(t, 0)
	t.Log("all consumer channels closed; consumers=0, ready=0, no Unacked work can remain")
}
