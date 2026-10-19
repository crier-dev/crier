package session

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSessionUnknownNameListsAccepted(t *testing.T) {
	s, err := NewJSONLStore(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	h := NewHTTPHandler(s, HTTPOptions{})
	r := httptest.NewRequest(http.MethodPost, "/sessions", strings.NewReader(`{"name":"ignored","created_by":{"principal":"operator"}}`))
	w := httptest.NewRecorder()
	h.HandleCreateSession(w, r)
	require.Equal(t, http.StatusBadRequest, w.Code)
	require.Contains(t, w.Body.String(), `unknown field \"name\" (accepted: id, title, kind, namespace, visibility, retention_seconds, created_by, principal_id, as_agent, members)`)
	t.Log(w.Body.String())
}
