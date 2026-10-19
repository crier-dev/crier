package main

import (
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/session"
	"github.com/stretchr/testify/require"
)

// Drive the real boot/HTTP/session/registry/webhook path, including a 200 that
// flushes valid JSON before stalling: headers (or even JSON) are not a full reply.
func TestSessionDeliveryBudgetCut(t *testing.T) {
	for _, flushed := range []bool{false, true} {
		t.Run(fmt.Sprintf("flushed=%t", flushed), func(t *testing.T) {
			const budget = 200 * time.Millisecond
			t.Setenv("CR_SESSION_DELIVERY_TIMEOUT_MS", "200")
			t.Setenv("RUNTIME_TIMEOUT_MS", "100")
			lateDone := make(chan struct{})
			root := t.TempDir()
			base, room, _, _ := sessionDispatchFixtureAtRoot(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				defer close(lateDone)
				if flushed {
					_, _ = w.Write([]byte(`{"reply":"partial answer must be discarded","live":true}`))
					w.(http.Flusher).Flush()
				}
				// Deliberately ignore cancellation: simulate a runtime that finishes late.
				time.Sleep(budget + 2*time.Second)
				if !flushed {
					_, _ = w.Write([]byte(`{"reply":"late answer must be discarded","live":true}`))
				}
			}), "allow", root)
			start := time.Now()
			msg := sendDispatchMessage(t, base, room)
			elapsed := time.Since(start)
			t.Logf("cut wall=%s budget=%s flushed=%t detail=%s", elapsed, budget, flushed, msg.Outcomes[0].Reason)
			require.True(t, strings.HasPrefix(msg.Outcomes[0].Reason, "answer did not arrive inside the budget:"), "budget must lead, not crier's wrapper status: %s", msg.Outcomes[0].Reason)
			require.Equal(t, "budget_cut", msg.Outcomes[0].Outcome)
			require.Equal(t, "DELIVERY_BUDGET_CUT", msg.Outcomes[0].Code)
			require.True(t, msg.Outcomes[0].Retryable)
			require.Zero(t, msg.Delivered)
			require.Zero(t, msg.Skipped, "a budget cut is not a refusal")
			for _, text := range []string{"200ms", "1 attempt", "budget exhausted", "answer did not arrive", "late answer", "discarded", "RUNTIME_TIMEOUT_MS", "CR_SESSION_DELIVERY_TIMEOUT_MS"} {
				require.Contains(t, msg.Outcomes[0].Reason, text)
			}
			require.GreaterOrEqual(t, elapsed, budget)
			require.Less(t, elapsed, time.Second, "cut must return without waiting for the late runtime")
			before := storedDispatchTranscript(t, root, room)
			require.Len(t, before.Messages, 1)
			require.Equal(t, session.Outcome("budget_cut"), before.Messages[0].Outcomes[0].Outcome)
			<-lateDone
			after := storedDispatchTranscript(t, root, room)
			require.Len(t, after.Messages, 1, "late/partial answers must never appear as bubbles")
			require.Equal(t, before.Messages[0].Outcomes, after.Messages[0].Outcomes)
			var transcript struct {
				Messages []sessionDispatchView `json:"messages"`
			}
			code, raw := sessionDo(t, http.DefaultClient, http.MethodGet, base+"/sessions/"+room+"/messages", nil, &transcript)
			require.Equal(t, http.StatusOK, code, raw)
			require.Len(t, transcript.Messages, 1)
			require.Equal(t, msg.Outcomes, transcript.Messages[0].Outcomes, "cut and discard note must survive GET")
		})
	}
}

func TestSessionDelivery503RefusalReportsAttempts(t *testing.T) {
	t.Setenv("CR_SESSION_DELIVERY_TIMEOUT_MS", "1200")
	t.Setenv("RUNTIME_TIMEOUT_MS", "100")
	var calls atomic.Int32
	root := t.TempDir()
	base, room, _, _ := sessionDispatchFixtureAtRoot(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}), "allow", root)
	start := time.Now()
	msg := sendDispatchMessage(t, base, room)
	elapsed := time.Since(start)
	t.Logf("refusal wall=%s calls=%d detail=%s", elapsed, calls.Load(), msg.Outcomes[0].Reason)
	require.True(t, strings.HasPrefix(msg.Outcomes[0].Reason, "target answered status 503;"), "target status must lead, budget must be secondary: %s", msg.Outcomes[0].Reason)
	require.Equal(t, "refused", msg.Outcomes[0].Outcome)
	require.Equal(t, "TARGET_HTTP_503", msg.Outcomes[0].Code)
	require.True(t, msg.Outcomes[0].Retryable)
	require.GreaterOrEqual(t, calls.Load(), int32(2), "exercise retry until budget, not only first refusal")
	for _, text := range []string{fmt.Sprintf("%d attempts", calls.Load()), "1.2s", "budget exhausted", "503"} {
		require.Contains(t, msg.Outcomes[0].Reason, text)
	}
	require.Zero(t, msg.Delivered)
	require.Equal(t, 1, msg.Skipped)
	require.Len(t, storedDispatchTranscript(t, root, room).Messages, 1)
	var transcript struct {
		Messages []sessionDispatchView `json:"messages"`
	}
	code, raw := sessionDo(t, http.DefaultClient, http.MethodGet, base+"/sessions/"+room+"/messages", nil, &transcript)
	require.Equal(t, http.StatusOK, code, raw)
	require.Equal(t, msg.Outcomes, transcript.Messages[0].Outcomes)
}
