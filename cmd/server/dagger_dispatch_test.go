package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/crier-dev/crier/internal/daggerctl"
	"github.com/crier-dev/crier/internal/webhook"
)

// Real server composition: both terminal notification and wait-request wiring
// must use its already-configured dispatcher, including an enabled guard.
func TestDaggerDeliveryChokePoint(t *testing.T) {
	for _, verdict := range []string{"allow", "block"} {
		t.Run(verdict, func(t *testing.T) {
			var checks atomic.Int32
			llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				checks.Add(1)
				risk := "low"
				if verdict == "block" {
					risk = "high"
				}
				content, _ := json.Marshal(map[string]any{"decision": verdict, "risk_level": risk, "reason": "dagger rule", "matched_patterns": []string{}})
				_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": string(content)}}}})
			}))
			t.Cleanup(llm.Close)
			received := make(chan webhook.Envelope, 4)
			sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var env webhook.Envelope
				if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return
				}
				received <- env
				_, _ = w.Write([]byte(`{"ok":true}`))
			}))
			t.Cleanup(sink.Close)
			stub := daggerStub(t, "guarded-run", func() string { return "succeeded" })
			base := startTestServerWithEnv(t, map[string]string{
				"CR_REQUIRE_AGENT_SIG": "false", "CR_GUARD_ENABLED": "true", "DAGGER_DISPATCH_TEST_KEY": "test-key",
				"CR_DAGGER_URL": stub.URL, "CR_DAGGER_STORE_DIR": t.TempDir(), "CR_DAGGER_POLL_S": "0",
			})
			code, raw := sessionDo(t, http.DefaultClient, "POST", base+"/agents", map[string]any{
				"id": "dagger-target", "webhook": map[string]string{"url": sink.URL, "delivery_mode": "async"},
				"guard": map[string]any{"policies": []any{map[string]any{"id": "dagger-rule", "fail_closed": true,
					"providers": []any{map[string]string{"provider": "custom", "model": "mock", "base_url": llm.URL, "api_key_ref": "env:DAGGER_DISPATCH_TEST_KEY"}}}}},
			}, nil)
			require.Equal(t, http.StatusCreated, code, "%s", raw)
			code, body := postJSON(t, http.DefaultClient, base+"/dagger/runs", `{"agent_id":"dagger-target","prompt":"run"}`)
			require.Equal(t, http.StatusCreated, code, "%s", body)
			record := getRun(t, http.DefaultClient, base, "guarded-run")
			require.EqualValues(t, 1, checks.Load(), "terminal notification must pass the live guard")
			if verdict == "block" {
				require.False(t, record.Notified)
				require.Contains(t, record.NotifyError, "GUARD_BLOCKED")
				require.Empty(t, received)
			} else {
				require.True(t, record.Notified)
				require.Empty(t, record.NotifyError)
				require.Len(t, received, 1)
				env := <-received
				require.NotEmpty(t, env.Crier.MessageID)
				require.Equal(t, "blocking", env.Crier.DeliveryMode)
				require.Equal(t, "dagger-target", env.Crier.Target)
				require.NotNil(t, env.Crier.Guard)
				var note daggerctl.RunNotification
				require.NoError(t, json.Unmarshal(env.Payload, &note))
				require.Equal(t, daggerctl.CodeRunSucceeded, note.Code)
			}
			// The wait itself must not resolve from a webhook response; its
			// acknowledgement/explicit-resolution protocol remains unchanged.
			before := checks.Load()
			_, body = postJSON(t, http.DefaultClient, base+"/dagger/wait", `{"agent_id":"dagger-target","payload":{"text":"work"},"idempotency_key":"guarded-wait","timeout_seconds":1}`)
			// One second is the existing minimum wait budget.
			require.EqualValues(t, before+1, checks.Load(), "wait delivery must pass the same guard")
			if verdict == "block" {
				require.Contains(t, string(body), "GUARD_BLOCKED")
				require.Empty(t, received)
			} else {
				require.Contains(t, string(body), "DELIVERY_WAIT_TIMEOUT")
				require.Len(t, received, 1)
			}
			require.Empty(t, inboxMessages(t, http.DefaultClient, base, "dagger-target"), "no direct-store bypass or webhook fallback")
		})
	}
}
