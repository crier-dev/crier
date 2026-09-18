package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/registry"
)

// DF-CRIER-217 / DF-CRIER-242: register_agent must answer the STORE's view of
// the registered row, never the caller-supplied struct the handler just built.
// The two defects were the same defect observed on two backends: registered
// through the remote bridge (CRIER_HTTP_URL set) the tool echoed
// status="" / registered_at=last_seen=0001-01-01T00:00:00Z while the SAME
// session's get_agent and the server's own GET /agents/<id> answered the real
// row — because RemoteStore.Register POSTs and discards the response, and the
// handler returned the struct it had handed to Register.

// storedRowSpyStore is a Store whose Register persists ONLY what it was handed
// and deliberately writes nothing back into the caller's struct — the remote
// bridge's shape, and the shape any future backend can have. Get answers a
// fully-populated row. register_agent must therefore answer Get's row; a
// handler that echoes its own constructed struct cannot pass this test on any
// backend, which is exactly the contract being pinned.
type storedRowSpyStore struct {
	*registry.MemoryStore // the rest of the Store interface, unused here

	row *registry.Agent // what Get answers
	// getErr, when set, makes Get fail — the fallback path.
	getErr error

	registerCalls int
	getCalls      int
	passed        *registry.Agent // the struct the handler handed to Register
}

func newStoredRowSpyStore(row *registry.Agent) *storedRowSpyStore {
	return &storedRowSpyStore{MemoryStore: registry.NewMemoryStore(), row: row}
}

func (s *storedRowSpyStore) Register(agent *registry.Agent) error {
	s.registerCalls++
	s.passed = agent
	// Nothing is written back into `agent` — the caller's struct stays zero
	// valued in status/registered_at/last_seen, exactly as it does through
	// RemoteStore.
	return nil
}

func (s *storedRowSpyStore) Get(id string) (*registry.Agent, error) {
	s.getCalls++
	if s.getErr != nil {
		return nil, s.getErr
	}
	if s.row == nil || s.row.ID != id {
		return nil, fmt.Errorf("%w: %q", registry.ErrAgentNotFound, id)
	}
	return s.row, nil
}

var _ registry.Store = (*storedRowSpyStore)(nil)

// TestRegisterAgentAnswersStoredRowNotCallerStruct is the regression test for
// DF-CRIER-217/242. RED on the pre-fix handler: `return agent, nil` answered
// the constructed struct, so status was "" and both timestamps were the zero
// time even though the store held a fully-populated row.
func TestRegisterAgentAnswersStoredRowNotCallerStruct(t *testing.T) {
	storedAt := time.Date(2026, 1, 2, 3, 4, 5, 123456789, time.UTC)
	key := strings.Repeat("ab", 32) // 64 hex chars = one ed25519 public key
	row := &registry.Agent{
		ID:           "spy-agent",
		PublicKey:    registry.HexKey(strings.Repeat("\xab", 32)),
		Capabilities: []string{"stored-only"},
		// StatusOffline is never produced by a registration path (both real
		// backends set online), so a response carrying it can only have come
		// from Get.
		Status:       registry.StatusOffline,
		RegisteredAt: storedAt,
		LastSeen:     storedAt,
	}
	spy := newStoredRowSpyStore(row)
	s := New(spy)

	resp := callTool(t, s, "register_agent", RegisterAgentInput{ID: "spy-agent", PublicKey: key})
	text, isErr := parseToolResult(t, resp)
	if isErr {
		t.Fatalf("expected success, got error: %s", text)
	}

	// Premise: the backend really is non-mutating. Without this the test could
	// silently stop proving anything if Register started writing back.
	if spy.passed == nil {
		t.Fatal("Register was never called")
	}
	if spy.passed.Status != "" || !spy.passed.RegisteredAt.IsZero() || !spy.passed.LastSeen.IsZero() {
		t.Fatalf("premise broken: Register mutated the caller's struct (status=%q registered_at=%v last_seen=%v)",
			spy.passed.Status, spy.passed.RegisteredAt, spy.passed.LastSeen)
	}
	if spy.registerCalls != 1 {
		t.Errorf("Register calls = %d, want 1", spy.registerCalls)
	}
	if spy.getCalls != 1 {
		t.Errorf("Get calls = %d, want exactly one read-back on the success path", spy.getCalls)
	}

	var got registry.Agent
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if got.Status != registry.StatusOffline {
		t.Errorf("status = %q, want %q (the STORE's value, not the caller's empty struct)", got.Status, registry.StatusOffline)
	}
	if !got.RegisteredAt.Equal(storedAt) {
		t.Errorf("registered_at = %v, want %v (the STORE's value)", got.RegisteredAt, storedAt)
	}
	if !got.LastSeen.Equal(storedAt) {
		t.Errorf("last_seen = %v, want %v (the STORE's value)", got.LastSeen, storedAt)
	}
	if !reflect.DeepEqual(got.Capabilities, []string{"stored-only"}) {
		t.Errorf("capabilities = %v, want [stored-only] (from the store)", got.Capabilities)
	}
	if strings.Contains(text, "0001-01-01T00:00:00Z") {
		t.Errorf("response carries a zero timestamp: %s", text)
	}
	if got.ID != "spy-agent" {
		t.Errorf("id = %q, want spy-agent", got.ID)
	}
	if got.PublicKey == nil || len(got.PublicKey) != 32 {
		t.Errorf("public_key = %v, want the 32-byte key the caller supplied", got.PublicKey)
	}
}

// TestRegisterAgentReadBackFailureStillAnswersSuccess pins the error-path
// contract: a read-back that fails must NOT turn a registration the store
// already accepted into a tool error. The constructed struct is answered, the
// same shape the pre-fix handler always answered.
func TestRegisterAgentReadBackFailureStillAnswersSuccess(t *testing.T) {
	spy := newStoredRowSpyStore(nil)
	spy.getErr = errors.New("registry unreachable")
	s := New(spy)

	key := strings.Repeat("cd", 32)
	resp := callTool(t, s, "register_agent", RegisterAgentInput{ID: "flaky-agent", PublicKey: key})
	text, isErr := parseToolResult(t, resp)
	if isErr {
		t.Fatalf("a failed read-back must not fail an accepted registration, got error: %s", text)
	}
	if spy.registerCalls != 1 || spy.getCalls != 1 {
		t.Errorf("Register/Get calls = %d/%d, want 1/1", spy.registerCalls, spy.getCalls)
	}

	var got registry.Agent
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if got.ID != "flaky-agent" {
		t.Errorf("id = %q, want flaky-agent", got.ID)
	}
	if len(got.PublicKey) != 32 {
		t.Errorf("public_key length = %d, want 32 (the caller's key preserved)", len(got.PublicKey))
	}
}

// TestRegisterAgentRemoteBridgeAnswersStoredRow drives the REAL RemoteStore —
// the store cmd/crier-mcp builds when CRIER_HTTP_URL is set — against a server
// that answers POST /agents and GET /agents/<id> with the same stored row (the
// shape the crier server returns: writeJSON(201, agent) after the store
// filled in status/timestamps). register_agent's text must equal the
// get_agent text for the same id, which is the bridge-side form of
// DF-CRIER-217/242: pre-fix the register response carried the zero-valued
// caller struct, so the two differed.
func TestRegisterAgentRemoteBridgeAnswersStoredRow(t *testing.T) {
	key := strings.Repeat("ab", 32)
	storedAt := "2026-09-18T03:19:10.272658937-05:00"
	rowJSON := fmt.Sprintf(
		`{"id":"bridge-agent","public_key":"%s","capabilities":[],"status":"online","registered_at":"%s","last_seen":"%s"}`,
		key, storedAt, storedAt)

	var posts, gets int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/agents":
			atomic.AddInt32(&posts, 1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(rowJSON))
		case r.Method == http.MethodGet && r.URL.Path == "/agents/bridge-agent":
			atomic.AddInt32(&gets, 1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(rowJSON))
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	s := New(registry.NewRemoteStore(srv.URL, "bridge-agent", ""))

	regResp := callTool(t, s, "register_agent", RegisterAgentInput{ID: "bridge-agent", PublicKey: key})
	regText, regErr := parseToolResult(t, regResp)
	if regErr {
		t.Fatalf("register_agent failed: %s", regText)
	}
	if got := atomic.LoadInt32(&posts); got != 1 {
		t.Errorf("POST /agents calls = %d, want 1", got)
	}
	// Exactly one read-back, on the success path (the extra round trip the
	// fix adds on the bridge).
	if got := atomic.LoadInt32(&gets); got != 1 {
		t.Errorf("GET calls after register_agent = %d, want 1 read-back", got)
	}

	getResp := callTool(t, s, "get_agent", GetAgentInput{ID: "bridge-agent"})
	getText, getErr := parseToolResult(t, getResp)
	if getErr {
		t.Fatalf("get_agent failed: %s", getText)
	}

	var regAgent, getAgent map[string]any
	if err := json.Unmarshal([]byte(regText), &regAgent); err != nil {
		t.Fatalf("unmarshal register response: %v", err)
	}
	if err := json.Unmarshal([]byte(getText), &getAgent); err != nil {
		t.Fatalf("unmarshal get response: %v", err)
	}
	if !reflect.DeepEqual(regAgent, getAgent) {
		t.Errorf("register_agent response differs from get_agent for the same id:\n register: %s\n get     : %s", regText, getText)
	}
	if regAgent["status"] != "online" {
		t.Errorf("status = %v, want online (non-empty)", regAgent["status"])
	}
	if regAgent["registered_at"] == "0001-01-01T00:00:00Z" || regAgent["registered_at"] == nil {
		t.Errorf("registered_at = %v, want the server's real timestamp", regAgent["registered_at"])
	}
	if regAgent["last_seen"] == "0001-01-01T00:00:00Z" || regAgent["last_seen"] == nil {
		t.Errorf("last_seen = %v, want the server's real timestamp", regAgent["last_seen"])
	}
}
