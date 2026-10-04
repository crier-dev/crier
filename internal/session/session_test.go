package session

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/crier-dev/crier/internal/registry"
)

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

const (
	fixSessionID = "3f9a7c2e-6b1d-4e2a-9c7e-1d2f8a4be9c1"
	fixRootMsg   = "msg_01J9Z7K4"
	fixReplyMsg  = "msg_01J9Z7K5"
	fixBranchMsg = "msg_01J9Z8Q2"
	fixTaskMsg   = "msg_01J9Z9A1"
)

// baseTime is a whole-second instant, so a value can round-trip through
// PostgreSQL's microsecond TIMESTAMPTZ without narrowing.
var baseTime = time.Date(2026, 10, 4, 5, 0, 0, 0, time.UTC)

func at(sec int) time.Time { return baseTime.Add(time.Duration(sec) * time.Second) }

func agentRef(id string) *AuthorRef { return &AuthorRef{Agent: id} }
func humanRef(p, as string) *AuthorRef {
	return &AuthorRef{Principal: p, AsAgent: as}
}

// buildScenario returns one canonical session's records in seq order: a
// channel with three agents and one human, a root message, a reply, a late
// join with a context share and a later change to it, a deliberate branch, a
// message whose fan-out outcomes were written back, a removal and a close.
//
// It is the ONE input both backends are measured against, which is what makes
// CR-CHAT-002's "identical content" acceptance a single comparison.
func buildScenario() []*Record {
	retention := 172800
	sess := &Session{
		ID:               fixSessionID,
		Namespace:        "acme",
		Kind:             KindChannel,
		Title:            "Build Plan",
		CreatedAt:        at(0),
		CreatedBy:        *humanRef("prin_bane", "atlas"),
		RetentionSeconds: &retention,
	}
	recs := []*Record{sess.CreateRecord(1, at(0))}

	atlas := &Member{SessionID: fixSessionID, MemberType: MemberAgent, MemberID: "atlas", Role: RoleOwner, AddedBy: "atlas"}
	recs = append(recs, atlas.AddRecord(2, at(1), nil))

	nimbus := &Member{SessionID: fixSessionID, MemberType: MemberAgent, MemberID: "nimbus", Role: RoleMember, AddedBy: "atlas"}
	recs = append(recs, nimbus.AddRecord(3, at(2), nil))

	human := &Member{SessionID: fixSessionID, MemberType: MemberPrincipal, MemberID: "prin_bane", Role: RoleMember, AddedBy: "atlas"}
	recs = append(recs, human.AddRecord(4, at(3), nil))

	root := &Message{
		ID:        fixRootMsg,
		SessionID: fixSessionID,
		ThreadID:  fixRootMsg,
		Kind:      MessagePlain,
		Author:    *agentRef("atlas"),
		Payload:   json.RawMessage(`{"text":"kick off the data sweep"}`),
		Audience:  Audience{Rule: AudienceSession, Targets: []AudienceTarget{{Kind: TargetAgent, ID: "nimbus"}, {Kind: TargetAgent, ID: "atlas"}}},
		Seq:       5,
		CreatedAt: at(4),
	}
	recs = append(recs, root.Record())

	reply := &Message{
		ID:        fixReplyMsg,
		SessionID: fixSessionID,
		ThreadID:  fixRootMsg, // a reply STAYS IN THREAD (D11)
		ParentID:  fixRootMsg,
		Kind:      MessageAddressed,
		Author:    *agentRef("nimbus"),
		Payload:   json.RawMessage(`{"text":"on it"}`),
		Audience:  Audience{Rule: AudienceReplyDefault, Targets: []AudienceTarget{{Kind: TargetAgent, ID: "atlas"}}},
		Seq:       6,
		CreatedAt: at(5),
	}
	recs = append(recs, reply.Record())

	// A late join to a thread in flight records the context answer D10 asks
	// for: the mode AND the boundary message id.
	kappa := &Member{SessionID: fixSessionID, MemberType: MemberAgent, MemberID: "kappa", Role: RoleMember, AddedBy: "atlas"}
	recs = append(recs, kappa.AddRecord(7, at(6), &ContextShare{Mode: ShareSummary, BoundaryMessageID: fixRootMsg}))

	// A later change is a NEW record, never a rewrite of the add (§4.6 rule 3).
	kappaCtx := &MemberContext{SessionID: fixSessionID, MemberType: MemberAgent, MemberID: "kappa", Mode: ShareFull, SetBy: "kappa"}
	recs = append(recs, kappaCtx.Record(8, at(7)))

	// The deliberate branch: the ONLY record that creates a new thread_id.
	branch := &Thread{
		ID:              fixBranchMsg,
		SessionID:       fixSessionID,
		ParentThreadID:  fixRootMsg,
		RootMessageID:   fixBranchMsg,
		AnchorMessageID: fixRootMsg,
		CreatedBy:       *agentRef("nimbus"),
	}
	recs = append(recs, branch.BranchRecord(9, at(8), "split the schema work out"))

	branchRoot := &Message{
		ID:        fixBranchMsg,
		SessionID: fixSessionID,
		ThreadID:  fixBranchMsg,
		Kind:      MessagePlain,
		Author:    *agentRef("nimbus"),
		Payload:   json.RawMessage(`{"text":"schema work here"}`),
		Audience:  Audience{Rule: AudienceExplicit, Targets: []AudienceTarget{{Kind: TargetAgent, ID: "atlas"}}},
		Seq:       10,
		CreatedAt: at(9),
	}
	recs = append(recs, branchRoot.Record())

	// The fan-out outcomes are written back onto the SAME message record
	// (§3.2 / D1): same seq, later content, keep-LAST.
	task := &Message{
		ID:        fixTaskMsg,
		SessionID: fixSessionID,
		ThreadID:  fixTaskMsg,
		Kind:      MessageTask,
		Author:    *agentRef("atlas"),
		Payload:   json.RawMessage(`{"text":"run the sweep"}`),
		Audience:  Audience{Rule: AudienceSession, Targets: []AudienceTarget{{Kind: TargetAgent, ID: "nimbus"}}},
		Seq:       11,
		CreatedAt: at(10),
	}
	recs = append(recs, task.Record()) // the intent: no outcomes yet
	task.Outcomes = []DeliveryOutcome{{
		Target:       "nimbus",
		Outcome:      OutcomeDelivered,
		InboxEntryID: InboxEntryID(fixTaskMsg, "nimbus"),
		UpdatedAt:    at(11),
	}}
	recs = append(recs, task.Record()) // the outcomes, written back at the same seq

	prin := &Member{SessionID: fixSessionID, MemberType: MemberPrincipal, MemberID: "prin_bane"}
	recs = append(recs, prin.RemoveRecord(12, at(12), AuthorRef{Agent: "atlas"}, "handed off"))

	recs = append(recs, CloseRecord(fixSessionID, 13, at(13), AuthorRef{Agent: "atlas"}, "shipped"))
	return recs
}

// ---------------------------------------------------------------------------
// Record shape / validation (§5.1)
// ---------------------------------------------------------------------------

func TestRecord_ValidateRejectsMalformedLines(t *testing.T) {
	valid := (&Session{ID: fixSessionID, CreatedBy: AuthorRef{Agent: "atlas"}}).CreateRecord(1, at(0))
	require.NoError(t, valid.Validate())

	root := &Message{ID: fixRootMsg, SessionID: fixSessionID, ThreadID: fixRootMsg, Kind: MessagePlain,
		Audience: Audience{Rule: AudienceSession}, CreatedAt: at(1), Seq: 2}

	cases := []struct {
		name string
		rec  *Record
	}{
		{"nil", nil},
		{"unknown version", func() *Record { r := *valid; r.V = 99; return &r }()},
		{"empty session", func() *Record { r := *valid; r.SessionID = ""; return &r }()},
		{"zero seq", func() *Record { r := *valid; r.Seq = 0; return &r }()},
		{"zero ts", func() *Record { r := *valid; r.TS = time.Time{}; return &r }()},
		{"create without created_by", func() *Record { r := *valid; r.CreatedBy = nil; return &r }()},
		{"unknown type", func() *Record { r := *valid; r.Type = "session.bogus"; return &r }()},
		{"message without audience", func() *Record { r := *root.Record(); r.Audience = nil; return &r }()},
		{"message with bad kind", func() *Record { r := *root.Record(); r.MessageKind = "task-action"; return &r }()},
		// A reply MUST NOT be shipped as session.message: the two types are
		// structurally distinguishable, so a reply with no parent_id is a
		// malformed reply, not a root.
		{"reply without parent", func() *Record {
			return &Record{V: 1, Type: RecordThreadReply, SessionID: fixSessionID, Seq: 3, TS: at(2), MessageID: fixReplyMsg, ThreadID: fixRootMsg, MessageKind: MessagePlain, Audience: &Audience{}}
		}()},
		// A thread root's thread_id must be its own message id (§4.3).
		{"root thread_id mismatch", func() *Record { r := *root.Record(); r.ThreadID = "someone-else"; return &r }()},
		// session.message must never carry parent_id (that is thread.reply).
		{"message carrying parent_id", func() *Record { r := *root.Record(); r.ParentID = fixRootMsg; return &r }()},
		{"branch without anchor", func() *Record {
			return &Record{V: 1, Type: RecordThreadBranch, SessionID: fixSessionID, Seq: 4, TS: at(3), ThreadID: "t1", ParentThreadID: "t0"}
		}()},
		// A share mode is a closed set, and `since` names a boundary message.
		{"member add with unknown share mode", (&Member{SessionID: fixSessionID, MemberType: MemberAgent,
			MemberID: "x", Role: RoleMember}).AddRecord(5, at(4), &ContextShare{Mode: "everything"})},
		{"member context since without boundary", (&MemberContext{SessionID: fixSessionID, MemberType: MemberAgent,
			MemberID: "x", Mode: ShareSince, SetAt: at(5)}).Record(6, at(5))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Error(t, tc.rec.Validate())
		})
	}
}

func TestRecord_MarshalParseRoundTrip(t *testing.T) {
	for _, rec := range buildScenario() {
		line, err := rec.MarshalLine()
		require.NoError(t, err)
		require.True(t, strings.HasSuffix(string(line), "\n"), "a log line ends with a newline")
		require.NotContains(t, string(line), "\n\n")

		// `v` must be the FIRST field so a reader can dispatch on a version it
		// knows without parsing the rest.
		require.True(t, strings.HasPrefix(string(line), `{"v":1,`), "line must start with the version field: %s", line)

		got, err := ParseRecord(line)
		require.NoError(t, err)
		require.Equal(t, rec.Type, got.Type)
		require.Equal(t, rec.SessionID, got.SessionID)
		require.Equal(t, rec.Seq, got.Seq)
	}
}

func TestParseRecord_RefusesUnknownVersion(t *testing.T) {
	_, err := ParseRecord([]byte(`{"v":99,"type":"session.create","session_id":"s","seq":1,"ts":"2026-10-04T05:00:00Z"}`))
	require.ErrorIs(t, err, ErrInvalidRecord)
}

// ---------------------------------------------------------------------------
// Replay: the transcript reconstructs the session (§4.3)
// ---------------------------------------------------------------------------

func TestReplay_ReconstructsSessionState(t *testing.T) {
	st, err := Replay(fixSessionID, buildScenario())
	require.NoError(t, err)

	require.Equal(t, fixSessionID, st.Session.ID)
	require.Equal(t, "acme", st.Session.Namespace)
	require.Equal(t, KindChannel, st.Session.Kind)
	require.Equal(t, SessionClosed, st.Session.State)
	require.NotNil(t, st.Session.ClosedAt)
	require.Equal(t, at(13), *st.Session.ClosedAt)
	require.Equal(t, AuthorRef{Principal: "prin_bane", AsAgent: "atlas"}, st.Session.CreatedBy)
	require.NotNil(t, st.Session.RetentionSeconds)
	require.Equal(t, 172800, *st.Session.RetentionSeconds)

	require.Len(t, st.Members, 4)
	require.Len(t, st.Messages, 4)
	// Three threads: the root conversation's message, the branch's message and
	// the task are each a thread root (§4.3).
	require.Len(t, st.Threads, 3)
	require.Len(t, st.ContextShares, 1)
	require.Equal(t, ContextShareMode("full"), st.ContextShares[0].Mode)

	// seq is the ordering authority.
	for i := 1; i < len(st.Messages); i++ {
		require.Less(t, st.Messages[i-1].Seq, st.Messages[i].Seq)
	}
	// The fan-out outcome was written back onto the same seq (keep-LAST).
	require.Len(t, st.Message(fixTaskMsg).Outcomes, 1)
	require.Equal(t, OutcomeDelivered, st.Message(fixTaskMsg).Outcomes[0].Outcome)
	require.Equal(t, InboxEntryID(fixTaskMsg, "nimbus"), st.Message(fixTaskMsg).Outcomes[0].InboxEntryID)
}

func TestReplay_KeepLastPerSeq(t *testing.T) {
	recs := []*Record{(&Session{ID: "s1", CreatedBy: AuthorRef{Agent: "a"}}).CreateRecord(1, at(0))}
	// Two lines claiming (s1, seq 2) with different content: the LAST wins, and
	// it is never a silent error — the log is append-only history.
	first := &Member{SessionID: "s1", MemberType: MemberAgent, MemberID: "x", Role: RoleMember, AddedBy: "a"}
	second := &Member{SessionID: "s1", MemberType: MemberAgent, MemberID: "x", Role: RoleOwner, AddedBy: "a"}
	recs = append(recs, first.AddRecord(2, at(1), nil), second.AddRecord(2, at(2), nil))

	st, err := Replay("s1", recs)
	require.NoError(t, err)
	require.Len(t, st.Members, 1)
	require.Equal(t, RoleOwner, st.Members[0].Role, "keep-LAST per (session_id, seq)")
	require.Equal(t, at(2), st.Members[0].AddedAt)
}

func TestReplay_MembershipAtTime(t *testing.T) {
	st, err := Replay(fixSessionID, buildScenario())
	require.NoError(t, err)

	// Before the human joined.
	early := st.MembersAt(at(0))
	require.Len(t, early, 0)

	// Everyone is in mid-conversation.
	mid := st.MembersAt(at(9))
	require.Len(t, mid, 4)

	// After the removal the human is out — membership at T is derivable by
	// replaying events up to T (§2.3 consequence 1).
	late := st.MembersAt(at(13))
	require.Len(t, late, 3)
	ids := map[string]bool{}
	for _, m := range late {
		ids[m.MemberID] = true
	}
	require.False(t, ids["prin_bane"])
}

func TestReplay_ClosedSessionRefusesNewRecords(t *testing.T) {
	recs := []*Record{(&Session{ID: "s1", CreatedBy: AuthorRef{Agent: "a"}}).CreateRecord(1, at(0))}
	recs = append(recs, CloseRecord("s1", 2, at(1), AuthorRef{Agent: "a"}, "done"))

	msg := &Message{ID: "m1", SessionID: "s1", ThreadID: "m1", Kind: MessagePlain,
		Author: AuthorRef{Agent: "a"}, Audience: Audience{Rule: AudienceSession}, Seq: 3, CreatedAt: at(2)}
	recs = append(recs, msg.Record())

	_, err := Replay("s1", recs)
	require.ErrorIs(t, err, ErrSessionClosed)

	// reopen is an event, not a field mutation: after it, records are admitted
	// again and "was this open on date T" stays answerable by replay.
	reopened := append([]*Record{recs[0], recs[1]}, ReopenRecord("s1", 3, at(3), AuthorRef{Agent: "a"}))
	msg.Seq = 4
	reopened = append(reopened, msg.Record())
	st, err := Replay("s1", reopened)
	require.NoError(t, err)
	require.Equal(t, SessionOpen, st.Session.State)
	require.Nil(t, st.Session.ClosedAt)
	require.Len(t, st.Messages, 1)
}

func TestReplay_RejectsForeignSessionAndOrphans(t *testing.T) {
	sessionRec := (&Session{ID: "s1", CreatedBy: AuthorRef{Agent: "a"}}).CreateRecord(1, at(0))

	// A record naming another session is refused, never folded in.
	other := (&Session{ID: "s2", CreatedBy: AuthorRef{Agent: "a"}}).CreateRecord(2, at(1))
	_, err := Replay("s1", []*Record{sessionRec, other})
	require.ErrorIs(t, err, ErrInvalidRecord)

	// A removal for a member who was never added is a broken thread class
	// defect, reported rather than silently applied.
	orphan := (&Member{SessionID: "s1", MemberType: MemberAgent, MemberID: "ghost"}).RemoveRecord(2, at(1), AuthorRef{Agent: "a"}, "")
	_, err = Replay("s1", []*Record{sessionRec, orphan})
	require.ErrorIs(t, err, ErrInvalidRecord)

	// A session with no create record does not exist (§1.3).
	_, err = Replay("s9", []*Record{})
	require.ErrorIs(t, err, ErrSessionNotFound)
}

// ---------------------------------------------------------------------------
// The depth rule (§4.5, D11) — the load-bearing correction
// ---------------------------------------------------------------------------

func TestDepthRule_ReplyStaysInThread(t *testing.T) {
	st, err := Replay(fixSessionID, buildScenario())
	require.NoError(t, err)

	reply := st.Message(fixReplyMsg)
	require.NotNil(t, reply)

	// parent_id is ATTRIBUTION: it names what was replied to.
	require.Equal(t, fixRootMsg, reply.ParentID)
	// ... and the reply did NOT move out of its thread: thread_id is the root's.
	require.Equal(t, fixRootMsg, reply.ThreadID)
	// ... and it did NOT deepen the tree: responding created no thread.
	require.False(t, reply.IsRoot())
	require.Equal(t, 3, len(st.Threads), "a reply creates no thread")
	require.Nil(t, st.Thread(fixReplyMsg), "a reply never becomes a thread root")
	require.Equal(t, 0, st.ThreadDepth(fixRootMsg), "a reply does not deepen its thread")

	// Grouping by thread_id (never by parent_id hops) yields the conversation.
	threadMsgs := st.ThreadMessages(fixRootMsg)
	require.Len(t, threadMsgs, 2)
	require.Equal(t, []int64{5, 6}, []int64{threadMsgs[0].Seq, threadMsgs[1].Seq})
}

func TestDepthRule_BranchCreatesNewThreadOnly(t *testing.T) {
	recs := buildScenario()
	st, err := Replay(fixSessionID, recs)
	require.NoError(t, err)

	child := st.Thread(fixBranchMsg)
	require.NotNil(t, child)
	require.Equal(t, fixRootMsg, child.ParentThreadID)
	require.Equal(t, fixRootMsg, child.AnchorMessageID)
	require.Equal(t, fixBranchMsg, child.RootMessageID)
	require.Equal(t, 1, st.ThreadDepth(fixBranchMsg), "a branch adds exactly one level")
	require.Equal(t, 0, st.ThreadDepth(fixRootMsg))

	// Branching does not touch the parent thread: no message was moved,
	// re-parented or copied (§4.5 rule 2). Compared against the SAME records
	// with the branch line removed, so "unchanged" is asserted, not claimed:
	// without the branch record the sub-thread is an ordinary root thread and
	// the parent thread's content is byte-identical.
	var noBranch []*Record
	for _, r := range recs {
		if r.Type != RecordThreadBranch {
			noBranch = append(noBranch, r)
		}
	}
	plain, err := Replay(fixSessionID, noBranch)
	require.NoError(t, err)
	require.Equal(t, 0, plain.ThreadDepth(fixBranchMsg), "without the branch record the thread is a root")
	require.Empty(t, plain.Thread(fixBranchMsg).ParentThreadID)
	require.Equal(t, threadSnapshot(st, fixRootMsg), threadSnapshot(plain, fixRootMsg),
		"the parent thread is byte-identical apart from the anchor (§4.5 rule 3)")
	require.Len(t, st.ThreadMessages(fixRootMsg), 2, "the branch issued no fan-out and moved nothing")

	// Two branches from ONE message are siblings, not levels (§4.5 rule 6).
	sibling := &Thread{ID: "msg_sibling", SessionID: fixSessionID, ParentThreadID: fixRootMsg,
		RootMessageID: "msg_sibling", AnchorMessageID: fixRootMsg, CreatedBy: AuthorRef{Agent: "kappa"}}
	var open []*Record
	for _, r := range recs {
		if r.Type != RecordClose {
			open = append(open, r)
		}
	}
	sibState, err := Replay(fixSessionID, append(open, sibling.BranchRecord(13, at(13), "other split")))
	require.NoError(t, err)
	require.Equal(t, 1, sibState.ThreadDepth(fixBranchMsg))
	require.Equal(t, 1, sibState.ThreadDepth("msg_sibling"), "a sibling is not deeper")
}

// threadSnapshot renders one thread's messages canonically, so "the parent
// thread is unchanged" is asserted as content rather than claimed.
func threadSnapshot(st *State, threadID string) string {
	b, err := CanonicalJSON(st.ThreadMessages(threadID))
	if err != nil {
		panic(err)
	}
	return string(b)
}

// ---------------------------------------------------------------------------
// Late-join context share (§4.6, D10)
// ---------------------------------------------------------------------------

func TestContextShare_LateJoinAndLaterChange(t *testing.T) {
	st, err := Replay(fixSessionID, buildScenario())
	require.NoError(t, err)

	require.Len(t, st.ContextShares, 1)
	cs := st.ContextShares[0]
	require.Equal(t, MemberAgent, cs.MemberType)
	require.Equal(t, "kappa", cs.MemberID)
	// The LATER session.member.context event won the projection; the add event
	// remains the record of the earlier answer.
	require.Equal(t, ShareFull, cs.Mode)
	require.Equal(t, "kappa", cs.SetBy)
	require.Empty(t, cs.BoundaryMessageID)
}

// A share mode never widens the audience: what a joiner is GIVEN is a context
// decision, while WHO receives deliveries stays the audience rule (§4.6 rule 5).
func TestContextShare_DoesNotChangeAudience(t *testing.T) {
	st, err := Replay(fixSessionID, buildScenario())
	require.NoError(t, err)

	require.Len(t, st.Message(fixBranchMsg).Audience.Targets, 1)
	require.Equal(t, "atlas", st.Message(fixBranchMsg).Audience.Targets[0].ID)
	// The membership is what it is; the share mode changed nothing about it.
	require.Len(t, st.MembersAt(at(9)), 4)
}

// ---------------------------------------------------------------------------
// The JSONL log and the transport bundle (§5.1, §6)
// ---------------------------------------------------------------------------

func TestJSONLStore_RoundTripsIdenticalContent(t *testing.T) {
	ctx := context.Background()
	store, err := NewJSONLStore(t.TempDir())
	require.NoError(t, err)

	recs := buildScenario()
	for _, rec := range recs {
		require.NoError(t, store.Append(ctx, rec))
	}

	// Re-reading the log yields the same records, in seq order.
	got, err := store.Records(ctx, fixSessionID)
	require.NoError(t, err)
	require.Len(t, got, 13, "13 distinct seqs (the outcome write-back shares a seq)")
	for i := 1; i < len(got); i++ {
		require.Less(t, got[i-1].Seq, got[i].Seq)
	}

	// Load reduces to the state Replay produces from the same input: the log IS
	// the ordering authority (§3.1).
	want, err := Replay(fixSessionID, recs)
	require.NoError(t, err)
	have, err := store.Load(ctx, fixSessionID)
	require.NoError(t, err)
	requireJSONEqual(t, want, have)
}

func TestJSONLStore_BundleExportImportRoundTrip(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := NewJSONLStore(filepath.Join(root, "log"))
	require.NoError(t, err)
	for _, rec := range buildScenario() {
		require.NoError(t, store.Append(ctx, rec))
	}

	ids, err := store.Sessions()
	require.NoError(t, err)
	require.Equal(t, []string{fixSessionID}, ids)

	bundleDir := filepath.Join(root, "bundle")
	path, err := store.Export(ctx, fixSessionID, bundleDir)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(bundleDir, fixSessionID+".jsonl"), path)

	// The bundle carries the SAME lines as the log — a filtered copy, not a
	// re-encoding — so an import is a replay.
	imported, err := store.Import(path)
	require.NoError(t, err)
	require.Len(t, imported, 13)

	fresh, err := NewJSONLStore(filepath.Join(root, "fresh"))
	require.NoError(t, err)
	state, err := fresh.LoadBundle(path)
	require.NoError(t, err)

	want, err := Replay(fixSessionID, buildScenario())
	require.NoError(t, err)
	requireJSONEqual(t, want, state)
}

func TestJSONLStore_MissingSessionAndUnsafeID(t *testing.T) {
	ctx := context.Background()
	store, err := NewJSONLStore(t.TempDir())
	require.NoError(t, err)

	_, err = store.Load(ctx, "nope")
	require.ErrorIs(t, err, ErrSessionNotFound)

	for _, bad := range []string{"", "../escape", "a/b", "with space"} {
		_, err := store.Load(ctx, bad)
		require.ErrorIs(t, err, ErrInvalidRecord, "session id %q must be refused", bad)
		_, err = store.Export(ctx, bad, t.TempDir())
		require.ErrorIs(t, err, ErrInvalidRecord)
	}
}

func TestJSONLStore_RefusesMalformedLine(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := NewJSONLStore(root)
	require.NoError(t, err)
	require.NoError(t, store.Append(ctx, (&Session{ID: "s1", CreatedBy: AuthorRef{Agent: "a"}}).CreateRecord(1, at(0))))

	require.NoError(t, osWriteFile(filepath.Join(root, "s1.jsonl"), "{not json}\n", true))
	_, err = store.Records(ctx, "s1")
	require.ErrorIs(t, err, ErrInvalidRecord)
}

// requireJSONEqual asserts two values are the same content, compared as
// canonical JSON so map ordering and jsonb re-serialisation cannot make equal
// content look different.
func requireJSONEqual(t *testing.T, want, got any) {
	t.Helper()
	wb, err := CanonicalJSON(want)
	require.NoError(t, err)
	gb, err := CanonicalJSON(got)
	require.NoError(t, err)
	require.JSONEq(t, string(wb), string(gb))
}

// ---------------------------------------------------------------------------
// Fan-out (§3.4, D1) — reuses the shipped inbox path
// ---------------------------------------------------------------------------

func newFanoutFixture(t *testing.T, ids ...string) *registry.MemoryStore {
	t.Helper()
	store := registry.NewMemoryStore()
	for _, id := range ids {
		require.NoError(t, store.Register(&registry.Agent{ID: id, Status: registry.StatusOnline}))
	}
	return store
}

func TestFanout_DeliversToOneInboxPerMember(t *testing.T) {
	ctx := context.Background()
	store := newFanoutFixture(t, "atlas", "nimbus", "kappa")

	msg := &Message{
		ID: "msg_fanout", SessionID: fixSessionID, ThreadID: "msg_fanout",
		Kind: MessagePlain, Author: *agentRef("atlas"),
		Payload: json.RawMessage(`{"text":"all hands"}`),
	}
	outcomes, err := Fanout(ctx, store, msg, []string{"nimbus", "kappa", "atlas"})
	require.NoError(t, err)
	require.Len(t, outcomes, 3)

	// Every member's durable inbox holds the message — through the EXISTING
	// deliver path, so the inbox's own lease/ack/TTL machinery applies.
	for _, target := range []string{"atlas", "nimbus", "kappa"} {
		entries, _, err := store.Retrieve(target, time.Minute, 10)
		require.NoError(t, err)
		require.Len(t, entries, 1, "%s must have exactly one delivery", target)
		require.Equal(t, InboxEntryID("msg_fanout", target), entries[0].ID,
			"the transcript's message id and the inbox entry are the same identity scoped per target (§3.4 rule 1)")
		require.Equal(t, "atlas", entries[0].Sender)
		require.Equal(t, []byte(`{"text":"all hands"}`), []byte(entries[0].Payload))
		// The delivery carries the deterministic per-target idempotency key
		// (§3.4 rule 2) — the durable half of "cannot double-deliver".
		require.Equal(t, IdempotencyKey(fixSessionID, "msg_fanout", "msg_fanout", target),
			entries[0].IdempotencyKey)
	}

	// The per-target idempotency key is deterministic and distinct per target
	// (§3.4 rule 2); the outcomes were written back onto the message.
	require.Equal(t, IdempotencyKey(fixSessionID, "msg_fanout", "msg_fanout", "nimbus"),
		IdempotencyKey(fixSessionID, "msg_fanout", "msg_fanout", "nimbus"))
	require.NotEqual(t, IdempotencyKey(fixSessionID, "msg_fanout", "msg_fanout", "nimbus"),
		IdempotencyKey(fixSessionID, "msg_fanout", "msg_fanout", "kappa"))
	for _, o := range outcomes {
		require.Equal(t, OutcomeDelivered, o.Outcome)
		require.Equal(t, InboxEntryID("msg_fanout", o.Target), o.InboxEntryID)
	}
	require.Len(t, msg.Outcomes, 3)
}

func TestFanout_RetryDoesNotDoubleDeliver(t *testing.T) {
	ctx := context.Background()
	store := newFanoutFixture(t, "nimbus")

	msg := &Message{ID: "msg_one", SessionID: "s1", ThreadID: "msg_one", Kind: MessagePlain,
		Author: *agentRef("atlas"), Payload: json.RawMessage(`{"text":"once"}`)}

	_, err := Fanout(ctx, store, msg, []string{"nimbus"})
	require.NoError(t, err)

	// A retried fan-out reuses the recorded outcome and issues nothing.
	outcomes, err := Fanout(ctx, store, msg, []string{"nimbus"})
	require.NoError(t, err)
	require.Len(t, outcomes, 1)

	depth, _, _, err := store.Stats("nimbus")
	require.NoError(t, err)
	require.Equal(t, 1, depth, "a retried fan-out must not store the message twice")
}

func TestFanout_RefusalIsANamedOutcome(t *testing.T) {
	ctx := context.Background()
	store := newFanoutFixture(t, "atlas") // nimbus is unknown

	msg := &Message{ID: "msg_r", SessionID: "s1", ThreadID: "msg_r", Kind: MessagePlain,
		Author: *agentRef("atlas"), Payload: json.RawMessage(`{"text":"hi"}`)}

	outcomes, err := Fanout(ctx, store, msg, []string{"atlas", "nimbus"})
	require.NoError(t, err, "a refused target is not a fan-out error")
	require.Len(t, outcomes, 2)
	byTarget := map[string]DeliveryOutcome{}
	for _, o := range outcomes {
		byTarget[o.Target] = o
	}
	require.Equal(t, OutcomeDelivered, byTarget["atlas"].Outcome)
	require.Equal(t, OutcomeRefused, byTarget["nimbus"].Outcome)
	require.Contains(t, byTarget["nimbus"].Reason, "agent not found", "a refusal is SHOWN with its reason, never swallowed")
}

func TestFanoutRecipients_GroupFansOutCapabilityDoesNot(t *testing.T) {
	aud := Audience{Rule: AudienceExplicit, Targets: []AudienceTarget{
		{Kind: TargetAgent, ID: "atlas"},
		{Kind: TargetGroup, ID: "team:infra"},
		{Kind: TargetCapability, ID: "data-capabilities"},
		{Kind: TargetPrincipal, ID: "prin_bane"},
		{Kind: TargetAgent, ID: "atlas"}, // duplicate
	}}
	agents, skipped := FanoutRecipients(aud, map[string][]string{"team:infra": {"nimbus", "kappa"}})

	require.Equal(t, []string{"atlas", "kappa", "nimbus"}, agents,
		"a named group's curated roster fans out to every current member (D8)")
	require.Len(t, skipped, 2)
	reasons := skipped[0].Reason + " | " + skipped[1].Reason
	require.Contains(t, reasons, "selector")
	require.Contains(t, reasons, "no inbox")
}

func TestReplyAudience_DefaultRule(t *testing.T) {
	st, err := Replay(fixSessionID, buildScenario())
	require.NoError(t, err)

	// The parent's author plus everyone who has spoken in the thread, minus the
	// replier — neither the whole session nor the parent author alone.
	// Thread msg_1 holds: the root (author atlas) and the reply (author nimbus).
	got := ReplyAudience(st, fixRootMsg, fixRootMsg, "nimbus")
	require.Equal(t, []string{"atlas"}, got, "the parent author, who is not the replier")

	got = ReplyAudience(st, fixRootMsg, fixRootMsg, "atlas")
	require.Equal(t, []string{"nimbus"}, got, "the other speaker in the thread, minus the replier")

	// A reply never notifies its own author: a third party replying to the root
	// addresses both speakers (none of whom is the replier).
	got = ReplyAudience(st, fixRootMsg, fixRootMsg, "kappa")
	require.Equal(t, []string{"atlas", "nimbus"}, got)
}

// ---------------------------------------------------------------------------
// Two projections of the same facts are ONE value (CR-CHAT-002 acceptance)
// ---------------------------------------------------------------------------

func TestState_TwoProjectionsAgree(t *testing.T) {
	// The JSONL log and (in the integration battery) the PostgreSQL view are
	// measured against the same State. Here the log's own two read paths — the
	// append log and a transported bundle — must agree, which is the half of
	// the acceptance that needs no database.
	ctx := context.Background()
	root := t.TempDir()
	store, err := NewJSONLStore(root)
	require.NoError(t, err)
	recs := buildScenario()
	for _, rec := range recs {
		require.NoError(t, store.Append(ctx, rec))
	}
	fromLog, err := store.Load(ctx, fixSessionID)
	require.NoError(t, err)

	path, err := store.Export(ctx, fixSessionID, filepath.Join(root, "b"))
	require.NoError(t, err)
	fresh, err := NewJSONLStore(filepath.Join(root, "fresh"))
	require.NoError(t, err)
	fromBundle, err := fresh.LoadBundle(path)
	require.NoError(t, err)

	requireJSONEqual(t, fromLog, fromBundle)

	// ... and the bundle is a transport form, so the bytes it wrote are the log
	// lines, one record per line, v first.
	raw, err := readAll(path)
	require.NoError(t, err)
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	require.Len(t, lines, 13)
	for _, l := range lines {
		require.True(t, strings.HasPrefix(l, `{"v":1,`), "one record-version per line: %s", l)
	}
}

func TestNextSeq(t *testing.T) {
	require.Equal(t, int64(1), NextSeq(nil))
	require.Equal(t, int64(14), NextSeq(buildScenario()))
}

// Conflicts surfaces §3.1's finding: a (session_id, seq) claimed by two lines
// with different content. The write-back case (same seq, later content) is one
// such pair by design; an identical duplicate is NOT.
func TestConflicts(t *testing.T) {
	sess := (&Session{ID: "s1", CreatedBy: AuthorRef{Agent: "a"}}).CreateRecord(1, at(0))
	msg := &Message{ID: "m1", SessionID: "s1", ThreadID: "m1", Kind: MessagePlain,
		Author: AuthorRef{Agent: "a"}, Audience: Audience{Rule: AudienceSession}, Seq: 2, CreatedAt: at(1)}
	intent := msg.Record()
	msg.Outcomes = []DeliveryOutcome{{Target: "atlas", Outcome: OutcomeDelivered, UpdatedAt: at(2)}}
	filled := msg.Record()

	require.Empty(t, Conflicts([]*Record{sess, intent, filled}), "the write-back is one known pair")

	// An identical duplicate is a no-op, not a conflict.
	require.Empty(t, Conflicts([]*Record{sess, intent, msg.Record()}))

	// Two DIFFERENT records at the same seq ARE reported as a finding.
	other := &Message{ID: "m2", SessionID: "s1", ThreadID: "m2", Kind: MessageTask,
		Author: AuthorRef{Agent: "b"}, Audience: Audience{Rule: AudienceSession}, Seq: 2, CreatedAt: at(1)}
	got := Conflicts([]*Record{sess, intent, other.Record()})
	require.Len(t, got, 1)
	require.Equal(t, int64(2), got[0].Seq)
	require.Equal(t, [2]RecordType{RecordMessage, RecordMessage}, got[0].Types)
}

func TestErrors_AreDistinct(t *testing.T) {
	// The error vocabulary is what a caller branches on; keep them distinct.
	for _, pair := range [][2]error{{ErrSessionNotFound, ErrInvalidRecord}, {ErrSessionNotFound, ErrSessionClosed}} {
		require.False(t, errors.Is(pair[0], pair[1]))
	}
}

// osWriteFile writes a file, optionally appending — a tiny helper so the tests
// can corrupt a log deliberately without importing os at every site.
func osWriteFile(path, content string, appendMode bool) error {
	flag := os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	if appendMode {
		flag = os.O_CREATE | os.O_WRONLY | os.O_APPEND
	}
	f, err := os.OpenFile(path, flag, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	_, err = f.WriteString(content)
	return err
}

// readAll reads a whole file.
func readAll(path string) ([]byte, error) { return os.ReadFile(path) }

// ---------------------------------------------------------------------------
// The CRUD surface: a caller creates a session, manages membership and posts
// messages without hand-assembling a union Record.
// ---------------------------------------------------------------------------

func TestJSONLStore_CRUDSurfaceRoundTrip(t *testing.T) {
	ctx := context.Background()
	store, err := NewJSONLStore(t.TempDir())
	require.NoError(t, err)

	sess := &Session{ID: "sess-crud", Namespace: "acme", Kind: KindChannel, Title: "CRUD",
		CreatedAt: at(0), CreatedBy: AuthorRef{Principal: "prin_bane", AsAgent: "atlas"}}
	seq, err := store.NextSeq(ctx, sess.ID)
	require.NoError(t, err)
	require.EqualValues(t, 1, seq)
	require.NoError(t, store.CreateSession(ctx, sess, seq))

	member := &Member{SessionID: sess.ID, MemberType: MemberAgent, MemberID: "atlas",
		Role: RoleOwner, AddedAt: at(1), AddedBy: "atlas"}
	require.NoError(t, store.AddMember(ctx, member, 2, nil))

	msg := &Message{ID: "m-crud", SessionID: sess.ID, ThreadID: "m-crud", Kind: MessageAddressed,
		Author: AuthorRef{Agent: "atlas"}, Payload: json.RawMessage(`{"text":"hi"}`),
		Audience: Audience{Rule: AudienceExplicit, Targets: []AudienceTarget{{Kind: TargetAgent, ID: "nimbus"}}},
		Seq:      3, CreatedAt: at(2)}
	require.NoError(t, store.PostMessage(ctx, msg))

	reply := &Message{ID: "m-crud-r", SessionID: sess.ID, ThreadID: "m-crud", ParentID: "m-crud",
		Kind: MessagePlain, Author: AuthorRef{Agent: "nimbus"}, Payload: json.RawMessage(`{"text":"yo"}`),
		Audience: Audience{Rule: AudienceReplyDefault, Targets: []AudienceTarget{{Kind: TargetAgent, ID: "atlas"}}},
		Seq:      4, CreatedAt: at(3)}
	require.NoError(t, store.PostMessage(ctx, reply))

	require.EqualValues(t, 5, mustNextSeq(t, store, ctx, sess.ID))

	branch := &Thread{ID: "m-crud-b", SessionID: sess.ID, ParentThreadID: "m-crud",
		RootMessageID: "m-crud-b", AnchorMessageID: "m-crud", CreatedBy: AuthorRef{Agent: "nimbus"},
		CreatedAt: at(4)}
	require.NoError(t, store.Branch(ctx, branch, 5, "split"))
	require.NoError(t, store.SetMemberContext(ctx, &MemberContext{
		SessionID: sess.ID, MemberType: MemberAgent, MemberID: "atlas", Mode: ShareSummary, SetBy: "atlas", SetAt: at(5)}, 6))
	require.NoError(t, store.CloseSession(ctx, sess.ID, 7, at(6), AuthorRef{Agent: "atlas"}, "done"))
	require.NoError(t, store.ReopenSession(ctx, sess.ID, 8, at(7), AuthorRef{Agent: "atlas"}))

	// Read paths.
	got, err := store.Session(ctx, sess.ID)
	require.NoError(t, err)
	require.Equal(t, SessionOpen, got.State, "reopen is an event, not a mutation of the close record")
	require.Equal(t, "CRUD", got.Title)

	members, err := store.Members(ctx, sess.ID)
	require.NoError(t, err)
	require.Len(t, members, 1)

	messages, err := store.Messages(ctx, sess.ID)
	require.NoError(t, err)
	require.Len(t, messages, 2)

	threads, err := store.Threads(ctx, sess.ID)
	require.NoError(t, err)
	require.Len(t, threads, 2)
	require.Equal(t, "m-crud", threads[1].ParentThreadID)

	shares, err := store.ContextShares(ctx, sess.ID)
	require.NoError(t, err)
	require.Len(t, shares, 1)
	require.Equal(t, ShareSummary, shares[0].Mode)

	// A removal is a record too, and the member list reports it.
	member.SessionID, member.MemberType, member.MemberID = sess.ID, MemberPrincipal, "prin_bane"
	require.NoError(t, store.AddMember(ctx, member, 9, nil))
	require.NoError(t, store.RemoveMember(ctx, member, 10, at(8), AuthorRef{Agent: "atlas"}, "left"))
	members, err = store.Members(ctx, sess.ID)
	require.NoError(t, err)
	require.Len(t, members, 2)
	require.NotNil(t, members[1].RemovedAt)

	// And the whole thing is still one State: the log and the reduced view.
	st, err := store.Load(ctx, sess.ID)
	require.NoError(t, err)
	require.Len(t, st.Messages, 2)
	require.Equal(t, 1, st.ThreadDepth("m-crud-b"))
}

func mustNextSeq(t *testing.T, store *JSONLStore, ctx context.Context, id string) int64 {
	t.Helper()
	seq, err := store.NextSeq(ctx, id)
	require.NoError(t, err)
	return seq
}
