package registry

// CR-FEAT-036 acceptance — the router dispatch contract at the delivery edge.
//
// The owner-confirmed architecture this row implements one leg of:
//
//	scheduler --(dispatch)--> Crier --(inbox)--> bunker task-router
//	       <--(correlated result)-- Crier <--(reply)--
//
// The bunker router POLLS Crier's inbox because the bunker accepts no inbound
// connection, so everything Crier owes the router has to be visible on a
// retrieve/lease: the optional `model`/`provider` selection hints exactly as
// the scheduler wrote them, and — on the way back — the Crier correlation the
// result answers. Crier's scope is transport, inbox delivery, authorization
// and preservation. It never selects a model or a provider, and its answer to
// "who may deliver where" comes from the verified sender identity plus the
// configured delivery ACL, never from a field inside a payload.
//
// Every test below runs the REAL HTTP surface (the same route table
// cmd/server registers) against a real client, and the raw JSON of every
// message is asserted member-by-member: an "absent", "null" and "blank" hint
// are three different inputs and each is checked as the caller spelled it.

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/stretchr/testify/require"

	"github.com/crier-dev/crier/internal/permissions"
)

// ---------------------------------------------------------------------------
// Harness: a scheduler, a router, and a real HTTP client that polls.
// ---------------------------------------------------------------------------

const (
	cr36Scheduler = "lane-scheduler"
	cr36Router    = "bunker-router"
	cr36Outsider  = "unrelated-agent"
)

// cr36Inbox is a polled inbox body decoded into RAW members, so the tests can
// assert the exact JSON form of each optional hint (absent vs null vs blank vs
// explicit) rather than a lossy Go value.
type cr36Inbox struct {
	Messages []map[string]json.RawMessage `json:"messages"`
	LeaseID  string                       `json:"lease_id"`
	Depth    int                          `json:"queue_depth"`
	Leased   int                          `json:"leased_count"`
}

// cr36Message returns the raw member spelling of one optional key, and whether
// the member is present at all. Absent and explicit-null are different answers:
// the first is "the caller said nothing", the second is "the caller said null".
func cr36Message(msg map[string]json.RawMessage, key string) (string, bool) {
	raw, ok := msg[key]
	if !ok {
		return "", false
	}
	return string(raw), true
}

// cr36Harness is a booted server (the shipped route table) plus a memory
// registry, with an optional armed delivery ACL.
type cr36Harness struct {
	server  *httptest.Server
	store   Store
	handler *Handler
	client  *http.Client
}

func newCR36Harness(t *testing.T, perms *permissions.JSONLStore) *cr36Harness {
	t.Helper()
	return newCR36HarnessWithStore(t, perms, NewMemoryStore())
}

func newCR36HarnessWithStore(t *testing.T, perms *permissions.JSONLStore, store Store) *cr36Harness {
	t.Helper()
	handler := NewHandler(store)
	if perms != nil {
		handler.SetPermissionsChecker(permissions.NewChecker(perms))
	}
	router := mux.NewRouter()
	router.HandleFunc("/agents", handler.HandleRegister).Methods("POST")
	router.HandleFunc("/agents/{id}/inbox", handler.HandleDeliver).Methods("POST")
	router.HandleFunc("/agents/{id}/inbox", handler.HandleRetrieve).Methods("GET")
	router.HandleFunc("/agents/{id}/inbox/ack", handler.HandleAck).Methods("POST")

	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	return &cr36Harness{server: srv, store: store, handler: handler, client: srv.Client()}
}

// signGet reads an agent's inbox the way the shipped agent-scoped gate expects:
// the three-line transcript (method, path, fresh unix timestamp) signed with the
// agent's own ed25519 key. The query string is NOT part of the transcript.
func (h *cr36Harness) signGet(t *testing.T, agentID string, priv ed25519.PrivateKey, path, query string) (int, string) {
	t.Helper()
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	sig := ed25519.Sign(priv, []byte(http.MethodGet+"\n"+path+"\n"+ts))
	if query != "" {
		path += "?" + query
	}
	return h.send(t, http.MethodGet, path, "", map[string]string{
		HeaderAgentID:  agentID,
		HeaderAgentTS:  ts,
		HeaderAgentSig: hex.EncodeToString(sig),
	})
}

// send posts an arbitrary deliver body to target's inbox and returns the status
// and the raw response body.
func (h *cr36Harness) send(t *testing.T, method, path, body string, headers map[string]string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, h.server.URL+path, strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := h.client.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(raw)
}

// deliver posts a deliver body and requires the expected status.
func (h *cr36Harness) deliver(t *testing.T, target, body string, want int) string {
	t.Helper()
	status, respBody := h.send(t, http.MethodPost, "/agents/"+target+"/inbox", body, nil)
	require.Equal(t, want, status, "deliver to %s: body=%s", target, respBody)
	return respBody
}

// poll runs the polling consumer's read: GET /agents/{id}/inbox with an
// optional lease budget, and returns the RAW members of what it was handed.
func (h *cr36Harness) poll(t *testing.T, agent string, leaseSeconds int) cr36Inbox {
	t.Helper()
	path := "/agents/" + agent + "/inbox?max=10"
	if leaseSeconds > 0 {
		path = fmt.Sprintf("%s&lease=%d", path, leaseSeconds)
	}
	status, body := h.send(t, http.MethodGet, path, "", nil)
	require.Equal(t, http.StatusOK, status, "poll %s: body=%s", agent, body)
	var out cr36Inbox
	require.NoError(t, json.Unmarshal([]byte(body), &out), "poll body: %s", body)
	return out
}

// ack acknowledges leased ids under a lease and returns the status.
func (h *cr36Harness) ack(t *testing.T, agent, leaseID string, ids ...string) int {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"lease_id": leaseID, "message_ids": ids})
	require.NoError(t, err)
	status, body := h.send(t, http.MethodPost, "/agents/"+agent+"/inbox/ack", string(raw), nil)
	if len(ids) > 0 && status != http.StatusNoContent {
		t.Logf("ack %s lease=%s ids=%v -> %d %s", agent, leaseID, ids, status, body)
	}
	return status
}

// register adds an agent row to the registry store backing the booted server.
func (h *cr36Harness) register(t *testing.T, id string) {
	t.Helper()
	_, pub := newTestPubKey(t)
	require.NoError(t, h.store.Register(&Agent{ID: id, PublicKey: HexKey(pub)}))
}

// dispatchBody builds a deliver body whose optional members are written as RAW
// JSON text: "" omits the member entirely, otherwise the text is spliced in
// verbatim (so a test can send `null`, `""`, `" "` or an explicit value and
// assert the server reproduces exactly that).
func dispatchBody(modelRaw, providerRaw, replyTo string) string {
	parts := []string{`"payload":{"task":"lane dispatch","lane":"crier-worker"}`}
	if modelRaw != "" {
		parts = append(parts, `"model":`+modelRaw)
	}
	if providerRaw != "" {
		parts = append(parts, `"provider":`+providerRaw)
	}
	if replyTo != "" {
		parts = append(parts, fmt.Sprintf(`"reply_to":%q`, replyTo))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// ---------------------------------------------------------------------------
// AC1 — the four selection cases, spelled three ways, across the real wire.
// ---------------------------------------------------------------------------

// hintSpelling is one supported input spelling of an optional hint and what the
// polling router must then see: "" means the member is absent from the message,
// and selects records whether the router must read it as a concrete choice.
type hintSpelling struct {
	name    string
	raw     string // the request-body member text; "" = the member is not sent
	want    string // the member text on the polled message; "" = the member is absent
	selects bool   // true only for an explicit value
}

// The supported DEFAULT input spellings and their wire representation.
// `absent` is the canonical default — the member is simply not
// there, which is what every pre-CR-FEAT-036 delivery looked like. An explicit
// JSON `null` means the same thing (a member with no value is no value), so it
// canonicalises to it rather than inventing a second default state for the
// consumer to handle. A blank string IS a value the caller wrote, so it is
// preserved as sent — and it is still a DEFAULT for selection purposes.
var cr36DefaultSpellings = []hintSpelling{
	{name: "absent", raw: "", want: ""},
	{name: "null", raw: "null", want: ""},
	{name: "blank", raw: `""`, want: `""`},
	{name: "blank_whitespace", raw: `"  "`, want: `"  "`},
}

const (
	cr36ExplicitModel    = `"gpt-5.3"`
	cr36ExplicitProvider = `"zai-glm-default"`
)

// TestDispatchHintsSurviveDeliveryToAPollingRouter is AC1's core: for EACH of
// the four selection combinations the consumer contract names — and for each
// supported spelling of the default arm — the hint the scheduler sent is the
// hint the polling router reads, and Crier has filled in nothing.
func TestDispatchHintsSurviveDeliveryToAPollingRouter(t *testing.T) {
	testDispatchHintsRoundTrip(t, func(t *testing.T) *cr36Harness { return newCR36Harness(t, nil) })
}

func testDispatchHintsRoundTrip(t *testing.T, harness func(*testing.T) *cr36Harness) {
	type hintCase struct {
		name                string
		modelRaw            string
		providerRaw         string
		wantModel           string // expected raw member text on the poll, "" when the member must be absent
		wantProvider        string
		wantModelSelects    bool
		wantProviderSelects bool
	}
	var cases []hintCase

	// (1) explicit model + explicit provider: use that pair.
	cases = append(cases, hintCase{
		name: "case1_explicit_model_explicit_provider", modelRaw: cr36ExplicitModel, providerRaw: cr36ExplicitProvider,
		wantModel: cr36ExplicitModel, wantProvider: cr36ExplicitProvider,
		wantModelSelects: true, wantProviderSelects: true,
	})
	cases = append(cases, hintCase{
		name: "explicit_values_not_trimmed_or_catalogued", modelRaw: `" Unknown/Model-X "`, providerRaw: `" Provider-X "`,
		wantModel: `" Unknown/Model-X "`, wantProvider: `" Provider-X "`,
		wantModelSelects: true, wantProviderSelects: true,
	})
	// (2) default model + explicit provider: the router picks a model inside
	// the named provider, for every supported default spelling.
	for _, sp := range cr36DefaultSpellings {
		cases = append(cases, hintCase{
			name: "case2_default_model_explicit_provider_" + sp.name, modelRaw: sp.raw, providerRaw: cr36ExplicitProvider,
			wantModel: sp.want, wantProvider: cr36ExplicitProvider,
			wantModelSelects: sp.selects, wantProviderSelects: true,
		})
	}
	// (3) explicit model + default provider: the router picks a compatible
	// provider for the named model.
	for _, sp := range cr36DefaultSpellings {
		cases = append(cases, hintCase{
			name: "case3_explicit_model_default_provider_" + sp.name, modelRaw: cr36ExplicitModel, providerRaw: sp.raw,
			wantModel: cr36ExplicitModel, wantProvider: sp.want,
			wantModelSelects: true, wantProviderSelects: sp.selects,
		})
	}
	// (4) both default: the router chooses the pair from the prompt.
	for _, sp := range cr36DefaultSpellings {
		cases = append(cases, hintCase{
			name: "case4_both_default_model_" + sp.name, modelRaw: sp.raw, providerRaw: "",
			wantModel: sp.want, wantProvider: "",
			wantModelSelects: sp.selects, wantProviderSelects: false,
		})
	}
	cases = append(cases, hintCase{name: "case4_both_default_both_absent"})
	cases = append(cases, hintCase{name: "case4_both_default_both_null", modelRaw: "null", providerRaw: "null"})

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := harness(t)
			h.register(t, cr36Router)

			body := dispatchBody(tc.modelRaw, tc.providerRaw, "")

			// The accept must not carry a selection Crier invented: the
			// deliver response echoes transport facts, and a model/provider
			// member appearing there with a concrete value would mean the bus
			// had chosen one.
			accept := h.deliver(t, cr36Router, body, http.StatusCreated)
			acceptMembers := map[string]json.RawMessage{}
			require.NoError(t, json.Unmarshal([]byte(accept), &acceptMembers))
			for _, key := range []string{"model", "provider"} {
				raw, present := acceptMembers[key]
				require.False(t, present && string(raw) != "null" && string(raw) != `""` && string(raw) != `"  "`,
					"the deliver accept invented a %s (%s): the bus never selects", key, string(raw))
			}

			// The polling router's read — the surface the bunker actually uses.
			got := h.poll(t, cr36Router, 30)
			require.Len(t, got.Messages, 1, "the dispatch must be pollable")
			msg := got.Messages[0]
			require.NotEmpty(t, got.LeaseID)

			model, modelPresent := cr36Message(msg, "model")
			provider, providerPresent := cr36Message(msg, "provider")

			if tc.wantModel == "" {
				require.False(t, modelPresent,
					"an absent model hint must stay absent on the poll (the canonical default is the omitted member), got %s", model)
			} else {
				require.True(t, modelPresent, "the model hint must be visible to the polling router")
				require.Equal(t, tc.wantModel, model, "the model hint must survive delivery byte-for-byte")
			}
			if tc.wantProvider == "" {
				require.False(t, providerPresent,
					"an absent provider hint must stay absent on the poll, got %s", provider)
			} else {
				require.True(t, providerPresent, "the provider hint must be visible to the polling router")
				require.Equal(t, tc.wantProvider, provider, "the provider hint must survive delivery byte-for-byte")
			}

			// The default/non-default CLASSIFICATION a router branches on,
			// read off the same bytes over the same HTTP surface: a blank
			// hint is present-but-default, an absent one is default, and only
			// an explicit value reads as a choice.
			var wire struct {
				Model    *SelectionHint `json:"model"`
				Provider *SelectionHint `json:"provider"`
			}
			rawMsg, err := json.Marshal(msg)
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(rawMsg, &wire))
			require.Equal(t, tc.wantModelSelects, wire.Model.Selects(), "model: Selects must agree with what was sent")
			require.Equal(t, tc.wantProviderSelects, wire.Provider.Selects(), "provider: Selects must agree with what was sent")
			require.Equal(t, !tc.wantModelSelects, wire.Model.IsDefault(), "model: IsDefault must agree with what was sent")
			require.Equal(t, !tc.wantProviderSelects, wire.Provider.IsDefault(), "provider: IsDefault must agree with what was sent")
			// Whatever the caller sent, Crier has not chosen a pair: a
			// "selecting" hint is exactly the value the caller wrote.
			if wire.Model.Selects() {
				require.Equal(t, strings.Trim(tc.wantModel, `"`), wire.Model.Value)
			}

			// The lease is releasable — this test leaves no message behind.
			require.Equal(t, http.StatusNoContent, h.ack(t, cr36Router, got.LeaseID, cr36ID(t, msg)))
		})
	}
}

// rawID reads the message id member as a plain string (the poll decodes members
// as raw JSON, so the id needs unwrapping).
func cr36ID(t *testing.T, msg map[string]json.RawMessage) string {
	t.Helper()
	var id string
	require.NoError(t, json.Unmarshal(msg["id"], &id))
	return id
}

// TestDispatchWithoutHintsIsByteIdenticalToBefore pins the "no new member on a
// hint-less delivery" half of the contract: a message that names no selection
// carries no `model`, `provider` or `reply_to` member at all — the canonical
// default is the absent member, and no default value is ever filled in.
func TestDispatchWithoutHintsIsByteIdenticalToBefore(t *testing.T) {
	h := newCR36Harness(t, nil)
	h.register(t, cr36Router)

	h.deliver(t, cr36Router, `{"payload":{"task":"plain"}}`, http.StatusCreated)

	got := h.poll(t, cr36Router, 30)
	require.Len(t, got.Messages, 1)
	msg := got.Messages[0]
	for _, key := range []string{"model", "provider", "reply_to"} {
		_, present := cr36Message(msg, key)
		require.False(t, present,
			"%s must not appear on a delivery that did not send it — Crier invents no default", key)
	}

	// The stored entry agrees with the wire: a nil hint, not an empty one.
	// Peek reads the row without claiming it, so the poll above keeps its lease.
	peeker, ok := h.store.(InboxPeeker)
	require.True(t, ok, "the memory store can answer a Peek")
	entry, err := peeker.Peek(cr36Router, cr36ID(t, msg))
	require.NoError(t, err)
	require.Nil(t, entry.Model)
	require.Nil(t, entry.Provider)
	require.Empty(t, entry.ReplyTo)
}

// TestDispatchHintsAreIndependent proves the two hints are independent members:
// naming one never materialises the other.
func TestDispatchHintsAreIndependent(t *testing.T) {
	h := newCR36Harness(t, nil)
	h.register(t, cr36Router)

	h.deliver(t, cr36Router, dispatchBody(cr36ExplicitModel, "", ""), http.StatusCreated)

	got := h.poll(t, cr36Router, 30)
	require.Len(t, got.Messages, 1)
	model, modelPresent := cr36Message(got.Messages[0], "model")
	require.True(t, modelPresent)
	require.Equal(t, cr36ExplicitModel, model)
	_, providerPresent := cr36Message(got.Messages[0], "provider")
	require.False(t, providerPresent, "an unsent provider must stay absent while the model is explicit")
}

// TestDispatchHintRejectsNonStringShape documents the boundary: a hint is a
// string or null. `5`, `{}` and `[]` are refused at the HTTP edge with the
// existing invalid-json answer instead of being silently dropped — a selection
// the bus cannot carry must not look like one it did.
func TestDispatchHintRejectsNonStringShape(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"number_hint", `{"payload":{"x":1},"model":5}`},
		{"object_hint", `{"payload":{"x":1},"provider":{"name":"zai"}}`},
		{"array_hint", `{"payload":{"x":1},"model":["gpt-5.3"]}`},
		{"bool_hint", `{"payload":{"x":1},"provider":true}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newCR36Harness(t, nil)
			h.register(t, cr36Router)
			status, _ := h.send(t, http.MethodPost, "/agents/"+cr36Router+"/inbox", tc.body, nil)
			require.Equal(t, http.StatusBadRequest, status)
			depth, _, _, err := h.store.Stats(cr36Router)
			require.NoError(t, err)
			require.Zero(t, depth, "a refused delivery stores nothing")
		})
	}
}

// ---------------------------------------------------------------------------
// AC3 — the stub router: lease, correlated reply, ack, and lease expiry.
// ---------------------------------------------------------------------------

// TestStubRouterLeasesRepliesAndAcks is the round trip the scheduler depends
// on: publish a dispatch, lease it as the router, return a reply that carries
// the ORIGINAL Crier message id, acknowledge only the finished work.
func TestStubRouterLeasesRepliesAndAcks(t *testing.T) {
	h := newCR36Harness(t, nil)
	h.register(t, cr36Scheduler)
	h.register(t, cr36Router)

	// The scheduler dispatches a lane with explicit hints.
	h.deliver(t, cr36Router, dispatchBody(cr36ExplicitModel, cr36ExplicitProvider, ""), http.StatusCreated)

	// The router polls (it cannot be reached inbound) and leases the dispatch.
	leased := h.poll(t, cr36Router, 30)
	require.Len(t, leased.Messages, 1, "the router must see the dispatch on its own poll")
	require.NotEmpty(t, leased.LeaseID)
	dispatchID := cr36ID(t, leased.Messages[0])

	// It returns a correlated reply into the scheduler's inbox. The
	// correlation is Crier's own identifier — no scheduler task id is added
	// to the envelope.
	replyBody := fmt.Sprintf(`{"payload":{"result":"done","lane":"crier-worker"},"sender":%q,"reply_to":%q}`, cr36Router, dispatchID)
	h.deliver(t, cr36Scheduler, replyBody, http.StatusCreated)

	// The scheduler matches the result to its dispatch from its inbox alone.
	answer := h.poll(t, cr36Scheduler, 30)
	require.Len(t, answer.Messages, 1, "the scheduler must see the reply on its own poll")
	correlation, present := cr36Message(answer.Messages[0], "reply_to")
	require.True(t, present, "the reply must carry the Crier correlation the scheduler matches on")
	var gotCorrelation string
	require.NoError(t, json.Unmarshal([]byte(correlation), &gotCorrelation))
	require.Equal(t, dispatchID, gotCorrelation,
		"the reply must carry the ORIGINAL dispatch message id, not a new one")
	// The reply is a normal message: the bus did not touch the result payload.
	// (An InboxEntry's payload is base64 on the wire — it is a []byte — so the
	// documented consumer step is decode-then-parse.)
	rawPayload, err := base64.StdEncoding.DecodeString(strings.Trim(string(answer.Messages[0]["payload"]), `"`))
	require.NoError(t, err, "the payload travels base64-encoded")
	var payload map[string]any
	require.NoError(t, json.Unmarshal(rawPayload, &payload))
	require.Equal(t, "done", payload["result"])

	// Work finished → acknowledge. An ack is per-lease and is a real fact.
	require.Equal(t, http.StatusNoContent, h.ack(t, cr36Router, leased.LeaseID, dispatchID))

	// The dispatch is gone from the router's inbox, and a second ack of the
	// same message is not a silent success.
	require.Empty(t, h.poll(t, cr36Router, 0).Messages, "an acknowledged message must not be re-delivered")
	require.Equal(t, http.StatusNotFound, h.ack(t, cr36Router, leased.LeaseID, dispatchID),
		"acknowledging an already-removed message must be reported, not swallowed")
}

// TestRouterInboxIsReadableOnlyByTheRoutersOwnSignedIdentity is the identity
// half of AC2 on the POLL surface rather than the delivery surface: the router
// may read the dispatch it was sent only as itself. Unsigned reads are refused,
// and a signed read by a DIFFERENT agent (the scheduler) is refused too —
// authorization is the verified identity, never a claim in the request.
func TestRouterInboxIsReadableOnlyByTheRoutersOwnSignedIdentity(t *testing.T) {
	h := newCR36Harness(t, nil)
	// The shipped default: per-agent signing ON (config.RequireAgentSig).
	h.handler.SetRequireAgentSig(true)
	_, routerKey := testAgentKeypair(t, h.store, cr36Router)
	_, schedulerKey := testAgentKeypair(t, h.store, cr36Scheduler)

	h.deliver(t, cr36Router, dispatchBody(cr36ExplicitModel, cr36ExplicitProvider, ""), http.StatusCreated)

	// Nobody may read the router's inbox without the router's signature.
	status, body := h.send(t, http.MethodGet, "/agents/"+cr36Router+"/inbox", "", nil)
	require.Equal(t, http.StatusUnauthorized, status, "an unsigned inbox read must be refused: %s", body)

	// The scheduler's own (valid) signature is not a credential for the
	// router's inbox.
	status, body = h.signGet(t, cr36Scheduler, schedulerKey, "/agents/"+cr36Router+"/inbox", "")
	require.Equal(t, http.StatusForbidden, status,
		"an agent may only read its own inbox, whatever it signs: %s", body)

	// The router's own signature leases the dispatch — hints and all.
	status, body = h.signGet(t, cr36Router, routerKey, "/agents/"+cr36Router+"/inbox", "max=10&lease=30")
	require.Equal(t, http.StatusOK, status, "the router's own signed poll must be admitted: %s", body)
	var got cr36Inbox
	require.NoError(t, json.Unmarshal([]byte(body), &got))
	require.Len(t, got.Messages, 1)
	require.NotEmpty(t, got.LeaseID)
	model, present := cr36Message(got.Messages[0], "model")
	require.True(t, present, "the signed poll sees the dispatch's hints")
	require.Equal(t, cr36ExplicitModel, model)
}

// TestRemoteProxyForwardsSelectionHintsAndCorrelation pins the proxy half of the
// contract (CR-FEAT-036). A RemoteStore builds the downstream deliver body field
// by field, so a new optional member that is not forwarded is silently dropped
// for every delivery that crosses a proxy — the dispatch would arrive at the
// router with no way to tell an explicit choice from a default, and a result
// would arrive at the scheduler with no id to match on.
func TestRemoteProxyForwardsSelectionHintsAndCorrelation(t *testing.T) {
	var (
		mu   sync.Mutex
		last []byte
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		last = body
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg-proxied","transport":"inbox"}`))
	}))
	t.Cleanup(srv.Close)

	rs := NewRemoteStore(srv.URL, "remote-client", "")
	sent := func() map[string]json.RawMessage {
		t.Helper()
		mu.Lock()
		body := last
		mu.Unlock()
		var out map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(body, &out), "forwarded body: %s", body)
		return out
	}

	// Explicit hints + a correlation: all three cross the proxy unchanged.
	require.NoError(t, rs.Deliver("router-svc", &InboxEntry{
		Payload:  []byte(`{"lane":"crier-worker"}`),
		Model:    NewHint("gpt-5.3"),
		Provider: NewHint("zai-glm-default"),
		ReplyTo:  "dispatch-42",
	}))
	forwarded := sent()
	require.JSONEq(t, `"gpt-5.3"`, string(forwarded["model"]), "the model hint must be forwarded verbatim")
	require.JSONEq(t, `"zai-glm-default"`, string(forwarded["provider"]))
	require.JSONEq(t, `"dispatch-42"`, string(forwarded["reply_to"]))

	// A blank hint crosses as the blank string the caller wrote, and the hint
	// the caller did not send is not forwarded at all.
	require.NoError(t, rs.Deliver("router-svc", &InboxEntry{
		Payload: []byte(`{"lane":"crier-worker"}`),
		Model:   &SelectionHint{Form: HintBlank, Value: ""},
	}))
	forwarded = sent()
	require.JSONEq(t, `""`, string(forwarded["model"]))
	_, present := forwarded["provider"]
	require.False(t, present, "a hint the caller did not send must not be forwarded at all")

	// A delivery that names none of them forwards none of them: the canonical
	// default is the absent member, and no default value is manufactured.
	require.NoError(t, rs.Deliver("router-svc", &InboxEntry{Payload: []byte(`{"lane":"crier-worker"}`)}))
	forwarded = sent()
	for _, key := range []string{"model", "provider", "reply_to"} {
		_, present := forwarded[key]
		require.False(t, present, "%s must not be forwarded for a delivery that did not send it", key)
	}
}

// TestStubRouterLeaseExpiryAndRedelivery proves the existing lease contract is
// untouched by the hint fields: an unfinished dispatch is redelivered after the
// lease expires, a stale lease cannot ack it, and the re-leased copy still
// carries the scheduler's hints.
func TestStubRouterLeaseExpiryAndRedelivery(t *testing.T) {
	h := newCR36Harness(t, nil)
	h.register(t, cr36Router)

	h.deliver(t, cr36Router, dispatchBody(cr36ExplicitModel, cr36ExplicitProvider, ""), http.StatusCreated)

	first := h.poll(t, cr36Router, 1)
	require.Len(t, first.Messages, 1)
	require.NotEmpty(t, first.LeaseID)
	firstID := cr36ID(t, first.Messages[0])

	// While the lease is live the message is not handed out twice.
	second := h.poll(t, cr36Router, 1)
	require.Empty(t, second.Messages, "a live lease must not be double-delivered")
	require.Empty(t, second.LeaseID, "a poll that claims nothing mints no lease")

	// The router dies before finishing: the lease expires and the dispatch
	// comes back — with the hints the scheduler wrote still on it.
	time.Sleep(1100 * time.Millisecond)
	again := h.poll(t, cr36Router, 30)
	require.Len(t, again.Messages, 1, "an expired lease must redeliver the unacknowledged dispatch")
	require.NotEmpty(t, again.LeaseID)
	require.NotEqual(t, first.LeaseID, again.LeaseID, "redelivery mints a fresh lease")
	require.Equal(t, firstID, cr36ID(t, again.Messages[0]))
	model, present := cr36Message(again.Messages[0], "model")
	require.True(t, present, "the redelivered dispatch still carries the scheduler's hints")
	require.Equal(t, cr36ExplicitModel, model)

	// The stale (expired) lease cannot acknowledge work it no longer owns.
	require.Equal(t, http.StatusConflict, h.ack(t, cr36Router, first.LeaseID, firstID),
		"an expired lease must not acknowledge the message it lost")

	// The fresh lease can — and does.
	require.Equal(t, http.StatusNoContent, h.ack(t, cr36Router, again.LeaseID, firstID))
	require.Empty(t, h.poll(t, cr36Router, 0).Messages)
}

// ---------------------------------------------------------------------------
// AC2 — least-privilege service identities through the REAL delivery ACL.
// ---------------------------------------------------------------------------

// cr36ACL is the armed-ACL harness: a scheduler service identity, the router
// service identity, and an unrelated principal, each with its own binding.
type cr36ACL struct {
	*cr36Harness
	perms *permissions.JSONLStore
}

func newCR36ACL(t *testing.T) *cr36ACL {
	t.Helper()
	pstore, err := permissions.NewJSONLStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = pstore.Close() })

	h := newCR36Harness(t, pstore)
	for _, id := range []string{cr36Scheduler, cr36Router, cr36Outsider} {
		h.register(t, id)
	}

	now := time.Now()
	records := []*permissions.Record{
		// The two service identities, each reachable only through a grant.
		(&permissions.AgentInfo{ID: cr36Scheduler, Class: permissions.ClassService}).Record(now),
		(&permissions.AgentInfo{ID: cr36Router, Class: permissions.ClassService}).Record(now),
		(&permissions.AgentInfo{ID: cr36Outsider, Class: permissions.ClassService}).Record(now),
		// The humans behind them.
		(&permissions.Principal{ID: "prin_sched", Role: permissions.RoleMember, Status: permissions.PrincipalActive}).Record(now),
		(&permissions.Principal{ID: "prin_router", Role: permissions.RoleMember, Status: permissions.PrincipalActive}).Record(now),
		(&permissions.Principal{ID: "prin_outsider", Role: permissions.RoleMember, Status: permissions.PrincipalActive}).Record(now),
		(&permissions.Binding{ID: "bind_sched", Principal: "prin_sched", Agent: cr36Scheduler, AsAgent: true}).Record(now),
		(&permissions.Binding{ID: "bind_router", Principal: "prin_router", Agent: cr36Router, AsAgent: true}).Record(now),
		(&permissions.Binding{ID: "bind_outsider", Principal: "prin_outsider", Agent: cr36Outsider, AsAgent: true}).Record(now),
		// Least privilege: exactly the two sends this architecture needs —
		// scheduler -> router (the dispatch) and router -> scheduler (the
		// correlated result). Nothing else is granted.
		(&permissions.Grant{
			ID: "grant_dispatch", Principal: "prin_sched",
			Subject: permissions.Subject{Type: permissions.SubjectAgent, Ref: cr36Router},
			Actions: []permissions.Action{permissions.ActionSend}, GrantedBy: "prin_owner", GrantedAt: now,
		}).Record(now),
		(&permissions.Grant{
			ID: "grant_result", Principal: "prin_router",
			Subject: permissions.Subject{Type: permissions.SubjectAgent, Ref: cr36Scheduler},
			Actions: []permissions.Action{permissions.ActionSend}, GrantedBy: "prin_owner", GrantedAt: now,
		}).Record(now),
	}
	for _, rec := range records {
		require.NoError(t, pstore.Append(context.Background(), rec))
	}
	return &cr36ACL{cr36Harness: h, perms: pstore}
}

// sendAs delivers body as the identity (principal, as_agent) — the §6.3
// spelling of "this service acts as this agent" — and returns the status and
// raw response body.
func (a *cr36ACL) sendAs(t *testing.T, target, principal, asAgent, body string, headers map[string]string) (int, string) {
	t.Helper()
	var envelope map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(body), &envelope))
	pid, err := json.Marshal(principal)
	require.NoError(t, err)
	identity, err := json.Marshal(asAgent)
	require.NoError(t, err)
	envelope["principal_id"] = pid
	envelope["as_agent"] = identity
	merged, err := json.Marshal(envelope)
	require.NoError(t, err)
	return a.send(t, http.MethodPost, "/agents/"+target+"/inbox", string(merged), headers)
}

// TestRouterDispatchAuthorizationAllowsTheTwoServiceSends is the positive half
// of AC2: least-privilege identities can do exactly the two deliveries the
// architecture needs, and the dispatch reaches the router with its hints.
func TestRouterDispatchAuthorizationAllowsTheTwoServiceSends(t *testing.T) {
	a := newCR36ACL(t)

	// scheduler -> router: the dispatch, hints intact.
	status, body := a.sendAs(t, cr36Router, "prin_sched", cr36Scheduler,
		dispatchBody(cr36ExplicitModel, cr36ExplicitProvider, ""), nil)
	require.Equal(t, http.StatusCreated, status, "the granted dispatch must be admitted: %s", body)

	leased := a.poll(t, cr36Router, 30)
	require.Len(t, leased.Messages, 1)
	dispatchID := cr36ID(t, leased.Messages[0])
	model, present := cr36Message(leased.Messages[0], "model")
	require.True(t, present)
	require.Equal(t, cr36ExplicitModel, model, "authorization must not disturb the selection hints")

	// router -> scheduler: the correlated result.
	reply := fmt.Sprintf(`{"payload":{"result":"done"},"reply_to":%q}`, dispatchID)
	status, body = a.sendAs(t, cr36Scheduler, "prin_router", cr36Router, reply, nil)
	require.Equal(t, http.StatusCreated, status, "the granted reply must be admitted: %s", body)

	answer := a.poll(t, cr36Scheduler, 30)
	require.Len(t, answer.Messages, 1)
	var correlation string
	require.NoError(t, json.Unmarshal(answer.Messages[0]["reply_to"], &correlation))
	require.Equal(t, dispatchID, correlation)
}

// TestRouterDispatchAuthorizationDeniesUnrelatedPairs is the negative half of
// AC2: least privilege means the pairs nobody granted are refused with a named
// error, and nothing is stored.
func TestRouterDispatchAuthorizationDeniesUnrelatedPairs(t *testing.T) {
	a := newCR36ACL(t)
	dispatch := dispatchBody(cr36ExplicitModel, cr36ExplicitProvider, "")

	cases := []struct {
		name      string
		target    string
		principal string
		asAgent   string
		headers   map[string]string
		body      string
	}{
		{
			name: "router_may_not_send_to_its_own_inbox", target: cr36Router,
			principal: "prin_router", asAgent: cr36Router, body: dispatch,
		},
		{
			name: "scheduler_may_not_send_to_an_unrelated_agent", target: cr36Outsider,
			principal: "prin_sched", asAgent: cr36Scheduler, body: dispatch,
		},
		{
			name: "outsider_may_not_reach_the_router", target: cr36Router,
			principal: "prin_outsider", asAgent: cr36Outsider, body: dispatch,
		},
		{
			name: "agent_identity_alone_reaches_nothing", target: cr36Router,
			headers: map[string]string{"X-Agent-ID": cr36Scheduler}, body: dispatch,
		},
		{
			name: "anonymous_sender_reaches_nothing", target: cr36Router,
			body: `{"payload":{"x":1},"sender":"lane-scheduler"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var status int
			var body string
			if tc.principal != "" {
				status, body = a.sendAs(t, tc.target, tc.principal, tc.asAgent, tc.body, tc.headers)
			} else {
				status, body = a.send(t, http.MethodPost, "/agents/"+tc.target+"/inbox", tc.body, tc.headers)
			}
			require.Equal(t, http.StatusForbidden, status, "an ungranted delivery must be refused: %s", body)
			var refusal struct {
				Error  string `json:"error"`
				Reason string `json:"reason"`
				Action string `json:"action"`
			}
			require.NoError(t, json.Unmarshal([]byte(body), &refusal), "refusal body: %s", body)
			require.Equal(t, permissions.ErrorDeliveryForbidden, refusal.Error,
				"the refusal must be machine-readable, never prose")
			require.Contains(t, []string{permissions.ReasonNoGrant, permissions.ReasonAnonymous}, refusal.Reason)

			depth, _, _, err := a.store.Stats(tc.target)
			require.NoError(t, err)
			require.Zero(t, depth, "a refused delivery must store nothing")
		})
	}
}

// TestPayloadReplyToCannotGrantAuthority is the AC2 clause that stops a caller
// from talking its way into an inbox: authority comes from the verified sender
// identity plus the configured grant, and a `reply_to` — in the envelope or in
// the payload — confers nothing. The same forged request is refused with and
// without it, and the same granted request is admitted with and without it.
func TestPayloadReplyToCannotGrantAuthority(t *testing.T) {
	a := newCR36ACL(t)

	// The forged shapes an attacker would try: the envelope claiming to answer
	// the scheduler, and the payload claiming to BE the scheduler.
	forged := []struct {
		name string
		body string
	}{
		{"envelope_reply_to_names_the_scheduler", `{"payload":{"x":1},"reply_to":"lane-scheduler-msg-id"}`},
		{"payload_claims_reply_to_and_sender", `{"payload":{"x":1,"reply_to":"lane-scheduler-msg-id","sender":"lane-scheduler","as_agent":"lane-scheduler"}}`},
		{"both", `{"payload":{"x":1,"reply_to":"lane-scheduler-msg-id"},"reply_to":"lane-scheduler-msg-id"}`},
	}
	for _, tc := range forged {
		t.Run("forged_"+tc.name, func(t *testing.T) {
			status, body := a.sendAs(t, cr36Router, "prin_outsider", cr36Outsider, tc.body, nil)
			require.Equal(t, http.StatusForbidden, status,
				"a payload/inline reply_to must not grant authority: %s", body)

			// The SAME identity sending the SAME body with the reply_to
			// removed is refused identically — the field changes nothing.
			plain := `{"payload":{"x":1}}`
			statusPlain, _ := a.sendAs(t, cr36Router, "prin_outsider", cr36Outsider, plain, nil)
			require.Equal(t, statusPlain, status,
				"the authorization answer must not depend on a reply_to value")
		})
	}

	// The other direction: an authorized send is authorized whatever the
	// correlation says, so the ACL neither grants nor revokes on it.
	granted := `{"payload":{"x":1},"reply_to":"anything-at-all"}`
	status, body := a.sendAs(t, cr36Router, "prin_sched", cr36Scheduler, granted, nil)
	require.Equal(t, http.StatusCreated, status, "a granted send stays granted: %s", body)

	statusNoCorrelation, _ := a.sendAs(t, cr36Router, "prin_sched", cr36Scheduler, `{"payload":{"x":1}}`, nil)
	require.Equal(t, http.StatusCreated, statusNoCorrelation)

	// And the stored messages carry exactly the correlations they were sent
	// with — provenance, not an identity the bus derived.
	entries, _, err := a.store.Retrieve(cr36Router, 30*time.Second, 10)
	require.NoError(t, err)
	require.Len(t, entries, 2)
	stored := make([]string, 0, len(entries))
	for _, entry := range entries {
		stored = append(stored, entry.ReplyTo)
	}
	require.ElementsMatch(t, []string{"", "anything-at-all"}, stored,
		"the two granted deliveries must carry exactly the correlations they were sent with")
}

// ---------------------------------------------------------------------------
// Unit: the hint type, and the storage representation behind it.
// ---------------------------------------------------------------------------

func TestSelectionHintWireForms(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want SelectionHint
	}{
		{"explicit", `"gpt-5.3"`, SelectionHint{Form: HintExplicit, Value: "gpt-5.3"}},
		{"blank_empty", `""`, SelectionHint{Form: HintBlank, Value: ""}},
		{"blank_whitespace", `"  "`, SelectionHint{Form: HintBlank, Value: "  "}},
		{"null", `null`, SelectionHint{Form: HintNull}},
		{"explicit_with_spaces_is_not_trimmed", `" gpt-5.3 "`, SelectionHint{Form: HintExplicit, Value: " gpt-5.3 "}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var h SelectionHint
			require.NoError(t, json.Unmarshal([]byte(tc.raw), &h))
			require.Equal(t, tc.want.Form, h.Form)
			require.Equal(t, tc.want.Value, h.Value)

			// The round trip reproduces the caller's spelling byte-for-byte:
			// no trimming, no rewriting null into "" and no rewriting "" into
			// null.
			out, err := json.Marshal(h)
			require.NoError(t, err)
			require.Equal(t, tc.raw, string(out), "the hint must marshal back as it arrived")

			require.Equal(t, tc.want.Form == HintExplicit, h.Selects())
			require.Equal(t, tc.want.Form != HintExplicit, h.IsDefault())
		})
	}

	// Absent is the nil pointer, and it is the canonical default.
	var absent *SelectionHint
	require.True(t, absent.IsDefault())
	require.False(t, absent.Selects())
	require.Equal(t, "", absent.String())

	// A value the caller constructed without a form is judged by its value.
	require.True(t, (&SelectionHint{}).IsDefault())
	require.True(t, NewHint("").IsDefault(), "an empty value is the blank form")
	require.True(t, NewHint(" ").IsDefault(), "a whitespace value is the blank form")
	require.False(t, NewHint("gpt-5.3").IsDefault())
	require.Equal(t, "gpt-5.3", NewHint("gpt-5.3").String())
}

func TestSelectionHintRejectsNonStringShapes(t *testing.T) {
	for _, raw := range []string{`5`, `{}`, `[]`, `true`, `"unterminated`} {
		t.Run(raw, func(t *testing.T) {
			var h SelectionHint
			require.Error(t, json.Unmarshal([]byte(raw), &h),
				"a hint that is neither a string nor null must be refused")
		})
	}
}

// TestSelectionHintStorageRoundTrip pins the JSONB representation behind the
// wire: it is lossless for the spellings a delivery can carry (explicit, blank,
// absent), it stores SQL NULL for "sent nothing", and it degrades to "nothing
// sent" rather than failing a read when the column is unreadable.
func TestSelectionHintStorageRoundTrip(t *testing.T) {
	blank := &SelectionHint{Form: HintBlank, Value: ""}
	space := &SelectionHint{Form: HintBlank, Value: " "}
	null := &SelectionHint{Form: HintNull}

	cases := []struct {
		name      string
		entry     *InboxEntry
		wantJSONB string // "" = the column must be SQL NULL
		// wantBack is what a read of that column must produce.
		wantModel    *SelectionHint
		wantProvider *SelectionHint
	}{
		{
			name: "nothing_sent", entry: &InboxEntry{}, wantJSONB: "",
			wantModel: nil, wantProvider: nil,
		},
		{
			name: "explicit_pair", entry: &InboxEntry{Model: NewHint("gpt-5.3"), Provider: NewHint("zai-glm-default")},
			wantJSONB:    `{"model":"gpt-5.3","provider":"zai-glm-default"}`,
			wantModel:    NewHint("gpt-5.3"),
			wantProvider: NewHint("zai-glm-default"),
		},
		{
			// An explicit null written into the column is read back as the
			// canonical default (no value), the same state an absent member
			// produces — one default representation, in storage too.
			name: "null_provider_canonicalises_on_read", entry: &InboxEntry{Model: NewHint("gpt-5.3"), Provider: null},
			wantJSONB:    `{"model":"gpt-5.3","provider":null}`,
			wantModel:    NewHint("gpt-5.3"),
			wantProvider: nil,
		},
		{
			name: "blank_model_default_provider_absent", entry: &InboxEntry{Model: blank},
			wantJSONB:    `{"model":""}`,
			wantModel:    blank,
			wantProvider: nil,
		},
		{
			name: "whitespace_is_preserved_as_sent", entry: &InboxEntry{Model: space},
			wantJSONB:    `{"model":" "}`,
			wantModel:    space,
			wantProvider: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := selectionJSONB(tc.entry)
			if tc.wantJSONB == "" {
				require.Nil(t, got, "nothing sent must store SQL NULL, not an empty document")
				return
			}
			asString, ok := got.(string)
			require.True(t, ok, "the column value must be the JSON text, got %T", got)
			require.Equal(t, tc.wantJSONB, asString)

			var back InboxEntry
			applySelectionJSONB(&back, []byte(asString))
			require.Equal(t, tc.wantModel.Form, back.Model.Form)
			require.Equal(t, tc.wantModel.Value, back.Model.Value)
			if tc.wantProvider == nil {
				require.Nil(t, back.Provider, "a default provider must read back as the canonical default")
			} else {
				require.Equal(t, tc.wantProvider.Form, back.Provider.Form)
				require.Equal(t, tc.wantProvider.Value, back.Provider.Value)
			}
		})
	}

	// A corrupt column must not make a delivered message unreadable.
	var entry InboxEntry
	applySelectionJSONB(&entry, []byte(`{"model":`))
	require.Nil(t, entry.Model)
	require.Nil(t, entry.Provider)
	applySelectionJSONB(&entry, nil)
	require.Nil(t, entry.Model)
}

// TestDefaultHintSpellingsLeaveSelectionOpen asserts default semantics at the message
// edge: a dispatch whose hints are absent, null or blank — in any mix — reaches
// the polling router in a state the router reads as "the choice is still
// open", and the message carries no model or provider Crier invented.
func TestDefaultHintSpellingsLeaveSelectionOpen(t *testing.T) {
	for _, tc := range cr36DefaultSpellings {
		t.Run(tc.name, func(t *testing.T) {
			h := newCR36Harness(t, nil)
			h.register(t, cr36Router)

			h.deliver(t, cr36Router, dispatchBody(tc.raw, tc.raw, ""), http.StatusCreated)

			got := h.poll(t, cr36Router, 30)
			require.Len(t, got.Messages, 1)
			msg := got.Messages[0]

			for _, key := range []string{"model", "provider"} {
				raw, present := cr36Message(msg, key)
				if tc.want == "" {
					require.False(t, present,
						"%s: a default hint must be visible as the canonical default (the member is absent), got %s", key, raw)
				} else {
					require.True(t, present, "%s: a blank hint is preserved as sent", key)
					require.Equal(t, tc.want, raw)
				}
				require.NotContains(t, []string{"gpt-5.3", "zai-glm-default", "gpt-5.6-luna"},
					strings.Trim(raw, `"`), "%s: Crier must never fill in a concrete model or provider", key)
			}

			// And the same bytes parsed the way a Go consumer parses them.
			var wire struct {
				Model    *SelectionHint `json:"model"`
				Provider *SelectionHint `json:"provider"`
			}
			rawMsg, err := json.Marshal(msg)
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(rawMsg, &wire))
			require.True(t, wire.Model.IsDefault(), "the router must read this as the default arm")
			require.True(t, wire.Provider.IsDefault(), "the router must read this as the default arm")
			require.False(t, wire.Model.Selects())
			require.False(t, wire.Provider.Selects())
		})
	}
}
