package config

import (
	"errors"
	"os"
	"strings"
)

type Config struct {
	DatabaseURL string
	AppEnv      string
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

	return Config{DatabaseURL: databaseURL, AppEnv: appEnv}, nil
}
