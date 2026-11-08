// Package protocol is crier's first-class protocol adapter layer
// (specs/CRIER-PROTOCOLS.md). Every HTTP protocol surface crier can expose —
// relay pub/sub, mesh request/response, A2A, dagger control, federation — is
// one Protocol value: a name from the closed KnownProtocols set, its own
// pre-existing gate, and a single RegisterRoutes entry point that the ONE
// registration loop in cmd/server calls at startup. Adding a protocol means
// adding a name here and one adapter constructor in cmd/server/protocols.go —
// never an edit to the server's wiring (§7.5).
//
// The layer is a registration refactor only: it owns no delivery path. The
// core surfaces (registry/inbox routes and POST /ingress/messages) are not
// protocols (decision D-P1) and stay direct in cmd/server/main.go, and
// internal/harness keeps delivering INTO sessions — harness.Registry is
// consumed by the ingress dispatch hook, not wrapped here (§2).
package protocol

import (
	"fmt"
	"log/slog"

	"github.com/gorilla/mux"

	"github.com/crier-dev/crier/config"
	"github.com/crier-dev/crier/internal/daggerctl"
	"github.com/crier-dev/crier/internal/federation"
	"github.com/crier-dev/crier/internal/mesh"
	"github.com/crier-dev/crier/internal/registry"
	"github.com/crier-dev/crier/internal/relay"
)

// Protocol is one wire surface crier can expose. Implemented by relay, mesh,
// a2a, daggerctl and federation in the first wave (spec §3).
type Protocol interface {
	// Name is the closed-set protocol id.
	Name() string
	// Enabled reports the protocol's own gate (its existing config field).
	Enabled() bool
	// RegisterRoutes mounts every route the protocol owns onto r.
	// It is called once at startup, only when Enabled() is true.
	// It must not start goroutines, dial peers, or read env directly.
	RegisterRoutes(r *mux.Router, deps Deps) error
}

// Deps carries the shared wiring a protocol may touch. Fields are the same
// values runWithEnv passes today; a protocol that needs a new dep adds the
// field here and nowhere else (spec §3). The five spec fields are first; the
// entries after them are the additions the five first-wave adapters needed,
// each still one shared wiring value handed to RegisterRoutes — none of them
// is read from env by an adapter.
type Deps struct {
	RegistryHandler *registry.Handler
	Store           registry.Store
	Relay           *relay.Relay
	Config          *config.Config
	Logger          *slog.Logger

	// Mesh is the shared mesh service; the mesh protocol mounts its two
	// routes on it.
	Mesh *mesh.Mesh
	// Dagger is the dagger control service, built by the wiring only when
	// CR_DAGGER_URL names an executor; nil (with Dagger protocol disabled)
	// otherwise.
	Dagger *daggerctl.Service
	// FedClient is the relay-to-relay federation client; nil when
	// CR_FED_LINKS is unset, exactly what the /fed/peers listing tolerated
	// before the layer existed (it then lists this relay alone).
	FedClient *federation.Client
	// LocalPeer reports this relay's own peer view for /fed/peers.
	LocalPeer func() federation.Peer
	// FederationRecords reports the peer-records management surface
	// (CR-CHAT-023, configureFederationPeers) owns /fed/* for this boot;
	// the federation protocol then mounts nothing.
	FederationRecords bool
}

// KnownProtocols is the closed name set, imitating harness.KnownAdapters
// (internal/harness/config.go). Unknown names are a load-time hard failure.
var KnownProtocols = []string{"relay", "mesh", "a2a", "dagger", "federation"}

// IsKnown reports name is in the closed KnownProtocols set.
func IsKnown(name string) bool {
	for _, k := range KnownProtocols {
		if k == name {
			return true
		}
	}
	return false
}

// Validate checks a built protocol set against the closed set: an unknown
// name, a duplicate Name(), or a known name left unbuilt is a programmer
// error and a load-time hard failure (spec §5) — the caller exits 1 and never
// serves the partially wired mux.
func Validate(protos []Protocol) error {
	seen := make(map[string]bool, len(protos))
	for _, p := range protos {
		if p == nil {
			return fmt.Errorf("protocol: nil protocol in build set")
		}
		name := p.Name()
		if !IsKnown(name) {
			return fmt.Errorf("protocol: unknown protocol %q — KnownProtocols is a closed set: %v", name, KnownProtocols)
		}
		if seen[name] {
			return fmt.Errorf("protocol: duplicate protocol %q", name)
		}
		seen[name] = true
	}
	for _, name := range KnownProtocols {
		if !seen[name] {
			return fmt.Errorf("protocol: known protocol %q has no adapter in the build set", name)
		}
	}
	return nil
}
