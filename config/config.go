package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// DatabaseConfig holds PostgreSQL connection and pool settings.
type DatabaseConfig struct {
	URL             string
	MaxConns        int32
	MinConns        int32
	MaxConnLifetime time.Duration
	MaxConnIdleTime time.Duration
	ConnectTimeout  time.Duration
}

// Config holds all Crier configuration.
type Config struct {
	Port               int
	AuthToken          string
	Database           DatabaseConfig
	LogLevel           string
	LogFormat          string
	RateLimitPerMinute int
}

// Load reads configuration from environment with defaults.
func Load() (Config, error) {
	cfg := Config{
		Port:               8767,
		AuthToken:          os.Getenv("CR_AUTH_TOKEN"),
		LogLevel:           "info",
		LogFormat:          "text",
		RateLimitPerMinute: 100,
		Database: DatabaseConfig{
			MaxConns:        4,
			MinConns:        0,
			MaxConnLifetime: 30 * time.Minute,
			MaxConnIdleTime: 5 * time.Minute,
			ConnectTimeout:  10 * time.Second,
		},
	}

	if v := os.Getenv("CR_LOG_LEVEL"); v != "" {
		switch v {
		case "debug", "info", "warn", "error":
			cfg.LogLevel = v
		default:
			return cfg, fmt.Errorf("invalid CR_LOG_LEVEL: %q (want debug/info/warn/error)", v)
		}
	}

	if v := os.Getenv("CR_LOG_FORMAT"); v != "" {
		switch v {
		case "text", "json":
			cfg.LogFormat = v
		default:
			return cfg, fmt.Errorf("invalid CR_LOG_FORMAT: %q (want text/json)", v)
		}
	}

	// Port
	if v := os.Getenv("CRIER_PORT"); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil || p < 1 || p > 65535 {
			return cfg, fmt.Errorf("invalid CRIER_PORT: %q", v)
		}
		cfg.Port = p
	}

	// Database URL — three env var precedence: CR_DATABASE_URL → DATABASE_URL → CRIER_DATABASE_URL
	dbURL := os.Getenv("CR_DATABASE_URL")
	if dbURL == "" {
		dbURL = os.Getenv("DATABASE_URL")
	}
	if dbURL == "" {
		dbURL = os.Getenv("CRIER_DATABASE_URL")
	}
	cfg.Database.URL = dbURL

	// Pool settings
	if v := os.Getenv("CR_DATABASE_MAX_CONNS"); v != "" {
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil || n <= 0 {
			return cfg, fmt.Errorf("invalid CR_DATABASE_MAX_CONNS: %q", v)
		}
		cfg.Database.MaxConns = int32(n)
	}
	if v := os.Getenv("CR_DATABASE_MIN_CONNS"); v != "" {
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil || n < 0 {
			return cfg, fmt.Errorf("invalid CR_DATABASE_MIN_CONNS: %q", v)
		}
		cfg.Database.MinConns = int32(n)
	}
	if cfg.Database.MinConns > cfg.Database.MaxConns {
		return cfg, fmt.Errorf("CR_DATABASE_MIN_CONNS (%d) > CR_DATABASE_MAX_CONNS (%d)", cfg.Database.MinConns, cfg.Database.MaxConns)
	}

	if v := os.Getenv("CR_DATABASE_MAX_CONN_LIFETIME"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return cfg, fmt.Errorf("invalid CR_DATABASE_MAX_CONN_LIFETIME: %q", v)
		}
		cfg.Database.MaxConnLifetime = d
	}
	if v := os.Getenv("CR_DATABASE_MAX_CONN_IDLE_TIME"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return cfg, fmt.Errorf("invalid CR_DATABASE_MAX_CONN_IDLE_TIME: %q", v)
		}
		cfg.Database.MaxConnIdleTime = d
	}
	if v := os.Getenv("CR_DATABASE_CONNECT_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return cfg, fmt.Errorf("invalid CR_DATABASE_CONNECT_TIMEOUT: %q", v)
		}
		cfg.Database.ConnectTimeout = d
	}

	// Rate limit
	if v := os.Getenv("CR_RATE_LIMIT_PER_MINUTE"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return cfg, fmt.Errorf("invalid CR_RATE_LIMIT_PER_MINUTE: %q (want non-negative integer)", v)
		}
		cfg.RateLimitPerMinute = n
	}

	return cfg, nil
}
