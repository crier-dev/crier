package registry

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/mux"

	"github.com/crier-dev/crier/internal/federation"
	"github.com/crier-dev/crier/internal/guard"
	"github.com/crier-dev/crier/internal/metrics"
	"github.com/crier-dev/crier/internal/middleware"
	"github.com/crier-dev/crier/internal/webhook"
)

// deliveriesTotal counts accepted inbox deliveries (DF-CRIER-142): one Inc
// per delivery that lands somewhere durable — the inbox store (201) or a
// webhook transport accept (202/200). Rejections (400/403/404/…) do not
// count: they are visible on http_requests_total{code} instead.
var deliveriesTotal = metrics.Default.NewCounter("deliveries_total",
	"Inbox deliveries accepted (stored to inbox or accepted for webhook push).")

// guardDecisionsTotal counts guard verdicts by decision at the choke point
// where the verdict is applied (DF-CRIER-142): allow / block / sanitize —
// the three decisions the pipeline's switch already distinguishes.
var guardDecisionsTotal = metrics.Default.NewCounterVec("guard_decisions_total",
	"LLM message-guard verdicts by decision (allow/block/sanitize).", "decision")

// registerRequest is the JSON body for POST /agents.
type registerRequest struct {
	ID           string                  `json:"id"`
	PublicKey    string                  `json:"public_key"`
	Capabilities []string                `json:"capabilities"`
	Webhook      *webhook.Config         `json:"webhook,omitempty"`
	Guard        *guard.AgentGuardConfig `json:"guard,omitempty"`
}

// agentsResponse is the JSON body for GET /agents.
type agentsResponse struct {
	Agents []*Agent `json:"agents"`
}

// deliverRequest is the JSON body for POST /agents/{id}/inbox.
type deliverRequest struct {
	Payload json.RawMessage `json:"payload"`
	// Sender is the originating agent id (webhook envelope metadata).
	Sender string `json:"sender,omitempty"`
	// SessionID carries conversation context for session-aware delivery
	// (CR-FEAT-004).
	SessionID string `json:"session_id,omitempty"`
	// ThreadID carries thread context for session-aware delivery
	// (CR-FEAT-004) — the deliver-API passthrough of the envelope's
	// thread_id (spec §9.3, CR-FEAT-011); used by the guard's per-channel
	// policy resolution (spec §4.2).
	ThreadID string `json:"thread_id,omitempty"`
	// DeliveryMode overrides the agent's webhook default:
	// blocking | async | batch (CR-FEAT-002/005).
	DeliveryMode string `json:"delivery_mode,omitempty"`
	// TimeoutMs bounds a blocking delivery (default 30000).
	TimeoutMs int `json:"timeout_ms,omitempty"`
	// RequestID is the sender's correlation id, echoed in the reply
	// (tool-call reply contract).
	RequestID string `json:"request_id,omitempty"`
	// Kind is the envelope kind (spec §3, §7): message (default), configure,
	// configure_ack. Passed through to the webhook envelope crier.kind and
	// the X-Crier-Event header untouched (CR-FEAT-007).
	Kind string `json:"kind,omitempty"`
	// TTLSeconds is the requested message lifetime in seconds (documented in
	// openapi.yaml; parsed nowhere until DF-CRIER-37). A POINTER so absent is
	// distinguishable from an explicit 0: absent = the store default (24h),
	// 0 = the message never expires, n > 0 = n seconds.
	TTLSeconds *int `json:"ttl_seconds,omitempty"`
}

// maxTTLSeconds bounds ttl_seconds so the requested lifetime still fits a
// time.Duration (int64 nanoseconds) without overflowing into a negative
// interval. 9223372036s ≈ 292 years.
const maxTTLSeconds = int64(math.MaxInt64) / int64(time.Second)

// maxDeliverTimeoutMs is the ceiling of the request-level blocking budget —
// range parity with the webhook config's own bound
// (webhook.Config.Validate: "webhook.timeout_ms must be 0..120000"), so a
// value the agent's config would be rejected for cannot ride in on a single
// deliver request instead (DF-CRIER-180).
const maxDeliverTimeoutMs = 120000

// defaultDeliverTimeoutMs is the blocking budget a request that states no
// usable timeout_ms (0 or absent) gets, as documented on the deliver schema
// in openapi.yaml.
const defaultDeliverTimeoutMs = 30000

// deliverModes is the closed set of queue semantics a deliver request may ask
// for — the same set webhook.Config.Validate enforces on the agent's own
// webhook config (blocking|async|batch, CR-FEAT-002/005) and the same set
// documented as the request schema's enum in openapi.yaml.
var deliverModes = []string{"blocking", "async", "batch"}

// validateDeliverParameters rejects request-level delivery parameters this
// handler cannot honor, before any transport or store choice is made.
//
//   - delivery_mode: "" keeps today's resolution (the target's webhook
//     default, then "async"); blocking|async|batch are the executable queue
//     semantics. Anything else — the value accepted verbatim in the accept
//     body before DF-CRIER-180 — is a 400 naming the allowed set.
//   - timeout_ms: 0 (or absent) keeps today's meaning on the blocking path
//     (the defaultDeliverTimeoutMs budget); a negative budget or one above
//     maxDeliverTimeoutMs is rejected rather than silently coerced.
//
// The set here matches webhook.Config.Validate on the agent-config path, so
// the two surfaces cannot disagree about what is deliverable.
func validateDeliverParameters(req *deliverRequest) error {
	if req.DeliveryMode != "" && !slices.Contains(deliverModes, req.DeliveryMode) {
		return fmt.Errorf("delivery_mode must be %s", strings.Join(deliverModes, "|"))
	}
	if req.TimeoutMs < 0 || req.TimeoutMs > maxDeliverTimeoutMs {
		return fmt.Errorf("timeout_ms must be 0..%d", maxDeliverTimeoutMs)
	}
	return nil
}

// patchRequest is the JSON body for PATCH /agents/{id} — partial update of
// an agent's registration (spec §7, CR-FEAT-007). capabilities replaces the
// advertised list when present; webhook registers/updates when present and
// is removed when absent or null; guard replaces the whole guard config
// when present, is removed when explicit null, and is UNCHANGED when absent
// (spec §9.2 — RawMessage distinguishes absent from null, CR-FEAT-011).
type patchRequest struct {
	Capabilities []string        `json:"capabilities,omitempty"`
	Webhook      *webhook.Config `json:"webhook,omitempty"`
	Guard        json.RawMessage `json:"guard,omitempty"`
}

// blockingDeliverResponse is returned for delivery_mode=blocking: the
// endpoint's reply, extracted per the agent's schema template.
type blockingDeliverResponse struct {
	ID string `json:"id"`
	// Transport is always "webhook": a blocking delivery exists only on the
	// webhook path, so no inbox entry is created (DF-CRIER-157).
	Transport string          `json:"transport"`
	Reply     json.RawMessage `json:"reply"`
	SessionID string          `json:"session_id,omitempty"`
	RequestID string          `json:"request_id,omitempty"`
}

// deliverResponse is the JSON body for POST /agents/{id}/inbox.
// Guard is present when the verdict was not plain allow (sanitize, or
// errored fail-open) — visibility for async senders (spec §9.3).
type deliverResponse struct {
	ID string `json:"id"`
	// Transport names where the message actually went (DF-CRIER-157):
	// "webhook" when the target agent has a webhook endpoint — such a
	// delivery BYPASSES the durable inbox by design, so a later retrieve is
	// empty — or "inbox" when the message was stored in the agent's durable
	// inbox. Always present, so a sender never has to infer the destination
	// from the status code alone.
	Transport string `json:"transport"`
	// DeliveryMode echoes the webhook queue semantics of a 202 accept:
	// "async" (one queued POST) or "batch" (coalesced into the endpoint's
	// next batch flush). Absent for inbox and blocking deliveries.
	DeliveryMode string `json:"delivery_mode,omitempty"`
	// ExpiresAt is the RESOLVED message expiry (RFC 3339) of a stored inbox
	// message, so a sender can see what the requested ttl_seconds actually
	// became; the zero time (0001-01-01T00:00:00Z) means the message never
	// expires (DF-CRIER-37). Absent on the webhook paths, where no inbox
	// entry is created and expiry does not apply.
	ExpiresAt *time.Time  `json:"expires_at,omitempty"`
	Guard     *guard.Meta `json:"guard,omitempty"`
}

// guardBlockedResponse is the uniform 403 body for blocked deliveries
// (spec §2.1/§9.3) — the same shape in every delivery mode.
type guardBlockedResponse struct {
	Error string     `json:"error"`
	Guard guard.Meta `json:"guard"`
}

// federationHeldResponse is the 202 body when a delivery to a remote agent
// was held at the source because every link failed transiently (DF-CRIER-7,
// spec §8). The delivery is retried for up to max_hold_s; if it still cannot
// be delivered the sender gets a FEDERATION_FAILED notification in its inbox.
// `id` is the assigned message id, the same field the async webhook-accept
// response carries, so a client that only reads `id` keeps working.
type federationHeldResponse struct {
	Status   string `json:"status"` // always "held"
	ID       string `json:"id"`
	Target   string `json:"target"`
	MaxHoldS int    `json:"max_hold_s"`
}

// federationFailureResponse is the stable JSON body for a synchronous
// federation transient failure (HTTP 502): the links are unreachable or
// unhealthy and no hold queue took the delivery. It carries the correlation
// context of the original request and is explicitly NOT an agent-not-found.
type federationFailureResponse struct {
	Error     string `json:"error"` // always FEDERATION_FAILED
	MessageID string `json:"message_id"`
	Target    string `json:"target"`
	Sender    string `json:"sender,omitempty"`
	RequestID string `json:"request_id,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	Attempts  int    `json:"attempts"`
	Status    int    `json:"status,omitempty"`
	Detail    string `json:"detail,omitempty"`
}

// federationFailure builds the 502 body for a transient federation failure.
// The link token is never included (TransientError only carries the link URL
// and the transport error).
func federationFailure(req deliverRequest, messageID, target string, err error) federationFailureResponse {
	resp := federationFailureResponse{
		Error:     federation.CodeFederationFailed,
		MessageID: messageID,
		Target:    target,
		Sender:    req.Sender,
		RequestID: req.RequestID,
		SessionID: req.SessionID,
		Attempts:  1,
		Detail:    err.Error(),
	}
	var transient *federation.TransientError
	if errors.As(err, &transient) {
		if transient.Attempts > 0 {
			resp.Attempts = transient.Attempts
		}
		resp.Status = transient.LastStatus
	}
	return resp
}

// retrieveResponse is the JSON body for GET /agents/{id}/inbox.
type retrieveResponse struct {
	Messages []*InboxEntry `json:"messages"`
	LeaseID  string        `json:"lease_id"`
}

// ackRequest is the JSON body for POST /agents/{id}/inbox/ack.
type ackRequest struct {
	LeaseID    string   `json:"lease_id"`
	MessageIDs []string `json:"message_ids"`
}

// statsResponse is the JSON body for GET /agents/{id}/inbox/stats.
type statsResponse struct {
	QueueDepth  int   `json:"queue_depth"`
	LeasedCount int   `json:"leased_count"`
	OldestAgeMs int64 `json:"oldest_age_ms"`
}

// HandleRegister handles POST /agents — registers a new agent.
func (h *Handler) HandleRegister(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	if req.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id is required"})
		return
	}
	// The public_key PRESENCE requirement follows signature enforcement
	// (DF-CRIER-192): with enforcement on (the default) a keyless agent
	// could never authenticate, so it is rejected up front with the
	// historical 400. With enforcement off — the README dev shortcut
	// (CR_REQUIRE_AGENT_SIG=false) — the key is optional and an omitted
	// key registers a keyless agent (empty key material, no fabrication);
	// it stays unusable on every signed route (agentsig fails closed). A
	// key that IS supplied is validated identically either way.
	var rawKey []byte
	if req.PublicKey == "" {
		if h.requireAgentSig {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "public_key is required"})
			return
		}
	} else {
		decoded, err := hex.DecodeString(req.PublicKey)
		if err != nil || len(decoded) != ed25519.PublicKeySize {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "public_key must be 64 hex characters (ed25519)"})
			return
		}
		rawKey = decoded
	}

	agent := &Agent{
		ID:           req.ID,
		PublicKey:    HexKey(rawKey),
		Capabilities: req.Capabilities,
		Webhook:      req.Webhook,
		Guard:        req.Guard,
	}
	if agent.Capabilities == nil {
		agent.Capabilities = []string{}
	}
	if agent.Webhook != nil {
		if err := agent.Webhook.Validate(); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
	}
	if agent.Guard != nil {
		// LLM message-guard config validation (spec §4.1/§9.2, CR-FEAT-010).
		if err := agent.Guard.Validate(); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
	}

	if err := h.store.Register(agent); err != nil {
		if errors.Is(err, ErrAgentExists) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		} else {
			writeStoreError(w, err)
		}
		return
	}

	writeJSON(w, http.StatusCreated, agent)
}

// HandleListAgents handles GET /agents — lists all registered agents.
// With ?capability=abc only agents whose capabilities include abc are
// returned (capability advertisement/discovery, spec §7, CR-FEAT-007).
// Without the parameter the behavior is unchanged.
func (h *Handler) HandleListAgents(w http.ResponseWriter, r *http.Request) {
	agents := h.store.List()
	if agents == nil {
		agents = []*Agent{}
	}
	if cap := r.URL.Query().Get("capability"); cap != "" {
		filtered := make([]*Agent, 0, len(agents))
		for _, a := range agents {
			for _, c := range a.Capabilities {
				if c == cap {
					filtered = append(filtered, a)
					break
				}
			}
		}
		agents = filtered
	}
	writeJSON(w, http.StatusOK, agentsResponse{Agents: agents})
}

// HandleGetAgent handles GET /agents/{id} — returns agent detail.
func (h *Handler) HandleGetAgent(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	agent, err := h.store.Get(id)
	if err != nil {
		if errors.Is(err, ErrAgentNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		} else {
			writeStoreError(w, err)
		}
		return
	}
	writeJSON(w, http.StatusOK, agent)
}

// HandleUnregister handles DELETE /agents/{id} — removes an agent.
// Agent-owned: requires a valid per-agent signature when enabled.
func (h *Handler) HandleUnregister(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	if !h.requireAgent(w, r, id) {
		return
	}
	if err := h.store.Unregister(id); err != nil {
		if errors.Is(err, ErrAgentNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		} else {
			writeStoreError(w, err)
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// HandleUpdateAgent handles PATCH /agents/{id} — partial update of an
// agent's registration (spec §7, CR-FEAT-007): capabilities replaces the
// advertised list when present; webhook registers/updates when present and
// is removed when absent or null. Agent-owned: requires a valid per-agent
// signature when enabled (same gate as DELETE /agents/{id}).
func (h *Handler) HandleUpdateAgent(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	if !h.requireAgent(w, r, id) {
		return
	}

	var req patchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}

	agent, err := h.store.Get(id)
	if err != nil {
		if errors.Is(err, ErrAgentNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		} else {
			writeStoreError(w, err)
		}
		return
	}

	if req.Capabilities != nil {
		agent.Capabilities = req.Capabilities
	}
	if req.Webhook == nil {
		// Spec §7: webhook absent or null removes the webhook.
		agent.Webhook = nil
	} else {
		if err := req.Webhook.Validate(); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		agent.Webhook = req.Webhook
	}
	if req.Guard != nil {
		// Spec §9.2 (CR-FEAT-011): guard present → replaces the whole
		// guard config; explicit null → guard removed; absent → unchanged
		// (RawMessage nil = absent, so this branch only fires on present).
		trimmed := bytes.TrimSpace(req.Guard)
		if string(trimmed) == "null" {
			agent.Guard = nil
		} else {
			var gc guard.AgentGuardConfig
			if err := json.Unmarshal(trimmed, &gc); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid guard config: " + err.Error()})
				return
			}
			if err := gc.Validate(); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			agent.Guard = &gc
		}
	}

	u, ok := h.store.(updater)
	if !ok {
		writeJSON(w, http.StatusNotImplemented, map[string]string{"error": "registry store does not support agent updates"})
		return
	}
	if err := u.Update(agent); err != nil {
		if errors.Is(err, ErrAgentNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		} else if errors.Is(err, ErrInvalidStoreInput) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		} else {
			writeStoreError(w, err)
		}
		return
	}

	writeJSON(w, http.StatusOK, agent)
}

// HandleDeliver handles POST /agents/{id}/inbox — delivers a message.
// If the target agent has a webhook endpoint configured and the webhook
// driver is enabled, the message is pushed to the endpoint instead of the
// inbox (CR-FEAT-001: webhook is the preferred push surface).
//
// LLM message-guard choke point (spec §2, CR-FEAT-010): when the handler
// has a guard filter and the target agent exists, the message is classified
// AFTER the deliver request is decoded and BEFORE both downstream branches
// (webhook driver, inbox store). Verdict routing (§2.1): allow → deliver as
// today; sanitize → the delivered payload is replaced with the quarantine
// notice and guard metadata rides on the envelope/entry; block → uniform
// 403 GUARD_BLOCKED with the verdict (never queued, never stored, never
// POSTed). Guard errors fail open (deliver, X-Crier-Guard-Error: true)
// unless the policy is fail_closed.
func (h *Handler) HandleDeliver(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]

	var req deliverRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}

	// Requested message lifetime (DF-CRIER-37): absent keeps the store
	// default, 0 means never expires. Reject values that cannot be honored —
	// a negative lifetime and a value that would overflow time.Duration.
	if req.TTLSeconds != nil {
		if *req.TTLSeconds < 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "ttl_seconds must not be negative"})
			return
		}
		if int64(*req.TTLSeconds) > maxTTLSeconds {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": fmt.Sprintf("ttl_seconds must be <= %d", maxTTLSeconds),
			})
			return
		}
	}

	// Request-level delivery parameters must be HONORED or REJECTED, never
	// silently accepted (DF-CRIER-180). An unknown delivery_mode used to be
	// echoed in the accept body as if it were the RESOLVED queue semantics
	// ({"transport":"webhook","delivery_mode":"bogus"}) — the same value is
	// rejected on the agent-registration/PATCH path (webhook.delivery_mode
	// must be blocking|async|batch), so a caller could not tell a typo from a
	// real queue mode — and an out-of-range timeout_ms was taken verbatim as
	// the blocking budget. Validate BEFORE any transport/store choice, so the
	// answer is identical for webhook targets, inbox-only targets and
	// unregistered targets: nothing is dispatched, nothing is stored.
	if err := validateDeliverParameters(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	msgID := make([]byte, 12)
	rand.Read(msgID)

	entry := &InboxEntry{
		ID:         hex.EncodeToString(msgID),
		Payload:    req.Payload,
		CreatedAt:  time.Now().UTC(),
		TTLSeconds: req.TTLSeconds,
	}
	// Resolve the expiry now, from the same instant the store will use, so
	// the response can report what the message's expiry actually became.
	if err := resolveMessageExpiry(entry); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	kind := req.Kind
	if kind == "" {
		kind = webhook.KindMessage
	}

	target, getErr := h.store.Get(id)

	// Federation fallback (CR-FEAT-006, hold/retry DF-CRIER-7): the target
	// agent is not registered on this relay. When relay links are
	// configured, forward the ORIGINAL deliver request to each linked relay
	// in order; the first non-404, non-retryable answer wins and is relayed
	// back verbatim — a blocking webhook reply from the remote relay (which
	// itself rides inside the remote HTTP response) therefore returns to the
	// original sender untouched. Requests that already arrived over a link
	// (hop marker) never forward again.
	// The guard is per-receiver: the REMOTE relay's target-agent policy
	// applies there, so forwarding happens before the local choke point.
	//
	// Failure contract (spec §8): a DEFINITIVE all-links-404 stays
	// agent-not-found, but a TRANSIENT link outage (unreachable / retryable
	// status) is never reported as 404 — the delivery is held at the source
	// and retried inside CR_FED_MAX_HOLD_S (202 Accepted), and only when no
	// hold queue is available (or it refuses the delivery) is an explicit
	// bounded 502 FEDERATION_FAILED returned.
	if h.fed != nil && r.Header.Get(federation.HopHeader) == "" && errors.Is(getErr, ErrAgentNotFound) {
		reqBytes, merr := json.Marshal(req)
		if merr != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "federation: marshal deliver request"})
			return
		}
		status, respBody, held, ferr := h.fed.ForwardOrHold(r.Context(), id, reqBytes, federation.HoldMeta{
			MessageID: entry.ID,
			Sender:    req.Sender,
			RequestID: req.RequestID,
			SessionID: req.SessionID,
		})
		switch {
		case ferr == nil:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			w.Write(respBody)
		case held != nil:
			// Transient link outage: queued at the source for bounded
			// retry. The sender is told the delivery is held, and a
			// terminal FEDERATION_FAILED lands in its inbox if the hold
			// budget expires (asymmetric — never a silent drop, never a
			// misleading 404).
			maxHold := h.fed.MaxHold()
			writeJSON(w, http.StatusAccepted, federationHeldResponse{
				Status:   "held",
				ID:       held.ID,
				Target:   id,
				MaxHoldS: int(maxHold.Seconds()),
			})
		case errors.Is(ferr, federation.ErrNotFoundOnAnyLink):
			// Every linked relay answered 404 (and none failed
			// transiently): the agent is nowhere in the federation — the
			// caller sees the same 404 a single relay would answer.
			writeJSON(w, http.StatusNotFound, map[string]string{"error": ErrAgentNotFound.Error()})
		default:
			// Transient failure with no hold queue to hold it: an explicit
			// bounded failure with the correlation context, never a 404.
			writeJSON(w, http.StatusBadGateway, federationFailure(req, entry.ID, id, ferr))
		}
		return
	}

	// ▼ GUARD CHOKE POINT (spec §2) — one call site covers webhook
	// (blocking/async/batch) AND inbox store. Per-message verdicts are
	// computed exactly once; redelivery/batch flush never re-run the guard.
	var guardMeta *guard.Meta
	payload := req.Payload
	if h.guard != nil && target != nil {
		res, gerr := h.guard.Check(r.Context(), id, target.Guard, guard.Input{
			AgentID:   id,
			MessageID: entry.ID,
			Sender:    req.Sender,
			SessionID: req.SessionID,
			ThreadID:  req.ThreadID,
			Kind:      kind,
			Payload:   payload,
		})
		if gerr != nil {
			// Misconfiguration (never provider failure — those are folded
			// into Result.Errored and resolved per §3.6): fail open with an
			// audit line (spec §2.2).
			slog.Warn("guard: check failed, delivering without guard metadata",
				"error", gerr, "target", id, "message_id", entry.ID)
		} else {
			switch res.Decision {
			case guard.DecisionBlock:
				// Uniform 403 across ALL modes (spec §2.1): the sender
				// learns immediately that the message was not accepted.
				guardDecisionsTotal.With("block").Inc()
				writeJSON(w, http.StatusForbidden, guardBlockedResponse{
					Error: "GUARD_BLOCKED",
					Guard: res.Meta(),
				})
				return
			case guard.DecisionSanitize:
				// Deterministic quarantine (§3.5): deliver the notice,
				// original rides in crier.guard.quarantined_payload.
				guardDecisionsTotal.With("sanitize").Inc()
				if len(res.DeliveredPayload) > 0 {
					payload = res.DeliveredPayload
				}
				m := res.Meta()
				guardMeta = &m
			default:
				guardDecisionsTotal.With("allow").Inc()
				m := res.Meta()
				guardMeta = &m
			}
		}
	}

	if h.webhooks != nil && target != nil && target.Webhook != nil {
		mode := req.DeliveryMode
		if mode == "" {
			mode = target.Webhook.DeliveryMode
		}
		if mode == "" {
			mode = "async"
		}
		env := &webhook.Envelope{
			Crier: webhook.EnvelopeMeta{
				Version:      1,
				MessageID:    entry.ID,
				RequestID:    req.RequestID,
				DeliveryMode: mode,
				Sender:       req.Sender,
				Kind:         kind,
				SessionID:    req.SessionID,
				ThreadID:     req.ThreadID,
				Guard:        guardMeta,
			},
			Payload: payload,
		}
		if mode == "blocking" {
			budget := time.Duration(req.TimeoutMs) * time.Millisecond
			if req.TimeoutMs <= 0 {
				budget = defaultDeliverTimeoutMs * time.Millisecond
			}
			reply, err := h.webhooks.DeliverBlocking(r.Context(), id, target.Webhook, env, budget)
			if err != nil {
				// 502 vs 504 (DF-CRIER-157): a PERMANENT endpoint rejection —
				// a non-retryable status, or a 2xx whose body the agent's reply
				// schema cannot map — can never succeed on retry, so it is a
				// Bad Gateway. A timeout / budget exhaustion is a Gateway
				// Timeout: the same delivery may still land later.
				if errors.Is(err, webhook.ErrPermanent) {
					writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
					return
				}
				writeJSON(w, http.StatusGatewayTimeout, map[string]string{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusOK, blockingDeliverResponse{
				ID:        entry.ID,
				Transport: "webhook",
				Reply:     reply,
				SessionID: req.SessionID,
				RequestID: req.RequestID,
			})
			deliveriesTotal.Inc()
			return
		}
		// DeliverContext carries the request's correlation id into the
		// driver so the background webhook dispatch/outcome log lines can be
		// joined to this request (DF-CRIER-141).
		if _, err := h.webhooks.DeliverContext(r.Context(), id, target.Webhook, env); err != nil {
			// Queue path handled inside the driver; the deliver call
			// itself only fails on config errors — surface those.
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		// Async/batch webhook delivery is fire-and-forget: the sender gets
		// 202 Accepted, delivery happens in the background queue (spec §4).
		// The accept names where the message went (transport=webhook — the
		// durable inbox is bypassed, so a later retrieve is empty) and which
		// queue semantics apply (delivery_mode=async|batch), so a sender is
		// never left inferring that from the status code alone
		// (DF-CRIER-157). The accept is logged with the correlation context
		// (DF-CRIER-141) so the sender's request can be joined to the
		// background dispatch and outcome lines the webhook driver emits.
		slog.Info("inbox deliver accepted",
			"target", id,
			"sender", req.Sender,
			"message_id", entry.ID,
			"mode", mode,
			"transport", "webhook",
			"request_id", middleware.RequestIDFromContext(r.Context()),
		)
		writeJSON(w, http.StatusAccepted, deliverResponse{
			ID:           entry.ID,
			Transport:    "webhook",
			DeliveryMode: mode,
			Guard:        guardInDeliverResponse(guardMeta),
		})
		deliveriesTotal.Inc()
		return
	}

	// Inbox store branch: the entry carries the (possibly quarantined)
	// payload and the guard metadata (spec §2.2/§9.3).
	entry.Payload = payload
	entry.Guard = guardMeta
	if err := h.store.Deliver(id, entry); err != nil {
		if errors.Is(err, ErrAgentNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		} else if errors.Is(err, ErrInvalidStoreInput) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		} else {
			writeStoreError(w, err)
		}
		return
	}

	slog.Info("inbox deliver accepted",
		"target", id,
		"sender", req.Sender,
		"message_id", entry.ID,
		"transport", "inbox",
		"request_id", middleware.RequestIDFromContext(r.Context()),
	)
	writeJSON(w, http.StatusCreated, deliverResponse{
		ID:        entry.ID,
		Transport: "inbox",
		ExpiresAt: &entry.ExpiresAt,
		Guard:     guardInDeliverResponse(guardMeta),
	})
	deliveriesTotal.Inc()
}

// guardInDeliverResponse surfaces guard metadata on success responses
// whenever the verdict carries anything the caller should see (spec §9.3:
// visibility for async senders, DF-CRIER-158). It stays absent ONLY for a
// clean pass: decision allow, no error, risk low, no matched patterns.
//
// A risk-MARKED allow is not a clean pass. The §6.3 oversize fast path
// answers `allow` with risk medium and `patterns: ["oversize"]` precisely
// so the marker is visible, and error-path verdicts now carry the
// deterministic prematch evidence; dropping those made an allow-with-risk
// indistinguishable from a clean allow on the wire (while fail-open was
// surfaced). A nil meta (guard disabled) also stays absent.
func guardInDeliverResponse(m *guard.Meta) *guard.Meta {
	if m == nil {
		return nil
	}
	if m.Decision == guard.DecisionAllow && !m.Errored &&
		m.RiskLevel == guard.RiskLow && len(m.Patterns) == 0 {
		return nil
	}
	return m
}

// HandleRetrieve handles GET /agents/{id}/inbox — retrieves leased messages.
// Agent-owned: requires a valid per-agent signature when enabled.
//
// Zero claimed messages is a successful read: the body carries
// {"messages":[],"lease_id":""} and the caller must not ack. A non-empty
// lease_id is returned exactly when messages were leased (DF-CRIER-32).
func (h *Handler) HandleRetrieve(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	if !h.requireAgent(w, r, id) {
		return
	}

	maxMsgs := 10
	if v := r.URL.Query().Get("max"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			maxMsgs = n
		}
	}
	// Cap max at 100 per spec §9
	if maxMsgs > 100 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "max must be <= 100"})
		return
	}

	leaseSecs := 30 * time.Second
	if v := r.URL.Query().Get("lease"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			leaseSecs = time.Duration(n) * time.Second
		}
	}

	messages, leaseID, err := h.store.Retrieve(id, leaseSecs, maxMsgs)
	if err != nil {
		if errors.Is(err, ErrAgentNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		} else if errors.Is(err, ErrInvalidStoreInput) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		} else {
			writeStoreError(w, err)
		}
		return
	}
	if messages == nil {
		messages = []*InboxEntry{}
	}

	writeJSON(w, http.StatusOK, retrieveResponse{
		Messages: messages,
		LeaseID:  leaseID,
	})
}

// HandleAck handles POST /agents/{id}/inbox/ack — acknowledges messages.
// Agent-owned: requires a valid per-agent signature when enabled.
//
// Failure classes are distinct (DF-CRIER-32): 404 when a requested message ID
// does not exist in the inbox, 409 when it exists but is leased under a
// different lease, 400 for a malformed request.
func (h *Handler) HandleAck(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	if !h.requireAgent(w, r, id) {
		return
	}

	var req ackRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	if req.LeaseID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "lease_id is required"})
		return
	}
	if len(req.MessageIDs) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "message_ids is required (ack without message IDs is a silent no-op)"})
		return
	}

	if err := h.store.Ack(id, req.LeaseID, req.MessageIDs); err != nil {
		switch {
		case errors.Is(err, ErrAgentNotFound):
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		case errors.Is(err, ErrMessageNotFound):
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		case errors.Is(err, ErrLeaseConflict):
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		case errors.Is(err, ErrInvalidStoreInput):
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		default:
			writeStoreError(w, err)
		}
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// HandleStats handles GET /agents/{id}/inbox/stats — returns queue stats.
// Agent-owned: requires a valid per-agent signature when enabled.
func (h *Handler) HandleStats(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	if !h.requireAgent(w, r, id) {
		return
	}

	depth, leased, age, err := h.store.Stats(id)
	if err != nil {
		if errors.Is(err, ErrAgentNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		} else {
			writeStoreError(w, err)
		}
		return
	}

	writeJSON(w, http.StatusOK, statsResponse{
		QueueDepth:  depth,
		LeasedCount: leased,
		OldestAgeMs: age.Milliseconds(),
	})
}

// writeStoreError logs a store-level error and returns a generic 500 to the client.
func writeStoreError(w http.ResponseWriter, err error) {
	slog.Error("registry store error", "error", err)
	writeJSON(w, http.StatusInternalServerError, map[string]string{
		"error": "registry storage unavailable",
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
