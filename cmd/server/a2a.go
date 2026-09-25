// a2a.go — the A2A JSON-RPC binding: SendMessage and SendStreamingMessage
// (INT-A2A-003, specs/A2A-OPTION.md §5.4).
//
// This is the second A2A surface crier serves, and like the Agent Card route it
// exists ONLY while CR_A2A_ENABLED is set AND only for an agent whose registry
// row opted in. With the switch unset the path is not registered at all and
// answers exactly what it answered before this option existed.
//
// The delivery is crier's OWN: this file translates an A2A message into the body
// POST /agents/{id}/inbox already accepts and then calls that route's handler —
// the same function the HTTP route is registered with. Nothing about the
// delivery is re-implemented here, so the guard, idempotency, the detection
// layer, federation hold/retry, webhook push, the durable inbox, TTL and the
// lease/ack lifecycle all apply exactly as they do to any other delivery.
//
// The stream is an SSE ADAPTER over crier's relay, not a change to it: the relay
// keeps serving its WebSocket subscribers exactly as before, and this handler
// calls the same exported Relay.Subscribe the WebSocket handler uses, writing
// what arrives as text/event-stream frames. On top of that subscription it
// reports the task's own lifecycle, read only through the store's optional
// InboxPeeker capability — a read, because a Retrieve would LEASE the message
// and steal it from the agent whose work is being watched.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/mux"

	"github.com/crier-dev/crier/internal/a2a"
	"github.com/crier-dev/crier/internal/httperr"
	"github.com/crier-dev/crier/internal/middleware"
	"github.com/crier-dev/crier/internal/registry"
	"github.com/crier-dev/crier/internal/relay"
)

const (
	// maxA2ABodyBytes bounds a JSON-RPC request body. The A2A parts a client
	// sends are base64 in a JSON document, so this is a request-size ceiling
	// and not a message-size policy: crier's delivery itself has no payload
	// limit, and a body beyond this is refused with 413 rather than read into
	// memory unbounded.
	maxA2ABodyBytes = 4 << 20

	// defaultStreamBudget is how long one SSE stream may stay open before the
	// binding closes it with its last-known state. It is deliberately the same
	// ceiling crier already applies to its other request-level waits
	// (maxWaitSeconds / maxDeliverTimeoutMs = 120s): a client's expectation of
	// how long this server holds a request open should not depend on which
	// endpoint it called. Reaching it is NOT a terminal state — the last event
	// says so, and the client polls or re-subscribes.
	defaultStreamBudget = 120 * time.Second

	// defaultStreamPoll is how often an open stream re-reads the task's inbox
	// entry. crier's inbox publishes no lifecycle notification (the long-poll
	// notifier fires on DELIVERY, not on lease/ack), so observation is a poll:
	// one read per interval per open stream, never a lease.
	defaultStreamPoll = 250 * time.Millisecond
)

// a2aOptions is the serving process's A2A JSON-RPC posture. The duration and
// clock fields are seams: production passes the defaults above, and tests pin
// them so a stream's budget can be exercised without waiting two minutes.
type a2aOptions struct {
	// port is the fallback origin port (kept alongside the card route's
	// options so both A2A surfaces are configured in one place).
	port int
	// authTokenSet reports CR_AUTH_TOKEN is in force.
	authTokenSet bool
	// agentSignatureEnforced reports CR_REQUIRE_AGENT_SIG is in force.
	agentSignatureEnforced bool
	// streamBudget bounds one stream's lifetime.
	streamBudget time.Duration
	// streamPoll is the lifecycle observation interval.
	streamPoll time.Duration
	// now is the clock (a stream's timestamps and its expiry comparison).
	now func() time.Time
}

// registerA2ARoute registers the JSON-RPC binding at the path the Agent Card
// already advertises (a2a.JSONRPCBindingPath). It is called from run() ONLY when
// cfg.A2AEnabled is true, so with the switch unset the path stays unregistered
// and answers the router's own 404 — the half that makes the option separable.
//
// deliver is the registry handler's HandleDeliver, passed in rather than
// reconstructed: the binding reuses the delivery path, it does not own it.
func registerA2ARoute(r *mux.Router, store registry.Store, deliver http.HandlerFunc, relaySvc *relay.Relay, opts a2aOptions) {
	if opts.streamBudget <= 0 {
		opts.streamBudget = defaultStreamBudget
	}
	if opts.streamPoll <= 0 {
		opts.streamPoll = defaultStreamPoll
	}
	if opts.now == nil {
		opts.now = time.Now
	}
	h := &a2aHandler{store: store, deliver: deliver, relay: relaySvc, opts: opts}
	r.HandleFunc(a2a.JSONRPCBindingPath, h.handle).Methods(http.MethodPost)
}

// a2aHandler serves the JSON-RPC binding.
type a2aHandler struct {
	store   registry.Store
	deliver http.HandlerFunc
	relay   *relay.Relay
	opts    a2aOptions
}

// handle dispatches one JSON-RPC 2.0 request (§9.4). Every JSON-RPC response —
// a result or an error — is answered with HTTP 200 and the binding's media type,
// which is what the JSON-RPC 2.0 binding is: the correlation is the request's
// id, and an HTTP status would be a second, coarser answer to a question the
// protocol already answers precisely. HTTP-level failures that never reach the
// JSON-RPC layer (a wrong method, a wrong media type, an oversized body) keep
// their honest HTTP status.
func (h *a2aHandler) handle(w http.ResponseWriter, r *http.Request) {
	if ct := r.Header.Get("Content-Type"); !a2aMediaTypeAccepted(ct) {
		httperr.WriteJSONError(w, http.StatusUnsupportedMediaType, fmt.Sprintf(
			"unsupported Content-Type %q: this binding accepts %s or application/json",
			ct, a2a.RPCMediaType))
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxA2ABodyBytes)
	body, err := readAll(r.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			httperr.WriteJSONError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf(
				"request body exceeds %d bytes", maxA2ABodyBytes))
			return
		}
		httperr.WriteJSONError(w, http.StatusBadRequest, "could not read the request body")
		return
	}

	req, rpcErr := a2a.DecodeRequest(body)
	if rpcErr != nil {
		h.writeRPC(w, a2a.ErrorResponse(a2a.NullID, rpcErr))
		return
	}
	// A2A service parameters travel as headers (§9.2), so version negotiation
	// happens before the method is looked at: a client speaking a version this
	// server does not serve is told that, whatever it asked for.
	if verr := a2a.CheckVersion(r.Header.Get(a2a.A2AVersionHeader)); verr != nil {
		h.writeRPC(w, a2a.ErrorResponse(req.ID, verr))
		return
	}

	switch req.Method {
	case a2a.MethodSendMessage:
		h.send(w, r, req, false)
	case a2a.MethodSendStreamingMessage:
		h.send(w, r, req, true)
	default:
		h.writeRPC(w, a2a.ErrorResponse(req.ID, a2a.NewRPCError(a2a.CodeMethodNotFound, fmt.Sprintf(
			"Method not found: this binding serves %s and %s; the task lifecycle (GetTask, ListTasks, CancelTask, SubscribeToTask) and the push-notification methods are later rows (INT-A2A-004/005) and are not registered",
			a2a.MethodSendMessage, a2a.MethodSendStreamingMessage))))
	}
}

// send performs SendMessage, or the delivery half of SendStreamingMessage, and
// answers either a result or a stream.
func (h *a2aHandler) send(w http.ResponseWriter, r *http.Request, req *a2a.RPCRequest, streaming bool) {
	params, rpcErr := a2a.DecodeSendMessageParams(req.Params)
	if rpcErr != nil {
		h.writeRPC(w, a2a.ErrorResponse(req.ID, rpcErr))
		return
	}

	// The per-agent half of the gate, checked BEFORE anything is delivered: an
	// agent that did not opt in is not reachable over A2A at all (§4.2).
	target, rpcErr := h.target(params.Tenant)
	if rpcErr != nil {
		h.writeRPC(w, a2a.ErrorResponse(req.ID, rpcErr))
		return
	}

	tr, err := a2a.Translate(params, params.Metadata, r.Header.Get(a2a.AgentIDHeader), streaming)
	if err != nil {
		h.writeRPC(w, a2a.ErrorResponse(req.ID, refusalError(err)))
		return
	}

	status, respBody, err := h.deliverTo(target.ID, tr.Request, r.Context())
	if err != nil {
		h.writeRPC(w, a2a.ErrorResponse(req.ID, a2a.NewRPCError(a2a.CodeInternalError,
			"crier's delivery path could not be reached: "+err.Error())))
		return
	}
	if status < 200 || status > 299 {
		h.writeRPC(w, a2a.ErrorResponse(req.ID, a2a.ErrorFromStatus(status, respBody)))
		return
	}

	var accept a2a.DeliverAccept
	if err := json.Unmarshal(respBody, &accept); err != nil {
		h.writeRPC(w, a2a.ErrorResponse(req.ID, a2a.NewRPCError(a2a.CodeInternalError,
			"crier's delivery accept could not be read: "+err.Error())))
		return
	}

	if streaming {
		h.stream(w, r, req, tr, target.ID, accept)
		return
	}

	if reply := accept.ReplyPayload(); len(reply) > 0 {
		// A blocking target answered: this is §3.1.1's direct Message for a
		// simple interaction, and there is no task to track.
		msg, err := a2a.MessageFromReply(tr, accept, h.opts.now())
		if err != nil {
			h.writeRPC(w, a2a.ErrorResponse(req.ID, a2a.NewRPCError(a2a.CodeInternalError, err.Error())))
			return
		}
		h.writeRPC(w, a2a.SuccessResponse(req.ID, a2a.SendMessageResponse{Message: msg}))
		return
	}

	task, err := a2a.TaskFromAccept(tr, accept, h.opts.now())
	if err != nil {
		h.writeRPC(w, a2a.ErrorResponse(req.ID, a2a.NewRPCError(a2a.CodeInternalError, err.Error())))
		return
	}
	h.writeRPC(w, a2a.SuccessResponse(req.ID, a2a.SendMessageResponse{Task: task}))
}

// target resolves the registry row the request's tenant names, enforcing the
// per-agent half of the A2A gate.
//
// The refusals are deliberately of one kind: an id that is not a row, and a row
// that did not opt in, get the same answer — the bus does not tell A2A clients
// which agents exist but stayed out (§4.2), and GET /agents/{id} is where that
// question belongs. A store that cannot answer is a different fact and is
// reported as one.
func (h *a2aHandler) target(tenant string) (*registry.Agent, *a2a.RPCError) {
	id := strings.TrimSpace(tenant)
	if id == "" {
		return nil, a2a.NewRPCError(a2a.CodeInvalidParams,
			"params.tenant is required: one crier origin hosts many agents, so an A2A request names its target with the tenant the Agent Card advertises (AgentInterface.tenant = the registry id)",
			a2a.InvalidParamsDetail(&a2a.InvalidParamsError{Field: "params.tenant", Detail: "is required"}))
	}
	agent, err := h.store.Get(id)
	if err != nil {
		if !errors.Is(err, registry.ErrAgentNotFound) {
			slog.Error("A2A JSON-RPC: registry lookup failed", "agent_id", id, "error", err)
			return nil, a2a.NewRPCError(a2a.CodeInternalError, "registry storage unavailable")
		}
		return nil, notA2ATarget(id)
	}
	if !agent.A2A.OptedIn() {
		return nil, notA2ATarget(id)
	}
	return agent, nil
}

// notA2ATarget is the single refusal for "no A2A agent here".
func notA2ATarget(id string) *a2a.RPCError {
	return a2a.NewRPCError(a2a.CodeInvalidParams, fmt.Sprintf(
		"params.tenant %q is not an A2A agent on this relay: either no such agent is registered, or the agent has not opted in (set \"a2a\":{\"enabled\":true} on POST /agents or PATCH /agents/{id})", id),
		a2a.InvalidParamsDetail(&a2a.InvalidParamsError{Field: "params.tenant",
			Detail: "names no opted-in A2A agent on this relay"}))
}

// deliverTo runs the translated delivery through crier's own deliver handler —
// the very function POST /agents/{id}/inbox is registered with — and returns the
// status and body it wrote.
//
// The inner request carries the OUTER request's context, so the delivery's own
// log lines and its detection observation are correlated to the A2A request that
// caused them rather than to a second, invented id. It is not re-dispatched
// through the router: the route's middleware has already run for this request
// (this handler is behind the same chain), and running it twice would mean two
// access-log lines and two auth decisions for one client request.
func (h *a2aHandler) deliverTo(agentID string, req *a2a.DeliveryRequest, parent context.Context) (int, []byte, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return 0, nil, fmt.Errorf("encode the crier deliver request: %w", err)
	}
	inner, err := http.NewRequestWithContext(parent, http.MethodPost,
		"/agents/"+agentID+"/inbox", bytes.NewReader(body))
	if err != nil {
		return 0, nil, fmt.Errorf("build the crier deliver request: %w", err)
	}
	inner.Header.Set("Content-Type", "application/json")
	// The deliver handler reads the target from the path vars; setting them
	// directly is what the router would have done had this arrived on the wire
	// (mux.SetURLVars returns the request carrying them).
	inner = mux.SetURLVars(inner, map[string]string{"id": agentID})

	rec := &captureWriter{header: http.Header{}}
	h.deliver(rec, inner)
	return rec.statusOrDefault(), rec.body.Bytes(), nil
}

// writeRPC writes a JSON-RPC response envelope with its binding media type.
func (h *a2aHandler) writeRPC(w http.ResponseWriter, resp a2a.RPCResponse) {
	w.Header().Set("Content-Type", a2a.RPCMediaType)
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		// The response is already committed, so all that is left is the reason
		// it may be short.
		slog.Warn("A2A JSON-RPC: response encode failed", "error", err)
	}
}

// captureWriter is the smallest http.ResponseWriter that can hold what a
// handler wrote: reusing registry.Handler.HandleDeliver from inside this binding
// means giving it somewhere to write.
type captureWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

// Header implements http.ResponseWriter.
func (c *captureWriter) Header() http.Header { return c.header }

// WriteHeader implements http.ResponseWriter, keeping the FIRST status written
// (net/http's own rule).
func (c *captureWriter) WriteHeader(status int) {
	if c.status == 0 {
		c.status = status
	}
}

// Write implements http.ResponseWriter, defaulting the status to 200 exactly as
// net/http does for a handler that never calls WriteHeader.
func (c *captureWriter) Write(b []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	return c.body.Write(b)
}

// statusOrDefault is the status the inner handler produced.
func (c *captureWriter) statusOrDefault() int {
	if c.status == 0 {
		return http.StatusOK
	}
	return c.status
}

// refusalError renders a translation failure as the JSON-RPC error object it
// is: an InvalidParamsError becomes -32602 with the spec's own fieldViolation
// detail shape (§9.5), and an error that already knows its code is reported
// verbatim.
func refusalError(err error) *a2a.RPCError {
	var rpc *a2a.RPCError
	if errors.As(err, &rpc) {
		return rpc
	}
	var params *a2a.InvalidParamsError
	if errors.As(err, &params) {
		return a2a.NewRPCError(a2a.CodeInvalidParams, params.Error(), a2a.InvalidParamsDetail(params))
	}
	return a2a.NewRPCError(a2a.CodeInvalidParams, err.Error())
}

// a2aMediaTypeAccepted reports whether a request's Content-Type is one this
// binding reads: the A2A media type registration, the plain application/json
// §9.1 names, or nothing at all. Any other explicit type is refused, because a
// client that labelled its body something else did not mean to send JSON.
func a2aMediaTypeAccepted(contentType string) bool {
	ct := strings.TrimSpace(contentType)
	if ct == "" {
		return true
	}
	if mediaType, _, found := strings.Cut(ct, ";"); found {
		ct = strings.TrimSpace(mediaType)
	}
	switch strings.ToLower(ct) {
	case "application/json", a2a.RPCMediaType:
		return true
	default:
		return false
	}
}

// readAll reads a request body in full.
func readAll(r io.Reader) ([]byte, error) {
	var buf bytes.Buffer
	_, err := buf.ReadFrom(r)
	return buf.Bytes(), err
}

// ---------------------------------------------------------------------------
// Streaming (SendStreamingMessage, §9.4.2)
// ---------------------------------------------------------------------------

// stream serves the SSE half of SendStreamingMessage against an already-accepted
// delivery.
//
// Two shapes, both from §3.1.2:
//
//   - a target that answered inline (a blocking webhook) gives the MESSAGE-ONLY
//     stream: exactly one Message event, then the stream closes. There is no
//     task to track, and inventing one would report work that does not exist.
//   - every other accepted delivery gives the TASK LIFECYCLE stream: the Task
//     first, then its status changes and any artifacts published for it, closing
//     when the task reaches a terminal state (§3.1.2 pattern 2).
func (h *a2aHandler) stream(w http.ResponseWriter, r *http.Request, req *a2a.RPCRequest, tr *a2a.Translation, targetID string, accept a2a.DeliverAccept) {
	if reply := accept.ReplyPayload(); len(reply) > 0 {
		msg, err := a2a.MessageFromReply(tr, accept, h.opts.now())
		if err != nil {
			h.writeRPC(w, a2a.ErrorResponse(req.ID, a2a.NewRPCError(a2a.CodeInternalError, err.Error())))
			return
		}
		if !h.openStream(w) {
			return
		}
		h.writeEvent(w, a2a.SuccessResponse(req.ID, a2a.StreamMessage(msg)))
		return
	}

	task, err := a2a.TaskFromAccept(tr, accept, h.opts.now())
	if err != nil {
		h.writeRPC(w, a2a.ErrorResponse(req.ID, a2a.NewRPCError(a2a.CodeInternalError, err.Error())))
		return
	}
	if !h.openStream(w) {
		return
	}

	s := &a2aStream{
		h:      h,
		w:      w,
		r:      r,
		id:     req.ID,
		target: targetID,
		task:   task,
		state:  task.Status.State,
	}
	s.run()
}

// openStream writes the streaming response's headers and reports whether the
// response can stream at all. A writer that cannot flush could not deliver SSE
// frames as they happen, so the binding refuses rather than buffering a stream
// into one response.
func (h *a2aHandler) openStream(w http.ResponseWriter) bool {
	flusher, ok := w.(http.Flusher)
	if !ok {
		httperr.WriteJSONError(w, http.StatusInternalServerError, "this server cannot stream")
		return false
	}
	header := w.Header()
	header.Set("Content-Type", a2a.StreamMediaType)
	// A stream is a live view of state the client will re-ask about; a cache
	// holding it would serve someone else's task.
	header.Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	return true
}

// writeEvent writes one SSE frame — `data: <json-rpc response>` (§9.4.2) — and
// flushes it. It reports whether the write reached the client.
func (h *a2aHandler) writeEvent(w http.ResponseWriter, resp a2a.RPCResponse) bool {
	body, err := json.Marshal(resp)
	if err != nil {
		slog.Error("A2A stream: encode event failed", "error", err)
		return false
	}
	if _, err := w.Write(append(append([]byte("data: "), body...), '\n', '\n')); err != nil {
		return false
	}
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	return true
}

// a2aStream is one open SendStreamingMessage stream.
type a2aStream struct {
	h      *a2aHandler
	w      http.ResponseWriter
	r      *http.Request
	id     json.RawMessage
	target string
	task   *a2a.Task
	// state is the last state reported to the client, so a change is only ever
	// reported once.
	state a2a.TaskState
	// expiresAt is the resolved message expiry the delivery accept stated, and
	// hasExpiry whether it applies at all (a webhook delivery creates no inbox
	// entry and therefore has none).
	expiresAt time.Time
	hasExpiry bool
	// observe reports whether this task's state can be read from crier's own
	// inbox, which is true only for an inbox delivery: a pushed delivery has no
	// inbox entry to look at, and pretending to read one would turn "no row"
	// into "completed".
	observe bool
	// artifactSeq numbers the artifacts forwarded off the relay, so each has a
	// unique artifactId within the task (§4.1.7).
	artifactSeq int
	// events counts what was written, for the close log.
	events int
	// relay topic subscription, when one could be derived.
	topic string
}

// run streams until a terminal state, the budget, or the client going away.
func (s *a2aStream) run() {
	s.expiresAt, s.hasExpiry = deliverExpiry(s.task)
	s.observe = acceptCarriesInboxEntry(s.task)

	if !s.h.writeEvent(s.w, a2a.SuccessResponse(s.id, a2a.StreamTask(s.task))) {
		s.close("client-disconnect")
		return
	}
	s.events++

	// The SSE view of crier's relay subscription. The relay keeps serving its
	// WebSocket subscribers untouched: this calls the same exported Subscribe
	// and writes what arrives as event-stream frames. Publishing to the task's
	// topic is an ordinary POST /relay/publish — no A2A-specific publish path
	// exists or is needed.
	var relayCh <-chan []byte
	if topic, ok := a2a.TaskTopic(s.task.ID); ok && s.h.relay != nil {
		s.topic = topic
		ch, unsub := s.h.relay.Subscribe(topic)
		defer unsub()
		relayCh = ch
	}

	slog.Info("A2A stream opened",
		"task_id", s.task.ID, "target", s.target, "topic", s.topic,
		"observing_inbox", s.observe,
		"request_id", middleware.RequestIDFromContext(s.r.Context()))

	budget := time.NewTimer(s.h.opts.streamBudget)
	defer budget.Stop()
	ticker := time.NewTicker(s.h.opts.streamPoll)
	defer ticker.Stop()

	for {
		select {
		case <-s.r.Context().Done():
			s.close("client-disconnect")
			return
		case <-budget.C:
			// The stream budget elapsed. This is NOT a terminal state: the last
			// event says where the task stands and that it is still open, so a
			// client is never left guessing whether the silence meant "done".
			s.emit(a2a.TaskStatusUpdateEvent{
				TaskID:    s.task.ID,
				ContextID: s.context(),
				Status: a2a.TaskStatus{
					State:     s.state,
					Timestamp: a2a.Timestamp(s.h.opts.now()),
					Message: &a2a.Message{
						MessageID: s.task.ID,
						ContextID: s.context(),
						TaskID:    s.task.ID,
						Role:      a2a.RoleAgent,
						Parts: []a2a.Part{a2a.TextPart(fmt.Sprintf(
							"stream budget of %s elapsed; the task is still %s — poll GetTask or open a new stream to continue watching (the A2A task lifecycle lands with INT-A2A-004)",
							s.h.opts.streamBudget, s.state))},
					},
				},
			})
			s.close("budget")
			return
		case frame, ok := <-relayCh:
			if !ok {
				// An impossible-to-subscribe topic closes the channel
				// immediately; dropping the case leaves the stream running
				// without it rather than spinning on a closed channel.
				relayCh = nil
				continue
			}
			s.artifact(frame)
		case <-ticker.C:
			if state, done := s.observeOnce(); done {
				s.close(closeReason(state))
				return
			}
		}
	}
}

// context is the task's A2A context id (never empty: the translation supplies
// the task id when the client named no context, which §3.4.1 sanctions).
func (s *a2aStream) context() string {
	if s.task.ContextID != "" {
		return s.task.ContextID
	}
	return s.task.ID
}

// observeOnce re-reads the task's inbox entry and reports a state the client has
// not been told about. done=true means the stream is finished: either the task
// reached a terminal state (whose event has been written) or the inbox could not
// be read (whose error has been written). The returned state is the terminal one
// for the log line, and empty otherwise.
func (s *a2aStream) observeOnce() (a2a.TaskState, bool) {
	if !s.observe {
		// A pushed delivery has no inbox entry: its progress arrives on the
		// task's relay topic, and its terminal state is GetTask's business
		// (INT-A2A-004). Nothing to read, so nothing is asserted.
		return "", false
	}
	peeker, ok := s.h.store.(registry.InboxPeeker)
	if !ok {
		// The backend cannot answer a read-only view (a remote proxy). The
		// stream keeps carrying the task and its relay artifacts, and says
		// nothing about a state it cannot see.
		return "", false
	}

	now := s.h.opts.now()
	entry, err := peeker.Peek(s.target, s.task.ID)
	switch {
	case err == nil:
		if !entry.ExpiresAt.IsZero() && now.After(entry.ExpiresAt) {
			s.emitTerminal(a2a.TaskStateFailed,
				"the message expired unacknowledged: no agent acknowledged it before its TTL elapsed, so the task cannot complete")
			return a2a.TaskStateFailed, true
		}
		if entry.LeaseID != "" {
			s.setState(a2a.TaskStateWorking, "")
			return "", false
		}
		s.setState(a2a.TaskStateSubmitted, "")
		return "", false
	case errors.Is(err, registry.ErrMessageNotFound):
		// The entry is gone. In crier an inbox entry leaves the store exactly
		// two ways: an ACK removes it (completed), and the expiry sweep removes
		// it after its TTL passed (failed — the row cannot be retrieved any
		// more). The expiry the delivery accept resolved decides which, and the
		// state is only asserted from those two facts — a third removal path an
		// operator drives out of band (a transferred message, a purge) is not
		// observable here and is why GetTask is the authority (INT-A2A-004).
		if s.hasExpiry && !s.expiresAt.IsZero() && now.After(s.expiresAt) {
			s.emitTerminal(a2a.TaskStateFailed,
				"the message expired unacknowledged and was removed from the inbox")
			return a2a.TaskStateFailed, true
		}
		s.emitTerminal(a2a.TaskStateCompleted,
			"acknowledged: the receiving agent retrieved and acknowledged the message")
		return a2a.TaskStateCompleted, true
	default:
		s.h.writeEvent(s.w, a2a.ErrorResponse(s.id, a2a.NewRPCError(a2a.CodeInternalError,
			"the task's inbox could not be read while streaming it: "+err.Error())))
		s.events++
		return "", true
	}
}

// setState reports a non-terminal state change, once.
func (s *a2aStream) setState(state a2a.TaskState, message string) bool {
	if state == s.state {
		return true
	}
	s.state = state
	update := a2a.TaskStatusUpdateEvent{
		TaskID:    s.task.ID,
		ContextID: s.context(),
		Status:    a2a.TaskStatus{State: state, Timestamp: a2a.Timestamp(s.h.opts.now())},
	}
	if message != "" {
		update.Status.Message = &a2a.Message{
			MessageID: s.task.ID, ContextID: s.context(), TaskID: s.task.ID,
			Role: a2a.RoleAgent, Parts: []a2a.Part{a2a.TextPart(message)},
		}
	}
	return s.emit(update)
}

// emitTerminal reports a terminal state, which closes the stream (§3.1.2: the
// stream MUST close when the task reaches a terminal state). setState does the
// work — including the decision not to repeat a state the client already has.
func (s *a2aStream) emitTerminal(state a2a.TaskState, message string) {
	s.setState(state, message)
}

// emit writes one status-update event.
func (s *a2aStream) emit(update a2a.TaskStatusUpdateEvent) bool {
	update.TaskID = s.task.ID
	update.ContextID = s.context()
	s.events++
	return s.h.writeEvent(s.w, a2a.SuccessResponse(s.id, a2a.StreamStatus(&update)))
}

// artifact forwards one relay frame for the task's topic as an artifact update.
// The frame is crier's own — {"topic":…,"event":…} — and the event is projected
// through the same part mapping the delivery payload uses, so a frame that
// carries one of this binding's envelopes contributes its parts and anything
// else contributes its value as a single data part.
func (s *a2aStream) artifact(frame []byte) {
	var f struct {
		Topic string          `json:"topic"`
		Event json.RawMessage `json:"event"`
	}
	if err := json.Unmarshal(frame, &f); err != nil {
		slog.Debug("A2A stream: unreadable relay frame", "task_id", s.task.ID, "error", err)
		return
	}
	parts, err := a2a.PartsFromPayload(f.Event)
	if err != nil || len(parts) == 0 {
		return
	}
	s.artifactSeq++
	event := a2a.TaskArtifactUpdateEvent{
		TaskID:    s.task.ID,
		ContextID: s.context(),
		Artifact: a2a.Artifact{
			ArtifactID: fmt.Sprintf("%s-%d", s.task.ID, s.artifactSeq),
			Parts:      parts,
			Metadata:   map[string]any{"crier": map[string]any{"relay_topic": f.Topic}},
		},
	}
	s.events++
	s.h.writeEvent(s.w, a2a.SuccessResponse(s.id, a2a.StreamArtifact(&event)))
}

// close logs why the stream ended. The reason names the cause rather than the
// outcome, so the log cannot claim a terminal state the stream never observed.
func (s *a2aStream) close(reason string) {
	slog.Info("A2A stream closed",
		"task_id", s.task.ID, "target", s.target, "state", s.state,
		"events", s.events, "reason", reason,
		"request_id", middleware.RequestIDFromContext(s.r.Context()))
}

// closeReason names a close reason: the terminal state that ended the stream, or
// "observe-error" when the stream ended because its state could not be read.
func closeReason(state a2a.TaskState) string {
	if state == "" {
		return "observe-error"
	}
	return "terminal:" + string(state)
}

// deliverExpiry reads the resolved expiry the delivery accept reported. It is
// carried on the Task's crier metadata, which is the same value the accept body
// stated — the accept's own `expires_at` is not part of the Task, so the Task is
// what the stream reads back.
func deliverExpiry(task *a2a.Task) (time.Time, bool) {
	if task == nil || task.Metadata == nil {
		return time.Time{}, false
	}
	raw, ok := task.Metadata["crier"].(a2a.CrierMeta)
	if !ok {
		return time.Time{}, false
	}
	if len(raw.ExpiresAt) == 0 {
		return time.Time{}, false
	}
	trimmed := strings.TrimSpace(string(raw.ExpiresAt))
	if trimmed == "null" {
		// Never expires (ttl_seconds=0): the expiry applies but is the zero
		// time, which the observer reads as "no clock to compare against".
		return time.Time{}, true
	}
	var at time.Time
	if err := json.Unmarshal(raw.ExpiresAt, &at); err != nil {
		return time.Time{}, false
	}
	return at, true
}

// acceptCarriesInboxEntry reports whether the accepted delivery created a
// durable inbox entry — i.e. whether there is a task state in crier's inbox for
// this stream to observe at all.
func acceptCarriesInboxEntry(task *a2a.Task) bool {
	if task == nil || task.Metadata == nil {
		return false
	}
	meta, ok := task.Metadata["crier"].(a2a.CrierMeta)
	return ok && meta.Transport == "inbox"
}
