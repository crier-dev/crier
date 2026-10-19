package main

import (
	"fmt"
	"net/http"

	"github.com/gorilla/mux"

	"github.com/crier-dev/crier/config"
	"github.com/crier-dev/crier/internal/federation"
	"github.com/crier-dev/crier/internal/registry"
)

// configureFederationPeers is the production composition root for the opt-in
// lifecycle store. Existing bearer and peer-auth middleware remain in force.
func configureFederationPeers(cfg config.Config, r *mux.Router, handler *registry.Handler) (*federation.HandshakeService, error) {
	if cfg.Federation.RecordsFile == "" {
		return nil, nil
	}
	key, err := registry.LoadEd25519PrivateKeyFile(cfg.Federation.SelfKeyFile)
	if err != nil {
		return nil, fmt.Errorf("load instance key: %w", err)
	}
	store, err := federation.OpenPeerStore(cfg.Federation.RecordsFile)
	if err != nil {
		return nil, err
	}
	hs, err := federation.NewHandshakeService(store, federationName(cfg), key)
	if err != nil {
		return nil, err
	}
	handler.SetPeerRecords(store)
	// Management is never public, even if CR_AUTH_TOKEN is empty. A
	// separate admin token is useful on a key-only, bearer-free relay.
	operator := cfg.Permissions.AdminToken
	if operator == "" {
		operator = cfg.AuthToken
	}
	gate := func(h http.HandlerFunc) http.Handler { return federation.OperatorGate(operator, h) }
	r.Handle("/fed/peers", gate(store.HandleCreate)).Methods(http.MethodPost)
	r.Handle("/fed/peers", gate(store.HandleList)).Methods(http.MethodGet)
	r.Handle("/fed/peers/{id}", gate(func(w http.ResponseWriter, r *http.Request) { store.HandlePatch(mux.Vars(r)["id"], w, r) })).Methods(http.MethodPatch)
	r.Handle("/fed/handshake/start", gate(hs.HandleStart)).Methods(http.MethodPost)
	r.Handle("/fed/handshake/finish", gate(hs.HandleFinish)).Methods(http.MethodPost)
	return hs, nil
}
