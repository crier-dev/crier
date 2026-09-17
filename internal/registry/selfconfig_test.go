package registry

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/mux"

	"github.com/crier-dev/crier/internal/webhook"
)

// selfConfigRouter wires the CR-FEAT-007 surface: PATCH /agents/{id},
// GET /agents (capability filter) and the deliver path.
func selfConfigRouter(h *Handler) *mux.Router {
	r := mux.NewRouter()
	r.HandleFunc("/agents", h.HandleListAgents).Methods("GET")
	r.HandleFunc("/agents/{id}", h.HandleGetAgent).Methods("GET")
	r.HandleFunc("/agents/{id}", h.HandleUpdateAgent).Methods("PATCH")
	r.HandleFunc("/agents/{id}/inbox", h.HandleDeliver).Methods("POST")
	return r
}

// ----- GET /agents?capability=abc (spec §7, CR-FEAT-007) -----

func TestListAgents_CapabilityFilter(t *testing.T) {
	store := setupTestStore(t)
	_, pubA := newTestPubKey(t)
	_, pubB := newTestPubKey(t)
	if err := store.Register(&Agent{ID: "agent-a", PublicKey: HexKey(pubA), Capabilities: []string{"solver"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Register(&Agent{ID: "agent-b", PublicKey: HexKey(pubB), Capabilities: []string{"solver", "renderer"}}); err != nil {
		t.Fatal(err)
	}
	router := selfConfigRouter(NewHandler(store))

	// Without the parameter: behavior unchanged — all agents.
	req := httptest.NewRequest("GET", "/agents", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var all agentsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &all); err != nil {
		t.Fatal(err)
	}
	if len(all.Agents) != 2 {
		t.Fatalf("unfiltered list = %d agents, want 2", len(all.Agents))
	}

	// Filter: agents advertising the capability.
	req = httptest.NewRequest("GET", "/agents?capability=solver", nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	var filtered agentsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &filtered); err != nil {
		t.Fatal(err)
	}
	if len(filtered.Agents) != 2 {
		t.Fatalf("capability=solver → %d agents, want 2", len(filtered.Agents))
	}

	req = httptest.NewRequest("GET", "/agents?capability=renderer", nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if err := json.Unmarshal(rec.Body.Bytes(), &filtered); err != nil {
		t.Fatal(err)
	}
	if len(filtered.Agents) != 1 || filtered.Agents[0].ID != "agent-b" {
		t.Fatalf("capability=renderer → %+v, want [agent-b]", filtered.Agents)
	}

	// No match: non-nil empty list (not null).
	req = httptest.NewRequest("GET", "/agents?capability=none", nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for no match, got %d", rec.Code)
	}
	if body := rec.Body.String(); !bytes.Contains([]byte(body), []byte(`"agents":[]`)) {
		t.Fatalf("no-match body = %s, want empty array", body)
	}

	// Empty parameter value behaves like no parameter.
	req = httptest.NewRequest("GET", "/agents?capability=", nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if err := json.Unmarshal(rec.Body.Bytes(), &all); err != nil {
		t.Fatal(err)
	}
	if len(all.Agents) != 2 {
		t.Fatalf("capability= (empty) → %d agents, want 2", len(all.Agents))
	}
}

// ----- PATCH /agents/{id} (spec §7, CR-FEAT-007) -----

func TestUpdateAgent_PatchCapabilities(t *testing.T) {
	store := setupTestStore(t)
	registerTestAgent(t, store) // agent-1 with [relay mesh]
	router := selfConfigRouter(NewHandler(store))

	body := []byte(`{"capabilities":["solver","renderer"]}`)
	req := httptest.NewRequest("PATCH", "/agents/agent-1", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var updated Agent
	if err := json.Unmarshal(rec.Body.Bytes(), &updated); err != nil {
		t.Fatal(err)
	}
	if len(updated.Capabilities) != 2 || updated.Capabilities[0] != "solver" || updated.Capabilities[1] != "renderer" {
		t.Fatalf("updated capabilities = %v, want [solver renderer]", updated.Capabilities)
	}

	// Follow-up capability query reflects the change (board PASS criteria).
	req = httptest.NewRequest("GET", "/agents?capability=solver", nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	var filtered agentsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &filtered); err != nil {
		t.Fatal(err)
	}
	if len(filtered.Agents) != 1 || filtered.Agents[0].ID != "agent-1" {
		t.Fatalf("capability=solver → %+v, want [agent-1]", filtered.Agents)
	}

	// The replaced list no longer advertises the old capability.
	req = httptest.NewRequest("GET", "/agents?capability=relay", nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if err := json.Unmarshal(rec.Body.Bytes(), &filtered); err != nil {
		t.Fatal(err)
	}
	if len(filtered.Agents) != 0 {
		t.Fatalf("capability=relay after replace → %+v, want none", filtered.Agents)
	}

	// PATCH with only capabilities leaves the webhook alone? No — spec §7:
	// webhook absent OR null removes it. Capabilities-only PATCH removes the
	// webhook. Verify the webhook is gone when one was set.
	store2 := setupTestStore(t)
	_, pub2 := newTestPubKey(t)
	if err := store2.Register(&Agent{ID: "agent-1", PublicKey: HexKey(pub2),
		Webhook: &webhook.Config{URL: "http://example.com/hook", DeliveryMode: "blocking"}}); err != nil {
		t.Fatal(err)
	}
	router2 := selfConfigRouter(NewHandler(store2))
	req = httptest.NewRequest("PATCH", "/agents/agent-1", bytes.NewReader([]byte(`{"capabilities":["x"]}`)))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	router2.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("capabilities-only PATCH: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var after Agent
	if err := json.Unmarshal(rec.Body.Bytes(), &after); err != nil {
		t.Fatal(err)
	}
	if after.Webhook != nil {
		t.Fatalf("webhook = %+v, want nil (absent webhook removes it)", after.Webhook)
	}
}

func TestUpdateAgent_WebhookAddUpdateRemove(t *testing.T) {
	store := setupTestStore(t)
	registerTestAgent(t, store)
	router := selfConfigRouter(NewHandler(store))

	// Register: PATCH with a webhook object adds it.
	req := httptest.NewRequest("PATCH", "/agents/agent-1",
		bytes.NewReader([]byte(`{"webhook":{"url":"http://hooks.example/agent-1","delivery_mode":"blocking"}}`)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("add webhook: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var added Agent
	if err := json.Unmarshal(rec.Body.Bytes(), &added); err != nil {
		t.Fatal(err)
	}
	if added.Webhook == nil || added.Webhook.URL != "http://hooks.example/agent-1" {
		t.Fatalf("webhook = %+v, want url http://hooks.example/agent-1", added.Webhook)
	}

	// Update: PATCH with a different webhook object replaces it.
	req = httptest.NewRequest("PATCH", "/agents/agent-1",
		bytes.NewReader([]byte(`{"webhook":{"url":"http://hooks.example/agent-1/v2","retries":3}}`)))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("update webhook: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var updated Agent
	if err := json.Unmarshal(rec.Body.Bytes(), &updated); err != nil {
		t.Fatal(err)
	}
	if updated.Webhook == nil || updated.Webhook.URL != "http://hooks.example/agent-1/v2" || updated.Webhook.Retries != 3 {
		t.Fatalf("webhook = %+v, want updated url/retries", updated.Webhook)
	}

	// Remove via explicit null.
	req = httptest.NewRequest("PATCH", "/agents/agent-1", bytes.NewReader([]byte(`{"webhook":null}`)))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("remove webhook (null): expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var removed Agent
	if err := json.Unmarshal(rec.Body.Bytes(), &removed); err != nil {
		t.Fatal(err)
	}
	if removed.Webhook != nil {
		t.Fatalf("webhook = %+v, want nil after null", removed.Webhook)
	}
	req = httptest.NewRequest("GET", "/agents/agent-1", nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	var fetched Agent
	if err := json.Unmarshal(rec.Body.Bytes(), &fetched); err != nil {
		t.Fatal(err)
	}
	if fetched.Webhook != nil {
		t.Fatalf("GET after null: webhook = %+v, want nil", fetched.Webhook)
	}

	// Remove via absent object (spec §7: absent or null).
	req = httptest.NewRequest("PATCH", "/agents/agent-1", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("empty PATCH: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Invalid webhook config → 400 (registration-time validation applies).
	req = httptest.NewRequest("PATCH", "/agents/agent-1", bytes.NewReader([]byte(`{"webhook":{"delivery_mode":"blocking"}}`)))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid webhook: expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestUpdateAgent_Errors(t *testing.T) {
	store := setupTestStore(t)
	registerTestAgent(t, store)
	router := selfConfigRouter(NewHandler(store))

	// Unknown agent → 404.
	req := httptest.NewRequest("PATCH", "/agents/nope", bytes.NewReader([]byte(`{"capabilities":["x"]}`)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown agent: expected 404, got %d: %s", rec.Code, rec.Body.String())
	}

	// Malformed body → 400.
	req = httptest.NewRequest("PATCH", "/agents/agent-1", bytes.NewReader([]byte(`{not json`)))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad body: expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestUpdateAgent_AdvancesLastSeen is the DF-CRIER-156 regression at the
// endpoint: the PATCH 200 body must report the last_seen that is NOW
// persisted (a following GET returns the same instant) and it must be
// strictly after the registration-time value. Pre-fix the memory backend
// echoed the registration value forever, so the response could not be used
// to confirm the write.
func TestUpdateAgent_AdvancesLastSeen(t *testing.T) {
	store := setupTestStore(t)
	registered := registerTestAgent(t, store) // agent-1
	router := selfConfigRouter(NewHandler(store))

	registeredAt := registered.RegisteredAt
	registeredStatus := registered.Status
	registeredSeen := registered.LastSeen

	// A fresh timestamp must be distinguishable from the registration one.
	time.Sleep(2 * time.Millisecond)

	req := httptest.NewRequest("PATCH", "/agents/agent-1",
		bytes.NewReader([]byte(`{"capabilities":["t303-probe"]}`)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var patched Agent
	if err := json.Unmarshal(rec.Body.Bytes(), &patched); err != nil {
		t.Fatalf("decode PATCH body: %v", err)
	}

	req = httptest.NewRequest("GET", "/agents/agent-1", nil)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var fetched Agent
	if err := json.Unmarshal(rec.Body.Bytes(), &fetched); err != nil {
		t.Fatalf("decode GET body: %v", err)
	}

	if !patched.LastSeen.After(registeredSeen) {
		t.Fatalf("PATCH body last_seen = %v, want strictly after the registration value %v",
			patched.LastSeen, registeredSeen)
	}
	if !patched.LastSeen.Equal(fetched.LastSeen) {
		t.Fatalf("PATCH body last_seen = %v, GET body last_seen = %v — the response must equal the persisted value",
			patched.LastSeen, fetched.LastSeen)
	}
	// The rest of the endpoint's contract is unchanged (AC3).
	if !patched.RegisteredAt.Equal(registeredAt) {
		t.Fatalf("PATCH body registered_at = %v, want the registration-time %v",
			patched.RegisteredAt, registeredAt)
	}
	if patched.Status != registeredStatus {
		t.Fatalf("PATCH body status = %q, want %q", patched.Status, registeredStatus)
	}
	if len(patched.Capabilities) != 1 || patched.Capabilities[0] != "t303-probe" {
		t.Fatalf("PATCH body capabilities = %v, want [t303-probe]", patched.Capabilities)
	}
	if !fetched.RegisteredAt.Equal(registeredAt) {
		t.Fatalf("GET body registered_at = %v, want the registration-time %v",
			fetched.RegisteredAt, registeredAt)
	}
}

// nonUpdatableStore wraps a Store without implementing the optional updater
// capability — the handler must answer 501, not panic.
type nonUpdatableStore struct {
	Store
}

func TestUpdateAgent_StoreWithoutUpdater(t *testing.T) {
	inner := setupTestStore(t)
	registerTestAgent(t, inner)
	router := selfConfigRouter(NewHandler(&nonUpdatableStore{inner}))

	req := httptest.NewRequest("PATCH", "/agents/agent-1", bytes.NewReader([]byte(`{"capabilities":["x"]}`)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("expected 501 for non-updatable store, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ----- PATCH signature gate (same requireAgent gate as DELETE) -----

func TestUpdateAgent_RequiresSignature(t *testing.T) {
	store := NewMemoryStore()
	h := newSigHandler(store)
	_, priv := testAgentKeypair(t, store, "agent-1")
	router := selfConfigRouter(h)

	// Unsigned PATCH → 401.
	req := httptest.NewRequest("PATCH", "/agents/agent-1", bytes.NewReader([]byte(`{"capabilities":["solver"]}`)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unsigned PATCH: expected 401, got %d: %s", rec.Code, rec.Body.String())
	}

	// Signed PATCH → 200 (sign "PATCH\n/agents/agent-1\n<ts>").
	req = signedRequest(t, priv, "agent-1", http.MethodPatch, "/agents/agent-1")
	req.Body = io.NopCloser(bytes.NewReader([]byte(`{"capabilities":["solver"]}`)))
	req.ContentLength = int64(len(`{"capabilities":["solver"]}`))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("signed PATCH: expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Unknown agent, properly signed → 404 (no existence leak).
	req = signedRequest(t, priv, "agent-ghost", http.MethodPatch, "/agents/agent-ghost")
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("signed PATCH to unknown agent: expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

// ----- configure kind delivery (spec §7) -----

// TestDeliver_WebhookConfigure_PassScenario is the board PASS criteria:
// the controller delivers a configure directive to agent-b, agent-b's
// webhook receives it (X-Crier-Event: configure, payload intact), agent-b
// acts on it by PATCHing its own registration, and a follow-up capability
// query reflects the change.
func TestDeliver_WebhookConfigure_PassScenario(t *testing.T) {
	var mu sync.Mutex
	var gotEvent, gotKind string
	var gotPayload map[string]any
	patched := make(chan struct{}, 1)

	// agent-b's keypair — it signs its own PATCH.
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	// The crier server URL is captured by the endpoint handler; deliveries
	// only start after it is assigned.
	var crierURL string

	// agent-b's webhook endpoint: verifies the directive, acts on it
	// (PATCH), then replies (blocking response = raw body).
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotEvent = r.Header.Get("X-Crier-Event")
		body, _ := io.ReadAll(r.Body)
		var env webhook.Envelope
		if err := json.Unmarshal(body, &env); err != nil {
			mu.Unlock()
			t.Errorf("endpoint: unmarshal envelope: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		gotKind = env.Crier.Kind
		_ = json.Unmarshal(env.Payload, &gotPayload)
		mu.Unlock()

		// The agent acts on the directive: PATCH its registration to add
		// the target capability (signed with its own key).
		ts := fmt.Sprintf("%d", time.Now().Unix())
		sig := ed25519.Sign(priv, []byte("PATCH\n/agents/agent-b\n"+ts))
		preq, err := http.NewRequest("PATCH", crierURL+"/agents/agent-b",
			bytes.NewReader([]byte(`{"capabilities":["solver","solver-v2"]}`)))
		if err != nil {
			t.Errorf("endpoint: build PATCH: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		preq.Header.Set("Content-Type", "application/json")
		preq.Header.Set(HeaderAgentID, "agent-b")
		preq.Header.Set(HeaderAgentTS, ts)
		preq.Header.Set(HeaderAgentSig, hex.EncodeToString(sig))
		presp, err := http.DefaultClient.Do(preq)
		if err != nil {
			t.Errorf("endpoint: PATCH request: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		presp.Body.Close()
		if presp.StatusCode != http.StatusOK {
			t.Errorf("endpoint: PATCH status = %d, want 200", presp.StatusCode)
		} else {
			select {
			case patched <- struct{}{}:
			default:
			}
		}

		// Reply body — blocking delivery returns it raw (generic-custom).
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ack":"configured"}`))
	}))
	defer endpoint.Close()

	store := setupTestStore(t)
	if err := store.Register(&Agent{
		ID:           "agent-b",
		PublicKey:    HexKey(pub),
		Capabilities: []string{"solver"},
		Webhook: &webhook.Config{
			URL:            endpoint.URL,
			DeliveryMode:   "blocking",
			Retries:        0,
			TimeoutMs:      5000,
			SchemaTemplate: "generic-custom",
		},
	}); err != nil {
		t.Fatal(err)
	}

	handler := NewHandler(store)
	handler.SetRequireAgentSig(true)
	driver := webhook.NewDriver(webhook.NewClient(2*time.Second, nil), webhook.NewMemoryQueue(), webhook.DriverConfig{
		MaxRetries:     5,
		RedeliverEvery: 200 * time.Millisecond,
		ProbeEvery:     200 * time.Millisecond,
	})
	driver.Start()
	defer driver.Stop()
	handler.SetWebhookDriver(driver)

	router := selfConfigRouter(handler)
	ts := httptest.NewServer(router)
	defer ts.Close()
	crierURL = ts.URL

	// The controller delivers a configure directive (blocking).
	payload := json.RawMessage(`{"directive":"enable solver-v2","target_capability":"solver-v2","schema":{"response_map":"raw"}}`)
	dbody, _ := json.Marshal(deliverRequest{
		Payload:      payload,
		Sender:       "agent-a",
		DeliveryMode: "blocking",
		Kind:         webhook.KindConfigure,
	})
	resp, err := http.Post(ts.URL+"/agents/agent-b/inbox", "application/json", bytes.NewReader(dbody))
	if err != nil {
		t.Fatalf("deliver configure: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		rb, _ := io.ReadAll(resp.Body)
		t.Fatalf("deliver configure: expected 200, got %d: %s", resp.StatusCode, rb)
	}
	var delivered blockingDeliverResponse
	if err := json.NewDecoder(resp.Body).Decode(&delivered); err != nil {
		t.Fatal(err)
	}
	if string(delivered.Reply) != `{"ack":"configured"}` {
		t.Fatalf("blocking reply = %s, want {\"ack\":\"configured\"}", delivered.Reply)
	}

	// agent-b acted on the directive (PATCH landed).
	select {
	case <-patched:
	case <-time.After(3 * time.Second):
		t.Fatal("agent-b never PATCHed its registration")
	}

	// Endpoint saw the configure kind on the wire, payload intact.
	mu.Lock()
	defer mu.Unlock()
	if gotEvent != "configure" {
		t.Errorf("X-Crier-Event = %q, want configure", gotEvent)
	}
	if gotKind != "configure" {
		t.Errorf("envelope crier.kind = %q, want configure", gotKind)
	}
	if gotPayload["directive"] != "enable solver-v2" || gotPayload["target_capability"] != "solver-v2" {
		t.Errorf("payload = %v, want directive/target_capability intact", gotPayload)
	}
	if _, ok := gotPayload["schema"]; !ok {
		t.Errorf("payload = %v, want optional schema object preserved", gotPayload)
	}

	// Follow-up capability query reflects the change (board PASS).
	req, _ := http.NewRequest("GET", ts.URL+"/agents?capability=solver-v2", nil)
	gresp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer gresp.Body.Close()
	var filtered agentsResponse
	if err := json.NewDecoder(gresp.Body).Decode(&filtered); err != nil {
		t.Fatal(err)
	}
	if len(filtered.Agents) != 1 || filtered.Agents[0].ID != "agent-b" {
		t.Fatalf("capability=solver-v2 → %+v, want [agent-b]", filtered.Agents)
	}
	if len(filtered.Agents[0].Capabilities) != 2 || filtered.Agents[0].Capabilities[1] != "solver-v2" {
		t.Fatalf("agent-b capabilities = %v, want [solver solver-v2]", filtered.Agents[0].Capabilities)
	}
}

// TestDeliver_WebhookKind_DefaultsAndAck covers the kind pass-through on the
// webhook wire: missing kind defaults to message; configure_ack (the agent's
// reply to a directive, spec §7) travels untouched.
func TestDeliver_WebhookKind_DefaultsAndAck(t *testing.T) {
	for _, tt := range []struct {
		name    string
		kind    string
		want    string
		payload string
	}{
		{"defaults to message", "", "message", `{"hello":"world"}`},
		{"configure ack passes through", webhook.KindConfigureAck, "configure_ack", `{"ack":true}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var mu sync.Mutex
			var gotEvent, gotKind string
			endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				gotEvent = r.Header.Get("X-Crier-Event")
				body, _ := io.ReadAll(r.Body)
				var env webhook.Envelope
				if err := json.Unmarshal(body, &env); err != nil {
					mu.Unlock()
					t.Errorf("endpoint: unmarshal: %v", err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				gotKind = env.Crier.Kind
				mu.Unlock()
				w.WriteHeader(http.StatusOK)
				w.Write([]byte(`{"ok":true}`))
			}))
			defer endpoint.Close()

			store := setupTestStore(t)
			_, pubKey := newTestPubKey(t)
			if err := store.Register(&Agent{
				ID:        "agent-w",
				PublicKey: HexKey(pubKey),
				Webhook: &webhook.Config{
					URL:          endpoint.URL,
					DeliveryMode: "blocking",
					Retries:      0,
					TimeoutMs:    5000,
				},
			}); err != nil {
				t.Fatal(err)
			}

			handler := NewHandler(store)
			driver := webhook.NewDriver(webhook.NewClient(2*time.Second, nil), webhook.NewMemoryQueue(), webhook.DriverConfig{
				MaxRetries:     5,
				RedeliverEvery: 200 * time.Millisecond,
				ProbeEvery:     200 * time.Millisecond,
			})
			handler.SetWebhookDriver(driver)
			router := selfConfigRouter(handler)

			dbody, _ := json.Marshal(deliverRequest{
				Payload:      json.RawMessage(tt.payload),
				Sender:       "agent-a",
				DeliveryMode: "blocking",
				Kind:         tt.kind,
			})
			req := httptest.NewRequest("POST", "/agents/agent-w/inbox", bytes.NewReader(dbody))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("deliver: expected 200, got %d: %s", rec.Code, rec.Body.String())
			}

			mu.Lock()
			defer mu.Unlock()
			if gotEvent != tt.want {
				t.Errorf("X-Crier-Event = %q, want %q", gotEvent, tt.want)
			}
			if gotKind != tt.want {
				t.Errorf("envelope crier.kind = %q, want %q", gotKind, tt.want)
			}
		})
	}
}
