package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/6kCraze/cloudforge/internal/config"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
)

func main() {
	if len(os.Args) != 2 {
		log.Fatal("usage: go run ./cmd/migrate [up|down|status]")
	}

	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}
	if err := goose.SetDialect("postgres"); err != nil {
		log.Fatal(err)
	}

	db, err := sql.Open("pgx", cfg.DatabaseURL)
	if err != nil {
		log.Fatal(fmt.Errorf("open PostgreSQL for migrations: %w", err))
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		log.Fatal(fmt.Errorf("ping PostgreSQL for migrations: %w", err))
	}

	switch os.Args[1] {
	case "up":
		err = goose.Up(db, "migrations")
	case "down":
		err = goose.Down(db, "migrations")
	case "status":
		err = goose.Status(db, "migrations")
	default:
		log.Fatal("usage: go run ./cmd/migrate [up|down|status]")
	}
	if err != nil {
		log.Fatal(err)
	}
}
