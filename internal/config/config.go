package config

import (
	"fmt"
	"os"

	"github.com/joho/godotenv"
)

type Config struct {
	Env         string
	DatabaseURL string
	HTTPAddr    string
}

func Load() (*Config, error) {
	_ = godotenv.Load()

	cfg := &Config{
		Env:         getEnv("ENV", "development"),
		DatabaseURL: getEnv("DATABASE_URL", ""),
		HTTPAddr:    getEnv("HTTP_ADDR", ":8080"),
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) validate() error {
	required := map[string]string{
		"DATABASE_URL": c.DatabaseURL,
	}
	for name, val := range required {
		if val == "" {
			return fmt.Errorf("missing required env var: %s", name)
		}
	}
	return nil
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
