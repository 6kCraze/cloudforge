package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	DatabaseURL              string
	StorageRoot              string
	RabbitMQURL              string
	AppEnv                   string
	HTTPPort                 int
	WorkerID                 string
	WorkerBackoffMin         time.Duration
	WorkerBackoffMax         time.Duration
	WorkerLeaseDuration      time.Duration
	WorkerHeartbeatInterval  time.Duration
	RetryBaseDelay           time.Duration
	RetryMaxDelay            time.Duration
	RecoveryPollInterval     time.Duration
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
	storageRoot := strings.TrimSpace(os.Getenv("STORAGE_ROOT"))
	if storageRoot == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			return Config{}, fmt.Errorf("default storage root: %w", err)
		}
		storageRoot = filepath.Join(cache, "CloudForge", "storage")
	}
	if !filepath.IsAbs(storageRoot) {
		return Config{}, errors.New("STORAGE_ROOT must be an absolute path")
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

	workerBackoffMin, err := durationEnv("WORKER_BACKOFF_MIN", 250*time.Millisecond, 10*time.Millisecond)
	if err != nil {
		return Config{}, err
	}
	workerBackoffMax, err := durationEnv("WORKER_BACKOFF_MAX", 5*time.Second, workerBackoffMin)
	if err != nil {
		return Config{}, err
	}
	workerLeaseDuration, err := durationEnv("WORKER_LEASE_DURATION", 30*time.Second, 3*time.Second)
	if err != nil {
		return Config{}, err
	}
	workerHeartbeatInterval, err := durationEnv("WORKER_HEARTBEAT_INTERVAL", 5*time.Second, 100*time.Millisecond)
	if err != nil {
		return Config{}, err
	}
	if workerLeaseDuration > 10*time.Minute || workerHeartbeatInterval >= workerLeaseDuration/3 {
		return Config{}, errors.New("worker lease must be at most 10m and heartbeat interval must be less than one third of the lease")
	}
	retryBaseDelay, err := durationEnv("RETRY_BASE_DELAY", time.Second, 100*time.Millisecond)
	if err != nil {
		return Config{}, err
	}
	retryMaxDelay, err := durationEnv("RETRY_MAX_DELAY", 4*time.Second, retryBaseDelay)
	if err != nil {
		return Config{}, err
	}
	if retryMaxDelay > time.Minute {
		return Config{}, errors.New("RETRY_MAX_DELAY must be at most 1m")
	}
	recoveryPollInterval, err := durationEnv("RECOVERY_POLL_INTERVAL", 500*time.Millisecond, 100*time.Millisecond)
	if err != nil {
		return Config{}, err
	}
	if recoveryPollInterval > 30*time.Second {
		return Config{}, errors.New("RECOVERY_POLL_INTERVAL must be at most 30s")
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
		StorageRoot:              filepath.Clean(storageRoot),
		RabbitMQURL:              strings.TrimSpace(os.Getenv("RABBITMQ_URL")),
		AppEnv:                   appEnv,
		HTTPPort:                 httpPort,
		WorkerID:                 strings.TrimSpace(os.Getenv("WORKER_ID")),
		WorkerBackoffMin:         workerBackoffMin,
		WorkerBackoffMax:         workerBackoffMax,
		WorkerLeaseDuration:      workerLeaseDuration,
		WorkerHeartbeatInterval:  workerHeartbeatInterval,
		RetryBaseDelay:           retryBaseDelay,
		RetryMaxDelay:            retryMaxDelay,
		RecoveryPollInterval:     recoveryPollInterval,
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
