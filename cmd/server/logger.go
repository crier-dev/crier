package main

import (
	"log/slog"
	"os"
	"strings"

	"github.com/totalwindupflightsystems/crier/config"
)

// initLogger installs the process-wide slog default logger based on cfg.
// It must be called once during startup; main() re-invokes it after cfg.Load()
// so the chosen level/format take effect.
//
//	CR_LOG_LEVEL  → debug | info | warn | error
//	CR_LOG_FORMAT → text | json
func initLogger(cfg config.Config) {
	level := parseLogLevel(cfg.LogLevel)

	opts := &slog.HandlerOptions{
		Level:     level,
		AddSource: level >= slog.LevelWarn,
	}

	var handler slog.Handler
	switch strings.ToLower(cfg.LogFormat) {
	case "json":
		handler = slog.NewJSONHandler(os.Stderr, opts)
	default:
		handler = slog.NewTextHandler(os.Stderr, opts)
	}

	slog.SetDefault(slog.New(handler))
}

// parseLogLevel maps the configured level string to slog.Level.
// Unknown values fall back to slog.LevelInfo — initLogger is best-effort
// and should never crash startup.
func parseLogLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
