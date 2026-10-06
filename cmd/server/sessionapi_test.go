package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSessionAPIRoutesServed is the WIRING test for CR-CHAT-019: the six
// session routes are registered on the served mux and answer the documented
// shapes. The behavioural battery lives in internal/session/http_test.go; this
// one exists so a route that is built but never registered fails here rather
// than only in a client.
func TestSessionAPIRoutesServed(t *testing.T) {
	base := startTestServerWithEnv(t, map[string]string{
		"CR_SESSION_BACKEND":  "jsonl",
		"CR_SESSION_LOG_ROOT": t.TempDir(),
	})
	client := &http.Client{}

	var created struct {
		ID string `json:"id"`
	}
	code, raw := sessionDo(t, client, http.MethodPost, base+"/sessions", map[string]any{
		"title":      "wiring",
		"created_by": map[string]string{"principal": "kara"},
		"members": []map[string]string{
			{"member_type": "agent", "member_id": "atlas", "role": "member"},
		},
	}, &created)
	require.Equal(t, http.StatusCreated, code, "body: %s", raw)
	require.NotEmpty(t, created.ID)

	var list struct {
		Count    int `json:"count"`
		Sessions []struct {
			ID string `json:"id"`
		} `json:"sessions"`
	}
	code, raw = sessionDo(t, client, http.MethodGet, base+"/sessions", nil, &list)
	require.Equal(t, http.StatusOK, code, "body: %s", raw)
	require.Equal(t, 1, list.Count)
	require.Equal(t, created.ID, list.Sessions[0].ID)

	var participants struct {
		Count int `json:"count"`
	}
	code, raw = sessionDo(t, client, http.MethodGet, base+"/sessions/"+created.ID+"/participants", nil, &participants)
	require.Equal(t, http.StatusOK, code, "body: %s", raw)
	require.Equal(t, 1, participants.Count)

	var transcript struct {
		Count int `json:"count"`
	}
	code, raw = sessionDo(t, client, http.MethodGet, base+"/sessions/"+created.ID+"/messages", nil, &transcript)
	require.Equal(t, http.StatusOK, code, "body: %s", raw)
	require.Equal(t, 0, transcript.Count)

	// Unknown session is a 404, not a silent empty transcript.
	code, _ = sessionDo(t, client, http.MethodGet, base+"/sessions/nope/messages", nil, nil)
	require.Equal(t, http.StatusNotFound, code)
}

func sessionDo(t *testing.T, c *http.Client, method, url string, body any, out any) (int, string) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, url, reader)
	require.NoError(t, err)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	if out != nil && len(raw) > 0 {
		require.NoError(t, json.Unmarshal(raw, out), "decode: %s", string(raw))
	}
	return resp.StatusCode, string(raw)
}
