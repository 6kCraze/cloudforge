//go:build integration

package system_test

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/6kCraze/cloudforge/internal/dispatcher"
	"github.com/6kCraze/cloudforge/internal/jobs"
	"github.com/6kCraze/cloudforge/internal/outbox"
	cloudrabbit "github.com/6kCraze/cloudforge/internal/rabbitmq"
	amqp "github.com/rabbitmq/amqp091-go"
)

// Opt in only after checking that the shared broker has no unrelated clients.
// This test stops and starts the existing container without touching volumes.
func TestV5LiveBrokerInterruptionRetainsRecoveryIntent(t *testing.T) {
	if os.Getenv("CLOUDFORGE_LIVE_BROKER_STOP") != "1" {
		t.Skip("shared broker interruption requires explicit opt-in and a connection check")
	}
	connections, err := exec.Command("docker", "compose", "exec", "-T", "rabbitmq", "rabbitmqctl", "-q", "list_connections", "name").CombinedOutput()
	if err != nil {
		t.Fatalf("inspect shared broker connections: %v: %s", err, connections)
	}
	if strings.TrimSpace(string(connections)) != "" {
		t.Skipf("shared broker has active clients: %s", connections)
	}
	queueState, err := exec.Command("docker", "compose", "exec", "-T", "rabbitmq", "rabbitmqctl", "-q", "list_queues", "-p", "/", "name", "messages_ready", "messages_unacknowledged", "consumers").CombinedOutput()
	if err != nil {
		t.Fatalf("inspect development queues: %v: %s", err, queueState)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(queueState)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 4 && fields[0] == "name" && fields[1] == "messages_ready" {
			continue
		}
		if len(fields) != 4 || fields[1] != "0" || fields[2] != "0" || fields[3] != "0" {
			t.Skipf("development queue is active or unknown: %s", line)
		}
	}
	f := newV3Fixture(t)
	store := jobs.NewStoreWithPolicy(f.pool, 10*time.Second, 20*time.Millisecond, 50*time.Millisecond)
	created, initial := f.createJob(t, "broker interruption", 1)
	if err := f.outbox.MarkPublished(context.Background(), initial.ID); err != nil {
		t.Fatal(err)
	}
	old, err := store.ClaimByID(context.Background(), created.ID, "expired-worker")
	if err != nil {
		t.Fatal(err)
	}
	connected := make(chan struct{}, 4)
	d := dispatcher.New(f.outbox, func(ctx context.Context) (dispatcher.Publisher, error) {
		p, err := cloudrabbit.Open(ctx, f.cfg.RabbitMQURL, time.Second)
		if err == nil {
			select {
			case connected <- struct{}{}:
			default:
			}
		}
		return p, err
	}, 20*time.Millisecond, 20*time.Millisecond, 200*time.Millisecond, f.logger)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("dispatcher did not stop")
		}
	})
	select {
	case <-connected:
	case <-time.After(3 * time.Second):
		t.Fatal("dispatcher never connected before outage")
	}
	brokerStopped := false
	restart := func() {
		if !brokerStopped {
			return
		}
		cmdCtx, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		output, err := exec.CommandContext(cmdCtx, "docker", "compose", "start", "rabbitmq").CombinedOutput()
		if err != nil {
			t.Errorf("restore broker: %v: %s", err, output)
			return
		}
		brokerStopped = false
	}
	t.Cleanup(restart)
	commandCtx, stop := context.WithTimeout(context.Background(), 20*time.Second)
	brokerStopped = true // Cleanup attempts to restore even if stop returns an ambiguous error.
	output, err := exec.CommandContext(commandCtx, "docker", "compose", "stop", "rabbitmq").CombinedOutput()
	stop()
	if err != nil {
		t.Fatalf("stop broker: %v: %s", err, output)
	}
	if _, err := f.pool.Exec(context.Background(), `UPDATE cloudforge.jobs SET lease_expires_at = clock_timestamp() - interval '1 second' WHERE id = $1`, created.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := store.RecoverExpired(context.Background()); err != nil || n != 1 {
		t.Fatalf("recover while broker down=%d err=%v", n, err)
	}
	if _, err := f.pool.Exec(context.Background(), `UPDATE cloudforge.jobs SET next_retry_at = clock_timestamp() - interval '1 second' WHERE id = $1`, created.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := store.ReleaseDue(context.Background()); err != nil || n != 1 {
		t.Fatalf("requeue while broker down=%d err=%v", n, err)
	}
	pending, err := f.outbox.ListUnpublished(context.Background(), 10)
	if err != nil || len(pending) != 1 || pending[0].AggregateID != created.ID || pending[0].PublishedAt != nil {
		t.Fatalf("unconfirmed intent=%v err=%v", pending, err)
	}
	if job := f.getJob(t, created.ID); job.Status != jobs.StatusQueued || job.AttemptCount != 1 {
		t.Fatalf("queued recovery lost while broker down: %+v", job)
	}
	// Give the live dispatcher a bounded chance to observe the closed broker.
	time.Sleep(250 * time.Millisecond)
	pending, err = f.outbox.ListUnpublished(context.Background(), 10)
	if err != nil || len(pending) != 1 || pending[0].PublishedAt != nil {
		t.Fatalf("outage falsely marked published: %v %v", pending, err)
	}
	restart()
	if brokerStopped {
		t.Fatal("broker did not restart")
	}
	// The existing volume and test-vhost queue must reappear after startup.
	deadline := time.Now().Add(12 * time.Second)
	for {
		newConn, dialErr := amqp.Dial(f.cfg.RabbitMQURL)
		if dialErr == nil {
			newAdmin, channelErr := newConn.Channel()
			if channelErr == nil {
				f.admin = newAdmin
				t.Cleanup(func() { _ = newAdmin.Close(); _ = newConn.Close() })
				break
			}
			_ = newConn.Close()
		}
		if time.Now().After(deadline) {
			t.Fatalf("test vhost did not return: %v", dialErr)
		}
		time.Sleep(100 * time.Millisecond)
	}
	select {
	case <-connected:
	case <-time.After(8 * time.Second):
		t.Fatal("dispatcher did not reconnect")
	}
	f.waitForReady(t, 1)
	f.startWorker(t, "broker-replacement-worker", store, jobs.Execute, func() {})
	f.waitForConsumers(t, 1)
	completed := f.waitForStatus(t, created.ID, jobs.StatusSucceeded)
	if completed.AttemptCount != 2 || completed.AttemptID == nil || *completed.AttemptID == *old.AttemptID {
		t.Fatalf("replacement outcome=%+v", completed)
	}
	f.waitForACKs(t, 1)
	pending, err = f.outbox.ListUnpublished(context.Background(), 10)
	if err != nil || len(pending) != 0 {
		t.Fatalf("confirmed event remained unpublished: %v %v", pending, err)
	}
	var readyEvents, marked int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*), count(*) FILTER (WHERE published_at IS NOT NULL) FROM cloudforge.outbox_events WHERE aggregate_id = $1 AND event_type = $2`, created.ID, outbox.EventTypeJobReady).Scan(&readyEvents, &marked); err != nil || readyEvents != 2 || marked != 2 {
		t.Fatalf("ready event history=%d/%d err=%v", readyEvents, marked, err)
	}
}
