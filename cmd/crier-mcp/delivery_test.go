package main

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/crier-dev/crier/config"
	"github.com/crier-dev/crier/internal/registry"
	"github.com/crier-dev/crier/internal/webhook"
)

func TestLocalMCPBootUsesConfiguredGuard(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(strconv.FormatBool(enabled), func(t *testing.T) {
			var checks atomic.Int32
			llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				checks.Add(1)
				_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": `{"decision":"block","risk_level":"high","reason":"boot policy","matched_patterns":[]}`}}}})
			}))
			t.Cleanup(llm.Close)
			policy, err := json.Marshal(map[string]any{"id": "boot-policy", "fail_closed": true, "providers": []any{map[string]string{"provider": "custom", "model": "mock", "base_url": llm.URL, "api_key_ref": "env:BOOT_MCP_TEST_KEY"}}})
			require.NoError(t, err)
			for _, key := range []string{"CR_DATABASE_URL", "DATABASE_URL", "CRIER_DATABASE_URL", "CRIER_HTTP_URL", "CRIER_MESH_URL", "CR_AUTH_TOKEN", "CR_NAMESPACES", "CR_NAMESPACES_FILE", "CR_GUARD_KANBAN_URL"} {
				t.Setenv(key, "")
			}
			t.Setenv("CR_REQUIRE_AGENT_SIG", "false")
			t.Setenv("CR_GUARD_ENABLED", strconv.FormatBool(enabled))
			t.Setenv("CR_GUARD_DEFAULT_POLICY", string(policy))
			t.Setenv("BOOT_MCP_TEST_KEY", "test-key")
			root := t.TempDir()
			input := filepath.Join(root, "input")
			require.NoError(t, os.WriteFile(input, []byte(strings.Join([]string{
				`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"register_agent","arguments":{"id":"target"}}}`,
				`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"send_message","arguments":{"agent_id":"target","payload":{"text":"hello"}}}}`, "",
			}, "\n")), 0600))
			stdin, err := os.Open(input)
			require.NoError(t, err)
			defer func() { _ = stdin.Close() }()
			stdout, err := os.Create(filepath.Join(root, "output"))
			require.NoError(t, err)
			defer func() { _ = stdout.Close() }()
			previousIn, previousOut := os.Stdin, os.Stdout
			os.Stdin, os.Stdout = stdin, stdout
			defer func() { os.Stdin, os.Stdout = previousIn, previousOut }()
			require.Zero(t, run(nil))
			_, err = stdout.Seek(0, 0)
			require.NoError(t, err)
			scanner := bufio.NewScanner(stdout)
			var responses []struct {
				ID     int `json:"id"`
				Result struct {
					IsError bool `json:"isError"`
					Content []struct {
						Text string `json:"text"`
					} `json:"content"`
				} `json:"result"`
			}
			for scanner.Scan() {
				var response struct {
					ID     int `json:"id"`
					Result struct {
						IsError bool `json:"isError"`
						Content []struct {
							Text string `json:"text"`
						} `json:"content"`
					} `json:"result"`
				}
				require.NoError(t, json.Unmarshal(scanner.Bytes(), &response))
				responses = append(responses, response)
			}
			require.NoError(t, scanner.Err())
			require.Len(t, responses, 2)
			require.False(t, responses[0].Result.IsError, "registration must succeed")
			require.Equal(t, 2, responses[1].ID)
			require.Equal(t, enabled, responses[1].Result.IsError)
			if enabled {
				require.EqualValues(t, 1, checks.Load())
				require.Contains(t, responses[1].Result.Content[0].Text, "GUARD_BLOCKED")
			} else {
				require.Zero(t, checks.Load())
			}
		})
	}
}

func TestLocalMCPDeliveryConfiguration(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "guard_off", true: "guard_on"}[enabled], func(t *testing.T) {
			var checks, hooks atomic.Int32
			llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				checks.Add(1)
				_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": `{"decision":"block","risk_level":"high","reason":"local MCP policy","matched_patterns":[]}`}}}})
			}))
			t.Cleanup(llm.Close)
			sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hooks.Add(1)
				_, _ = w.Write([]byte(`{"ok":true}`))
			}))
			t.Cleanup(sink.Close)
			t.Setenv("LOCAL_MCP_TEST_KEY", "test-key")
			policy, err := json.Marshal(map[string]any{"id": "local-policy", "fail_closed": true, "providers": []any{map[string]string{"provider": "custom", "model": "mock", "base_url": llm.URL, "api_key_ref": "env:LOCAL_MCP_TEST_KEY"}}})
			require.NoError(t, err)
			cfg := config.Config{Guard: config.GuardConfig{Enabled: enabled, DefaultPolicy: string(policy)}, Webhook: config.WebhookConfig{Timeout: time.Second}}
			cfg.Namespaces.Inline = `{"namespaces":[{"name":"team"}]}`
			store := registry.NewMemoryStore()
			require.NoError(t, store.Register(&registry.Agent{ID: "target", Namespace: "team"}))
			h, closeDelivery, err := localDeliveryHandler(cfg, store)
			require.NoError(t, err)
			t.Cleanup(closeDelivery)
			entry := &registry.InboxEntry{Payload: []byte(`{"text":"hello"}`)}
			err = h.Deliver("target", entry)
			if enabled {
				require.ErrorContains(t, err, "GUARD_BLOCKED")
				require.EqualValues(t, 1, checks.Load())
			} else {
				require.NoError(t, err)
				require.Equal(t, "team", entry.Namespace)
				require.Zero(t, checks.Load())
			}
			// Independently prove this boot helper wires a webhook driver; a
			// guard bypass in the namespace is the configured policy, not a
			// second transport. No live provider or real credential is used.
			require.NoError(t, store.Register(&registry.Agent{ID: "hook", Webhook: &webhook.Config{URL: sink.URL}}))
			cfg.Guard.Enabled = false
			push, cleanup, err := localDeliveryHandler(cfg, store)
			require.NoError(t, err)
			t.Cleanup(cleanup)
			require.NoError(t, push.Deliver("hook", &registry.InboxEntry{Payload: []byte(`{"text":"hello"}`)}))
			require.EqualValues(t, 1, hooks.Load())
		})
	}
}

func TestLocalMCPDeliveryRejectsBrokenConfiguration(t *testing.T) {
	for _, cfg := range []config.Config{
		{Namespaces: config.NamespaceConfig{Inline: "broken"}},
		{Guard: config.GuardConfig{Enabled: true, DefaultPolicy: "broken"}},
		{Guard: config.GuardConfig{Enabled: true, KanbanURL: "not-a-url"}},
	} {
		h, cleanup, err := localDeliveryHandler(cfg, registry.NewMemoryStore())
		require.Error(t, err)
		require.Nil(t, h)
		require.Nil(t, cleanup)
	}
}
