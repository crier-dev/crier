package registry

import (
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// This file is INBOX-CRIER-001: a delivery is not action.
//
// Before it, an accepted message could sit in an inbox — "delivered", the
// accept already answered — for as long as its message-level TTL allowed, with
// no statement anywhere about WHO was supposed to read it, HOW OFTEN, or what
// the receiving side had promised. On 2026-11-09 the live registry held
// unacknowledged entries eleven days old and every surface still read healthy.
//
// The consumer CONTRACT closes that hole from the receiving side: an agent (or
// its harness) declares who reads the inbox, the cadence it means to read on,
// and the maximum time an unacknowledged message may remain. The expiry sweep
// enforces that maximum by promoting overdue entries into the existing
// dead-letter destination and writing exactly one MESSAGE_EXPIRED receipt to
// the sender — the CR-FEAT-025 outcome, now reachable by contract as well as by
// message TTL. Delivery semantics themselves are untouched: lease, ack and
// idempotency behave byte-for-byte as before.

// DefaultConsumerContractTTL is the deployment default an agent's inbox
// contract inherits when the agent declares none (`contract_ttl_secs` absent
// from the consumer block). 24h is the documented pre-existing message
// lifetime (DefaultMessageTTL), so introducing the contract changes no
// existing agent's observable expiry until it declares something else. It is
// the value config.New() seeds Config.InboxContractTTL with and the value a
// Handler built without SetConsumerContractTTL uses — one constant, not a
// copy.
const DefaultConsumerContractTTL = 24 * time.Hour

// The closed vocabulary of `consumer.reader`: who physically drains the inbox.
const (
	// ConsumerReaderAgent means the registered agent process itself polls or
	// holds a long-poll / lease on its own inbox.
	ConsumerReaderAgent = "agent"
	// ConsumerReaderHarness means a harness (a foreman, a session runner, an
	// MCP bridge) reads the inbox on the agent's behalf and hands messages to
	// the agent's work loop.
	ConsumerReaderHarness = "harness"
)

// ConsumerContract is the optional `consumer` block on an agent registration
// (POST /agents, POST /agents/{id}, PATCH /agents/{id}).
//
// Three fields, each answering a question an operator otherwise has to infer:
//
//	reader            — who reads (agent | harness), REQUIRED when the block is
//	                    present. It is a closed vocabulary: a reader nobody can
//	                    name is a policy nobody can act on.
//	read_cadence_secs — the interval the consumer expects between reads (0 =
//	                    not stated). Descriptive only: it is reported back and
//	                    never silently substituted for the TTL.
//	ttl_seconds       — the per-agent override of the maximum time an
//	                    unacknowledged message may sit. POINTER so absent is
//	                    distinguishable from an explicit 0:
//	                      absent → the deployment default
//	                               (DefaultConsumerContractTTL / CR_INBOX_CONTRACT_TTL_S)
//	                      0      → NO consumer-contract bound: messages expire
//	                               only by their own message-level expiry — the
//	                               explicit opt-out, documented on the field
//	                      n > 0  → n seconds
type ConsumerContract struct {
	Reader          string `json:"reader"`
	ReadCadenceSecs int    `json:"read_cadence_secs,omitempty"`
	TTLSeconds      *int   `json:"ttl_seconds,omitempty"`
}

// Validate checks the contract at the registration/PATCH boundary. A negative
// or absurd cadence/TTL is refused rather than clamped: a caller that asked for
// a bound the server cannot honor must learn it, not receive a different one.
func (c *ConsumerContract) Validate() error {
	if c == nil {
		return nil
	}
	switch c.Reader {
	case ConsumerReaderAgent, ConsumerReaderHarness:
	case "":
		return errors.New("consumer.reader is required (\"agent\" or \"harness\")")
	default:
		return fmt.Errorf("consumer.reader must be %q or %q, got %q",
			ConsumerReaderAgent, ConsumerReaderHarness, c.Reader)
	}
	if c.ReadCadenceSecs < 0 {
		return fmt.Errorf("consumer.read_cadence_secs must not be negative (%d)", c.ReadCadenceSecs)
	}
	if c.TTLSeconds != nil {
		if *c.TTLSeconds < 0 {
			return fmt.Errorf("consumer.ttl_seconds must not be negative (%d)", *c.TTLSeconds)
		}
		// time.Duration overflows around 292 years; refuse a value that would
		// wrap into a nonsense instant instead of storing it.
		if int64(*c.TTLSeconds) > int64(maxContractTTL/time.Second) {
			return fmt.Errorf("consumer.ttl_seconds must be <= %d", int64(maxContractTTL/time.Second))
		}
	}
	return nil
}

// maxContractTTL bounds the accepted contract TTL (100 years). It exists only
// to reject duration overflow; "effectively forever" is spelled ttl_seconds=0.
const maxContractTTL = 100 * 365 * 24 * time.Hour

// EffectiveTTL resolves the contract's TTL against a deployment default.
//
//	contract nil            → def
//	TTLSeconds nil          → def
//	*TTLSeconds == 0        → 0 (never — the explicit opt-in, never a default)
//	*TTLSeconds > 0         → that many seconds
func (c *ConsumerContract) EffectiveTTL(def time.Duration) time.Duration {
	if c == nil || c.TTLSeconds == nil {
		return def
	}
	if *c.TTLSeconds == 0 {
		return 0
	}
	return time.Duration(*c.TTLSeconds) * time.Second
}

// SetConsumerContractTTL configures the deployment default an inbox contract
// inherits when an agent declares no TTL override (CR_INBOX_CONTRACT_TTL_S).
// A zero value is honoured as "no default contract bound" — it is the explicit
// deployment opt-out, matching the per-agent ttl_seconds=0 spelling — so a
// Handler that was never configured keeps the documented 24h default instead
// of silently disabling expiry.
func (h *Handler) SetConsumerContractTTL(def time.Duration) {
	if def < 0 {
		def = DefaultConsumerContractTTL
	}
	h.consumerContractTTL = def
	h.consumerContractSet = true
}

// resolvedConsumerContractTTL returns the deployment default for this Handler. A
// Handler built without SetConsumerContractTTL (the zero value, and every
// direct test handler) uses the documented default; an explicit zero is
// honoured as the deployment opt-out.
func (h *Handler) resolvedConsumerContractTTL() time.Duration {
	if !h.consumerContractSet {
		return DefaultConsumerContractTTL
	}
	return h.consumerContractTTL
}

// StaleConsumer is one agent whose inbox holds unacknowledged messages older
// than its own contract — the detector's finding, named rather than inferred
// from an age in milliseconds.
type StaleConsumer struct {
	AgentID     string
	Reader      string
	ContractTTL time.Duration
	Unacked     int
	Oldest      time.Time
}

// ContractExpirer is the sweep capability: promote unacknowledged messages past
// their agent's contract TTL into the dead-letter destination, reporting each
// removed entry AFTER the deletion committed so the caller can dead-letter and
// receipt it exactly once (the same contract as PurgeReporter).
//
// defaultTTL is the deployment default for agents that declare no override;
// a contract (or default) of 0 means "no contract bound" and expires nothing.
type ContractExpirer interface {
	ExpireContractOverdue(now time.Time, defaultTTL time.Duration, report func(agentID string, entry *InboxEntry)) int
}

// StaleConsumerReporter is the detector capability: the agents with unacked
// messages older than their contract right now. It is read-only and never
// skips silently — an agent it cannot compute a contract for resolves to the
// deployment default, identically to the expiry sweep.
type StaleConsumerReporter interface {
	StaleConsumers(now time.Time, defaultTTL time.Duration) ([]StaleConsumer, error)
}

// InboxAgingReporter is the stats capability behind `oldest_unacked` and
// `unacked_count`: the count of unacknowledged messages still in the inbox
// (not past their message expiry) and the creation instant of the oldest one.
//
// It is separate from Store.Stats because Stats answers the queue/lease
// question every existing caller already depends on; this answers the
// contract question and must not be inferred from `oldest_age_ms` when a store
// can answer it directly.
type InboxAgingReporter interface {
	OldestUnacked(agentID string) (count int, oldest time.Time, err error)
}

// DeadLetterReasonContractExpired is the dead-letter reason for a message the
// CONSUMER CONTRACT sweep removed (INBOX-CRIER-001). It is distinct from
// DeadLetterReasonExpired ("ttl_expired_unacked", the message's own TTL) so an
// operator can tell "the sender's lifetime ran out" from "the receiver's
// promise ran out" on the same record shape.
const DeadLetterReasonContractExpired = "consumer_contract_expired"

// SweepConsumerContracts runs one detector + expiry pass over the inbox
// contracts and returns the number of messages expired by them.
//
// Ordering: detection runs FIRST, so the log names the stale consumer even on
// the pass that expires its last overdue message. A stale consumer is never
// silently skipped — every finding produces one named log line, and the
// expiry path produces one dead letter plus one receipt per removed message
// through the same sink as the message-TTL purge.
func (h *Handler) SweepConsumerContracts(now time.Time) int {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	defaultTTL := h.resolvedConsumerContractTTL()

	if rep, ok := h.store.(StaleConsumerReporter); ok {
		stale, err := rep.StaleConsumers(now, defaultTTL)
		if err != nil {
			slog.Error("inbox contract detector", "error", err)
		}
		for _, sc := range stale {
			slog.Warn("inbox consumer contract stale",
				"agent", sc.AgentID,
				"reader", sc.Reader,
				"contract_ttl_secs", int64(sc.ContractTTL/time.Second),
				"unacked_count", sc.Unacked,
				"oldest_unacked", sc.Oldest.UTC().Format(time.RFC3339Nano),
				"stale", true,
			)
		}
	}

	exp, ok := h.store.(ContractExpirer)
	if !ok {
		return 0
	}
	expired := 0
	removed := exp.ExpireContractOverdue(now, defaultTTL, func(agentID string, entry *InboxEntry) {
		expired++
		h.expireContractMessage(agentID, entry)
	})
	return removed
}
