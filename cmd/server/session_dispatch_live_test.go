// DF-CRIER-414: a send must not look frozen while delivery is in flight.
//
// The per-target delivery budget is deliberate (30s by default); the defect was
// VISIBILITY inside it. These tests drive the real production composition — a
// registered agent whose webhook points at a dead endpoint (and one that is slow
// but alive) — and pin the read surface the composer panel is built on:
//
//  1. while the fan-out is in flight the transcript carries `dispatch_live`:
//     the pass boundaries (attempt, started_at, the budget) and the per-target
//     attempts the runtime reported, including an UNREACHABLE dial named with
//     its own reason — long before the budget expires;
//  2. the pass carries no outcome, so in-flight state can never be mistaken for
//     a delivery result, and a merely SLOW endpoint is never reported
//     unreachable;
//  3. after the budget the terminal outcome is the honest budget_cut and
//     `dispatch_live` is GONE — the indicator cannot be left spinning.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// dispatchLiveView is the wire shape this row adds, decoded from a real
// transcript response rather than from an internal struct.
type dispatchLiveView struct {
	Attempt   int    `json:"attempt"`
	StartedAt string `json:"started_at"`
	ElapsedMs int64  `json:"elapsed_ms"`
	BudgetMs  int    `json:"budget_ms"`
	Targets   []struct {
		Target        string `json:"target"`
		DeliveryClass string `json:"delivery_class"`
		Attempts      int    `json:"attempts"`
		LastAttemptAt string `json:"last_attempt_at"`
		Status        int    `json:"status"`
		Class         string `json:"class"`
		Reason        string `json:"reason"`
	} `json:"targets"`
}

type liveTranscriptView struct {
	Messages []struct {
		ID                 string            `json:"id"`
		IdempotencyKey     string            `json:"idempotency_key"`
		OutcomesIncomplete bool              `json:"outcomes_incomplete"`
		DispatchLive       *dispatchLiveView `json:"dispatch_live"`
		Outcomes           []struct {
			Target  string `json:"target"`
			Outcome string `json:"outcome"`
			Detail  string `json:"detail"`
		} `json:"outcomes"`
	} `json:"messages"`
}

func fetchLiveTranscript(t *testing.T, client *http.Client, base, room string) liveTranscriptView {
	t.Helper()
	resp, err := client.Get(base + "/sessions/" + room + "/messages")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var body liveTranscriptView
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	return body
}

// postMessageAsync issues a real send and returns a channel carrying the
// terminal response, so in-flight reads and the terminal outcome are observed
// through the same production path.
type sendOutcome struct {
	status int
	body   []byte
}

func postMessageAsync(t *testing.T, client *http.Client, base, room string, payload any) <-chan sendOutcome {
	t.Helper()
	raw, err := json.Marshal(payload)
	require.NoError(t, err)
	out := make(chan sendOutcome, 1)
	go func() {
		req, err := http.NewRequest(http.MethodPost, base+"/sessions/"+room+"/messages", bytes.NewReader(raw))
		if err != nil {
			out <- sendOutcome{status: -1, body: []byte(err.Error())}
			return
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			out <- sendOutcome{status: -1, body: []byte(err.Error())}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		out <- sendOutcome{status: resp.StatusCode, body: body}
	}()
	return out
}

// registerLiveTarget creates a session with one agent member and a
// webhook-registered target, exercising the shipped registry and session routes.
func registerLiveTarget(t *testing.T, base, agentID, hookURL string) string {
	t.Helper()
	code, raw := sessionDo(t, http.DefaultClient, http.MethodPost, base+"/agents", map[string]any{
		"id": agentID, "public_key": strings.Repeat("ab", 32),
		"webhook": map[string]any{"url": hookURL, "delivery_mode": "blocking"},
	}, nil)
	require.Equal(t, http.StatusCreated, code, "%s", raw)
	var room struct {
		ID string `json:"id"`
	}
	code, raw = sessionDo(t, http.DefaultClient, http.MethodPost, base+"/sessions", map[string]any{
		"title": "in-flight window", "created_by": map[string]string{"principal": "operator"},
		"members": []map[string]string{{"member_type": "agent", "member_id": agentID, "role": "member"}},
	}, &room)
	require.Equal(t, http.StatusCreated, code, "%s", raw)
	return room.ID
}

// TestSessionDispatchLivePinsTheInFlightWindow keeps the run short with a 2s
// delivery budget, still strictly above the declared runtime budget as
// config.Load requires. The endpoint is dead (nothing listens on the port), so
// the dial fails at once — the measured defect's shape.
func TestSessionDispatchLivePinsTheInFlightWindow(t *testing.T) {
	parallelBoot(t)
	deadPort := freePort(t)
	base := startTestServerWithEnv(t, map[string]string{
		"CR_SESSION_BACKEND": "jsonl", "CR_SESSION_LOG_ROOT": t.TempDir(),
		"CR_REQUIRE_AGENT_SIG": "false", "CR_GROUP_ROOT": t.TempDir(),
		"RUNTIME_TIMEOUT_MS": "1000", "CR_SESSION_DELIVERY_TIMEOUT_MS": "2000",
	})
	room := registerLiveTarget(t, base, "dead-bot", fmt.Sprintf("http://127.0.0.1:%d/hook", deadPort))

	// A long-lived client: the blocking fan-out legitimately outlasts the 2s
	// budget of this fixture, and the browser's own wait is not the server's.
	client := &http.Client{Timeout: 30 * time.Second}
	sent := postMessageAsync(t, client, base, room, map[string]any{
		"sender": "operator", "payload": map[string]string{"text": "are you there?"},
		"idempotency_key": "live-progress-key",
	})

	// (1) The in-flight window: poll for the runtime's own first attempt. Every
	// assertion below reads the server's response; nothing is synthesised from
	// a client-side timer. The window is the DELIVERY budget (2s), not a fixed
	// 1500ms: under a fully loaded host the durable-write→dispatch start can
	// itself take a second+ (measured, INT-CI-014 three-run floor), and the
	// in-flight reading is still genuinely pre-budget whenever the fan-out has
	// not yet expired — the Later assertions pin ElapsedMs < BudgetMs anyway.
	deadline := time.Now().Add(2 * time.Second)
	var live *dispatchLiveView
	for live == nil && time.Now().Before(deadline) {
		view := fetchLiveTranscript(t, client, base, room)
		if len(view.Messages) != 0 {
			require.Len(t, view.Messages, 1, "the durable intent is written before dispatch starts (§3.2 D1)")
			msg := view.Messages[0]
			require.Equal(t, "live-progress-key", msg.IdempotencyKey)
			require.Empty(t, msg.Outcomes, "in-flight state must not carry an outcome")
			if msg.DispatchLive != nil && len(msg.DispatchLive.Targets) == 1 && msg.DispatchLive.Targets[0].Class != "" {
				live = msg.DispatchLive
			}
		}
		if live == nil {
			time.Sleep(20 * time.Millisecond)
		}
	}
	require.NotNil(t, live, "the transcript must carry dispatch_live while the fan-out is in flight")
	require.Equal(t, 1, live.Attempt, "the live pass names the same journal attempt the durable record has")
	require.Equal(t, 2000, live.BudgetMs, "the budget is the server's configured per-target budget")
	require.NotEmpty(t, live.StartedAt)
	require.GreaterOrEqual(t, live.ElapsedMs, int64(0))
	require.Less(t, live.ElapsedMs, int64(2000), "the in-flight reading lands before the budget expires")
	require.Len(t, live.Targets, 1)
	target := live.Targets[0]
	require.Equal(t, "dead-bot", target.Target)
	require.Equal(t, "blocking_webhook", target.DeliveryClass, "the production handler identifies where attempt evidence is expected")
	require.GreaterOrEqual(t, target.Attempts, 1, "the runtime reported the attempt it made")
	require.NotEmpty(t, target.LastAttemptAt)
	require.Equal(t, "unreachable", target.Class, "a refused dial is named, not waited out")
	require.Contains(t, target.Reason, fmt.Sprintf("127.0.0.1:%d", deadPort), "the reason names the endpoint that failed")
	require.Contains(t, target.Reason, "connection refused")
	require.Zero(t, target.Status, "no HTTP response was received")
	t.Logf("in-flight: elapsed=%dms budget=%dms target=%s class=%s reason=%q",
		live.ElapsedMs, live.BudgetMs, target.Target, target.Class, target.Reason)

	// (2) The terminal outcome. The budget is deliberate: the send ends in an
	// honest budget_cut, with the attempts and budget it consumed named.
	var result sendOutcome
	select {
	case result = <-sent:
	case <-time.After(20 * time.Second):
		t.Fatal("the send never returned inside its budget")
	}
	require.Equal(t, http.StatusCreated, result.status, "%s", result.body)
	var sendBody struct {
		Outcomes []struct {
			Target  string `json:"target"`
			Outcome string `json:"outcome"`
			Detail  string `json:"detail"`
		} `json:"outcomes"`
		DispatchLive *dispatchLiveView `json:"dispatch_live"`
	}
	require.NoError(t, json.Unmarshal(result.body, &sendBody))
	require.Len(t, sendBody.Outcomes, 1)
	// The terminated vocabulary is unchanged, and for a REFUSED dial it is
	// `refused`, not budget_cut: §3.2 is explicit that an exhausted budget alone
	// is not a cut — the endpoint refused every attempt, the backoff merely
	// consumed the budget. The reason keeps the attempts, the budget and the
	// transport's own diagnostic.
	require.Equal(t, "refused", sendBody.Outcomes[0].Outcome, "terminal vocabulary is unchanged")
	require.Contains(t, sendBody.Outcomes[0].Detail, "connection refused")
	require.Contains(t, sendBody.Outcomes[0].Detail, "budget")
	require.Nil(t, sendBody.DispatchLive, "a returned send is no longer in flight")

	// (3) The indicator cannot be left spinning: the live pass is gone and the
	// durable record carries the terminal outcome.
	settled := fetchLiveTranscript(t, client, base, room)
	require.Len(t, settled.Messages, 1)
	require.Nil(t, settled.Messages[0].DispatchLive, "the in-flight view ends with the fan-out")
	require.False(t, settled.Messages[0].OutcomesIncomplete)
	require.Len(t, settled.Messages[0].Outcomes, 1)
	require.Equal(t, "refused", settled.Messages[0].Outcomes[0].Outcome)
	require.Equal(t, "dead-bot", settled.Messages[0].Outcomes[0].Target)
}

// A slow-but-alive endpoint must keep the panel honest in the other direction:
// the live pass reports the attempt it made and never invents an unreachable
// class, and the delivery still ends as delivered.
func TestSessionDispatchLiveNeverCallsASlowEndpointUnreachable(t *testing.T) {
	parallelBoot(t)
	release := make(chan struct{})
	hook := newBlockingHook(t, release)
	base := startTestServerWithEnv(t, map[string]string{
		"CR_SESSION_BACKEND": "jsonl", "CR_SESSION_LOG_ROOT": t.TempDir(),
		"CR_REQUIRE_AGENT_SIG": "false", "CR_GROUP_ROOT": t.TempDir(),
		"RUNTIME_TIMEOUT_MS": "1000", "CR_SESSION_DELIVERY_TIMEOUT_MS": "5000",
	})
	room := registerLiveTarget(t, base, "slow-bot", hook)

	client := &http.Client{Timeout: 30 * time.Second}
	sent := postMessageAsync(t, client, base, room, map[string]any{
		"sender": "operator", "payload": map[string]string{"text": "hello"},
		"idempotency_key": "slow-but-alive",
	})

	deadline := time.Now().Add(2 * time.Second)
	var live *dispatchLiveView
	for live == nil && time.Now().Before(deadline) {
		view := fetchLiveTranscript(t, client, base, room)
		if len(view.Messages) == 1 && view.Messages[0].DispatchLive != nil &&
			len(view.Messages[0].DispatchLive.Targets) == 1 {
			live = view.Messages[0].DispatchLive
		}
		if live == nil {
			time.Sleep(20 * time.Millisecond)
		}
	}
	close(release)
	require.NotNil(t, live, "the transcript must carry dispatch_live while the fan-out is in flight")
	require.Equal(t, "slow-bot", live.Targets[0].Target)
	// An attempt is reported when it RETURNS. An endpoint that is still holding
	// the request open has produced no result yet, so there is no attempt
	// evidence — and emphatically no invented failure class.
	require.Empty(t, live.Targets[0].Class, "a slow endpoint is in flight, never reported unreachable")
	require.Empty(t, live.Targets[0].Reason)
	require.Zero(t, live.Targets[0].Attempts, "no attempt has returned yet")
	require.Empty(t, live.Targets[0].LastAttemptAt)
	require.Less(t, live.ElapsedMs, int64(live.BudgetMs), "the reading is bounded by the server's own budget")

	var result sendOutcome
	select {
	case result = <-sent:
	case <-time.After(20 * time.Second):
		t.Fatal("the send never returned")
	}
	require.Equal(t, http.StatusCreated, result.status, "%s", result.body)
	var body struct {
		Outcomes []struct {
			Outcome string `json:"outcome"`
		} `json:"outcomes"`
	}
	require.NoError(t, json.Unmarshal(result.body, &body))
	require.Len(t, body.Outcomes, 1)
	require.Equal(t, "delivered", body.Outcomes[0].Outcome, "the live view must not change the terminal outcome")
}

// newBlockingHook starts an endpoint that holds the request open until release
// is closed, then answers with a displayable reply.
func newBlockingHook(t *testing.T, release <-chan struct{}) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"reply":"still here"}`))
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return "http://" + ln.Addr().String()
}
