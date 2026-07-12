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
	"github.com/totalwindupflightsystems/crier/internal/relay"
)

func main() {
	cfg := config.Load()

	r := mux.NewRouter()

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

	srv := &http.Server{
		Addr:         fmt.Sprintf(":%d", cfg.Port),
		Handler:      r,
		ReadTimeout:  15 * time.Second,
		// WriteTimeout must be 0 for long-lived WebSocket subscriptions.
		WriteTimeout: 0,
		IdleTimeout:  60 * time.Second,
	}

	// Graceful shutdown
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		<-sigCh

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	}()

	log.Printf("Crier relay starting on :%d", cfg.Port)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("server failed: %v", err)
	}
}
