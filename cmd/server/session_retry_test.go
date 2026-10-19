package main

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/session"
	"github.com/stretchr/testify/require"
)

// These tests cross the production HTTP -> session -> registry -> webhook path,
// including the registry's successful-HTTP receipt cache and durable read-back.
func TestSessionSlowHarnessAnswerLands(t *testing.T) {
	var calls atomic.Int32
	root := t.TempDir()
	base, room, _, _ := sessionDispatchFixtureAtRoot(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		select {
		case <-time.After(6 * time.Second):
			_, _ = w.Write([]byte(`{"reply":"DF-CRIER-349 six-second unique answer","live":true}`))
		case <-r.Context().Done():
		}
	}), "allow", root)
	start := time.Now()
	msg := sendDispatchMessage(t, base, room)
	require.Equal(t, "delivered", msg.Outcomes[0].Outcome)
	require.GreaterOrEqual(t, time.Since(start), 6*time.Second)
	stored := storedDispatchTranscript(t, root, room)
	require.Len(t, stored.Messages, 2)
	reply := stored.Messages[1]
	require.JSONEq(t, `{"text":"DF-CRIER-349 six-second unique answer"}`, string(reply.Payload))
	require.Equal(t, msg.ID, reply.ParentID)
	require.Equal(t, msg.ThreadID, reply.ThreadID)
	require.Equal(t, "session-target", reply.Author.Agent)
	require.EqualValues(t, 1, calls.Load())
	t.Logf("6s harness: calls=%d outcome=%s rows=%d", calls.Load(), msg.Outcomes[0].Outcome, len(stored.Messages))
}

func TestSessionRetryRedeliversOnlyAllNegative(t *testing.T) {
	for _, firstOutcome := range []string{"refused", "failed", "budget_cut", "delivered", "canned"} {
		t.Run(firstOutcome, func(t *testing.T) {
			// A short declared runtime keeps the timeout arm fast and satisfies
			// the same startup invariant as the shipped 20s/30s defaults.
			t.Setenv("CR_SESSION_DELIVERY_TIMEOUT_MS", "300")
			t.Setenv("RUNTIME_TIMEOUT_MS", "100")
			var calls atomic.Int32
			root := t.TempDir()
			base, room, _, envelopes := sessionDispatchFixtureAtRoot(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					switch firstOutcome {
					case "refused":
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					case "budget_cut":
						<-r.Context().Done()
						return
					case "failed":
						_, _ = w.Write([]byte(`{"status":"FAILED","error":"first runtime failed","reply":"must not become a bubble"}`))
						return
					case "canned":
						_, _ = w.Write([]byte(`{"live":false,"reply":"must not become a bubble"}`))
						return
					}
				}
				_, _ = w.Write([]byte(`{"live":true,"reply":"recovered unique answer"}`))
			}), "allow", root)
			path := base + "/sessions/" + room + "/messages"
			draft := map[string]any{"sender": "speaker", "payload": map[string]string{"text": "same draft"},
				"idempotency_key": "retry-same-draft", "targets": []map[string]string{{"kind": "agent", "id": "session-target"}}}
			post := func() struct {
				sessionDispatchView
				ReplayAction string `json:"replay_action"`
			} {
				var out struct {
					sessionDispatchView
					ReplayAction string `json:"replay_action"`
				}
				code, raw := sessionDo(t, http.DefaultClient, http.MethodPost, path, draft, &out)
				require.Equal(t, http.StatusCreated, code, "%s", raw)
				return out
			}
			first := post()
			require.Equal(t, firstOutcome, first.Outcomes[0].Outcome)
			require.Empty(t, first.ReplayAction)
			before := storedDispatchTranscript(t, root, room)
			if firstOutcome != "delivered" {
				require.Len(t, before.Messages, 1, "negative runtime replies must still suppress the bubble")
			}
			second := post()
			require.Equal(t, first.ID, second.ID)
			require.Equal(t, first.ThreadID, second.ThreadID)
			wantCalls, wantAction, wantOutcome := int32(1), "replayed", firstOutcome
			if firstOutcome == "refused" || firstOutcome == "failed" || firstOutcome == "budget_cut" {
				wantCalls, wantAction, wantOutcome = 2, "redelivered", "delivered"
			}
			require.Equal(t, wantAction, second.ReplayAction)
			require.Equal(t, wantOutcome, second.Outcomes[0].Outcome)
			require.Equal(t, wantCalls, calls.Load(), "cached HTTP-successful FAILED replies must not mask a real retry")
			stored := storedDispatchTranscript(t, root, room)
			operators := 0
			for _, m := range stored.Messages {
				if m.Author.Agent == "speaker" {
					operators++
					require.Equal(t, first.ID, m.ID)
					require.Equal(t, before.Messages[0].Seq, m.Seq)
					require.Equal(t, session.Outcome(wantOutcome), m.Outcomes[0].Outcome)
				}
			}
			require.Equal(t, 1, operators)
			wantRows := 2
			if firstOutcome == "canned" {
				wantRows = 1
			}
			require.Len(t, stored.Messages, wantRows)
			third := post()
			require.Equal(t, "replayed", third.ReplayAction)
			require.Equal(t, wantCalls, calls.Load(), "successful retry becomes a pure replay thereafter")
			require.Len(t, storedDispatchTranscript(t, root, room).Messages, wantRows)
			for i := int32(0); i < wantCalls; i++ {
				env := <-envelopes
				require.Equal(t, first.ID, env.Crier.RequestID)
				require.Equal(t, session.InboxEntryID(first.ID, "session-target"), env.Crier.MessageID)
			}
			var transcript struct {
				Messages []sessionDispatchView `json:"messages"`
			}
			code, raw := sessionDo(t, http.DefaultClient, http.MethodGet, path, nil, &transcript)
			require.Equal(t, http.StatusOK, code, "%s", raw)
			require.Len(t, transcript.Messages, wantRows)
			require.Equal(t, wantOutcome, transcript.Messages[0].Outcomes[0].Outcome)
			if wantRows == 2 {
				require.True(t, strings.Contains(raw, "recovered unique answer"))
			}
			t.Logf("initial=%s calls first=1 after retry=%d after replay=%d operator_rows=%d action=%s", firstOutcome, wantCalls, calls.Load(), operators, second.ReplayAction)
		})
	}
}
