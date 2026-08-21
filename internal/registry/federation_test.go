package registry

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/mux"

	"github.com/totalwindupflightsystems/crier/internal/federation"
)

// fedRemoteRelay is a fake linked relay: it records the forwarded deliver
// request and answers with a fixed status/body.
type fedRemoteRelay struct {
	t       *testing.T
	status  int
	body    string
	gotPath string
	gotBody []byte
	gotHop  string
	calls   int
}

func (r *fedRemoteRelay) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		r.calls++
		r.gotPath = req.URL.Path
		r.gotHop = req.Header.Get(federation.HopHeader)
		buf := new(bytes.Buffer)
		buf.ReadFrom(req.Body)
		r.gotBody = buf.Bytes()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(r.status)
		w.Write([]byte(r.body))
	}
}

// setupFedRouter builds the registry routes with federation wired to the
// given remote relay URLs.
func setupFedRouter(store Store, links []string) (*mux.Router, *Handler) {
	handler := NewHandler(store)
	if len(links) > 0 {
		handler.SetFederationClient(federation.NewClient(links, 0))
	}
	r := mux.NewRouter()
	r.HandleFunc("/agents/{id}/inbox", handler.HandleDeliver).Methods("POST")
	return r, handler
}

func deliverTo(t *testing.T, router *mux.Router, id string, reqBody []byte, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/agents/"+id+"/inbox", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func TestHandleDeliverFederationFallback(t *testing.T) {
	// The remote relay answers like a real relay-2 in blocking webhook
	// mode: 200 with {id, reply, session_id, request_id}. The reply must
	// come back to the original caller verbatim.
	remote := &fedRemoteRelay{t: t, status: http.StatusOK,
		body: `{"id":"r2-abc","reply":{"text":"pong from relay-2"},"session_id":"sess-1","request_id":"req-1"}`}
	srv := httptest.NewServer(remote.handler())
	defer srv.Close()

	store := setupTestStore(t)
	router, _ := setupFedRouter(store, []string{srv.URL})

	reqBody := []byte(`{"payload":{"text":"ping"},"sender":"agent-local","delivery_mode":"blocking","session_id":"sess-1","request_id":"req-1"}`)
	rec := deliverTo(t, router, "agent-remote", reqBody, nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.String() != remote.body {
		t.Errorf("body = %s, want the remote response relayed verbatim", rec.Body.String())
	}
	if remote.calls != 1 {
		t.Fatalf("remote calls = %d, want 1", remote.calls)
	}
	if remote.gotPath != "/agents/agent-remote/inbox" {
		t.Errorf("remote path = %s, want /agents/agent-remote/inbox", remote.gotPath)
	}
	if remote.gotHop != "1" {
		t.Errorf("remote hop header = %q, want \"1\"", remote.gotHop)
	}
	// The forwarded body must carry the original deliver JSON (so relay-2
	// can route by sender/session/request ids).
	var forwarded map[string]any
	if err := json.Unmarshal(remote.gotBody, &forwarded); err != nil {
		t.Fatalf("forwarded body is not JSON: %v", err)
	}
	if forwarded["sender"] != "agent-local" || forwarded["delivery_mode"] != "blocking" || forwarded["request_id"] != "req-1" {
		t.Errorf("forwarded body = %s, want original deliver fields", remote.gotBody)
	}
}

func TestHandleDeliverFederationAllLinks404(t *testing.T) {
	remote := &fedRemoteRelay{t: t, status: http.StatusNotFound, body: `{"error":"agent not found"}`}
	srv := httptest.NewServer(remote.handler())
	defer srv.Close()

	router, _ := setupFedRouter(setupTestStore(t), []string{srv.URL})
	rec := deliverTo(t, router, "ghost", []byte(`{"payload":{}}`), nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 when every link 404s", rec.Code)
	}
	if remote.calls != 1 {
		t.Errorf("remote calls = %d, want 1", remote.calls)
	}
}

func TestHandleDeliverFederationDisabledKeepsLocal404(t *testing.T) {
	router, _ := setupFedRouter(setupTestStore(t), nil)
	rec := deliverTo(t, router, "ghost", []byte(`{"payload":{}}`), nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (unchanged local behavior without links)", rec.Code)
	}
}

func TestHandleDeliverFederationHopGuard(t *testing.T) {
	// A request that already crossed a link (hop marker set) must never be
	// forwarded again — relay-2 answering relay-1's forward would otherwise
	// bounce forever when links are mutual.
	remote := &fedRemoteRelay{t: t, status: http.StatusOK, body: `{"id":"x"}`}
	srv := httptest.NewServer(remote.handler())
	defer srv.Close()

	router, _ := setupFedRouter(setupTestStore(t), []string{srv.URL})
	rec := deliverTo(t, router, "ghost", []byte(`{"payload":{}}`), map[string]string{federation.HopHeader: "1"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (hop guard)", rec.Code)
	}
	if remote.calls != 0 {
		t.Errorf("remote calls = %d, want 0 (hop must not forward)", remote.calls)
	}
}

func TestHandleDeliverFederationLocalAgentUnchanged(t *testing.T) {
	// A locally-registered agent must be delivered to its local inbox — the
	// federation fallback only fires for unknown agents.
	remote := &fedRemoteRelay{t: t, status: http.StatusOK, body: `{"id":"x"}`}
	srv := httptest.NewServer(remote.handler())
	defer srv.Close()

	store := setupTestStore(t)
	registerTestAgent(t, store)
	router, _ := setupFedRouter(store, []string{srv.URL})

	rec := deliverTo(t, router, "agent-1", []byte(`{"payload":{"msg":"hello"}}`), nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (local inbox delivery)", rec.Code)
	}
	if remote.calls != 0 {
		t.Errorf("remote calls = %d, want 0 (local agent must not be forwarded)", remote.calls)
	}
}

func TestHandleDeliverFederationFirstNon404Wins(t *testing.T) {
	// relay-1 404s (does not know the agent), relay-2 answers 202 — the
	// second link wins and its async accept is relayed verbatim.
	miss := &fedRemoteRelay{t: t, status: http.StatusNotFound, body: `{"error":"agent not found"}`}
	hit := &fedRemoteRelay{t: t, status: http.StatusAccepted, body: `{"id":"async-1"}`}
	srv1 := httptest.NewServer(miss.handler())
	srv2 := httptest.NewServer(hit.handler())
	defer srv1.Close()
	defer srv2.Close()

	router, _ := setupFedRouter(setupTestStore(t), []string{srv1.URL, srv2.URL})
	rec := deliverTo(t, router, "agent-remote", []byte(`{"payload":{}}`), nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 from the second link", rec.Code)
	}
	if rec.Body.String() != `{"id":"async-1"}` {
		t.Errorf("body = %s, want the 202 body relayed verbatim", rec.Body.String())
	}
	if miss.calls != 1 || hit.calls != 1 {
		t.Errorf("calls = (%d, %d), want (1, 1)", miss.calls, hit.calls)
	}
}
