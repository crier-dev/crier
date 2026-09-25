package registry

import (
	"net/http"
	"time"
)

// Verdicts the delivery path reports to a Detector (CR-FEAT-030). They are the
// bus's own words for what happened to a delivery, and the detector logs them
// verbatim.
const (
	// VerdictDelivered — the message was stored in the target's inbox (201).
	VerdictDelivered = "delivered"
	// VerdictWebhookAccepted — the message was handed to the target's webhook
	// transport (200 blocking reply or 202 accepted for async/batch).
	VerdictWebhookAccepted = "webhook_accepted"
	// VerdictWebhookFailed — a blocking webhook delivery failed terminally
	// (502 permanent, 504 budget exhausted).
	VerdictWebhookFailed = "webhook_failed"
	// VerdictFederationHeld — the target is unknown here and the delivery was
	// held at the source for bounded retry over a link (202).
	VerdictFederationHeld = "federation_held"
	// VerdictFederationFailed — no link could take the delivery and it could
	// not be held (502).
	VerdictFederationFailed = "federation_failed"
	// VerdictGuardBlocked — the message guard refused the message (403
	// GUARD_BLOCKED).
	VerdictGuardBlocked = "guard_blocked"
	// VerdictQuarantined — the detector's own containment refused the
	// delivery (403 AGENT_QUARANTINED).
	VerdictQuarantined = "quarantined"
	// VerdictAgentNotFound — the target has no registry row anywhere (404).
	VerdictAgentNotFound = "agent_not_found"
	// VerdictRejected — the request itself was refused (400).
	VerdictRejected = "rejected"
	// VerdictRateLimited — the request was well-formed and the target was
	// reachable, but the bus refused to take on more work: the global ingest
	// budget shed it (429 RATE_LIMITED_GLOBAL, CR-FEAT-035). It is its own
	// verdict rather than "rejected" because the delivery was NOT the problem —
	// a retry later is expected to succeed — and an operator reading the log
	// needs to tell a client bug from a saturated bus.
	VerdictRateLimited = "rate_limited"
	// VerdictUnspecified — the handler reached no terminal branch the
	// detector knows; recorded as such rather than guessed.
	VerdictUnspecified = "unspecified"
)

// DeliveryObservation is ONE delivery outcome, as the delivery choke point saw
// it: who claimed to send, whom it was for, which message, what the bus
// decided, over which transport, and the payload (the detector scans it for
// canary tokens).
type DeliveryObservation struct {
	At        time.Time
	Sender    string
	Target    string
	MessageID string
	Verdict   string
	Transport string
	Payload   []byte
}

// Detector is the detection layer's view of the delivery path (CR-FEAT-030).
//
// Two methods, because detection needs exactly two things from the bus: a
// record of every delivery outcome (Observe), and a containment question the
// path must ask BEFORE it delivers anything (Quarantined). Nil is the default
// and means detection is off — every call site is a no-op, so a server without
// CR_DETECT_ENABLED behaves exactly as it did before this existed.
type Detector interface {
	// Observe is called exactly once per delivery request, with the verdict
	// the caller actually received.
	Observe(obs DeliveryObservation)
	// Quarantined reports whether this agent has been contained by the
	// kill-switch. A quarantined agent may neither send nor receive.
	Quarantined(agentID string) bool
}

// SetDetector wires the detection layer into the delivery path.
func (h *Handler) SetDetector(d Detector) { h.detector = d }

// statusRecorder captures the status code the handler wrote, so the
// observation records the verdict the CALLER got rather than a re-derivation
// of the branch taken. It is only installed when a detector is wired, so the
// disabled path keeps the bare ResponseWriter.
type statusRecorder struct {
	http.ResponseWriter
	code int
}

func newStatusRecorder(w http.ResponseWriter) *statusRecorder {
	return &statusRecorder{ResponseWriter: w}
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.code == 0 {
		s.code = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.code == 0 {
		s.code = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

// status returns the recorded status, defaulting to 200 like net/http does for
// a handler that wrote a body without calling WriteHeader.
func (s *statusRecorder) status() int {
	if s.code == 0 {
		return http.StatusOK
	}
	return s.code
}

// deliveryVerdict maps the status the caller received (plus an explicit
// override for the two branches a status code alone cannot distinguish) to the
// documented verdict and transport.
func deliveryVerdict(status int, override string) (verdict, transport string) {
	if override != "" {
		return override, transportForVerdict(override)
	}
	switch status {
	case http.StatusCreated:
		return VerdictDelivered, "inbox"
	case http.StatusAccepted:
		return VerdictWebhookAccepted, "webhook"
	case http.StatusOK:
		return VerdictWebhookAccepted, "webhook"
	case http.StatusNotFound:
		return VerdictAgentNotFound, ""
	case http.StatusBadRequest:
		return VerdictRejected, ""
	case http.StatusForbidden:
		return VerdictGuardBlocked, ""
	case http.StatusBadGateway, http.StatusGatewayTimeout:
		return VerdictWebhookFailed, "webhook"
	default:
		return VerdictUnspecified, ""
	}
}

func transportForVerdict(v string) string {
	switch v {
	case VerdictDelivered:
		return "inbox"
	case VerdictWebhookAccepted, VerdictWebhookFailed:
		return "webhook"
	case VerdictFederationHeld, VerdictFederationFailed:
		return "federation"
	default:
		return ""
	}
}

// quarantinedResponse is the body a contained agent's traffic gets — from
// either side of the delivery.
type quarantinedResponse struct {
	Error string `json:"error"`
	Agent string `json:"agent"`
	Side  string `json:"side"`
	// Detail states the consequence in words, because a 403 an operator reads
	// at 3am should not need the spec open.
	Detail string `json:"detail"`
}
