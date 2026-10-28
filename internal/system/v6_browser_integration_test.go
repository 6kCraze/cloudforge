//go:build integration

package system_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/6kCraze/cloudforge/internal/dispatcher"
	"github.com/6kCraze/cloudforge/internal/httpapi"
	"github.com/6kCraze/cloudforge/internal/images"
	"github.com/6kCraze/cloudforge/internal/jobs"
)

// An opt-in browser fixture, never an application mode or development-data
// fallback. Small deterministic faults make real persisted states inspectable.
func TestV6BrowserSession(t *testing.T) {
	if os.Getenv("CLOUDFORGE_BROWSER_DEMO") != "1" {
		t.Skip("opt-in isolated browser session")
	}
	stopFile := os.Getenv("CLOUDFORGE_BROWSER_STOP_FILE")
	if !filepath.IsAbs(stopFile) {
		t.Fatal("absolute temporary stop-file path required")
	}
	f := newV3Fixture(t)
	root := t.TempDir()
	storage, err := images.Open(filepath.Join(root, "storage"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	store := jobs.NewStoreWithPolicy(f.pool, 30*time.Second, 12*time.Second, 12*time.Second)
	listener, err := net.Listen("tcp", "127.0.0.1:18080")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(httpapi.NewWithImages(jobs.NewService(store), f.pool, f.logger, storage).Handler())
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)
	releaseFile := filepath.Join(root, "release-running.signal")
	processor := func(ctx context.Context, job jobs.Job) (json.RawMessage, error) {
		var name string
		if err := f.pool.QueryRow(ctx, `SELECT original_name FROM cloudforge.batch_images WHERE job_id=$1`, job.ID).Scan(&name); err != nil {
			return nil, err
		}
		switch name {
		case "running.png":
			ticker := time.NewTicker(50 * time.Millisecond)
			defer ticker.Stop()
			for {
				if _, err := os.Stat(releaseFile); err == nil {
					break
				}
				select {
				case <-ctx.Done():
					return nil, ctx.Err()
				case <-ticker.C:
				}
			}
		case "retry.png":
			if job.AttemptCount == 1 {
				return nil, errors.New("controlled browser-fixture recoverable failure")
			}
		case "failure.png":
			return nil, jobs.ErrPermanentJob
		}
		return storage.Execute(ctx, job)
	}
	f.startWorker(t, "v6-browser-worker", store, processor, func() {})
	t.Cleanup(func() { _ = os.WriteFile(releaseFile, []byte("release"), 0600) })
	f.waitForConsumers(t, 1)
	startV4Dispatcher(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		dispatcher.RunRecovery(ctx, store, 100*time.Millisecond, f.logger)
	}()
	t.Cleanup(func() { cancel(); <-done })
	for index, name := range []string{"ready.png", "running.png", "retry.png", "failure.png"} {
		if err := os.WriteFile(filepath.Join(root, name), v4Image(t, 120+index*20, 80+index*10, "png", 255), 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("BROWSER_URL=%s FIXTURES=%s RELEASE_RUNNING=%s", server.URL, root, releaseFile)
	deadline := time.Now().Add(20 * time.Minute)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(stopFile); err == nil {
			// Release the controlled processor before the fixture gracefully stops.
			_ = os.WriteFile(releaseFile, []byte("release"), 0600)
			t.Log("browser session stop requested; closing isolated runtime")
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	_ = os.WriteFile(releaseFile, []byte("release"), 0600)
	t.Fatal("browser session timed out")
}
