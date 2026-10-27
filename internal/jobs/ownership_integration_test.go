//go:build integration

package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestV5ClaimLeaseHeartbeatAndHistory(t *testing.T) {
	pool := integrationPool(t)
	store := NewStoreWithLease(pool, 20*time.Second)
	created := createTestJob(t, store)
	claimed, err := store.ClaimByID(context.Background(), created.ID, "worker-a")
	if err != nil {
		t.Fatal(err)
	}
	if claimed.AttemptID == nil || claimed.LeaseExpiresAt == nil || claimed.ClaimedBy == nil ||
		*claimed.ClaimedBy != "worker-a" || claimed.AttemptCount != 1 ||
		claimed.LeaseExpiresAt.Before(time.Now().Add(10*time.Second)) {
		t.Fatalf("bad claim: %+v", claimed)
	}
	if parsed, err := uuid.Parse(*claimed.AttemptID); err != nil || parsed.String() != *claimed.AttemptID {
		t.Fatalf("bad attempt ID: %v", claimed.AttemptID)
	}
	var worker, outcome string
	var count int16
	if err := pool.QueryRow(context.Background(), `SELECT worker_id, outcome, attempt_number FROM cloudforge.job_attempts WHERE attempt_id = $1`, *claimed.AttemptID).Scan(&worker, &outcome, &count); err != nil {
		t.Fatal(err)
	}
	if worker != "worker-a" || outcome != "running" || count != 1 {
		t.Fatalf("history = %q/%q/%d", worker, outcome, count)
	}
	if err := store.Heartbeat(context.Background(), created.ID, "worker-a", *claimed.AttemptID); err != nil {
		t.Fatal(err)
	}
	got, err := store.Get(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.LeaseExpiresAt.After(*claimed.LeaseExpiresAt) {
		t.Fatalf("lease did not renew: %v -> %v", claimed.LeaseExpiresAt, got.LeaseExpiresAt)
	}
	if err := store.Heartbeat(context.Background(), created.ID, "worker-a", uuid.NewString()); !errors.Is(err, ErrOwnershipLost) {
		t.Fatalf("stale token heartbeat: %v", err)
	}
	if err := store.Heartbeat(context.Background(), created.ID, "worker-b", *claimed.AttemptID); !errors.Is(err, ErrOwnershipLost) {
		t.Fatalf("wrong worker heartbeat: %v", err)
	}
	if _, err := store.ClaimByID(context.Background(), created.ID, "worker-b"); !errors.Is(err, ErrNotClaimable) {
		t.Fatalf("duplicate claim: %v", err)
	}
}

func TestV5ExpiredAndReplacedAttemptsCannotComplete(t *testing.T) {
	pool := integrationPool(t)
	store := NewStoreWithLease(pool, 20*time.Second)
	created := createTestJob(t, store)
	old, err := store.ClaimByID(context.Background(), created.ID, "same-worker-id")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE cloudforge.jobs SET lease_expires_at = clock_timestamp() - interval '1 second' WHERE id = $1`, created.ID); err != nil {
		t.Fatal(err)
	}
	result := json.RawMessage(`{"digest":"winning","iterations":1,"duration_ms":1}`)
	if err := store.Heartbeat(context.Background(), created.ID, "same-worker-id", *old.AttemptID); !errors.Is(err, ErrOwnershipLost) {
		t.Fatalf("expired heartbeat: %v", err)
	}
	if err := store.MarkSucceeded(context.Background(), created.ID, "same-worker-id", *old.AttemptID, result); !errors.Is(err, ErrOwnershipLost) {
		t.Fatalf("expired completion: %v", err)
	}
	if err := store.MarkFailed(context.Background(), created.ID, "same-worker-id", *old.AttemptID, "x", "x"); !errors.Is(err, ErrOwnershipLost) {
		t.Fatalf("expired failure: %v", err)
	}
	// Checkpoint 2 will perform this requeue in a transaction with outbox intent.
	// Here it is controlled test setup for the ownership generation fence only.
	if _, err := pool.Exec(context.Background(), `UPDATE cloudforge.job_attempts SET outcome = 'expired' WHERE attempt_id = $1`, *old.AttemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE cloudforge.jobs SET status = 'queued', claimed_by = NULL, attempt_id = NULL, lease_expires_at = NULL WHERE id = $1`, created.ID); err != nil {
		t.Fatal(err)
	}
	newAttempt, err := store.ClaimByID(context.Background(), created.ID, "same-worker-id")
	if err != nil {
		t.Fatal(err)
	}
	if *old.AttemptID == *newAttempt.AttemptID || newAttempt.AttemptCount != 2 {
		t.Fatalf("attempts old=%+v new=%+v", old, newAttempt)
	}
	if err := store.Heartbeat(context.Background(), created.ID, "same-worker-id", *old.AttemptID); !errors.Is(err, ErrOwnershipLost) {
		t.Fatalf("replaced heartbeat: %v", err)
	}
	if err := store.MarkSucceeded(context.Background(), created.ID, "same-worker-id", *old.AttemptID, json.RawMessage(`{"digest":"stale"}`)); !errors.Is(err, ErrOwnershipLost) {
		t.Fatalf("replaced completion: %v", err)
	}
	if err := store.MarkSucceeded(context.Background(), created.ID, "same-worker-id", *newAttempt.AttemptID, result); err != nil {
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
	if got.Status != StatusSucceeded || decoded.Digest != "winning" {
		t.Fatalf("winner changed: %+v", got)
	}
	if err := store.MarkFailed(context.Background(), created.ID, "same-worker-id", *old.AttemptID, "stale", "stale"); !errors.Is(err, ErrOwnershipLost) {
		t.Fatalf("replaced failure: %v", err)
	}
}

func TestV5ImageResultMustPointToWinningAttempt(t *testing.T) {
	pool := integrationPool(t)
	store := NewStore(pool)
	job, err := store.CreateThumbnail(context.Background(), ThumbnailPayload{InputID: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimByID(context.Background(), job.ID, "image-worker")
	if err != nil {
		t.Fatal(err)
	}
	bad, _ := json.Marshal(ThumbnailResult{OutputID: job.ID, AttemptID: uuid.NewString(), Width: 2, Height: 2, Format: "png", Bytes: 40})
	if err := store.MarkSucceeded(context.Background(), job.ID, "image-worker", *claimed.AttemptID, bad); !errors.Is(err, ErrOwnershipLost) {
		t.Fatalf("mismatched output accepted: %v", err)
	}
	got, err := store.Get(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusRunning || got.Result != nil {
		t.Fatalf("mismatch modified job: %+v", got)
	}
	good, _ := json.Marshal(ThumbnailResult{OutputID: job.ID, AttemptID: *claimed.AttemptID, Width: 2, Height: 2, Format: "png", Bytes: 40})
	if err := store.MarkSucceeded(context.Background(), job.ID, "image-worker", *claimed.AttemptID, good); err != nil {
		t.Fatal(err)
	}
}
