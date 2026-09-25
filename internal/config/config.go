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
	DatabaseURL              string
	RabbitMQURL              string
	AppEnv                   string
	HTTPPort                 int
	WorkerID                 string
	WorkerPollInterval       time.Duration
	DispatcherPollInterval   time.Duration
	DispatcherConfirmTimeout time.Duration
	DispatcherBackoffMin     time.Duration
	DispatcherBackoffMax     time.Duration
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

	dispatcherPollInterval, err := durationEnv("DISPATCHER_POLL_INTERVAL", 500*time.Millisecond, 10*time.Millisecond)
	if err != nil {
		return Config{}, err
	}
	dispatcherConfirmTimeout, err := durationEnv("DISPATCHER_CONFIRM_TIMEOUT", 5*time.Second, 100*time.Millisecond)
	if err != nil {
		return Config{}, err
	}
	dispatcherBackoffMin, err := durationEnv("DISPATCHER_BACKOFF_MIN", 250*time.Millisecond, 10*time.Millisecond)
	if err != nil {
		return Config{}, err
	}
	dispatcherBackoffMax, err := durationEnv("DISPATCHER_BACKOFF_MAX", 5*time.Second, dispatcherBackoffMin)
	if err != nil {
		return Config{}, err
	}

	return Config{
		DatabaseURL:              databaseURL,
		RabbitMQURL:              strings.TrimSpace(os.Getenv("RABBITMQ_URL")),
		AppEnv:                   appEnv,
		HTTPPort:                 httpPort,
		WorkerID:                 strings.TrimSpace(os.Getenv("WORKER_ID")),
		WorkerPollInterval:       pollInterval,
		DispatcherPollInterval:   dispatcherPollInterval,
		DispatcherConfirmTimeout: dispatcherConfirmTimeout,
		DispatcherBackoffMin:     dispatcherBackoffMin,
		DispatcherBackoffMax:     dispatcherBackoffMax,
	}, nil
}

func durationEnv(name string, fallback, minimum time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil || parsed < minimum {
		return 0, fmt.Errorf("%s must be a duration of at least %s", name, minimum)
	}
	return parsed, nil
}
