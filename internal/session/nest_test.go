// Package session_test — CR-CHAT-015: nested threads + the request→thread flow.
//
// This file is the acceptance test of the interface (specs/CHAT-INTERFACE.md
// §1.4), stated on ONE project with ONE agent exactly as Bane stated it:
//
//  1. two features are discussed in two PARALLEL threads with no cross-talk;
//  2. the top level shows the feature conversations distinctly;
//  3. a request becomes a thread and the replies live inside it;
//  4. a thread nests at least three levels;
//  5. every message is reconstructable with its EXACT parent from the
//     transcript alone (specs/CHAT-SESSIONS.md §4.3).
//
// It lives in the external test package for the reason thread_id_test.go
// records: the repo's GitReins Tier-1 lint lane typechecks a changed .go file
// on its own, so every file it is handed must resolve through an import.
package session_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/crier-dev/crier/internal/session"
)

// ---------------------------------------------------------------------------
// The fixture: ONE session ("one project"), ONE agent ("atlas"), two feature
// threads opened by two REQUESTS, and one of them branching to three thread
// levels. The seqs are interleaved on purpose (A, B, A, B, ...) so a read that
// accidentally concatenated the transcript would mix the two conversations.
// ---------------------------------------------------------------------------

const (
	nestSessionID = "sess-nested-1"
	nestAgent     = "atlas"

	// Feature A: the request that opens thread A, and its reply chain.
	nestReqA = "msg_req_a"
	nestA1   = "msg_a1"
	nestA2   = "msg_a2"
	nestA3   = "msg_a3"

	// Feature B: the request that opens thread B, and its reply chain.
	nestReqB = "msg_req_b"
	nestB1   = "msg_b1"
	nestB2   = "msg_b2"

	// Branch A2 (off A) and branch A3 (off A2): the thread tree's depth.
	nestThreadA2 = "msg_branch_a2"
	nestA2Reply  = "msg_a2_r1"
	nestThreadA3 = "msg_branch_a3"
	nestA3Reply  = "msg_a3_r1"
)

var nestBase = time.Date(2026, 10, 4, 7, 0, 0, 0, time.UTC)

func nestAt(sec int) time.Time { return nestBase.Add(time.Duration(sec) * time.Second) }

// nestReq builds a request message: a thread root authored by the human
// principal speaking as the one agent.
func nestReq(id string, seq int64, sec int) *session.Message {
	return &session.Message{
		ID: id, SessionID: nestSessionID, Kind: session.MessagePlain,
		Author:  session.AuthorRef{Principal: "prin_bane", AsAgent: nestAgent},
		Payload: json.RawMessage(`{"text":"request ` + id + `"}`),
		Audience: session.Audience{Rule: session.AudienceSession,
			Targets: []session.AudienceTarget{{Kind: session.TargetAgent, ID: nestAgent}}},
		Seq: seq, CreatedAt: nestAt(sec),
	}
}

// nestReply builds a reply that names what it replies TO; its thread_id is left
// empty so the flow resolves it from the parent.
func nestReply(id, parentID string, seq int64, sec int) *session.Message {
	return &session.Message{
		ID: id, SessionID: nestSessionID, ParentID: parentID, Kind: session.MessagePlain,
		Author:  session.AuthorRef{Agent: nestAgent},
		Payload: json.RawMessage(`{"text":"reply ` + id + `"}`),
		Audience: session.Audience{Rule: session.AudienceReplyDefault,
			Targets: []session.AudienceTarget{{Kind: session.TargetAgent, ID: nestAgent}}},
		Seq: seq, CreatedAt: nestAt(sec),
	}
}

// nestBuildStore writes the whole fixture through the package's own write
// surface (CreateSession / OpenRequest / Reply / Branch), so the reads under
// test are measured against records produced the way a caller produces them.
func nestBuildStore(t *testing.T) *session.JSONLStore {
	t.Helper()
	ctx := context.Background()
	store, err := session.NewJSONLStore(t.TempDir())
	require.NoError(t, err)

	require.NoError(t, store.CreateSession(ctx, &session.Session{
		ID: nestSessionID, Namespace: "acme", Kind: session.KindChannel, Title: "Project Atlas",
		CreatedAt: nestAt(0), CreatedBy: session.AuthorRef{Principal: "prin_bane", AsAgent: nestAgent},
	}, 1))
	require.NoError(t, store.AddMember(ctx, &session.Member{
		SessionID: nestSessionID, MemberType: session.MemberAgent, MemberID: nestAgent,
		Role: session.RoleOwner, AddedAt: nestAt(0), AddedBy: nestAgent,
	}, 2, nil))

	// Two requests -> two parallel threads.
	threadA, err := session.OpenRequest(ctx, store, nestReq(nestReqA, 3, 1))
	require.NoError(t, err)
	threadB, err := session.OpenRequest(ctx, store, nestReq(nestReqB, 4, 2))
	require.NoError(t, err)
	require.Equal(t, nestReqA, threadA)
	require.Equal(t, nestReqB, threadB)

	// Interleaved replies, each naming only the message it answers.
	require.NoError(t, session.Reply(ctx, store, nestReply(nestA1, nestReqA, 5, 3)))
	require.NoError(t, session.Reply(ctx, store, nestReply(nestB1, nestReqB, 6, 4)))
	require.NoError(t, session.Reply(ctx, store, nestReply(nestA2, nestA1, 7, 5)))
	require.NoError(t, session.Reply(ctx, store, nestReply(nestB2, nestB1, 8, 6)))
	require.NoError(t, session.Reply(ctx, store, nestReply(nestA3, nestA2, 9, 7)))

	// A deliberate branch off a message in thread A, and one off THAT branch:
	// the ONLY thing that deepens the thread tree (§4.5, D11).
	branchA2 := &session.Thread{
		ID: nestThreadA2, SessionID: nestSessionID, ParentThreadID: nestReqA,
		RootMessageID: nestThreadA2, AnchorMessageID: nestA1,
		CreatedBy: session.AuthorRef{Agent: nestAgent}, CreatedAt: nestAt(8),
	}
	require.NoError(t, store.Branch(ctx, branchA2, 10, "split feature A detail"))
	_, err = session.OpenRequest(ctx, store, nestReq(nestThreadA2, 11, 9))
	require.NoError(t, err)
	require.NoError(t, session.Reply(ctx, store, nestReply(nestA2Reply, nestThreadA2, 12, 10)))

	branchA3 := &session.Thread{
		ID: nestThreadA3, SessionID: nestSessionID, ParentThreadID: nestThreadA2,
		RootMessageID: nestThreadA3, AnchorMessageID: nestThreadA2,
		CreatedBy: session.AuthorRef{Agent: nestAgent}, CreatedAt: nestAt(11),
	}
	require.NoError(t, store.Branch(ctx, branchA3, 13, "split it again"))
	_, err = session.OpenRequest(ctx, store, nestReq(nestThreadA3, 14, 12))
	require.NoError(t, err)
	require.NoError(t, session.Reply(ctx, store, nestReply(nestA3Reply, nestThreadA3, 15, 13)))

	return store
}

// nestIDs renders messages as their ids.
func nestIDs(msgs []*session.Message) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m.ID)
	}
	return out
}

// nestThreadIDs renders threads as their ids.
func nestThreadIDs(threads []*session.Thread) []string {
	out := make([]string, 0, len(threads))
	for _, th := range threads {
		out = append(out, th.ID)
	}
	return out
}

// nestWalk flattens an attachment tree to its messages in pre-order.
func nestWalk(nodes []*session.ReplyNode) []*session.Message {
	var out []*session.Message
	for _, n := range nodes {
		out = append(out, n.Message)
		out = append(out, nestWalk(n.Replies)...)
	}
	return out
}

// ---------------------------------------------------------------------------
// 1 + 2. Two PARALLEL threads, read distinctly, with no cross-talk.
// ---------------------------------------------------------------------------

func TestNestedThreads_ParallelThreadsReadDistinct(t *testing.T) {
	ctx := context.Background()
	store := nestBuildStore(t)
	st, err := store.Load(ctx, nestSessionID)
	require.NoError(t, err)

	// The top level is one entry per FEATURE conversation, not one per message.
	tops := st.SessionThreads()
	require.Len(t, tops, 2, "two features -> two top-level threads, not one interleaved stream")
	require.Equal(t, nestReqA, tops[0].Thread.ID, "ordered by the request that opened it")
	require.Equal(t, nestReqB, tops[1].Thread.ID)
	require.Equal(t, []string{nestReqA, nestReqB}, nestThreadIDs(st.RootThreads()))

	// No cross-talk: each conversation holds exactly its own records, in seq
	// order, even though the seqs were written interleaved.
	require.Equal(t, []string{nestReqA, nestA1, nestA2, nestA3}, nestIDs(tops[0].Messages))
	require.Equal(t, []string{nestReqB, nestB1, nestB2}, nestIDs(tops[1].Messages))
	for _, m := range tops[0].Messages {
		require.Equal(t, nestReqA, m.ThreadID, "%s belongs to feature A", m.ID)
	}
	for _, m := range tops[1].Messages {
		require.Equal(t, nestReqB, m.ThreadID, "%s belongs to feature B", m.ID)
	}
	require.Equal(t, []string{nestReqA, nestA1, nestA2, nestA3}, nestIDs(st.ThreadMessages(nestReqA)))
	require.Equal(t, []string{nestReqB, nestB1, nestB2}, nestIDs(st.ThreadMessages(nestReqB)))

	// The two threads are disjoint as sets of message ids.
	inB := map[string]bool{}
	for _, m := range st.ThreadMessages(nestReqB) {
		inB[m.ID] = true
	}
	for _, m := range st.ThreadMessages(nestReqA) {
		require.False(t, inB[m.ID], "%s must not appear in the other thread", m.ID)
	}

	// A message in one thread is not ORDERED against the other: the read is by
	// thread_id, so interleaved seqs never interleave the conversations.
	for i := 1; i < len(tops[0].Messages); i++ {
		require.Less(t, tops[0].Messages[i-1].Seq, tops[0].Messages[i].Seq)
	}

	// And a message in one feature is not the same delivery as a message in
	// the other: the per-target idempotency key carries the thread id.
	require.NotEqual(t,
		session.IdempotencyKey(nestSessionID, nestReqA, nestA1, nestAgent),
		session.IdempotencyKey(nestSessionID, nestReqB, nestB1, nestAgent),
		"parallel threads never collapse into one delivery key")
}

// ---------------------------------------------------------------------------
// 4. A thread nests at least three levels, and every node names its own parent.
// ---------------------------------------------------------------------------

func TestNestedThreads_ThreeLevelsWithExactParents(t *testing.T) {
	ctx := context.Background()
	st, err := nestBuildStore(t).Load(ctx, nestSessionID)
	require.NoError(t, err)

	// The thread tree: request A -> branch A2 -> branch A3 = three levels.
	require.Equal(t, 0, st.ThreadDepth(nestReqA))
	require.Equal(t, 1, st.ThreadDepth(nestThreadA2))
	require.Equal(t, 2, st.ThreadDepth(nestThreadA3), "a thread nests to at least three levels")
	require.Equal(t, 0, st.ThreadDepth(nestReqB), "the other feature is untouched")

	// The nested read returns that tree, with sub-threads under their parent.
	a := st.SessionThreads()[0]
	require.Equal(t, nestReqA, a.Thread.ID)
	require.Len(t, a.Branches, 1)
	require.Equal(t, nestThreadA2, a.Branches[0].Thread.ID)
	require.Len(t, a.Branches[0].Branches, 1)
	require.Equal(t, nestThreadA3, a.Branches[0].Branches[0].Thread.ID)
	require.Empty(t, a.Branches[0].Branches[0].Branches)
	require.Equal(t, []string{nestThreadA3}, nestThreadIDs(st.SubThreads(nestThreadA2)))

	// Every message names its EXACT parent: asserted as a map over the whole
	// transcript, so a missing or renamed parent fails rather than passes.
	wantParent := map[string]string{
		nestReqA: "", nestReqB: "",
		nestA1: nestReqA, nestA2: nestA1, nestA3: nestA2,
		nestB1: nestReqB, nestB2: nestB1,
		nestThreadA2: "", nestA2Reply: nestThreadA2,
		nestThreadA3: "", nestA3Reply: nestThreadA3,
	}
	for id, parent := range wantParent {
		m := st.Message(id)
		require.NotNil(t, m, "%s is in the transcript", id)
		require.Equal(t, parent, m.ParentID, "%s names its exact parent", id)
	}

	// The attachment read returns that tree nested, from parent_id alone.
	tree := st.MessageTree(nestReqA)
	require.Len(t, tree, 1)
	require.Equal(t, nestReqA, tree[0].Message.ID)
	require.Len(t, tree[0].Replies, 1)
	require.Equal(t, nestA1, tree[0].Replies[0].Message.ID)
	require.Len(t, tree[0].Replies[0].Replies, 1)
	require.Equal(t, nestA2, tree[0].Replies[0].Replies[0].Message.ID)
	require.Len(t, tree[0].Replies[0].Replies[0].Replies, 1)
	require.Equal(t, nestA3, tree[0].Replies[0].Replies[0].Replies[0].Message.ID)
	require.Equal(t, []string{nestReqA, nestA1, nestA2, nestA3}, nestIDs(nestWalk(tree)))

	// D11 (§4.5): that four-deep attachment chain is ATTRIBUTION, not levels —
	// every message is at the SAME thread level, and the thread never deepened.
	for _, m := range nestWalk(tree) {
		require.Equal(t, 0, st.ThreadDepth(m.ThreadID),
			"%s: a reply stays in thread and adds no level", m.ID)
	}
}

// ---------------------------------------------------------------------------
// 3. A request becomes a thread, and replies land under it.
// ---------------------------------------------------------------------------

func TestRequestThread_RequestBecomesThreadAndRepliesNest(t *testing.T) {
	ctx := context.Background()
	store, err := session.NewJSONLStore(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, store.CreateSession(ctx, &session.Session{
		ID: nestSessionID, Namespace: "acme", Kind: session.KindChannel, Title: "Project Atlas",
		CreatedAt: nestAt(0), CreatedBy: session.AuthorRef{Agent: nestAgent},
	}, 1))

	// Opening a request creates the thread; the request IS its root.
	threadID, err := session.OpenRequest(ctx, store, nestReq(nestReqA, 2, 1))
	require.NoError(t, err)
	require.Equal(t, nestReqA, threadID)

	// Replies name only what they answer; the flow resolves the thread.
	require.NoError(t, session.Reply(ctx, store, nestReply(nestA1, nestReqA, 3, 2)))
	require.NoError(t, session.Reply(ctx, store, nestReply(nestA2, nestA1, 4, 3)))

	st, err := store.Load(ctx, nestSessionID)
	require.NoError(t, err)

	// The request and its resolution are ONE addressable conversation.
	require.Equal(t, []string{nestReqA, nestA1, nestA2}, nestIDs(st.ThreadMessages(nestReqA)))
	for _, m := range st.ThreadMessages(nestReqA) {
		require.Equal(t, nestReqA, m.ThreadID)
	}
	th := st.Thread(nestReqA)
	require.NotNil(t, th)
	require.Equal(t, nestReqA, th.RootMessageID)
	require.Empty(t, th.ParentThreadID, "a request opens a root thread")
	chain, complete := st.ReplyChain(nestA2)
	require.True(t, complete)
	require.Equal(t, []string{nestReqA, nestA1, nestA2}, nestIDs(chain))

	// A reply cannot cross conversations, and it cannot invent one.
	wrongThread := nestReply(nestA2Reply, nestReqA, 5, 4)
	wrongThread.ThreadID = "some-other-thread"
	err = session.Reply(ctx, store, wrongThread)
	require.ErrorIs(t, err, session.ErrThreadMismatch,
		"a reply naming another thread is refused, not moved (D11)")

	orphan := nestReply("msg_orphan", "no-such-message", 6, 5)
	err = session.Reply(ctx, store, orphan)
	require.ErrorIs(t, err, session.ErrMessageNotFound)

	// A request must be a root: a parent, or a foreign thread id, is refused.
	withParent := nestReq(nestReqB, 7, 6)
	withParent.ParentID = nestReqA
	_, err = session.OpenRequest(ctx, store, withParent)
	require.ErrorIs(t, err, session.ErrInvalidRecord)

	foreign := nestReq(nestReqB, 8, 7)
	foreign.ThreadID = "not-my-own-id"
	_, err = session.OpenRequest(ctx, store, foreign)
	require.ErrorIs(t, err, session.ErrInvalidRecord)

	// The refusals wrote nothing: the transcript still holds the one thread.
	st, err = store.Load(ctx, nestSessionID)
	require.NoError(t, err)
	require.Equal(t, []string{nestReqA, nestA1, nestA2}, nestIDs(st.ThreadMessages(nestReqA)))
	require.Nil(t, st.Message("msg_orphan"))
}

// ---------------------------------------------------------------------------
// 5. The transcript ALONE reconstructs the full tree, exact parents and all.
// ---------------------------------------------------------------------------

func TestNestedThreads_TranscriptAloneReconstructsFullTree(t *testing.T) {
	ctx := context.Background()
	store := nestBuildStore(t)

	// Read the BYTES on disk and reduce them with no store and no client state:
	// parse each line and Replay. This is the §4.3 property — a reader that has
	// only the transcript reconstructs every thread and every parent.
	raw, err := os.ReadFile(filepath.Join(store.Root(), nestSessionID+".jsonl"))
	require.NoError(t, err)
	var recs []*session.Record
	for _, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		rec, err := session.ParseRecord([]byte(line))
		require.NoError(t, err)
		recs = append(recs, rec)
	}
	fromBytes, err := session.Replay(nestSessionID, recs)
	require.NoError(t, err)
	require.Empty(t, fromBytes.Findings, "the writer states every thread key, so nothing is derived")

	fromStore, err := store.Load(ctx, nestSessionID)
	require.NoError(t, err)

	// The two independent reconstructions are the same value.
	want, err := session.CanonicalJSON(fromStore)
	require.NoError(t, err)
	got, err := session.CanonicalJSON(fromBytes)
	require.NoError(t, err)
	require.JSONEq(t, string(want), string(got))

	// The reconstructed tree: two features at the top level, three thread
	// levels on one of them, and every message with its exact parent.
	tops := fromBytes.SessionThreads()
	require.Len(t, tops, 2)
	require.Equal(t, []string{nestReqA, nestA1, nestA2, nestA3}, nestIDs(tops[0].Messages))
	require.Equal(t, []string{nestReqB, nestB1, nestB2}, nestIDs(tops[1].Messages))
	require.Equal(t, 2, fromBytes.ThreadDepth(nestThreadA3))

	// Reconstruct by parent_id alone: walk each attachment tree and require
	// that every non-root node's parent is the message it hangs from.
	var checkParents func(nodes []*session.ReplyNode, parentID string)
	checkParents = func(nodes []*session.ReplyNode, parentID string) {
		for _, n := range nodes {
			require.Equal(t, parentID, n.Message.ParentID,
				"%s hangs from its exact parent", n.Message.ID)
			checkParents(n.Replies, n.Message.ID)
		}
	}
	checkParents(fromBytes.MessageTree(nestReqA), "")
	checkParents(fromBytes.MessageTree(nestThreadA3), "")

	// And the full ordered list of the whole session, each with its parent.
	wantParent := map[string]string{
		nestReqA: "", nestA1: nestReqA, nestA2: nestA1, nestA3: nestA2,
		nestReqB: "", nestB1: nestReqB, nestB2: nestB1,
		nestThreadA2: "", nestA2Reply: nestThreadA2,
		nestThreadA3: "", nestA3Reply: nestThreadA3,
	}
	require.Len(t, fromBytes.Messages, len(wantParent))
	for _, m := range fromBytes.Messages {
		require.Equal(t, wantParent[m.ID], m.ParentID, "%s parent", m.ID)
	}
}
