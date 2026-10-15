package mcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/stretchr/testify/require"

	"github.com/crier-dev/crier/internal/registry"
)

// Exercise HTTP delivery serialization and MCP result serialization, not a
// MessageView constructor. get_messages has two paths (fresh and buffered);
// retrieve_inbox is the lease-aware surface a work-performing router must use.
func TestDispatchHintsOnMCPPollSurfaces(t *testing.T) {
	for _, surface := range []string{"retrieve_inbox", "get_messages", "buffered_get_messages"} {
		t.Run(surface, func(t *testing.T) {
			store := registry.NewMemoryStore()
			h := registry.NewHandler(store)
			r := mux.NewRouter()
			r.HandleFunc("/agents/{id}/inbox", h.HandleDeliver).Methods(http.MethodPost)
			srv := httptest.NewServer(r)
			t.Cleanup(srv.Close)
			s := NewWithOptions(store, Options{AgentID: "router"})
			registerAgentIn(t, s, "router")
			for _, body := range []string{
				`{"payload":{"prompt":"dispatch"},"model":" Model/Case ","provider":"provider-X","reply_to":"original-crier-id"}`,
				`{"payload":{"prompt":"default"},"model":null,"provider":"  "}`,
				`{"payload":{"prompt":"legacy"}}`,
			} {
				resp, err := srv.Client().Post(srv.URL+"/agents/router/inbox", "application/json", strings.NewReader(body))
				require.NoError(t, err)
				require.Equal(t, http.StatusCreated, resp.StatusCode)
				require.NoError(t, resp.Body.Close())
			}
			tool := surface
			args := map[string]any{}
			if surface == "retrieve_inbox" {
				args["agent_id"] = "router"
			}
			if surface == "buffered_get_messages" {
				entries, lease, err := store.Retrieve("router", time.Minute, 10)
				require.NoError(t, err)
				for _, entry := range entries {
					require.NoError(t, store.Ack("router", lease, []string{entry.ID}))
				}
				// This is the queue populated by ask_agent's unrelated-message path.
				s.buffered = entries
				tool = "get_messages"
			}
			text, isErr := parseToolResult(t, callTool(t, s, tool, args))
			require.False(t, isErr, text)
			var out struct {
				Messages []map[string]json.RawMessage `json:"messages"`
			}
			require.NoError(t, json.Unmarshal([]byte(text), &out))
			require.Len(t, out.Messages, 3)
			require.JSONEq(t, `" Model/Case "`, string(out.Messages[0]["model"]))
			require.JSONEq(t, `"provider-X"`, string(out.Messages[0]["provider"]))
			require.JSONEq(t, `"original-crier-id"`, string(out.Messages[0]["reply_to"]))
			require.NotContains(t, out.Messages[1], "model")
			require.JSONEq(t, `"  "`, string(out.Messages[1]["provider"]))
			for _, key := range []string{"model", "provider", "reply_to"} {
				require.NotContains(t, out.Messages[2], key, "legacy entries gain no fields")
			}
		})
	}
}
