package registry

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/crier-dev/crier/internal/webhook"
)

// DF-CRIER-180 — a deliver request must reject the parameters it cannot honor.
//
// POST /agents/{id}/inbox used to accept ANY request-level delivery_mode and
// echo it back in the accept body as if it were the RESOLVED queue semantics
// ({"transport":"webhook","delivery_mode":"bogus"}), while the very same value
// is rejected on the registration/PATCH path by webhook.Config.Validate
// ("webhook.delivery_mode must be blocking|async|batch"). A caller could not
// tell a typo from a real queue mode: the accept claimed a delivery semantics
// the handler never had. timeout_ms had the same gap — 0..120000 is enforced
// on the webhook config, while the request-level value was taken verbatim as
// the blocking budget (so a negative budget silently fell back to the 30s
// default instead of being reported).
//
// The validation runs BEFORE any transport/store choice, so it holds for
// webhook targets, inbox-only targets and unknown targets alike: nothing is
// dispatched, nothing is stored, and the 400 names the allowed set.

// allowedModes is the error text the deliver 400 must carry — the same set
// webhook.Config.Validate enforces at registration time.
const allowedModes = "blocking|async|batch"

// TestHandleDeliver_RejectsUnhonorableRequestParameters is the table-driven
// gate: one named case per row, each with its own target, endpoint and body.
func TestHandleDeliver_RejectsUnhonorableRequestParameters(t *testing.T) {
	tests := []struct {
		name       string
		hasWebhook bool   // target has a webhook endpoint (false = inbox-only agent)
		agentMode  string // the agent's configured webhook delivery_mode
		targetID   string // "" = address the registered agent
		body       string
		wantStatus int
		wantMode   string // expected accept delivery_mode ("" = the key must be ABSENT)
		wantReply  string // expected blocking reply body ("" = unchecked/inapplicable)
		wantErrSub []string
		wantPosts  int32 // endpoint POSTs observed
		wantStored bool  // an entry must be retrievable from the durable inbox
	}{
		{
			name:       "bogus_mode_webhook_target_rejected_without_dispatch",
			hasWebhook: true, agentMode: "async",
			body:       `{"payload":{"ping":1},"delivery_mode":"bogus"}`,
			wantStatus: http.StatusBadRequest,
			wantErrSub: []string{allowedModes},
			wantPosts:  0, wantStored: false,
		},
		{
			name:       "bogus_mode_inbox_only_target_rejected",
			hasWebhook: false,
			body:       `{"payload":{"ping":1},"delivery_mode":"bogus"}`,
			wantStatus: http.StatusBadRequest,
			wantErrSub: []string{allowedModes},
			wantPosts:  0, wantStored: false,
		},
		{
			name:       "bogus_mode_unknown_target_rejected_before_lookup",
			hasWebhook: true, agentMode: "async",
			targetID:   "agent-nowhere",
			body:       `{"payload":{"ping":1},"delivery_mode":"bogus"}`,
			wantStatus: http.StatusBadRequest,
			wantErrSub: []string{allowedModes},
			wantPosts:  0, wantStored: false,
		},
		{
			name:       "blocking_mode_still_returns_the_endpoint_reply",
			hasWebhook: true, agentMode: "async",
			body:       `{"payload":{"ping":1},"delivery_mode":"blocking"}`,
			wantStatus: http.StatusOK,
			wantReply:  `{"pong":true}`,
			wantPosts:  1, wantStored: false,
		},
		{
			name:       "async_mode_still_accepted_and_echoed",
			hasWebhook: true, agentMode: "batch",
			body:       `{"payload":{"ping":1},"delivery_mode":"async"}`,
			wantStatus: http.StatusAccepted,
			wantMode:   "async",
			wantPosts:  1, wantStored: false,
		},
		{
			name:       "batch_mode_still_accepted_and_echoed",
			hasWebhook: true, agentMode: "async",
			body:       `{"payload":{"ping":1},"delivery_mode":"batch"}`,
			wantStatus: http.StatusAccepted,
			wantMode:   "batch",
			wantPosts:  1, wantStored: false,
		},
		{
			name:       "absent_mode_uses_the_agent_webhook_default",
			hasWebhook: true, agentMode: "batch",
			body:       `{"payload":{"ping":1}}`,
			wantStatus: http.StatusAccepted,
			wantMode:   "batch",
			wantPosts:  1, wantStored: false,
		},
		{
			name:       "absent_mode_defaults_to_async_when_the_agent_states_none",
			hasWebhook: true, agentMode: "",
			body:       `{"payload":{"ping":1}}`,
			wantStatus: http.StatusAccepted,
			wantMode:   "async",
			wantPosts:  1, wantStored: false,
		},
		{
			name:       "absent_mode_inbox_only_target_unchanged",
			hasWebhook: false,
			body:       `{"payload":{"ping":1}}`,
			wantStatus: http.StatusCreated,
			wantStored: true,
		},
		{
			name:       "negative_timeout_ms_rejected",
			hasWebhook: true, agentMode: "blocking",
			body:       `{"payload":{"ping":1},"timeout_ms":-1}`,
			wantStatus: http.StatusBadRequest,
			wantErrSub: []string{"timeout_ms", "120000"},
			wantPosts:  0, wantStored: false,
		},
		{
			name:       "timeout_ms_above_the_config_bound_rejected",
			hasWebhook: true, agentMode: "blocking",
			body:       `{"payload":{"ping":1},"timeout_ms":120001}`,
			wantStatus: http.StatusBadRequest,
			wantErrSub: []string{"timeout_ms", "120000"},
			wantPosts:  0, wantStored: false,
		},
		{
			name:       "timeout_ms_at_the_bound_accepted",
			hasWebhook: true, agentMode: "blocking",
			body:       `{"payload":{"ping":1},"delivery_mode":"blocking","timeout_ms":120000}`,
			wantStatus: http.StatusOK,
			wantReply:  `{"pong":true}`,
			wantPosts:  1, wantStored: false,
		},
		{
			name:       "timeout_ms_zero_keeps_the_blocking_default",
			hasWebhook: true, agentMode: "blocking",
			body:       `{"payload":{"ping":1},"delivery_mode":"blocking","timeout_ms":0}`,
			wantStatus: http.StatusOK,
			wantReply:  `{"pong":true}`,
			wantPosts:  1, wantStored: false,
		},
		{
			name:       "timeout_ms_absent_inbox_only_target_unchanged",
			hasWebhook: false,
			body:       `{"payload":{"ping":1}}`,
			wantStatus: http.StatusCreated,
			wantStored: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var posts atomic.Int32
			endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				posts.Add(1)
				w.WriteHeader(http.StatusOK)
				w.Write([]byte(`{"pong":true}`))
			}))
			defer endpoint.Close()

			agentID := "agent-" + strings.ReplaceAll(tc.name, "_", "-")
			var cfg *webhook.Config
			if tc.hasWebhook {
				cfg = &webhook.Config{URL: endpoint.URL, DeliveryMode: tc.agentMode, TimeoutMs: 5000}
				// A batch accept must be able to flush with a single message,
				// or the "the accept is not a lie" POST check would wait out
				// the driver's default coalescing window.
				if tc.agentMode == "batch" || strings.Contains(tc.body, `"batch"`) {
					cfg.Batch = &webhook.BatchConfig{MaxMessages: 1, FlushIntervalS: 0}
				}
			}
			h, store := deliverHarness(t, agentID, cfg)

			targetID := tc.targetID
			if targetID == "" {
				targetID = agentID
			}

			rec, wire := postDeliver(t, h, targetID, tc.body)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d — body: %s", rec.Code, tc.wantStatus, rec.Body.String())
			}

			for _, sub := range tc.wantErrSub {
				if !strings.Contains(rec.Body.String(), sub) {
					t.Errorf("400 body = %s, want it to name %q", rec.Body.String(), sub)
				}
			}

			if tc.wantMode == "" {
				// Assert ABSENCE on the raw body: delivery_mode is omitempty, so
				// decoding cannot tell "absent" from "empty".
				var raw map[string]json.RawMessage
				if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
					t.Fatalf("decode %s: %v", rec.Body.String(), err)
				}
				if _, ok := raw["delivery_mode"]; ok {
					t.Errorf("accept carries delivery_mode=%q, want the key absent — body: %s",
						wire.DeliveryMode, rec.Body.String())
				}
			} else if wire.DeliveryMode != tc.wantMode {
				t.Errorf("delivery_mode = %q, want %q — body: %s",
					wire.DeliveryMode, tc.wantMode, rec.Body.String())
			}

			if tc.wantReply != "" && string(wire.Reply) != tc.wantReply {
				t.Errorf("reply = %s, want the endpoint's body %s", wire.Reply, tc.wantReply)
			}
			if tc.wantStatus == http.StatusGatewayTimeout && len(wire.Reply) != 0 {
				t.Errorf("504 carried reply = %s, want none", wire.Reply)
			}

			if tc.wantPosts > 0 {
				waitForEndpointPosts(t, &posts, tc.wantPosts, tc.name)
			} else {
				// The rejection must happen before dispatch, so no POST can
				// arrive — give the driver's queue worker room to (not) send.
				time.Sleep(300 * time.Millisecond)
				if n := posts.Load(); n != 0 {
					t.Errorf("endpoint received %d POST(s), want 0 — a rejected or "+
						"undispatchable request must never reach the endpoint", n)
				}
			}

			entries, _, err := store.Retrieve(agentID, time.Minute, 10)
			if err != nil {
				t.Fatalf("retrieve: %v", err)
			}
			if gotStored := len(entries) > 0; gotStored != tc.wantStored {
				t.Errorf("inbox retrieve = %d entries, want stored=%v — a rejected request "+
					"must not be silently parked in the durable inbox", len(entries), tc.wantStored)
			}
		})
	}
}

// TestHandleDeliver_BlockingTimeoutBudgetIsHonored pins the DF-CRIER-157
// budget contract that the request-level validation must NOT change: a
// blocking delivery with timeout_ms=500 against an endpoint that sleeps 3s
// returns 504 (a Gateway Timeout — the same delivery may still land later,
// never a PERMANENT rejection) in far less than the endpoint's own latency.
// Without the budget the caller would wait for the endpoint's 3s.
func TestHandleDeliver_BlockingTimeoutBudgetIsHonored(t *testing.T) {
	const (
		endpointSleep = 3 * time.Second
		budget        = 500 * time.Millisecond
	)

	var posts atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		time.Sleep(endpointSleep)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"pong":true}`))
	}))
	defer endpoint.Close()

	h, store := deliverHarness(t, "agent-budget", &webhook.Config{
		URL:          endpoint.URL,
		DeliveryMode: "blocking",
		TimeoutMs:    5000,
	})

	start := time.Now()
	rec, wire := postDeliver(t, h, "agent-budget",
		`{"payload":{"ping":1},"delivery_mode":"blocking","timeout_ms":500}`)
	elapsed := time.Since(start)

	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("blocking budget: status = %d, want 504 — body: %s", rec.Code, rec.Body.String())
	}
	if len(wire.Reply) != 0 {
		t.Errorf("504 carried reply = %s, want none — the endpoint never answered inside the budget", wire.Reply)
	}
	if strings.Contains(rec.Body.String(), "permanent") {
		t.Errorf("504 body = %s, want a NON-permanent failure class (a timeout may still succeed on retry)",
			rec.Body.String())
	}

	// The 504 is real, not a false positive on a dead endpoint: the endpoint
	// was entered at least once.
	deadline := time.Now().Add(time.Second)
	for posts.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if posts.Load() == 0 {
		t.Errorf("endpoint never received the POST — the 504 came from somewhere else")
	}

	// The budget, not the endpoint, decided when to give up. 1500ms leaves
	// headroom for a loaded CI box while staying well below the 3s sleep.
	if elapsed >= 1500*time.Millisecond {
		t.Errorf("blocking budget not honored: returned after %v (budget %v, endpoint sleeps %v)",
			elapsed, budget, endpointSleep)
	}
	t.Logf("blocking budget honored: 504 after %v (budget %v, endpoint sleeps %v)",
		elapsed, budget, endpointSleep)

	entries, _, err := store.Retrieve("agent-budget", time.Minute, 10)
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("timed-out blocking delivery stored %d inbox entr(ies), want none — "+
			"the webhook path never falls back to the inbox", len(entries))
	}
}

// TestHandleDeliver_DocsMatchTheValidatedParameters pins the documented deliver
// contract to the enforced one. The OpenAPI deliver schema is what a client
// codes against, so an enum or range that only lives in the docs is the exact
// failure class this task fixes — a documented parameter the server does not
// honor (and, pre-fix, one it accepted and echoed while never executing it).
func TestHandleDeliver_DocsMatchTheValidatedParameters(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "openapi.yaml"))
	if err != nil {
		t.Fatalf("read docs/openapi.yaml: %v", err)
	}

	type param struct {
		Type    string   `yaml:"type"`
		Enum    []string `yaml:"enum"`
		Minimum *int     `yaml:"minimum"`
		Maximum *int     `yaml:"maximum"`
		Default *int     `yaml:"default"`
	}
	var doc struct {
		Paths map[string]map[string]struct {
			RequestBody struct {
				Content map[string]struct {
					Schema struct {
						Type       string           `yaml:"type"`
						Required   []string         `yaml:"required"`
						Properties map[string]param `yaml:"properties"`
					} `yaml:"schema"`
				} `yaml:"content"`
			} `yaml:"requestBody"`
			Responses map[string]struct {
				Description string `yaml:"description"`
			} `yaml:"responses"`
		} `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("docs/openapi.yaml is not valid YAML: %v", err)
	}

	post, ok := doc.Paths["/agents/{id}/inbox"]["post"]
	if !ok {
		t.Fatal("docs/openapi.yaml has no post operation for /agents/{id}/inbox")
	}
	schema := post.RequestBody.Content["application/json"].Schema

	dm, ok := schema.Properties["delivery_mode"]
	if !ok {
		t.Fatal("deliver request schema documents no delivery_mode")
	}
	require.Equal(t, deliverModes, dm.Enum,
		"the documented delivery_mode enum must be the set the handler enforces")

	tm, ok := schema.Properties["timeout_ms"]
	if !ok {
		t.Fatal("deliver request schema documents no timeout_ms")
	}
	require.NotNil(t, tm.Minimum, "timeout_ms must document its lower bound")
	require.NotNil(t, tm.Maximum, "timeout_ms must document its upper bound")
	require.NotNil(t, tm.Default, "timeout_ms must document its default")
	require.Equal(t, 0, *tm.Minimum)
	require.Equal(t, maxDeliverTimeoutMs, *tm.Maximum,
		"the documented timeout_ms ceiling must be the bound the handler enforces")
	require.Equal(t, defaultDeliverTimeoutMs, *tm.Default,
		"the documented timeout_ms default must be the blocking budget the handler applies")

	// Every documented mode must be accepted, and every accepted mode must be
	// documented — in both directions, so neither side can drift alone.
	for _, mode := range dm.Enum {
		require.NoError(t, validateDeliverParameters(&deliverRequest{DeliveryMode: mode}),
			"documented delivery_mode %q is rejected by the handler", mode)
	}
	for _, mode := range []string{"bogus", "BLOCKING", "sync", "blocking "} {
		require.Error(t, validateDeliverParameters(&deliverRequest{DeliveryMode: mode}),
			"undocumented delivery_mode %q is accepted by the handler", mode)
	}

	// The REQUIRED payload is the other half of the same contract
	// (DF-CRIER-112): the schema has always declared `required: [payload]`
	// with `payload: type: object`, so the handler must enforce exactly that
	// — and the docs must keep saying it, or the next reader of the schema
	// cannot tell what the boundary accepts.
	pl, ok := schema.Properties["payload"]
	if !ok {
		t.Fatal("deliver request schema documents no payload")
	}
	require.Equal(t, "object", pl.Type,
		"the documented payload type must be the JSON type the handler enforces")
	require.Contains(t, schema.Required, "payload",
		"the documented request schema must mark payload required — the handler refuses the key when it is absent")

	require.NoError(t, validateDeliverPayload(&deliverRequest{Payload: json.RawMessage(`{"x":1}`)}),
		"a documented JSON-object payload is rejected by the handler")
	require.NoError(t, validateDeliverPayload(&deliverRequest{Payload: json.RawMessage(`{}`)}),
		"an empty JSON object is still an object — the contract refuses a MISSING payload, not an empty one")
	require.EqualError(t, validateDeliverPayload(&deliverRequest{}), deliverPayloadRequiredError,
		"the handler must refuse a payload-less request with the documented text")
	require.EqualError(t, validateDeliverPayload(&deliverRequest{Payload: json.RawMessage(`null`)}), deliverPayloadObjectError,
		"the handler must refuse an explicit null payload with the documented text")

	// The 400 the handler returns must be documented for this operation.
	bad, ok := post.Responses["400"]
	if !ok {
		t.Fatal("the deliver operation documents no 400 response")
	}
	for _, want := range []string{
		"delivery_mode", "timeout_ms", "blocking|async|batch",
		deliverPayloadRequiredError, deliverPayloadObjectError,
	} {
		require.Contains(t, bad.Description, want,
			"the documented 400 must explain the %q rejection", want)
	}
}

// DF-CRIER-112 — the deliver API must refuse a request its own published
// contract calls invalid.
//
// POST /agents/{id}/inbox used to accept a body that omits `payload` (e.g.
// `{}`, or the mistyped `{"payloads":{"x":1}}`) with 201, storing an empty
// message, even though docs/openapi.yaml declares the deliver requestBody
// `required: [payload]` with `payload: type: object`. The client got a
// success it could not act on: nothing useful was ever stored, and the typo
// was indistinguishable from a message that genuinely had no content.
//
// The refusal happens at the HTTP boundary, BEFORE any transport/store
// choice — the same call site and the same reasoning as DF-CRIER-180 — so the
// answer is identical for a webhook target (nothing dispatched), an
// inbox-only target (nothing stored) and an unregistered target.
func TestHandleDeliver_RejectsMissingOrNonObjectPayload(t *testing.T) {
	// The refused inputs: one named case per shape. `{}` is the reproduced
	// defect; the `payloads` row is the client-bug shape the contract exists
	// to catch (a typo'd key must not read as a content-less success).
	refusals := []struct {
		name    string
		body    string
		wantErr string
	}{
		{"key_absent", `{}`, deliverPayloadRequiredError},
		{"key_absent_typo_payloads", `{"payloads":{"x":1}}`, deliverPayloadRequiredError},
		{"key_present_explicit_null", `{"payload":null}`, deliverPayloadObjectError},
		{"value_is_a_string", `{"payload":"x"}`, deliverPayloadObjectError},
		{"value_is_an_array", `{"payload":[{"x":1}]}`, deliverPayloadObjectError},
		{"value_is_a_number", `{"payload":5}`, deliverPayloadObjectError},
		{"value_is_a_bool", `{"payload":true}`, deliverPayloadObjectError},
	}

	// The three target kinds the deliver path can be asked to serve.
	targets := []struct {
		name       string
		hasWebhook bool
		agentMode  string
		targetID   string // "" = the registered agent itself
	}{
		{"webhook_target", true, "async", ""},
		{"inbox_only_target", false, "", ""},
		{"unregistered_target", true, "async", "agent-nowhere"},
	}

	for _, tc := range refusals {
		for _, tg := range targets {
			t.Run(tc.name+"/"+tg.name, func(t *testing.T) {
				var posts atomic.Int32
				endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					posts.Add(1)
					w.WriteHeader(http.StatusOK)
					w.Write([]byte(`{"pong":true}`))
				}))
				defer endpoint.Close()

				agentID := "agent-" + strings.ReplaceAll(tc.name, "_", "-")
				var cfg *webhook.Config
				if tg.hasWebhook {
					cfg = &webhook.Config{URL: endpoint.URL, DeliveryMode: tg.agentMode, TimeoutMs: 5000}
				}
				h, store := deliverHarness(t, agentID, cfg)

				targetID := tg.targetID
				if targetID == "" {
					targetID = agentID
				}

				rec, _ := postDeliver(t, h, targetID, tc.body)

				if rec.Code != http.StatusBadRequest {
					t.Fatalf("status = %d, want 400 — body: %s", rec.Code, rec.Body.String())
				}
				// The error must name the field AND the reason, so a client
				// can fix its body instead of guessing.
				for _, want := range []string{"payload", tc.wantErr} {
					if !strings.Contains(rec.Body.String(), want) {
						t.Errorf("400 body = %s, want it to name %q", rec.Body.String(), want)
					}
				}

				// Nothing was dispatched: give the driver's queue worker room
				// to (not) send before concluding.
				time.Sleep(300 * time.Millisecond)
				if n := posts.Load(); n != 0 {
					t.Errorf("endpoint received %d POST(s), want 0 — a refused request must never reach the endpoint", n)
				}

				// Nothing was stored: the registered agent's own inbox stays
				// empty, and an unknown target has no inbox to store into at
				// all (its retrieve is "agent not found", not an entry).
				for _, id := range []string{targetID, agentID} {
					entries, _, err := store.Retrieve(id, time.Minute, 10)
					if errors.Is(err, ErrAgentNotFound) {
						continue // no inbox exists — nothing can have been stored
					}
					if err != nil {
						t.Fatalf("retrieve %s: %v", id, err)
					}
					if len(entries) != 0 {
						t.Errorf("inbox %s = %d entr(ies), want an EMPTY message list — a refused "+
							"request must not be silently parked in the durable inbox", id, len(entries))
					}
				}
			})
		}
	}
}

// TestHandleDeliver_ValidObjectPayloadUnchanged is the no-regression control
// for DF-CRIER-112: the refusal must be about the MISSING/INVALID required
// key, never about valid senders. A JSON object payload — including the empty
// object `{}` — still delivers on every transport, and the stored bytes are
// the sender's bytes.
func TestHandleDeliver_ValidObjectPayloadUnchanged(t *testing.T) {
	const valid = `{"payload":{"x":1}}`

	t.Run("inbox_only_stores_the_payload_intact", func(t *testing.T) {
		h, store := deliverHarness(t, "agent-valid-inbox", nil)

		rec, wire := postDeliver(t, h, "agent-valid-inbox", valid)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201 — body: %s", rec.Code, rec.Body.String())
		}
		if wire.Transport != "inbox" {
			t.Errorf("transport = %q, want inbox — body: %s", wire.Transport, rec.Body.String())
		}

		entries, _, err := store.Retrieve("agent-valid-inbox", time.Minute, 10)
		if err != nil {
			t.Fatalf("retrieve: %v", err)
		}
		if len(entries) != 1 {
			t.Fatalf("inbox = %d entr(ies), want 1", len(entries))
		}
		if got := string(entries[0].Payload); got != `{"x":1}` {
			t.Errorf("stored payload = %s, want the sender's bytes {\"x\":1}", got)
		}
	})

	t.Run("empty_object_is_still_an_object", func(t *testing.T) {
		h, store := deliverHarness(t, "agent-valid-empty", nil)

		rec, _ := postDeliver(t, h, "agent-valid-empty", `{"payload":{}}`)
		if rec.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201 — the contract refuses a MISSING payload, not an empty one — body: %s",
				rec.Code, rec.Body.String())
		}
		entries, _, err := store.Retrieve("agent-valid-empty", time.Minute, 10)
		if err != nil {
			t.Fatalf("retrieve: %v", err)
		}
		if len(entries) != 1 || string(entries[0].Payload) != `{}` {
			t.Fatalf("inbox = %d entr(ies) (first payload %q), want exactly one entry carrying {}",
				len(entries), firstPayload(entries))
		}
	})

	t.Run("webhook_async_still_accepted", func(t *testing.T) {
		var posts atomic.Int32
		endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			posts.Add(1)
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"pong":true}`))
		}))
		defer endpoint.Close()

		h, _ := deliverHarness(t, "agent-valid-webhook", &webhook.Config{
			URL: endpoint.URL, DeliveryMode: "async", TimeoutMs: 5000,
		})

		rec, wire := postDeliver(t, h, "agent-valid-webhook", valid)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202 — body: %s", rec.Code, rec.Body.String())
		}
		if wire.Transport != "webhook" || wire.DeliveryMode != "async" {
			t.Errorf("transport/mode = %q/%q, want webhook/async — body: %s",
				wire.Transport, wire.DeliveryMode, rec.Body.String())
		}
		waitForEndpointPosts(t, &posts, 1, "valid payload async")
	})

	t.Run("blocking_still_returns_the_reply", func(t *testing.T) {
		endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"pong":true}`))
		}))
		defer endpoint.Close()

		h, _ := deliverHarness(t, "agent-valid-blocking", &webhook.Config{
			URL: endpoint.URL, DeliveryMode: "blocking", TimeoutMs: 5000,
		})

		rec, wire := postDeliver(t, h, "agent-valid-blocking", valid)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 — body: %s", rec.Code, rec.Body.String())
		}
		if string(wire.Reply) != `{"pong":true}` {
			t.Errorf("reply = %s, want the endpoint's body {\"pong\":true}", wire.Reply)
		}
	})
}

// firstPayload reports the first entry's payload for failure messages.
func firstPayload(entries []*InboxEntry) string {
	if len(entries) == 0 {
		return ""
	}
	return string(entries[0].Payload)
}
