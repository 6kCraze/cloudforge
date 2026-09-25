//go:build integration

package worker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/6kCraze/cloudforge/internal/database"
	"github.com/6kCraze/cloudforge/internal/jobs"
)

func TestProcessorErrorPersistsFailedJob(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL is not set")
	}
	pool, err := database.Open(context.Background(), databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	lockConn, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lockConn.Exec(context.Background(), `SELECT pg_advisory_lock($1)`, int64(240022)); err != nil {
		lockConn.Release()
		t.Fatal(err)
	}
	defer func() {
		_, _ = lockConn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, int64(240022))
		lockConn.Release()
	}()
	if _, err := pool.Exec(context.Background(), `TRUNCATE cloudforge.outbox_events, cloudforge.jobs`); err != nil {
		t.Fatal(err)
	}

	store := jobs.NewStore(pool)
	created, err := store.Create(context.Background(), jobs.CreateInput{
		Type:    jobs.TypeSyntheticSHA256,
		Payload: jobs.SHA256Payload{Text: "cloudforge", Iterations: 10},
	})
	if err != nil {
		t.Fatal(err)
	}
	runner := New(store, "failing-worker", time.Millisecond, func(context.Context, jobs.Job) (json.RawMessage, error) {
		return nil, errors.New("processor failed")
	}, discardLogger())
	processed, err := runner.ProcessOne(context.Background())
	if err != nil || !processed {
		t.Fatalf("processed=%v err=%v", processed, err)
	}

	got, err := store.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != jobs.StatusFailed || got.ErrorCode == nil || *got.ErrorCode != "execution_failed" || got.ErrorMessage == nil || *got.ErrorMessage != "processor failed" {
		t.Fatalf("failed job = %#v", got)
	}
}
