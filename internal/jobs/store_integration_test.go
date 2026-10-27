//go:build integration

package jobs

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/6kCraze/cloudforge/internal/database"
	"github.com/6kCraze/cloudforge/internal/outbox"
	"github.com/6kCraze/cloudforge/internal/testenv"
	"github.com/jackc/pgx/v5/pgxpool"
)

const integrationAdvisoryLockID int64 = 240022

func integrationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	cfg, err := testenv.Load()
	if err != nil {
		t.Fatal(err)
	}
	pool, err := database.Open(context.Background(), cfg.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	lockConn, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lockConn.Exec(context.Background(), `SELECT pg_advisory_lock($1)`, integrationAdvisoryLockID); err != nil {
		lockConn.Release()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = lockConn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, integrationAdvisoryLockID)
		lockConn.Release()
	})
	if _, err := pool.Exec(context.Background(), `TRUNCATE cloudforge.batch_images, cloudforge.batches, cloudforge.outbox_events, cloudforge.jobs`); err != nil {
		t.Fatalf("truncate job data: %v", err)
	}
	return pool
}

func createTestJob(t *testing.T, store *Store) Job {
	t.Helper()
	job, err := store.Create(context.Background(), CreateInput{
		Type:    TypeSyntheticSHA256,
		Payload: SHA256Payload{Text: "cloudforge", Iterations: 10},
	})
	if err != nil {
		t.Fatal(err)
	}
	return job
}

func TestStoreCreateAndGet(t *testing.T) {
	pool := integrationPool(t)
	store := NewStore(pool)
	created := createTestJob(t, store)
	if created.Status != StatusQueued || created.AttemptCount != 0 {
		t.Fatalf("created job = %#v", created)
	}

	got, err := store.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != created.ID || got.Status != StatusQueued {
		t.Fatalf("got job = %#v", got)
	}

	var eventID, eventType, aggregateID string
	var eventPayload json.RawMessage
	var publishedAt any
	err = pool.QueryRow(context.Background(), `
		SELECT id::text, event_type, aggregate_id::text, payload, published_at
		FROM cloudforge.outbox_events
		WHERE aggregate_id = $1
	`, created.ID).Scan(&eventID, &eventType, &aggregateID, &eventPayload, &publishedAt)
	if err != nil {
		t.Fatal(err)
	}
	var decodedPayload outbox.JobReadyPayload
	if err := json.Unmarshal(eventPayload, &decodedPayload); err != nil {
		t.Fatal(err)
	}
	if eventID == "" || eventType != outbox.EventTypeJobReady || aggregateID != created.ID || decodedPayload.JobID != created.ID || publishedAt != nil {
		t.Fatalf("outbox event: id=%q type=%q aggregate=%q payload=%s published=%v", eventID, eventType, aggregateID, eventPayload, publishedAt)
	}
	var jobCount, eventCount int
	if err := pool.QueryRow(context.Background(), `SELECT
		(SELECT count(*) FROM cloudforge.jobs WHERE id = $1),
		(SELECT count(*) FROM cloudforge.outbox_events WHERE aggregate_id = $1)
	`, created.ID).Scan(&jobCount, &eventCount); err != nil {
		t.Fatal(err)
	}
	if jobCount != 1 || eventCount != 1 {
		t.Fatalf("job count=%d event count=%d, want 1 each", jobCount, eventCount)
	}
}

func TestCreateRollsBackJobWhenOutboxInsertFails(t *testing.T) {
	pool := integrationPool(t)
	ctx := context.Background()
	_, err := pool.Exec(ctx, `
		CREATE FUNCTION cloudforge.test_reject_selected_outbox_event()
		RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF EXISTS (
				SELECT 1 FROM cloudforge.jobs
				WHERE id = NEW.aggregate_id
				  AND payload->>'text' = 'force-outbox-rollback-test'
			) THEN
				RAISE EXCEPTION 'intentional outbox insertion failure';
			END IF;
			RETURN NEW;
		END;
		$$;
		CREATE TRIGGER test_reject_selected_outbox_event
		BEFORE INSERT ON cloudforge.outbox_events
		FOR EACH ROW EXECUTE FUNCTION cloudforge.test_reject_selected_outbox_event();
	`)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `
			DROP TRIGGER IF EXISTS test_reject_selected_outbox_event ON cloudforge.outbox_events;
			DROP FUNCTION IF EXISTS cloudforge.test_reject_selected_outbox_event();
		`)
	})

	store := NewStore(pool)
	_, err = store.Create(ctx, CreateInput{
		Type:    TypeSyntheticSHA256,
		Payload: SHA256Payload{Text: "force-outbox-rollback-test", Iterations: 10},
	})
	if err == nil {
		t.Fatal("Create() succeeded, want outbox insertion error")
	}

	var jobCount, eventCount int
	if err := pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM cloudforge.jobs),
		(SELECT count(*) FROM cloudforge.outbox_events)
	`).Scan(&jobCount, &eventCount); err != nil {
		t.Fatal(err)
	}
	if jobCount != 0 || eventCount != 0 {
		t.Fatalf("job count=%d event count=%d, want both zero", jobCount, eventCount)
	}
}

func TestConcurrentClaimOnlyOneWorkerWins(t *testing.T) {
	store := NewStore(integrationPool(t))
	created := createTestJob(t, store)

	type outcome struct {
		job Job
		err error
	}
	start := make(chan struct{})
	outcomes := make(chan outcome, 2)
	var wg sync.WaitGroup
	for _, workerID := range []string{"worker-a", "worker-b"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			<-start
			job, err := store.ClaimNext(context.Background(), id)
			outcomes <- outcome{job: job, err: err}
		}(workerID)
	}
	close(start)
	wg.Wait()
	close(outcomes)

	wins, empty := 0, 0
	for result := range outcomes {
		switch result.err {
		case nil:
			wins++
			if result.job.ID != created.ID || result.job.Status != StatusRunning || result.job.AttemptCount != 1 {
				t.Fatalf("claimed job = %#v", result.job)
			}
		case ErrNoJobs:
			empty++
		default:
			t.Fatal(result.err)
		}
	}
	if wins != 1 || empty != 1 {
		t.Fatalf("wins=%d empty=%d, want 1 and 1", wins, empty)
	}
}

func TestConcurrentClaimByIDOnlyOneWorkerWins(t *testing.T) {
	store := NewStore(integrationPool(t))
	created := createTestJob(t, store)

	type outcome struct {
		job Job
		err error
	}
	start := make(chan struct{})
	outcomes := make(chan outcome, 2)
	var wg sync.WaitGroup
	for _, workerID := range []string{"worker-a", "worker-b"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			<-start
			job, err := store.ClaimByID(context.Background(), created.ID, id)
			outcomes <- outcome{job: job, err: err}
		}(workerID)
	}
	close(start)
	wg.Wait()
	close(outcomes)

	wins, rejected := 0, 0
	for result := range outcomes {
		switch result.err {
		case nil:
			wins++
			if result.job.ID != created.ID || result.job.Status != StatusRunning || result.job.AttemptCount != 1 {
				t.Fatalf("claimed job = %#v", result.job)
			}
		case ErrNotClaimable:
			rejected++
		default:
			t.Fatal(result.err)
		}
	}
	if wins != 1 || rejected != 1 {
		t.Fatalf("wins=%d rejected=%d, want 1 and 1", wins, rejected)
	}
}

func TestSuccessfulResultPersists(t *testing.T) {
	store := NewStore(integrationPool(t))
	created := createTestJob(t, store)
	if _, err := store.ClaimNext(context.Background(), "worker-a"); err != nil {
		t.Fatal(err)
	}
	result := json.RawMessage(`{"digest":"abc","iterations":10,"duration_ms":1}`)
	if err := store.MarkSucceeded(context.Background(), created.ID, "worker-a", result); err != nil {
		t.Fatal(err)
	}

	got, err := store.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	var decoded SHA256Result
	if err := json.Unmarshal(got.Result, &decoded); err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusSucceeded || got.CompletedAt == nil || decoded.Digest != "abc" || decoded.Iterations != 10 {
		t.Fatalf("completed job = %#v result=%s", got, got.Result)
	}
}

func TestFailurePersists(t *testing.T) {
	store := NewStore(integrationPool(t))
	created := createTestJob(t, store)
	if _, err := store.ClaimNext(context.Background(), "worker-a"); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkFailed(context.Background(), created.ID, "worker-a", "execution_failed", "synthetic failure"); err != nil {
		t.Fatal(err)
	}

	got, err := store.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusFailed || got.CompletedAt == nil || got.ErrorCode == nil || *got.ErrorCode != "execution_failed" || got.ErrorMessage == nil || *got.ErrorMessage != "synthetic failure" {
		t.Fatalf("failed job = %#v", got)
	}
}

func TestClaimWithoutCompletionRemainsRunning(t *testing.T) {
	store := NewStore(integrationPool(t))
	created := createTestJob(t, store)
	if _, err := store.ClaimNext(context.Background(), "crashed-worker"); err != nil {
		t.Fatal(err)
	}

	got, err := store.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusRunning || got.CompletedAt != nil {
		t.Fatalf("job after simulated crash = %#v", got)
	}
	if _, err := store.ClaimNext(context.Background(), "replacement-worker"); err != ErrNoJobs {
		t.Fatalf("replacement claim error = %v, want ErrNoJobs", err)
	}
}
