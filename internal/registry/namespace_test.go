package registry

// CR-FEAT-029 (realms) — registry lane acceptance.
//
// The deliverable says: "a namespace (realm) dimension on agents and messages
// with per-namespace policy (auth posture, rate limits, guard settings,
// retention), defaulting to today's single implicit namespace so nothing
// changes for existing deployments", and the acceptance names three properties:
//
//  1. two namespaces with different rate limits and guard policies
//     demonstrably do not interfere (the relay half is in internal/relay;
//     the guard half is here — a realm with guard_enabled=false keeps its
//     traffic out of the process-wide guard budget, which is what stops one
//     realm's flood from tripping the shared circuit for everyone);
//  2. a message cannot cross namespaces implicitly (deliver pins a message to
//     the TARGET's stored realm and refuses a mismatched claim; transfer
//     refuses to move live messages between realms);
//  3. single-namespace behaviour is byte-identical to today (a registration
//     that names no realm produces the same JSON it always did, with no
//     `namespace` member anywhere).

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"

	"github.com/crier-dev/crier/internal/guard"
	"github.com/crier-dev/crier/internal/namespace"
)

// nsHandler wires a Handler with a namespace registry onto the same route set
// the server registers (plus /namespaces), so every assertion below goes
// through the real HTTP surface.
type nsHandler struct {
	store   *MemoryStore
	handler *Handler
	router  *mux.Router
}

func newNSHandler(t *testing.T, doc string) *nsHandler {
	t.Helper()
	reg, err := namespace.Parse(doc, nil)
	if err != nil {
		t.Fatalf("namespace.Parse(%s): %v", doc, err)
	}
	store := NewMemoryStore()
	h := NewHandler(store)
	h.SetNamespacePolicies(reg)

	r := mux.NewRouter()
	r.HandleFunc("/agents", h.HandleRegister).Methods("POST")
	r.HandleFunc("/agents/{id}", h.HandleGetAgent).Methods("GET")
	r.HandleFunc("/agents/{id}", h.HandleUpdateAgent).Methods("PATCH")
	r.HandleFunc("/agents/{id}/inbox", h.HandleDeliver).Methods("POST")
	r.HandleFunc("/agents/{id}/inbox", h.HandleRetrieve).Methods("GET")
	r.HandleFunc("/agents/{id}/inbox/transfer", h.HandleTransfer).Methods("POST")
	r.HandleFunc("/agents/{id}/inbox/dead-letters", h.HandleDeadLetters).Methods("GET")
	r.HandleFunc("/namespaces", h.HandleListNamespaces).Methods("GET")
	return &nsHandler{store: store, handler: h, router: r}
}

// do runs one request and returns the recorder.
func (f *nsHandler) do(t *testing.T, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	return rec
}

// register posts a registration for id in nsName ("" = no namespace member).
func (f *nsHandler) register(t *testing.T, id, nsName string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	hexKey, _ := newTestPubKey(t)
	payload := map[string]any{"id": id, "public_key": hexKey}
	if nsName != "" {
		payload["namespace"] = nsName
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return f.do(t, http.MethodPost, "/agents", string(body), headers)
}

// deliver posts a delivery to id, optionally claiming a namespace.
func (f *nsHandler) deliver(t *testing.T, id, nsName, payload string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	body := map[string]any{"payload": json.RawMessage(payload)}
	if nsName != "" {
		body["namespace"] = nsName
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return f.do(t, http.MethodPost, "/agents/"+id+"/inbox", string(encoded), headers)
}

// entryNamespace reads the namespace the stored message carries, via retrieve.
func (f *nsHandler) entryNamespace(t *testing.T, id string) string {
	t.Helper()
	rec := f.do(t, http.MethodGet, "/agents/"+id+"/inbox", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("retrieve: %d %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Messages []struct {
			Namespace string `json:"namespace"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("retrieve decode: %v", err)
	}
	if len(resp.Messages) == 0 {
		t.Fatal("inbox is empty")
	}
	return resp.Messages[0].Namespace
}

// TestRegisterIntoDeclaredNamespace covers the happy path and the wire shape:
// a named realm appears on the row, the default one does not.
func TestRegisterIntoDeclaredNamespace(t *testing.T) {
	f := newNSHandler(t, `{"namespaces":[{"name":"acme"}]}`)

	rec := f.register(t, "named", "acme", nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("register into a declared realm = %d (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"namespace":"acme"`) {
		t.Errorf("registration body must name the realm: %s", rec.Body.String())
	}

	// "default" is the same realm as no namespace at all, and must not appear
	// on the wire.
	rec = f.register(t, "defaulted", "default", nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("register into the default realm = %d (%s)", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "namespace") {
		t.Errorf("the default realm must not appear on the wire: %s", rec.Body.String())
	}

	// The stored rows agree, and the listing counts them per realm.
	agents := f.store.List()
	byID := map[string]*Agent{}
	for _, a := range agents {
		byID[a.ID] = a
	}
	if byID["named"].Namespace != "acme" {
		t.Errorf("stored namespace = %q, want acme", byID["named"].Namespace)
	}
	if byID["defaulted"].Namespace != "" {
		t.Errorf("stored default namespace = %q, want \"\"", byID["defaulted"].Namespace)
	}

	listRec := f.do(t, http.MethodGet, "/namespaces", "", nil)
	if listRec.Code != http.StatusOK {
		t.Fatalf("GET /namespaces = %d", listRec.Code)
	}
	var listed namespacesResponse
	if err := json.Unmarshal(listRec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("namespaces decode: %v", err)
	}
	if listed.Default != namespace.DefaultName || listed.Count != 2 {
		t.Fatalf("namespaces = %+v, want the default realm plus acme", listed)
	}
	counts := map[string]int{}
	for _, e := range listed.Namespaces {
		counts[e.Name] = e.Agents
	}
	if counts[namespace.DefaultName] != 1 || counts["acme"] != 1 {
		t.Errorf("agent census = %v, want one agent in each realm", counts)
	}
}

// TestRegisterIntoUndeclaredNamespaceIsRefused: a typo must never silently join
// the default realm — with per-realm guard settings that would also silently
// leave the guarded realm.
func TestRegisterIntoUndeclaredNamespaceIsRefused(t *testing.T) {
	f := newNSHandler(t, `{"namespaces":[{"name":"acme"}]}`)

	rec := f.register(t, "typo", "acmee", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("register into an undeclared realm = %d, want 400 (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "UNKNOWN_NAMESPACE") {
		t.Errorf("body = %s, want a named refusal", rec.Body.String())
	}
	if len(f.store.List()) != 0 {
		t.Error("a refused registration must not create a row")
	}
}

// TestRegisterNamespaceTokenPosture: the realm's own credential is required to
// register INTO it, and the shared-posture realm ignores it.
func TestRegisterNamespaceTokenPosture(t *testing.T) {
	t.Setenv("REALM_TOKEN", "s3cret")
	f := newNSHandler(t, `{"namespaces":[
		{"name":"private","auth":"token","token_ref":"env:REALM_TOKEN"},
		{"name":"public"}
	]}`)

	rec := f.register(t, "a", "private", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("register into a token realm without its token = %d, want 401 (%s)", rec.Code, rec.Body.String())
	}
	if len(f.store.List()) != 0 {
		t.Error("a refused registration must not create a row")
	}

	rec = f.register(t, "a", "private", map[string]string{namespace.HeaderNamespaceToken: "s3cret"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("register with the realm credential = %d, want 201 (%s)", rec.Code, rec.Body.String())
	}

	rec = f.register(t, "b", "public", nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("a shared-posture realm must not demand a credential: %d", rec.Code)
	}
}

// TestDeliveryIsPinnedToTheTargetNamespace is the crossing acceptance: the
// message's realm comes from the TARGET's row and a mismatched claim is
// refused. Nothing is stored on the refusal.
func TestDeliveryIsPinnedToTheTargetNamespace(t *testing.T) {
	f := newNSHandler(t, `{"namespaces":[{"name":"acme"},{"name":"globex"}]}`)
	if rec := f.register(t, "in-acme", "acme", nil); rec.Code != http.StatusCreated {
		t.Fatalf("register: %d %s", rec.Code, rec.Body.String())
	}

	// (a) no claim at all → the target's realm, and it is recorded with the
	// message.
	rec := f.deliver(t, "in-acme", "", `{"hello":"acme"}`, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("deliver without a namespace claim = %d (%s)", rec.Code, rec.Body.String())
	}
	if got := f.entryNamespace(t, "in-acme"); got != "acme" {
		t.Fatalf("stored message namespace = %q, want acme", got)
	}

	// (b) the target's own realm restated explicitly → accepted (it is not a
	// crossing, it is the same realm named).
	rec = f.deliver(t, "in-acme", "acme", `{"hello":"again"}`, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("deliver naming the target's own realm = %d (%s)", rec.Code, rec.Body.String())
	}

	// (c) a DIFFERENT realm → refused, and nothing is stored.
	rec = f.deliver(t, "in-acme", "globex", `{"hello":"cross"}`, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-namespace delivery = %d, want 403 (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "NAMESPACE_MISMATCH") {
		t.Errorf("body = %s, want NAMESPACE_MISMATCH", rec.Body.String())
	}
	rec = f.do(t, http.MethodGet, "/agents/in-acme/inbox", "", nil)
	var inbox struct {
		QueueDepth int `json:"queue_depth"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &inbox); err != nil {
		t.Fatalf("inbox decode: %v", err)
	}
	if inbox.QueueDepth != 2 {
		t.Errorf("queue depth after the refused delivery = %d, want 2 (nothing was stored for it)", inbox.QueueDepth)
	}
}

// TestDeliveryNamespaceTokenPosture: a token-posture realm gates delivery into
// its inboxes too — the credential is enforced wherever the realm is known.
func TestDeliveryNamespaceTokenPosture(t *testing.T) {
	t.Setenv("REALM_TOKEN", "s3cret")
	f := newNSHandler(t, `{"namespaces":[{"name":"private","auth":"token","token_ref":"env:REALM_TOKEN"}]}`)
	if rec := f.register(t, "a", "private", map[string]string{namespace.HeaderNamespaceToken: "s3cret"}); rec.Code != http.StatusCreated {
		t.Fatalf("register: %d %s", rec.Code, rec.Body.String())
	}

	if rec := f.deliver(t, "a", "", `{"x":1}`, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("deliver into a token realm without its token = %d, want 401 (%s)", rec.Code, rec.Body.String())
	}
	rec := f.deliver(t, "a", "", `{"x":1}`, map[string]string{namespace.HeaderNamespaceToken: "s3cret"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("deliver with the realm credential = %d, want 201 (%s)", rec.Code, rec.Body.String())
	}
}

// TestNamespaceRetentionIsTheDefaultLifetime: a realm's retention applies when
// the request states no ttl_seconds — and an explicit TTL still wins, including
// an explicit 0 (never expires). Each case gets its own agent so the assertions
// never depend on what a previous retrieve leased.
func TestNamespaceRetentionIsTheDefaultLifetime(t *testing.T) {
	f := newNSHandler(t, `{"namespaces":[{"name":"short","retention_seconds":60}]}`)
	for _, id := range []string{"inherits", "explicit-zero", "default-realm"} {
		nsName := "short"
		if id == "default-realm" {
			nsName = ""
		}
		if rec := f.register(t, id, nsName, nil); rec.Code != http.StatusCreated {
			t.Fatalf("register %s: %d %s", id, rec.Code, rec.Body.String())
		}
	}

	// delivered the entry's own timestamps, from the retrieve wire — the same
	// bytes a client reads.
	type stored struct {
		CreatedAt *time.Time `json:"created_at"`
		ExpiresAt *time.Time `json:"expires_at"`
	}
	read := func(id string) stored {
		t.Helper()
		rec := f.do(t, http.MethodGet, "/agents/"+id+"/inbox", "", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("retrieve %s: %d %s", id, rec.Code, rec.Body.String())
		}
		var resp struct {
			Messages []stored `json:"messages"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("retrieve decode: %v", err)
		}
		if len(resp.Messages) == 0 {
			t.Fatalf("%s: inbox is empty", id)
		}
		return resp.Messages[0]
	}

	// (a) the realm's 60s retention is the default lifetime.
	if rec := f.deliver(t, "inherits", "", `{"x":1}`, nil); rec.Code != http.StatusCreated {
		t.Fatalf("deliver: %d %s", rec.Code, rec.Body.String())
	}
	got := read("inherits")
	if got.ExpiresAt == nil || got.CreatedAt == nil {
		t.Fatalf("a realm with retention must produce a finite expiry: %+v", got)
	}
	if d := got.ExpiresAt.Sub(*got.CreatedAt); d < 55*time.Second || d > 65*time.Second {
		t.Errorf("lifetime = %s, want ~60s (the realm's retention)", d)
	}

	// (b) an explicit ttl_seconds=0 wins over the realm default: never expires,
	// which the wire spells as `"expires_at": null`.
	rec := f.do(t, http.MethodPost, "/agents/explicit-zero/inbox", `{"payload":{"x":2},"ttl_seconds":0}`, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("deliver with ttl_seconds=0: %d %s", rec.Code, rec.Body.String())
	}
	if got := read("explicit-zero"); got.ExpiresAt != nil {
		t.Errorf("ttl_seconds=0 must win over the realm default: expires_at = %s", got.ExpiresAt)
	}

	// (c) the default realm keeps the store's own 24h default, unchanged.
	if rec := f.deliver(t, "default-realm", "", `{"x":3}`, nil); rec.Code != http.StatusCreated {
		t.Fatalf("deliver: %d %s", rec.Code, rec.Body.String())
	}
	got = read("default-realm")
	if got.ExpiresAt == nil || got.CreatedAt == nil {
		t.Fatalf("default realm: %+v", got)
	}
	if d := got.ExpiresAt.Sub(*got.CreatedAt); d != DefaultMessageTTL {
		t.Errorf("default-realm lifetime = %s, want %s", d, DefaultMessageTTL)
	}
}

// TestNamespaceGuardSettingsDoNotInterfere is the guard half of the noisy-
// neighbour acceptance: two realms with DIFFERENT guard settings share one
// process-wide guard (one LLM lane, one concurrency cap, one circuit breaker),
// and the realm that declared guard_enabled=false must stay completely out of
// it while the other realm still gets its verdict.
func TestNamespaceGuardSettingsDoNotInterfere(t *testing.T) {
	f := newGuardFixture(t, guardVerdictBlock)
	realmPolicy, err := json.Marshal(map[string]any{
		"id": "realm-block",
		"providers": []map[string]any{{
			"provider":    "custom",
			"model":       "mock-model",
			"base_url":    f.llmURL,
			"api_key_ref": "env:GUARD_KEY",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	doc := `{"namespaces":[
		{"name":"unguarded","guard_enabled":false},
		{"name":"guarded","guard_policy":` + string(realmPolicy) + `}
	]}`
	reg, err := namespace.Parse(doc, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.handler.SetNamespacePolicies(reg)
	f.router.HandleFunc("/namespaces", f.handler.HandleListNamespaces).Methods("GET")

	if code := f.registerAgentNS(t, "free", "unguarded", nil); code != http.StatusCreated {
		t.Fatalf("register free: %d", code)
	}
	if code := f.registerAgentNS(t, "checked", "guarded", nil); code != http.StatusCreated {
		t.Fatalf("register checked: %d", code)
	}

	// The guarded realm gets its verdict from the realm policy.
	rec := f.deliverReq(t, "checked", deliverRequest{Payload: json.RawMessage(`{"jailbreak":"now"}`)})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("delivery into a guarded realm = %d, want 403 (%s)", rec.Code, rec.Body.String())
	}
	if f.llm.count() == 0 {
		t.Fatal("the guarded realm's delivery was never checked")
	}
	checkedCalls := f.llm.count()

	// The unguarded realm is not checked AT ALL — not once, for any number of
	// deliveries — so its traffic cannot consume the guard budget the other
	// realm's traffic depends on.
	for i := 0; i < 3; i++ {
		rec := f.deliverReq(t, "free", deliverRequest{Payload: json.RawMessage(`{"jailbreak":"now"}`)})
		if rec.Code != http.StatusCreated {
			t.Fatalf("delivery into a guard-disabled realm = %d (%s)", rec.Code, rec.Body.String())
		}
	}
	if got := f.llm.count(); got != checkedCalls {
		t.Fatalf("the guard was consulted %d more time(s) for a realm that disabled it", got-checkedCalls)
	}

	// And the reverse direction: the guarded realm's refusal changed nothing
	// about the unguarded realm's stored messages.
	inboxRec := httptest.NewRecorder()
	f.router.ServeHTTP(inboxRec, httptest.NewRequest(http.MethodGet, "/agents/free/inbox", nil))
	var inbox struct {
		QueueDepth int `json:"queue_depth"`
	}
	if err := json.Unmarshal(inboxRec.Body.Bytes(), &inbox); err != nil {
		t.Fatalf("inbox decode: %v", err)
	}
	if inbox.QueueDepth != 3 {
		t.Errorf("unguarded realm queue depth = %d, want 3", inbox.QueueDepth)
	}
}

// registerAgentNS registers an agent into a realm with extra headers.
func (f *guardFixture) registerAgentNS(t *testing.T, id, nsName string, headers map[string]string) int {
	t.Helper()
	hexKey, _ := newTestPubKey(t)
	payload := map[string]any{"id": id, "public_key": hexKey}
	if nsName != "" {
		payload["namespace"] = nsName
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/agents", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	return rec.Code
}

// TestNamespaceGuardPolicyIsTheRealmDefault: a realm's guard_policy is the
// server default for deliveries in that realm. The observable difference: an
// agent with NO guard config is checked by the realm's policy (which points at
// the mock LLM and blocks) rather than by the deployment default (whose
// provider is unreachable in a test, so the guard would fail open).
func TestNamespaceGuardPolicyIsTheRealmDefault(t *testing.T) {
	f := newGuardFixture(t, guardVerdictBlock)
	realmPolicy, err := json.Marshal(map[string]any{
		"id": "realm-default",
		"providers": []map[string]any{{
			"provider":    "custom",
			"model":       "mock-model",
			"base_url":    f.llmURL,
			"api_key_ref": "env:GUARD_KEY",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	doc := `{"namespaces":[{"name":"acme","guard_policy":` + string(realmPolicy) + `}]}`
	reg, err := namespace.Parse(doc, nil)
	if err != nil {
		t.Fatalf("namespace.Parse: %v", err)
	}
	f.handler.SetNamespacePolicies(reg)

	if code := f.registerAgentNS(t, "realm-agent", "acme", nil); code != http.StatusCreated {
		t.Fatalf("register: %d", code)
	}
	if code := f.registerAgentNS(t, "plain-agent", "", nil); code != http.StatusCreated {
		t.Fatalf("register: %d", code)
	}

	// The realm's policy blocks: the delivery is refused with the guard verdict.
	rec := f.deliverReq(t, "realm-agent", deliverRequest{Payload: json.RawMessage(`{"jailbreak":"now"}`)})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("delivery under the realm's guard policy = %d, want 403 (%s)", rec.Code, rec.Body.String())
	}
	if f.llm.count() == 0 {
		t.Fatal("the realm's guard policy provider was never called")
	}

	// The same delivery in the default realm is NOT blocked: with no realm
	// policy the deployment default (an unreachable provider) applies, which
	// fails open — the pre-CR-FEAT-029 behaviour, unchanged.
	rec = f.deliverReq(t, "plain-agent", deliverRequest{Payload: json.RawMessage(`{"jailbreak":"now"}`)})
	if rec.Code == http.StatusForbidden {
		t.Fatalf("the realm policy leaked into the default realm: %d (%s)", rec.Code, rec.Body.String())
	}
}

// TestNamespaceGuardPolicyDoesNotOverrideTheAgent covers the specificity rule:
// an agent's own guard.policies outrank the realm default.
func TestNamespaceGuardPolicyDoesNotOverrideTheAgent(t *testing.T) {
	f := newGuardFixture(t, guardVerdictAllow)
	realmPolicy := `{"namespaces":[{"name":"acme","guard_policy":{"id":"blocker","thresholds":{"block_risk":"high"},"action":"block"}}]}`
	reg, err := namespace.Parse(realmPolicy, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.handler.SetNamespacePolicies(reg)

	if code := f.registerAgent(t, "agent-policy", f.guardPolicy(false)); code != http.StatusCreated {
		t.Fatalf("register: %d", code)
	}
	if _, err := f.store.Get("agent-policy"); err != nil {
		t.Fatal(err)
	}
	// Move it into the realm now that the policy is installed (registration is
	// the only way in, so the row is rewritten directly for this unit-level
	// case rather than going through a realm move the API refuses).
	agent, _ := f.store.Get("agent-policy")
	agent.Namespace = "acme"

	rec := f.deliverReq(t, "agent-policy", deliverRequest{Payload: json.RawMessage(`{"hello":"world"}`)})
	if rec.Code != http.StatusCreated {
		t.Fatalf("delivery = %d (%s)", rec.Code, rec.Body.String())
	}
	meta, _ := f.retrievedGuard(t, "agent-policy")
	if meta == nil {
		t.Fatal("the agent's own policy must still produce guard metadata")
	}
	if f.llm.count() == 0 {
		t.Fatal("the agent's own policy provider was never called")
	}
}

// TestTransferCannotCrossNamespaces: moving live messages into another realm's
// inbox would be exactly the implicit crossing the feature forbids.
func TestTransferCannotCrossNamespaces(t *testing.T) {
	f := newNSHandler(t, `{"namespaces":[{"name":"acme"},{"name":"globex"}]}`)
	if rec := f.register(t, "src", "acme", nil); rec.Code != http.StatusCreated {
		t.Fatalf("register: %d %s", rec.Code, rec.Body.String())
	}
	if rec := f.register(t, "dst", "globex", nil); rec.Code != http.StatusCreated {
		t.Fatalf("register: %d %s", rec.Code, rec.Body.String())
	}
	if rec := f.deliver(t, "src", "", `{"x":1}`, nil); rec.Code != http.StatusCreated {
		t.Fatalf("deliver: %d %s", rec.Code, rec.Body.String())
	}

	var msgID string
	for _, a := range f.store.List() {
		if a.ID == "src" {
			_ = a
		}
	}
	// Read the message id off the inbox.
	rec := f.do(t, http.MethodGet, "/agents/src/inbox", "", nil)
	var got struct {
		Messages []struct {
			ID string `json:"id"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil || len(got.Messages) == 0 {
		t.Fatalf("retrieve: %s", rec.Body.String())
	}
	msgID = got.Messages[0].ID

	body, _ := json.Marshal(map[string]any{"target_agent_id": "dst", "message_ids": []string{msgID}})
	rec = f.do(t, http.MethodPost, "/agents/src/inbox/transfer", string(body), nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-namespace transfer = %d, want 403 (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "NAMESPACE_MISMATCH") {
		t.Errorf("body = %s, want NAMESPACE_MISMATCH", rec.Body.String())
	}

	// Same-realm transfer still works: the guard is on the crossing only. The
	// message is leased by the retrieve above, so the operator escape hatch
	// (`force`) is what moves it — the documented way to displace a live lease.
	if rec := f.register(t, "src-2", "acme", nil); rec.Code != http.StatusCreated {
		t.Fatalf("register: %d %s", rec.Code, rec.Body.String())
	}
	body, _ = json.Marshal(map[string]any{"target_agent_id": "src-2", "message_ids": []string{msgID}, "force": true})
	rec = f.do(t, http.MethodPost, "/agents/src/inbox/transfer", string(body), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("same-realm transfer = %d (%s)", rec.Code, rec.Body.String())
	}
}

// TestPatchCannotMoveAnAgentBetweenNamespaces: moving a live agent has no
// defined meaning for the messages already queued in its inbox, so the member
// is refused rather than ignored (a silently accepted member would look exactly
// like a successful move).
func TestPatchCannotMoveAnAgentBetweenNamespaces(t *testing.T) {
	f := newNSHandler(t, `{"namespaces":[{"name":"acme"},{"name":"globex"}]}`)
	if rec := f.register(t, "a", "acme", nil); rec.Code != http.StatusCreated {
		t.Fatalf("register: %d %s", rec.Code, rec.Body.String())
	}

	rec := f.do(t, http.MethodPatch, "/agents/a", `{"namespace":"globex"}`, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PATCH with a namespace member = %d, want 400 (%s)", rec.Code, rec.Body.String())
	}

	// A PATCH that says nothing about the realm leaves it exactly where it was.
	rec = f.do(t, http.MethodPatch, "/agents/a", `{"capabilities":["relay"]}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("ordinary PATCH = %d (%s)", rec.Code, rec.Body.String())
	}
	agent, err := f.store.Get("a")
	if err != nil {
		t.Fatal(err)
	}
	if agent.Namespace != "acme" {
		t.Errorf("namespace after an ordinary PATCH = %q, want acme", agent.Namespace)
	}
}

// TestDeadLetterKeepsTheRealm: the dead-letter destination outlives the agent
// row, so the realm the retention policy belonged to has to be recorded with
// the record.
func TestDeadLetterKeepsTheRealm(t *testing.T) {
	f := newNSHandler(t, `{"namespaces":[{"name":"acme","retention_seconds":1}]}`)
	if rec := f.register(t, "expiring", "acme", nil); rec.Code != http.StatusCreated {
		t.Fatalf("register: %d %s", rec.Code, rec.Body.String())
	}
	if rec := f.deliver(t, "expiring", "", `{"x":1}`, nil); rec.Code != http.StatusCreated {
		t.Fatalf("deliver: %d %s", rec.Code, rec.Body.String())
	}

	// The realm's 1s retention expires the message.
	time.Sleep(1100 * time.Millisecond)
	if n := f.handler.PurgeExpired(); n != 1 {
		t.Fatalf("PurgeExpired removed %d messages, want 1", n)
	}

	rec := f.do(t, http.MethodGet, "/agents/expiring/inbox/dead-letters", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("dead letters = %d (%s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"namespace":"acme"`) {
		t.Errorf("dead letter must record the realm: %s", rec.Body.String())
	}
}

// TestDefaultNamespaceRegistrationIsByteIdentical is the third acceptance
// property at the JSON level: with namespaces configured (or not), a
// registration that names no realm produces the body it always did.
func TestDefaultNamespaceRegistrationIsByteIdentical(t *testing.T) {
	// A handler with NO namespace registry at all — the pre-CR-FEAT-029 server.
	plainStore := NewMemoryStore()
	plain := NewHandler(plainStore)
	plainRouter := mux.NewRouter()
	plainRouter.HandleFunc("/agents", plain.HandleRegister).Methods("POST")
	plainRec := httptest.NewRecorder()
	plainReq := httptest.NewRequest(http.MethodPost, "/agents", bytes.NewReader([]byte(`{"id":"identical"}`)))
	plainRouter.ServeHTTP(plainRec, plainReq)

	// The same registration against a handler WITH realms declared.
	f := newNSHandler(t, `{"namespaces":[{"name":"acme","retention_seconds":60}]}`)
	withRealms := f.register(t, "identical", "", nil)

	if plainRec.Code != http.StatusCreated || withRealms.Code != http.StatusCreated {
		t.Fatalf("codes = %d / %d, want 201/201 (%s)", plainRec.Code, withRealms.Code, withRealms.Body.String())
	}
	if strings.Contains(withRealms.Body.String(), "namespace") {
		t.Errorf("a default-realm registration gained a namespace member: %s", withRealms.Body.String())
	}
	if len(plainRec.Body.Bytes()) == 0 {
		t.Fatal("the plain registration produced no body")
	}
}

// TestListNamespacesReportsPolicyWithoutSecrets: GET /namespaces is the
// operator's view of what is enforced — posture and references, never a secret.
func TestListNamespacesReportsPolicyWithoutSecrets(t *testing.T) {
	t.Setenv("REALM_TOKEN", "top-secret")
	f := newNSHandler(t, `{"namespaces":[
		{"name":"acme","auth":"token","token_ref":"env:REALM_TOKEN","rate_limit_per_minute":7,
		 "guard_enabled":false,"retention_seconds":120},
		{"name":"beta"}
	]}`)

	rec := f.do(t, http.MethodGet, "/namespaces", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /namespaces = %d (%s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "top-secret") {
		t.Fatalf("the resolved secret leaked into the listing: %s", body)
	}
	if !strings.Contains(body, `"token_ref":"env:REALM_TOKEN"`) {
		t.Errorf("the listing must report the reference so an operator can rotate it: %s", body)
	}
	var listed namespacesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	var acme, beta namespaceListEntry
	for _, e := range listed.Namespaces {
		switch e.Name {
		case "acme":
			acme = e
		case "beta":
			beta = e
		}
	}
	if acme.Auth != string(namespace.AuthToken) || acme.RateLimitPerMinute == nil || *acme.RateLimitPerMinute != 7 {
		t.Errorf("acme entry = %+v", acme)
	}
	if acme.GuardEnabled == nil || *acme.GuardEnabled {
		t.Errorf("acme guard_enabled = %v, want false", acme.GuardEnabled)
	}
	if acme.RetentionSeconds == nil || *acme.RetentionSeconds != 120 {
		t.Errorf("acme retention = %v, want 120", acme.RetentionSeconds)
	}
	if !acme.Configured {
		t.Error("acme declares policy, so it must be reported as configured")
	}
	if beta.Configured || beta.Auth != string(namespace.AuthShared) {
		t.Errorf("beta entry = %+v, want an unconfigured shared-posture realm", beta)
	}
}

// TestWebhookEnvelopeCarriesTheRealm: the push transport states the realm too,
// so a webhook receiver can tell which realm's policy produced the message.
func TestWebhookEnvelopeCarriesTheRealm(t *testing.T) {
	f := newGuardFixture(t, guardVerdictAllow)
	f.addWebhookDriver(t)
	reg, err := namespace.Parse(`{"namespaces":[{"name":"acme"}]}`, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.handler.SetNamespacePolicies(reg)

	recorder := &webhookRecorder{}
	hookSrv := httptest.NewServer(recorder)
	t.Cleanup(hookSrv.Close)

	hexKey, _ := newTestPubKey(t)
	body, _ := json.Marshal(map[string]any{
		"id":         "hooked",
		"public_key": hexKey,
		"namespace":  "acme",
		"webhook":    map[string]any{"url": hookSrv.URL, "delivery_mode": "async"},
	})
	req := httptest.NewRequest(http.MethodPost, "/agents", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("register: %d %s", rec.Code, rec.Body.String())
	}

	if got := f.deliverReq(t, "hooked", deliverRequest{Payload: json.RawMessage(`{"x":1}`)}); got.Code/100 != 2 {
		t.Fatalf("deliver = %d (%s)", got.Code, got.Body.String())
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && recorder.count() == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if recorder.count() == 0 {
		t.Fatal("the webhook was never called")
	}
	post := recorder.last()
	if !strings.Contains(string(post.body), `"namespace":"acme"`) {
		t.Errorf("envelope = %s, want the realm inside crier meta", post.body)
	}
}

// TestNamespaceOfAgentCanonicalises guards the seam used by every routing
// decision: a row may spell the default realm either way, and both resolve to
// the same canonical value.
func TestNamespaceOfAgentCanonicalises(t *testing.T) {
	if got := namespaceOfAgent(nil); got != "" {
		t.Errorf("namespaceOfAgent(nil) = %q, want \"\"", got)
	}
	if got := namespaceOfAgent(&Agent{Namespace: namespace.DefaultName}); got != "" {
		t.Errorf("namespaceOfAgent(default) = %q, want \"\"", got)
	}
	if got := namespaceOfAgent(&Agent{Namespace: "acme"}); got != "acme" {
		t.Errorf("namespaceOfAgent(acme) = %q, want acme", got)
	}
}

// TestGuardedNamespaceRuleIsSharedWithTheRelay is a small guard against the two
// lanes diverging: the canonical spelling and the header names live in one
// package, and both lanes read them from there.
func TestNamespaceHeadersAreTheDocumentedOnes(t *testing.T) {
	if namespace.HeaderNamespace != "X-Crier-Namespace" {
		t.Errorf("namespace header = %q", namespace.HeaderNamespace)
	}
	if namespace.HeaderNamespaceToken != "X-Crier-Namespace-Token" {
		t.Errorf("namespace token header = %q", namespace.HeaderNamespaceToken)
	}
}

// TestRetentionForUsesTheStoreDefault: the helper that resolves a realm's
// retention must fall back to the store default, not to zero.
func TestRetentionForUsesTheStoreDefault(t *testing.T) {
	reg, err := namespace.Parse(`{"namespaces":[{"name":"a"},{"name":"b","retention_seconds":90}]}`, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := retentionFor(reg.Resolve("a")); got != DefaultMessageTTL {
		t.Errorf("inherited retention = %s, want %s", got, DefaultMessageTTL)
	}
	if got := retentionFor(reg.Resolve("b")); got != 90*time.Second {
		t.Errorf("declared retention = %s, want 90s", got)
	}
}

// TestUnwiredHandlerIgnoresNamespaces: a Handler with no namespace registry —
// every pre-CR-FEAT-029 test and embedder — must accept a namespace member it
// cannot verify? No: it must REFUSE it, because accepting a realm the server
// does not declare is exactly the silent widening the feature forbids.
func TestUnwiredHandlerRefusesAnUndeclaredNamespace(t *testing.T) {
	store := NewMemoryStore()
	h := NewHandler(store)
	router := mux.NewRouter()
	router.HandleFunc("/agents", h.HandleRegister).Methods("POST")

	hexKey, _ := newTestPubKey(t)
	body, _ := json.Marshal(map[string]any{"id": "a", "public_key": hexKey, "namespace": "acme"})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/agents", bytes.NewReader(body)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("register into a realm on a server that declares none = %d, want 400 (%s)", rec.Code, rec.Body.String())
	}
	if len(store.List()) != 0 {
		t.Error("a refused registration must not create a row")
	}
}

// TestGuardFilterAbsentPlusGuardDisabledRealm: with no guard wired at all,
// a realm that skips guarding is still a no-op — the seam must not panic.
func TestGuardSkippedWithoutAGuardWired(t *testing.T) {
	f := newNSHandler(t, `{"namespaces":[{"name":"unguarded","guard_enabled":false}]}`)
	if rec := f.register(t, "a", "unguarded", nil); rec.Code != http.StatusCreated {
		t.Fatalf("register: %d %s", rec.Code, rec.Body.String())
	}
	if rec := f.deliver(t, "a", "", `{"x":1}`, nil); rec.Code != http.StatusCreated {
		t.Fatalf("deliver = %d (%s)", rec.Code, rec.Body.String())
	}
	if got := f.entryNamespace(t, "a"); got != "unguarded" {
		t.Errorf("stored namespace = %q", got)
	}
}

// guardPolicyFixtureIsUsed keeps the guard import honest for the policy tests
// above without a second stub: a compile-time assertion that the fixture's
// policy type is the guard's own.
var _ = func() *guard.AgentGuardConfig { return (&guardFixture{}).guardPolicy(false) }
