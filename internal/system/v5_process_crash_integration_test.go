//go:build integration

package system_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/6kCraze/cloudforge/internal/database"
	"github.com/6kCraze/cloudforge/internal/dispatcher"
	"github.com/6kCraze/cloudforge/internal/httpapi"
	"github.com/6kCraze/cloudforge/internal/images"
	"github.com/6kCraze/cloudforge/internal/jobs"
	cloudrabbit "github.com/6kCraze/cloudforge/internal/rabbitmq"
	"github.com/6kCraze/cloudforge/internal/testenv"
	"github.com/6kCraze/cloudforge/internal/worker"
)

// This helper runs inside a separate copy of the Go test executable. The
// parent process may terminate it without affecting any unrelated Go process.
func TestV5WorkerProcessHelper(t *testing.T) {
	if os.Getenv("CLOUDFORGE_WORKER_HELPER") != "1" {
		t.Skip("subprocess helper")
	}
	cfg, err := testenv.Load()
	if err != nil {
		t.Fatal(err)
	}
	pool, err := database.Open(context.Background(), cfg.DatabaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	storage, err := images.Open(os.Getenv("CLOUDFORGE_HELPER_STORAGE"))
	if err != nil {
		t.Fatal(err)
	}
	defer storage.Close()
	workerID := os.Getenv("CLOUDFORGE_HELPER_WORKER_ID")
	processor := storage.Execute
	if os.Getenv("CLOUDFORGE_HELPER_MODE") == "block" {
		processor = func(ctx context.Context, job jobs.Job) (json.RawMessage, error) {
			if job.Type != jobs.TypeImageThumbnail || job.AttemptID == nil {
				return nil, fmt.Errorf("unexpected blocked job: %+v", job)
			}
			if err := os.WriteFile(os.Getenv("CLOUDFORGE_HELPER_SIGNAL"), []byte(*job.AttemptID), 0600); err != nil {
				return nil, err
			}
			<-ctx.Done()
			return nil, ctx.Err()
		}
	}
	if os.Getenv("CLOUDFORGE_HELPER_MODE") == "output_then_block" {
		processor = func(ctx context.Context, job jobs.Job) (json.RawMessage, error) {
			result, err := storage.Execute(ctx, job)
			if err != nil {
				return nil, err
			}
			if err := os.WriteFile(os.Getenv("CLOUDFORGE_HELPER_SIGNAL"), []byte(*job.AttemptID), 0600); err != nil {
				return nil, err
			}
			<-ctx.Done()
			return result, ctx.Err()
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopPath := os.Getenv("CLOUDFORGE_HELPER_STOP")
	go func() {
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if _, err := os.Stat(stopPath); err == nil {
					cancel()
					return
				}
			}
		}
	}()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := jobs.NewStoreWithPolicy(pool, 900*time.Millisecond, 500*time.Millisecond, time.Second)
	handler := worker.NewHandler(store, workerID, processor, logger, 100*time.Millisecond)
	runner := worker.NewRabbitWorker(handler, func(ctx context.Context) (worker.DeliveryConsumer, error) {
		return cloudrabbit.OpenConsumer(ctx, cfg.RabbitMQURL, workerID)
	}, workerID, 20*time.Millisecond, 100*time.Millisecond, logger)
	if err := runner.Run(ctx); err != nil {
		t.Fatal(err)
	}
}

type ownedWorkerProcess struct {
	cmd     *exec.Cmd
	done    chan struct{}
	err     error
	logPath string
}

func startOwnedWorker(t *testing.T, root, workerID, mode, signalPath, stopPath string) *ownedWorkerProcess {
	t.Helper()
	logPath := filepath.Join(t.TempDir(), workerID+".log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestV5WorkerProcessHelper$")
	cmd.Env = append(os.Environ(),
		"CLOUDFORGE_WORKER_HELPER=1", "CLOUDFORGE_HELPER_STORAGE="+root,
		"CLOUDFORGE_HELPER_WORKER_ID="+workerID, "CLOUDFORGE_HELPER_MODE="+mode,
		"CLOUDFORGE_HELPER_SIGNAL="+signalPath, "CLOUDFORGE_HELPER_STOP="+stopPath)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		t.Fatal(err)
	}
	process := &ownedWorkerProcess{cmd: cmd, done: make(chan struct{}), logPath: logPath}
	go func() { process.err = cmd.Wait(); _ = logFile.Close(); close(process.done) }()
	t.Cleanup(func() { process.killAndWait(t) })
	return process
}

func (p *ownedWorkerProcess) killAndWait(t *testing.T) {
	t.Helper()
	select {
	case <-p.done:
		return
	default:
	}
	_ = p.cmd.Process.Kill() // This PID belongs to the test-owned child only.
	select {
	case <-p.done:
	case <-time.After(5 * time.Second):
		t.Error("test-owned worker process did not exit")
	}
}

func (p *ownedWorkerProcess) stopGracefully(t *testing.T, stopPath string) {
	t.Helper()
	if err := os.WriteFile(stopPath, []byte("stop"), 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.done:
		if p.err != nil {
			data, _ := os.ReadFile(p.logPath)
			t.Fatalf("replacement worker exited: %v\n%s", p.err, data)
		}
	case <-time.After(5 * time.Second):
		p.killAndWait(t)
		t.Fatal("replacement worker did not stop gracefully")
	}
}

func TestV5RealWorkerProcessCrashRecoversImage(t *testing.T) {
	for _, mode := range []string{"block", "output_then_block"} {
		t.Run(mode, func(t *testing.T) { runV5WorkerProcessCrash(t, mode) })
	}
}

func runV5WorkerProcessCrash(t *testing.T, mode string) {
	f := newV3Fixture(t)
	root := t.TempDir()
	storage, err := images.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	server := httptest.NewServer(httpapi.NewWithImages(jobs.NewService(f.jobs), f.pool, f.logger, storage).Handler())
	t.Cleanup(server.Close)
	res, err := server.Client().Post(server.URL+"/v1/images", "image/png", bytes.NewReader(v4Image(t, 3, 2, "png", 255)))
	if err != nil {
		t.Fatal(err)
	}
	var accepted struct{ ID, Status string }
	if err := json.NewDecoder(res.Body).Decode(&accepted); err != nil {
		_ = res.Body.Close()
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusAccepted || accepted.Status != jobs.StatusQueued {
		t.Fatalf("upload status=%d accepted=%+v", res.StatusCode, accepted)
	}
	events, err := f.outbox.ListUnpublished(context.Background(), 10)
	if err != nil || len(events) != 1 {
		t.Fatalf("initial event=%v err=%v", events, err)
	}
	f.publish(t, events[0])
	if err := f.outbox.MarkPublished(context.Background(), events[0].ID); err != nil {
		t.Fatal(err)
	}
	signalPath := filepath.Join(t.TempDir(), "claimed.signal")
	stopA := filepath.Join(t.TempDir(), "stop-a.signal")
	a := startOwnedWorker(t, root, "v5-crash-worker-a", mode, signalPath, stopA)
	deadline := time.Now().Add(8 * time.Second)
	for {
		if _, err := os.Stat(signalPath); err == nil {
			break
		}
		select {
		case <-a.done:
			data, _ := os.ReadFile(a.logPath)
			t.Fatalf("worker A exited before claim: %v\n%s", a.err, data)
		default:
		}
		if time.Now().After(deadline) {
			data, _ := os.ReadFile(a.logPath)
			t.Fatalf("worker A never claimed: %s", data)
		}
		time.Sleep(10 * time.Millisecond)
	}
	claimed := f.waitForStatus(t, accepted.ID, jobs.StatusRunning)
	if claimed.AttemptID == nil || claimed.LeaseExpiresAt == nil || claimed.AttemptCount != 1 {
		t.Fatalf("invalid first claim: %+v", claimed)
	}
	if mode == "output_then_block" {
		oldPath := filepath.Join(root, "outputs", accepted.ID, *claimed.AttemptID, "thumbnail.png")
		if data, err := os.ReadFile(oldPath); err != nil || !bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")) {
			t.Fatalf("worker A did not publish candidate before crash: %v", err)
		}
	}
	var active bool
	if err := f.pool.QueryRow(context.Background(), `SELECT lease_expires_at > clock_timestamp() FROM cloudforge.jobs WHERE id = $1`, accepted.ID).Scan(&active); err != nil || !active {
		t.Fatalf("first lease inactive: active=%v err=%v", active, err)
	}
	var heartbeatBefore time.Time
	deadline = time.Now().Add(3 * time.Second)
	for {
		if err := f.pool.QueryRow(context.Background(), `SELECT last_heartbeat_at FROM cloudforge.job_attempts WHERE attempt_id = $1`, *claimed.AttemptID).Scan(&heartbeatBefore); err != nil {
			t.Fatal(err)
		}
		if heartbeatBefore.After(*claimed.StartedAt) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("worker A never renewed its lease")
		}
		time.Sleep(20 * time.Millisecond)
	}
	a.killAndWait(t)
	if a.err == nil {
		t.Fatal("worker A was not forcibly terminated")
	}
	f.waitForReady(t, 1) // RabbitMQ redelivered the killed process's unacked message.
	if _, ok, err := f.admin.Get(cloudrabbit.QueueName, true); err != nil || !ok {
		t.Fatalf("consume old redelivery: ok=%v err=%v", ok, err)
	}
	time.Sleep(250 * time.Millisecond)
	var heartbeatAfter time.Time
	if err := f.pool.QueryRow(context.Background(), `SELECT last_heartbeat_at FROM cloudforge.job_attempts WHERE attempt_id = $1`, *claimed.AttemptID).Scan(&heartbeatAfter); err != nil {
		t.Fatal(err)
	}
	if !heartbeatAfter.Equal(heartbeatBefore) {
		t.Fatalf("killed worker heartbeat advanced: %v -> %v", heartbeatBefore, heartbeatAfter)
	}
	store := jobs.NewStoreWithPolicy(f.pool, 900*time.Millisecond, 500*time.Millisecond, time.Second)
	recoveryCtx, stopRecovery := context.WithCancel(context.Background())
	recoveryDone := make(chan struct{})
	go func() { dispatcher.RunRecovery(recoveryCtx, store, 20*time.Millisecond, f.logger); close(recoveryDone) }()
	t.Cleanup(func() {
		stopRecovery()
		select {
		case <-recoveryDone:
		case <-time.After(2 * time.Second):
			t.Error("recovery loop did not stop")
		}
	})
	waiting := f.waitForStatus(t, accepted.ID, jobs.StatusRetryWait)
	if waiting.NextRetryAt == nil || waiting.AttemptCount != 1 {
		t.Fatalf("retry not scheduled: %+v", waiting)
	}
	queued := f.waitForStatus(t, accepted.ID, jobs.StatusQueued)
	if queued.AttemptCount != 1 {
		t.Fatalf("retry counted without claim: %+v", queued)
	}
	var outcome, decision string
	if err := f.pool.QueryRow(context.Background(), `SELECT outcome, retry_decision FROM cloudforge.job_attempts WHERE attempt_id = $1`, *claimed.AttemptID).Scan(&outcome, &decision); err != nil || outcome != "expired" || decision != "scheduled" {
		t.Fatalf("abandoned history=%s/%s err=%v", outcome, decision, err)
	}
	pending, err := f.outbox.ListUnpublished(context.Background(), 10)
	if err != nil || len(pending) != 1 || pending[0].AggregateID != accepted.ID {
		t.Fatalf("retry intent=%v err=%v", pending, err)
	}
	d := dispatcher.New(f.outbox, nil, time.Millisecond, time.Millisecond, time.Second, f.logger)
	if n, err := d.DispatchBatch(context.Background(), f.publisher); err != nil || n != 1 {
		t.Fatalf("retry publication=%d err=%v", n, err)
	}
	f.waitForReady(t, 1)
	stopB := filepath.Join(t.TempDir(), "stop-b.signal")
	b := startOwnedWorker(t, root, "v5-replacement-worker-b", "normal", "", stopB)
	completed := f.waitForStatus(t, accepted.ID, jobs.StatusSucceeded)
	if completed.AttemptCount != 2 || completed.AttemptID == nil || *completed.AttemptID == *claimed.AttemptID {
		t.Fatalf("replacement claim=%+v", completed)
	}
	var attempts, successes int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*), count(*) FILTER (WHERE outcome = 'succeeded') FROM cloudforge.job_attempts WHERE job_id = $1`, accepted.ID).Scan(&attempts, &successes); err != nil || attempts != 2 || successes != 1 {
		t.Fatalf("attempt history=%d/%d err=%v", attempts, successes, err)
	}
	if data := v4Download(t, server, accepted.ID, 200); !bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")) {
		t.Fatal("replacement PNG unavailable")
	}
	if mode == "output_then_block" {
		oldPath := filepath.Join(root, "outputs", accepted.ID, *claimed.AttemptID, "thumbnail.png")
		newPath := filepath.Join(root, "outputs", accepted.ID, *completed.AttemptID, "thumbnail.png")
		if _, err := os.Stat(oldPath); err != nil {
			t.Fatalf("old candidate unexpectedly removed: %v", err)
		}
		if _, err := os.Stat(newPath); err != nil {
			t.Fatalf("winning candidate missing: %v", err)
		}
	}
	b.stopGracefully(t, stopB)
}
