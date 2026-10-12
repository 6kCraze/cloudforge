//go:build integration

package database

import (
	"context"
	"testing"
	"time"

	"github.com/6kCraze/cloudforge/internal/testenv"
)

func TestPostgresConnectionAndMigration(t *testing.T) {
	cfg, err := testenv.Load()
	if err != nil {
		t.Fatalf("load configuration: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pool, err := Open(ctx, cfg.DatabaseURL)
	if err != nil {
		t.Fatalf("open PostgreSQL: %v", err)
	}
	defer pool.Close()

	var exists bool
	err = pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = 'cloudforge')").Scan(&exists)
	if err != nil {
		t.Fatalf("check cloudforge schema: %v", err)
	}
	if !exists {
		t.Fatal("cloudforge schema is missing; run migrations first")
	}
}
