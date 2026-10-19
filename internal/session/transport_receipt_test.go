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
	for _, tc := range []struct {
		name, body, inbound, verdict, want, code string
		status                                   int
	}{
		{name: "held", status: 202, body: `{"status":"held"}`, want: "held"},
		{name: "token_only", status: 201, body: `{}`, inbound: "FED_TOKEN_ONLY", want: "accepted", code: "FED_TOKEN_ONLY"},
		{name: "inbox_queue", status: 201, body: `{"id":"queued","transport":"inbox"}`, want: "accepted"},
		{name: "async", status: 202, body: `{}`, want: "accepted"},
		{name: "explicit_body_delivery", status: 201, body: `{"status":"delivered"}`, want: "delivered"},
		{name: "explicit_verdict_delivery", status: 201, body: `{}`, verdict: "delivered", want: "delivered"},
		{name: "policy_allow_is_not_delivery", status: 201, body: `{"verdict":"FED_ALLOWED"}`, want: "accepted"},
		{name: "token_only_body", status: 201, body: `{"verdict":"FED_TOKEN_ONLY","status":"delivered"}`, want: "accepted", code: "FED_TOKEN_ONLY"},
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
			require.Len(t, transcript.Messages, 1, "a receipt is not an agent answer")
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
	for _, tc := range []struct {
		name, transport, mode, reply, want string
		answers                            int
	}{
		{"inbox_queue", "inbox", "", "", "accepted", 0},
		{"async", "webhook", "async", `{"reply":"not a blocking answer"}`, "accepted", 0},
		{"batch", "webhook", "batch", "", "accepted", 0},
		{"explicit_inbox_delivery", "inbox", "", `{"status":"delivered"}`, "delivered", 0},
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
