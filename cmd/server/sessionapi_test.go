package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
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

// TestSessionAPICompileRoutesServed is the WIRING test for CR-CHAT-028: the
// compile (merge) and expand routes are registered on the served mux and answer
// the documented shapes. The behavioural battery lives in
// internal/session/compile_test.go; this one exists so a route that is built but
// never registered fails here rather than only in a client.
func TestSessionAPICompileRoutesServed(t *testing.T) {
	base := startTestServerWithEnv(t, map[string]string{
		"CR_SESSION_BACKEND":  "jsonl",
		"CR_SESSION_LOG_ROOT": t.TempDir(),
	})
	client := &http.Client{}

	// A registered agent to tag, so the fan-out has a real target.
	code, raw := sessionDo(t, client, http.MethodPost, base+"/agents", map[string]any{
		"id": "atlas", "public_key": strings.Repeat("ab", 32),
	}, nil)
	require.Equal(t, http.StatusCreated, code, "register: %s", raw)

	newRoom := func(title string) string {
		t.Helper()
		var created struct {
			ID string `json:"id"`
		}
		code, raw := sessionDo(t, client, http.MethodPost, base+"/sessions", map[string]any{
			"title":      title,
			"created_by": map[string]string{"principal": "kara"},
			"members":    []map[string]string{{"member_type": "agent", "member_id": "atlas", "role": "member"}},
		}, &created)
		require.Equal(t, http.StatusCreated, code, "create %s: %s", title, raw)
		require.NotEmpty(t, created.ID)
		return created.ID
	}
	postMsg := func(room, text string) string {
		t.Helper()
		var msg struct {
			ID string `json:"id"`
		}
		code, raw := sessionDo(t, client, http.MethodPost, base+"/sessions/"+room+"/messages", map[string]any{
			"payload": map[string]string{"text": text},
			"sender":  "atlas",
		}, &msg)
		require.Equal(t, http.StatusCreated, code, "post: %s", raw)
		return msg.ID
	}

	roomA := newRoom("compile-a")
	roomB := newRoom("compile-b")
	msgA := postMsg(roomA, "from a")
	msgB := postMsg(roomB, "from b")

	var compiled struct {
		Message struct {
			ID        string `json:"id"`
			ThreadID  string `json:"thread_id"`
			ParentID  string `json:"parent_id"`
			MessageKd string `json:"message_kind"`
		} `json:"message"`
		PartsCount    int      `json:"parts_count"`
		ResolvedCount int      `json:"resolved_count"`
		OmittedCount  int      `json:"omitted_count"`
		Gaps          []string `json:"gaps"`
	}
	code, raw = sessionDo(t, client, http.MethodPost, base+"/sessions/"+roomA+"/compile", map[string]any{
		"sources": []map[string]string{
			{"session_id": roomA, "message_id": msgA},
			{"session_id": roomB, "message_id": msgB},
		},
		"sender":       "atlas",
		"targets":      []map[string]string{{"kind": "agent", "id": "atlas"}},
		"message_kind": "addressed",
	}, &compiled)
	require.Equal(t, http.StatusCreated, code, "compile: %s", raw)
	require.NotEmpty(t, compiled.Message.ID)
	require.Equal(t, compiled.Message.ID, compiled.Message.ThreadID, "the merge is a NEW thread root")
	require.Empty(t, compiled.Message.ParentID)
	require.Equal(t, "addressed", compiled.Message.MessageKd)
	require.Equal(t, 2, compiled.PartsCount)
	require.Equal(t, 2, compiled.ResolvedCount)
	require.Equal(t, 0, compiled.OmittedCount)

	// The expand route resolves each citation back to its original.
	var expanded struct {
		MessageID     string `json:"message_id"`
		Count         int    `json:"count"`
		ResolvedCount int    `json:"resolved_count"`
		Parts         []struct {
			SourceSessionID string `json:"source_session_id"`
			SourceMessageID string `json:"source_message_id"`
			Resolved        bool   `json:"resolved"`
			Content         any    `json:"content"`
		} `json:"parts"`
	}
	code, raw = sessionDo(t, client, http.MethodGet,
		base+"/sessions/"+roomA+"/messages/"+compiled.Message.ID+"/expand", nil, &expanded)
	require.Equal(t, http.StatusOK, code, "expand: %s", raw)
	require.Equal(t, compiled.Message.ID, expanded.MessageID)
	require.Equal(t, 2, expanded.Count)
	require.Equal(t, 2, expanded.ResolvedCount)
	require.Equal(t, roomA, expanded.Parts[0].SourceSessionID)
	require.Equal(t, msgA, expanded.Parts[0].SourceMessageID)
	require.Equal(t, roomB, expanded.Parts[1].SourceSessionID)
	require.Equal(t, msgB, expanded.Parts[1].SourceMessageID)
	require.True(t, expanded.Parts[0].Resolved)
	require.NotNil(t, expanded.Parts[0].Content)

	// An unknown session still answers the documented 404 (the realm wall),
	// so the route is live rather than vacuously registered.
	code, _ = sessionDo(t, client, http.MethodPost, base+"/sessions/nope/compile", map[string]any{
		"sources": []map[string]string{{"message_id": "x"}},
		"sender":  "atlas",
	}, nil)
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
