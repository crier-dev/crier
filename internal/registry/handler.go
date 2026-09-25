package registry

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/mux"

	"github.com/crier-dev/crier/internal/a2a"
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
	// A2A is the optional per-agent half of the A2A gate (INT-A2A-001).
	// Absent is the default and keeps every existing registration byte-
	// identical; present is strictly decoded by strictA2AMember.
	A2A *a2a.Config `json:"a2a,omitempty"`
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
	// IdempotencyKey is the sender's optional deduplication key (CR-FEAT-025):
	// a second delivery of the same key to the same agent within the relay's
	// deduplication window is answered with the FIRST delivery's accept — same
	// message id — and stores nothing, so a sender that retried after losing
	// the connection does not duplicate work. Absent (or empty) means no
	// deduplication, exactly as before.
	IdempotencyKey string `json:"idempotency_key,omitempty"`
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

// The published deliver contract's required `payload` field, as the 400 error
// texts the handler returns (DF-CRIER-112). Kept as constants so the docs
// check and the tests assert the same strings the wire carries.
const (
	// deliverPayloadRequiredError is returned when the key is ABSENT — the
	// client never sent a payload at all (the typo'd `paylaod`/`payloads`
	// case), which used to be accepted and stored as an empty message.
	deliverPayloadRequiredError = "payload is required"
	// deliverPayloadObjectError is returned for anything present-but-not-an-
	// object — `null`, a string, an array, a number, a bool.
	deliverPayloadObjectError = "payload must be a JSON object"
)

// validateDeliverPayload enforces the published contract on the deliver
// request's REQUIRED `payload` field, before any transport or store choice.
//
// docs/openapi.yaml has declared the deliver requestBody `required: [payload]`
// with `payload: type: object` since the request schema existed, and
// cmd/server/openapi.yaml (the generated copy) says the same — but the handler
// decoded the body into a struct whose `json.RawMessage` payload defaults to
// absent for BOTH a missing key and an explicit null, so `{"payloads":{"x":1}}`
// (or `{}`, or `{"payload":5}`) was accepted with 201 and silently stored
// nothing useful (DF-CRIER-112, the same class of contract weakening
// DF-CRIER-180 fixed for delivery_mode/timeout_ms).
//
// This is an HTTP-BOUNDARY contract: store.Deliver keeps accepting an empty
// payload, because internal callers (the MCP bridge, federation, the guard
// notice path) and their tests construct InboxEntry values directly and the
// store never had — and does not gain — an opinion about wire-required keys.
func validateDeliverPayload(req *deliverRequest) error {
	payload := bytes.TrimSpace(req.Payload)
	if len(payload) == 0 {
		return errors.New(deliverPayloadRequiredError)
	}
	// The decoder has already proven the value is syntactically valid JSON
	// (the enclosing object could not have decoded otherwise), so the opening
	// byte decides the JSON TYPE: `{` is an object, and every other value —
	// null, a string, an array, a number, a bool — is refused by the same
	// class of rejection.
	if payload[0] != '{' {
		return errors.New(deliverPayloadObjectError)
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
	// A2A mirrors the registration field (INT-A2A-001): present replaces the
	// block, explicit null clears it, absent leaves it unchanged — the same
	// three-state contract the webhook object uses, so an agent that opted in
	// can opt back out without being deleted and re-registered.
	A2A *a2a.Config `json:"a2a,omitempty"`
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
	// IdempotentReplay marks a blocking accept answered from the recorded
	// accept of an earlier delivery under the same idempotency key
	// (CR-FEAT-025): the endpoint was called ONCE, and `reply` is the reply
	// that call produced.
	IdempotentReplay bool `json:"idempotent_replay,omitempty"`
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
	// ExpiresAt is the RESOLVED message expiry of a stored inbox message, so
	// a sender can see what the requested ttl_seconds actually became. The
	// wire form is tri-state (DF-CRIER-182): ABSENT on the webhook paths,
	// where no inbox entry is created and expiry does not apply; JSON null
	// when the message never expires (ttl_seconds=0 — the zero time
	// internally, DF-CRIER-37); RFC 3339 otherwise.
	ExpiresAt *MessageExpiry `json:"expires_at,omitempty"`
	Guard     *guard.Meta    `json:"guard,omitempty"`
	// IdempotentReplay marks a delivery answered from the recorded accept of
	// an earlier delivery that carried the same idempotency_key (CR-FEAT-025).
	// The body is otherwise the original accept — same id, same status — so a
	// sender that retried can read the id it needs from either response while
	// still being able to tell a replay from a fresh delivery.
	IdempotentReplay bool `json:"idempotent_replay,omitempty"`
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
	// QueueDepth and LeasedCount mirror the counters GET
	// /agents/{id}/inbox/stats reports (DF-CRIER-177): an empty messages
	// array with leased_count > 0 means everything queued is HELD under an
	// unexpired lease, not lost — without them, an all-leased inbox was
	// byte-identical to a genuinely empty one.
	QueueDepth  int `json:"queue_depth"`
	LeasedCount int `json:"leased_count"`
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

// strictWebhookMember strict-decodes the top-level `webhook` member of an
// agent registration/update body (DF-CRIER-150): an unknown or misnamed key
// inside the webhook object is an error instead of being dropped by
// encoding/json. present=false means the body carries no webhook member at
// all; present=true with a nil config means the member was an explicit JSON
// null, which both paths read as "remove the webhook". Strictness covers the
// webhook object only — the surrounding agent body and the guard object stay
// permissive.
func strictWebhookMember(body []byte) (cfg *webhook.Config, present bool, err error) {
	raw, ok := agentWebhookRaw(body)
	if !ok {
		return nil, false, nil
	}
	cfg, err = webhook.DecodeConfig(raw)
	if err != nil {
		return nil, true, err
	}
	return cfg, true, nil
}

// agentWebhookRaw returns the raw JSON of the top-level `webhook` member of an
// agent body. ok=false when the body carries no such member (or is not a JSON
// object). The LAST occurrence wins, mirroring encoding/json's
// later-key-wins decode, and the name is matched case-insensitively like the
// decoder matches the struct field.
func agentWebhookRaw(body []byte) (json.RawMessage, bool) {
	return topLevelMemberRaw(body, "webhook")
}

// strictA2AMember strict-decodes the top-level `a2a` member of an agent
// registration/update body (INT-A2A-001, specs/A2A-OPTION.md §4.2), the same
// discipline strictWebhookMember applies to `webhook`: an unknown or misnamed
// key inside the object is an error instead of being dropped by encoding/json.
// present=false means the body carries no a2a member at all; present=true with
// a nil config means the member was an explicit JSON null, which the update
// path reads as "remove the block".
func strictA2AMember(body []byte) (cfg *a2a.Config, present bool, err error) {
	raw, ok := topLevelMemberRaw(body, "a2a")
	if !ok {
		return nil, false, nil
	}
	cfg, err = a2a.DecodeConfig(raw)
	if err != nil {
		return nil, true, err
	}
	return cfg, true, nil
}

// topLevelMemberRaw returns the raw JSON of one top-level member of an agent
// body, matched case-insensitively like encoding/json matches the struct field
// it will decode into. ok=false when the body carries no such member (or is not
// a JSON object). The LAST occurrence wins, mirroring the decoder's
// later-key-wins rule. It is the single implementation behind agentWebhookRaw
// and strictA2AMember, so the two strict-member scans cannot drift apart.
func topLevelMemberRaw(body []byte, name string) (json.RawMessage, bool) {
	dec := json.NewDecoder(bytes.NewReader(body))
	tok, err := dec.Token()
	if err != nil {
		return nil, false
	}
	if delim, isDelim := tok.(json.Delim); !isDelim || delim != '{' {
		return nil, false
	}
	var (
		raw     json.RawMessage
		present bool
	)
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return nil, false
		}
		key, isString := kt.(string)
		if !isString {
			return nil, false
		}
		var val json.RawMessage
		if err := dec.Decode(&val); err != nil {
			return nil, false
		}
		if strings.EqualFold(key, name) {
			raw, present = val, true
		}
	}
	return raw, present
}

// HandleRegister handles POST /agents — registers a new agent.
func (h *Handler) HandleRegister(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	var req registerRequest
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	// The webhook object is held to its declared contract even though the
	// body around it is decoded permissively (DF-CRIER-150). The optional a2a
	// object is held to the same discipline (INT-A2A-001): a misnamed key
	// inside it is a 400 naming the key, never a silently dropped opt-in.
	if cfg, present, err := strictWebhookMember(body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	} else if present {
		req.Webhook = cfg
	}
	if cfg, present, err := strictA2AMember(body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	} else if present {
		req.A2A = cfg
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
		A2A:          req.A2A,
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

	// The 201 body reports the same DERIVED status every other read of this row
	// reports (CR-FEAT-024). A just-registered row always derives `online`
	// (last_seen is now), so this is consistency rather than a change — but it
	// keeps ONE rule behind every status this API ever emits, instead of a
	// special case a later reader has to know about.
	writeJSON(w, http.StatusCreated, h.presence.Derive(agent, time.Now()))
}

// HandleListAgents handles GET /agents — lists all registered agents.
// With ?capability=abc only agents whose capabilities include abc are
// returned (capability advertisement/discovery, spec §7, CR-FEAT-007).
// Without the parameter the behavior is unchanged.
//
// Every row's status is DERIVED at this read from its own liveness evidence
// (CR-FEAT-024, presence.go) against ONE instant, so a dashboard is told
// `stale` for an agent whose heartbeats stopped instead of `online` forever.
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
	writeJSON(w, http.StatusOK, agentsResponse{Agents: h.presence.DeriveAll(agents, time.Now())})
}

// HandleGetAgent handles GET /agents/{id} — returns agent detail.
// The returned status is derived from the row's liveness evidence, exactly as
// the listing derives it (CR-FEAT-024).
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
	writeJSON(w, http.StatusOK, h.presence.Derive(agent, time.Now()))
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

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	var req patchRequest
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	// Same strict webhook contract as POST /agents (DF-CRIER-150): a
	// misnamed key inside the object is a 400, and the agent is left
	// untouched. Absent or explicit null still means "remove the webhook".
	// The optional a2a object follows the identical three-state rule
	// (INT-A2A-001): present replaces, explicit null removes, absent leaves
	// the agent's opt-in untouched.
	if cfg, present, err := strictWebhookMember(body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	} else if present {
		req.Webhook = cfg
	}
	a2aPresent := false
	if cfg, present, err := strictA2AMember(body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	} else if present {
		req.A2A = cfg
		a2aPresent = true
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
	if a2aPresent {
		// INT-A2A-001: the a2a member PRESENT replaces the block, an explicit
		// JSON null clears it (req.A2A is nil), and an ABSENT member leaves
		// the agent's existing opt-in alone — the three-state rule, expressed
		// through a2aPresent because the webhook-style nil check alone could
		// not tell absent from null.
		agent.A2A = req.A2A
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

	// The 200 body reports the DERIVED status like every other read of this row
	// (CR-FEAT-024): a PATCH just advanced last_seen, so it derives `online`
	// unless the row states `offline`.
	writeJSON(w, http.StatusOK, h.presence.Derive(agent, time.Now()))
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
// POSTed). Guard errors fail open (the delivery proceeds with the errored
// verdict attached) unless the policy is fail_closed. Where that verdict is
// observable depends on the surface (DF-CRIER-175): an inbox delivery answers
// 201 with the verdict in the RESPONSE BODY (`"guard":{…,"errored":true}` —
// there is no guard response header), while the X-Crier-Guard-* headers exist
// only on the OUTBOUND webhook POST.
func (h *Handler) HandleDeliver(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]

	var req deliverRequest

	// DETECTION (CR-FEAT-030): exactly one observation per delivery request,
	// carrying the verdict the caller actually received.
	//
	// The status recorder is the measurement, not a re-derivation of the
	// branch: whatever this handler writes is what the log records, so a
	// branch that returns early (bad JSON, bad payload, a refusal) is logged
	// too — the log's whole point is that it is not selective. verdictOverride
	// carries the two outcomes a status code alone cannot distinguish (the
	// guard's 403 from the detection layer's own 403, and the federation
	// hold/failure pair). Installed only when a detector is wired: with
	// detection off, w is the bare ResponseWriter it always was.
	var verdictOverride string
	var observationID string
	if h.detector != nil {
		rec := newStatusRecorder(w)
		w = rec
		defer func() {
			verdict, transport := deliveryVerdict(rec.status(), verdictOverride)
			h.detector.Observe(DeliveryObservation{
				At:        time.Now().UTC(),
				Sender:    req.Sender,
				Target:    id,
				MessageID: observationID,
				Verdict:   verdict,
				Transport: transport,
				Payload:   req.Payload,
			})
		}()
	}

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

	// The required `payload` key is part of the same contract (DF-CRIER-112):
	// docs/openapi.yaml declares `required: [payload]` + `type: object`, and a
	// body that omits it (or sends null / a non-object) used to be accepted
	// with 201 and stored as an empty message — so a client that mistyped the
	// key got a success it could not act on. Same place as the parameter
	// validation below, for the same reason: the answer cannot depend on the
	// target.
	if err := validateDeliverPayload(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
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

	// DETECTION CHOKE POINT (CR-FEAT-030): a contained agent may neither send
	// nor receive. This runs before the message id is minted, before the
	// federation fallback and before the guard, so a contained agent reaches
	// no transport at all — and the refusal is itself observed (the deferred
	// observation above records it), so containment is visible in the log.
	if h.detector != nil {
		if h.detector.Quarantined(req.Sender) {
			verdictOverride = VerdictQuarantined
			writeJSON(w, http.StatusForbidden, quarantinedResponse{
				Error:  "AGENT_QUARANTINED",
				Agent:  req.Sender,
				Side:   "sender",
				Detail: "this agent is contained; deliveries from it are refused until an operator clears the quarantine",
			})
			return
		}
		if h.detector.Quarantined(id) {
			verdictOverride = VerdictQuarantined
			writeJSON(w, http.StatusForbidden, quarantinedResponse{
				Error:  "AGENT_QUARANTINED",
				Agent:  id,
				Side:   "target",
				Detail: "the target agent is contained; deliveries to it are refused until an operator clears the quarantine",
			})
			return
		}
	}

	// ▼ SENDER-SUPPLIED IDEMPOTENCY (CR-FEAT-025) — before any transport or
	// store choice, for the same reason the validations above are: a duplicate
	// delivery must not reach a webhook endpoint or the inbox store at all.
	// A replay answers with the ORIGINAL accept (same id, same status) so a
	// sender that retried after losing the response learns what its first
	// attempt produced; nothing is stored and nothing is dispatched.
	var attempt *idempotencyAttempt
	if key := req.IdempotencyKey; key != "" {
		if err := validateIdempotencyKey(key); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if h.idempotency != nil {
			rec, outcome, att := h.idempotency.Acquire(id, key, time.Now())
			switch outcome {
			case idempotencyReplay:
				idempotentReplaysTotal.Inc()
				slog.Info("inbox deliver replayed",
					"target", id,
					"sender", req.Sender,
					"message_id", rec.ID,
					"transport", rec.Transport,
					"request_id", middleware.RequestIDFromContext(r.Context()),
				)
				if rec.Blocking {
					// A blocking delivery is request/response: the recorded
					// reply is the answer, and the endpoint was called once.
					writeJSON(w, rec.Status, blockingDeliverResponse{
						ID:               rec.ID,
						Transport:        rec.Transport,
						Reply:            rec.Reply,
						SessionID:        req.SessionID,
						RequestID:        req.RequestID,
						IdempotentReplay: true,
					})
					return
				}
				writeJSON(w, rec.Status, rec.replayResponse())
				return
			case idempotencyBusy:
				// Another delivery under this key is still in flight. Nothing
				// was stored by THIS request, and answering with a fresh
				// delivery would be the duplicate this key exists to prevent.
				writeJSON(w, http.StatusConflict, map[string]string{
					"error": "a delivery with this idempotency_key is already in progress",
				})
				return
			}
			attempt = att
			// Any response that is NOT an accept (a guard block, a federation
			// failure, a store error) releases the key without recording
			// anything, so a corrected retry under the same key is delivered
			// rather than answered with a replay of the rejection.
			defer attempt.Abandon()
		}
	}

	msgID := make([]byte, 12)
	rand.Read(msgID)

	entry := &InboxEntry{
		ID:         hex.EncodeToString(msgID),
		Payload:    req.Payload,
		CreatedAt:  time.Now().UTC(),
		TTLSeconds: req.TTLSeconds,
		// The sender is stored WITH the message (CR-FEAT-025): it is the
		// address a terminal outcome is reported to. Without it a TTL expiry
		// can only be counted, never reported — which is exactly how an
		// expired delivery became a mystery.
		Sender: req.Sender,
		// Provenance for the same reason: a dead letter states which key
		// produced the message it holds.
		IdempotencyKey: req.IdempotencyKey,
	}
	observationID = entry.ID
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
	// and retried inside CR_FED_MAX_HOLD_S (202 Accepted). The bounded 502
	// FEDERATION_FAILED below (this switch's default arm) is returned when no
	// hold queue is available, when the queue refuses the delivery, and when
	// the request names no sender: an unreportable delivery is never held
	// (DF-CRIER-129) — the terminal FEDERATION_FAILED is addressed to the
	// sender, so a 202 "held" could never be followed by any outcome.
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
			verdictOverride = VerdictFederationHeld
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
			// Transient failure that cannot be held: no queue to hold it,
			// a queue that refused it, or a request that names no sender
			// (nothing could receive its terminal FEDERATION_FAILED —
			// DF-CRIER-129). An explicit bounded failure with the
			// correlation context, never a 404.
			verdictOverride = VerdictFederationFailed
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
				// The delivery is FOR the agent in the path, so the target is
				// this handler's own `id` — the one place both the envelope
				// body (crier.target) and the outbound X-Crier-Target header
				// can be sourced from without deriving anything from the
				// webhook URL (DF-CRIER-175, spec §3).
				Target:    id,
				Kind:      kind,
				SessionID: req.SessionID,
				ThreadID:  req.ThreadID,
				Guard:     guardMeta,
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
			// The endpoint answered, so this delivery produced an accept: record it
			// under the sender's key (CR-FEAT-025) BEFORE answering, so a
			// concurrent duplicate that is waiting on the key finds the receipt
			// instead of racing a second call to the endpoint.
			attempt.Finish(idempotentReceipt{
				Status:    http.StatusOK,
				ID:        entry.ID,
				Transport: "webhook",
				Blocking:  true,
				Reply:     reply,
			})
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
		// Queued for push: the accept is recorded under the sender's key
		// (CR-FEAT-025) so a retry does not queue the same work twice.
		attempt.Finish(idempotentReceipt{
			Status:       http.StatusAccepted,
			ID:           entry.ID,
			Transport:    "webhook",
			DeliveryMode: mode,
			Guard:        guardInDeliverResponse(guardMeta),
		})
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

	// The message is durable from here on, so this is the one place that tells
	// the other side of the same write about it (CR-FEAT-023), both
	// fire-and-forget: the reads parked on this agent by a long-poll retrieve
	// are woken so they answer immediately, and an agent that asked for a
	// new-message ping is tapped on its own transport. Neither can fail the
	// delivery — they run on the sender's response path only as a wake-up —
	// and neither touches lease/ack/TTL behaviour.
	h.wakeLongPolls(id)
	h.pingInbox(id, entry.ID, req.Sender)

	slog.Info("inbox deliver accepted",
		"target", id,
		"sender", req.Sender,
		"message_id", entry.ID,
		"transport", "inbox",
		"request_id", middleware.RequestIDFromContext(r.Context()),
	)
	// The expiry is resolved into the tri-state wire type (DF-CRIER-182): a
	// never-expiring message reports null instead of the zero time.
	expiresAt := MessageExpiry(entry.ExpiresAt)
	// The message is stored: record the accept under the sender's key
	// (CR-FEAT-025), so a retry of the same key answers with THIS id instead
	// of storing a second copy of the same work.
	attempt.Finish(idempotentReceipt{
		Status:    http.StatusCreated,
		ID:        entry.ID,
		Transport: "inbox",
		ExpiresAt: &expiresAt,
		Guard:     guardInDeliverResponse(guardMeta),
	})
	writeJSON(w, http.StatusCreated, deliverResponse{
		ID:        entry.ID,
		Transport: "inbox",
		ExpiresAt: &expiresAt,
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

// parseWaitSeconds reads the optional long-poll budget `wait_seconds`
// (CR-FEAT-023). Absent or empty is 0 — the poll-only read every existing
// caller already gets, byte-identical to what this endpoint has always
// answered. A present value is HONORED or REJECTED, never silently ignored
// (DF-CRIER-180): a non-integer, a negative budget and one above
// maxWaitSeconds are all 400s naming the bound, so a caller that mistypes the
// parameter learns it instead of quietly polling.
func parseWaitSeconds(r *http.Request) (time.Duration, error) {
	raw := r.URL.Query().Get("wait_seconds")
	if raw == "" {
		return 0, nil
	}
	secs, err := strconv.Atoi(raw)
	if err != nil {
		return 0, errors.New("wait_seconds must be an integer number of seconds")
	}
	if secs < 0 || secs > maxWaitSeconds {
		return 0, fmt.Errorf("wait_seconds must be 0..%d", maxWaitSeconds)
	}
	return time.Duration(secs) * time.Second, nil
}

// HandleRetrieve handles GET /agents/{id}/inbox — retrieves leased messages.
// Agent-owned: requires a valid per-agent signature when enabled.
//
// Zero claimed messages is a successful read: the body carries
// {"messages":[],"lease_id":""} plus queue_depth / leased_count (DF-CRIER-177)
// and the caller must not ack. A non-empty lease_id is returned exactly when
// messages were leased (DF-CRIER-32).
//
// Long-poll (CR-FEAT-023): `?wait_seconds=N` (0..maxWaitSeconds, default 0)
// parks the read until a message is claimable and then answers with that
// batch, or answers the SAME empty body a poll-only read gets when the budget
// expires. Absent/0 is the unchanged poll-only read — no extra timer, no
// notification subscription — and lease/ack/TTL semantics are identical on
// both paths, because a long-poll is a sequence of ordinary store retrieves.
func (h *Handler) HandleRetrieve(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	if !h.requireAgent(w, r, id) {
		return
	}

	wait, werr := parseWaitSeconds(r)
	if werr != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": werr.Error()})
		return
	}

	// Query spellings: the spec'd `limit`/`lease_seconds`
	// (docs/openapi.yaml) alias the historical `max`/`lease`. All four
	// work. Precedence when both spellings appear in one request: the
	// historical `max`/`lease` wins — the alias fills in only what the
	// historical name left absent, so a request that uses the historical
	// spellings behaves byte-identically to pre-DF-CRIER-177 (DF-CRIER-180
	// family: a documented parameter must never be silently ignored).
	maxMsgs := 10
	if v := r.URL.Query().Get("max"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			maxMsgs = n
		}
	} else if v := r.URL.Query().Get("limit"); v != "" {
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
	} else if v := r.URL.Query().Get("lease_seconds"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			leaseSecs = time.Duration(n) * time.Second
		}
	}

	messages, leaseID, err := h.retrieveWithWait(r.Context(), id, leaseSecs, maxMsgs, wait)
	if err != nil {
		// A client that stopped waiting is not answered: there is nobody
		// left to write to. Every other error keeps its existing mapping —
		// a long-poll never turns a 404 or a 500 into a late empty 200.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return
		}
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

	// Post-retrieve counters (DF-CRIER-177): the same Stats call the
	// /inbox/stats handler makes, taken AFTER the retrieve so the body
	// reflects the state this response just produced (the leased batch is
	// counted as leased, not queued-free). A Stats failure must not fail a
	// successful retrieve: the counters degrade to 0 and the error is
	// logged, mirroring writeStoreError's degradation posture.
	depth, leased, _, err := h.store.Stats(id)
	if err != nil {
		slog.Error("registry stats after retrieve", "error", err)
	}

	writeJSON(w, http.StatusOK, retrieveResponse{
		Messages:    messages,
		LeaseID:     leaseID,
		QueueDepth:  depth,
		LeasedCount: leased,
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
