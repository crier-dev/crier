package session

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/crier-dev/crier/internal/registry"
)

// A fully unavailable journal is not the same fault as PostMessage write-back.
type dispatchJournalFailure struct {
	Repository
	after bool
}

func (s *dispatchJournalFailure) Append(ctx context.Context, rec *Record) error {
	if n := len(rec.DispatchEvidence); n != 0 {
		if !s.after || rec.DispatchEvidence[n-1].CompletedAt != nil {
			return errors.New("injected dispatch journal outage")
		}
	}
	return s.Repository.Append(ctx, rec)
}

func TestDispatchEvidenceStorageFailureAcrossBackends(t *testing.T) {
	for _, backend := range localBackends() {
		for _, after := range []bool{false, true} {
			name := "before"
			if after {
				name = "after"
			}
			t.Run(backend.name+"/"+name, func(t *testing.T) {
				path := backend.pathFn(t)
				store := backend.open(t, path)
				reg := registry.NewMemoryStore()
				for _, agent := range []string{"atlas", "nimbus"} {
					require.NoError(t, reg.Register(&registry.Agent{ID: agent}))
				}
				real := registry.NewSessionDeliverer(registry.NewHandler(reg))
				d := &countedRealDispatcher{InboxDeliverer: real, dispatcher: real}
				h := outcomeFailureHarness(t, &dispatchJournalFailure{Repository: store, after: after}, reg, d)
				room := h.createRoom(t)
				req := postMessageRequest{Sender: "atlas", Payload: json.RawMessage(`{"text":"journal outage"}`), IdempotencyKey: "journal-outage", Targets: []AudienceTarget{{Kind: TargetAgent, ID: "nimbus"}}}
				var failed map[string]any
				require.Equal(t, http.StatusInternalServerError, h.do(t, http.MethodPost, "/sessions/"+room+"/messages", req, &failed, nil))
				require.Equal(t, "STORE_ERROR", failed["error"])
				detail := failed["detail"].(string)
				wantCalls := int32(0)
				if after {
					wantCalls = 1
					require.Contains(t, detail, "receipts cannot be guaranteed durable")
				} else {
					require.Contains(t, detail, "fan-out not started")
				}
				require.Equal(t, wantCalls, d.calls.Load())
				entries, _, err := reg.Retrieve("nimbus", 0, 10)
				require.NoError(t, err)
				require.Len(t, entries, int(wantCalls))
				h.srv.Close()
				require.NoError(t, store.Close())
				store = backend.open(t, path)
				defer func() { require.NoError(t, store.Close()) }()
				h = outcomeFailureHarness(t, store, reg, d)
				var retry transcriptMessage
				require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions/"+room+"/messages", req, &retry, nil))
				require.Equal(t, "replayed_outcome_unknown", retry.ReplayAction)
				require.True(t, retry.OutcomesIncomplete)
				require.Equal(t, wantCalls, d.calls.Load(), "unknown never grants speculative dispatch")
				if after {
					require.Len(t, retry.DispatchEvidence, 1)
					require.Nil(t, retry.DispatchEvidence[0].CompletedAt)
					require.Empty(t, retry.DispatchEvidence[0].Receipts)
				} else {
					require.Empty(t, retry.DispatchEvidence)
				}
			})
		}
	}
}

func TestOutcomeMissingNegativeRetryNeverRedispatches(t *testing.T) {
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
			seq, err := store.NextSeq(context.Background(), room)
			require.NoError(t, err)
			// A historical negative outcome legitimately authorizes the first retry.
			msg := &Message{ID: "old-negative", ThreadID: "old-negative", SessionID: room, Seq: seq,
				Kind: MessagePlain, Author: AuthorRef{Agent: "atlas"}, Payload: json.RawMessage(`{"text":"old intent"}`),
				Audience:       Audience{Rule: "explicit", Targets: []AudienceTarget{{Kind: TargetAgent, ID: "nimbus"}}},
				IdempotencyKey: "lost-outcomes", CreatedAt: time.Now().UTC(),
				Outcomes: []DeliveryOutcome{{Target: "nimbus", Outcome: OutcomeFailed, UpdatedAt: time.Now().UTC()}}}
			require.NoError(t, store.PostMessage(context.Background(), msg))
			req := postMessageRequest{Sender: "atlas", Payload: json.RawMessage(`{"text":"changed"}`), IdempotencyKey: "lost-outcomes"}
			var failed map[string]any
			require.Equal(t, http.StatusInternalServerError, h.do(t, http.MethodPost, "/sessions/"+room+"/messages", req, &failed, nil))
			require.EqualValues(t, 1, d.calls.Load())
			h.srv.Close()
			require.NoError(t, store.Close())
			store = backend.open(t, path)
			defer func() { require.NoError(t, store.Close()) }()
			h = outcomeFailureHarness(t, store, reg, d)
			var retry transcriptMessage
			require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions/"+room+"/messages", req, &retry, nil))
			require.Equal(t, "replayed_outcome_missing", retry.ReplayAction)
			require.True(t, retry.OutcomesIncomplete)
			require.Equal(t, string(OutcomeFailed), retry.Outcomes[0].Outcome, "old outcomes remain explicitly incomplete")
			require.Equal(t, OutcomeAccepted, retry.DispatchEvidence[0].Receipts[0].Outcome)
			require.EqualValues(t, 1, d.calls.Load(), "stale negative outcomes cannot authorize a third send")
		})
	}
}

func TestOutcomeLegacyUnknownAndZeroTargetReplay(t *testing.T) {
	for _, backend := range localBackends() {
		t.Run(backend.name, func(t *testing.T) {
			store := backend.open(t, backend.pathFn(t))
			defer func() { require.NoError(t, store.Close()) }()
			h := newAPIHarness(t, store)
			room := h.createRoom(t)
			seq, err := store.NextSeq(context.Background(), room)
			require.NoError(t, err)
			msg := &Message{ID: "legacy-intent", ThreadID: "legacy-intent", SessionID: room, Seq: seq,
				Kind: MessagePlain, Author: AuthorRef{Agent: "atlas"}, Payload: json.RawMessage(`{"text":"legacy"}`),
				Audience:       Audience{Rule: "explicit", Targets: []AudienceTarget{{Kind: TargetAgent, ID: "nimbus"}}},
				IdempotencyKey: "legacy", CreatedAt: time.Now().UTC()}
			require.NoError(t, store.PostMessage(context.Background(), msg))
			req := postMessageRequest{Sender: "atlas", Payload: json.RawMessage(`{"text":"changed"}`), IdempotencyKey: "legacy"}
			var replay transcriptMessage
			require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions/"+room+"/messages", req, &replay, nil))
			require.Equal(t, "replayed_outcome_unknown", replay.ReplayAction)
			require.True(t, replay.OutcomesIncomplete)
			entries, _, err := h.reg.Retrieve("nimbus", 0, 10)
			require.NoError(t, err)
			require.Empty(t, entries)

			req.IdempotencyKey = "zero-target"
			req.Targets = []AudienceTarget{{Kind: TargetAgent, ID: "atlas"}}
			var first transcriptMessage
			require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions/"+room+"/messages", req, &first, nil))
			require.False(t, first.OutcomesIncomplete)
			require.Empty(t, first.Outcomes)
			replay = transcriptMessage{}
			require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions/"+room+"/messages", req, &replay, nil))
			require.Equal(t, "replayed", replay.ReplayAction)
			require.False(t, replay.OutcomesIncomplete)
			require.Len(t, replay.DispatchEvidence, 1)
			require.Empty(t, replay.DispatchEvidence[0].Targets)
			require.Empty(t, replay.DispatchEvidence[0].Receipts)
			require.True(t, replay.DispatchEvidence[0].OutcomesRecorded)
			require.NotNil(t, replay.DispatchEvidence[0].CompletedAt)
		})
	}
}

func TestDispatchEvidenceReplayStates(t *testing.T) {
	now := time.Now().UTC()
	for _, tc := range []struct {
		name     string
		outcomes []DeliveryOutcome
		evidence []DispatchEvidence
		want     string
	}{
		{name: "legacy-empty", want: "replayed_outcome_unknown"},
		{name: "legacy-receipt", outcomes: []DeliveryOutcome{{Outcome: OutcomeAccepted}}, want: "replayed"},
		{name: "boundary-only", evidence: []DispatchEvidence{{StartedAt: now}}, want: "replayed_outcome_unknown"},
		{name: "stale-negative", outcomes: []DeliveryOutcome{{Outcome: OutcomeFailed}}, evidence: []DispatchEvidence{{StartedAt: now}}, want: "replayed_outcome_unknown"},
		{name: "completed-missing", evidence: []DispatchEvidence{{CompletedAt: &now, Targets: []string{"nimbus"}}}, want: "replayed_outcome_missing"},
		{name: "marker-cannot-hide-missing-outcomes", evidence: []DispatchEvidence{{CompletedAt: &now, Targets: []string{"nimbus"}, OutcomesRecorded: true}}, want: "replayed_outcome_missing"},
		{name: "proven-zero-target", evidence: []DispatchEvidence{{CompletedAt: &now, OutcomesRecorded: true}}, want: "replayed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &Message{IdempotencyKey: "K", Outcomes: tc.outcomes, DispatchEvidence: tc.evidence}
			require.Equal(t, tc.want, replayAction(m))
			require.Equal(t, tc.want != "replayed", messageOutcomesIncomplete(m))
		})
	}
}

type evidenceAppendProbe struct {
	Repository
	append func(context.Context, *Record) error
}

func (s evidenceAppendProbe) Append(ctx context.Context, rec *Record) error {
	return s.append(ctx, rec)
}

func TestDispatchEvidenceAppendOutlivesCanceledRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	store := evidenceAppendProbe{append: func(ctx context.Context, rec *Record) error {
		called = true
		require.NoError(t, ctx.Err())
		deadline, bounded := ctx.Deadline()
		require.True(t, bounded)
		require.LessOrEqual(t, time.Until(deadline), 5*time.Second)
		require.Empty(t, rec.Outcomes, "journal must not silently perform final write-back")
		require.Len(t, rec.DispatchEvidence, 1)
		return nil
	}}
	h := NewHTTPHandler(store, HTTPOptions{})
	msg := &Message{ID: "probe", Outcomes: []DeliveryOutcome{{Outcome: OutcomeAccepted}}, DispatchEvidence: []DispatchEvidence{{Attempt: 1}}}
	require.NoError(t, h.appendDispatchEvidence(ctx, msg, nil))
	require.True(t, called)
}

func TestFanoutPreservesReceiptsWhenAnswerAppendFails(t *testing.T) {
	d := receiptDispatcher{registry.DeliveryResult{Transport: "webhook", DeliveryMode: "blocking", Reply: json.RawMessage(`{"reply":"genuine answer"}`)}}
	msg := &Message{ID: "answer", SessionID: "room", ThreadID: "answer", Payload: json.RawMessage(`{}`)}
	outcomes, err := fanoutWithReplies(context.Background(), d, msg, []string{"nimbus"}, nil, 0, "", func(string, json.RawMessage) error {
		return errors.New("answer append failed")
	})
	require.ErrorContains(t, err, "answer append failed")
	require.Len(t, outcomes, 1)
	require.Equal(t, OutcomeDelivered, outcomes[0].Outcome)
}

func TestDispatchEvidenceProjectionAndConflict(t *testing.T) {
	now := time.Now().UTC()
	msg := &Message{ID: "journal", ThreadID: "journal", SessionID: "s", Seq: 1, Kind: MessagePlain,
		Author: AuthorRef{Agent: "atlas"}, Audience: Audience{Rule: "explicit", Targets: []AudienceTarget{}}, CreatedAt: now}
	intent := msg.Record()
	msg.DispatchEvidence = []DispatchEvidence{{Attempt: 1, StartedAt: now, Targets: []string{"a"}, Receipts: []DeliveryOutcome{}}}
	msg.DispatchEvidence[0].addReceipts([]DeliveryOutcome{{Target: "z", Outcome: OutcomeAccepted}, {Target: "a", Outcome: OutcomeHeld}})
	msg.DispatchEvidence[0].addReceipts([]DeliveryOutcome{{Target: "a", Outcome: OutcomeHeld}})
	require.Len(t, msg.DispatchEvidence[0].Receipts, 2)
	require.Equal(t, "a", msg.DispatchEvidence[0].Receipts[0].Target)
	rec := msg.Record()
	require.True(t, hasMessageRequestMetadata(rec), "even unkeyed internal send needs evidence on SQL")
	raw, err := marshalMessageRequest(rec)
	require.NoError(t, err)
	restored := &Message{ID: msg.ID}
	require.NoError(t, restoreMessageRequest([]*Message{restored}, msg.ID, "", string(raw)))
	require.Equal(t, msg.DispatchEvidence, restored.DispatchEvidence)
	require.Empty(t, Conflicts([]*Record{intent, rec}), "journal revisions are not intent conflicts")
	changed := *rec
	changed.Payload = json.RawMessage(`{"text":"different intent"}`)
	require.Len(t, Conflicts([]*Record{intent, &changed}), 1, "evidence does not hide an intent mutation")
}
