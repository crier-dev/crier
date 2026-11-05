package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/crier-dev/crier/internal/a2a"
)

// peer is a deliberately crier-shaped A2A peer: it decodes requests with
// internal/a2a's OWN decoders (DecodeRequest, DecodeSendMessageParams, …) and
// answers with internal/a2a's OWN response builder, so a payload the client
// gets wrong fails here for the same reason it would fail against a real crier
// server — not because a hand-written fixture disagreed with the server.
type peer struct {
	t   *testing.T
	srv *httptest.Server

	mu      sync.Mutex
	card    []byte
	seen    []seenRequest
	tasks   map[string]string // messageId → task id
	nextN   int
	sends   int
	rpcHits int

	// Behaviour switches.
	cardStatus    int    // non-zero answers that status on discovery
	rpcStatus     int    // non-zero answers that status on /a2a
	rpcBody       string // non-empty answers these exact bytes on /a2a
	requireAuth   string // non-empty requires "Bearer <value>"
	requireHeader string // non-empty requires header name = value
	headerValue   string
	dropFirstSend bool // close the connection on the first /a2a POST
	cardAuth      bool // the card route itself requires the bearer
}

type seenRequest struct {
	method      string
	path        string
	auth        string
	contentType string
	a2aVersion  string
	headers     http.Header
	body        []byte
}

func newPeer(t *testing.T) *peer {
	t.Helper()
	p := &peer{t: t, tasks: map[string]string{}}
	p.srv = httptest.NewServer(http.HandlerFunc(p.serve))
	t.Cleanup(p.srv.Close)
	p.buildCard(a2a.ServerInfo{Origin: p.srv.URL, Version: "test"})
	return p
}

// buildCard projects a card with the PUBLISHER's own projection (a2a.BuildCard)
// and stores its exact bytes — the client must parse the same type the server
// renders, not a fixture that drifts from it.
func (p *peer) buildCard(srv a2a.ServerInfo) {
	row := a2a.CardRow{AgentID: "agent-b", Capabilities: []string{"solver", "reviewer"}}
	card := a2a.BuildCard(row, srv)
	raw, err := json.Marshal(card)
	if err != nil {
		p.t.Fatalf("marshal card: %v", err)
	}
	p.mu.Lock()
	p.card = raw
	p.mu.Unlock()
}

func (p *peer) origin() string { return p.srv.URL }

func (p *peer) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == a2a.AgentCardPath {
		p.serveCard(w, r)
		return
	}
	if r.URL.Path == a2a.JSONRPCBindingPath {
		p.serveRPC(w, r)
		return
	}
	http.NotFound(w, r)
}

func (p *peer) serveCard(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	card, status := p.card, p.cardStatus
	requireAuth := p.cardAuth
	p.seen = append(p.seen, seenRequest{method: r.Method, path: r.URL.Path, auth: r.Header.Get("Authorization")})
	p.mu.Unlock()

	if requireAuth && r.Header.Get("Authorization") != "Bearer "+p.requireAuthValue() {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if status != 0 {
		w.WriteHeader(status)
		return
	}
	w.Header().Set("Content-Type", a2a.CardMediaType)
	w.Header().Set("ETag", a2a.CardETag(card))
	_, _ = w.Write(card)
}

func (p *peer) requireAuthValue() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.requireAuth
}

func (p *peer) serveRPC(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	p.mu.Lock()
	p.seen = append(p.seen, seenRequest{
		method: r.Method, path: r.URL.Path, auth: r.Header.Get("Authorization"),
		contentType: r.Header.Get("Content-Type"), a2aVersion: r.Header.Get(versionHeader),
		headers: r.Header.Clone(), body: body,
	})
	p.rpcHits++
	status, rpcBody := p.rpcStatus, p.rpcBody
	requireAuth, requireHeader, headerValue := p.requireAuth, p.requireHeader, p.headerValue
	drop := p.dropFirstSend
	if drop {
		p.dropFirstSend = false
	}
	p.mu.Unlock()

	if drop {
		// A transport failure the client cannot distinguish from a send that
		// never landed: hijack and close, so the retry path is exercised for
		// real rather than simulated with a 5xx (which the client must NOT
		// retry).
		if hj, ok := w.(http.Hijacker); ok {
			conn, _, err := hj.Hijack()
			if err == nil {
				_ = conn.Close()
				return
			}
		}
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if requireAuth != "" && r.Header.Get("Authorization") != "Bearer "+requireAuth {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if requireHeader != "" && r.Header.Get(requireHeader) != headerValue {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if status != 0 {
		w.WriteHeader(status)
		return
	}
	if rpcBody != "" {
		w.Header().Set("Content-Type", a2a.RPCMediaType)
		_, _ = io.WriteString(w, rpcBody)
		return
	}

	req, rpcErr := a2a.DecodeRequest(body)
	if rpcErr != nil {
		p.writeRPC(w, a2a.ErrorResponse(nil, rpcErr))
		return
	}
	p.dispatch(w, req)
}

// dispatch answers exactly the operations this client sends, decoding params
// with the server's own strict decoders.
func (p *peer) dispatch(w http.ResponseWriter, req *a2a.RPCRequest) {
	switch req.Method {
	case a2a.MethodSendMessage, MethodMessageSend:
		params, rpcErr := a2a.DecodeSendMessageParams(req.Params)
		if rpcErr != nil {
			p.writeRPC(w, a2a.ErrorResponse(req.ID, rpcErr))
			return
		}
		p.mu.Lock()
		p.sends++
		taskID, ok := p.tasks[params.Message.MessageID]
		replayed := ok
		if !ok {
			p.nextN++
			taskID = fmt.Sprintf("task-%d", p.nextN)
			p.tasks[params.Message.MessageID] = taskID
		}
		p.mu.Unlock()
		task := a2a.Task{
			ID:        taskID,
			ContextID: params.Message.ContextID,
			Status:    a2a.TaskStatus{State: a2a.TaskStateSubmitted},
			Metadata: map[string]any{"crier": map[string]any{
				"idempotent_replay": replayed,
				"idempotency": map[string]any{
					"durability": "process", "replay": replayLabel(replayed),
				},
			}},
			History: []a2a.Message{*params.Message},
		}
		p.writeRPC(w, a2a.SuccessResponse(req.ID, a2a.SendMessageResponse{Task: &task}))

	case a2a.MethodGetTask, MethodTasksGet:
		params, rpcErr := a2a.DecodeGetTaskParams(req.Params)
		if rpcErr != nil {
			p.writeRPC(w, a2a.ErrorResponse(req.ID, rpcErr))
			return
		}
		p.writeRPC(w, a2a.SuccessResponse(req.ID, a2a.Task{
			ID:     params.ID,
			Status: a2a.TaskStatus{State: a2a.TaskStateWorking},
		}))

	case a2a.MethodCancelTask, MethodTasksCancel:
		params, rpcErr := a2a.DecodeCancelTaskParams(req.Params)
		if rpcErr != nil {
			p.writeRPC(w, a2a.ErrorResponse(req.ID, rpcErr))
			return
		}
		p.writeRPC(w, a2a.SuccessResponse(req.ID, a2a.Task{
			ID:     params.ID,
			Status: a2a.TaskStatus{State: a2a.TaskStateCanceled},
		}))

	default:
		p.writeRPC(w, a2a.ErrorResponse(req.ID, a2a.NewRPCError(a2a.CodeMethodNotFound,
			"Method not found: "+req.Method)))
	}
}

func (p *peer) writeRPC(w http.ResponseWriter, resp a2a.RPCResponse) {
	w.Header().Set("Content-Type", a2a.RPCMediaType)
	raw, err := json.Marshal(resp)
	if err != nil {
		p.t.Fatalf("marshal rpc response: %v", err)
	}
	_, _ = w.Write(raw)
}

func (p *peer) seenCopy() []seenRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]seenRequest(nil), p.seen...)
}

func (p *peer) rpcRequests() []seenRequest {
	var out []seenRequest
	for _, s := range p.seenCopy() {
		if s.method == http.MethodPost {
			out = append(out, s)
		}
	}
	return out
}

func replayLabel(replayed bool) string {
	if replayed {
		return "replayed"
	}
	return "new"
}

// newClient builds a client for p with options a caller would set.
func newClient(t *testing.T, p *peer, mutate func(*Options)) *Client {
	t.Helper()
	opts := Options{Origin: p.origin()}
	if mutate != nil {
		mutate(&opts)
	}
	c, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// TestDiscoverParsesThePublishersCard proves discovery reads the card through
// the SAME type the publisher projects: the fixture is a2a.BuildCard's own
// output, and every field the client acts on (endpoint, tenant, digest) is
// derived from that projection.
func TestDiscoverParsesThePublishersCard(t *testing.T) {
	p := newPeer(t)
	c := newClient(t, p, nil)

	card, err := c.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if card.AgentCard.Name != "agent-b" {
		t.Errorf("card name = %q, want agent-b", card.AgentCard.Name)
	}
	if len(card.AgentCard.Skills) != 2 {
		t.Errorf("skills = %d, want 2 (one per capability tag)", len(card.AgentCard.Skills))
	}
	if want := p.origin() + a2a.JSONRPCBindingPath; card.Endpoint != want {
		t.Errorf("endpoint = %q, want %q (the URL the card advertises)", card.Endpoint, want)
	}
	if card.Binding != a2a.ProtocolBindingJSONRPC {
		t.Errorf("binding = %q, want %q", card.Binding, a2a.ProtocolBindingJSONRPC)
	}
	if card.Tenant != "agent-b" {
		t.Errorf("tenant = %q, want agent-b (AgentInterface.tenant)", card.Tenant)
	}
	if got, want := card.Digest, DigestOf(p.card); got != want {
		t.Errorf("digest = %q, want %q (sha256 over the served bytes)", got, want)
	}
	if card.CardETag == "" {
		t.Error("card etag is empty, want the publisher's own validator")
	}
	if !strings.HasPrefix(card.Digest, "sha256:") {
		t.Errorf("digest %q lacks the sha256: prefix", card.Digest)
	}
}

// TestDiscoverySendsAgentSelectorOnlyWhenAsked covers both card shapes: a crier
// relay needs agent_id, a single-agent peer serves the bare path.
func TestDiscoverySendsAgentSelectorOnlyWhenAsked(t *testing.T) {
	p := newPeer(t)
	c := newClient(t, p, func(o *Options) { o.AgentID = "agent-b" })
	if _, err := c.Discover(context.Background()); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if got := c.cardURL(); !strings.Contains(got, a2a.AgentCardAgentQueryParam+"=agent-b") {
		t.Errorf("card url %q does not carry the agent selector", got)
	}

	plain := newClient(t, p, nil)
	if got := plain.cardURL(); strings.Contains(got, "?") {
		t.Errorf("card url %q carries a selector it was never given", got)
	}
}

// TestSendUsesTheServersOwnDecoders proves the request payload is the shape the
// server side already accepts: the peer decodes it with
// a2a.DecodeSendMessageParams (strict — an unknown member is a -32602) and the
// envelope with a2a.DecodeRequest (strict, DisallowUnknownFields).
func TestSendUsesTheServersOwnDecoders(t *testing.T) {
	p := newPeer(t)
	c := newClient(t, p, nil)
	card, err := c.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}

	res, err := c.Send(context.Background(), card, SendRequest{
		MessageID: "msg-fixed-1",
		ContextID: "ctx-1",
		Text:      "deploy the canary",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.MessageID != "msg-fixed-1" {
		t.Errorf("result messageId = %q, want msg-fixed-1", res.MessageID)
	}
	if res.TaskID != "task-1" {
		t.Errorf("result taskId = %q, want the peer's task-1", res.TaskID)
	}
	if res.CardDigest != card.Digest {
		t.Errorf("result card digest = %q, want %q", res.CardDigest, card.Digest)
	}
	if res.Attempts != 1 {
		t.Errorf("attempts = %d, want 1", res.Attempts)
	}
	if res.Replayed {
		t.Error("a first send reported replayed")
	}

	sent := p.rpcRequests()
	if len(sent) != 1 {
		t.Fatalf("peer saw %d POSTs, want 1", len(sent))
	}
	if got := sent[0].contentType; got != a2a.RPCMediaType {
		t.Errorf("content type = %q, want %q", got, a2a.RPCMediaType)
	}
	if got := sent[0].a2aVersion; got != a2a.ProtocolVersion {
		t.Errorf("A2A-Version header = %q, want %q", got, a2a.ProtocolVersion)
	}
	// The envelope the client wrote is exactly the one a2a.RPCRequest decodes.
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(sent[0].body, &envelope); err != nil {
		t.Fatalf("request body is not JSON: %v", err)
	}
	for _, key := range []string{"jsonrpc", "id", "method", "params"} {
		if _, ok := envelope[key]; !ok {
			t.Errorf("request envelope has no %q member (body: %s)", key, sent[0].body)
		}
	}
	if len(envelope) != 4 {
		t.Errorf("request envelope carries %d members, want exactly 4 (body: %s)", len(envelope), sent[0].body)
	}
	if got := string(envelope["method"]); got != `"SendMessage"` {
		t.Errorf("method = %s, want the v1 name SendMessage", got)
	}
}

// TestLegacyDialectSelectsSlashMethods covers the pre-1.0 peer generation: the
// same params, the slash method names.
func TestLegacyDialectSelectsSlashMethods(t *testing.T) {
	p := newPeer(t)
	c := newClient(t, p, func(o *Options) { o.Methods = &WireLegacy })
	card, err := c.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if _, err := c.Send(context.Background(), card, SendRequest{MessageID: "m-1", Text: "hi"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	var envelope struct {
		Method string `json:"method"`
	}
	if err := json.Unmarshal(p.rpcRequests()[0].body, &envelope); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if envelope.Method != MethodMessageSend {
		t.Errorf("method = %q, want %q", envelope.Method, MethodMessageSend)
	}
}

// TestGetTaskAndCancelTaskPayloads proves the two task-lifecycle requests are
// the shapes a2a.DecodeGetTaskParams / DecodeCancelTaskParams accept, with the
// tenant the card published.
func TestGetTaskAndCancelTaskPayloads(t *testing.T) {
	p := newPeer(t)
	c := newClient(t, p, nil)
	card, err := c.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}

	got, err := c.GetTask(context.Background(), card, "task-9", nil)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.TaskID != "task-9" || got.Task.Status.State != a2a.TaskStateWorking {
		t.Errorf("GetTask = %+v, want task-9 in TASK_STATE_WORKING", got.Task)
	}

	canceled, err := c.CancelTask(context.Background(), card, "task-9")
	if err != nil {
		t.Fatalf("CancelTask: %v", err)
	}
	if canceled.Task.Status.State != a2a.TaskStateCanceled {
		t.Errorf("cancel state = %q, want TASK_STATE_CANCELED", canceled.Task.Status.State)
	}

	sent := p.rpcRequests()
	if len(sent) != 2 {
		t.Fatalf("peer saw %d POSTs, want 2", len(sent))
	}
	var get struct {
		Method string            `json:"method"`
		Params a2a.GetTaskParams `json:"params"`
	}
	if err := json.Unmarshal(sent[0].body, &get); err != nil {
		t.Fatalf("decode get request: %v", err)
	}
	if get.Method != a2a.MethodGetTask || get.Params.ID != "task-9" || get.Params.Tenant != "agent-b" {
		t.Errorf("get request = %+v, want GetTask{tenant:agent-b id:task-9}", get)
	}
	var cancel struct {
		Method string               `json:"method"`
		Params a2a.CancelTaskParams `json:"params"`
	}
	if err := json.Unmarshal(sent[1].body, &cancel); err != nil {
		t.Fatalf("decode cancel request: %v", err)
	}
	if cancel.Method != a2a.MethodCancelTask || cancel.Params.ID != "task-9" {
		t.Errorf("cancel request = %+v, want CancelTask{id:task-9}", cancel)
	}
}

// TestRetryReusesOneMessageID is the idempotency proof: the first attempt's
// connection is dropped, the client retries, and the retry carries the SAME
// messageId — so the peer mints ONE task, which the result reports.
func TestRetryReusesOneMessageID(t *testing.T) {
	p := newPeer(t)
	p.dropFirstSend = true
	c := newClient(t, p, nil)
	card, err := c.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}

	res, err := c.Send(context.Background(), card, SendRequest{Text: "retry me"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.Attempts != 2 {
		t.Errorf("attempts = %d, want 2 (the dropped connection was retried once)", res.Attempts)
	}
	if res.TaskID != "task-1" {
		t.Errorf("taskId = %q, want task-1", res.TaskID)
	}

	sent := p.rpcRequests()
	if len(sent) != 2 {
		t.Fatalf("peer saw %d POSTs, want 2", len(sent))
	}
	first := messageIDOf(t, sent[0].body)
	second := messageIDOf(t, sent[1].body)
	if first != second {
		t.Fatalf("retry changed the messageId (%q then %q) — that would mint a second task", first, second)
	}
	if first != res.MessageID {
		t.Errorf("result messageId %q is not the id on the wire %q", res.MessageID, first)
	}
	p.mu.Lock()
	tasks := len(p.tasks)
	p.mu.Unlock()
	if tasks != 1 {
		t.Errorf("peer holds %d tasks, want 1 — a retry must not create a second", tasks)
	}
}

// TestPinnedMessageIDIsTheSameTask covers the caller's own retry: the same
// messageId answered twice is one task (and the peer says so), a different one
// is a different task, and the result always names the id it used.
func TestPinnedMessageIDIsTheSameTask(t *testing.T) {
	p := newPeer(t)
	c := newClient(t, p, nil)
	card, err := c.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}

	first, err := c.Send(context.Background(), card, SendRequest{MessageID: "pinned-1", Text: "work"})
	if err != nil {
		t.Fatalf("first Send: %v", err)
	}
	again, err := c.Send(context.Background(), card, SendRequest{MessageID: "pinned-1", Text: "work"})
	if err != nil {
		t.Fatalf("replayed Send: %v", err)
	}
	if again.TaskID != first.TaskID {
		t.Errorf("same messageId produced task %q then %q", first.TaskID, again.TaskID)
	}
	if !again.Replayed {
		t.Error("the retried send was not reported as replayed")
	}

	other, err := c.Send(context.Background(), card, SendRequest{MessageID: "pinned-2", Text: "work"})
	if err != nil {
		t.Fatalf("third Send: %v", err)
	}
	if other.TaskID == first.TaskID {
		t.Errorf("a different messageId reused task %q", other.TaskID)
	}
	if other.MessageID != "pinned-2" {
		t.Errorf("result messageId = %q, want pinned-2", other.MessageID)
	}
}

func messageIDOf(t *testing.T, body []byte) string {
	t.Helper()
	var req struct {
		Params struct {
			Message struct {
				MessageID string `json:"messageId"`
			} `json:"message"`
		} `json:"params"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("decode send body: %v", err)
	}
	return req.Params.Message.MessageID
}

// TestFourFailurePathsAreDistinct is the acceptance criterion: unreachable,
// card-404, invalid JSON-RPC response and 401 each answer their own named
// error (never a shared "request failed"), and a 401 is ONE request.
func TestFourFailurePathsAreDistinct(t *testing.T) {
	t.Run("unreachable", func(t *testing.T) {
		// A port that was open a moment ago and is now closed: the definitive
		// connection-refused case, without depending on a fixed port number.
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		addr := ln.Addr().String()
		_ = ln.Close()

		c, err := New(Options{Origin: "http://" + addr})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		_, err = c.Discover(context.Background())
		if !isNamed(err, ErrPeerUnreachable, NamePeerUnreachable) {
			t.Fatalf("Discover error = %v, want %s", err, NamePeerUnreachable)
		}
	})

	t.Run("card404", func(t *testing.T) {
		p := newPeer(t)
		p.cardStatus = http.StatusNotFound
		_, err := newClient(t, p, nil).Discover(context.Background())
		if !isNamed(err, ErrCardNotFound, NameCardNotFound) {
			t.Fatalf("Discover error = %v, want %s", err, NameCardNotFound)
		}
	})

	t.Run("invalidJSONRPC", func(t *testing.T) {
		p := newPeer(t)
		p.rpcBody = "<html>not a JSON-RPC response</html>"
		c := newClient(t, p, nil)
		card, err := c.Discover(context.Background())
		if err != nil {
			t.Fatalf("Discover: %v", err)
		}
		_, err = c.Send(context.Background(), card, SendRequest{Text: "x"})
		if !isNamed(err, ErrInvalidResponse, NameInvalidResponse) {
			t.Fatalf("Send error = %v, want %s", err, NameInvalidResponse)
		}
		if hits := len(p.rpcRequests()); hits != 1 {
			t.Errorf("peer saw %d POSTs, want 1 — an invalid response is not retried", hits)
		}
	})

	t.Run("unauthenticated", func(t *testing.T) {
		p := newPeer(t)
		p.requireAuth = "s3cret"
		c := newClient(t, p, nil)
		card, err := c.Discover(context.Background())
		if err != nil {
			t.Fatalf("Discover: %v", err)
		}
		_, err = c.Send(context.Background(), card, SendRequest{Text: "x"})
		if !isNamed(err, ErrUnauthenticated, NameUnauthenticated) {
			t.Fatalf("Send error = %v, want %s", err, NameUnauthenticated)
		}
		if hits := len(p.rpcRequests()); hits != 1 {
			t.Errorf("peer saw %d POSTs, want exactly 1 — a 401 must not be retried", hits)
		}
		if strings.Contains(err.Error(), "s3cret") {
			t.Errorf("the error echoes the credential: %v", err)
		}
	})
}

// TestUnauthenticatedDiscoveryIsOneError covers the card route being behind the
// peer's auth: same named error, no retry.
func TestUnauthenticatedDiscoveryIsOneError(t *testing.T) {
	p := newPeer(t)
	p.cardAuth = true
	p.requireAuth = "s3cret"
	_, err := newClient(t, p, nil).Discover(context.Background())
	if !isNamed(err, ErrUnauthenticated, NameUnauthenticated) {
		t.Fatalf("Discover error = %v, want %s", err, NameUnauthenticated)
	}
	if got := len(p.seenCopy()); got != 1 {
		t.Errorf("peer saw %d requests, want 1", got)
	}
}

// TestCredentialRequiredRefusesBeforeSending is the §5.3.1 posture gate: a card
// that requires bearerAuth is refused for a client holding no credential, and
// the peer never sees a JSON-RPC request.
func TestCredentialRequiredRefusesBeforeSending(t *testing.T) {
	p := newPeer(t)
	p.buildCard(a2a.ServerInfo{Origin: p.origin(), Version: "test", AuthTokenSet: true})
	c := newClient(t, p, nil)

	_, err := c.Discover(context.Background())
	if !isNamed(err, ErrCredentialRequired, NameCredentialRequired) {
		t.Fatalf("Discover error = %v, want %s", err, NameCredentialRequired)
	}
	if !strings.Contains(err.Error(), "bearerAuth") {
		t.Errorf("the refusal does not name the required scheme: %v", err)
	}
	if hits := len(p.rpcRequests()); hits != 0 {
		t.Errorf("peer saw %d JSON-RPC requests, want 0 — the refusal must precede any send", hits)
	}
}

// TestBearerCredentialSatisfiesTheCard covers the satisfied posture: the
// credential is read from a file, the card requires bearerAuth, and the peer
// sees the header on discovery and on the send.
func TestBearerCredentialSatisfiesTheCard(t *testing.T) {
	p := newPeer(t)
	p.requireAuth = "token-abc"
	p.cardAuth = true
	p.buildCard(a2a.ServerInfo{Origin: p.origin(), Version: "test", AuthTokenSet: true})

	path := writeCredentialFile(t, "token-abc\n", 0o600)
	cred, err := CredentialFromFile(bearerAuthSchemeName, path)
	if err != nil {
		t.Fatalf("CredentialFromFile: %v", err)
	}
	c := newClient(t, p, func(o *Options) { o.Credentials = []Credential{cred} })
	card, err := c.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if _, err := c.Send(context.Background(), card, SendRequest{Text: "x"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	for _, s := range p.seenCopy() {
		if s.auth != "Bearer token-abc" {
			t.Errorf("%s %s carried Authorization %q, want the bearer token", s.method, s.path, s.auth)
		}
	}
}

// TestAPIKeySchemeFromCard covers a peer that declares its own apiKey scheme:
// the header name comes from the card, not from this client's assumptions.
func TestAPIKeySchemeFromCard(t *testing.T) {
	p := newPeer(t)
	card := map[string]any{
		"name":        "foreign",
		"description": "a peer with its own scheme name",
		"version":     "9",
		"supportedInterfaces": []any{map[string]any{
			"url": p.origin() + a2a.JSONRPCBindingPath, "protocolBinding": "JSONRPC",
			"protocolVersion": "1.0",
		}},
		"capabilities":       map[string]any{"streaming": false, "pushNotifications": false},
		"defaultInputModes":  []any{"application/json"},
		"defaultOutputModes": []any{"application/json"},
		"skills":             []any{},
		"securitySchemes": map[string]any{
			"peerKey": map[string]any{"apiKeySecurityScheme": map[string]any{
				"location": "header", "name": "X-Peer-Key",
			}},
		},
		"securityRequirements": []any{map[string]any{
			"schemes": map[string]any{"peerKey": map[string]any{"list": []any{}}},
		}},
	}
	raw, err := json.Marshal(card)
	if err != nil {
		t.Fatalf("marshal card: %v", err)
	}
	p.mu.Lock()
	p.card = raw
	p.mu.Unlock()
	p.requireHeader = "X-Peer-Key"
	p.headerValue = "key-1"

	path := writeCredentialFile(t, "key-1", 0o600)
	cred, err := CredentialFromFile("peerKey", path)
	if err != nil {
		t.Fatalf("CredentialFromFile: %v", err)
	}
	c := newClient(t, p, func(o *Options) { o.Credentials = []Credential{cred} })
	card2, err := c.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if _, err := c.Send(context.Background(), card2, SendRequest{Text: "x"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := p.rpcRequests()[0].headers.Get("X-Peer-Key"); got != "key-1" {
		t.Errorf("X-Peer-Key = %q, want key-1 (the header the card names)", got)
	}
}

// TestMalformedCardsAreRefused covers the card shapes that must not be silently
// defaulted into a request.
func TestMalformedCardsAreRefused(t *testing.T) {
	cases := map[string]string{
		"empty":              ``,
		"notJSON":            `{"name":`,
		"array":              `[]`,
		"noName":             `{"description":"x","version":"1","supportedInterfaces":[{"url":"http://h/a2a","protocolBinding":"JSONRPC"}],"capabilities":{},"defaultInputModes":[],"defaultOutputModes":[],"skills":[]}`,
		"noInterfaces":       `{"name":"a","description":"x","version":"1","supportedInterfaces":[],"capabilities":{},"defaultInputModes":[],"defaultOutputModes":[],"skills":[]}`,
		"noJSONRPCInterface": `{"name":"a","description":"x","version":"1","supportedInterfaces":[{"url":"http://h/grpc","protocolBinding":"GRPC"}],"capabilities":{},"defaultInputModes":[],"defaultOutputModes":[],"skills":[]}`,
		"badInterfaceURL":    `{"name":"a","description":"x","version":"1","supportedInterfaces":[{"url":"/relative","protocolBinding":"JSONRPC"}],"capabilities":{},"defaultInputModes":[],"defaultOutputModes":[],"skills":[]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			p := newPeer(t)
			p.mu.Lock()
			p.card = []byte(body)
			p.mu.Unlock()
			_, err := newClient(t, p, nil).Discover(context.Background())
			if !isNamed(err, ErrCardMalformed, NameCardMalformed) {
				t.Fatalf("Discover error = %v, want %s", err, NameCardMalformed)
			}
		})
	}
}

// TestPeerRefusalIsReportedVerbatim covers the peer refusing an operation with
// its own JSON-RPC error: the code and message reach the caller, the failure is
// PEER_REFUSED (not a transport or shape failure), and it is not retried.
func TestPeerRefusalIsReportedVerbatim(t *testing.T) {
	p := newPeer(t)
	// A method name the peer does not implement: it answers -32601, exactly as
	// a peer that speaks a different dialect would.
	c := newClient(t, p, func(o *Options) {
		o.Methods = &Operations{SendMessage: a2a.MethodSendMessage, GetTask: "GetTaskFuture", CancelTask: a2a.MethodCancelTask, Name: "test"}
	})
	card, err := c.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	_, err = c.GetTask(context.Background(), card, "task-1", nil)
	if !isNamed(err, ErrPeerRefused, NamePeerRefused) {
		t.Fatalf("GetTask error = %v, want %s", err, NamePeerRefused)
	}
	var clientErr *Error
	if !errors.As(err, &clientErr) || clientErr.RPC == nil {
		t.Fatalf("the failure does not carry the peer's error object: %v", err)
	}
	if clientErr.RPC.Code != a2a.CodeMethodNotFound {
		t.Errorf("peer error code = %d, want %d", clientErr.RPC.Code, a2a.CodeMethodNotFound)
	}
	if hits := len(p.rpcRequests()); hits != 1 {
		t.Errorf("peer saw %d POSTs, want 1 — a refusal is not retried", hits)
	}
}

// TestUncorrelatedResponseIsInvalid covers the id-correlation rule: a
// well-formed JSON-RPC answer for a DIFFERENT id is not this request's answer.
func TestUncorrelatedResponseIsInvalid(t *testing.T) {
	p := newPeer(t)
	c := newClient(t, p, nil)
	card, err := c.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	p.mu.Lock()
	p.rpcBody = `{"jsonrpc":"2.0","id":"someone-else","result":{"id":"task-1"}}`
	p.mu.Unlock()
	_, err = c.GetTask(context.Background(), card, "task-1", nil)
	if !isNamed(err, ErrInvalidResponse, NameInvalidResponse) {
		t.Fatalf("GetTask error = %v, want %s for a non-correlated response", err, NameInvalidResponse)
	}
}

// TestCredentialIsNeverPrinted is the "never printed, never logged" rule.
func TestCredentialIsNeverPrinted(t *testing.T) {
	path := writeCredentialFile(t, "super-secret-value", 0o600)
	cred, err := CredentialFromFile(bearerAuthSchemeName, path)
	if err != nil {
		t.Fatalf("CredentialFromFile: %v", err)
	}
	rendered := fmt.Sprintf("%v|%+v|%#v|%s", cred, cred, cred, cred)
	if strings.Contains(rendered, "super-secret-value") {
		t.Fatalf("a fmt verb printed the secret: %s", rendered)
	}
	raw, err := json.Marshal(cred)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), "super-secret-value") {
		t.Fatalf("JSON serialization leaked the secret: %s", raw)
	}
	if !strings.Contains(string(raw), "redacted") {
		t.Errorf("JSON form does not state that the value is redacted: %s", raw)
	}
}

// TestCredentialSourcesCoverTheirFailures covers the env/file constructors'
// refusals — each named, none printing a value.
func TestCredentialSourcesCoverTheirFailures(t *testing.T) {
	if _, err := CredentialFromEnv("bearerAuth", "CRIER_TEST_MISSING", func(string) string { return "" }); !isNamed(err, ErrCredentialInvalid, NameCredentialInvalid) {
		t.Errorf("unset env var error = %v, want %s", err, NameCredentialInvalid)
	}
	if _, err := CredentialFromEnv("", "X", func(string) string { return "v" }); !isNamed(err, ErrCredentialInvalid, NameCredentialInvalid) {
		t.Errorf("empty scheme error = %v, want %s", err, NameCredentialInvalid)
	}
	if _, err := CredentialFromFile(bearerAuthSchemeName, filepath.Join(t.TempDir(), "absent")); !isNamed(err, ErrCredentialInvalid, NameCredentialInvalid) {
		t.Errorf("missing file error = %v, want %s", err, NameCredentialInvalid)
	}
	if _, err := CredentialFromFile(bearerAuthSchemeName, writeCredentialFile(t, "   \n", 0o600)); !isNamed(err, ErrCredentialInvalid, NameCredentialInvalid) {
		t.Errorf("blank file error = %v, want %s", err, NameCredentialInvalid)
	}
	if _, err := CredentialFromFile(bearerAuthSchemeName, writeCredentialFile(t, "v", 0o644)); !isNamed(err, ErrCredentialInvalid, NameCredentialInvalid) {
		t.Errorf("world-readable file error = %v, want %s", err, NameCredentialInvalid)
	}

	cred, err := CredentialFromEnv(bearerAuthSchemeName, "CRIER_TEST_TOKEN", func(name string) string {
		if name == "CRIER_TEST_TOKEN" {
			return "from-env\n"
		}
		return ""
	})
	if err != nil {
		t.Fatalf("CredentialFromEnv: %v", err)
	}
	if cred.secret != "from-env" {
		t.Errorf("env credential value = %q, want the trimmed value", cred.secret)
	}
	if cred.Source != "env:CRIER_TEST_TOKEN" {
		t.Errorf("source = %q, want env:CRIER_TEST_TOKEN", cred.Source)
	}
}

// TestConfigRefusals covers the option validation a caller can get wrong.
func TestConfigRefusals(t *testing.T) {
	cases := map[string]Options{
		"noOrigin":   {},
		"badScheme":  {Origin: "ftp://peer"},
		"noHost":     {Origin: "http://"},
		"withQuery":  {Origin: "http://peer/?x=1"},
		"emptyCred":  {Origin: "http://peer", Credentials: []Credential{{Scheme: "bearerAuth"}}},
		"halfMethod": {Origin: "http://peer", Methods: &Operations{SendMessage: "SendMessage"}},
	}
	for name, opts := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := New(opts); err == nil {
				t.Fatalf("New(%+v) accepted an unusable configuration", opts)
			}
		})
	}
	if _, err := New(Options{Origin: "http://peer/"}); err != nil {
		t.Fatalf("New refused a valid origin: %v", err)
	}
}

// isNamed reports whether err is this package's failure with the given sentinel
// AND name — both halves, because the CLI relies on the name and callers on the
// sentinel.
func isNamed(err error, kind error, name string) bool {
	if err == nil {
		return false
	}
	if !errors.Is(err, kind) {
		return false
	}
	return ErrorName(err) == name
}

// writeCredentialFile writes a credential fixture with an explicit mode.
func writeCredentialFile(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "credential")
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatalf("write credential: %v", err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod credential: %v", err)
	}
	return path
}
