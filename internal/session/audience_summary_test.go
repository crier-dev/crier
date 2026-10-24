package session

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// DF-CRIER-479 — a send whose resolved audience is EMPTY must SAY SO.
//
// The natural first ordering a new user runs is "create the session, then send
// before any member joins". The session default audience is "every active
// participant except the author", so that send legitimately resolves to nobody
// and answers 201 with empty targets/outcomes. Per-contract but unreadable: the
// caller could not see "delivered to 0 participants" without deriving it from
// participant_count themselves.
//
// The audience summary in dispatch_evidence (audience_rule +
// resolved_target_count) makes that outcome explicit in the 201 body. These
// tests assert the BODY — the wire field names and values — not just the status,
// on both the empty and the non-empty arm, so an additive change that silently
// dropped the summary fails here.
func TestSessionAudienceSummaryEmptyAudience(t *testing.T) {
	for _, backend := range localBackends() {
		t.Run(backend.name, func(t *testing.T) {
			store := backend.open(t, backend.pathFn(t))
			defer func() { require.NoError(t, store.Close()) }()
			h := newAPIHarness(t, store)

			// A room created with NO members: creating does not imply inviting.
			var created sessionView
			require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions", createSessionRequest{
				Title:     "empty-room",
				Kind:      string(KindChannel),
				CreatedBy: &AuthorRef{Principal: fixCreator},
			}, &created, nil))
			require.Equal(t, 0, created.ParticipantCount, "a fresh room has zero participants")
			require.Equal(t, 0, created.AudienceCount)

			req := postMessageRequest{
				Sender:         fixAgentA,
				Payload:        json.RawMessage(`{"text":"hello, anyone?"}`),
				IdempotencyKey: "empty-audience",
			}
			var body map[string]any
			require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions/"+created.ID+"/messages", req, &body, nil))

			// The resolved audience is the empty session default, on the wire.
			audience, ok := body["audience"].(map[string]any)
			require.True(t, ok, "the 201 body carries its audience: %v", body)
			require.Equal(t, "session", audience["rule"])
			require.Nil(t, audience["targets"], "an empty session audience resolves to no targets")
			require.NotContains(t, body, "outcomes", "no per-target outcome exists, so none is claimed")

			// The audience SUMMARY states zero deliveries explicitly.
			evidence, ok := body["dispatch_evidence"].([]any)
			require.True(t, ok, "the 201 body carries dispatch_evidence: %v", body)
			require.Len(t, evidence, 1)
			attempt, ok := evidence[0].(map[string]any)
			require.True(t, ok)
			require.Equal(t, "session", attempt["audience_rule"], "the rule that resolved the audience is named")
			require.EqualValues(t, 0, attempt["resolved_target_count"], "zero targets resolved: this send delivered to nobody")
			require.Empty(t, attempt["targets"])
			require.Empty(t, attempt["receipts"])
			require.Equal(t, true, attempt["outcomes_recorded"])
			require.NotEmpty(t, attempt["completed_at"], "a zero-target pass is COMPLETED, not unfinished")

			// The summary is durable: a keyed replay and a transcript read
			// report the same audience summary from storage.
			var replay map[string]any
			require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions/"+created.ID+"/messages", req, &replay, nil))
			require.Equal(t, "replayed", replay["replay_action"])
			require.Equal(t, evidenceString(replay), evidenceString(body))

			var transcript map[string]any
			require.Equal(t, http.StatusOK, h.do(t, http.MethodGet, "/sessions/"+created.ID+"/messages", nil, &transcript, nil))
			require.EqualValues(t, 1, transcript["count"])
			row := transcript["messages"].([]any)[0].(map[string]any)
			require.Equal(t, evidenceString(replay), evidenceString(row))

			// And nothing was put in anyone's inbox.
			for _, id := range []string{"atlas", "nimbus"} {
				entries, _, err := h.reg.Retrieve(id, 0, 10)
				require.NoError(t, err)
				require.Empty(t, entries, "an empty audience delivers to no inbox")
			}
		})
	}
}

// TestSessionAudienceSummaryNonEmptyAudience is the preserved-behaviour arm: an
// audience with real targets still reports the rule and the COUNT of resolved
// recipients (roster-expanded, author excluded), beside the per-target outcomes.
func TestSessionAudienceSummaryNonEmptyAudience(t *testing.T) {
	for _, backend := range localBackends() {
		t.Run(backend.name, func(t *testing.T) {
			store := backend.open(t, backend.pathFn(t))
			defer func() { require.NoError(t, store.Close()) }()
			h := newAPIHarness(t, store)
			// createRoom: agents atlas + nimbus, human observer kara.
			room := h.createRoom(t)

			// (a) the session default: one agent participant besides the author.
			// The human principal is recorded in the audience but has no inbox,
			// so the resolved DISPATCH count is 1.
			var sent map[string]any
			require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions/"+room+"/messages",
				postMessageRequest{Sender: fixAgentA, Payload: json.RawMessage(`{"text":"kickoff"}`)}, &sent, nil))
			attempt := sent["dispatch_evidence"].([]any)[0].(map[string]any)
			require.Equal(t, "session", attempt["audience_rule"])
			require.EqualValues(t, 1, attempt["resolved_target_count"], "one agent participant, author excluded")
			require.Equal(t, []any{"nimbus"}, attempt["targets"])
			require.EqualValues(t, 1, sent["accepted"], "the existing per-target outcome summary is unchanged")

			// (b) explicit targets: the rule says so, and the count follows.
			var explicit map[string]any
			require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions/"+room+"/messages",
				postMessageRequest{
					Sender:  fixAgentA,
					Payload: json.RawMessage(`{"text":"both of you"}`),
					Targets: []AudienceTarget{{Kind: TargetAgent, ID: "nimbus"}, {Kind: TargetAgent, ID: "nimbus-2"}},
				}, &explicit, nil))
			attempt = explicit["dispatch_evidence"].([]any)[0].(map[string]any)
			require.Equal(t, "explicit", attempt["audience_rule"])
			require.EqualValues(t, 2, attempt["resolved_target_count"])
			require.Equal(t, []any{"nimbus", "nimbus-2"}, attempt["targets"])
		})
	}
}

// evidenceString renders a body's dispatch_evidence as JSON so two responses can
// be compared byte-for-byte without depending on map iteration order.
func evidenceString(body map[string]any) string {
	raw, err := json.Marshal(body["dispatch_evidence"])
	if err != nil {
		return "<unmarshalable>"
	}
	return string(raw)
}
