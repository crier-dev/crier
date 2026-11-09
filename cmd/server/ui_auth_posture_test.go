package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestUIAuthPostureAgreementAndUnauthenticatedTranscriptNoLeak is the
// UI-CRIER-001 acceptance test. The page, governing spec and user-facing docs
// must all choose posture (c), and an auth-enabled server must refuse every
// protected chat/transcript read without returning the recorded message.
func TestUIAuthPostureAgreementAndUnauthenticatedTranscriptNoLeak(t *testing.T) {
	read := func(path string) string {
		t.Helper()
		body, err := os.ReadFile(path)
		require.NoError(t, err)
		return string(body)
	}

	spec := read("../../specs/CHAT-INTERFACE.md")
	readme := read("../../README.md")
	fronting := read("../../docs/fronting-auth.md")
	page := string(chatBrowserRefusalTemplate)
	postureDocs := []struct {
		name, text, decision string
	}{
		{"governing spec", spec, "Decision: (c)"},
		{"README", readme, "decision (c)"},
		{"fronting docs", fronting, "decision (c)"},
		{"/chat refusal", page, "(c)"},
	}
	for _, doc := range postureDocs {
		doc := doc
		t.Run(doc.name+" chooses API-only posture", func(t *testing.T) {
			require.Contains(t, doc.text, "UI-CRIER-001")
			require.Contains(t, doc.text, "API-only")
			require.Contains(t, doc.text, doc.decision)
			require.Contains(t, strings.ToLower(doc.text), "browser login")
		})
	}
	for name, text := range map[string]string{
		"README":        readme,
		"fronting docs": fronting,
		"/chat refusal": page,
	} {
		t.Run(name+" does not promise direct browser chat", func(t *testing.T) {
			require.NotContains(t, text, "so a browser can use `/chat`")
			require.NotContains(t, text, "Then browse `http://127.0.0.1:8770/chat`")
		})
	}

	const (
		deploymentToken = "ui-posture-deployment-token"
		transcriptText  = "UI-CRIER-001 transcript sentinel"
	)
	parallelBoot(t)
	base := startTestServerWithEnv(t, map[string]string{
		"CR_AUTH_TOKEN":        deploymentToken,
		"CR_REQUIRE_AGENT_SIG": "false",
		"CR_SESSION_BACKEND":   "jsonl",
		"CR_SESSION_LOG_ROOT":  t.TempDir(),
		"CR_GROUP_ROOT":        t.TempDir(),
	})
	client := &http.Client{Timeout: 5 * time.Second}
	authedRequest := func(method, path string, body any, out any) (int, string) {
		t.Helper()
		reqBody, err := json.Marshal(body)
		require.NoError(t, err)
		req, err := http.NewRequest(method, base+path, bytes.NewReader(reqBody))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+deploymentToken)
		resp, err := client.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		raw, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		if out != nil && len(raw) > 0 {
			require.NoError(t, json.Unmarshal(raw, out), string(raw))
		}
		return resp.StatusCode, string(raw)
	}

	var room struct {
		ID string `json:"id"`
	}
	code, raw := authedRequest(http.MethodPost, "/sessions", map[string]any{
		"title":      "UI auth posture",
		"created_by": map[string]string{"principal": "ui-test"},
	}, &room)
	require.Equal(t, http.StatusCreated, code, raw)
	require.NotEmpty(t, room.ID)
	code, raw = authedRequest(http.MethodPost, "/sessions/"+room.ID+"/messages", map[string]any{
		"sender":  "ui-test",
		"payload": map[string]string{"text": transcriptText},
	}, nil)
	require.Equal(t, http.StatusCreated, code, raw)

	for _, path := range []string{"/chat", "/sessions", "/sessions/" + room.ID + "/messages"} {
		t.Run("unauthenticated "+path, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, base+path, nil)
			require.NoError(t, err)
			if path == "/chat" {
				req.Header.Set("Accept", "text/html")
			}
			resp, err := client.Do(req)
			require.NoError(t, err)
			defer func() { _ = resp.Body.Close() }()
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.Equal(t, http.StatusUnauthorized, resp.StatusCode, string(body))
			require.NotContains(t, string(body), transcriptText)
		})
	}

	// The public documentation endpoint is not a transcript surface and must
	// likewise contain no data from the protected session.
	req, err := http.NewRequest(http.MethodGet, base+"/docs", nil)
	require.NoError(t, err)
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.False(t, strings.Contains(string(body), transcriptText))
}
