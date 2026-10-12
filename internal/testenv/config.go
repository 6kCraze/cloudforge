// Package testenv validates isolated local integration-test targets before any
// connection or fixture cleanup. Development configuration is never consulted.
package testenv

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/6kCraze/cloudforge/internal/config"
	"github.com/jackc/pgx/v5"
)

func Load() (config.Config, error) {
	databaseURL := strings.TrimSpace(os.Getenv("TEST_DATABASE_URL"))
	brokerURL := strings.TrimSpace(os.Getenv("TEST_RABBITMQ_URL"))
	if databaseURL == "" || brokerURL == "" {
		return config.Config{}, fmt.Errorf("TEST_DATABASE_URL and TEST_RABBITMQ_URL are required; no development fallback is allowed")
	}
	if err := Validate(databaseURL, brokerURL); err != nil {
		return config.Config{}, err
	}
	return config.Config{
		DatabaseURL: databaseURL, RabbitMQURL: brokerURL, AppEnv: "test",
		DispatcherConfirmTimeout: 5 * time.Second,
	}, nil
}

func Validate(databaseURL, brokerURL string) error {
	dbURI, err := url.Parse(databaseURL)
	if err != nil || dbURI.User == nil || (dbURI.Scheme != "postgres" && dbURI.Scheme != "postgresql") {
		return fmt.Errorf("TEST_DATABASE_URL must be a PostgreSQL URL")
	}
	if !localHost(dbURI.Hostname()) || dbURI.Path != "/cloudforge_test" || dbURI.User.Username() != "cloudforge_test" {
		return fmt.Errorf("unsafe TEST_DATABASE_URL: use local database cloudforge_test and role cloudforge_test")
	}
	// Disallow query overrides of host/database/user and service configuration.
	for key := range dbURI.Query() {
		if key != "sslmode" {
			return fmt.Errorf("unsafe TEST_DATABASE_URL query parameter %q", key)
		}
	}
	parsed, err := pgx.ParseConfig(databaseURL)
	if err != nil || parsed.Database != "cloudforge_test" || parsed.User != "cloudforge_test" || !localHost(parsed.Host) {
		return fmt.Errorf("unsafe resolved PostgreSQL test target")
	}
	amqpURI, err := url.Parse(brokerURL)
	if err != nil || amqpURI.User == nil || (amqpURI.Scheme != "amqp" && amqpURI.Scheme != "amqps") {
		return fmt.Errorf("TEST_RABBITMQ_URL must be an AMQP URL")
	}
	if !localHost(amqpURI.Hostname()) || amqpURI.Path != "/cloudforge_test" || amqpURI.User.Username() != "cloudforge_test" || amqpURI.RawQuery != "" {
		return fmt.Errorf("unsafe TEST_RABBITMQ_URL: use local vhost cloudforge_test and account cloudforge_test")
	}
	return nil
}

func localHost(host string) bool {
	switch host {
	case "localhost", "127.0.0.1", "::1", "host.docker.internal":
		return true
	default:
		return false
	}
}
