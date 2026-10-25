package session

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gorilla/mux"

	"github.com/crier-dev/crier/internal/registry"
	"github.com/stretchr/testify/require"
)

func TestAllRetryableOutcomes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		outcomes []Outcome
		want     bool
	}{
		{"empty", nil, false},
		{"refused", []Outcome{OutcomeRefused}, true},
		{"failed", []Outcome{OutcomeFailed}, true},
		{"budget_cut", []Outcome{OutcomeBudgetCut}, true},
		{"cut_and_refused", []Outcome{OutcomeBudgetCut, OutcomeRefused}, true},
		{"cut_and_delivered", []Outcome{OutcomeBudgetCut, OutcomeDelivered}, false},
		{"cut_and_canned", []Outcome{OutcomeBudgetCut, OutcomeCanned}, false},
		{"mixed_negative", []Outcome{OutcomeRefused, OutcomeFailed}, true},
		{"accepted", []Outcome{OutcomeAccepted}, false},
		{"held", []Outcome{OutcomeHeld}, false},
		{"held_and_refused", []Outcome{OutcomeHeld, OutcomeRefused}, false},
		{"accepted_and_cut", []Outcome{OutcomeAccepted, OutcomeBudgetCut}, false},
		{"delivered", []Outcome{OutcomeDelivered}, false},
		{"mixed_success", []Outcome{OutcomeDelivered, OutcomeRefused}, false},
		{"canned", []Outcome{OutcomeCanned, OutcomeFailed}, false},
		{"incomplete", []Outcome{"", OutcomeRefused}, false},
		{"acked", []Outcome{OutcomeAcked}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var outcomes []DeliveryOutcome
			for _, o := range tc.outcomes {
				outcomes = append(outcomes, DeliveryOutcome{Outcome: o})
			}
			require.Equal(t, tc.want, allRetryableOutcomes(outcomes))
		})
	}
}

type retryRuntimeDeliverer struct {
	mu      sync.Mutex
	calls   []registry.DeliveryRequest
	targets []string
}

func (d *retryRuntimeDeliverer) Deliver(string, *registry.InboxEntry) error {
	panic("dispatcher must be used")
}
func (d *retryRuntimeDeliverer) DispatchDelivery(_ context.Context, target string, req registry.DeliveryRequest) (registry.DeliveryResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, req)
	d.targets = append(d.targets, target)
	reply := json.RawMessage(`{"status":"FAILED","error":"try again"}`)
	if len(d.calls) >= 3 {
		reply = json.RawMessage(`{"reply":"recovered"}`)
	}
	return registry.DeliveryResult{Transport: "webhook", DeliveryMode: "blocking", Reply: reply}, nil
}

// Reopen the repository/handler after each attempt: original intent, refreshed
// outcomes and per-attempt keys must survive independently of process caches.
func TestMessageRetryPersistsAcrossBackends(t *testing.T) {
	t.Parallel()
	for _, backend := range localBackends() {
		t.Run(backend.name, func(t *testing.T) {
			path := backend.pathFn(t)
			d := &retryRuntimeDeliverer{}
			var id, room string
			var seq int64
			for attempt := 0; attempt < 4; attempt++ {
				store := backend.open(t, path)
				reg := registry.NewMemoryStore()
				for _, agent := range []string{"atlas", "nimbus"} {
					require.NoError(t, reg.Register(&registry.Agent{ID: agent}))
				}
				router := mux.NewRouter()
				registerSessionRoutes(router, NewHTTPHandler(store, HTTPOptions{Deliverer: d, Agents: reg}))
				srv := httptest.NewServer(router)
				t.Cleanup(srv.Close)
				h := &apiHarness{srv: srv, reg: reg}
				if attempt == 0 {
					room = h.createRoom(t)
				}
				req := postMessageRequest{Sender: "atlas", Payload: json.RawMessage(`{"text":"original intent"}`),
					IdempotencyKey: "recover", Targets: []AudienceTarget{{Kind: TargetAgent, ID: "nimbus"}}}
				if attempt > 0 {
					req.Payload = json.RawMessage(`{"text":"changed retry must be ignored"}`)
					req.Targets = []AudienceTarget{{Kind: TargetAgent, ID: "stranger"}}
				}
				var view transcriptMessage
				code := h.do(t, http.MethodPost, "/sessions/"+room+"/messages", req, &view, nil)
				require.Equal(t, http.StatusCreated, code)
				if attempt == 0 {
					id, seq = view.ID, view.Seq
				}
				require.Equal(t, id, view.ID)
				require.Equal(t, seq, view.Seq)
				if attempt == 1 || attempt == 2 {
					require.Equal(t, "redelivered", view.ReplayAction)
				}
				if attempt == 3 {
					require.Equal(t, "replayed", view.ReplayAction)
				}
				st, err := store.Load(context.Background(), room)
				require.NoError(t, err)
				wantRows := 1
				wantOutcome := OutcomeFailed
				if attempt >= 2 {
					wantRows, wantOutcome = 2, OutcomeDelivered
				}
				require.Len(t, st.Messages, wantRows)
				require.Equal(t, wantOutcome, st.Message(id).Outcomes[0].Outcome)
				require.JSONEq(t, `{"text":"original intent"}`, string(st.Message(id).Payload))
				require.NoError(t, store.Close())
			}
			require.Len(t, d.calls, 3)
			for i, req := range d.calls {
				require.Equal(t, "nimbus", d.targets[i])
				require.Equal(t, id, req.RequestID)
				require.JSONEq(t, `{"text":"original intent"}`, string(req.Entry.Payload))
				if i > 0 {
					require.NotEqual(t, d.calls[i-1].Entry.IdempotencyKey, req.Entry.IdempotencyKey)
				}
			}
			t.Logf("backend=%s invocations=3 failed->failed->delivered->replay operator_rows=1", backend.name)
		})
	}
}
