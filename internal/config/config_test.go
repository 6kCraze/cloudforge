package config

import "testing"

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
}
