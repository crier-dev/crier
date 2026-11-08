// protocols.go — the ONE place a protocol surface is mounted
// (specs/CRIER-PROTOCOLS.md §3, INT-007).
//
// The five first-wave adapters (relay, mesh, a2a, dagger, federation) live
// here because they mount through the unexported registration helpers that
// own the handler construction (registerA2ARoute, registerA2ACardRoute, the
// dagger handler, federation.HandlePeers) — code that cannot move to
// internal/protocol without moving the A2A serving stack out of the cmd
// layer. The CONTRACT (Protocol interface, Deps, the closed KnownProtocols
// set, load-time validation) lives in internal/protocol; nothing in
// cmd/server/main.go registers a protocol route directly any more: main
// builds protocol.Deps once and calls registerProtocols, and every surface
// below decides its own gate and mounts its own routes.
//
// Core surfaces — registry/inbox routes, POST /ingress/messages, trust,
// sessions, status, detection, observability — are NOT protocols (decision
// D-P1: the core delivery path is not a removable surface) and stay direct
// in main.go.
package main

import (
	"fmt"
	"log/slog"
	"net/http"

	"github.com/gorilla/mux"

	"github.com/crier-dev/crier/config"
	"github.com/crier-dev/crier/internal/a2a"
	"github.com/crier-dev/crier/internal/daggerctl"
	"github.com/crier-dev/crier/internal/federation"
	"github.com/crier-dev/crier/internal/mesh"
	"github.com/crier-dev/crier/internal/protocol"
	"github.com/crier-dev/crier/internal/relay"
)

// ---------------------------------------------------------------------------
// The five first-wave adapters.
// ---------------------------------------------------------------------------

// relayProtocol mounts the relay pub/sub surface. Always on today (spec §3):
// the relay has no gate boolean, so Enabled is the constant true it always
// behaved as.
type relayProtocol struct {
	svc *relay.Relay
}

func (p relayProtocol) Name() string  { return "relay" }
func (p relayProtocol) Enabled() bool { return true }

func (p relayProtocol) RegisterRoutes(r *mux.Router, _ protocol.Deps) error {
	if p.svc == nil {
		return fmt.Errorf("relay service not wired (Deps.Relay is nil)")
	}
	r.HandleFunc("/relay/publish", p.svc.HandlePublish).Methods("POST")
	r.HandleFunc("/relay/subscribe/{topic}", p.svc.HandleSubscribe)
	r.HandleFunc("/relay/topics", p.svc.HandleTopics).Methods("GET")
	return nil
}

// meshProtocol mounts the P2P mesh request/response surface. Always on today
// (spec §3): the mesh has no gate boolean either.
type meshProtocol struct {
	svc *mesh.Mesh
}

func (p meshProtocol) Name() string  { return "mesh" }
func (p meshProtocol) Enabled() bool { return true }

func (p meshProtocol) RegisterRoutes(r *mux.Router, _ protocol.Deps) error {
	if p.svc == nil {
		return fmt.Errorf("mesh service not wired (Deps.Mesh is nil)")
	}
	r.HandleFunc("/mesh/connect/{agentID}", mesh.HandleConnect(p.svc))
	r.HandleFunc("/mesh/peers", mesh.HandlePeers(p.svc)).Methods("GET")
	return nil
}

// a2aProtocol mounts the A2A discovery + JSON-RPC surfaces. Its gate is the
// EXISTING two-half opt-in (A2A-OPTION §1, preserved exactly by §2 of the
// protocol spec): CR_A2A_ENABLED (default false) plus the per-agent `a2a`
// registry block. With the switch unset zero A2A routes exist.
type a2aProtocol struct {
	cfg *config.Config
}

func (p a2aProtocol) Name() string  { return "a2a" }
func (p a2aProtocol) Enabled() bool { return p.cfg.A2AEnabled }

func (p a2aProtocol) RegisterRoutes(r *mux.Router, deps protocol.Deps) error {
	registerA2ACardRoute(r, deps.Store, a2aCardOptions{
		replayWindow:           p.cfg.IdempotencyWindow,
		port:                   p.cfg.Port,
		authTokenSet:           p.cfg.AuthToken != "",
		agentSignatureEnforced: p.cfg.RequireAgentSig,
	})
	// The JSON-RPC binding (INT-A2A-003 §5.4, extended by INT-A2A-004 §5.5 and
	// INT-A2A-005): SendMessage, SendStreamingMessage, the task lifecycle and
	// the push-notification configuration methods, at the path the card
	// advertises. A send reuses the registry handler's own HandleDeliver; the
	// push configuration operations reuse HandleUpdateAgent.
	registerA2ARoute(r, deps.Store, deps.RegistryHandler.HandleDeliver, deps.RegistryHandler.HandleUpdateAgent, deps.Relay, a2aOptions{
		replayWindow:           p.cfg.IdempotencyWindow,
		port:                   p.cfg.Port,
		authTokenSet:           p.cfg.AuthToken != "",
		agentSignatureEnforced: p.cfg.RequireAgentSig,
	})
	slog.Info("A2A option enabled", "route", a2a.AgentCardPath,
		"serves", "an AgentCard for agents whose registry row opted in (\"a2a\":{\"enabled\":true}); every other id answers 404",
		"json_rpc_route", a2a.JSONRPCBindingPath,
		"methods", a2a.MethodSendMessage+" (a Task or a direct Message), "+
			a2a.MethodSendStreamingMessage+" (text/event-stream), "+
			a2a.MethodGetTask+", "+a2a.MethodListTasks+", "+
			a2a.MethodCancelTask+", "+a2a.MethodSubscribeToTask+
			" and the push-notification configuration methods "+
			a2a.MethodCreateTaskPushNotificationConfig+"/"+a2a.MethodGetTaskPushNotificationConfig+"/"+
			a2a.MethodListTaskPushNotificationConfigs+"/"+a2a.MethodDeleteTaskPushNotificationConfig+
			" (a view over the agent's existing webhook config)",
		"not_registered", "GetExtendedAgentCard (INT-A2A-006)")
	return nil
}

// daggerProtocol mounts the dagger control surface. Its gate is the EXISTING
// opt-in, verbatim (spec §3): CR_DAGGER_URL names an executor or the surface
// does not exist (cmd/server/daggerctl.go). The service itself is built by
// the wiring (buildDaggerService) because its construction error is a boot
// failure with dagger-specific diagnostics; nil with the gate off.
type daggerProtocol struct {
	cfg *config.Config
	svc *daggerctl.Service
}

func (p daggerProtocol) Name() string  { return "dagger" }
func (p daggerProtocol) Enabled() bool { return p.cfg.Dagger.URL != "" }

func (p daggerProtocol) RegisterRoutes(r *mux.Router, _ protocol.Deps) error {
	if p.svc == nil {
		// Enabled with no service is a wiring inconsistency (spec §6 edge:
		// refuse startup rather than nil-pointer mid-request).
		return fmt.Errorf("dagger control: CR_DAGGER_URL is set but the control service was not built")
	}
	h := daggerctl.NewHandler(p.svc)
	r.HandleFunc("/dagger/runs", h.HandleCreateRun).Methods(http.MethodPost)
	r.HandleFunc("/dagger/runs/{id}", h.HandleGetRun).Methods(http.MethodGet)
	r.HandleFunc("/dagger/runs/{id}/cancel", h.HandleCancel).Methods(http.MethodPost)
	r.HandleFunc("/dagger/runs/{id}/resume", h.HandleResume).Methods(http.MethodPost)
	r.HandleFunc("/dagger/runs/{id}/rewind", h.HandleRewind).Methods(http.MethodPost)
	r.HandleFunc("/dagger/skills/{skill}/run", h.HandleRunSkill).Methods(http.MethodPost)
	// Delivery waits (CR-CHAT-034): deliver-and-wait, resolve, and status.
	r.HandleFunc("/dagger/wait", h.HandleDeliverAndWait).Methods(http.MethodPost)
	r.HandleFunc("/dagger/waits/{key}/resolve", h.HandleResolveWait).Methods(http.MethodPost)
	r.HandleFunc("/dagger/waits/{key}", h.HandleWaitStatus).Methods(http.MethodGet)
	return nil
}

// federationProtocol mounts the /fed/peers listing.
//
// Enabled reproduces the pre-refactor mounting exactly (main.go's
// `peerService == nil` branch): the listing is mounted on EVERY boot where
// the peer-records management surface (CR-CHAT-023, configureFederationPeers
// in federation_peers.go) does not own /fed/* — with no federation configured
// at all it still answers, listing this relay alone. Spec §3's gate formula
// (`len(Links) > 0 || RecordsFile != ""`) does not describe that behavior —
// and the binding route-census parity (§2 R-A1, §7.1) requires byte-identical
// routes — so parity wins and the deviation is recorded in the INT-007
// report. CR_FED_LINKS changes the handler's client, not the mounted set:
// the census is insensitive to the fed-links axis either way.
type federationProtocol struct {
	cfg       *config.Config
	fedClient *federation.Client
	localPeer func() federation.Peer
}

func (p federationProtocol) Name() string { return "federation" }
func (p federationProtocol) Enabled() bool {
	// Records mode: the peer-records management surface owns /fed/* (see
	// the type comment); the protocol mounts nothing.
	return p.cfg.Federation.RecordsFile == ""
}

func (p federationProtocol) RegisterRoutes(r *mux.Router, _ protocol.Deps) error {
	// fedClient is nil with no CR_FED_LINKS — the listing then shows this
	// relay alone, exactly as before the layer existed.
	r.HandleFunc("/fed/peers", federation.HandlePeers(p.fedClient, p.localPeer)).Methods("GET")
	return nil
}

// ---------------------------------------------------------------------------
// The closed constructor set + the ONE registration loop.
// ---------------------------------------------------------------------------

// protocolConstructors is the closed set of adapter constructors, one per
// name in protocol.KnownProtocols. Adding a protocol means adding the name
// to protocol.KnownProtocols and one entry here (spec §7.5) —
// buildProtocols, registerProtocols and cmd/server/main.go need no edit.
// Tests swap entries to inject bogus, failing and throwaway protocols.
var protocolConstructors = map[string]func(protocol.Deps) protocol.Protocol{
	"relay": func(d protocol.Deps) protocol.Protocol {
		return relayProtocol{svc: d.Relay}
	},
	"mesh": func(d protocol.Deps) protocol.Protocol {
		return meshProtocol{svc: d.Mesh}
	},
	"a2a": func(d protocol.Deps) protocol.Protocol {
		return a2aProtocol{cfg: d.Config}
	},
	"dagger": func(d protocol.Deps) protocol.Protocol {
		return daggerProtocol{cfg: d.Config, svc: d.Dagger}
	},
	"federation": func(d protocol.Deps) protocol.Protocol {
		return federationProtocol{cfg: d.Config, fedClient: d.FedClient, localPeer: d.LocalPeer}
	},
}

// buildProtocols builds the validated closed set in KnownProtocols order
// (spec §4). An unknown name, a missing adapter, or a duplicate Name() is a
// load-time hard failure — the caller exits 1 and never serves the partial
// mux (spec §5). getenv is part of the loop's signature (§3): the adapters
// read their gates from Deps.Config, never env directly, so it is carried
// for a future protocol whose wiring needs it.
func buildProtocols(deps protocol.Deps, getenv config.Getenv) ([]protocol.Protocol, error) {
	protos := make([]protocol.Protocol, 0, len(protocol.KnownProtocols))
	for _, name := range protocol.KnownProtocols {
		build, ok := protocolConstructors[name]
		if !ok {
			return nil, fmt.Errorf("protocol: no adapter constructor for %q — KnownProtocols is a closed set: %v", name, protocol.KnownProtocols)
		}
		protos = append(protos, build(deps))
	}
	for name := range protocolConstructors {
		if !protocol.IsKnown(name) {
			return nil, fmt.Errorf("protocol: unknown protocol %q — KnownProtocols is a closed set: %v", name, protocol.KnownProtocols)
		}
	}
	if err := protocol.Validate(protos); err != nil {
		return nil, err
	}
	// A fresh slice every call: the caller cannot mutate the closed set
	// through it (spec §6).
	return protos, nil
}

// registerProtocols is the ONE registration loop every protocol surface
// mounts through (spec §3). A disabled protocol registers zero routes and
// logs one line; an error aborts the boot — a partial mux is never served.
func registerProtocols(r *mux.Router, deps protocol.Deps, getenv config.Getenv) error {
	protos, err := buildProtocols(deps, getenv)
	if err != nil {
		return err
	}
	for _, p := range protos {
		if !p.Enabled() {
			slog.Info("protocol disabled", "protocol", p.Name())
			continue
		}
		if err := p.RegisterRoutes(r, deps); err != nil {
			return fmt.Errorf("protocol %s: %w", p.Name(), err)
		}
	}
	return nil
}
