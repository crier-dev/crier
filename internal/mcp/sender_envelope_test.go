package mcp

// Envelope-sender stamping on bridge deliveries.
//
// A bridge delivery used to arrive UNATTRIBUTED: the receiver saw
// raw["sender"] == null and could attribute the message only by a payload
// convention, while CR-FEAT-025's terminal-outcome reporting (a
// MESSAGE_EXPIRED receipt is written into the SENDER's inbox) had no address
// to report to. The bridge knows its own id (CRIER_AGENT_ID) and now stamps it
// as the envelope sender on every delivery path it owns.
//
// These tests pin both halves of that on the REAL registry HTTP handler: the
// WIRE (the deliver request body the RemoteStore produces) and the STORED row
// a read-back returns. They also pin the half that must NOT change — a bridge
// with no identity stores no sender and errors on nothing.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/mux"

	"github.com/crier-dev/crier/internal/registry"
)

// deliverBodyRecorder captures the body of every POST .../inbox the test
// server receives, so a test asserts the exact wire the RemoteStore produced
// rather than only the row that survived storage.
type deliverBodyRecorder struct {
	mu     sync.Mutex
	bodies []map[string]any
}

func (r *deliverBodyRecorder) record(body []byte) {
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		decoded = map[string]any{"__undecodable__": string(body)}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bodies = append(r.bodies, decoded)
}

func (r *deliverBodyRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.bodies)
}

func (r *deliverBodyRecorder) at(i int) map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	if i < 0 || i >= len(r.bodies) {
		return nil
	}
	return r.bodies[i]
}

// newBridgeTestServer is the RemoteStore's real counterpart: the registry HTTP
// handler over an in-memory store, per-agent signing disabled (the bridge's
// signing itself is exercised in the registry package), wrapped so every
// deliver body is recorded before the handler sees it.
func newBridgeTestServer(t *testing.T) (*httptest.Server, *registry.MemoryStore, *deliverBodyRecorder) {
	t.Helper()
	store := registry.NewMemoryStore()
	h := registry.NewHandler(store)
	h.SetRequireAgentSig(false)
	r := mux.NewRouter()
	r.HandleFunc("/agents", h.HandleRegister).Methods(http.MethodPost)
	r.HandleFunc("/agents/{id}", h.HandleGetAgent).Methods(http.MethodGet)
	r.HandleFunc("/agents/{id}/inbox", h.HandleDeliver).Methods(http.MethodPost)
	r.HandleFunc("/agents/{id}/inbox", h.HandleRetrieve).Methods(http.MethodGet)
	r.HandleFunc("/agents/{id}/inbox/ack", h.HandleAck).Methods(http.MethodPost)

	rec := &deliverBodyRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/inbox") {
			body, err := io.ReadAll(req.Body)
			if err == nil {
				rec.record(body)
				req.Body = io.NopCloser(bytes.NewReader(body))
			}
		}
		r.ServeHTTP(w, req)
	}))
	t.Cleanup(srv.Close)
	return srv, store, rec
}

// assertInboxSender reads exactly one message from the agent's inbox (through
// whatever Store the caller passes) and asserts its stored envelope sender.
func assertInboxSender(t *testing.T, store registry.Store, agentID, want string) {
	t.Helper()
	entries, _, err := store.Retrieve(agentID, 30*time.Second, 10)
	if err != nil {
		t.Fatalf("Retrieve(%s): %v", agentID, err)
	}
	if len(entries) != 1 {
		t.Fatalf("%s inbox = %d entries, want 1", agentID, len(entries))
	}
	if entries[0].Sender != want {
		t.Fatalf("%s stored sender = %q, want %q", agentID, entries[0].Sender, want)
	}
}

// TestRemoteBridgeSendMessageStampsEnvelopeSender is the deterministic test
// for the finding: a remote-bridge send_message against an httptest server
// carries the bridge's own agent id as the envelope sender, and the stored row
// returned by a read-back carries it too.
func TestRemoteBridgeSendMessageStampsEnvelopeSender(t *testing.T) {
	srv, _, rec := newBridgeTestServer(t)
	rs := registry.NewRemoteStore(srv.URL, "bridge-x", "")
	s := NewWithOptions(rs, Options{AgentID: "bridge-x"})

	registerAgentIn(t, s, "target")

	resp := callTool(t, s, "send_message", SendMessageInput{
		AgentID: "target",
		Payload: map[string]any{"probe": "sender-stamp"},
	})
	if text, isErr := parseToolResult(t, resp); isErr {
		t.Fatalf("send_message error: %s", text)
	}

	// The wire: the deliver request body names the bridge as the sender.
	if rec.count() != 1 {
		t.Fatalf("deliver requests recorded = %d, want 1", rec.count())
	}
	body := rec.at(0)
	if got, _ := body["sender"].(string); got != "bridge-x" {
		t.Fatalf("deliver request body sender = %#v, want %q (body: %v)", body["sender"], "bridge-x", body)
	}
	if _, ok := body["payload"]; !ok {
		t.Fatalf("deliver request body lost its payload: %v", body)
	}

	// Parity: the row the read-back returns carries the same sender.
	viewer := registry.NewRemoteStore(srv.URL, "target", "")
	assertInboxSender(t, viewer, "target", "bridge-x")
}

// TestRemoteBridgeSenderIsNotTheTransportCredential pins WHICH identity is
// stamped: the bridge's own (Options.AgentID), never the store's transport
// credential, and when the bridge has no identity the member is ABSENT rather
// than sender:"" — the single representation of "not recorded".
func TestRemoteBridgeSenderIsNotTheTransportCredential(t *testing.T) {
	srv, _, rec := newBridgeTestServer(t)
	// The transport authenticates as one agent; the bridge claims no identity.
	rs := registry.NewRemoteStore(srv.URL, "transport-x", "")
	s := NewWithOptions(rs, Options{})

	registerAgentIn(t, s, "target")

	resp := callTool(t, s, "send_message", SendMessageInput{
		AgentID: "target",
		Payload: map[string]any{"probe": "no-identity"},
	})
	if text, isErr := parseToolResult(t, resp); isErr {
		t.Fatalf("send_message error: %s", text)
	}
	body := rec.at(0)
	if _, present := body["sender"]; present {
		t.Fatalf("deliver request body carries sender %#v with no bridge identity, want the member absent", body["sender"])
	}
}

// TestDeliveryPathsStampBridgeIdentity covers the same rule on the other
// delivery paths (deliver_message and the ask_agent request leg) and, on the
// same handlers, the guard that must not change: with no bridge identity every
// path stores none and errors on nothing.
func TestDeliveryPathsStampBridgeIdentity(t *testing.T) {
	s, store := newTestServer(t, Options{AgentID: "alice"})
	registerAgentIn(t, s, "alice")
	registerAgentIn(t, s, "t-deliver")
	registerAgentIn(t, s, "t-send")
	registerAgentIn(t, s, "t-ask")

	resp := callTool(t, s, "deliver_message", DeliverMessageInput{
		AgentID: "t-deliver", Payload: json.RawMessage(`{"kind":"note"}`),
	})
	if text, isErr := parseToolResult(t, resp); isErr {
		t.Fatalf("deliver_message error: %s", text)
	}
	assertInboxSender(t, store, "t-deliver", "alice")

	resp = callTool(t, s, "send_message", SendMessageInput{
		AgentID: "t-send", Payload: map[string]any{"kind": "note"},
	})
	if text, isErr := parseToolResult(t, resp); isErr {
		t.Fatalf("send_message error: %s", text)
	}
	assertInboxSender(t, store, "t-send", "alice")

	// ask_agent's REQUEST leg is a delivery: it must carry the asker too. A
	// 1s timeout keeps this bounded; the request is delivered before the
	// bridge starts polling, so the row is there when the call returns.
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = callTool(t, s, "ask_agent", AskAgentInput{
			AgentID: "t-ask", Payload: map[string]any{"kind": "question"}, TimeoutS: 1,
		})
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("ask_agent did not return")
	}
	assertInboxSender(t, store, "t-ask", "alice")

	// No bridge identity: today's behaviour on both specs-level paths.
	plain, pstore := newTestServer(t, Options{})
	registerAgentIn(t, plain, "p-deliver")
	registerAgentIn(t, plain, "p-send")

	resp = callTool(t, plain, "deliver_message", DeliverMessageInput{
		AgentID: "p-deliver", Payload: json.RawMessage(`{"kind":"note"}`),
	})
	if text, isErr := parseToolResult(t, resp); isErr {
		t.Fatalf("deliver_message (no identity) error: %s", text)
	}
	assertInboxSender(t, pstore, "p-deliver", "")

	resp = callTool(t, plain, "send_message", SendMessageInput{
		AgentID: "p-send", Payload: map[string]any{"kind": "note"},
	})
	if text, isErr := parseToolResult(t, resp); isErr {
		t.Fatalf("send_message (no identity) error: %s", text)
	}
	assertInboxSender(t, pstore, "p-send", "")
}
