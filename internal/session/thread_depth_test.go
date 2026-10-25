package session

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDeliberateBranchAndNonRootReplies(t *testing.T) {
	t.Parallel()
	for _, be := range localBackends() {
		t.Run(be.name, func(t *testing.T) {
			path := be.pathFn(t)
			store := be.open(t, path)
			h := newAPIHarness(t, store)
			id := h.createRoom(t)
			post := func(parent string, branch bool) transcriptMessage {
				t.Helper()
				var m transcriptMessage
				// Raw HTTP proves the explicit wire flag is decoded, not a helper-only seam.
				require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions/"+id+"/messages",
					map[string]any{"sender": fixAgentA, "payload": map[string]string{"text": "depth fixture"}, "parent_id": parent, "branch": branch}, &m, nil))
				return m
			}
			root := post("", false)
			r1 := post(root.ID, false)
			r2 := post(r1.ID, false)
			require.Equal(t, r1.ID, r2.ParentID)
			require.Equal(t, root.ThreadID, r2.ThreadID)
			require.Zero(t, r2.ThreadDepth, "attribution chains never add depth")
			parent := r2
			for depth := 1; depth <= DefaultCollapseDepth+1; depth++ {
				child := post(parent.ID, true)
				require.NotNil(t, child.SpawnedThread)
				require.Equal(t, parent.ID, child.SpawnedThread.AnchorMessageID)
				require.Equal(t, parent.ThreadID, child.SpawnedThread.ParentThreadID)
				require.Equal(t, depth, child.ThreadDepth)
				require.Equal(t, child.ID, child.ThreadID)
				require.Empty(t, child.ParentID, "branch roots retain attribution on the thread anchor, per D11")
				var tr transcriptResponse
				require.Equal(t, http.StatusOK, h.do(t, http.MethodGet, "/sessions/"+id+"/messages", nil, &tr, nil))
				require.Equal(t, parent.ID, tr.Messages[len(tr.Messages)-1].AnchorMessageID)
				parent = child
			}
			reopened := be.open(t, path)
			defer func() { _ = reopened.Close() }()
			reopenedAPI := newAPIHarness(t, reopened)
			var durable transcriptResponse
			require.Equal(t, http.StatusOK, reopenedAPI.do(t, http.MethodGet, "/sessions/"+id+"/messages", nil, &durable, nil))
			require.Equal(t, parent.ID, durable.Messages[len(durable.Messages)-1].ID)
			require.NotEmpty(t, durable.Messages[len(durable.Messages)-1].AnchorMessageID)
			require.Equal(t, DefaultCollapseDepth+1, durable.Messages[len(durable.Messages)-1].ThreadDepth)
			// Reject a branch with no anchor before it can append anything.
			var before, after transcriptResponse
			require.Equal(t, http.StatusOK, h.do(t, http.MethodGet, "/sessions/"+id+"/messages", nil, &before, nil))
			require.Equal(t, http.StatusBadRequest, h.do(t, http.MethodPost, "/sessions/"+id+"/messages",
				map[string]any{"sender": fixAgentA, "payload": map[string]string{"text": "invalid"}, "branch": true}, nil, nil))
			require.Equal(t, http.StatusOK, h.do(t, http.MethodGet, "/sessions/"+id+"/messages", nil, &after, nil))
			require.Equal(t, before.Count, after.Count)
		})
	}
}
