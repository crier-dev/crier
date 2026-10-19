package session

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/registry"
	"github.com/crier-dev/crier/internal/webhook"
	"github.com/stretchr/testify/require"
)

// Contend on the REAL registry lock, not the session request lock (which waits
// and replays). Both fan-outs derive the same per-target delivery key.
func TestFanoutConcurrentIdempotencyNeverRefusesInProgress(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	var calls atomic.Int32
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			close(entered)
		}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_, _ = w.Write([]byte(`{"reply":"real answer","live":true}`))
	}))
	defer hook.Close()
	defer unblock()
	registryStore := registry.NewMemoryStore()
	require.NoError(t, registryStore.Register(&registry.Agent{ID: "receiver", Webhook: &webhook.Config{URL: hook.URL}}))
	dispatcher := registry.NewHandler(registryStore)
	dispatcher.SetWebhookDriver(webhook.NewDriver(webhook.NewClient(5*time.Second, nil), webhook.NewMemoryQueue(), webhook.DriverConfig{}))
	deliverer := registry.NewSessionDeliverer(dispatcher)
	repo := newTestSQLiteStore(t)
	h := newAPIHarness(t, repo)
	room := h.createRoom(t)
	msg := &Message{ID: "same-message", SessionID: room, ThreadID: "same-message", Seq: 1,
		Author: AuthorRef{Agent: "atlas"}, Kind: MessagePlain, Payload: json.RawMessage(`{"text":"hello"}`), CreatedAt: time.Now().UTC()}
	seq, err := repo.NextSeq(ctx, room)
	msg.Seq = seq
	require.NoError(t, err)
	first := *msg
	type result struct {
		outcomes []DeliveryOutcome
		err      error
	}
	done := make(chan result, 1)
	go func() {
		outcomes, err := Fanout(ctx, deliverer, &first, []string{"receiver"}, nil)
		done <- result{outcomes, err}
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("first delivery never reached webhook")
	}
	outcomes, err := Fanout(ctx, deliverer, msg, []string{"receiver"}, nil)
	require.NoError(t, err)
	require.Len(t, outcomes, 1)
	require.Equal(t, Outcome("in_progress"), outcomes[0].Outcome, "a held same-key delivery is not a refusal")
	require.NotEqual(t, OutcomeRefused, outcomes[0].Outcome)
	require.NotContains(t, outcomes[0].Reason, `{"error"`)
	require.NoError(t, repo.PostMessage(ctx, msg))
	var transcript transcriptResponse
	require.Equal(t, http.StatusOK, h.do(t, http.MethodGet, "/sessions/"+room+"/messages", nil, &transcript, nil))
	require.Len(t, transcript.Messages, 1)
	wire, err := json.Marshal(transcript.Messages[0].Outcomes[0])
	require.NoError(t, err)
	var diagnostic map[string]any
	require.NoError(t, json.Unmarshal(wire, &diagnostic))
	require.Equal(t, "in_progress", diagnostic["outcome"])
	require.Equal(t, "DELIVERY_IN_PROGRESS", diagnostic["code"])
	require.Equal(t, "receiver", diagnostic["target"])
	require.Equal(t, true, diagnostic["retryable"])
	require.Contains(t, diagnostic["detail"], "already in progress")
	var summary transcriptMessage
	summarizeFanout(&summary, outcomes)
	require.Zero(t, summary.Delivered)
	require.Zero(t, summary.Skipped)
	require.False(t, allRetryableOutcomes(outcomes), "retryable busy does not authorize a new send while work is in flight")
	unblock()
	winner := <-done
	require.NoError(t, winner.err)
	require.Equal(t, OutcomeDelivered, winner.outcomes[0].Outcome)
	require.EqualValues(t, 1, calls.Load())
	t.Logf("busy transcript outcome: %s", wire)
}

func TestOutcomeDiagnosticsSurviveReopen(t *testing.T) {
	for _, backend := range localBackends() {
		t.Run(backend.name, func(t *testing.T) {
			path := backend.pathFn(t)
			repo := backend.open(t, path)
			h := newAPIHarness(t, repo)
			room := h.createRoom(t)
			var outcomes []DeliveryOutcome
			require.NoError(t, json.Unmarshal([]byte(`[{"target":"receiver","outcome":"in_progress","code":"DELIVERY_IN_PROGRESS","retryable":true,"reason":"already in progress","updated_at":"2026-10-19T00:00:00Z"}]`), &outcomes))
			msg := &Message{ID: "diagnostic", SessionID: room, ThreadID: "diagnostic", Seq: 1, Author: AuthorRef{Agent: "atlas"}, Kind: MessagePlain, Outcomes: outcomes, CreatedAt: time.Now().UTC()}
			seq, err := repo.NextSeq(context.Background(), room)
			require.NoError(t, err)
			msg.Seq = seq
			require.NoError(t, repo.PostMessage(context.Background(), msg))
			require.NoError(t, repo.Close())
			reopened := backend.open(t, path)
			defer func() { _ = reopened.Close() }()
			st, err := reopened.Load(context.Background(), room)
			require.NoError(t, err)
			views := outcomeViews(st.Message(msg.ID).Outcomes)
			wire, err := json.Marshal(views)
			require.NoError(t, err)
			require.Contains(t, string(wire), `"code":"DELIVERY_IN_PROGRESS"`)
			require.Contains(t, string(wire), `"retryable":true`)
		})
	}
}
