package mcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/stretchr/testify/require"

	"github.com/crier-dev/crier/internal/guard"
	"github.com/crier-dev/crier/internal/registry"
	"github.com/crier-dev/crier/internal/webhook"
)

// Exercise tools/call, a real guard provider, and an actual webhook receiver.
// Remote mode intentionally has an async agent default: success must still
// confirm delivery and surface a permanent webhook refusal synchronously.
func TestMCPDeliveryChokePoint(t *testing.T) {
	for _, mode := range []string{"local", "remote", "library_default"} {
		for _, outcome := range []string{"allow", "block", "webhook_refusal"} {
			if mode == "library_default" && outcome == "block" {
				continue // bare library construction deliberately has no guard configured
			}
			for _, tool := range []string{"send_message", "deliver_message", "ask_agent"} {
				t.Run(mode+"/"+outcome+"/"+tool, func(t *testing.T) {
					var checks atomic.Int32
					llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						checks.Add(1)
						decision, risk := "allow", "low"
						if outcome == "block" {
							decision, risk = "block", "high"
						}
						verdict, _ := json.Marshal(map[string]any{"decision": decision, "risk_level": risk, "reason": "MCP rule", "matched_patterns": []string{}})
						_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": string(verdict)}}}})
					}))
					t.Cleanup(llm.Close)
					store := registry.NewMemoryStore()
					require.NoError(t, store.Register(&registry.Agent{ID: "alice"}))
					received := make(chan webhook.Envelope, 4)
					sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						var env webhook.Envelope
						if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
							t.Error(err)
							w.WriteHeader(400)
							return
						}
						received <- env
						if outcome == "webhook_refusal" {
							w.WriteHeader(400)
							return
						}
						if tool == "ask_agent" {
							var payload map[string]any
							_ = json.Unmarshal(env.Payload, &payload)
							reply, _ := json.Marshal(map[string]any{"crier_reply_to": payload["crier_correlation_id"], "text": "reply"})
							if err := store.Deliver("alice", &registry.InboxEntry{Payload: reply}); err != nil {
								t.Error(err)
							}
						}
						_, _ = w.Write([]byte(`{"ok":true}`))
					}))
					t.Cleanup(sink.Close)
					policy := guard.Policy{ID: "mcp-rule", FailClosed: true, Providers: []guard.ProviderSpec{{Provider: "custom", Model: "mock", BaseURL: llm.URL, APIKeyRef: "env:MCP_TEST_KEY"}}}
					require.NoError(t, store.Register(&registry.Agent{ID: "target", Guard: &guard.AgentGuardConfig{Policies: []guard.Policy{policy}}, Webhook: &webhook.Config{URL: sink.URL, DeliveryMode: "async"}}))
					h := registry.NewHandler(store)
					h.SetRequireAgentSig(false)
					filter, err := guard.New(guard.Options{LookupEnv: func(string) string { return "test-key" }})
					require.NoError(t, err)
					t.Cleanup(filter.Close)
					h.SetGuardFilter(filter)
					h.SetWebhookDriver(webhook.NewDriver(webhook.NewClient(time.Second, nil), webhook.NewMemoryQueue(), webhook.DriverConfig{}))
					var backend registry.Store = store
					opts := Options{AgentID: "alice", DeliveryHandler: h}
					switch mode {
					case "remote":
						router := mux.NewRouter()
						router.HandleFunc("/agents/{id}/inbox", h.HandleDeliver).Methods("POST")
						router.HandleFunc("/agents/{id}/inbox", h.HandleRetrieve).Methods("GET")
						router.HandleFunc("/agents/{id}/inbox/ack", h.HandleAck).Methods("POST")
						server := httptest.NewServer(router)
						t.Cleanup(server.Close)
						backend = registry.NewRemoteStore(server.URL, "alice", "")
					case "library_default":
						opts.DeliveryHandler = nil
					}
					s := NewWithOptions(backend, opts)
					args := map[string]any{"agent_id": "target", "payload": map[string]string{"text": "hello"}}
					if tool == "ask_agent" {
						args["timeout_s"] = 1
					}
					text, isErr := parseToolResult(t, callTool(t, s, tool, args))
					if mode != "library_default" {
						require.EqualValues(t, 1, checks.Load(), "guard must run exactly once")
					}
					if outcome == "block" {
						require.True(t, isErr, text)
						require.Contains(t, text, "GUARD_BLOCKED")
						require.Empty(t, received, "guard refusal must precede webhook")
					} else {
						require.Len(t, received, 1, "tool must actually reach registered webhook")
						env := <-received
						require.NotEmpty(t, env.Crier.MessageID)
						require.Equal(t, "alice", env.Crier.Sender)
						require.Equal(t, "target", env.Crier.Target)
						require.Equal(t, "blocking", env.Crier.DeliveryMode)
						if mode != "library_default" {
							require.NotNil(t, env.Crier.Guard)
						}
						if outcome == "webhook_refusal" {
							require.True(t, isErr, text)
							require.Contains(t, text, "permanent")
						} else {
							require.False(t, isErr, text)
							var output map[string]any
							require.NoError(t, json.Unmarshal([]byte(text), &output))
							require.Len(t, output, 2, "unchanged MCP success shape")
							require.NotEmpty(t, output["message_id"])
							if tool != "ask_agent" {
								require.Equal(t, "target", output["agent_id"])
								require.Equal(t, env.Crier.MessageID, output["message_id"])
							} else {
								require.NotNil(t, output["response"])
							}
						}
					}
					depth, leased, _, err := store.Stats("target")
					require.NoError(t, err)
					require.Zero(t, depth+leased, "never silently append/fallback to inbox")
				})
			}
		}
	}
}
