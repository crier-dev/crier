package main

import (
	"net/http"
	"testing"

	"github.com/crier-dev/crier/internal/session"
	"github.com/stretchr/testify/require"
)

// Use a real session HTTP send, the production guarded dispatcher and an HTTP
// webhook. Check both the send response and the independently re-opened log plus
// public transcript; a 200 must not make those three surfaces contradict.
func TestSessionFanoutWebhookBodyVerdict(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    string
		outcome string
		reasons []string
		answer  string
	}{
		{name: "live false", body: `{"reply":"demo answer","live":false,"reason":"requires OPENCODE_LIVE=1"}`, outcome: "canned", reasons: []string{"requires OPENCODE_LIVE=1"}},
		{name: "canned true", body: `{"reply":"demo answer","canned":true}`, outcome: "canned", reasons: []string{"canned"}},
		{name: "canned overrides live", body: `{"text":"demo answer","live":true,"canned":true}`, outcome: "canned"},
		{name: "consumer refused demo", body: `{"reply":"REFUSED: missing key","live":false,"canned":true,"status":"REFUSED","reason":"missing DEEPSEEK_API_KEY"}`, outcome: "canned", reasons: []string{"missing DEEPSEEK_API_KEY"}},
		{name: "failed error", body: `{"status":"FAILED","error":"codex: exit code 3; stderr: boom"}`, outcome: "failed", reasons: []string{"codex: exit code 3; stderr: boom"}},
		{name: "failed reason and text", body: `{"status":"FAILED","reason":"runtime stopped","reply":"not an answer"}`, outcome: "failed", reasons: []string{"runtime stopped"}},
		{name: "failed overrides canned", body: `{"status":"FAILED","live":false,"canned":true,"reason":"runtime stopped","error":"exit code 3","text":"not an answer"}`, outcome: "failed", reasons: []string{"runtime stopped", "exit code 3"}},
		{name: "failed without diagnostics", body: `{"status":"FAILED"}`, outcome: "failed", reasons: []string{"FAILED"}},
		{name: "error without status", body: `{"error":"runtime crashed","reply":"not an answer"}`, outcome: "failed", reasons: []string{"runtime crashed"}},
		{name: "genuine reply only", body: `{"reply":"ATLAS-ANSWER"}`, outcome: "delivered", answer: "ATLAS-ANSWER"},
		{name: "explicit live", body: `{"text":"LIVE-ANSWER","live":true,"canned":false,"status":"OK"}`, outcome: "delivered", answer: "LIVE-ANSWER"},
		{name: "null live is not false", body: `{"reply":"ATLAS-ANSWER","live":null}`, outcome: "delivered", answer: "ATLAS-ANSWER"},
		{name: "empty reply", body: `{"reply":""}`, outcome: "delivered"},
		{name: "blank live reply", body: `{"reply":"  ","live":true}`, outcome: "delivered"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			base, room, calls, envelopes := sessionDispatchFixtureAtRoot(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}), "allow", root)
			msg := sendDispatchMessage(t, base, room)
			stored := storedDispatchTranscript(t, root, room)
			wantMessages := 1
			if tc.answer != "" {
				wantMessages++
			}
			require.Len(t, stored.Messages, wantMessages, "non-live or empty replies must not become agent answers")
			require.Equal(t, tc.outcome, msg.Outcomes[0].Outcome)
			require.Len(t, stored.Messages[0].Outcomes, 1)
			require.Equal(t, session.Outcome(tc.outcome), stored.Messages[0].Outcomes[0].Outcome)
			require.Equal(t, msg.Outcomes[0].Reason, stored.Messages[0].Outcomes[0].Reason)
			for _, reason := range tc.reasons {
				require.Contains(t, msg.Outcomes[0].Reason, reason)
			}
			if tc.outcome == "delivered" {
				require.Equal(t, 1, msg.Delivered)
				require.Empty(t, msg.Outcomes[0].Reason)
			} else {
				require.Zero(t, msg.Delivered, "a runtime's negative verdict must not be summarized as delivered")
			}
			require.Zero(t, msg.Skipped, "a runtime verdict is not a refused delivery")

			var transcript struct {
				Messages []struct {
					sessionDispatchView
					ParentID string            `json:"parent_id"`
					Author   session.AuthorRef `json:"author"`
					Payload  map[string]string `json:"payload"`
				} `json:"messages"`
			}
			code, raw := sessionDo(t, http.DefaultClient, http.MethodGet, base+"/sessions/"+room+"/messages", nil, &transcript)
			require.Equal(t, http.StatusOK, code, "%s", raw)
			require.Len(t, transcript.Messages, wantMessages)
			require.Len(t, transcript.Messages[0].Outcomes, 1)
			require.Equal(t, tc.outcome, transcript.Messages[0].Outcomes[0].Outcome)
			require.Equal(t, msg.Outcomes[0].Reason, transcript.Messages[0].Outcomes[0].Reason)
			if tc.answer != "" {
				reply := stored.Messages[1]
				require.Equal(t, msg.ID, reply.ParentID)
				require.Equal(t, msg.ThreadID, reply.ThreadID)
				require.Equal(t, room, reply.SessionID)
				require.Equal(t, session.AuthorRef{Agent: "session-target"}, reply.Author)
				require.Equal(t, session.AudienceExplicit, reply.Audience.Rule)
				require.NotNil(t, reply.Audience.Targets)
				require.Empty(t, reply.Audience.Targets)
				require.Empty(t, reply.Outcomes)
				require.Equal(t, msg.ID, transcript.Messages[1].ParentID)
				require.Equal(t, msg.ThreadID, transcript.Messages[1].ThreadID)
				require.Equal(t, "session-target", transcript.Messages[1].Author.Agent)
				require.Equal(t, tc.answer, transcript.Messages[1].Payload["text"])
				require.Len(t, stored.Threads, 1)
			}
			require.Len(t, envelopes, 1, "recording must never fan out the reply")
			require.EqualValues(t, 1, calls.Load(), "the target guard still runs exactly once")
		})
	}
}
