package main

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/session"
	"github.com/stretchr/testify/require"
)

// Real production mux, registry verifier and active backend. No verifier stub.
func TestSessionLifecycleProductionRoute(t *testing.T) {
	for _, backend := range []string{"jsonl", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			root := t.TempDir()
			db := filepath.Join(root, "sessions.sqlite")
			base := startTestServerWithEnv(t, map[string]string{"CR_REQUIRE_AGENT_SIG": "true", "CR_SESSION_BACKEND": backend, "CR_SESSION_LOG_ROOT": root, "CR_SESSION_SQLITE_PATH": db, "CR_GROUP_ROOT": t.TempDir()})
			key := registerA2AAgent(t, base, "operator", false, nil)
			client := &http.Client{Timeout: 10 * time.Second}
			var room struct {
				ID string `json:"id"`
			}
			code, raw := sessionDo(t, client, "POST", base+"/sessions", map[string]any{"created_by": map[string]string{"agent": "operator"}}, &room)
			require.Equal(t, 201, code, string(raw))
			endpoint := "/sessions/" + room.ID + "/state"
			code, raw = sessionDo(t, client, "POST", base+endpoint, map[string]string{"state": "closed", "sender": "operator"}, nil)
			require.Equal(t, 401, code, string(raw))
			require.Contains(t, string(raw), "SESSION_SENDER_SIGNATURE_REQUIRED")
			// Incorrect signer ID cannot be promoted to an authenticated actor.
			code, wire := signedAs(t, client, "POST", base, endpoint, "operator", key, []byte(`{"state":"closed","sender":"another"}`))
			require.Equal(t, 403, code, string(wire))
			require.Contains(t, string(wire), "SESSION_SENDER_IDENTITY_MISMATCH")
			code, wire = signedAs(t, client, "POST", base, endpoint, "operator", key, []byte(`{"state":"closed","sender":"operator"}`))
			require.Equal(t, 200, code, string(wire))
			var closed map[string]any
			require.NoError(t, json.Unmarshal(wire, &closed))
			require.Equal(t, "closed", closed["state"])
			require.NotEmpty(t, closed["closed_at"])
			// Open a separate store handle and read the committed row, not the response.
			store, err := session.OpenStore(context.Background(), backend, session.StoreOptions{SQLitePath: db, LogRoot: root})
			require.NoError(t, err)
			t.Cleanup(func() { _ = store.Close() })
			st, err := store.Load(context.Background(), room.ID)
			require.NoError(t, err)
			require.Len(t, st.Lifecycle, 1)
			event := st.Lifecycle[0]
			require.Equal(t, "operator", event.Actor.Agent)
			require.Equal(t, session.SenderIdentityVerified, event.SenderIdentityBasis)
			require.Equal(t, "Session "+room.ID+" marked complete", event.Reason)
			code, wire = signedAs(t, client, "POST", base, endpoint, "operator", key, []byte(`{"state":"open","sender":"operator"}`))
			require.Equal(t, 200, code, string(wire))
			st, err = store.Load(context.Background(), room.ID)
			require.NoError(t, err)
			require.Len(t, st.Lifecycle, 2)
			require.Equal(t, session.SessionOpen, st.Session.State)
			require.Nil(t, st.Session.ClosedAt)
			t.Logf("%s: production route persisted verified close and reopen events through the real registry verifier", backend)
		})
	}
}
