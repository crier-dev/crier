package session

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/registry"
	"github.com/crier-dev/crier/internal/webhook"
	"github.com/stretchr/testify/require"
)

// The in-progress exchange this fixture exercises is STRUCTURALLY slow, and the
// cost is not this test's assertions: a duplicate under a held key waits out
// registry.idempotencyClaimWait (2s) before it answers busy, and only then does
// Fanout return in_progress. MEASURED at HEAD 46c4afe on this host (16 CPUs,
// one-minute loadavg ~13): second Fanout 2.00-2.39 s, provisioning (store +
// harness + room) ~0.47 s — ~2.4-2.9 s per run, unchanged by this edit and
// unrelated to any product defect. Nothing here shortens that: the 2 s is a
// product constant, and lowering it would be changing product code.
//
// A FIXED 5 s deadline left barely 2x that measured cost, so a loaded host spent
// the whole budget on scheduling and the fixture failed with "context deadline
// exceeded" at the second Fanout (observed red at one-minute loadavg 27) — a
// harness limit, not a product defect. The budget is therefore DERIVED from the
// measurement instead of picked: 10x the measured cost, scaled by the host's
// runnable queue per CPU (a CPU-bound fixture's wall clock grows with it), and
// capped below `make test`'s -timeout 60s per-binary bound so an over-budget run
// reports THIS fixture's deadline clearly instead of dying as a suite-wide panic.
const (
	fanoutMeasuredCost   = 2400 * time.Millisecond // measured cost of this fixture's own path
	fanoutBudgetMultiple = 10                      // headroom over that measurement (=> 24s unloaded)
	fanoutBudgetCeiling  = 45 * time.Second        // stays under the 60s per-binary test timeout
)

// fanoutIdempotencyBudget derives this run's wall-clock budget and LOGS how it
// was derived, so a future failure states its own conditions rather than
// repeating an unexplained number.
func fanoutIdempotencyBudget(t *testing.T) time.Duration {
	t.Helper()
	cpus := runtime.NumCPU()
	load, factor := -1.0, 1.0
	if raw, err := os.ReadFile("/proc/loadavg"); err == nil {
		if fields := strings.Fields(string(raw)); len(fields) > 0 {
			if parsed, perr := strconv.ParseFloat(fields[0], 64); perr == nil {
				load = parsed
			}
		}
	}
	// A fixture whose wall clock is mostly scheduling grows with the runnable
	// queue per CPU; below that the host is not contended and the factor stays 1.
	if load >= 0 && cpus > 0 && load > float64(cpus) {
		factor = load / float64(cpus)
	}
	budget := time.Duration(float64(fanoutMeasuredCost) * fanoutBudgetMultiple * factor)
	if budget > fanoutBudgetCeiling {
		budget = fanoutBudgetCeiling
	}
	if load >= 0 {
		t.Logf("fanout idempotency budget: %s = measured %s x%d, loadavg=%.2f over %d CPUs (factor %.2f, ceiling %s)",
			budget, fanoutMeasuredCost, fanoutBudgetMultiple, load, cpus, factor, fanoutBudgetCeiling)
	} else {
		t.Logf("fanout idempotency budget: %s = measured %s x%d (loadavg unavailable, factor 1.00, ceiling %s)",
			budget, fanoutMeasuredCost, fanoutBudgetMultiple, fanoutBudgetCeiling)
	}
	return budget
}

// Contend on the REAL registry lock, not the session request lock (which waits
// and replays). Both fan-outs derive the same per-target delivery key.
func TestFanoutConcurrentIdempotencyNeverRefusesInProgress(t *testing.T) {
	t.Parallel()
	budget := fanoutIdempotencyBudget(t)
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	provisionStart := time.Now()
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
	t.Logf("fixture provisioning (store, harness, room) took %s of the %s budget", time.Since(provisionStart), budget)
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
	busyAt := time.Now()
	outcomes, err := Fanout(ctx, deliverer, msg, []string{"receiver"}, nil)
	busyCost := time.Since(busyAt)
	require.NoError(t, err)
	require.Len(t, outcomes, 1)
	require.Equal(t, Outcome("in_progress"), outcomes[0].Outcome, "a held same-key delivery is not a refusal")
	require.NotEqual(t, OutcomeRefused, outcomes[0].Outcome)
	require.NotContains(t, outcomes[0].Reason, `{"error"`)
	// The short-circuit itself: the second fan-out returned while the webhook is
	// STILL holding the first delivery (`release` is closed only by the unblock()
	// far below), so it neither waited for the holder nor slipped a second
	// delivery past the lock. One webhook call so far == nothing was re-sent.
	require.EqualValues(t, 1, calls.Load(), "the second fan-out must not reach the webhook while the first delivery is held")
	t.Logf("second fan-out answered in %s while the first delivery was still held; outcome=%s code=%s",
		busyCost, outcomes[0].Outcome, outcomes[0].Code)
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
	t.Parallel()
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
