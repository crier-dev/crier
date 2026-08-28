package registry

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"
)

// newRemoteTestServer spins a real registry HTTP handler (signing disabled)
// over an in-memory store — the same wire the RemoteStore will talk to.
func newRemoteTestServer(t *testing.T) (*httptest.Server, *MemoryStore) {
	t.Helper()
	store := NewMemoryStore()
	h := NewHandler(store)
	h.SetRequireAgentSig(false)
	r := mux.NewRouter()
	r.HandleFunc("/agents", h.HandleRegister).Methods(http.MethodPost)
	r.HandleFunc("/agents", h.HandleListAgents).Methods(http.MethodGet)
	r.HandleFunc("/agents/{id}", h.HandleGetAgent).Methods(http.MethodGet)
	r.HandleFunc("/agents/{id}", h.HandleUnregister).Methods(http.MethodDelete)
	r.HandleFunc("/agents/{id}/inbox", h.HandleDeliver).Methods(http.MethodPost)
	r.HandleFunc("/agents/{id}/inbox", h.HandleRetrieve).Methods(http.MethodGet)
	r.HandleFunc("/agents/{id}/inbox/ack", h.HandleAck).Methods(http.MethodPost)
	r.HandleFunc("/agents/{id}/inbox/stats", h.HandleStats).Methods(http.MethodGet)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv, store
}

func TestRemoteStore_RegisterGetListUnregister(t *testing.T) {
	srv, _ := newRemoteTestServer(t)
	rs := NewRemoteStore(srv.URL, "remote-client", "")

	// Build the key the way real clients do: hex string -> raw bytes.
	keyBytes, err := hex.DecodeString(strings.Repeat("ab", 32))
	if err != nil {
		t.Fatalf("decode key: %v", err)
	}
	agent := &Agent{ID: "alice", PublicKey: HexKey(keyBytes), Capabilities: []string{"chat"}}
	if err := rs.Register(agent); err != nil {
		t.Fatalf("Register: %v", err)
	}

	got, err := rs.Get("alice")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != "alice" || hex.EncodeToString([]byte(got.PublicKey)) != strings.Repeat("ab", 32) {
		t.Fatalf("Get = %+v", got)
	}

	list := rs.List()
	if len(list) != 1 || list[0].ID != "alice" {
		t.Fatalf("List = %+v", list)
	}

	if err := rs.Unregister("alice"); err != nil {
		t.Fatalf("Unregister: %v", err)
	}
	if _, err := rs.Get("alice"); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("Get after unregister: %v (want ErrAgentNotFound)", err)
	}
}

func TestRemoteStore_DeliverRetrieveAck(t *testing.T) {
	srv, _ := newRemoteTestServer(t)
	rs := NewRemoteStore(srv.URL, "bob", "")

	keyBytes, err := hex.DecodeString(strings.Repeat("cd", 32))
	if err != nil {
		t.Fatalf("decode key: %v", err)
	}
	if err := rs.Register(&Agent{ID: "bob", PublicKey: HexKey(keyBytes)}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	payload := json.RawMessage(`{"kind":"question","text":"which plot?"}`)
	if err := rs.Deliver("bob", &InboxEntry{AgentID: "bob", Payload: payload}); err != nil {
		t.Fatalf("Deliver: %v", err)
	}

	entries, leaseID, err := rs.Retrieve("bob", 30*time.Second, 10)
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	if len(entries) != 1 || leaseID == "" {
		t.Fatalf("Retrieve = %d entries lease=%q", len(entries), leaseID)
	}
	if string(entries[0].Payload) != string(payload) {
		t.Fatalf("payload = %s", entries[0].Payload)
	}

	if err := rs.Ack("bob", leaseID, []string{entries[0].ID}); err != nil {
		t.Fatalf("Ack: %v", err)
	}

	again, _, err := rs.Retrieve("bob", 30*time.Second, 10)
	if err != nil {
		t.Fatalf("Retrieve after ack: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("inbox not empty after ack: %d", len(again))
	}

	depth, leased, _, err := rs.Stats("bob")
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if depth != 0 || leased != 0 {
		t.Fatalf("Stats = depth %d leased %d", depth, leased)
	}
}

func TestRemoteStore_Errors(t *testing.T) {
	srv, _ := newRemoteTestServer(t)
	rs := NewRemoteStore(srv.URL+"/", "ghost", "")

	if _, err := rs.Get("ghost"); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("Get unknown: %v (want ErrAgentNotFound)", err)
	}
	if err := rs.Deliver("ghost", &InboxEntry{Payload: json.RawMessage(`{}`)}); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("Deliver unknown: %v (want ErrAgentNotFound)", err)
	}
	if _, _, err := rs.Retrieve("ghost", 30*time.Second, 5); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("Retrieve unknown: %v (want ErrAgentNotFound)", err)
	}

	// Duplicate register -> ErrAgentExists.
	keyBytes, err := hex.DecodeString(strings.Repeat("ef", 32))
	if err != nil {
		t.Fatalf("decode key: %v", err)
	}
	rs.Register(&Agent{ID: "dup", PublicKey: HexKey(keyBytes)})
	if err := rs.Register(&Agent{ID: "dup", PublicKey: HexKey(keyBytes)}); !errors.Is(err, ErrAgentExists) {
		t.Fatalf("Register dup: %v (want ErrAgentExists)", err)
	}
}

func TestRemoteStore_SendsAgentIDHeader(t *testing.T) {
	var gotHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("X-Agent-ID")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"agents":[]}`))
	}))
	t.Cleanup(srv.Close)

	rs := NewRemoteStore(srv.URL, "header-check", "")
	rs.List()
	if gotHeader != "header-check" {
		t.Fatalf("X-Agent-ID = %q", gotHeader)
	}
}
