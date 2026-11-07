package session

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSessionUnknownNameListsAccepted(t *testing.T) {
	t.Parallel()
	s, err := NewJSONLStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	h := NewHTTPHandler(s, HTTPOptions{})
	r := httptest.NewRequest(http.MethodPost, "/sessions", strings.NewReader(`{"name":"ignored","created_by":{"principal":"operator"}}`))
	w := httptest.NewRecorder()
	h.HandleCreateSession(w, r)
	require.Equal(t, http.StatusBadRequest, w.Code)
	// DF-CRIER-500 added `harness`: a create may bind the new session to a
	// harness adapter (CRIER-HARNESS §4.3), so the accepted vocabulary is one
	// field wider than it was and the refusal names it.
	require.Contains(t, w.Body.String(), `unknown field \"name\" (accepted: id, title, kind, namespace, visibility, retention_seconds, created_by, principal_id, as_agent, members, harness)`)
	t.Log(w.Body.String())
}
