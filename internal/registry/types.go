package registry

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/crier-dev/crier/internal/a2a"
	"github.com/crier-dev/crier/internal/guard"
	"github.com/crier-dev/crier/internal/webhook"
)

// AgentStatus represents the online/stale/offline state of an agent.
//
// "online" and "stale" are DERIVED per read from the row's liveness evidence —
// `last_seen` plus the documented staleness window (see presence.go,
// CR-FEAT-024) — never stored: an agent whose process died stops being reported
// online once the window passes, with no sweeper and no write. "offline" is the
// one value a row can STATE about itself, and a stated offline outranks the
// derivation.
type AgentStatus string

const (
	StatusOnline  AgentStatus = "online"
	StatusStale   AgentStatus = "stale"
	StatusOffline AgentStatus = "offline"
)

// HexKey is an ed25519.PublicKey that marshals as hex in JSON.
type HexKey ed25519.PublicKey

// MarshalJSON encodes the key as a hex string.
func (k HexKey) MarshalJSON() ([]byte, error) {
	return json.Marshal(hex.EncodeToString(k))
}

// UnmarshalJSON decodes a hex string into the key. An EMPTY key ("", zero
// decoded bytes) decodes to an empty HexKey — the keyless wire form a
// signature-enforcement-off agent legally carries since DF-CRIER-192 — so
// the JSON read path (RemoteStore) matches the in-memory and postgres
// representations. Any other length still fails the 32-byte guard.
func (k *HexKey) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	raw, err := hex.DecodeString(s)
	if err != nil {
		return fmt.Errorf("invalid hex key: %w", err)
	}
	if len(raw) != ed25519.PublicKeySize && len(raw) != 0 {
		return fmt.Errorf("invalid key length: %d, want %d", len(raw), ed25519.PublicKeySize)
	}
	*k = make(HexKey, len(raw))
	copy(*k, raw)
	return nil
}

// Agent represents a registered agent in the system.
type Agent struct {
	ID           string      `json:"id"`
	PublicKey    HexKey      `json:"public_key"`
	Capabilities []string    `json:"capabilities"`
	Status       AgentStatus `json:"status"`
	RegisteredAt time.Time   `json:"registered_at"`
	LastSeen     time.Time   `json:"last_seen"`
	// Webhook is the optional push-delivery endpoint (CR-FEAT-001).
	Webhook *webhook.Config `json:"webhook,omitempty"`
	// Guard is the optional LLM message-guard policy config (CR-FEAT-010).
	// API keys are never part of it — only env: refs (spec §4.1/§9.2).
	Guard *guard.AgentGuardConfig `json:"guard,omitempty"`
	// A2A is the optional OPT-IN A2A interoperability block (INT-A2A-001,
	// specs/A2A-OPTION.md §4.2) — the per-agent half of the A2A gate; the
	// server-side half is CR_A2A_ENABLED (config.Config.A2AEnabled). Absent
	// (omitempty) means the agent takes no part in A2A, which keeps the
	// serialized row byte-identical to a pre-A2A registration. Nothing
	// consumes it yet: INT-A2A-002..006 add the surfaces that do.
	A2A *a2a.Config `json:"a2a,omitempty"`
	// Namespace is the realm this agent belongs to (CR-FEAT-029). The DEFAULT
	// namespace — the only one a deployment that declares no namespaces has —
	// is spelled "" here, and omitempty then keeps the serialized row
	// byte-identical to a pre-CR-FEAT-029 registration. That is deliberate:
	// the acceptance criterion for this row is that single-namespace
	// behaviour is unchanged, and a row that gained a `"namespace":"default"`
	// member would change every existing client's payload (see
	// namespace.Canonical, the one place the mapping lives).
	//
	// A non-empty value names a namespace the server was started with
	// (CR_NAMESPACES / CR_NAMESPACES_FILE); registering into an undeclared
	// namespace is refused rather than mapped back to the default, because a
	// typo must never silently widen access.
	Namespace string `json:"namespace,omitempty"`
}

// InboxEntry is a message stored in an agent's persistent inbox.
type InboxEntry struct {
	ID            string        `json:"id"`
	AgentID       string        `json:"agent_id"`
	Payload       []byte        `json:"payload"`
	CreatedAt     time.Time     `json:"created_at"`
	ExpiresAt     time.Time     `json:"expires_at"`
	LeasedAt      *time.Time    `json:"leased_at,omitempty"`
	LeaseID       string        `json:"lease_id,omitempty"`
	LeaseDuration time.Duration `json:"-"` // not serialized; used by PurgeExpired
	ACKed         bool          `json:"acked"`
	// Sender is the originating agent id, recorded WITH the stored message
	// (CR-FEAT-025). It is not used for delivery routing — the webhook
	// envelope carries its own copy — but it is the address a terminal
	// outcome is reported to: when this message expires unacknowledged the
	// MESSAGE_EXPIRED receipt is written into the SENDER's inbox, which is
	// impossible to do from a purge that no longer knows who sent it. Empty
	// when the delivery named no sender.
	Sender string `json:"sender,omitempty"`
	// IdempotencyKey is the sender-supplied deduplication key the delivery
	// carried (CR-FEAT-025), recorded as provenance: it is what lets a dead
	// letter state which key produced the message. Deduplication itself
	// happens at deliver time, before anything is stored.
	IdempotencyKey string `json:"idempotency_key,omitempty"`
	// DeliveryMs is the wall-clock latency, in whole milliseconds, of the
	// deliver request that stored this message (CR-CHAT-020): decode → guard
	// → durable write, measured around the whole POST and stamped by the
	// store on the deliver path. It is the per-message figure the chat UI's
	// meta line draws. Absent (omitempty) when the entry was not written by
	// a timed deliver — a federation hold release, a webhook-failure notice,
	// an expiry receipt — which is "not measured", never a zero: a
	// sub-millisecond delivery is a real measurement and must not share a
	// representation with "no delivery produced this entry".
	DeliveryMs int64 `json:"delivery_ms,omitempty"`
	// Priority is the sender-supplied retrieval priority (CR-FEAT-035),
	// bounded by MinMessagePriority..MaxMessagePriority (0..9). Retrieve hands
	// back the HIGHEST priority messages first; messages of equal priority keep
	// their arrival order, and the zero value is that arrival order for
	// everything — a delivery that names no priority is exactly the FIFO
	// message it was before this field existed.
	//
	// It is `omitempty` on purpose: a priority-less message must serialise to
	// byte-identical JSON (no `"priority":0` key appears on the wire), so the
	// retrieve body of every pre-existing client is unchanged.
	Priority int `json:"priority,omitempty"`
	// TTLSeconds is the lifetime the delivery requested, in seconds
	// (POST /agents/{id}/inbox `ttl_seconds`, documented in openapi.yaml but
	// parsed nowhere until DF-CRIER-37). It is not serialized: it exists so
	// the store — or a remote proxy forwarding the delivery — can resolve the
	// expiry from the CALLER'S INTENT instead of re-deriving it from a
	// timestamp. nil = unspecified (the store default applies); 0 = never
	// expires (ExpiresAt stays the zero time); n > 0 = CreatedAt + n seconds.
	TTLSeconds *int `json:"-"`
	// Guard carries the LLM message-guard metadata for this entry
	// (CR-FEAT-010, spec §9.3). Present on guarded deliveries; absent when
	// the guard is disabled.
	Guard *guard.Meta `json:"guard,omitempty"`
	// Namespace is the realm the message was delivered into (CR-FEAT-029).
	// It is derived from the TARGET agent's row and recorded WITH the message,
	// so a stored message states its realm even after the agent is
	// unregistered or moved. The default namespace is "" and
	// `omitempty` keeps the entry byte-identical to a pre-CR-FEAT-029
	// message.
	Namespace string `json:"namespace,omitempty"`
	// ThreadID is the thread the message belongs to (CR-CHAT-019). It is the
	// deliver body's `thread_id` (CR-FEAT-004) recorded WITH the stored
	// message, so a thread is reconstructable from storage ALONE — before
	// this field a thread existed only as a wire tag and was lost the moment
	// the delivery was stored. A thread root's thread_id is its own message
	// id; a reply carries the thread it answers, never a new one
	// (specs/CHAT-SESSIONS.md §4.3/§4.5, D11). `omitempty` keeps an entry
	// from a client that names no thread byte-identical to before.
	ThreadID string `json:"thread_id,omitempty"`
	// Location is the machine-readable WHERE of this delivery (CR-CHAT-029):
	// instance, namespace, channel (the session), thread and sub-thread the
	// agent can SAY it is in, derived from the delivery context — never
	// inferred by the agent from prose. It is populated by the delivery path
	// itself (the registry deliver handler, and the session fan-out, which
	// owns the thread tree and therefore also resolves a sub-thread); a
	// REMOTE proxy deliberately does NOT forward it — the downstream relay
	// re-derives every field from its own row and context, exactly as it
	// already re-derives the namespace. Absent (nil, omitempty) on entries
	// written before this field existed.
	Location *Location `json:"location,omitempty"`
}

// Location states WHERE a delivery sits, so an agent can name its place
// instead of inferring it (CR-CHAT-029). Every field is the wire identity,
// not a display label: `channel` is the session id (the same value the
// deliver body carries as session_id), `thread` is the thread's root message
// id and `sub_thread` the sub-thread's id when the delivery was fanned out
// into a deliberately branched child thread (specs/CHAT-SESSIONS.md §4.5).
// `instance` names the serving relay (CR_FED_NAME / host:port); `namespace`
// is the realm the message was delivered into — the same canonical spelling
// as InboxEntry.Namespace. A delivery with no session context still carries
// instance + namespace: "where am I" is answerable for every delivery.
type Location struct {
	Instance  string `json:"instance,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	Channel   string `json:"channel,omitempty"`
	ThreadID  string `json:"thread,omitempty"`
	SubThread string `json:"sub_thread,omitempty"`
}

// MessageExpiry is the tri-state WIRE encoding of a message expiry
// (DF-CRIER-182). Storage is untouched: every consumption path — retrieve,
// stats, purge (DF-CRIER-37) — keeps reading the plain time.Time on
// InboxEntry, where the zero time means "never expires".
//
// That zero time is a perfectly valid time.Time, and time.Time's own JSON
// encoding renders it as "0001-01-01T00:00:00Z" — a syntactically valid
// RFC 3339 instant that any date-parsing client reads as broken or
// long-expired. On the wire the three states are therefore:
//
//   - ABSENT   — no expiry applies because no inbox entry was created (the
//     webhook delivery paths). Expressed by the carrying field being a nil
//     *MessageExpiry with omitempty, never by this type.
//   - null     — the message never expires (ttl_seconds=0): the key is
//     present, the value is JSON null.
//   - RFC 3339 — the resolved finite instant (ttl_seconds>0, or the 24h
//     default), byte-identical to what time.Time emitted before.
type MessageExpiry time.Time

// MarshalJSON encodes a never-expires expiry as JSON null and any other
// instant exactly as time.Time would (so a finite expiry is unchanged).
func (e MessageExpiry) MarshalJSON() ([]byte, error) {
	t := time.Time(e)
	if t.IsZero() {
		return []byte("null"), nil
	}
	return json.Marshal(t)
}

// UnmarshalJSON accepts the two forms this type emits on the wire: null
// (never expires → the zero time) and an RFC 3339 instant.
func (e *MessageExpiry) UnmarshalJSON(b []byte) error {
	if string(bytes.TrimSpace(b)) == "null" {
		*e = MessageExpiry(time.Time{})
		return nil
	}
	var t time.Time
	if err := json.Unmarshal(b, &t); err != nil {
		return err
	}
	*e = MessageExpiry(t)
	return nil
}

// MarshalJSON renders an inbox entry with the tri-state expiry (DF-CRIER-182):
// a never-expiring message (the zero ExpiresAt) goes out as
// "expires_at": null instead of the zero time. This is the ONE seam every
// surface that renders an entry — the HTTP retrieve body and the MCP bridge
// alike — goes through, so no two of them can disagree about the wire.
//
// The embedded alias keeps every other field exactly as its struct tag
// declares it (including fields added later), and the outer ExpiresAt field
// shadows the alias's own field of the same JSON name — encoding/json
// prefers the shallowest field for a duplicated name.
func (e InboxEntry) MarshalJSON() ([]byte, error) {
	type inboxEntry InboxEntry
	return json.Marshal(struct {
		inboxEntry
		ExpiresAt MessageExpiry `json:"expires_at"`
	}{
		inboxEntry: inboxEntry(e),
		ExpiresAt:  MessageExpiry(e.ExpiresAt),
	})
}
