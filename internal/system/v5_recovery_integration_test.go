//go:build integration

package system_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/6kCraze/cloudforge/internal/dispatcher"
	"github.com/6kCraze/cloudforge/internal/httpapi"
	"github.com/6kCraze/cloudforge/internal/images"
	"github.com/6kCraze/cloudforge/internal/jobs"
	"github.com/6kCraze/cloudforge/internal/outbox"
)

type unavailablePublisher struct{}

func (unavailablePublisher) Publish(context.Context, outbox.Event) error {
	return errors.New("broker unavailable")
}

func TestV5RetryImageOutputsStayIsolatedAndOnlyWinnerDownloads(t *testing.T) {
	f := newV3Fixture(t)
	store := jobs.NewStoreWithPolicy(f.pool, 20*time.Second, 200*time.Millisecond, time.Second)
	root := t.TempDir()
	storage, err := images.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	input, err := storage.SaveInput(bytes.NewReader(v4Image(t, 3, 2, "png", 255)))
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.CreateThumbnail(context.Background(), jobs.ThumbnailPayload{InputID: input.ID})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(httpapi.NewWithImages(jobs.NewService(store), f.pool, f.logger, storage).Handler())
	t.Cleanup(server.Close)
	first, err := store.ClaimByID(context.Background(), created.ID, "image-worker")
	if err != nil {
		t.Fatal(err)
	}
	firstResult, err := storage.Execute(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}
	firstPath := filepath.Join(root, "outputs", created.ID, *first.AttemptID, "thumbnail.png")
	firstBytes, err := os.ReadFile(firstPath)
	if err != nil {
		t.Fatal(err)
	}
	if status, err := store.ScheduleRetry(context.Background(), created.ID, "image-worker", *first.AttemptID, "execution_failed", "temporary completion failure"); err != nil || status != jobs.StatusRetryWait {
		t.Fatalf("retry status=%s err=%v", status, err)
	}
	v4Download(t, server, created.ID, 409)
	if _, err := f.pool.Exec(context.Background(), `UPDATE cloudforge.jobs SET next_retry_at = clock_timestamp() - interval '1 second' WHERE id = $1`, created.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := store.ReleaseDue(context.Background()); err != nil || n != 1 {
		t.Fatalf("release=%d err=%v", n, err)
	}
	second, err := store.ClaimByID(context.Background(), created.ID, "image-worker")
	if err != nil {
		t.Fatal(err)
	}
	secondResult, err := storage.Execute(context.Background(), second)
	if err != nil {
		t.Fatal(err)
	}
	if *first.AttemptID == *second.AttemptID || bytes.Equal(firstResult, secondResult) {
		t.Fatal("retry reused output identity")
	}
	if err := store.MarkSucceeded(context.Background(), created.ID, "image-worker", *first.AttemptID, firstResult); !errors.Is(err, jobs.ErrOwnershipLost) {
		t.Fatalf("old output finalized: %v", err)
	}
	if err := store.MarkSucceeded(context.Background(), created.ID, "image-worker", *second.AttemptID, secondResult); err != nil {
		t.Fatal(err)
	}
	if now, err := os.ReadFile(firstPath); err != nil || !bytes.Equal(now, firstBytes) {
		t.Fatalf("old output changed: %v", err)
	}
	secondPath := filepath.Join(root, "outputs", created.ID, *second.AttemptID, "thumbnail.png")
	secondBytes, err := os.ReadFile(secondPath)
	if err != nil {
		t.Fatal(err)
	}
	// Damage only the uncommitted first candidate. The download must still use
	// the second attempt named by PostgreSQL's succeeded result.
	if err := os.WriteFile(firstPath, []byte("stale candidate"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := v4Download(t, server, created.ID, 200); !bytes.Equal(got, secondBytes) {
		t.Fatal("download did not use committed attempt")
	}
	var result jobs.ThumbnailResult
	if err := json.Unmarshal(secondResult, &result); err != nil || result.AttemptID != *second.AttemptID {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

// An old process can remain alive after losing its lease. Even if its
// processor ignores cancellation and finishes late, only the new attempt may
// publish a result, and the download must resolve through PostgreSQL's winner.
func TestV5LiveStaleImageAttemptCannotReplaceWinner(t *testing.T) {
	f := newV3Fixture(t)
	store := jobs.NewStoreWithPolicy(f.pool, 10*time.Second, 20*time.Millisecond, 50*time.Millisecond)
	root := t.TempDir()
	storage, err := images.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	input, err := storage.SaveInput(bytes.NewReader(v4Image(t, 3, 2, "png", 255)))
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.CreateThumbnail(context.Background(), jobs.ThumbnailPayload{InputID: input.ID})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(httpapi.NewWithImages(jobs.NewService(store), f.pool, f.logger, storage).Handler())
	t.Cleanup(server.Close)
	old, err := store.ClaimByID(context.Background(), created.ID, "worker-a")
	if err != nil {
		t.Fatal(err)
	}
	// The old worker is still alive but blocked in its processor. Force only
	// this isolated test job's lease to expire, as if renewal stopped.
	startOld := make(chan struct{})
	oldDone := make(chan error, 1)
	go func() {
		defer close(oldDone)
		<-startOld
		result, err := storage.Execute(context.Background(), old)
		if err == nil {
			err = store.MarkSucceeded(context.Background(), old.ID, "worker-a", *old.AttemptID, result)
		}
		oldDone <- err
	}()
	t.Cleanup(func() {
		select {
		case <-startOld:
		default:
			close(startOld)
		}
		select {
		case <-oldDone:
		case <-time.After(2 * time.Second):
			t.Error("old processor did not exit")
		}
	})
	if _, err := f.pool.Exec(context.Background(), `UPDATE cloudforge.jobs SET lease_expires_at = clock_timestamp() - interval '1 second' WHERE id = $1`, created.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.Heartbeat(context.Background(), old.ID, "worker-a", *old.AttemptID); !errors.Is(err, jobs.ErrOwnershipLost) {
		t.Fatalf("expired renewal=%v", err)
	}
	if n, err := store.RecoverExpired(context.Background()); err != nil || n != 1 {
		t.Fatalf("recovery=%d err=%v", n, err)
	}
	if _, err := f.pool.Exec(context.Background(), `UPDATE cloudforge.jobs SET next_retry_at = clock_timestamp() - interval '1 second' WHERE id = $1`, created.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := store.ReleaseDue(context.Background()); err != nil || n != 1 {
		t.Fatalf("release=%d err=%v", n, err)
	}
	newAttempt, err := store.ClaimByID(context.Background(), created.ID, "worker-b")
	if err != nil {
		t.Fatal(err)
	}
	result, err := storage.Execute(context.Background(), newAttempt)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkSucceeded(context.Background(), created.ID, "worker-b", *newAttempt.AttemptID, result); err != nil {
		t.Fatal(err)
	}
	winnerPath := filepath.Join(root, "outputs", created.ID, *newAttempt.AttemptID, "thumbnail.png")
	winner, err := os.ReadFile(winnerPath)
	if err != nil {
		t.Fatal(err)
	}
	close(startOld)
	select {
	case err := <-oldDone:
		if !errors.Is(err, jobs.ErrOwnershipLost) {
			t.Fatalf("late completion=%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("old processor did not finish")
	}
	if got, err := os.ReadFile(winnerPath); err != nil || !bytes.Equal(got, winner) {
		t.Fatalf("winner file replaced: %v", err)
	}
	if got := v4Download(t, server, created.ID, 200); !bytes.Equal(got, winner) {
		t.Fatal("download exposed stale candidate")
	}
	completed := f.getJob(t, created.ID)
	if completed.Status != jobs.StatusSucceeded || completed.AttemptCount != 2 || *completed.AttemptID != *newAttempt.AttemptID {
		t.Fatalf("winner state=%+v", completed)
	}
}
func (unavailablePublisher) Done() <-chan error { return nil }
func (unavailablePublisher) Close() error       { return nil }

func TestV5RecoveredJobSurvivesPublicationFailureAndCompletes(t *testing.T) {
	f := newV3Fixture(t)
	store := jobs.NewStoreWithPolicy(f.pool, 20*time.Second, 200*time.Millisecond, time.Second)
	created, initial := f.createJob(t, "recovered work", 2)
	// The initial notification was already consumed before the old worker died.
	if err := f.outbox.MarkPublished(context.Background(), initial.ID); err != nil {
		t.Fatal(err)
	}
	old, err := store.ClaimByID(context.Background(), created.ID, "crashed-worker")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(context.Background(), `UPDATE cloudforge.jobs SET lease_expires_at = clock_timestamp() - interval '1 second' WHERE id = $1`, created.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := store.RecoverExpired(context.Background()); err != nil || n != 1 {
		t.Fatalf("recover=%d err=%v", n, err)
	}
	if _, err := f.pool.Exec(context.Background(), `UPDATE cloudforge.jobs SET next_retry_at = clock_timestamp() - interval '1 second' WHERE id = $1`, created.ID); err != nil {
		t.Fatal(err)
	}
	if n, err := store.ReleaseDue(context.Background()); err != nil || n != 1 {
		t.Fatalf("release=%d err=%v", n, err)
	}
	if err := store.Heartbeat(context.Background(), created.ID, "crashed-worker", *old.AttemptID); !errors.Is(err, jobs.ErrOwnershipLost) {
		t.Fatalf("stale heartbeat=%v", err)
	}
	d := dispatcher.New(f.outbox, nil, time.Millisecond, time.Millisecond, time.Second, f.logger)
	if _, err := d.DispatchBatch(context.Background(), unavailablePublisher{}); err == nil {
		t.Fatal("expected simulated broker publication failure")
	}
	pending, err := f.outbox.ListUnpublished(context.Background(), 10)
	if err != nil || len(pending) != 1 || pending[0].AggregateID != created.ID {
		t.Fatalf("publication intent lost: events=%v err=%v", pending, err)
	}
	if count, err := d.DispatchBatch(context.Background(), f.publisher); err != nil || count != 1 {
		t.Fatalf("confirmed publication count=%d err=%v", count, err)
	}
	f.waitForReady(t, 1)
	f.startWorker(t, "replacement-worker", store, jobs.Execute, func() {})
	f.waitForConsumers(t, 1)
	completed := f.waitForStatus(t, created.ID, jobs.StatusSucceeded)
	if completed.AttemptCount != 2 || completed.AttemptID == nil || *completed.AttemptID == *old.AttemptID {
		t.Fatalf("replacement attempt=%+v", completed)
	}
	f.waitForACKs(t, 1)
	if err := store.MarkSucceeded(context.Background(), created.ID, "crashed-worker", *old.AttemptID, []byte(`{"stale":true}`)); !errors.Is(err, jobs.ErrOwnershipLost) {
		t.Fatalf("old completion=%v", err)
	}
}

func TestV5RecoveryLoopAutomaticallyRequeuesWithoutBrokerPublisher(t *testing.T) {
	f := newV3Fixture(t)
	store := jobs.NewStoreWithPolicy(f.pool, 20*time.Second, 5*time.Second, 5*time.Second)
	created, initial := f.createJob(t, "automatic recovery", 2)
	if err := f.outbox.MarkPublished(context.Background(), initial.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimByID(context.Background(), created.ID, "expired-worker"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(context.Background(), `UPDATE cloudforge.jobs SET lease_expires_at = clock_timestamp() - interval '1 second' WHERE id = $1`, created.ID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { dispatcher.RunRecovery(ctx, store, 10*time.Millisecond, f.logger); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("recovery loop cleanup timeout")
		}
	})
	waiting := f.waitForStatus(t, created.ID, jobs.StatusRetryWait)
	if waiting.NextRetryAt == nil {
		t.Fatal("retry time missing")
	}
	if _, err := f.pool.Exec(context.Background(), `UPDATE cloudforge.jobs SET next_retry_at = clock_timestamp() - interval '1 second' WHERE id = $1`, created.ID); err != nil {
		t.Fatal(err)
	}
	f.waitForStatus(t, created.ID, jobs.StatusQueued)
	pending, err := f.outbox.ListUnpublished(context.Background(), 10)
	if err != nil || len(pending) != 1 || pending[0].AggregateID != created.ID {
		t.Fatalf("automatic outbox intent=%v err=%v", pending, err)
	}
}
