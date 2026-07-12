package config

import "os"

// Config holds all Crier configuration.
type Config struct {
	Port int
}

// Load reads configuration from environment with defaults.
func Load() Config {
	port := 8767
	return Config{Port: port}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
