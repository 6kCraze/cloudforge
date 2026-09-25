package config

import (
	"testing"
	"time"
)

func TestLoadRequiresDatabaseURL(t *testing.T) {
	t.Setenv("DATABASE_URL", "")

	if _, err := Load(); err == nil {
		t.Fatal("expected a missing DATABASE_URL error")
	}
}

func TestLoadDefaultsToDevelopment(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://example")
	t.Setenv("APP_ENV", "")
	t.Setenv("HTTP_PORT", "")
	t.Setenv("WORKER_POLL_INTERVAL", "")
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
	if got.DispatcherPollInterval != 500*time.Millisecond || got.DispatcherConfirmTimeout != 5*time.Second {
		t.Fatalf("dispatcher defaults = %s/%s", got.DispatcherPollInterval, got.DispatcherConfirmTimeout)
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
