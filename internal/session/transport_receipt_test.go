package session

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/crier-dev/crier/internal/federation"
	"github.com/crier-dev/crier/internal/registry"
	"github.com/stretchr/testify/require"
)

// String expectations deliberately compile against fb82c89 too: the RED control
// must exercise the old producers, not fail because new constants are absent.
func TestRemoteTransportReceipt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, body, inbound, verdict, want, code, answer string
		status                                           int
	}{
		{name: "held-unchanged", status: 202, body: `{"status":"held"}`, want: "held"},
		{name: "token_only", status: 201, body: `{}`, inbound: "FED_TOKEN_ONLY", want: "accepted", code: "FED_TOKEN_ONLY"},
		{name: "inbox_queue", status: 201, body: `{"id":"queued","transport":"inbox"}`, want: "accepted"},
		{name: "async", status: 202, body: `{}`, want: "accepted"},
		{name: "claimed-delivered-no-reply", status: 200, body: `{"delivered":true}`, want: "accepted", code: "PEER_CLAIMED_DELIVERY"},
		{name: "status-word-delivered-no-reply", status: 200, body: `{"status":"delivered"}`, want: "accepted", code: "PEER_CLAIMED_DELIVERY"},
		{name: "real-reply-no-word", status: 200, body: `{"reply":"the answer","live":true}`, want: "delivered", answer: "the answer"},
		{name: "nested-artifact", status: 200, body: `{"artifact":{"parts":[{"text":"first"},{"kind":"text","text":"second"}]}}`, want: "delivered", answer: "first\nsecond"},
		{name: "nested-message", status: 200, body: `{"message":{"parts":[{"type":"text","text":"the answer"}]}}`, want: "delivered", answer: "the answer"},
		{name: "plain-text", status: 200, body: "the answer", want: "delivered", answer: "the answer"},
		{name: "explicit_verdict_delivery", status: 201, body: `{}`, verdict: "delivered", want: "accepted", code: "PEER_CLAIMED_DELIVERY"},
		{name: "policy_allow_is_not_delivery", status: 201, body: `{"verdict":"FED_ALLOWED"}`, want: "accepted"},
		{name: "token_only_body", status: 201, body: `{"verdict":"FED_TOKEN_ONLY","status":"delivered"}`, want: "accepted", code: "PEER_CLAIMED_DELIVERY"},
		{name: "untrusted_body", status: 201, body: `{"status":"secret-not-a-verdict","reason":"do-not-persist"}`, want: "accepted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path != "/agents/worker/inbox" {
					t.Errorf("unexpected path: %s", r.URL.Path)
				}
				w.Header().Set("X-Crier-Fed-Inbound-Verdict", tc.inbound)
				w.Header().Set(federation.VerdictHeader, tc.verdict)
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(peer.Close)
			a := newInstance(t)
			a.armPeerPolicy(federation.PeerPolicies{"peer": {Peer: "peer", URL: peer.URL, NamespacesAllow: []string{""}}})
			room := createFederatedSession(t, a)
			status, body := a.do("POST", "/sessions/"+room+"/participants", `{"member_type":"agent","member_id":"remote:peer:worker"}`, nil)
			require.Equal(t, 201, status, body)
			status, body = a.do("POST", "/sessions/"+room+"/messages", `{"sender":"operator","payload":{"text":"receipt probe"}}`, nil)
			require.Equal(t, 201, status, body)
			var sent transcriptMessage
			require.NoError(t, json.Unmarshal([]byte(body), &sent))
			require.Len(t, sent.Outcomes, 1)
			t.Logf("SEND %s", body)
			require.Equal(t, tc.want, sent.Outcomes[0].Outcome, "transport accept must not be called delivered: %s", body)
			require.Equal(t, tc.code, sent.Outcomes[0].Code)
			require.False(t, sent.Outcomes[0].Retryable)
			if tc.code == "PEER_CLAIMED_DELIVERY" {
				require.Contains(t, sent.Outcomes[0].Detail, "no reply observed")
			}
			if tc.answer != "" {
				require.Equal(t, 1, sent.Delivered)
			}
			var counters map[string]json.RawMessage
			require.NoError(t, json.Unmarshal([]byte(body), &counters))
			if tc.want == "accepted" || tc.want == "held" {
				require.JSONEq(t, "1", string(counters[tc.want]), "each receipt keeps its own summary counter")
				require.Zero(t, sent.Delivered)
			}
			status, body = a.do("GET", "/sessions/"+room+"/messages", "", nil)
			require.Equal(t, 200, status)
			var transcript struct {
				Messages []transcriptMessage `json:"messages"`
			}
			require.NoError(t, json.Unmarshal([]byte(body), &transcript))
			answers := 0
			if tc.answer != "" {
				answers = 1
			}
			require.Len(t, transcript.Messages, 1+answers, "only an observed reply creates an agent answer")
			if answers == 1 {
				bubble := transcript.Messages[1]
				require.Equal(t, "remote:peer:worker", bubble.Author.AgentID())
				require.Equal(t, transcript.Messages[0].ID, bubble.ParentID)
				require.Equal(t, transcript.Messages[0].ThreadID, bubble.ThreadID)
				require.Equal(t, string(MessagePlain), bubble.MessageKind)
				var payload struct {
					Text string `json:"text"`
				}
				require.NoError(t, json.Unmarshal(bubble.Payload, &payload))
				require.Equal(t, tc.answer, payload.Text)
				require.Empty(t, bubble.Outcomes)
				require.Equal(t, AudienceExplicit, bubble.Audience.Rule)
				require.Empty(t, bubble.Audience.Targets)
			}
			require.Equal(t, sent.Outcomes, transcript.Messages[0].Outcomes)
			require.NotContains(t, body, "do-not-persist")
			wire, err := json.Marshal(transcript.Messages[0].Outcomes)
			require.NoError(t, err)
			t.Logf("TRANSCRIPT %s", wire)
			require.EqualValues(t, 1, calls.Load(), "classification must not initiate another attempt")
		})
	}
}

type receiptDispatcher struct{ result registry.DeliveryResult }

func (d receiptDispatcher) Deliver(string, *registry.InboxEntry) error { panic("unguarded delivery") }
func (d receiptDispatcher) DispatchDelivery(context.Context, string, registry.DeliveryRequest) (registry.DeliveryResult, error) {
	return d.result, nil
}

func TestLocalTransportReceipt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, transport, mode, reply, want string
		answers                            int
	}{
		{"inbox_queue", "inbox", "", "", "accepted", 0},
		{"async", "webhook", "async", `{"reply":"not a blocking answer"}`, "accepted", 0},
		{"batch", "webhook", "batch", "", "accepted", 0},
		{"explicit_inbox_delivery", "inbox", "", `{"status":"delivered"}`, "accepted", 0},
		{"observed_inbox_reply", "inbox", "", `{"answer":"genuine answer"}`, "delivered", 1},
		{"genuine_blocking", "webhook", "blocking", `{"reply":"genuine answer"}`, "delivered", 1},
		{"empty_blocking", "webhook", "blocking", "", "unreadable_reply", 0},
		{"canned_blocking", "webhook", "blocking", `{"reply":"demo","canned":true}`, "canned", 0},
		{"failed_blocking", "webhook", "blocking", `{"reply":"failed","status":"FAILED"}`, "failed", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := receiptDispatcher{registry.DeliveryResult{Transport: tc.transport, DeliveryMode: tc.mode, Reply: json.RawMessage(tc.reply), StatusCode: 201}}
			m := &Message{ID: "probe", SessionID: "room", ThreadID: "probe", Payload: json.RawMessage(`{}`)}
			answers := 0
			outcomes, err := fanoutWithReplies(context.Background(), d, m, []string{"worker"}, nil, 0, "", func(string, json.RawMessage) error { answers++; return nil })
			require.NoError(t, err)
			require.Len(t, outcomes, 1)
			require.Equal(t, tc.want, string(outcomes[0].Outcome), "transport accept must not be called delivered")
			require.Equal(t, tc.answers, answers)
		})
	}
}
