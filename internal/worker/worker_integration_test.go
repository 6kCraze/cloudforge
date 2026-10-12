//go:build integration

package worker

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/6kCraze/cloudforge/internal/config"
	"github.com/6kCraze/cloudforge/internal/database"
	"github.com/6kCraze/cloudforge/internal/jobs"
	"github.com/6kCraze/cloudforge/internal/outbox"
	cloudrabbit "github.com/6kCraze/cloudforge/internal/rabbitmq"
	"github.com/6kCraze/cloudforge/internal/testenv"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	amqp "github.com/rabbitmq/amqp091-go"
)

const workerIntegrationLockID int64 = 240022

type workerFixture struct {
	cfg       config.Config
	pool      *pgxpool.Pool
	store     *jobs.Store
	outbox    *outbox.Store
	publisher *cloudrabbit.Publisher
	admin     *amqp.Channel
}

func newWorkerFixture(t *testing.T) *workerFixture {
	t.Helper()
	cfg, err := testenv.Load()
	if err != nil {
		t.Fatal(err)
	}
	// Integration packages share one database and RabbitMQ queue. A longer
	// setup deadline lets the advisory lock serialize packages under `go test ./...`.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool, err := database.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	lockConn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lockConn.Exec(ctx, `SELECT pg_advisory_lock($1)`, workerIntegrationLockID); err != nil {
		lockConn.Release()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = lockConn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, workerIntegrationLockID)
		lockConn.Release()
	})
	if _, err := pool.Exec(ctx, `TRUNCATE cloudforge.outbox_events, cloudforge.jobs`); err != nil {
		t.Fatal(err)
	}
	publisher, err := cloudrabbit.Open(ctx, cfg.RabbitMQURL, cfg.DispatcherConfirmTimeout)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = publisher.Close() })
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
	if _, err := admin.QueuePurge(cloudrabbit.QueueName, false); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.QueuePurge(cloudrabbit.QueueName, false)
		_, _ = pool.Exec(context.Background(), `TRUNCATE cloudforge.outbox_events, cloudforge.jobs`)
	})
	return &workerFixture{cfg: cfg, pool: pool, store: jobs.NewStore(pool), outbox: outbox.NewStore(pool), publisher: publisher, admin: admin}
}

func (f *workerFixture) createAndPublish(t *testing.T, text string) (jobs.Job, outbox.Event) {
	t.Helper()
	job, err := f.store.Create(context.Background(), jobs.CreateInput{
		Type:    jobs.TypeSyntheticSHA256,
		Payload: jobs.SHA256Payload{Text: text, Iterations: 10},
	})
	if err != nil {
		t.Fatal(err)
	}
	events, err := f.outbox.ListUnpublished(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.AggregateID == job.ID {
			if err := f.publisher.Publish(context.Background(), event); err != nil {
				t.Fatal(err)
			}
			if err := f.outbox.MarkPublished(context.Background(), event.ID); err != nil {
				t.Fatal(err)
			}
			return job, event
		}
	}
	t.Fatal("job outbox event not found")
	return jobs.Job{}, outbox.Event{}
}

func (f *workerFixture) startWorker(t *testing.T, processor Processor) (context.CancelFunc, <-chan error) {
	t.Helper()
	handler := NewHandler(f.store, "integration-worker", processor, discardLogger())
	connect := func(ctx context.Context) (DeliveryConsumer, error) {
		return cloudrabbit.OpenConsumer(ctx, f.cfg.RabbitMQURL, "integration-worker")
	}
	runner := NewRabbitWorker(handler, connect, "integration-worker", 10*time.Millisecond, 100*time.Millisecond, discardLogger())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()
	t.Cleanup(cancel)
	return cancel, done
}

func waitForJobStatus(t *testing.T, store *jobs.Store, jobID, status string) jobs.Job {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		job, err := store.Get(context.Background(), jobID)
		if err != nil {
			t.Fatal(err)
		}
		if job.Status == status {
			return job
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s status=%s, want %s", jobID, job.Status, status)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForQueueMessages(t *testing.T, channel *amqp.Channel, count int) amqp.Queue {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		queue, err := channel.QueueInspect(cloudrabbit.QueueName)
		if err != nil {
			t.Fatal(err)
		}
		if queue.Messages == count {
			return queue
		}
		if time.Now().After(deadline) {
			t.Fatalf("queue messages=%d consumers=%d, want messages=%d", queue.Messages, queue.Consumers, count)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRealRabbitMQDeliveryIsProcessedThenAcknowledged(t *testing.T) {
	fixture := newWorkerFixture(t)
	job, _ := fixture.createAndPublish(t, "real delivery")
	cancel, done := fixture.startWorker(t, jobs.Execute)

	got := waitForJobStatus(t, fixture.store, job.ID, jobs.StatusSucceeded)
	queue := waitForQueueMessages(t, fixture.admin, 0)
	if got.AttemptCount != 1 || got.ClaimedBy == nil || *got.ClaimedBy != "integration-worker" || got.CompletedAt == nil {
		t.Fatalf("completed job = %#v", got)
	}
	if queue.Consumers != 1 {
		t.Fatalf("queue consumers=%d, want 1", queue.Consumers)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestPrefetchOneKeepsSecondMessageReadyUntilFirstIsAcknowledged(t *testing.T) {
	fixture := newWorkerFixture(t)
	first, _ := fixture.createAndPublish(t, "first")
	second, _ := fixture.createAndPublish(t, "second")
	started := make(chan string, 2)
	release := make(chan struct{})
	var once sync.Once
	processor := func(_ context.Context, job jobs.Job) (json.RawMessage, error) {
		started <- job.ID
		once.Do(func() { <-release })
		return json.RawMessage(`{"ok":true}`), nil
	}
	cancel, done := fixture.startWorker(t, processor)

	<-started
	queue := waitForQueueMessages(t, fixture.admin, 1)
	if queue.Consumers != 1 {
		t.Fatalf("queue consumers=%d, want 1", queue.Consumers)
	}
	select {
	case id := <-started:
		t.Fatalf("second job %s started before first was acknowledged", id)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	waitForJobStatus(t, fixture.store, first.ID, jobs.StatusSucceeded)
	waitForJobStatus(t, fixture.store, second.ID, jobs.StatusSucceeded)
	waitForQueueMessages(t, fixture.admin, 0)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestDuplicateDeliveryExecutesJobOnlyOnce(t *testing.T) {
	fixture := newWorkerFixture(t)
	job, event := fixture.createAndPublish(t, "duplicate")
	if err := fixture.publisher.Publish(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	cancel, done := fixture.startWorker(t, jobs.Execute)

	got := waitForJobStatus(t, fixture.store, job.ID, jobs.StatusSucceeded)
	waitForQueueMessages(t, fixture.admin, 0)
	if got.AttemptCount != 1 {
		t.Fatalf("attempt_count=%d, want 1", got.AttemptCount)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestMalformedAndUnknownMessagesDoNotPoisonConsumer(t *testing.T) {
	fixture := newWorkerFixture(t)
	cancel, done := fixture.startWorker(t, jobs.Execute)
	if err := fixture.admin.PublishWithContext(context.Background(), cloudrabbit.ExchangeName, cloudrabbit.RoutingKey, false, false, amqp.Publishing{
		ContentType: "application/json", Type: outbox.EventTypeJobReady, MessageId: uuid.NewString(), Body: []byte(`{"job_id":`),
	}); err != nil {
		t.Fatal(err)
	}
	invalidUUIDBody, _ := json.Marshal(outbox.JobReadyPayload{JobID: "not-a-uuid"})
	if err := fixture.admin.PublishWithContext(context.Background(), cloudrabbit.ExchangeName, cloudrabbit.RoutingKey, false, false, amqp.Publishing{
		ContentType: "application/json", Type: outbox.EventTypeJobReady, MessageId: uuid.NewString(), Body: invalidUUIDBody,
	}); err != nil {
		t.Fatal(err)
	}
	unknownBody, _ := json.Marshal(outbox.JobReadyPayload{JobID: uuid.NewString()})
	if err := fixture.admin.PublishWithContext(context.Background(), cloudrabbit.ExchangeName, cloudrabbit.RoutingKey, false, false, amqp.Publishing{
		ContentType: "application/json", Type: outbox.EventTypeJobReady, MessageId: uuid.NewString(), Body: unknownBody,
	}); err != nil {
		t.Fatal(err)
	}
	job, _ := fixture.createAndPublish(t, "after invalid deliveries")
	waitForJobStatus(t, fixture.store, job.ID, jobs.StatusSucceeded)
	waitForQueueMessages(t, fixture.admin, 0)
	select {
	case err := <-done:
		t.Fatalf("worker stopped after invalid delivery: %v", err)
	default:
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestSucceededJobRedeliveryIsAcknowledgedWithoutReexecution(t *testing.T) {
	fixture := newWorkerFixture(t)
	job, _ := fixture.createAndPublish(t, "succeeded redelivery")
	first, err := cloudrabbit.OpenConsumer(context.Background(), fixture.cfg.RabbitMQURL, "first-worker")
	if err != nil {
		t.Fatal(err)
	}
	var original amqp.Delivery
	select {
	case original = <-first.Deliveries():
	case <-time.After(3 * time.Second):
		t.Fatal("first consumer did not receive delivery")
	}
	executions := 0
	firstHandler := NewHandler(fixture.store, "first-worker", func(context.Context, jobs.Job) (json.RawMessage, error) {
		executions++
		return json.RawMessage(`{"ok":true}`), nil
	}, discardLogger())
	if outcome, err := firstHandler.Handle(context.Background(), job.ID); err != nil || outcome != OutcomeExecuted {
		t.Fatalf("first handle outcome=%q err=%v", outcome, err)
	}
	// Simulate a crash after durable success but before ACK.
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := cloudrabbit.OpenConsumer(context.Background(), fixture.cfg.RabbitMQURL, "replacement-worker")
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	var redelivery amqp.Delivery
	select {
	case redelivery = <-second.Deliveries():
	case <-time.After(3 * time.Second):
		t.Fatal("replacement consumer did not receive redelivery")
	}
	if !redelivery.Redelivered || redelivery.MessageId != original.MessageId {
		t.Fatalf("redelivered=%v message_id=%q original=%q", redelivery.Redelivered, redelivery.MessageId, original.MessageId)
	}
	secondHandler := NewHandler(fixture.store, "replacement-worker", func(context.Context, jobs.Job) (json.RawMessage, error) {
		executions++
		return nil, errors.New("duplicate must not execute")
	}, discardLogger())
	runner := NewRabbitWorker(secondHandler, nil, "replacement-worker", time.Millisecond, time.Second, discardLogger())
	if err := runner.handleDelivery(context.Background(), redelivery); err != nil {
		t.Fatal(err)
	}
	got := waitForJobStatus(t, fixture.store, job.ID, jobs.StatusSucceeded)
	if executions != 1 || got.AttemptCount != 1 {
		t.Fatalf("executions=%d attempt_count=%d", executions, got.AttemptCount)
	}
	waitForQueueMessages(t, fixture.admin, 0)
}

func TestUnackedDeliveryIsRedeliveredAndRunningJobIsNotReexecuted(t *testing.T) {
	fixture := newWorkerFixture(t)
	job, _ := fixture.createAndPublish(t, "crash window")
	first, err := cloudrabbit.OpenConsumer(context.Background(), fixture.cfg.RabbitMQURL, "crashed-worker")
	if err != nil {
		t.Fatal(err)
	}
	var original amqp.Delivery
	select {
	case original = <-first.Deliveries():
	case <-time.After(3 * time.Second):
		t.Fatal("first consumer did not receive delivery")
	}
	if _, err := fixture.store.ClaimByID(context.Background(), job.ID, "crashed-worker"); err != nil {
		t.Fatal(err)
	}
	// Closing without ACK simulates a process crash after the PostgreSQL claim.
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := cloudrabbit.OpenConsumer(context.Background(), fixture.cfg.RabbitMQURL, "replacement-worker")
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	var redelivery amqp.Delivery
	select {
	case redelivery = <-second.Deliveries():
	case <-time.After(3 * time.Second):
		t.Fatal("replacement consumer did not receive redelivery")
	}
	if !redelivery.Redelivered || redelivery.MessageId != original.MessageId {
		t.Fatalf("redelivered=%v message_id=%q original=%q", redelivery.Redelivered, redelivery.MessageId, original.MessageId)
	}
	executed := false
	handler := NewHandler(fixture.store, "replacement-worker", func(context.Context, jobs.Job) (json.RawMessage, error) {
		executed = true
		return nil, errors.New("must not execute")
	}, discardLogger())
	runner := NewRabbitWorker(handler, nil, "replacement-worker", time.Millisecond, time.Second, discardLogger())
	if err := runner.handleDelivery(context.Background(), redelivery); err != nil {
		t.Fatal(err)
	}
	got := waitForJobStatus(t, fixture.store, job.ID, jobs.StatusRunning)
	if executed || got.AttemptCount != 1 || got.ClaimedBy == nil || *got.ClaimedBy != "crashed-worker" {
		t.Fatalf("executed=%v job=%#v", executed, got)
	}
	waitForQueueMessages(t, fixture.admin, 0)
}
