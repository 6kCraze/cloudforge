package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	DatabaseURL        string
	AppEnv             string
	HTTPPort           int
	WorkerID           string
	WorkerPollInterval time.Duration
}

func Load() (Config, error) {
	databaseURL := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if databaseURL == "" {
		return Config{}, errors.New("DATABASE_URL is required")
	}

	appEnv := strings.TrimSpace(os.Getenv("APP_ENV"))
	if appEnv == "" {
		appEnv = "development"
	}

	httpPort := 8080
	if raw := strings.TrimSpace(os.Getenv("HTTP_PORT")); raw != "" {
		port, err := strconv.Atoi(raw)
		if err != nil || port < 1 || port > 65535 {
			return Config{}, fmt.Errorf("HTTP_PORT must be an integer between 1 and 65535")
		}
		httpPort = port
	}

	pollInterval := 500 * time.Millisecond
	if raw := strings.TrimSpace(os.Getenv("WORKER_POLL_INTERVAL")); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil || parsed < 10*time.Millisecond {
			return Config{}, fmt.Errorf("WORKER_POLL_INTERVAL must be a duration of at least 10ms")
		}
		pollInterval = parsed
	}

	return Config{
		DatabaseURL:        databaseURL,
		AppEnv:             appEnv,
		HTTPPort:           httpPort,
		WorkerID:           strings.TrimSpace(os.Getenv("WORKER_ID")),
		WorkerPollInterval: pollInterval,
	}, nil
}
