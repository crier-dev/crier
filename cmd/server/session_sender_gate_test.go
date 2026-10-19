package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type sessionBearerTransport struct{ token string }

func (tr sessionBearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	clone.Header.Set("Authorization", "Bearer "+tr.token)
	return http.DefaultTransport.RoundTrip(clone)
}

func TestSessionSenderBearerCannotBypassEnforcement(t *testing.T) {
	for _, enforce := range []bool{true, false} {
		t.Run(strconv.FormatBool(enforce), func(t *testing.T) {
			const token = "session-test-deployment-access"
			base := startTestServerWithEnv(t, map[string]string{"CR_AUTH_TOKEN": token, "CR_REQUIRE_AGENT_SIG": strconv.FormatBool(enforce), "CR_SESSION_BACKEND": "jsonl", "CR_SESSION_LOG_ROOT": t.TempDir(), "CR_GROUP_ROOT": t.TempDir()})
			client := &http.Client{Timeout: 5 * time.Second, Transport: sessionBearerTransport{token}}
			code, raw := sessionDo(t, client, http.MethodPost, base+"/agents", map[string]string{"id": "receiver", "public_key": strings.Repeat("ab", 32)}, nil)
			require.Equal(t, 201, code, raw)
			var room struct {
				ID string `json:"id"`
			}
			code, raw = sessionDo(t, client, http.MethodPost, base+"/sessions", map[string]any{"created_by": map[string]string{"agent": "sender"}, "members": []map[string]string{{"member_type": "agent", "member_id": "receiver"}}}, &room)
			require.Equal(t, 201, code, raw)
			endpoint := base + "/sessions/" + room.ID + "/messages"
			var sent map[string]any
			code, raw = sessionDo(t, client, http.MethodPost, endpoint, map[string]any{"sender": "sender", "payload": map[string]string{"text": "same bearer send"}}, &sent)
			if enforce {
				require.Equal(t, 401, code, raw)
				require.Equal(t, "SESSION_SENDER_SIGNATURE_REQUIRED", sent["error"])
			} else {
				require.Equal(t, 201, code, raw)
				require.Equal(t, "sender-identity-claimed", sent["sender_identity_basis"])
			}
			var transcript struct {
				Messages []map[string]any `json:"messages"`
			}
			code, raw = sessionDo(t, client, http.MethodGet, endpoint, nil, &transcript)
			require.Equal(t, 200, code, raw)
			if enforce {
				require.Empty(t, transcript.Messages)
			} else {
				require.Len(t, transcript.Messages, 1)
				require.Equal(t, "sender-identity-claimed", transcript.Messages[0]["sender_identity_basis"])
			}
		})
	}
}

func TestSessionSenderGateCoversEverySendEntrypoint(t *testing.T) {
	base := startTestServerWithEnv(t, map[string]string{"CR_REQUIRE_AGENT_SIG": "true", "CR_SESSION_BACKEND": "jsonl", "CR_SESSION_LOG_ROOT": t.TempDir(), "CR_GROUP_ROOT": t.TempDir()})
	client := &http.Client{Timeout: 5 * time.Second}
	key := registerA2AAgent(t, base, "owner", false, nil)
	other := registerA2AAgent(t, base, "receiver", false, nil)
	var room struct {
		ID string `json:"id"`
	}
	code, raw := sessionDo(t, client, http.MethodPost, base+"/sessions", map[string]any{"created_by": map[string]string{"agent": "owner"}, "members": []map[string]string{{"member_type": "agent", "member_id": "receiver"}}}, &room)
	require.Equal(t, 201, code, raw)
	path := "/sessions/" + room.ID
	code, wire := signedAs(t, client, http.MethodPost, base, path+"/tasks", "owner", key, []byte(`{"sender":"owner","payload":{"text":"work"},"targets":[{"kind":"agent","id":"receiver"}]}`))
	require.Equal(t, 201, code, string(wire))
	var task struct {
		TaskID string `json:"task_id"`
		ID     string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(wire, &task))
	code, wire = signedAs(t, client, http.MethodPost, base, path+"/tasks/"+task.TaskID+"/claim", "receiver", other, []byte(`{"actor":"receiver","actor_kind":"agent"}`))
	require.Equal(t, 201, code, string(wire))
	var before map[string]any
	code, raw = sessionDo(t, client, http.MethodGet, base+path+"/messages", nil, &before)
	require.Equal(t, 200, code, raw)
	for _, tc := range []struct{ suffix, body string }{
		{"/messages", `{"sender":"owner","payload":{"text":"blocked"},"targets":[{"kind":"agent","id":"outsider"}]}`},
		{"/tasks", `{"sender":"owner","payload":{"text":"blocked"},"targets":[{"kind":"agent","id":"receiver"}]}`},
		{"/tasks/" + task.TaskID + "/claim", `{"actor":"receiver","actor_kind":"agent"}`},
		{"/tasks/" + task.TaskID + "/complete", `{"actor":"receiver","actor_kind":"agent","payload":{"text":"evidence"}}`},
		{"/compile", `{"sender":"owner","message_ids":["` + task.ID + `"]}`},
	} {
		var body map[string]any
		require.NoError(t, json.Unmarshal([]byte(tc.body), &body))
		code, raw = sessionDo(t, client, http.MethodPost, base+path+tc.suffix, body, nil)
		require.Equal(t, 401, code, tc.suffix+raw)
		require.Contains(t, raw, "SESSION_SENDER_SIGNATURE_REQUIRED")
		var after map[string]any
		code, raw = sessionDo(t, client, http.MethodGet, base+path+"/messages", nil, &after)
		require.Equal(t, 200, code, raw)
		require.Equal(t, before, after, "unsigned %s must not record or branch", tc.suffix)
	}
}
