package testenv

import "testing"

const safeDatabase = "postgres://cloudforge_test:test@localhost:5432/cloudforge_test?sslmode=disable"
const safeBroker = "amqp://cloudforge_test:test@localhost:5672/cloudforge_test"

func TestLoadNeverFallsBackToDevelopment(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://cloudforge:dev@localhost/cloudforge")
	t.Setenv("RABBITMQ_URL", "amqp://cloudforge:dev@localhost/")
	t.Setenv("TEST_DATABASE_URL", "")
	t.Setenv("TEST_RABBITMQ_URL", "")
	if _, err := Load(); err == nil {
		t.Fatal("missing test configuration must fail before connecting")
	}
}

func TestValidateRejectsUnsafeTargets(t *testing.T) {
	for _, tc := range []struct{ name, db, broker string }{
		{"development database", "postgres://cloudforge_test:test@localhost/cloudforge", safeBroker},
		{"development role", "postgres://cloudforge:test@localhost/cloudforge_test", safeBroker},
		{"remote database", "postgres://cloudforge_test:test@example.com/cloudforge_test", safeBroker},
		{"database override", safeDatabase + "&dbname=cloudforge", safeBroker},
		{"development vhost", safeDatabase, "amqp://cloudforge_test:test@localhost/"},
		{"development broker account", safeDatabase, "amqp://cloudforge:test@localhost/cloudforge_test"},
		{"remote broker", safeDatabase, "amqp://cloudforge_test:test@example.com/cloudforge_test"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := Validate(tc.db, tc.broker); err == nil {
				t.Fatal("unsafe target accepted")
			}
		})
	}
}

func TestLoadAcceptsOnlyExplicitIsolatedConfiguration(t *testing.T) {
	t.Setenv("TEST_DATABASE_URL", safeDatabase)
	t.Setenv("TEST_RABBITMQ_URL", safeBroker)
	cfg, err := Load()
	if err != nil || cfg.DatabaseURL != safeDatabase || cfg.RabbitMQURL != safeBroker {
		t.Fatalf("isolated configuration rejected: %v", err)
	}
}
