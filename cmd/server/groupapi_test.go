package main

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestGroupAPIRoutesServed is the WIRING test for CR-CHAT-013: the four group
// routes are registered on the served mux and answer the documented shapes.
// The behavioural battery lives in internal/session/groups_http_test.go; this
// one exists so a route that is built but never registered fails here rather
// than only in a client.
func TestGroupAPIRoutesServed(t *testing.T) {
	base := startTestServerWithEnv(t, map[string]string{
		"CR_SESSION_LOG_ROOT": t.TempDir(),
		"CR_GROUP_ROOT":       t.TempDir(),
	})
	client := &http.Client{}

	// Create.
	var created struct {
		Name    string   `json:"name"`
		Members []string `json:"members"`
	}
	code, raw := sessionDo(t, client, http.MethodPost, base+"/groups", map[string]any{
		"name":       "infra",
		"created_by": "kara",
		"members":    []string{"atlas", "nimbus"},
	}, &created)
	require.Equal(t, http.StatusCreated, code, "body: %s", raw)
	require.Equal(t, "infra", created.Name)
	require.Equal(t, []string{"atlas", "nimbus"}, created.Members)

	// Edit the roster.
	var edited struct {
		Members []string `json:"members"`
	}
	code, raw = sessionDo(t, client, http.MethodPatch, base+"/groups/infra/members", map[string]any{
		"add":   []string{"orion"},
		"actor": "sam",
	}, &edited)
	require.Equal(t, http.StatusOK, code, "body: %s", raw)
	require.Equal(t, []string{"atlas", "nimbus", "orion"}, edited.Members)

	// Inspect.
	var got struct {
		Name      string   `json:"name"`
		Members   []string `json:"members"`
		CreatedBy string   `json:"created_by"`
		UpdatedBy string   `json:"updated_by"`
	}
	code, raw = sessionDo(t, client, http.MethodGet, base+"/groups/infra", nil, &got)
	require.Equal(t, http.StatusOK, code, "body: %s", raw)
	require.Equal(t, "kara", got.CreatedBy)
	require.Equal(t, "sam", got.UpdatedBy)

	// List.
	var list struct {
		Count int `json:"count"`
	}
	code, _ = sessionDo(t, client, http.MethodGet, base+"/groups", nil, &list)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, 1, list.Count)

	// A duplicate create is a 409; an unknown group is a 404.
	code, _ = sessionDo(t, client, http.MethodPost, base+"/groups", map[string]any{"name": "infra", "created_by": "kara"}, nil)
	require.Equal(t, http.StatusConflict, code)
	code, _ = sessionDo(t, client, http.MethodGet, base+"/groups/nope", nil, nil)
	require.Equal(t, http.StatusNotFound, code)
}
