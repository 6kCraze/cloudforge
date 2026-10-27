package config

import (
	"path/filepath"
	"testing"
	"time"
)

func TestLoadRequiresDatabaseURL(t *testing.T) {
	t.Setenv("DATABASE_URL", "")

	if _, err := Load(); err == nil {
		t.Fatal("expected a missing DATABASE_URL error")
	}
}

func TestStorageRootMustBeAbsoluteAndShared(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://example")
	root := t.TempDir()
	t.Setenv("STORAGE_ROOT", root)
	api, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	worker, err := Load()
	if err != nil || api.StorageRoot != worker.StorageRoot || api.StorageRoot != filepath.Clean(root) {
		t.Fatalf("storage root mismatch: %v", err)
	}
	t.Setenv("STORAGE_ROOT", "relative/storage")
	if _, err := Load(); err == nil {
		t.Fatal("relative STORAGE_ROOT accepted")
	}
	t.Setenv("STORAGE_ROOT", "")
	defaults, err := Load()
	if err != nil || !filepath.IsAbs(defaults.StorageRoot) {
		t.Fatalf("default root must be absolute: %v", err)
	}
}

func TestLoadDefaultsToDevelopment(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://example")
	t.Setenv("APP_ENV", "")
	t.Setenv("HTTP_PORT", "")
	t.Setenv("WORKER_BACKOFF_MIN", "")
	t.Setenv("WORKER_BACKOFF_MAX", "")
	t.Setenv("DISPATCHER_POLL_INTERVAL", "")
	t.Setenv("DISPATCHER_CONFIRM_TIMEOUT", "")
	t.Setenv("DISPATCHER_BACKOFF_MIN", "")
	t.Setenv("DISPATCHER_BACKOFF_MAX", "")

	got, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got.AppEnv != "development" {
		t.Fatalf("AppEnv = %q, want development", got.AppEnv)
	}
	if got.HTTPPort != 8080 {
		t.Fatalf("HTTPPort = %d, want 8080", got.HTTPPort)
	}
	if got.WorkerBackoffMin != 250*time.Millisecond || got.WorkerBackoffMax != 5*time.Second {
		t.Fatalf("worker backoff defaults = %s/%s", got.WorkerBackoffMin, got.WorkerBackoffMax)
	}
	if got.DispatcherPollInterval != 500*time.Millisecond || got.DispatcherConfirmTimeout != 5*time.Second {
		t.Fatalf("dispatcher defaults = %s/%s", got.DispatcherPollInterval, got.DispatcherConfirmTimeout)
	}
}

func TestLoadRejectsWorkerBackoffMaxBelowMin(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://example")
	t.Setenv("WORKER_BACKOFF_MIN", "2s")
	t.Setenv("WORKER_BACKOFF_MAX", "1s")

	if _, err := Load(); err == nil {
		t.Fatal("expected invalid worker backoff error")
	}
}

func TestLoadRejectsDispatcherBackoffMaxBelowMin(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://example")
	t.Setenv("DISPATCHER_BACKOFF_MIN", "2s")
	t.Setenv("DISPATCHER_BACKOFF_MAX", "1s")

	if _, err := Load(); err == nil {
		t.Fatal("expected invalid dispatcher backoff error")
	}
}

func TestWorkerLeaseAndHeartbeatBounds(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://example")
	t.Setenv("WORKER_LEASE_DURATION", "30s")
	t.Setenv("WORKER_HEARTBEAT_INTERVAL", "5s")
	if cfg, err := Load(); err != nil || cfg.WorkerLeaseDuration != 30*time.Second || cfg.WorkerHeartbeatInterval != 5*time.Second {
		t.Fatalf("config=%+v err=%v", cfg, err)
	}
	t.Setenv("WORKER_HEARTBEAT_INTERVAL", "10s")
	if _, err := Load(); err == nil {
		t.Fatal("heartbeat at one third of lease accepted")
	}
	t.Setenv("WORKER_HEARTBEAT_INTERVAL", "5s")
	t.Setenv("WORKER_LEASE_DURATION", "11m")
	if _, err := Load(); err == nil {
		t.Fatal("unbounded lease accepted")
	}
}

func TestRetryAndRecoveryBounds(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://example")
	t.Setenv("RETRY_BASE_DELAY", "1s")
	t.Setenv("RETRY_MAX_DELAY", "4s")
	t.Setenv("RECOVERY_POLL_INTERVAL", "500ms")
	cfg, err := Load()
	if err != nil || cfg.RetryBaseDelay != time.Second || cfg.RetryMaxDelay != 4*time.Second || cfg.RecoveryPollInterval != 500*time.Millisecond {
		t.Fatalf("config=%+v err=%v", cfg, err)
	}
	t.Setenv("RETRY_MAX_DELAY", "500ms")
	if _, err := Load(); err == nil {
		t.Fatal("retry maximum below base accepted")
	}
	t.Setenv("RETRY_MAX_DELAY", "4s")
	t.Setenv("RECOVERY_POLL_INTERVAL", "31s")
	if _, err := Load(); err == nil {
		t.Fatal("unbounded recovery interval accepted")
	}
}
