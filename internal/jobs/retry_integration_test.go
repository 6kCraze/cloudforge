//go:build integration

package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestV5ExpiredLeaseRecoveryAndTransactionalRedispatch(t *testing.T) {
	pool := integrationPool(t)
	store := NewStoreWithPolicy(pool, 20*time.Second, 2*time.Second, 4*time.Second)
	created := createTestJob(t, store)
	old, err := store.ClaimByID(context.Background(), created.ID, "old-worker")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE cloudforge.jobs SET lease_expires_at = clock_timestamp() - interval '1 second' WHERE id = $1`, created.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := store.RecoverExpired(context.Background()); err != nil || n != 1 {
		t.Fatalf("expired=%d err=%v", n, err)
	}
	if n, err := store.RecoverExpired(context.Background()); err != nil || n != 0 {
		t.Fatalf("duplicate recovery=%d err=%v", n, err)
	}
	job, err := store.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if job.Status != StatusRetryWait || job.NextRetryAt == nil || job.AttemptCount != 1 || job.AttemptID != nil {
		t.Fatalf("recovered job=%+v", job)
	}
	if delay := job.NextRetryAt.Sub(job.UpdatedAt); delay < 1900*time.Millisecond || delay > 2100*time.Millisecond {
		t.Fatalf("retry delay=%s, want 2s", delay)
	}
	var outcome, decision, code string
	if err := pool.QueryRow(context.Background(), `SELECT outcome, retry_decision, error_code FROM cloudforge.job_attempts WHERE attempt_id = $1`, *old.AttemptID).Scan(&outcome, &decision, &code); err != nil {
		t.Fatal(err)
	}
	if outcome != "expired" || decision != "scheduled" || code != "lease_expired" {
		t.Fatalf("attempt=%s/%s/%s", outcome, decision, code)
	}
	if err := store.Heartbeat(context.Background(), created.ID, "old-worker", *old.AttemptID); !errors.Is(err, ErrOwnershipLost) {
		t.Fatalf("old heartbeat=%v", err)
	}
	if err := store.MarkSucceeded(context.Background(), created.ID, "old-worker", *old.AttemptID, json.RawMessage(`{"stale":true}`)); !errors.Is(err, ErrOwnershipLost) {
		t.Fatalf("old completion=%v", err)
	}
	if _, err := store.ClaimByID(context.Background(), created.ID, "new-worker"); !errors.Is(err, ErrNotClaimable) {
		t.Fatalf("early claim=%v", err)
	}
	if n, err := store.ReleaseDue(context.Background()); err != nil || n != 0 {
		t.Fatalf("early release=%d err=%v", n, err)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE cloudforge.jobs SET next_retry_at = clock_timestamp() - interval '1 second' WHERE id = $1`, created.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := store.ReleaseDue(context.Background()); err != nil || n != 1 {
		t.Fatalf("due release=%d err=%v", n, err)
	}
	if n, err := store.ReleaseDue(context.Background()); err != nil || n != 0 {
		t.Fatalf("duplicate due release=%d err=%v", n, err)
	}
	var events, unpublished, minGen, maxGen int
	if err := pool.QueryRow(context.Background(), `SELECT count(*), count(*) FILTER (WHERE published_at IS NULL),
		min(delivery_generation), max(delivery_generation) FROM cloudforge.outbox_events WHERE aggregate_id = $1`, created.ID).Scan(&events, &unpublished, &minGen, &maxGen); err != nil {
		t.Fatal(err)
	}
	if events != 2 || unpublished != 2 || minGen != 0 || maxGen != 1 {
		t.Fatalf("outbox=%d unpublished=%d generations=%d..%d", events, unpublished, minGen, maxGen)
	}
	if _, err := pool.Exec(context.Background(), `INSERT INTO cloudforge.outbox_events (id,event_type,aggregate_id,payload,delivery_generation)
		VALUES ($1,'job.ready',$2,$3,1)`, uuid.NewString(), created.ID, json.RawMessage(`{"job_id":"`+created.ID+`"}`)); err == nil {
		t.Fatal("duplicate ready event for one generation accepted")
	}
	newAttempt, err := store.ClaimByID(context.Background(), created.ID, "new-worker")
	if err != nil {
		t.Fatal(err)
	}
	if newAttempt.AttemptCount != 2 || *newAttempt.AttemptID == *old.AttemptID {
		t.Fatalf("new attempt=%+v", newAttempt)
	}
	if err := store.MarkSucceeded(context.Background(), created.ID, "old-worker", *old.AttemptID, json.RawMessage(`{"stale":true}`)); !errors.Is(err, ErrOwnershipLost) {
		t.Fatalf("replaced completion=%v", err)
	}
	if err := store.MarkSucceeded(context.Background(), created.ID, "new-worker", *newAttempt.AttemptID, json.RawMessage(`{"digest":"ok"}`)); err != nil {
		t.Fatal(err)
	}
}

func TestV5ConcurrentRecoveryProcessesExpiredAttemptOnce(t *testing.T) {
	pool := integrationPool(t)
	store := NewStore(pool)
	created := createTestJob(t, store)
	claimed, err := store.ClaimByID(context.Background(), created.ID, "worker")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE cloudforge.jobs SET lease_expires_at = clock_timestamp() - interval '1 second' WHERE id = $1`, created.ID); err != nil {
		t.Fatal(err)
	}
	type recoveryResult struct {
		count int
		err   error
	}
	results := make(chan recoveryResult, 2)
	var group sync.WaitGroup
	for i := 0; i < 2; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			n, err := store.RecoverExpired(context.Background())
			results <- recoveryResult{n, err}
		}()
	}
	group.Wait()
	close(results)
	total := 0
	for result := range results {
		if result.err != nil {
			t.Fatal(result.err)
		}
		total += result.count
	}
	if total != 1 {
		t.Fatalf("concurrent recovery count=%d", total)
	}
	var history int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM cloudforge.job_attempts WHERE attempt_id = $1 AND outcome = 'expired'`, *claimed.AttemptID).Scan(&history); err != nil {
		t.Fatal(err)
	}
	if history != 1 {
		t.Fatalf("expired history count=%d", history)
	}
}

func TestV5RecoverableRetriesStopAfterThreeAttempts(t *testing.T) {
	pool := integrationPool(t)
	store := NewStoreWithPolicy(pool, 20*time.Second, 2*time.Second, 3*time.Second)
	created := createTestJob(t, store)
	for attempt := int16(1); attempt <= MaxAttempts; attempt++ {
		claimed, err := store.ClaimByID(context.Background(), created.ID, "worker")
		if err != nil {
			t.Fatal(err)
		}
		if claimed.AttemptCount != attempt {
			t.Fatalf("claim count=%d want %d", claimed.AttemptCount, attempt)
		}
		status, err := store.ScheduleRetry(context.Background(), created.ID, "worker", *claimed.AttemptID, "execution_failed", "temporary failure")
		if err != nil {
			t.Fatal(err)
		}
		decision := "scheduled"
		if attempt == MaxAttempts {
			decision = "exhausted"
		}
		var gotDecision, gotCode string
		if err := pool.QueryRow(context.Background(), `SELECT retry_decision, error_code FROM cloudforge.job_attempts WHERE attempt_id = $1`, *claimed.AttemptID).Scan(&gotDecision, &gotCode); err != nil {
			t.Fatal(err)
		}
		if gotDecision != decision || gotCode != "execution_failed" {
			t.Fatalf("attempt %d history=%s/%s", attempt, gotDecision, gotCode)
		}
		job, err := store.Get(context.Background(), created.ID)
		if err != nil {
			t.Fatal(err)
		}
		if attempt == MaxAttempts {
			if status != StatusFailed || job.CompletedAt == nil || job.NextRetryAt != nil {
				t.Fatalf("exhausted job=%+v", job)
			}
			break
		}
		if status != StatusRetryWait || job.NextRetryAt == nil {
			t.Fatalf("retry job=%+v", job)
		}
		want := 2 * time.Second
		if attempt == 2 {
			want = 3 * time.Second
		}
		if delta := job.NextRetryAt.Sub(job.UpdatedAt); delta < want-100*time.Millisecond || delta > want+100*time.Millisecond {
			t.Fatalf("attempt %d delay=%s want %s", attempt, delta, want)
		}
		if _, err := pool.Exec(context.Background(), `UPDATE cloudforge.jobs SET next_retry_at = clock_timestamp() - interval '1 second' WHERE id = $1`, created.ID); err != nil {
			t.Fatal(err)
		}
		if n, err := store.ReleaseDue(context.Background()); err != nil || n != 1 {
			t.Fatalf("release=%d err=%v", n, err)
		}
	}
	var events int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM cloudforge.outbox_events WHERE aggregate_id = $1`, created.ID).Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 3 {
		t.Fatalf("outbox events=%d want initial plus two retries", events)
	}
	if n, err := store.RecoverExpired(context.Background()); err != nil || n != 0 {
		t.Fatalf("terminal recovery=%d err=%v", n, err)
	}
}

func TestV5PermanentFailureHasNoRetry(t *testing.T) {
	pool := integrationPool(t)
	store := NewStore(pool)
	created := createTestJob(t, store)
	claimed, err := store.ClaimByID(context.Background(), created.ID, "worker")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkFailed(context.Background(), created.ID, "worker", *claimed.AttemptID, "invalid_job", "bad payload"); err != nil {
		t.Fatal(err)
	}
	job, err := store.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if job.Status != StatusFailed || job.NextRetryAt != nil || job.AttemptCount != 1 {
		t.Fatalf("permanent job=%+v", job)
	}
	var decision, message string
	if err := pool.QueryRow(context.Background(), `SELECT retry_decision, error_message FROM cloudforge.job_attempts WHERE attempt_id = $1`, *claimed.AttemptID).Scan(&decision, &message); err != nil {
		t.Fatal(err)
	}
	if decision != "permanent" || message != "bad payload" {
		t.Fatalf("history=%s/%s", decision, message)
	}
	if n, err := store.ReleaseDue(context.Background()); err != nil || n != 0 {
		t.Fatalf("permanent release=%d err=%v", n, err)
	}
}

func TestV5BatchReportsWaitingRetryWithoutDownload(t *testing.T) {
	pool := integrationPool(t)
	store := NewStore(pool)
	batch, err := store.CreateBatch(context.Background(), []BatchInput{{InputID: uuid.NewString(), OriginalName: "small.png", Format: "png", Bytes: 10, Width: 2, Height: 2}})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimByID(context.Background(), batch.Jobs[0].ID, "worker")
	if err != nil {
		t.Fatal(err)
	}
	if status, err := store.ScheduleRetry(context.Background(), claimed.ID, "worker", *claimed.AttemptID, "execution_failed", "temporary failure"); err != nil || status != StatusRetryWait {
		t.Fatalf("status=%s err=%v", status, err)
	}
	got, err := store.GetBatch(context.Background(), batch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.RetryWait != 1 || got.Succeeded != 0 || got.Failed != 0 || got.Jobs[0].Status != StatusRetryWait || got.Jobs[0].NextRetryAt == nil || got.Jobs[0].OutputURL != "" {
		t.Fatalf("batch retry view=%+v", got)
	}
}
