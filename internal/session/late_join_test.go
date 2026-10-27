package session

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// Exercise the public wire shape, not a Go request struct that could hide a
// dropped field. Every local durable backend must retain the same system row.
func TestLateJoinWire(t *testing.T) {
	t.Parallel()
	for _, be := range localBackends() {
		t.Run(be.name, func(t *testing.T) {
			store := be.open(t, be.pathFn(t))
			t.Cleanup(func() { _ = store.Close() })
			h := newAPIHarness(t, store)
			id := h.createRoom(t)
			first := postText(t, h, id, fixAgentA, "first")
			edge := postText(t, h, id, fixAgentB, "summary edge")
			other := h.createRoom(t)
			foreign := postText(t, h, other, fixAgentA, "not in this session")

			for _, tc := range []struct {
				name, mode, boundary, wantMode, wantBoundary string
				status                                       int
			}{
				{"default", "", "", "summary", edge, http.StatusCreated},
				{"since missing", "since", "", "", "", http.StatusBadRequest},
				{"since unknown", "since", "not-a-message", "", "", http.StatusBadRequest},
				{"since foreign", "since", foreign, "", "", http.StatusBadRequest},
				{"since", "since", first, "since", first, http.StatusCreated},
				{"full", "full", "", "full", edge, http.StatusCreated},
				{"none", "none", "", "none", "", http.StatusCreated},
				{"invalid", "everything", "", "", "", http.StatusBadRequest},
			} {
				t.Run(tc.name, func(t *testing.T) {
					mid := "human-" + tc.name
					body := map[string]any{"member_type": "principal", "member_id": mid, "actor": fixAgentA}
					if tc.mode != "" {
						body["context_share"] = tc.mode
					}
					if tc.boundary != "" {
						body["context_boundary_message_id"] = tc.boundary
					}
					var response map[string]any
					require.Equal(t, tc.status, h.do(t, http.MethodPost, "/sessions/"+id+"/participants", body, &response, nil), response)
					if tc.status == http.StatusBadRequest {
						require.NotEmpty(t, response["detail"])
						if tc.mode == "since" {
							require.Contains(t, response["detail"], "boundary")
						}
						st, err := store.Load(context.Background(), id)
						require.NoError(t, err)
						require.Nil(t, st.findMember(MemberPrincipal, mid), "invalid decision must not add a member")
						return
					}
					share := response["context_share"].(map[string]any)
					require.Equal(t, tc.wantMode, share["mode"])
					if tc.wantBoundary != "" {
						require.Equal(t, tc.wantBoundary, share["boundary_message_id"])
					} else {
						require.Empty(t, share["boundary_message_id"])
					}
					assertLateJoinEvent(t, h, id, mid, tc.wantMode, tc.wantBoundary, "")
				})
			}
			// Later context changes remain separately explainable in seq order.
			var changed map[string]any
			require.Equal(t, http.StatusOK, h.do(t, http.MethodPut, "/sessions/"+id+"/participants/principal/human-default/context",
				map[string]any{"mode": "since", "boundary_message_id": first, "actor": fixAgentA}, &changed, nil))
			assertLateJoinEvent(t, h, id, "human-default", "summary", edge, "")
			assertLateJoinEvent(t, h, id, "human-default", "since", first, "")
		})
	}
}

func assertLateJoinEvent(t *testing.T, h *apiHarness, id, member, mode, boundary, reason string) {
	t.Helper()
	var transcript struct {
		Events []struct {
			Type     string        `json:"type"`
			Seq      int64         `json:"seq"`
			MemberID string        `json:"member_id"`
			Share    *ContextShare `json:"context_share"`
			Text     string        `json:"text"`
			Reason   string        `json:"reason"`
		} `json:"system_events"`
	}
	require.Equal(t, http.StatusOK, h.do(t, http.MethodGet, "/sessions/"+id+"/messages", nil, &transcript, nil))
	var last int64
	for _, ev := range transcript.Events {
		require.Greater(t, ev.Seq, last)
		last = ev.Seq
		if ev.MemberID == member && ev.Share != nil && string(ev.Share.Mode) == mode {
			require.Equal(t, boundary, ev.Share.BoundaryMessageID)
			require.Contains(t, ev.Text, member)
			require.Contains(t, ev.Text, mode)
			if boundary != "" {
				require.Contains(t, ev.Text, boundary)
			}
			if reason != "" {
				require.Contains(t, ev.Reason, reason)
				require.Contains(t, ev.Text, reason)
			}
			return
		}
	}
	t.Fatalf("missing recorded system event for %s mode %s: %+v", member, mode, transcript.Events)
}

func TestLateJoinEmptySession(t *testing.T) {
	t.Parallel()
	for _, be := range localBackends() {
		t.Run(be.name, func(t *testing.T) {
			store := be.open(t, be.pathFn(t))
			t.Cleanup(func() { _ = store.Close() })
			h := newAPIHarness(t, store)
			id := h.createRoom(t)
			var member participantView
			require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions/"+id+"/participants",
				map[string]any{"member_type": "agent", "member_id": "nimbus-2", "actor": fixAgentA}, &member, nil))
			require.NotNil(t, member.ContextShare)
			require.Equal(t, ShareNone, member.ContextShare.Mode)
			require.Empty(t, member.ContextShare.BoundaryMessageID)
			assertLateJoinEvent(t, h, id, "nimbus-2", "none", "", "nothing to share")
			// Invalid since is not downgraded even in an empty session.
			var refusal map[string]string
			require.Equal(t, http.StatusBadRequest, h.do(t, http.MethodPost, "/sessions/"+id+"/participants",
				json.RawMessage(`{"member_type":"agent","member_id":"nimbus-3","actor":"atlas","context_share":"since"}`), &refusal, nil))
		})
	}
}
