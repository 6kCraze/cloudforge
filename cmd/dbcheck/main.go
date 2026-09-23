package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/6kCraze/cloudforge/internal/config"
	"github.com/6kCraze/cloudforge/internal/database"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pool, err := database.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()

	fmt.Printf("PostgreSQL connection OK (APP_ENV=%s)\n", cfg.AppEnv)
}
