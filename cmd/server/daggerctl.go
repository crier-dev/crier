// daggerctl.go — wiring for the OPT-IN dagger control surface (CR-CHAT-033).
//
// The surface is a CLIENT of an executor crier does not own (decision D18):
// every control verb is one request to the dagger HTTP/JSON endpoint named by
// CR_DAGGER_URL, and crier holds only the run records it creates. With
// CR_DAGGER_URL unset this file registers nothing, so a deployment that does
// not control DAGs serves exactly the surface it served before the feature
// existed.
//
// The routes live here rather than in main.go for the same reason the A2A
// option's do: an opt-in surface must not inflate the always-on route count
// main.go publishes (README's "router paths registered in cmd/server/main.go").
package main

import (
	"fmt"
	"net/http"

	"github.com/gorilla/mux"

	"github.com/crier-dev/crier/config"
	"github.com/crier-dev/crier/internal/daggerctl"
	"github.com/crier-dev/crier/internal/registry"
)

// registerDaggerRoutes builds the control service and registers its six routes.
// It returns (nil, nil) when the surface is switched off — no bridge, no store,
// no routes — and never a half-wired surface: a bad CR_DAGGER_URL or an
// unusable store directory is an error the caller turns into a boot failure.
//
// The routes:
//
//	POST /dagger/runs                   create a prompt-driven run
//	GET  /dagger/runs/{id}              read a run's record
//	POST /dagger/runs/{id}/cancel       cancel a run
//	POST /dagger/runs/{id}/resume       resume a run from its checkpoint
//	POST /dagger/runs/{id}/rewind       rewind a run to a node
//	POST /dagger/skills/{skill}/run     run a registered skill
//
// Everything is additive: no existing route, body or auth requirement moves,
// and the terminal outcome travels the SHIPPED inbox path (registry Deliver)
// rather than a side channel.
func registerDaggerRoutes(r *mux.Router, cfg config.DaggerConfig, store registry.Store) (*daggerctl.Service, error) {
	if cfg.URL == "" {
		return nil, nil
	}

	opts := []daggerctl.HTTPBridgeOption{daggerctl.WithBearerToken(cfg.Token)}
	if cfg.Timeout > 0 {
		opts = append(opts, daggerctl.WithHTTPClient(&http.Client{Timeout: cfg.Timeout}))
	}
	bridge, err := daggerctl.NewHTTPBridge(cfg.URL, opts...)
	if err != nil {
		return nil, fmt.Errorf("dagger control: %w", err)
	}

	runStore, err := daggerctl.NewJSONLStore(cfg.StoreDir)
	if err != nil {
		return nil, fmt.Errorf("dagger control: %w", err)
	}

	svc := daggerctl.NewService(bridge, runStore, daggerctl.InboxDeliverer(store))
	svc.SetWatchInterval(cfg.PollInterval)

	h := daggerctl.NewHandler(svc)
	r.HandleFunc("/dagger/runs", h.HandleCreateRun).Methods(http.MethodPost)
	r.HandleFunc("/dagger/runs/{id}", h.HandleGetRun).Methods(http.MethodGet)
	r.HandleFunc("/dagger/runs/{id}/cancel", h.HandleCancel).Methods(http.MethodPost)
	r.HandleFunc("/dagger/runs/{id}/resume", h.HandleResume).Methods(http.MethodPost)
	r.HandleFunc("/dagger/runs/{id}/rewind", h.HandleRewind).Methods(http.MethodPost)
	r.HandleFunc("/dagger/skills/{skill}/run", h.HandleRunSkill).Methods(http.MethodPost)
	return svc, nil
}
