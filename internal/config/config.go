package config

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/joho/godotenv"
)

type ParserKind string

const (
	ParserRegex  ParserKind = "regex"
	ParserClaude ParserKind = "claude"
)

type Config struct {
	Env              string
	DatabaseURL      string
	HTTPAddr         string
	TelegramBotToken string

	Parser          ParserKind
	AnthropicAPIKey string
	Location        *time.Location

	// Memory layer
	VerbatimTurns     int           // CHAT_VERBATIM_TURNS: recent turns kept verbatim; older ones are summarized
	SummarizeInterval time.Duration // SUMMARIZE_INTERVAL: how often the summarize loop runs

	// Proactive messaging
	ProactiveInterval time.Duration // PROACTIVE_INTERVAL: min gap between proactive messages per user
}

func Load() (*Config, error) {
	_ = godotenv.Load()

	cfg := &Config{
		Env:              getEnv("ENV", "development"),
		DatabaseURL:      getEnv("DATABASE_URL", ""),
		HTTPAddr:         getEnv("HTTP_ADDR", ":8080"),
		TelegramBotToken: getEnv("TELEGRAM_BOT_TOKEN", ""),
		Parser:           ParserKind(getEnv("PARSER", "regex")),
		AnthropicAPIKey:  getEnv("ANTHROPIC_API_KEY", ""),
	}

	tzName := getEnv("DEFAULT_TIMEZONE", "UTC")
	loc, err := time.LoadLocation(tzName)
	if err != nil {
		return nil, fmt.Errorf("load timezone %q: %w", tzName, err)
	}
	cfg.Location = loc

	verbatimTurns, err := parseInt(getEnv("CHAT_VERBATIM_TURNS", "20"))
	if err != nil {
		return nil, fmt.Errorf("CHAT_VERBATIM_TURNS: %w", err)
	}
	cfg.VerbatimTurns = verbatimTurns

	summarizeInterval, err := time.ParseDuration(getEnv("SUMMARIZE_INTERVAL", "5m"))
	if err != nil {
		return nil, fmt.Errorf("SUMMARIZE_INTERVAL: %w", err)
	}
	cfg.SummarizeInterval = summarizeInterval

	proactiveInterval, err := time.ParseDuration(getEnv("PROACTIVE_INTERVAL", "24h"))
	if err != nil {
		return nil, fmt.Errorf("PROACTIVE_INTERVAL: %w", err)
	}
	cfg.ProactiveInterval = proactiveInterval

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) validate() error {
	if c.DatabaseURL == "" {
		return fmt.Errorf("missing required env var: DATABASE_URL")
	}
	if c.TelegramBotToken == "" {
		return fmt.Errorf("missing required env var: TELEGRAM_BOT_TOKEN")
	}

	switch c.Parser {
	case ParserRegex:
		// nothing extra required
	case ParserClaude:
		if c.AnthropicAPIKey == "" {
			return fmt.Errorf("PARSER=claude requires ANTHROPIC_API_KEY")
		}
	default:
		return fmt.Errorf("invalid PARSER %q (want %q or %q)",
			c.Parser, ParserRegex, ParserClaude)
	}

	if c.VerbatimTurns <= 0 {
		return fmt.Errorf("CHAT_VERBATIM_TURNS must be > 0, got %d", c.VerbatimTurns)
	}
	if c.SummarizeInterval < time.Minute {
		return fmt.Errorf("SUMMARIZE_INTERVAL must be >= 1m to avoid hammering the API, got %s", c.SummarizeInterval)
	}
	if c.ProactiveInterval < time.Hour {
		return fmt.Errorf("PROACTIVE_INTERVAL must be >= 1h to avoid spamming users, got %s", c.ProactiveInterval)
	}

	return nil
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func parseInt(s string) (int, error) {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("parse int %q: %w", s, err)
	}
	return n, nil
}
