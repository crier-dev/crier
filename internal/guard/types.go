// Package guard implements the LLM message-guard choke point for Crier
// deliveries: an OpenAI-compatible LLM classifies every inbound message
// (allow / block / sanitize) before it reaches a receiving agent's webhook
// endpoint or durable inbox.
//
// Spec: specs/LLM-MESSAGE-GUARD.md (CR-SPEC-002). Ticket: CR-FEAT-010.
// The guard is inbound to the receiving agent and applies at ONE choke
// point (internal/registry Handler.HandleDeliver) which covers every
// delivery mode that lands content in a receiver's context.
package guard

import "errors"

// Decision is the guard verdict action (spec §3.1).
type Decision string

const (
	DecisionAllow    Decision = "allow"
	DecisionBlock    Decision = "block"
	DecisionSanitize Decision = "sanitize"
)

// RiskLevel is the verdict risk tier (spec §3.1).
type RiskLevel string

const (
	RiskLow    RiskLevel = "low"
	RiskMedium RiskLevel = "medium"
	RiskHigh   RiskLevel = "high"
)

// Verdict is the LLM-returned, schema-validated verdict (wire §3.1).
type Verdict struct {
	Decision        Decision  `json:"decision"`
	RiskLevel       RiskLevel `json:"risk_level"`
	Reason          string    `json:"reason"`
	MatchedPatterns []string  `json:"matched_patterns"`
}

// Result is the full guard outcome for one message (envelope crier.guard,
// spec §3.7).
type Result struct {
	Decision    Decision  `json:"decision"`
	RiskLevel   RiskLevel `json:"risk_level"`
	Reason      string    `json:"reason"`
	Patterns    []string  `json:"matched_patterns,omitempty"`
	PolicyID    string    `json:"policy,omitempty"`
	Provider    string    `json:"provider,omitempty"`
	Model       string    `json:"model,omitempty"`
	Errored     bool      `json:"errored,omitempty"`
	Quarantined bool      `json:"quarantined,omitempty"`
	DurationMs  int64     `json:"duration_ms,omitempty"`
	// MessageID is the guarded message's id (from Input). Internal
	// plumbing: surfaced on Meta.MessageID (spec §8.2); never serialized
	// on Result itself (queue items carry their own message id).
	MessageID string `json:"-"`
	// DeliveredPayload is the payload to deliver IN PLACE of the original
	// when the message was sanitized (spec §3.5 notice object). Internal
	// plumbing — the handler applies it; never serialized.
	DeliveredPayload []byte `json:"-"`
	// QuarantinedPayload is base64(std) of the ORIGINAL payload when the
	// message was sanitized (spec §3.5 step 1). Empty otherwise. Internal
	// on Result; surfaced on Meta (wire).
	QuarantinedPayload string `json:"-"`
}

// Meta is the per-message guard metadata carried on the envelope / inbox
// entry (wire contract addition, spec §3.7).
type Meta struct {
	// MessageID is the guarded message's id (spec §8.2: populated on the
	// envelope path). Carried on cards, envelope crier.guard metadata,
	// inbox entries and the 403 body.
	MessageID   string    `json:"message_id,omitempty"`
	Decision    Decision  `json:"decision"`
	RiskLevel   RiskLevel `json:"risk_level"`
	Reason      string    `json:"reason"`
	Patterns    []string  `json:"matched_patterns,omitempty"`
	Policy      string    `json:"policy,omitempty"`
	Provider    string    `json:"provider,omitempty"`
	Model       string    `json:"model,omitempty"`
	Errored     bool      `json:"errored,omitempty"`
	Quarantined bool      `json:"quarantined,omitempty"`
	// QuarantinedPayload is base64(std) of the original payload when the
	// message was sanitized (decision=sanitize). Empty otherwise.
	QuarantinedPayload string `json:"quarantined_payload,omitempty"`
}

// Meta converts a Result into the wire metadata form (spec §3.7).
func (r Result) Meta() Meta {
	return Meta{
		MessageID:          r.MessageID,
		Decision:           r.Decision,
		RiskLevel:          r.RiskLevel,
		Reason:             r.Reason,
		Patterns:           r.Patterns,
		Policy:             r.PolicyID,
		Provider:           r.Provider,
		Model:              r.Model,
		Errored:            r.Errored,
		Quarantined:        r.Quarantined,
		QuarantinedPayload: r.QuarantinedPayload,
	}
}

// Result converts wire metadata back into a Result (queue-item verdict
// carry, spec §2.2). DurationMs is not on the wire and resets to 0.
func (m Meta) Result() Result {
	return Result{
		MessageID:          m.MessageID,
		Decision:           m.Decision,
		RiskLevel:          m.RiskLevel,
		Reason:             m.Reason,
		Patterns:           m.Patterns,
		PolicyID:           m.Policy,
		Provider:           m.Provider,
		Model:              m.Model,
		Errored:            m.Errored,
		Quarantined:        m.Quarantined,
		QuarantinedPayload: m.QuarantinedPayload,
	}
}

// Input is the per-message context the choke point needs (spec §2). It is
// guard-defined rather than webhook.Envelope so the guard package stays
// free of the webhook import: webhook.EnvelopeMeta carries guard.Meta, so
// webhook imports guard — a guard→webhook dependency would cycle.
type Input struct {
	AgentID   string
	MessageID string
	Sender    string
	SessionID string
	ThreadID  string
	Kind      string
	Payload   []byte
}

// Sentinel errors (spec §5.3 / §5.4).
var (
	// ErrProvider is returned by the client for HTTP/network/timeout/4xx-5xx
	// failures (except 400 on response_format/thinking, which is
	// ErrModelRejected).
	ErrProvider = errors.New("guard: provider error")
	// ErrModelRejected is returned for a 400 on the response_format or
	// thinking fields — treated as a provider failure by the router (the
	// client MUST NOT downgrade to unconstrained output, spec §3.2).
	ErrModelRejected = errors.New("guard: model rejected request")
	// ErrAllProvidersFailed is returned by the router when every provider
	// in the policy chain failed; the caller assembles the guard-error
	// result per §3.6.
	ErrAllProvidersFailed = errors.New("guard: all providers failed")
	// ErrConcurrencySaturated is returned when the concurrency semaphore
	// cannot be acquired within 2s (spec §5.4).
	ErrConcurrencySaturated = errors.New("guard: concurrency saturated")
)
