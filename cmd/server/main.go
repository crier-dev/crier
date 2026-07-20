package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gorilla/mux"
	"github.com/totalwindupflightsystems/crier/config"
	"github.com/totalwindupflightsystems/crier/internal/mesh"
	"github.com/totalwindupflightsystems/crier/internal/middleware"
	"github.com/totalwindupflightsystems/crier/internal/registry"
	"github.com/totalwindupflightsystems/crier/internal/relay"
)

func main() {
	// Bootstrap with a default info-level text logger so any pre-config
	// logging has a destination. Re-initialized with the user's choices
	// after cfg.Load() below.
	initLogger(config.Config{LogLevel: "info", LogFormat: "text"})

	cfg, err := config.Load()
	if err != nil {
		slog.Error("load configuration", "error", err)
		os.Exit(1)
	}

	// Re-install the logger now that we know the user's level/format.
	initLogger(cfg)

	// Configure WebSocket origin check for relay and mesh.
	wsCheck := config.BuildCheckOrigin(cfg.WSAllowedOrigins)
	relay.SetWSCheckOrigin(wsCheck)
	mesh.SetWSCheckOrigin(wsCheck)

	r := mux.NewRouter()

	// Middleware
	r.Use(middleware.Auth(cfg.AuthToken))
	r.Use(middleware.Recovery)
	r.Use(middleware.Logging)

	// Health check
	r.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))
	}).Methods("GET")

	// Relay pub/sub
	relaySvc := relay.New(cfg.RateLimitPerMinute)
	r.HandleFunc("/relay/publish", relaySvc.HandlePublish).Methods("POST")
	r.HandleFunc("/relay/subscribe/{topic}", relaySvc.HandleSubscribe)
	r.HandleFunc("/relay/topics", relaySvc.HandleTopics).Methods("GET")

	// P2P mesh
	meshCfg := mesh.DefaultMeshConfig("crier")
	meshSvc := mesh.NewMesh(meshCfg)
	r.HandleFunc("/mesh/connect/{agentID}", mesh.HandleConnect(meshSvc))
	r.HandleFunc("/mesh/peers", mesh.HandlePeers(meshSvc)).Methods("GET")

	// Agent registry + inboxes
	var regStore registry.Store

	if cfg.Database.URL != "" {
		startupCtx, cancelStartup := context.WithTimeout(context.Background(), cfg.Database.ConnectTimeout)
		defer cancelStartup()

		pgStore, err := registry.NewPostgresStoreWithPoolConfig(startupCtx, cfg.Database.URL, registry.PoolConfig{
			MaxConns:        cfg.Database.MaxConns,
			MinConns:        cfg.Database.MinConns,
			MaxConnLifetime: cfg.Database.MaxConnLifetime,
			MaxConnIdleTime: cfg.Database.MaxConnIdleTime,
		})
		if err != nil {
			slog.Error("initialize PostgreSQL registry store", "error", err)
			os.Exit(1)
		}
		defer pgStore.Close()
		regStore = pgStore
		slog.Info("registry backend", "type", "postgres", "max_conns", cfg.Database.MaxConns)
	} else {
		regStore = registry.NewMemoryStore()
		slog.Info("registry backend", "type", "memory", "hint", "set CR_DATABASE_URL for PostgreSQL")
	}

	registryHandler := registry.NewHandler(regStore)
	r.HandleFunc("/agents", registryHandler.HandleRegister).Methods("POST")
	r.HandleFunc("/agents", registryHandler.HandleListAgents).Methods("GET")
	r.HandleFunc("/agents/{id}", registryHandler.HandleGetAgent).Methods("GET")
	r.HandleFunc("/agents/{id}", registryHandler.HandleUnregister).Methods("DELETE")
	r.HandleFunc("/agents/{id}/inbox", registryHandler.HandleDeliver).Methods("POST")
	r.HandleFunc("/agents/{id}/inbox", registryHandler.HandleRetrieve).Methods("GET")
	r.HandleFunc("/agents/{id}/inbox/ack", registryHandler.HandleAck).Methods("POST")
	r.HandleFunc("/agents/{id}/inbox/stats", registryHandler.HandleStats).Methods("GET")

	srv := &http.Server{
		Addr:         fmt.Sprintf(":%d", cfg.Port),
		Handler:      r,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 0,
		IdleTimeout:  60 * time.Second,
	}

	// Periodic expired message purging
	purgeCtx, purgeCancel := context.WithCancel(context.Background())
	defer purgeCancel()
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-purgeCtx.Done():
				return
			case <-ticker.C:
				regStore.PurgeExpired()
			}
		}
	}()

	// Graceful shutdown
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		purgeCancel()
		meshSvc.Stop()
		if err := srv.Shutdown(ctx); err != nil {
			slog.Warn("http shutdown", "error", err)
		}
		if closer, ok := regStore.(interface{ Close() }); ok {
			closer.Close()
		}
	}()

	slog.Info("crier starting", "port", cfg.Port, "services", "relay+mesh+registry")
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("server failed", "error", err)
		os.Exit(1)
	}
}
