package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/totalwindupflightsystems/crier/config"
	"github.com/totalwindupflightsystems/crier/internal/mcp"
	"github.com/totalwindupflightsystems/crier/internal/registry"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	store, cleanup, err := initStore(cfg)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer cleanup()

	server := mcp.New(store)
	if err := server.Serve(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "server: %v\n", err)
		os.Exit(1)
	}
}

// initStore creates a Store backend: PostgreSQL if CR_DATABASE_URL is set, otherwise in-memory.
func initStore(cfg config.Config) (registry.Store, func(), error) {
	if cfg.Database.URL != "" {
		ctx, cancel := context.WithTimeout(context.Background(), cfg.Database.ConnectTimeout)
		defer cancel()

		pgStore, err := registry.NewPostgresStoreWithPoolConfig(ctx, cfg.Database.URL, registry.PoolConfig{
			MaxConns:        cfg.Database.MaxConns,
			MinConns:        cfg.Database.MinConns,
			MaxConnLifetime: cfg.Database.MaxConnLifetime,
			MaxConnIdleTime: cfg.Database.MaxConnIdleTime,
		})
		if err != nil {
			return nil, nil, fmt.Errorf("postgres: %w", err)
		}
		return pgStore, func() { pgStore.Close() }, nil
	}
	return registry.NewMemoryStore(), func() {}, nil
}
