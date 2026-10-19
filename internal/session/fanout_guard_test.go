package session

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/stretchr/testify/require"

	"github.com/crier-dev/crier/internal/registry"
)

// Deliberately exposes only the legacy store seam, not DispatchDelivery.
// If fan-out ever falls back to Deliver again, this writes a real inbox row.
type unguardedFanoutDeliverer struct {
	*registry.MemoryStore
	calls atomic.Int32
}

func (d *unguardedFanoutDeliverer) Deliver(target string, entry *registry.InboxEntry) error {
	d.calls.Add(1)
	return d.MemoryStore.Deliver(target, entry)
}

func captureFanoutWarnings(t *testing.T) *bytes.Buffer {
	t.Helper()
	var log bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&log, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &log
}

func TestFanoutRefusesUnguardedDeliverer(t *testing.T) {
	log := captureFanoutWarnings(t)
	d := &unguardedFanoutDeliverer{MemoryStore: newFanoutFixture(t, "receiver")}
	msg := &Message{ID: "message", SessionID: "session", ThreadID: "message", Kind: MessagePlain,
		Author: AuthorRef{Agent: "sender"}, Payload: json.RawMessage(`{"text":"must not arrive"}`)}
	var replies int
	outcomes, err := fanoutWithReplies(context.Background(), d, msg, []string{"receiver", "receiver"}, nil, 250, "private-token",
		func(string, json.RawMessage) error { replies++; return nil })
	require.NoError(t, err)
	depth, leased, _, err := d.Stats("receiver")
	require.NoError(t, err)
	t.Logf("plain Deliver calls=%d inbox rows=%d outcomes=%+v", d.calls.Load(), depth+leased, outcomes)
	require.Zero(t, d.calls.Load(), "an unavailable guarded dispatcher must never fall back to plain Deliver")
	require.Zero(t, depth+leased)
	require.Zero(t, replies, "a refusal cannot produce an agent answer bubble")
	require.Len(t, outcomes, 1)
	require.Equal(t, OutcomeRefused, outcomes[0].Outcome)
	require.Equal(t, "DELIVERY_UNGUARDED_UNAVAILABLE", outcomes[0].Code)
	require.False(t, outcomes[0].Retryable)
	require.Empty(t, outcomes[0].InboxEntryID)
	require.NotEmpty(t, outcomes[0].Reason)
	require.Equal(t, outcomes, msg.Outcomes)
	require.Equal(t, 1, strings.Count(log.String(), `"level":"WARN"`))
	require.Contains(t, log.String(), `"code":"DELIVERY_UNGUARDED_UNAVAILABLE"`)
	require.Contains(t, log.String(), `"target":"receiver"`)
	require.Contains(t, log.String(), `"message_id":"message"`)
	require.NotContains(t, log.String(), "must not arrive")
	require.NotContains(t, log.String(), "private-token")
	_, err = Fanout(context.Background(), d, msg, []string{"receiver"}, nil)
	require.NoError(t, err)
	require.Equal(t, 1, strings.Count(log.String(), `"level":"WARN"`), "pure replay must not log a second delivery")
}

func TestSessionTranscriptGuardedDeliveryBoundary(t *testing.T) {
	for _, backend := range localBackends() {
		for _, guarded := range []bool{false, true} {
			name := "unguarded"
			if guarded {
				name = "guarded"
			}
			t.Run(backend.name+"/"+name, func(t *testing.T) {
				log := captureFanoutWarnings(t)
				path := backend.pathFn(t)
				repo := backend.open(t, path)
				h := newAPIHarness(t, repo)
				room := h.createRoom(t)
				d := &unguardedFanoutDeliverer{MemoryStore: h.reg}
				var deliverer InboxDeliverer = d
				if guarded {
					deliverer = registry.NewSessionDeliverer(registry.NewHandler(d))
				}
				router := mux.NewRouter()
				registerSessionRoutes(router, NewHTTPHandler(repo, HTTPOptions{Deliverer: deliverer, Agents: h.reg}))
				srv := httptest.NewServer(router)
				t.Cleanup(srv.Close)
				api := &apiHarness{srv: srv, reg: h.reg}
				sent := api.postTo(t, room, postMessageRequest{Sender: "atlas", Payload: json.RawMessage(`{"text":"boundary test"}`),
					Targets: []AudienceTarget{{Kind: TargetAgent, ID: "nimbus"}}})
				entries, _, err := h.reg.Retrieve("nimbus", time.Minute, 10)
				require.NoError(t, err)
				t.Logf("plain Deliver calls=%d inbox rows=%d outcomes=%+v", d.calls.Load(), len(entries), sent.Outcomes)
				require.Len(t, sent.Outcomes, 1)
				if guarded {
					require.EqualValues(t, 1, d.calls.Load(), "guarded dispatch must still reach its store exactly once")
					require.Len(t, entries, 1)
					require.JSONEq(t, `{"text":"boundary test"}`, string(entries[0].Payload))
					require.Equal(t, "atlas", entries[0].Sender)
					require.Equal(t, string(OutcomeAccepted), sent.Outcomes[0].Outcome)
					require.Empty(t, sent.Outcomes[0].Code)
					require.Equal(t, 1, sent.Accepted)
					require.Zero(t, sent.Delivered)
					require.Zero(t, sent.Skipped)
					require.Empty(t, log.String())
				} else {
					require.Zero(t, d.calls.Load())
					require.Empty(t, entries)
					require.Equal(t, string(OutcomeRefused), sent.Outcomes[0].Outcome)
					require.Equal(t, "DELIVERY_UNGUARDED_UNAVAILABLE", sent.Outcomes[0].Code)
					require.False(t, sent.Outcomes[0].Retryable)
					require.Zero(t, sent.Delivered)
					require.Equal(t, 1, sent.Skipped)
					require.Equal(t, 1, strings.Count(log.String(), `"level":"WARN"`), "HTTP reporting must not duplicate the boundary warning")
				}
				var transcript transcriptResponse
				require.Equal(t, http.StatusOK, api.do(t, http.MethodGet, "/sessions/"+room+"/messages", nil, &transcript, nil))
				require.Len(t, transcript.Messages, 1, "no agent answer bubble may be fabricated")
				require.Equal(t, sent.Outcomes, transcript.Messages[0].Outcomes)
				require.NoError(t, repo.Close())
				reopened := backend.open(t, path)
				t.Cleanup(func() { _ = reopened.Close() })
				st, err := reopened.Load(context.Background(), room)
				require.NoError(t, err)
				require.Len(t, st.Messages, 1)
				require.Equal(t, sent.Outcomes, outcomeViews(st.Message(sent.ID).Outcomes), "the operator's outcome must survive reopen")
			})
		}
	}
}
