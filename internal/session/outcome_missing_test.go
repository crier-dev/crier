package session

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/gorilla/mux"
	"github.com/stretchr/testify/require"

	"github.com/crier-dev/crier/internal/registry"
)

// Fault only the outcome write-back, not the intent or dispatch journal.
type outcomeWriteFailure struct{ Repository }

func (s *outcomeWriteFailure) PostMessage(ctx context.Context, m *Message) error {
	if m.IdempotencyKey == "lost-outcomes" && len(m.Outcomes) != 0 {
		return errors.New("injected outcome write-back failure")
	}
	return s.Repository.PostMessage(ctx, m)
}

type countedRealDispatcher struct {
	InboxDeliverer
	dispatcher guardedInboxDispatcher
	calls      atomic.Int32
}

func (d *countedRealDispatcher) DispatchDelivery(ctx context.Context, target string, req registry.DeliveryRequest) (registry.DeliveryResult, error) {
	d.calls.Add(1)
	return d.dispatcher.DispatchDelivery(ctx, target, req)
}

func outcomeFailureHarness(t *testing.T, store Repository, reg *registry.MemoryStore, d InboxDeliverer) *apiHarness {
	t.Helper()
	router := mux.NewRouter()
	registerSessionRoutes(router, NewHTTPHandler(store, HTTPOptions{Deliverer: d, Agents: reg}))
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	return &apiHarness{srv: srv, reg: reg}
}

func TestOutcomeMissingReplayAcrossBackends(t *testing.T) {
	t.Parallel()
	for _, backend := range localBackends() {
		t.Run(backend.name, func(t *testing.T) {
			path := backend.pathFn(t)
			store := backend.open(t, path)
			reg := registry.NewMemoryStore()
			for _, agent := range []string{"atlas", "nimbus"} {
				require.NoError(t, reg.Register(&registry.Agent{ID: agent}))
			}
			real := registry.NewSessionDeliverer(registry.NewHandler(reg))
			d := &countedRealDispatcher{InboxDeliverer: real, dispatcher: real}
			h := outcomeFailureHarness(t, &outcomeWriteFailure{store}, reg, d)
			room := h.createRoom(t)
			req := postMessageRequest{Sender: "atlas", Payload: json.RawMessage(`{"text":"persist my receipt"}`), IdempotencyKey: "lost-outcomes", Targets: []AudienceTarget{{Kind: TargetAgent, ID: "nimbus"}}}
			var first map[string]any
			require.Equal(t, http.StatusInternalServerError, h.do(t, http.MethodPost, "/sessions/"+room+"/messages", req, &first, nil))
			require.Equal(t, "STORE_ERROR", first["error"])
			require.EqualValues(t, 1, d.calls.Load())
			entries, _, err := reg.Retrieve("nimbus", 0, 10)
			require.NoError(t, err)
			require.Len(t, entries, 1, "real inbox proves transport admission")
			st, err := store.Load(context.Background(), room)
			require.NoError(t, err)
			require.Len(t, st.Messages, 1)
			require.Empty(t, st.Messages[0].Outcomes)
			id, seq := st.Messages[0].ID, st.Messages[0].Seq
			h.srv.Close()
			require.NoError(t, store.Close())

			// A new handle and handler rule out a process-local receipt cache.
			store = backend.open(t, path)
			defer func() { require.NoError(t, store.Close()) }()
			h = outcomeFailureHarness(t, store, reg, d)
			var body json.RawMessage
			require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions/"+room+"/messages", req, &body, nil))
			var retry map[string]any
			require.NoError(t, json.Unmarshal(body, &retry))
			t.Logf("degraded retry JSON: %s", body)
			require.EqualValues(t, 1, d.calls.Load())
			require.Equal(t, id, retry["id"])
			require.EqualValues(t, seq, retry["seq"])
			require.Equal(t, "replayed_outcome_missing", retry["replay_action"])
			require.Equal(t, true, retry["outcomes_incomplete"])
			evidence := retry["dispatch_evidence"].([]any)
			require.Len(t, evidence, 1)
			attempt := evidence[0].(map[string]any)
			require.Equal(t, float64(1), attempt["attempt"])
			require.NotEmpty(t, attempt["completed_at"])
			receipts := attempt["receipts"].([]any)
			require.Len(t, receipts, 1)
			receipt := receipts[0].(map[string]any)
			require.Equal(t, "nimbus", receipt["target"])
			require.Equal(t, "accepted", receipt["outcome"])
			require.Equal(t, entries[0].ID, receipt["inbox_entry_id"])
			before, err := store.Records(context.Background(), room)
			require.NoError(t, err)
			var again map[string]any
			require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions/"+room+"/messages", req, &again, nil))
			require.Equal(t, "replayed_outcome_missing", again["replay_action"])
			require.EqualValues(t, 1, d.calls.Load())
			after, err := store.Records(context.Background(), room)
			require.NoError(t, err)
			require.Equal(t, before, after, "degraded replay is read-only")
			var transcript map[string]any
			require.Equal(t, http.StatusOK, h.do(t, http.MethodGet, "/sessions/"+room+"/messages", nil, &transcript, nil))
			messages := transcript["messages"].([]any)
			require.Len(t, messages, 1)
			row := messages[0].(map[string]any)
			require.Equal(t, true, row["outcomes_incomplete"])
			require.Equal(t, again["dispatch_evidence"], row["dispatch_evidence"])
			require.NotContains(t, row, "replay_action")
		})
	}
}
