//go:build integration

package system_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/6kCraze/cloudforge/internal/database"
	"github.com/6kCraze/cloudforge/internal/dispatcher"
	"github.com/6kCraze/cloudforge/internal/jobs"
	"github.com/6kCraze/cloudforge/internal/outbox"
	cloudrabbit "github.com/6kCraze/cloudforge/internal/rabbitmq"
	"github.com/6kCraze/cloudforge/internal/testenv"
	"github.com/6kCraze/cloudforge/internal/worker"
	amqp "github.com/rabbitmq/amqp091-go"
)

const integrationLockID int64 = 240022

// TestV2EndToEndAcrossDispatcherAndWorkerDowntime exercises the boundaries
// between the focused component tests: PostgreSQL retains publication intent
// without a dispatcher, RabbitMQ retains ready work without a worker, and the
// real consumer finishes the job exactly once when it starts.
func TestV2EndToEndAcrossDispatcherAndWorkerDowntime(t *testing.T) {
	cfg, err := testenv.Load()
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancelSetup := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancelSetup()
	pool, err := database.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	lock, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lock.Exec(ctx, `SELECT pg_advisory_lock($1)`, integrationLockID); err != nil {
		lock.Release()
		t.Fatal(err)
	}
	defer func() {
		_, _ = lock.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, integrationLockID)
		lock.Release()
	}()
	if _, err := pool.Exec(ctx, `TRUNCATE cloudforge.job_attempts, cloudforge.batch_images, cloudforge.batches, cloudforge.outbox_events, cloudforge.jobs`); err != nil {
		t.Fatal(err)
	}

	adminConnection, err := amqp.Dial(cfg.RabbitMQURL)
	if err != nil {
		t.Fatal(err)
	}
	defer adminConnection.Close()
	admin, err := adminConnection.Channel()
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	publisher, err := cloudrabbit.Open(ctx, cfg.RabbitMQURL, cfg.DispatcherConfirmTimeout)
	if err != nil {
		t.Fatal(err)
	}
	defer publisher.Close()
	if _, err := admin.QueuePurge(cloudrabbit.QueueName, false); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = admin.QueuePurge(cloudrabbit.QueueName, false)
		_, _ = pool.Exec(context.Background(), `TRUNCATE cloudforge.job_attempts, cloudforge.batch_images, cloudforge.batches, cloudforge.outbox_events, cloudforge.jobs`)
	}()

	jobStore := jobs.NewStore(pool)
	outboxStore := outbox.NewStore(pool)
	created, err := jobStore.Create(context.Background(), jobs.CreateInput{
		Type:    jobs.TypeSyntheticSHA256,
		Payload: jobs.SHA256Payload{Text: "V2 end-to-end validation", Iterations: 100},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Dispatcher absent: job and publication intent are durable, broker empty.
	events, err := outboxStore.ListUnpublished(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].AggregateID != created.ID {
		t.Fatalf("unpublished events=%#v, want one for job %s", events, created.ID)
	}
	assertQueue(t, admin, 0, 0)
	assertJob(t, jobStore, created.ID, jobs.StatusQueued, 0)

	// Dispatcher returns while worker remains absent: message is Ready.
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	d := dispatcher.New(outboxStore, nil, time.Millisecond, time.Millisecond, time.Second, logger)
	published, err := d.DispatchBatch(context.Background(), publisher)
	if err != nil || published != 1 {
		t.Fatalf("published=%d err=%v", published, err)
	}
	assertQueue(t, admin, 1, 0)
	assertJob(t, jobStore, created.ID, jobs.StatusQueued, 0)
	if remaining, err := outboxStore.ListUnpublished(context.Background(), 10); err != nil || len(remaining) != 0 {
		t.Fatalf("remaining unpublished=%d err=%v", len(remaining), err)
	}

	// Worker returns: the real manual-ACK consumer drains and completes once.
	handler := worker.NewHandler(jobStore, "v2-system-worker", jobs.Execute, logger)
	runner := worker.NewRabbitWorker(handler, func(ctx context.Context) (worker.DeliveryConsumer, error) {
		return cloudrabbit.OpenConsumer(ctx, cfg.RabbitMQURL, "v2-system-worker")
	}, "v2-system-worker", 10*time.Millisecond, 100*time.Millisecond, logger)
	workerCtx, stopWorker := context.WithCancel(context.Background())
	workerDone := make(chan error, 1)
	go func() { workerDone <- runner.Run(workerCtx) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		got, getErr := jobStore.Get(context.Background(), created.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if got.Status == jobs.StatusSucceeded {
			if got.AttemptCount != 1 {
				t.Fatalf("attempt_count=%d, want 1", got.AttemptCount)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job status=%s, want succeeded", got.Status)
		}
		time.Sleep(10 * time.Millisecond)
	}
	assertQueue(t, admin, 0, 1)
	stopWorker()
	if err := <-workerDone; err != nil {
		t.Fatal(err)
	}
}

func assertJob(t *testing.T, store *jobs.Store, id, status string, attempts int16) {
	t.Helper()
	job, err := store.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if job.Status != status || job.AttemptCount != attempts {
		t.Fatalf("job status=%s attempts=%d, want %s/%d", job.Status, job.AttemptCount, status, attempts)
	}
}

func assertQueue(t *testing.T, channel *amqp.Channel, messages, consumers int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		queue, err := channel.QueueInspect(cloudrabbit.QueueName)
		if err != nil {
			t.Fatal(err)
		}
		if queue.Messages == messages && queue.Consumers == consumers {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("queue messages=%d consumers=%d, want %d/%d", queue.Messages, queue.Consumers, messages, consumers)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
