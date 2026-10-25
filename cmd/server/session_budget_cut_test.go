package main

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/session"
	"github.com/stretchr/testify/require"
)

// Drive the real boot/HTTP/session/registry/webhook path, including a 200 that
// flushes valid JSON before stalling: headers (or even JSON) are not a full reply.
//
// The cut is graded by CAUSAL invariants, not by a wall-clock margin (QA-CRIER-45).
// The late runtime's answer is held on a channel until this test releases it, so
// "the cut returned without waiting for the runtime" is a fact about the code:
// a dispatch that waits for the runtime cannot return before that answer exists.
// The one remaining wall-clock assertion is the budget LOWER bound, which host
// contention can only make more true.
//
// A budget-relative UPPER bound would not survive the failure this replaces. The
// recorded full-suite red measured 1.53s and 2.53s of end-to-end elapsed against
// this 200ms budget while the cut itself fired ON the budget (~20-40ms of
// overhead); the response goroutine was not scheduled, so the extra second is the
// host's, not the code's. Any bound near the budget (budget*1.5, budget+50ms)
// grades the host and re-reds the same way.
func TestSessionDeliveryBudgetCut(t *testing.T) {
	for _, flushed := range []bool{false, true} {
		t.Run(fmt.Sprintf("flushed=%t", flushed), func(t *testing.T) {
			const budget = 200 * time.Millisecond
			// lateHold bounds only the failure path. A dispatch that WAITS for the
			// runtime returns when the runtime answers, so the hold must be finite
			// for that regression to fail an assertion instead of hanging the
			// package; a healthy run never reaches it — the test releases the answer
			// as soon as it has graded the cut.
			const lateHold = 10 * time.Second
			// INT-CI-014: the budget posture travels in the boot overlay —
			// both keys are read by config at boot, so the fixture's extra
			// map carries them and the test never touches process env.
			budgetExtra := map[string]string{"CR_SESSION_DELIVERY_TIMEOUT_MS": "200", "RUNTIME_TIMEOUT_MS": "100"}
			lateDone := make(chan struct{})
			lateRelease := make(chan struct{})
			var closeLate sync.Once
			root := t.TempDir()
			base, room, _, _ := sessionDispatchFixtureAtRootWithEnv(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				defer closeLate.Do(func() { close(lateDone) })
				if flushed {
					_, _ = w.Write([]byte(`{"reply":"partial answer must be discarded","live":true}`))
					w.(http.Flusher).Flush()
				}
				// Deliberately ignore cancellation: simulate a runtime that finishes
				// late. The body is produced only after the hold, so lateness is a
				// causal property of this fixture and not a timing bet.
				select {
				case <-lateRelease:
				case <-time.After(lateHold):
				}
				if !flushed {
					_, _ = w.Write([]byte(`{"reply":"late answer must be discarded","live":true}`))
				}
			}), "allow", root, budgetExtra)
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
			require.GreaterOrEqual(t, elapsed, budget, "the cut must exercise its configured budget (host load can only increase this)")
			// No upper wall-clock bound: the outcome assertions above ARE that bound.
			// A dispatch that waited for the runtime would have the late answer in
			// hand and report a delivered outcome carrying its body, not a
			// budget_cut with "1 attempt" and Delivered 0.
			before := storedDispatchTranscript(t, root, room)
			require.Len(t, before.Messages, 1)
			require.Equal(t, session.Outcome("budget_cut"), before.Messages[0].Outcomes[0].Outcome)
			// Release the held answer now, so it reaches the (already answered)
			// dispatch strictly after the cut and must be discarded.
			close(lateRelease)
			<-lateDone
			// A negative assertion: waiting longer can only make a wrongful append
			// more visible, so this settle window is load-safe in the direction that
			// matters and is not a latency SLA.
			time.Sleep(budget)
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
	parallelBoot(t)
	// INT-CI-014: the budget posture travels in the boot overlay.
	budgetExtra := map[string]string{"CR_SESSION_DELIVERY_TIMEOUT_MS": "1200", "RUNTIME_TIMEOUT_MS": "100"}
	var calls atomic.Int32
	root := t.TempDir()
	base, room, _, _ := sessionDispatchFixtureAtRootWithEnv(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}), "allow", root, budgetExtra)
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
