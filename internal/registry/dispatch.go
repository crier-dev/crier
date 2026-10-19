package registry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/crier-dev/crier/internal/guard"
	"github.com/crier-dev/crier/internal/middleware"
	"github.com/crier-dev/crier/internal/namespace"
	"github.com/crier-dev/crier/internal/webhook"
)

// DeliveryRequest carries a delivery intent and its transport context, not an
// alternative inbox write. The entry owns the durable message identity and
// payload. HTTP authorization remains at ingress; non-HTTP dispatch enforces
// the target namespace's auth using NamespaceToken.
type DeliveryRequest struct {
	Entry        *InboxEntry
	SessionID    string
	RequestID    string
	Kind         string
	DeliveryMode string
	TimeoutMs    int
	Namespace    string
	// NamespaceToken is the caller's namespace credential, used only for
	// authorization; it must never be persisted or forwarded to a webhook.
	NamespaceToken string `json:"-"`
	httpState      *dispatchHTTPState
}

// dispatchHTTPState preserves the HTTP ingress's resolved authorization and
// idempotency attempt without exposing either as caller-controlled wire fields.
type dispatchHTTPState struct {
	request    deliverRequest
	attempt    *idempotencyAttempt
	capability string
	authority  *ServiceAuthority
	started    time.Time
	skipGuard  bool
}

// DeliveryResult describes the transport that accepted the entry. A webhook
// accept does not imply an inbox write. StatusCode preserves the existing HTTP
// status contract; Guard also includes clean allow metadata used by the store.
type DeliveryResult struct {
	Entry        *InboxEntry
	Guard        *guard.Meta
	Transport    string
	DeliveryMode string
	Reply        json.RawMessage
	StatusCode   int
	response     any
	receipt      *ServiceDeliveryReceipt
}

// DispatchError is a refusal or transport/store failure. Its status and body are
// the original HTTP contract, while Error is usable by non-HTTP clients.
type DispatchError struct {
	StatusCode int
	Response   any
	Cause      error // structured transport evidence for non-HTTP consumers
}

func (e *DispatchError) Unwrap() error { return e.Cause }

func (e *DispatchError) Error() string {
	b, _ := json.Marshal(e.Response)
	return fmt.Sprintf("delivery failed (%d): %s", e.StatusCode, b)
}

func (r DeliveryResult) writeHTTP(w http.ResponseWriter) {
	if r.receipt != nil {
		writeServiceReceipt(w, r.receipt)
		return
	}
	writeJSON(w, r.StatusCode, r.response)
}

// Dispatch is the single guard -> webhook OR durable inbox choke point. HTTP
// callers authorize at ingress; non-HTTP callers are checked against the target's
// namespace here. Only this method performs transport side effects.
// An error always means not dispatched, including guard refusal and webhook
// rejection. Queue acceptance is distinguished from blocking completion by mode.
func (h *Handler) Dispatch(ctx context.Context, target *Agent, delivery DeliveryRequest) (result DeliveryResult, dispatchErr error) {
	entry := delivery.Entry
	respond := func(status int, body any) {
		result.StatusCode, result.response = status, body
		switch b := body.(type) {
		case deliverResponse:
			result.Transport, result.DeliveryMode = b.Transport, b.DeliveryMode
		case blockingDeliverResponse:
			result.Transport, result.DeliveryMode, result.Reply = b.Transport, "blocking", b.Reply
		}
		if status >= 400 {
			dispatchErr = &DispatchError{StatusCode: status, Response: body}
		}
	}
	storeFailure := func(err error) {
		slog.Error("registry store error", "error", err)
		respond(http.StatusInternalServerError, map[string]string{"error": "registry storage unavailable"})
	}
	if err := ctx.Err(); err != nil {
		respond(http.StatusGatewayTimeout, map[string]string{"error": err.Error()})
		return
	}
	if entry == nil {
		respond(http.StatusBadRequest, map[string]string{"error": "nil delivery entry"})
		return
	}
	result.Entry = entry
	req := deliverRequest{Payload: entry.Payload, Sender: entry.Sender,
		ThreadID: entry.ThreadID, SessionID: delivery.SessionID, RequestID: delivery.RequestID,
		Kind: delivery.Kind, DeliveryMode: delivery.DeliveryMode, TimeoutMs: delivery.TimeoutMs,
		IdempotencyKey: entry.IdempotencyKey, TTLSeconds: entry.TTLSeconds, Namespace: delivery.Namespace,
		PrincipalID: entry.PrincipalID, Model: entry.Model, Provider: entry.Provider, ReplyTo: entry.ReplyTo,
	}
	var attempt *idempotencyAttempt
	var capability string
	var serviceAuthority *ServiceAuthority
	deliverStart := time.Now()
	if st := delivery.httpState; st != nil {
		req, attempt, capability, serviceAuthority, deliverStart = st.request, st.attempt, st.capability, st.authority, st.started
	}
	id := entry.AgentID
	if target != nil {
		id = target.ID
	}
	if target == nil && delivery.httpState == nil {
		respond(http.StatusNotFound, map[string]string{"error": ErrAgentNotFound.Error()})
		return
	}
	nsPolicy := h.namespaces.Resolve(entry.Namespace)
	if delivery.httpState == nil {
		// A session send carries no signed service authority. In particular,
		// a protected service with a webhook must not bypass the store's gate.
		if secured, ok := h.store.(*AuthorityStore); ok {
			if err := secured.validateAuthority(id, entry, nil); err != nil {
				respond(http.StatusForbidden, map[string]string{"error": err.Error()})
				return
			}
		}
		if _, protected := h.serviceDeliveries[id]; protected {
			respond(http.StatusForbidden, map[string]string{"error": "service delivery requires signed service authority"})
			return
		}
		if err := validateDeliveryTTL(req.TTLSeconds); err != nil {
			respond(http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if err := validateDeliverPayload(&req); err != nil {
			respond(http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if err := validateDeliverParameters(&req); err != nil {
			respond(http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		targetNS := namespaceOfAgent(target)
		if req.Namespace != "" && namespace.Canonical(req.Namespace) != targetNS {
			respond(http.StatusForbidden, namespaceMismatchResponse(id, targetNS, req.Namespace))
			return
		}
		if err := h.namespaces.CheckAuth(targetNS, delivery.NamespaceToken); err != nil {
			status, body := namespaceErrorResponse(err)
			respond(status, body)
			return
		}
		entry.Namespace = targetNS
		logNamespaceDeliver(id, targetNS, req.Sender)
		if entry.Location == nil {
			entry.Location = h.locationOf(entry, req.SessionID)
		} else {
			loc := *entry.Location
			loc.Instance, loc.Namespace = h.instanceName, targetNS
			entry.Location = &loc
		}
		nsPolicy = h.namespaces.Resolve(targetNS)
		if req.TTLSeconds == nil && nsPolicy.RetentionSeconds != nil {
			ttl := int(*nsPolicy.RetentionSeconds)
			entry.TTLSeconds = &ttl
		}
		if entry.CreatedAt.IsZero() {
			entry.CreatedAt = time.Now().UTC()
		}
		if err := resolveMessageExpiry(entry); err != nil {
			respond(http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if h.detector != nil && (h.detector.Quarantined(req.Sender) || h.detector.Quarantined(id)) {
			respond(http.StatusForbidden, map[string]string{"error": "AGENT_QUARANTINED"})
			return
		}
		if err := validateIdempotencyKey(req.IdempotencyKey); req.IdempotencyKey != "" && err != nil {
			respond(http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if req.IdempotencyKey != "" && h.idempotency != nil {
			rec, outcome, att := h.idempotency.Acquire(id, req.IdempotencyKey, time.Now())
			switch outcome {
			case idempotencyReplay:
				entry.ID = rec.ID
				result.Guard = rec.Guard
				if rec.Blocking {
					respond(rec.Status, blockingDeliverResponse{ID: rec.ID, Transport: rec.Transport, Reply: rec.Reply, SessionID: req.SessionID, RequestID: req.RequestID, IdempotentReplay: true})
				} else {
					respond(rec.Status, rec.replayResponse())
				}
				return
			case idempotencyBusy:
				respond(http.StatusConflict, map[string]string{"error": "a delivery with this idempotency_key is already in progress"})
				return
			}
			attempt = att
			defer attempt.Abandon()
		}
	}
	kind := req.Kind
	if kind == "" {
		kind = webhook.KindMessage
	}
	// accept records an accept under the sender's key, stamping the routing
	// provenance of a capability-routed delivery onto the receipt (CR-FEAT-026)
	// so a REPLAY of it names the same capability and the same holder. nil
	// attempt (no idempotency key) is a no-op — the same tolerance the
	// deferred Abandon() relies on.
	accept := func(rec idempotentReceipt) {
		if capability != "" {
			rec.Capability = capability
			rec.Target = id
		}
		attempt.Finish(rec)
	}

	// ▼ GUARD CHOKE POINT (spec §2) — one call site covers webhook
	// (blocking/async/batch) AND inbox store. Per-message verdicts are
	// computed exactly once; redelivery/batch flush never re-run the guard.
	//
	// REALMS (CR-FEAT-029): the namespace can skip this choke point entirely
	// (guard_enabled=false) or supply the realm-level default policy. That is
	// what keeps one realm's traffic out of another realm's guard budget — the
	// guard's concurrency cap and circuit breaker are process-wide, so without
	// a per-namespace switch a flood in one realm would stop every other realm
	// being checked. An agent's own guard.policies still outrank the realm
	// default, per the guard spec's own specificity order (§4.2).
	var guardMeta *guard.Meta
	defer func() { result.Guard = guardMeta }()
	payload := req.Payload
	guardSkipped := nsPolicy.GuardSkipped()
	if st := delivery.httpState; st != nil {
		// HTTP ingress already resolved the target's namespace and its valve.
		guardSkipped = st.skipGuard
	}
	if h.guard != nil && target != nil && !guardSkipped {
		guardCfg := target.Guard
		if guardCfg == nil && nsPolicy.GuardPolicy != nil {
			guardCfg = &guard.AgentGuardConfig{Policies: []guard.Policy{*nsPolicy.GuardPolicy}}
		}
		res, gerr := h.guard.Check(ctx, id, guardCfg, guard.Input{
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
				m := res.Meta()
				guardMeta = &m
				// Uniform 403 across ALL modes (spec §2.1): the sender
				// learns immediately that the message was not accepted.
				guardDecisionsTotal.With("block").Inc()
				respond(http.StatusForbidden, guardBlockedResponse{
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
		result.Transport, result.DeliveryMode = "webhook", mode
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
				// The realm the delivery belongs to (CR-FEAT-029) — the
				// target's own, resolved above from its registry row. Omitted
				// entirely for the default namespace.
				Namespace: entry.Namespace,
				// The HUMAN behind the send (CR-CHAT-007, §6.3), riding
				// alongside the sender exactly as the stored entry carries
				// it. Omitted for anonymous and agent-initiated deliveries.
				PrincipalID: entry.PrincipalID,
			},
			Payload: payload,
		}
		if mode == "blocking" {
			budget := time.Duration(req.TimeoutMs) * time.Millisecond
			if req.TimeoutMs <= 0 {
				budget = defaultDeliverTimeoutMs * time.Millisecond
			}
			reply, err := h.webhooks.DeliverBlocking(ctx, id, target.Webhook, env, budget)
			if err != nil {
				// Keep attempts/budget/cut evidence across the HTTP-shaped error;
				// session fan-out must not classify a flattened 504 by string.
				defer func() {
					var dispatch *DispatchError
					if errors.As(dispatchErr, &dispatch) {
						dispatch.Cause = err
					}
				}()
				// 502 vs 504 (DF-CRIER-157): a PERMANENT endpoint rejection —
				// a non-retryable status, or a 2xx whose body the agent's reply
				// schema cannot map — can never succeed on retry, so it is a
				// Bad Gateway. A timeout / budget exhaustion is a Gateway
				// Timeout: the same delivery may still land later.
				if errors.Is(err, webhook.ErrPermanent) {
					respond(http.StatusBadGateway, map[string]string{"error": err.Error()})
					return
				}
				respond(http.StatusGatewayTimeout, map[string]string{"error": err.Error()})
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
			respond(http.StatusOK, blockingDeliverResponse{
				ID:         entry.ID,
				Transport:  "webhook",
				Reply:      reply,
				SessionID:  req.SessionID,
				RequestID:  req.RequestID,
				Capability: capability,
				Target:     routedTarget(capability, id),
			})
			deliveriesTotal.Inc()
			return
		}
		// DeliverContext carries the request's correlation id into the
		// driver so the background webhook dispatch/outcome log lines can be
		// joined to this request (DF-CRIER-141).
		if _, err := h.webhooks.DeliverContext(ctx, id, target.Webhook, env); err != nil {
			// Queue path handled inside the driver; the deliver call
			// itself only fails on config errors — surface those.
			respond(http.StatusBadGateway, map[string]string{"error": err.Error()})
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
			"request_id", middleware.RequestIDFromContext(ctx),
		)
		// Queued for push: the accept is recorded under the sender's key
		// (CR-FEAT-025) so a retry does not queue the same work twice.
		accept(idempotentReceipt{
			Status:       http.StatusAccepted,
			ID:           entry.ID,
			Transport:    "webhook",
			DeliveryMode: mode,
			Guard:        guardInDeliverResponse(guardMeta),
		})
		respond(http.StatusAccepted, deliverResponse{
			ID:           entry.ID,
			Transport:    "webhook",
			DeliveryMode: mode,
			Guard:        guardInDeliverResponse(guardMeta),
			Capability:   capability,
			Target:       routedTarget(capability, id),
		})
		deliveriesTotal.Inc()
		return
	}

	// Inbox store branch: the entry carries the (possibly quarantined)
	// payload and the guard metadata (spec §2.2/§9.3).
	result.Transport = "inbox"
	entry.Payload = payload
	entry.Guard = guardMeta
	// CR-CHAT-020: stamp the measured delivery latency. time.Since uses a
	// monotonic clock, so a sub-millisecond figure is a real measurement;
	// whole milliseconds (>=1) survive the omitempty encoding.
	entry.DeliveryMs = time.Since(deliverStart).Milliseconds()
	var storeErr error
	var durableReceipt *ServiceDeliveryReceipt
	if serviceAuthority != nil {
		if secured, ok := h.store.(*AuthorityStore); ok {
			if err := resolveMessageExpiry(entry); err != nil {
				respond(http.StatusBadRequest, map[string]string{"error": err.Error()})
				return
			}
			expiresAt := MessageExpiry(entry.ExpiresAt)
			response, err := json.Marshal(deliverResponse{ID: entry.ID, Transport: "inbox", ExpiresAt: &expiresAt, Guard: guardInDeliverResponse(guardMeta), Location: entry.Location})
			if err != nil {
				storeFailure(err)
				return
			}
			response = append(response, '\n')
			var replayed bool
			durableReceipt, replayed, storeErr = secured.deliverServiceAuthorized(id, entry, serviceAuthority, ServiceDeliveryReceipt{Status: http.StatusCreated, MessageID: entry.ID, Response: response})
			if storeErr == nil && replayed {
				result.receipt = durableReceipt
				result.StatusCode = durableReceipt.Status
				return
			}
		} else {
			// Compatibility-only handlers configured through the deprecated
			// SetServiceDeliveryAllowlist setter predate store-wide authority.
			storeErr = h.store.Deliver(id, entry)
		}
	} else {
		storeErr = h.store.Deliver(id, entry)
	}
	if err := storeErr; err != nil {
		if errors.Is(err, ErrServiceIdempotencyConflict) {
			respond(http.StatusConflict, map[string]string{"error": err.Error()})
		} else if errors.Is(err, ErrAgentNotFound) {
			respond(http.StatusNotFound, map[string]string{"error": err.Error()})
		} else if errors.Is(err, ErrInvalidStoreInput) {
			respond(http.StatusBadRequest, map[string]string{"error": err.Error()})
		} else {
			storeFailure(err)
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
		"request_id", middleware.RequestIDFromContext(ctx),
	)
	// The expiry is resolved into the tri-state wire type (DF-CRIER-182): a
	// never-expiring message reports null instead of the zero time.
	expiresAt := MessageExpiry(entry.ExpiresAt)
	// The message is stored: record the accept under the sender's key
	// (CR-FEAT-025), so a retry of the same key answers with THIS id instead
	// of storing a second copy of the same work.
	accept(idempotentReceipt{
		Status:    http.StatusCreated,
		ID:        entry.ID,
		Transport: "inbox",
		ExpiresAt: &expiresAt,
		Guard:     guardInDeliverResponse(guardMeta),
	})
	response := deliverResponse{
		ID:         entry.ID,
		Transport:  "inbox",
		ExpiresAt:  &expiresAt,
		Guard:      guardInDeliverResponse(guardMeta),
		Capability: capability,
		Target:     routedTarget(capability, id),
		Location:   entry.Location,
	}
	if durableReceipt != nil {
		result.receipt = durableReceipt
		result.StatusCode = durableReceipt.Status
	} else {
		respond(http.StatusCreated, response)
	}
	deliveriesTotal.Inc()
	return
}
