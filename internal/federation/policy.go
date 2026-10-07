package federation

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Peer policy (CR-CHAT-023, specs/CHAT-FEDERATION.md §6): the per-peer rule
// about which namespaces and which agents may cross the boundary, in either
// direction. One policy per peer; the record is DATA the boundary resolves
// locally — a peer's own claim about itself (§3.3) is never authority.
//
// The default is DENY: a peer with no policy record, or a namespace/agent
// absent from the policy, is refused with a NAMED outcome, never silently
// admitted and never silently dropped. The shipped token path (§5.3) is
// unchanged: with no peer policies configured, an instance federates exactly
// as it did before this file existed (§1.3 — extend, never replace).

// PeerHeader is the HTTP header a relay sets on a forwarded deliver request
// to name the identity it announces to the destination (the policy key the
// DESTINATION resolves). It is an identity CLAIM, not a credential: the
// destination's policy decides what the named peer may reach, and nothing in
// the request is believed because of it (§3.3). Absent means "no peer
// identity declared" — the destination then applies no peer policy, which is
// the shipped (degraded) posture, byte-for-byte unchanged.
const PeerHeader = "X-Crier-Fed-Peer"

// PeerPolicy is one peer's admission rule. It serves BOTH directions:
//
//   - OUTBOUND (this instance → the peer): NamespacesAllow decides which of
//     THIS instance's namespaces may cross to the peer (a session fan-out to
//     a remote participant of this peer refuses a namespace the policy does
//     not admit — §6.4 FED_NAMESPACE_NOT_PERMITTED), and URL + SelfAs are
//     the shipped forward's addressing (§4.1: addressable, not authoritative).
//   - INBOUND (the peer → this instance): NamespacesAllow decides which of
//     THIS instance's namespaces the peer may deliver INTO, and AgentsAllow /
//     AgentsDeny decide which local agents it may reach (deny wins — §6.1).
type PeerPolicy struct {
	// Peer is the local name for the peer — the handle a refusal, an audit
	// line and a remote member id (`remote:<peer>:<subject>`) all carry.
	// Local and stable; never derived from a remote claim.
	Peer string `json:"peer"`
	// URL is the peer's base URL for the shipped forward. Empty means the
	// policy is INBOUND-only: no fan-out may cross to this peer (a remote
	// participant of it is admitted, but delivery to the peer refuses).
	URL string `json:"url,omitempty"`
	// SelfAs is the identity THIS instance announces to that peer via
	// PeerHeader — the peer id the DESTINATION's policy is keyed by. Empty
	// announces nothing (no header), which leaves the destination in the
	// shipped no-peer-identity posture.
	SelfAs string `json:"self_as,omitempty"`
	// NamespacesAllow lists the namespaces (canonicalized) that may cross
	// the boundary with this peer, in either direction. EMPTY MEANS NONE —
	// the default is deny (§6.1: no grant-style fall-through).
	NamespacesAllow []string `json:"namespaces_allow,omitempty"`
	// AgentsAllow lists the local agent ids this peer may reach on an
	// INBOUND forward. Empty admits every agent not denied (the namespace
	// gate is the primary wall); a name here is an explicit second gate.
	AgentsAllow []string `json:"agents_allow,omitempty"`
	// AgentsDeny lists local agent ids this peer may NEVER reach. Deny wins
	// over allow (§6.1) — an explicit exclusion is never overridden.
	AgentsDeny []string `json:"agents_deny,omitempty"`
}

// AdmitsNamespace reports whether the named namespace may cross the boundary
// with this peer. Canonicalized comparison, deny by default.
func (p *PeerPolicy) AdmitsNamespace(ns string) bool {
	if p == nil {
		return false
	}
	for _, allowed := range p.NamespacesAllow {
		if allowed == ns {
			return true
		}
	}
	return false
}

// AdmitsAgent reports whether this peer may reach the named local agent on an
// inbound forward. Deny wins over allow; an empty allow admits every agent
// the namespace gate already admitted.
func (p *PeerPolicy) AdmitsAgent(id string) bool {
	if p == nil {
		return false
	}
	for _, denied := range p.AgentsDeny {
		if denied == id {
			return false
		}
	}
	if len(p.AgentsAllow) == 0 {
		return true
	}
	for _, allowed := range p.AgentsAllow {
		if allowed == id {
			return true
		}
	}
	return false
}

// PeerPolicies is the policy set: one record per peer, keyed by the local
// peer id. A nil/empty set admits no peer — the shipped posture when nothing
// is configured (every peer-scoped surface then behaves as before).
type PeerPolicies map[string]*PeerPolicy

// PolicyFor resolves the policy for a peer id. nil means the peer is unknown:
// the caller refuses with a NAMED outcome (wholesale), never falls through to
// "treat as local".
func (pp PeerPolicies) PolicyFor(peer string) *PeerPolicy {
	if pp == nil {
		return nil
	}
	return pp[peer]
}

// NamespaceNotPermittedError is the OUTBOUND refusal of §6.4: every link
// that could carry the delivery is governed by a peer policy that does not
// admit the message's namespace, so nothing was forwarded. It is a
// DEFINITIVE refusal, never a TransientError and never a 404: the caller
// answers 403 FED_NAMESPACE_NOT_PERMITTED and the sender's outcome is
// recorded as refused with that reason.
type NamespaceNotPermittedError struct{}

// Error implements error.
func (e *NamespaceNotPermittedError) Error() string {
	return "federation: no peer policy admits this message's namespace (FED_NAMESPACE_NOT_PERMITTED)"
}

// peersDocument is the CR_FED_PEERS_FILE document shape.
type peersDocument struct {
	Peers []PeerPolicy `json:"peers"`
}

// LoadPeerPoliciesFile reads and validates a CR_FED_PEERS_FILE document.
// Every policy must carry a peer id; duplicate ids are a load error (a
// silently-wins duplicate would make the operator's intent ambiguous).
// Namespace names are canonicalized on load, so a policy file and the
// session/registry comparisons can never disagree about spelling.
func LoadPeerPoliciesFile(path string) (PeerPolicies, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("federation: read peers file: %w", err)
	}
	var doc peersDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("federation: parse peers file %s: %w", path, err)
	}
	out := PeerPolicies{}
	for i := range doc.Peers {
		p := &doc.Peers[i]
		p.Peer = strings.TrimSpace(p.Peer)
		if p.Peer == "" {
			return nil, fmt.Errorf("federation: peers file %s: peers[%d] has no peer id", path, i)
		}
		if _, dup := out[p.Peer]; dup {
			return nil, fmt.Errorf("federation: peers file %s: duplicate peer id %q", path, p.Peer)
		}
		p.URL = strings.TrimSuffix(strings.TrimSpace(p.URL), "/")
		for j, ns := range p.NamespacesAllow {
			p.NamespacesAllow[j] = strings.TrimSpace(ns)
		}
		out[p.Peer] = p
	}
	return out, nil
}
