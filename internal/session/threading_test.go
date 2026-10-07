package session

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// CR-CHAT-017 — sub-thread spawning and navigability, over the REAL route
// surface registered by registerSessionRoutes.
// ---------------------------------------------------------------------------

// branchOff posts a reply from fixAgentA that addresses a NON-member
// ("vortex") and asserts it spawned — the CR-CHAT-017 trigger.
func branchOff(t *testing.T, h *apiHarness, sessID, parentID string) transcriptMessage {
	t.Helper()
	var child transcriptMessage
	code := h.do(t, http.MethodPost, "/sessions/"+sessID+"/messages", postMessageRequest{
		Payload:  mustJSONRaw(map[string]any{"text": "@vortex can you take this side question?"}),
		Sender:   fixAgentA,
		ParentID: parentID,
		Targets:  []AudienceTarget{{Kind: TargetAgent, ID: "vortex"}},
	}, &child, nil)
	require.Equal(t, http.StatusCreated, code, "branch send")
	require.NotNil(t, child.SpawnedThread, "addressing a non-member must spawn a sub-thread")
	return child
}

// TestSubThreadSpawnOnNonMemberAddress: a reply that addresses an agent NOT
// in the session's participants spawns a child thread (parent_thread_id +
// anchor_message_id), the branching message becomes the child's root, and the
// send reports the spawn.
func TestSubThreadSpawnOnNonMemberAddress(t *testing.T) {
	for _, be := range localBackends() {
		t.Run(be.name, func(t *testing.T) {
			store := be.open(t, be.pathFn(t))
			h := newAPIHarness(t, store)
			sessID := h.createRoom(t)

			var root transcriptMessage
			require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions/"+sessID+"/messages", postMessageRequest{
				Payload: mustJSONRaw(map[string]any{"text": "root message"}),
				Sender:  fixAgentA,
			}, &root, nil))

			child := branchOff(t, h, sessID, root.ID)
			spawn := child.SpawnedThread

			// The child thread's key IS the branching message's id (§4.3),
			// the parent is the thread the reply was headed for, and the
			// anchor is the message the branch hangs from.
			require.Equal(t, child.ID, spawn.ThreadID)
			require.Equal(t, root.ThreadID, spawn.ParentThreadID)
			require.Equal(t, root.ID, spawn.AnchorMessageID)
			require.Equal(t, []string{"vortex"}, spawn.NonMembers)
			require.Equal(t, 1, spawn.Depth)

			// The message was recorded AS the child's root: no parent_id.
			require.Equal(t, "", child.ParentID)
			require.Equal(t, spawn.ThreadID, child.ThreadID)

			// Re-read from storage: durable and reconstructable.
			var tr transcriptResponse
			code := h.do(t, http.MethodGet, "/sessions/"+sessID+"/messages", nil, &tr, nil)
			require.Equal(t, http.StatusOK, code)
			var recorded *transcriptMessage
			for i := range tr.Messages {
				if tr.Messages[i].ID == child.ID {
					recorded = &tr.Messages[i]
				}
			}
			require.NotNil(t, recorded)
			require.Equal(t, spawn.ThreadID, recorded.ThreadID)
			require.Equal(t, 1, recorded.ThreadDepth, "child of a root thread is depth 1")
		})
	}
}

// TestParentThreadUnchangedApartFromAnchor: the spawn leaves the parent
// thread untouched apart from the anchor the branch record names — its root
// message id is unchanged, its messages stay in it, and the branch adds no
// message to the parent's transcript.
func TestParentThreadUnchangedApartFromAnchor(t *testing.T) {
	store := NewJSONLStoreForTest(t)
	h := newAPIHarness(t, store)
	sessID := h.createRoom(t)

	// Thread root + one member reply first.
	var root transcriptMessage
	require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions/"+sessID+"/messages", postMessageRequest{
		Payload: mustJSONRaw(map[string]any{"text": "root message"}),
		Sender:  fixAgentA,
	}, &root, nil))
	var reply transcriptMessage
	require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions/"+sessID+"/messages", postMessageRequest{
		Payload:  mustJSONRaw(map[string]any{"text": "member reply"}),
		Sender:   fixAgentB,
		ParentID: root.ID,
	}, &reply, nil))

	child := branchOff(t, h, sessID, reply.ID)
	spawn := child.SpawnedThread

	// The parent thread still holds exactly its two original messages.
	var after transcriptResponse
	require.Equal(t, http.StatusOK, h.do(t, http.MethodGet, "/sessions/"+sessID+"/messages", nil, &after, nil))
	parentMsgs := 0
	for _, m := range after.Messages {
		if m.ThreadID == root.ThreadID {
			parentMsgs++
		}
	}
	require.Equal(t, 2, parentMsgs, "parent thread message count changed by the spawn")

	// The parent's row is unchanged; the branch (with its anchor) lives on
	// the CHILD's row.
	var read threadReadResponse
	code := h.do(t, http.MethodGet, "/sessions/"+sessID+"/threads/"+root.ThreadID, nil, &read, nil)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, root.ThreadID, read.Thread.RootMessageID)
	require.Empty(t, read.Thread.AnchorMessageID, "a root thread never gains an anchor on its own row")
	require.Len(t, read.Branches, 1, "the spawned thread appears as the parent's branch")
	require.Equal(t, spawn.ThreadID, read.Branches[0].Thread.ID)
	require.Equal(t, spawn.AnchorMessageID, read.Branches[0].Thread.AnchorMessageID)
	require.Equal(t, spawn.ParentThreadID, read.Branches[0].Thread.ParentThreadID)
}

// TestReplyToParticipantStaysInThread: a reply whose addressees are all
// participants spawns NOTHING (D11 — a reply stays in thread).
func TestReplyToParticipantStaysInThread(t *testing.T) {
	store := NewJSONLStoreForTest(t)
	h := newAPIHarness(t, store)
	sessID := h.createRoom(t)

	var root transcriptMessage
	require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions/"+sessID+"/messages", postMessageRequest{
		Payload: mustJSONRaw(map[string]any{"text": "root"}),
		Sender:  fixAgentA,
	}, &root, nil))

	var reply transcriptMessage
	code := h.do(t, http.MethodPost, "/sessions/"+sessID+"/messages", postMessageRequest{
		Payload:  mustJSONRaw(map[string]any{"text": "@nimbus what do you think?"}),
		Sender:   fixAgentA,
		ParentID: root.ID,
		Targets:  []AudienceTarget{{Kind: TargetAgent, ID: fixAgentB}},
	}, &reply, nil)
	require.Equal(t, http.StatusCreated, code)
	require.Nil(t, reply.SpawnedThread, "a reply to a participant must not spawn")
	require.Equal(t, root.ThreadID, reply.ThreadID, "the reply stays in its thread")
	require.Equal(t, root.ID, reply.ParentID, "the reply keeps its reply attribution")

	// A fresh top-level message addressed to a member also spawns nothing.
	var plain transcriptMessage
	code = h.do(t, http.MethodPost, "/sessions/"+sessID+"/messages", postMessageRequest{
		Payload: mustJSONRaw(map[string]any{"text": "@nimbus still the same thread"}),
		Sender:  fixAgentA,
		Targets: []AudienceTarget{{Kind: TargetAgent, ID: fixAgentB}},
	}, &plain, nil)
	require.Equal(t, http.StatusCreated, code)
	require.Nil(t, plain.SpawnedThread)

	// The session still has exactly one thread.
	var read threadReadResponse
	require.Equal(t, http.StatusOK, h.do(t, http.MethodGet, "/sessions/"+sessID+"/threads/"+root.ThreadID, nil, &read, nil))
	require.Empty(t, read.Branches)
}

// TestDepthCollapseThreshold: GET /threads/{id}?depth=N returns children
// beyond N as collapsed:true summary cards, and on-screen below it.
func TestDepthCollapseThreshold(t *testing.T) {
	store := NewJSONLStoreForTest(t)
	h := newAPIHarness(t, store)
	sessID := h.createRoom(t)

	var root transcriptMessage
	require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions/"+sessID+"/messages", postMessageRequest{
		Payload: mustJSONRaw(map[string]any{"text": "root"}),
		Sender:  fixAgentA,
	}, &root, nil))
	child := branchOff(t, h, sessID, root.ID)
	spawn := child.SpawnedThread

	// depth=0: the child (beyond depth 0) is COLLAPSED into a summary card.
	var collapsed threadReadResponse
	code := h.do(t, http.MethodGet, "/sessions/"+sessID+"/threads/"+root.ThreadID+"?depth=0", nil, &collapsed, nil)
	require.Equal(t, http.StatusOK, code)
	require.Empty(t, collapsed.Branches, "nothing on-screen beyond depth 0")
	require.Len(t, collapsed.Collapsed, 1)
	card := collapsed.Collapsed[0]
	require.Equal(t, spawn.ThreadID, card.ThreadID)
	require.True(t, card.Collapsed)
	require.True(t, card.Summary.Generated)
	require.Equal(t, 1, card.Summary.Depth)
	require.Equal(t, 1, card.Summary.MessageCount)
	require.Equal(t, root.ID, card.Summary.AnchorMessageID)

	// The default (depth=3): the child is ON-SCREEN, not collapsed.
	var expanded threadReadResponse
	code = h.do(t, http.MethodGet, "/sessions/"+sessID+"/threads/"+root.ThreadID, nil, &expanded, nil)
	require.Equal(t, http.StatusOK, code)
	require.Len(t, expanded.Branches, 1)
	require.Equal(t, spawn.ThreadID, expanded.Branches[0].Thread.ID)
	require.Empty(t, expanded.Collapsed)

	// depth=1: the child sits AT the limit, so it collapses (strictly-below
	// rendering) — exactly the boundary case.
	var d1 threadReadResponse
	code = h.do(t, http.MethodGet, "/sessions/"+sessID+"/threads/"+root.ThreadID+"?depth=1", nil, &d1, nil)
	require.Equal(t, http.StatusOK, code)
	require.Empty(t, d1.Branches, "a branch at the limit is not rendered on-screen")
	require.Len(t, d1.Collapsed, 1)
	require.Equal(t, spawn.ThreadID, d1.Collapsed[0].ThreadID)

	// An invalid depth is refused, not clamped.
	code = h.do(t, http.MethodGet, "/sessions/"+sessID+"/threads/"+root.ThreadID+"?depth=-2", nil, nil, nil)
	require.Equal(t, http.StatusBadRequest, code)

	// An unknown thread is a 404, not an empty tree.
	code = h.do(t, http.MethodGet, "/sessions/"+sessID+"/threads/nope", nil, nil, nil)
	require.Equal(t, http.StatusNotFound, code)
}

// TestTimelineAndSummary: the timeline returns the branch tree with the
// current position, and the summary returns a generated index with a link to
// the raw messages.
func TestTimelineAndSummary(t *testing.T) {
	store := NewJSONLStoreForTest(t)
	h := newAPIHarness(t, store)
	sessID := h.createRoom(t)

	var root transcriptMessage
	require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions/"+sessID+"/messages", postMessageRequest{
		Payload: mustJSONRaw(map[string]any{"text": "root"}),
		Sender:  fixAgentA,
	}, &root, nil))
	child := branchOff(t, h, sessID, root.ID)

	// Timeline: two nodes, child nested under the root, current = the child
	// (it holds the session's latest message).
	var tl timelineResponse
	code := h.do(t, http.MethodGet, "/sessions/"+sessID+"/threads/"+root.ThreadID+"/timeline", nil, &tl, nil)
	require.Equal(t, http.StatusOK, code)
	require.Len(t, tl.Nodes, 1)
	rail := tl.Nodes[0]
	require.Equal(t, root.ThreadID, rail.ThreadID)
	require.Equal(t, 0, rail.Depth)
	require.Len(t, rail.Children, 1)
	require.Equal(t, child.SpawnedThread.ThreadID, rail.Children[0].ThreadID)
	require.Equal(t, 1, rail.Children[0].Depth)
	require.Equal(t, root.ID, rail.Children[0].AnchorMessageID)
	require.True(t, rail.Children[0].Current, "the sub-thread with the latest message is current")
	require.False(t, rail.Current)
	require.Equal(t, child.SpawnedThread.ThreadID, tl.Current)

	// Summary: generated index + the raw-messages link.
	var sum threadSummaryResponse
	code = h.do(t, http.MethodGet, "/sessions/"+sessID+"/threads/"+child.SpawnedThread.ThreadID+"/summary", nil, &sum, nil)
	require.Equal(t, http.StatusOK, code)
	require.True(t, sum.Generated)
	require.True(t, sum.Summary.Generated)
	require.Equal(t, 1, sum.Summary.MessageCount)
	require.Contains(t, sum.RawMessages, "/sessions/"+sessID+"/messages")

	// Timeline/summary of an unknown thread: 404.
	code = h.do(t, http.MethodGet, "/sessions/"+sessID+"/threads/nope/timeline", nil, nil, nil)
	require.Equal(t, http.StatusNotFound, code)
	code = h.do(t, http.MethodGet, "/sessions/"+sessID+"/threads/nope/summary", nil, nil, nil)
	require.Equal(t, http.StatusNotFound, code)
}

// TestSearchReturnsFullPath: a search hit carries
// full_path [namespace, channel, thread_id, parent_thread_id?, message_id] —
// the parent id present for a sub-thread hit.
func TestSearchReturnsFullPath(t *testing.T) {
	store := NewJSONLStoreForTest(t)
	h := newAPIHarness(t, store)
	sessID := h.createRoom(t)

	var root transcriptMessage
	require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions/"+sessID+"/messages", postMessageRequest{
		Payload: mustJSONRaw(map[string]any{"text": "the launcher manifest is wrong"}),
		Sender:  fixAgentA,
	}, &root, nil))
	var child transcriptMessage
	require.Equal(t, http.StatusCreated, h.do(t, http.MethodPost, "/sessions/"+sessID+"/messages", postMessageRequest{
		Payload:  mustJSONRaw(map[string]any{"text": "@vortex the launcher manifest is wrong here too"}),
		Sender:   fixAgentA,
		ParentID: root.ID,
		Targets:  []AudienceTarget{{Kind: TargetAgent, ID: "vortex"}},
	}, &child, nil))
	require.NotNil(t, child.SpawnedThread)

	var res searchResponse
	code := h.do(t, http.MethodGet, "/sessions/"+sessID+"/search?q=manifest", nil, &res, nil)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, 2, res.Count)

	byMsg := map[string]searchHit{}
	for _, hit := range res.Hits {
		byMsg[hit.MessageID] = hit
	}
	rootHit, ok := byMsg[root.ID]
	require.True(t, ok)
	// Root hit: [namespace, channel, thread, message] — no parent segment.
	require.Equal(t, []string{h.namespace(), "build-plan", root.ThreadID, root.ID}, rootHit.FullPath)

	childHit, ok := byMsg[child.ID]
	require.True(t, ok)
	// Sub-thread hit: the parent thread id sits between thread and message.
	require.Equal(t, []string{h.namespace(), "build-plan", child.ThreadID, root.ThreadID, child.ID}, childHit.FullPath)

	// A query that matches nothing: an empty, non-nil hit list.
	var empty searchResponse
	code = h.do(t, http.MethodGet, "/sessions/"+sessID+"/search?q=zzz-nothing", nil, &empty, nil)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, 0, empty.Count)

	// An empty query is refused.
	code = h.do(t, http.MethodGet, "/sessions/"+sessID+"/search?q=", nil, nil, nil)
	require.Equal(t, http.StatusBadRequest, code)
}

// mustJSONRaw marshals v, panicking on failure (test helper).
func mustJSONRaw(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return raw
}

// namespace reads the default realm the harness serves ("" for the unset
// X-Crier-Namespace header — sessions created without a namespace field).
func (h *apiHarness) namespace() string {
	return strings.TrimSpace("")
}
