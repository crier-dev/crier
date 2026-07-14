package main

import (
	"context"
	"fmt"
	"log"
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
	cfg := config.Load()

	r := mux.NewRouter()

	// Middleware
	r.Use(middleware.Recovery)
	r.Use(middleware.Logging)

	// Health check
	r.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))
	}).Methods("GET")

	// Relay pub/sub
	relaySvc := relay.New()
	r.HandleFunc("/relay/publish", relaySvc.HandlePublish).Methods("POST")
	r.HandleFunc("/relay/subscribe/{topic}", relaySvc.HandleSubscribe)
	r.HandleFunc("/relay/topics", relaySvc.HandleTopics).Methods("GET")

	// P2P mesh
	meshCfg := mesh.DefaultMeshConfig("crier")
	meshSvc := mesh.NewMesh(meshCfg)
	r.HandleFunc("/mesh/connect/{agentID}", mesh.HandleConnect(meshSvc))
	r.HandleFunc("/mesh/peers", mesh.HandlePeers(meshSvc)).Methods("GET")

	// Agent registry + inboxes
	regStore := registry.NewStore()
	r.HandleFunc("/agents", regStore.HandleRegister).Methods("POST")
	r.HandleFunc("/agents", regStore.HandleListAgents).Methods("GET")
	r.HandleFunc("/agents/{id}", regStore.HandleGetAgent).Methods("GET")
	r.HandleFunc("/agents/{id}", regStore.HandleUnregister).Methods("DELETE")
	r.HandleFunc("/agents/{id}/inbox", regStore.HandleDeliver).Methods("POST")
	r.HandleFunc("/agents/{id}/inbox", regStore.HandleRetrieve).Methods("GET")
	r.HandleFunc("/agents/{id}/inbox/ack", regStore.HandleAck).Methods("POST")
	r.HandleFunc("/agents/{id}/inbox/stats", regStore.HandleStats).Methods("GET")

	srv := &http.Server{
		Addr:         fmt.Sprintf(":%d", cfg.Port),
		Handler:      r,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 0, // Required for WebSocket connections.
		IdleTimeout:  60 * time.Second,
	}

	// Graceful shutdown
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		meshSvc.Stop()
		srv.Shutdown(ctx)
	}()

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

	log.Printf("Crier starting on :%d (relay + mesh + registry)", cfg.Port)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("server failed: %v", err)
	}
}
