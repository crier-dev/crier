package main

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/session"
	"github.com/stretchr/testify/require"
)

func TestSessionParticipantAdmissionAndLegacyPartialSend(t *testing.T) {
	parallelBoot(t)
	root := t.TempDir()
	base, room, calls, envelopes := sessionDispatchFixtureAtRoot(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"reply":"Echo answer"}`)) }), "allow", root)
	code, raw := sessionDo(t, http.DefaultClient, http.MethodPost, base+"/agents", map[string]any{"id": "speaker", "public_key": strings.Repeat("cd", 32)}, nil)
	require.Equal(t, http.StatusCreated, code, "%s", raw)
	code, raw = sessionDo(t, http.DefaultClient, http.MethodPost, base+"/sessions/"+room+"/participants", map[string]string{"member_type": "agent", "member_id": "speaker", "actor": "speaker"}, nil)
	require.Equal(t, http.StatusCreated, code, "%s", raw)
	code, raw = sessionDo(t, http.DefaultClient, http.MethodPost, base+"/sessions/"+room+"/participants", map[string]string{"member_type": "agent", "member_id": "ghost", "actor": "speaker"}, nil)
	require.Equal(t, http.StatusNotFound, code)
	require.JSONEq(t, `{"error":"AGENT_NOT_FOUND","detail":"no such agent \"ghost\": register the agent before adding it to a session"}`, string(raw))
	store, err := session.NewJSONLStore(root)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	st, err := store.Load(context.Background(), room)
	require.NoError(t, err)
	require.Len(t, st.Members, 2)
	// This is intentionally a historical store write, not a new API admission.
	seq, err := store.NextSeq(context.Background(), room)
	require.NoError(t, err)
	require.NoError(t, store.AddMember(context.Background(), &session.Member{SessionID: room, MemberType: session.MemberAgent, MemberID: "ghost", Role: session.RoleMember, AddedAt: time.Now().UTC()}, seq, nil))
	var sent sessionDispatchView
	code, raw = sessionDo(t, http.DefaultClient, http.MethodPost, base+"/sessions/"+room+"/messages", map[string]any{"sender": "speaker", "payload": map[string]string{"text": "hello echo"}}, &sent)
	require.Equal(t, http.StatusCreated, code, "%s", raw)
	require.Equal(t, 1, sent.Delivered)
	require.Equal(t, 1, sent.Skipped)
	require.Len(t, sent.Outcomes, 2)
	require.Equal(t, "ghost", sent.Outcomes[0].Target)
	require.Contains(t, sent.Outcomes[0].Reason, "no such agent")
	require.Contains(t, sent.Outcomes[0].Reason, "AGENT_NOT_FOUND")
	require.Equal(t, "session-target", sent.Outcomes[1].Target)
	require.Equal(t, "delivered", sent.Outcomes[1].Outcome)
	require.EqualValues(t, 1, calls.Load())
	require.Len(t, envelopes, 1)
	stored := storedDispatchTranscript(t, root, room)
	require.Len(t, stored.Messages, 2)
	require.Equal(t, sent.ID, stored.Messages[1].ParentID)
	require.Equal(t, sent.ThreadID, stored.Messages[1].ThreadID)
	require.JSONEq(t, `{"text":"Echo answer"}`, string(stored.Messages[1].Payload))
	var inbox struct {
		Messages []any `json:"messages"`
	}
	code, raw = sessionDo(t, http.DefaultClient, http.MethodGet, base+"/agents/speaker/inbox", nil, &inbox)
	require.Equal(t, http.StatusOK, code, "%s", raw)
	require.Empty(t, inbox.Messages)
}
