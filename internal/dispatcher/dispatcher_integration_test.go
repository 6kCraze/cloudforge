//go:build integration

package dispatcher

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/6kCraze/cloudforge/internal/config"
	"github.com/6kCraze/cloudforge/internal/database"
	"github.com/6kCraze/cloudforge/internal/jobs"
	"github.com/6kCraze/cloudforge/internal/outbox"
	cloudrabbit "github.com/6kCraze/cloudforge/internal/rabbitmq"
	"github.com/6kCraze/cloudforge/internal/testenv"
	"github.com/jackc/pgx/v5/pgxpool"
	amqp "github.com/rabbitmq/amqp091-go"
)

const dispatcherIntegrationLockID int64 = 240022

type integrationFixture struct {
	cfg       config.Config
	pool      *pgxpool.Pool
	store     *outbox.Store
	publisher *cloudrabbit.Publisher
	admin     *amqp.Channel
}

func newIntegrationFixture(t *testing.T) *integrationFixture {
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
	if _, err := lockConn.Exec(ctx, `SELECT pg_advisory_lock($1)`, dispatcherIntegrationLockID); err != nil {
		lockConn.Release()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = lockConn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, dispatcherIntegrationLockID)
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

	return &integrationFixture{
		cfg:       cfg,
		pool:      pool,
		store:     outbox.NewStore(pool),
		publisher: publisher,
		admin:     admin,
	}
}

func TestRealPublicationIsConfirmedRoutedPersistentAndRecorded(t *testing.T) {
	fixture := newIntegrationFixture(t)
	job := createIntegrationJob(t, fixture.pool, "confirmed publication")
	d := testDispatcher(fixture.store, nil)

	publishedCount, err := d.DispatchBatch(context.Background(), fixture.publisher)
	if err != nil {
		t.Fatal(err)
	}
	if publishedCount != 1 {
		t.Fatalf("published=%d, want 1", publishedCount)
	}

	delivery, ok, err := fixture.admin.Get(cloudrabbit.QueueName, true)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("RabbitMQ queue did not receive the publication")
	}
	if delivery.MessageId == "" || delivery.Type != outbox.EventTypeJobReady || delivery.RoutingKey != cloudrabbit.RoutingKey {
		t.Fatalf("metadata: message_id=%q type=%q routing_key=%q", delivery.MessageId, delivery.Type, delivery.RoutingKey)
	}
	if delivery.ContentType != "application/json" || delivery.DeliveryMode != amqp.Persistent {
		t.Fatalf("content_type=%q delivery_mode=%d", delivery.ContentType, delivery.DeliveryMode)
	}
	if version, ok := delivery.Headers["schema_version"].(int32); !ok || version != 1 {
		t.Fatalf("schema_version=%#v", delivery.Headers["schema_version"])
	}
	var payload outbox.JobReadyPayload
	if err := json.Unmarshal(delivery.Body, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.JobID != job.ID {
		t.Fatalf("job_id=%q, want %q", payload.JobID, job.ID)
	}
	var eventID string
	var published bool
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT id::text, published_at IS NOT NULL
		FROM cloudforge.outbox_events WHERE aggregate_id = $1
	`, job.ID).Scan(&eventID, &published); err != nil {
		t.Fatal(err)
	}
	if delivery.MessageId != eventID || !published {
		t.Fatalf("message_id=%q event_id=%q published=%v", delivery.MessageId, eventID, published)
	}
	publishedCount, err = d.DispatchBatch(context.Background(), fixture.publisher)
	if err != nil || publishedCount != 0 {
		t.Fatalf("already-published event count=%d err=%v", publishedCount, err)
	}
	queue, err := fixture.admin.QueueInspect(cloudrabbit.QueueName)
	if err != nil {
		t.Fatal(err)
	}
	if queue.Consumers != 0 || queue.Messages != 0 {
		t.Fatalf("queue consumers=%d messages=%d, want 0/0 after inspection consume", queue.Consumers, queue.Messages)
	}
}

func TestRealUnroutablePublicationLeavesEventUnpublished(t *testing.T) {
	fixture := newIntegrationFixture(t)
	job := createIntegrationJob(t, fixture.pool, "unroutable publication")
	events, err := fixture.store.ListUnpublished(context.Background(), batchSize)
	if err != nil {
		t.Fatal(err)
	}
	event := events[0]
	event.EventType = "job.unbound"
	err = fixture.publisher.Publish(context.Background(), event)
	var unroutable *cloudrabbit.UnroutableError
	if !errors.As(err, &unroutable) {
		t.Fatalf("Publish() error=%v, want UnroutableError", err)
	}
	var published bool
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT published_at IS NOT NULL FROM cloudforge.outbox_events WHERE aggregate_id = $1
	`, job.ID).Scan(&published); err != nil {
		t.Fatal(err)
	}
	if published {
		t.Fatal("unroutable event was marked published")
	}
}

func TestDispatcherReconnectsAndPublishesPendingEvent(t *testing.T) {
	fixture := newIntegrationFixture(t)
	_ = createIntegrationJob(t, fixture.pool, "reconnect publication")
	_ = fixture.publisher.Close()

	attempts := 0
	connect := func(ctx context.Context) (Publisher, error) {
		attempts++
		if attempts == 1 {
			return cloudrabbit.Open(ctx, "amqp://guest:guest@127.0.0.1:1/", 100*time.Millisecond)
		}
		return cloudrabbit.Open(ctx, fixture.cfg.RabbitMQURL, fixture.cfg.DispatcherConfirmTimeout)
	}
	d := New(fixture.store, connect, 10*time.Millisecond, 5*time.Millisecond, 20*time.Millisecond, discardLogger())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()

	deadline := time.Now().Add(4 * time.Second)
	for {
		var published bool
		if err := fixture.pool.QueryRow(context.Background(), `
			SELECT published_at IS NOT NULL FROM cloudforge.outbox_events LIMIT 1
		`).Scan(&published); err != nil {
			t.Fatal(err)
		}
		if published {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("pending event was not published after reconnect")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if attempts < 2 {
		t.Fatalf("connection attempts=%d, want at least 2", attempts)
	}
}

func TestConfirmedButUnrecordedEventCanBePublishedTwice(t *testing.T) {
	fixture := newIntegrationFixture(t)
	_ = createIntegrationJob(t, fixture.pool, "duplicate window")
	events, err := fixture.store.ListUnpublished(context.Background(), batchSize)
	if err != nil {
		t.Fatal(err)
	}
	event := events[0]
	if err := fixture.publisher.Publish(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash before MarkPublished by publishing the still-NULL event again.
	if err := fixture.publisher.Publish(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	first, firstOK, err := fixture.admin.Get(cloudrabbit.QueueName, true)
	if err != nil {
		t.Fatal(err)
	}
	second, secondOK, err := fixture.admin.Get(cloudrabbit.QueueName, true)
	if err != nil {
		t.Fatal(err)
	}
	if !firstOK || !secondOK || first.MessageId != event.ID || second.MessageId != event.ID {
		t.Fatalf("duplicate deliveries: first=%v/%q second=%v/%q", firstOK, first.MessageId, secondOK, second.MessageId)
	}
}

func TestMalformedOldestEventDoesNotBlockLaterValidEvent(t *testing.T) {
	fixture := newIntegrationFixture(t)
	malformedJob := createIntegrationJob(t, fixture.pool, "malformed oldest")
	validJob := createIntegrationJob(t, fixture.pool, "valid later")

	var malformedEventID, validEventID string
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT id::text FROM cloudforge.outbox_events WHERE aggregate_id = $1
	`, malformedJob.ID).Scan(&malformedEventID); err != nil {
		t.Fatal(err)
	}
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT id::text FROM cloudforge.outbox_events WHERE aggregate_id = $1
	`, validJob.ID).Scan(&validEventID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(context.Background(), `
		UPDATE cloudforge.outbox_events
		SET payload = jsonb_build_object('job_id', $2::text),
			created_at = now() - interval '2 seconds'
		WHERE id = $1
	`, malformedEventID, validEventID); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.pool.Exec(context.Background(), `
		UPDATE cloudforge.outbox_events
		SET created_at = now() - interval '1 second'
		WHERE id = $1
	`, validEventID); err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	d := New(fixture.store, func(context.Context) (Publisher, error) {
		return fixture.publisher, nil
	}, 10*time.Millisecond, 5*time.Millisecond, 20*time.Millisecond, logger)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()

	deadline := time.Now().Add(4 * time.Second)
	for {
		var validPublished bool
		if err := fixture.pool.QueryRow(context.Background(), `
			SELECT published_at IS NOT NULL FROM cloudforge.outbox_events WHERE id = $1
		`, validEventID).Scan(&validPublished); err != nil {
			t.Fatal(err)
		}
		if validPublished {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("valid event behind malformed event was not published")
		}
		time.Sleep(10 * time.Millisecond)
	}

	select {
	case err := <-done:
		t.Fatalf("dispatcher stopped before cancellation: %v", err)
	default:
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	var malformedPublished, validPublished bool
	if err := fixture.pool.QueryRow(context.Background(), `
		SELECT
			(SELECT published_at IS NOT NULL FROM cloudforge.outbox_events WHERE id = $1),
			(SELECT published_at IS NOT NULL FROM cloudforge.outbox_events WHERE id = $2)
	`, malformedEventID, validEventID).Scan(&malformedPublished, &validPublished); err != nil {
		t.Fatal(err)
	}
	if malformedPublished || !validPublished {
		t.Fatalf("malformed published=%v valid published=%v", malformedPublished, validPublished)
	}

	delivery, ok, err := fixture.admin.Get(cloudrabbit.QueueName, true)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || delivery.MessageId != validEventID {
		t.Fatalf("delivery ok=%v message_id=%q, want %q", ok, delivery.MessageId, validEventID)
	}
	if _, more, err := fixture.admin.Get(cloudrabbit.QueueName, true); err != nil || more {
		t.Fatalf("unexpected second delivery: more=%v err=%v", more, err)
	}
	if output := logs.String(); !strings.Contains(output, malformedEventID) || !strings.Contains(output, "malformed outbox event") {
		t.Fatalf("malformed event log missing ID or validation message: %s", output)
	}
}

func createIntegrationJob(t *testing.T, pool *pgxpool.Pool, text string) jobs.Job {
	t.Helper()
	job, err := jobs.NewStore(pool).Create(context.Background(), jobs.CreateInput{
		Type: jobs.TypeSyntheticSHA256,
		Payload: jobs.SHA256Payload{
			Text:       text,
			Iterations: 10,
		},
	})
	if err != nil {
		t.Fatal(fmt.Errorf("create integration job: %w", err))
	}
	return job
}
