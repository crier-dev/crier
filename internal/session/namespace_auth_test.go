package session

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/gorilla/mux"
	"github.com/stretchr/testify/require"

	"github.com/crier-dev/crier/internal/namespace"
	"github.com/crier-dev/crier/internal/registry"
)

func TestSessionAPI_TargetNamespaceAuth(t *testing.T) {
	for _, tc := range []struct {
		name  string
		token string
		want  Outcome
	}{
		{name: "missing_token", want: OutcomeRefused},
		{name: "wrong_token", token: "wrong", want: OutcomeRefused},
		{name: "correct_token", token: "realm-secret", want: OutcomeDelivered},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policies, err := namespace.Parse(`{"namespaces":[{"name":"private","auth":"token","token_ref":"env:TARGET_REALM_TOKEN"}]}`, func(key string) (string, bool) {
				return "realm-secret", key == "TARGET_REALM_TOKEN"
			})
			require.NoError(t, err)
			reg := registry.NewMemoryStore()
			require.NoError(t, reg.Register(&registry.Agent{ID: "protected", Namespace: "private"}))
			require.NoError(t, reg.Register(&registry.Agent{ID: "open"}))
			dispatcher := registry.NewHandler(reg)
			dispatcher.SetNamespacePolicies(policies)
			path := filepath.Join(t.TempDir(), "sessions")
			store, err := NewJSONLStore(path)
			require.NoError(t, err)
			t.Cleanup(func() { _ = store.Close() })

			// Deliberately omit the optional session-side Agents lookup: the
			// target dispatcher must enforce its own authoritative realm policy,
			// independently of session admission or a cached audience lookup.
			handler := NewHTTPHandler(store, HTTPOptions{Deliverer: registry.NewSessionDeliverer(dispatcher)})
			router := mux.NewRouter()
			registerSessionRoutes(router, handler)
			srv := httptest.NewServer(router)
			t.Cleanup(srv.Close)
			h := &apiHarness{srv: srv, reg: reg}
			var room sessionView
			require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions", createSessionRequest{
				Title: "namespace auth", CreatedBy: &AuthorRef{Principal: "operator"},
			}, &room, nil))
			headers := map[string]string{}
			if tc.token != "" {
				headers[namespace.HeaderNamespaceToken] = tc.token
			}
			var sent transcriptMessage
			require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions/"+room.ID+"/messages", postMessageRequest{
				Payload: json.RawMessage(`{"text":"namespace-gated send"}`), PrincipalID: "operator",
				Targets: []AudienceTarget{{Kind: TargetAgent, ID: "protected"}, {Kind: TargetAgent, ID: "open"}},
			}, &sent, headers), "per-target auth refusal must not become a whole-send 502")
			require.Len(t, sent.Outcomes, 2)
			outcomes := make(map[string]outcomeView)
			for _, outcome := range sent.Outcomes {
				outcomes[outcome.Target] = outcome
			}
			require.Equal(t, string(tc.want), outcomes["protected"].Outcome)
			require.Equal(t, string(OutcomeDelivered), outcomes["open"].Outcome, "default namespace needs no token")
			if tc.want == OutcomeRefused {
				require.Contains(t, outcomes["protected"].Detail, "NAMESPACE_UNAUTHORIZED")
				require.Contains(t, outcomes["protected"].Detail, `namespace \"private\"`)
				require.NotContains(t, outcomes["protected"].Detail, "NAMESPACE_MISMATCH")
				require.Empty(t, outcomes["protected"].InboxEntryID)
			}
			for _, target := range []string{"protected", "open"} {
				entries, _, err := reg.Retrieve(target, 0, 10)
				require.NoError(t, err)
				if target == "protected" && tc.want == OutcomeRefused {
					require.Empty(t, entries, "refused target inbox must be untouched")
					continue
				}
				require.Len(t, entries, 1)
				require.Equal(t, InboxEntryID(sent.ID, target), entries[0].ID)
				require.JSONEq(t, `{"text":"namespace-gated send"}`, string(entries[0].Payload))
			}
			// Reopen the actual transcript log, not just the response projection.
			reopened, err := NewJSONLStore(path)
			require.NoError(t, err)
			t.Cleanup(func() { _ = reopened.Close() })
			state, err := reopened.Load(context.Background(), room.ID)
			require.NoError(t, err)
			durable := state.Message(sent.ID)
			require.NotNil(t, durable)
			require.Len(t, durable.Outcomes, len(sent.Outcomes))
			for _, outcome := range durable.Outcomes {
				require.Equal(t, outcomes[outcome.Target].Outcome, string(outcome.Outcome))
				require.Equal(t, outcomes[outcome.Target].InboxEntryID, outcome.InboxEntryID)
			}
		})
	}
}
