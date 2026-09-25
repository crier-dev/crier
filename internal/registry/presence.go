package registry

// Presence: status derived from liveness evidence (CR-FEAT-024).
//
// Before this, `status` was a REGISTRATION fact: the store stamped "online"
// when the row was created and nothing ever changed it, so an agent whose
// process had crashed went on reporting "online" with a frozen `last_seen` —
// and a dashboard built on GET /agents reported a dead fleet as healthy
// (external review 'DISPATCH · CRI-001' by Carter: liveness "a known lie").
//
// The fix is not a second source of truth but a rule over the one this
// registry already had. Liveness EVIDENCE advances a row's `last_seen`:
//
//   - a mesh socket is ACCEPTED for that agent (`AcceptPeer`, i.e. someone
//     presented itself as that agent on /mesh/connect/{agentID});
//   - a KEEPALIVE heartbeat arrives on it (internal/mesh/peer.go) — the 30s
//     loop every shipped mesh client already runs;
//   - a signed PATCH /agents/{id} succeeds (DF-CRIER-156, unchanged).
//
// and the status the registry REPORTS is then derived, per read, from that
// timestamp and one documented window:
//
//	last_seen inside the window   → online
//	last_seen older, or never set → stale
//	row stored as offline         → offline (an explicit statement wins)
//
// The derivation is a READ-time projection, deliberately not a stored value:
// nothing has to run on a timer for a dead agent to stop looking alive, a
// restart cannot disagree with a sweeper that never ran, and the storage
// contract is untouched — the `agents.status` column still holds only
// "online"/"offline" (its CHECK constraint, migrations/001), because `stale`
// is what a row MEANS at this instant, not a fact about the row.
//
// The window is the whole contract, so it is a documented, configurable
// number: `CR_PRESENCE_STALE_AFTER_S`, default DefaultStalenessWindow — three
// missed mesh heartbeats, so a single dropped tick can never flap a live agent
// to `stale`.
//
// The limits are stated rather than implied, because a presence signal that
// over-claims is the same lie in a new place:
//
//   - evidence is MESH and registry activity. An agent that never opens a mesh
//     socket and never PATCHes reads `stale` after the window even while its
//     process is healthy — this is a mesh-presence statement, not a process
//     liveness probe. (GET /mesh/peers is the connection-table view: exact,
//     and empty for an agent that never connected.)
//   - the mesh does NOT disconnect a peer that stops heartbeating (there is no
//     heartbeat deadline on an accepted socket): such a peer keeps its
//     connection, is still listed by GET /mesh/peers, and only its registry row
//     goes stale.
//   - a row is refreshed only by the evidence above. Delivering to an agent,
//     retrieving its inbox or publishing as it does NOT count: those paths
//     cannot attribute the caller to the agent with the same confidence (the
//     signed routes can, but that is a separate decision — see the row's
//     closing note).

import (
	"errors"
	"log/slog"
	"time"
)

// DefaultStalenessWindow is how long a registry row may go without liveness
// evidence before it is reported StatusStale. It is three times the mesh
// keepalive interval (mesh.DefaultMeshConfig().KeepaliveInterval, 30s), so a
// single missed heartbeat never flips a live agent to stale.
const DefaultStalenessWindow = 90 * time.Second

// Presence is the read-time rule that turns a row's liveness evidence
// (`last_seen`) into the status reported for it.
//
// The zero value is usable and means the documented default: an unset
// StalenessWindow resolves to DefaultStalenessWindow, so a Handler or embedder
// that never configured presence still reports stale rows honestly instead of
// reporting every row online (window 0 would do the opposite — it would call a
// row stale the instant it stopped being touched).
type Presence struct {
	// StalenessWindow is the documented window: liveness evidence older than
	// this makes a row report stale. <= 0 means DefaultStalenessWindow.
	StalenessWindow time.Duration
}

// NewPresence builds the presence rule for window. A non-positive window means
// "the default", never "stale immediately" — the fail-closed direction here
// would mark every row stale on a misconfiguration and make the signal
// useless.
func NewPresence(window time.Duration) Presence {
	return Presence{StalenessWindow: window}
}

// StaleAfter returns the EFFECTIVE window — what this rule actually applies,
// with the zero/negative case resolved to the default. It is the value
// GET /status reports, so an operator reading a `stale` row sees the window it
// was judged by rather than the environment variable they think they set.
func (p Presence) StaleAfter() time.Duration {
	if p.StalenessWindow > 0 {
		return p.StalenessWindow
	}
	return DefaultStalenessWindow
}

// StatusOf reports the status a row's evidence implies at instant now.
func (p Presence) StatusOf(agent *Agent, now time.Time) AgentStatus {
	if agent == nil {
		return ""
	}
	// An explicit statement on the row outranks the derivation: the stores
	// still accept "offline" (PostgresStore.Register validates the same set),
	// and a row that says offline must not be reported online just because
	// something touched it.
	if agent.Status == StatusOffline {
		return StatusOffline
	}
	if agent.LastSeen.IsZero() {
		// No evidence ever recorded for this row: it cannot be claimed online.
		return StatusStale
	}
	if now.Sub(agent.LastSeen) <= p.StaleAfter() {
		return StatusOnline
	}
	return StatusStale
}

// Derive returns a COPY of agent carrying the status its evidence implies.
//
// A copy, never the stored row, for two reasons that both matter: the in-memory
// store hands out its live pointers, so writing the derived value into one
// would race every concurrent reader (and would persist a derived value into a
// column whose CHECK constraint — migrations/001 — allows only online/offline);
// and a read must not change what the next read sees.
//
// A nil agent derives to nil, so callers can pass a value straight through.
func (p Presence) Derive(agent *Agent, now time.Time) *Agent {
	if agent == nil {
		return nil
	}
	derived := *agent
	derived.Status = p.StatusOf(agent, now)
	return &derived
}

// DeriveAll applies Derive to a whole listing with ONE instant, so every row in
// a GET /agents body is judged against the same clock.
func (p Presence) DeriveAll(agents []*Agent, now time.Time) []*Agent {
	if agents == nil {
		return nil
	}
	out := make([]*Agent, 0, len(agents))
	for _, a := range agents {
		out = append(out, p.Derive(a, now))
	}
	return out
}

// Toucher is an optional Store capability: record liveness evidence for an
// agent by advancing its `last_seen`, WITHOUT rewriting the rest of the row and
// without changing its stored status (the heartbeat path, CR-FEAT-024).
//
// Stores that cannot touch (the remote proxy behind the MCP bridge's
// RemoteStore) leave it unimplemented; HeartbeatSink then wires nothing and the
// mesh records no liveness, exactly as it did before the sink existed.
//
// Implementations must be safe for concurrent callers and must be cheap: the
// mesh calls this on its socket read path, once per heartbeat per agent.
type Toucher interface {
	Touch(id string, at time.Time) error
}

// HeartbeatSink returns the liveness sink a mesh should call when an agent's
// socket shows evidence of life: it advances that agent's `last_seen` via the
// store's Touch.
//
// It returns nil when the store cannot touch, and a nil sink is the documented
// "no liveness bookkeeping" posture — the mesh checks for nil and calls
// nothing, so a store without the capability costs one type assertion at boot
// and nothing per frame.
//
// Errors are logged, never propagated: the caller is a WebSocket read loop, and
// a heartbeat is by definition a signal that can be missed without harm (the
// next one arrives in 30s). "Agent not registered" is the ORDINARY case for a
// peer that connected without a registry row (a raw client, or the server's own
// mesh identity), so it is debug; anything else — a store failure — is a warn,
// because a registry that cannot record liveness is a presence signal that has
// silently stopped telling the truth.
func HeartbeatSink(store Store) func(agentID string, at time.Time) {
	toucher, ok := store.(Toucher)
	if !ok {
		return nil
	}
	return func(agentID string, at time.Time) {
		if agentID == "" {
			return
		}
		if err := toucher.Touch(agentID, at); err != nil {
			if errors.Is(err, ErrAgentNotFound) {
				slog.Debug("presence: heartbeat from an unregistered agent", "agent_id", agentID)
				return
			}
			slog.Warn("presence: heartbeat not recorded", "agent_id", agentID, "error", err)
		}
	}
}
