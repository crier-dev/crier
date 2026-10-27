package session

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/registry"
	"github.com/stretchr/testify/require"
)

func TestSessionParticipantRejectsUnknownAgent(t *testing.T) {
	t.Parallel()
	for _, be := range localBackends() {
		t.Run(be.name, func(t *testing.T) {
			store := be.open(t, be.pathFn(t))
			t.Cleanup(func() { _ = store.Close() })
			h := newAPIHarness(t, store)
			room := h.createRoom(t)
			var body map[string]any
			require.Equal(t, http.StatusNotFound, h.do(t, http.MethodPost, "/sessions/"+room+"/participants", addParticipantRequest{MemberType: "agent", MemberID: "operator", Actor: fixAgentA}, &body, nil))
			require.Equal(t, "AGENT_NOT_FOUND", body["error"])
			require.Contains(t, body["detail"], `"operator"`)
			st, err := store.Load(context.Background(), room)
			require.NoError(t, err)
			require.Len(t, st.Members, 3, "rejected membership must not be persisted")
			require.Equal(t, http.StatusNotFound, h.do(t, http.MethodPost, "/sessions", createSessionRequest{ID: "invalid-initial", CreatedBy: &AuthorRef{Principal: "human"}, Members: []addParticipantRequest{{MemberType: "agent", MemberID: "operator"}}}, &body, nil))
			_, err = store.Load(context.Background(), "invalid-initial")
			require.ErrorIs(t, err, ErrSessionNotFound, "reject initial members before creating a partial room")
		})
	}
}

type failingAgentLookup struct{ err error }

func (l failingAgentLookup) Get(string) (*registry.Agent, error) { return nil, l.err }
func TestSessionParticipantRegistryFailureIsNotMissing(t *testing.T) {
	t.Parallel()
	for _, lookup := range []AgentLookup{nil, failingAgentLookup{errors.New("registry unavailable")}, failingAgentLookup{nil}} {
		h := NewHTTPHandler(NewJSONLStoreForTest(t), HTTPOptions{Agents: lookup})
		_, err := h.addOrReaddMember(context.Background(), &Session{ID: "room"}, addParticipantRequest{MemberType: "agent", MemberID: "operator"}, "human", SenderIdentityClaimed)
		require.Equal(t, http.StatusServiceUnavailable, statusForMemberError(err))
		require.Equal(t, "AGENT_LOOKUP_UNAVAILABLE", codeForMemberError(err))
	}
}

func TestSessionLegacyMemberSendExcludesAuthorAndKeepsPartialSuccess(t *testing.T) {
	t.Parallel()
	for _, be := range localBackends() {
		t.Run(be.name, func(t *testing.T) {
			store := be.open(t, be.pathFn(t))
			t.Cleanup(func() { _ = store.Close() })
			h := newAPIHarness(t, store)
			room := h.createRoom(t)
			// Model old persisted rows without going through the new admission check.
			for _, id := range []string{"operator", "ghost"} {
				seq, err := store.NextSeq(context.Background(), room)
				require.NoError(t, err)
				require.NoError(t, store.AddMember(context.Background(), &Member{SessionID: room, MemberType: MemberAgent, MemberID: id, Role: RoleMember, AddedAt: time.Now().UTC()}, seq, nil))
			}
			var msg transcriptMessage
			require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions/"+room+"/messages", postMessageRequest{Sender: "operator", Payload: json.RawMessage(`{"text":"hello"}`)}, &msg, nil))
			require.Equal(t, 2, msg.Accepted)
			require.Zero(t, msg.Delivered)
			require.Equal(t, 1, msg.Skipped)
			require.Equal(t, []string{"ghost"}, msg.SkippedAgents)
			for _, o := range msg.Outcomes {
				require.NotEqual(t, "operator", o.Target)
				if o.Target == "ghost" {
					require.Equal(t, "refused", o.Outcome)
					require.Contains(t, o.Detail, "no such agent")
				} else {
					require.Equal(t, "accepted", o.Outcome)
				}
			}
			entries, _, err := h.reg.Retrieve("nimbus", time.Minute, 10)
			require.NoError(t, err)
			require.Len(t, entries, 1)
			require.Equal(t, msg.ID, entries[0].ID[:len(msg.ID)])
		})
	}
}

func TestSessionExplicitAudienceExcludesAuthor(t *testing.T) {
	t.Parallel()
	store := NewJSONLStoreForTest(t)
	h := newAPIHarness(t, store)
	room := h.createRoom(t)
	var msg transcriptMessage
	require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions/"+room+"/messages", postMessageRequest{Sender: "atlas", Payload: json.RawMessage(`{"text":"hello"}`), Targets: []AudienceTarget{{Kind: TargetAgent, ID: "atlas"}, {Kind: TargetAgent, ID: "nimbus"}}}, &msg, nil))
	require.Equal(t, []AudienceTarget{{Kind: TargetAgent, ID: "nimbus"}}, msg.Audience.Targets)
	require.Len(t, msg.Outcomes, 1)
	require.Equal(t, "nimbus", msg.Outcomes[0].Target)
	entries, _, err := h.reg.Retrieve("atlas", time.Minute, 10)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestSessionFanoutNeverDeliversToAuthor(t *testing.T) {
	t.Parallel()
	reg := registry.NewMemoryStore()
	require.NoError(t, reg.Register(&registry.Agent{ID: "atlas"}))
	require.NoError(t, reg.Register(&registry.Agent{ID: "nimbus"}))
	for _, retry := range []bool{false, true} {
		msg := &Message{ID: "m", SessionID: "room", ThreadID: "m", Author: AuthorRef{Principal: "human", AsAgent: "atlas"}, Payload: json.RawMessage(`{"text":"hello"}`), Outcomes: []DeliveryOutcome{{Target: "atlas", Outcome: OutcomeRefused, UpdatedAt: time.Now()}}}
		outcomes, err := fanoutAttemptWithReplies(context.Background(), reg, msg, []string{"atlas", "nimbus"}, nil, 0, "", retry, nil)
		require.NoError(t, err)
		require.Len(t, outcomes, 1)
		require.Equal(t, "nimbus", outcomes[0].Target)
	}
	entries, _, err := reg.Retrieve("atlas", time.Minute, 10)
	require.NoError(t, err)
	require.Empty(t, entries)
}
