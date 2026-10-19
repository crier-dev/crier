package session

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/mux"
	"github.com/stretchr/testify/require"
)

func TestSenderIdentitySurvivesReopenWithoutRecipients(t *testing.T) {
	for _, backend := range localBackends() {
		t.Run(backend.name, func(t *testing.T) {
			path := backend.pathFn(t)
			repo := backend.open(t, path)
			harness := newAPIHarness(t, repo)
			room := harness.createRoom(t)
			var sent map[string]any
			require.Equal(t, http.StatusCreated, harness.do(t, http.MethodPost, "/sessions/"+room+"/messages", map[string]any{"sender": "atlas", "payload": map[string]any{"text": "note", "sender_identity_basis": "sender-identity-verified"}, "targets": []map[string]string{{"kind": "agent", "id": "atlas"}}}, &sent, nil))
			require.Equal(t, "sender-identity-claimed", sent["sender_identity_basis"], "payload must not upgrade assurance")
			require.Empty(t, sent["outcomes"])
			harness.srv.Close()
			if closer, ok := repo.(interface{ Close() error }); ok {
				require.NoError(t, closer.Close())
			}
			reopened := backend.open(t, path)
			if closer, ok := reopened.(interface{ Close() error }); ok {
				t.Cleanup(func() { _ = closer.Close() })
			}
			state, err := reopened.Load(context.Background(), room)
			require.NoError(t, err)
			require.Len(t, state.Messages, 1)
			encoded, err := json.Marshal(state.Messages[0])
			require.NoError(t, err)
			var stored map[string]any
			require.NoError(t, json.Unmarshal(encoded, &stored))
			require.Equal(t, "sender-identity-claimed", stored["sender_identity_basis"])
		})
	}
}

func TestSignatureEnforcementWithoutVerifierFailsClosed(t *testing.T) {
	repo := newTestSQLiteStore(t)
	harness := newAPIHarness(t, repo)
	room := harness.createRoom(t)
	handler := NewHTTPHandler(repo, HTTPOptions{Deliverer: harness.reg, Agents: harness.reg, RequireAgentSignature: true})
	for _, offered := range []bool{false, true} {
		request := httptest.NewRequest(http.MethodPost, "/sessions/"+room+"/messages", bytes.NewBufferString(`{"sender":"atlas","payload":{"text":"must not record"}}`))
		request = mux.SetURLVars(request, map[string]string{"id": room})
		if offered {
			request.Header.Set("X-Agent-Sig", "unconfigured")
		}
		response := httptest.NewRecorder()
		handler.HandlePostMessage(response, request)
		if offered {
			require.Equal(t, 503, response.Code)
			require.Contains(t, response.Body.String(), "SESSION_SENDER_SIGNATURE_UNAVAILABLE")
		} else {
			require.Equal(t, 401, response.Code)
			require.Contains(t, response.Body.String(), "SESSION_SENDER_SIGNATURE_REQUIRED")
		}
		state, err := repo.Load(context.Background(), room)
		require.NoError(t, err)
		require.Empty(t, state.Messages)
	}
}
