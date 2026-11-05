package registry

import "github.com/crier-dev/crier/internal/hermesession"

// Hermes-session delivery (specs/HERMES-SESSION-DELIVERY.md).
//
// A third delivery transport beside the durable inbox and the webhook push:
// the message becomes ONE agent turn inside a Hermes gateway session, created
// or reused according to the agent's configured policy. Like a webhook accept
// it BYPASSES the durable inbox — a later retrieve is empty — and like a
// blocking webhook it is request/response, so the accept carries the turn's
// reply.
//
// The transport owns the session lifecycle and the replay window; this package
// owns the delivery contract around it: the guard runs before it exactly as it
// does for every other transport, the sender's idempotency_key records the
// accept, and a transport failure is answered with the transport's own named
// code and status rather than flattened into a webhook 502.

// hermesDeliveryFailure is the JSON body of a refused session delivery. The
// named code is in `error` (the same shape NO_CAPABLE_AGENT uses), the
// transport's own detail is in `detail`, and gateway_status is the gateway's
// HTTP status VERBATIM when an exchange happened — 0/absent when it did not.
type hermesDeliveryFailure struct {
	Error         string `json:"error"`
	Detail        string `json:"detail,omitempty"`
	GatewayStatus int    `json:"gateway_status,omitempty"`
}

// hermesTransportUnavailable is the named refusal of a session-configured
// agent on a relay that never wired the transport. It is deliberately loud:
// silently falling back to the inbox would deliver a message somewhere the
// agent's configuration does not name.
const hermesTransportUnavailable = "HERMES_SESSION_TRANSPORT_UNAVAILABLE"

// SetHermesSessionClient installs the Hermes-session delivery transport. Nil
// (the default) leaves every delivery byte-identical to a relay without it,
// and a session-configured agent is then refused loudly rather than delivered
// to a transport it did not ask for.
func (h *Handler) SetHermesSessionClient(c *hermesession.Client) { h.hermes = c }
