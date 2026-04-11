package config

import (
	"fmt"
	"os"
)

type Config struct {
	Env         string
	DatabaseURL string
	HTTPAddr    string

	AnthropicAPIKey string

	TwilioAccountSID string
	TwilioAuthToken  string
	TwilioFromNumber string

	PostmarkServerToken string
	PostmarkFromEmail   string
}

func Load() (*Config, error) {
	cfg := &Config{
		Env:                 getEnv("ENV", "development"),
		DatabaseURL:         os.Getenv("DATABASE_URL"),
		HTTPAddr:            getEnv("HTTP_ADDR", ":8080"),
		AnthropicAPIKey:     os.Getenv("ANTHROPIC_API_KEY"),
		TwilioAccountSID:    os.Getenv("TWILIO_ACCOUNT_SID"),
		TwilioAuthToken:     os.Getenv("TWILIO_AUTH_TOKEN"),
		TwilioFromNumber:    os.Getenv("TWILIO_FROM_NUMBER"),
		PostmarkServerToken: os.Getenv("POSTMARK_SERVER_TOKEN"),
		PostmarkFromEmail:   os.Getenv("POSTMARK_FROM_EMAIL"),
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) validate() error {
	required := map[string]string{
		// "DATABASE_URL":      c.DatabaseURL,
		// "ANTHROPIC_API_KEY": c.AnthropicAPIKey,
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
