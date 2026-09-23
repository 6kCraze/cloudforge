//go:build integration

package jobs

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"testing"

	"github.com/6kCraze/cloudforge/internal/database"
	"github.com/jackc/pgx/v5/pgxpool"
)

func integrationPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL is not set")
	}
	pool, err := database.Open(context.Background(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(context.Background(), `TRUNCATE cloudforge.jobs`); err != nil {
		t.Fatalf("truncate jobs: %v", err)
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
	store := NewStore(integrationPool(t))
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
