package main

// mesh_auth.go — the server's bridge between the registry's identity records
// and the mesh connect handshake (DF-CRIER-287).
//
// The mesh package must not import the registry (the registry knows nothing
// about sockets, and the mesh is usable without a registry at all), so the
// adapter lives here: it is the one place that says "the key the mesh verifies
// against is the key the registry holds for that agent id". That single source
// is the point of the fix — the inbox lane and the mesh lane now answer "who
// is agent X?" from the same record, and an agent that cannot sign for X on
// the inbox lane cannot claim X on the mesh either.

import (
	"crypto/ed25519"
	"fmt"

	"github.com/crier-dev/crier/internal/registry"
)

// registryKeyProvider resolves an agent's registered ed25519 public key from
// the registry store serving this process. It implements
// mesh.AgentKeyProvider.
type registryKeyProvider struct {
	store registry.Store
}

// RegisteredPublicKey returns the key the registry holds for agentID.
//
// Every failure is a failure to verify, so the handshake fails closed: an
// unknown agent, a store error, and — importantly — a KEYLESS registration
// (legal on a server running with CR_REQUIRE_AGENT_SIG=false, since DF-CRIER-192
// keeps those rows) are all errors here rather than a zero-length key the
// signature check would reject anyway. That way the refusal on the wire names
// the real cause instead of reporting a signature that "did not verify"
// against a key that does not exist.
func (p registryKeyProvider) RegisteredPublicKey(agentID string) (ed25519.PublicKey, error) {
	agent, err := p.store.Get(agentID)
	if err != nil {
		return nil, fmt.Errorf("registry lookup for %q: %w", agentID, err)
	}
	if len(agent.PublicKey) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("agent %q is registered without a usable ed25519 public key (%d bytes); re-register it with its key (POST /agents) before using the authenticated mesh",
			agentID, len(agent.PublicKey))
	}
	return ed25519.PublicKey(agent.PublicKey), nil
}
