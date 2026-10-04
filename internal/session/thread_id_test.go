// Package session_test is the EXTERNAL test package for internal/session.
//
// The CR-CHAT-005 threading tests live here rather than in the internal
// `package session` file for a mechanical reason with a measurable effect: the
// repo's GitReins Tier-1 lint lane runs
//
//	golangci-lint run --new-from-rev=HEAD~1 <changed .go files>
//
// and a golangci-lint FILE LIST is built from the named files ALONE, so a
// changed subset of a package is typechecked without its siblings. Every file
// it is handed must therefore resolve on its own. A file in the internal test
// package that reaches for a fixture defined in session_test.go compiles under
// `go test ./...` and FAILS the lane with `undefined: <fixture> (typecheck)`;
// an external test package that IMPORTS the package under test resolves every
// symbol through the import instead. The fixtures below are this file's own for
// the same reason.
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
// CR-CHAT-005: thread_id is PERSISTED on a stored message, and a thread is
// reconstructable from the transcript alone (§4.3 of specs/CHAT-THREADING.md /
// CHAT-SESSIONS.md). The records here are the two things the row owes: a stored
// key that survives the log and the transport form, and a reader that
// reconstructs the same tree from legacy records that predate the field.
// ---------------------------------------------------------------------------

// The threading fixture: one thread with a three-message reply chain, so a
// reconstruction has a root, a reply and a reply-to-a-reply to attach.
const (
	thrSessionID = "sess-thread-1"
	thrThread    = "msg_thread_root"
	thrThreadR1  = "msg_thread_r1"
	thrThreadR2  = "msg_thread_r2"
)

// The legacy fixture: the SAME shape written by a generation that stored no
// thread_id — the CR-FEAT-004 wire tag that was not yet a record field.
const (
	thrLegacySID   = "legacy-1"
	thrLegacyRoot  = "lg_root"
	thrLegacyReply = "lg_r1"
	thrLegacyDeep  = "lg_r2"
)

// thrBase is a whole-second instant, so a value round-trips through a
// TIMESTAMPTZ without narrowing.
var thrBase = time.Date(2026, 10, 4, 5, 0, 0, 0, time.UTC)

func thrAt(sec int) time.Time { return thrBase.Add(time.Duration(sec) * time.Second) }

func thrRef(id string) *session.AuthorRef { return &session.AuthorRef{Agent: id} }

func thrReadAll(path string) ([]byte, error) { return os.ReadFile(path) }

func thrWriteFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}

// thrAudience builds a resolved audience with one agent target, so a message
// record satisfies the §5.1 shape without hand-writing the whole literal.
func thrAudience(rule session.AudienceRule, target string) session.Audience {
	return session.Audience{
		Rule:    rule,
		Targets: []session.AudienceTarget{{Kind: session.TargetAgent, ID: target}},
	}
}

// thrCreateSession writes the session.create record both fixtures need.
func thrCreateSession(t *testing.T, ctx context.Context, store *session.JSONLStore, id string) {
	t.Helper()
	require.NoError(t, store.CreateSession(ctx, &session.Session{
		ID: id, Namespace: "acme", Kind: session.KindChannel, Title: "Threads",
		CreatedAt: thrAt(0), CreatedBy: session.AuthorRef{Agent: "atlas"},
	}, 1))
}

// postThreadFixture posts one thread root and two chained replies, every one of
// them stating its thread — the shape a writer produces (§5.1, CR-CHAT-005).
func postThreadFixture(t *testing.T, ctx context.Context, store *session.JSONLStore, base int64) {
	t.Helper()
	msgs := []*session.Message{
		{ID: thrThread, SessionID: thrSessionID, ThreadID: thrThread, Kind: session.MessagePlain,
			Author: *thrRef("atlas"), Payload: json.RawMessage(`{"text":"root"}`),
			Audience: thrAudience(session.AudienceSession, "nimbus"),
			Seq:      base, CreatedAt: thrAt(1)},
		{ID: thrThreadR1, SessionID: thrSessionID, ThreadID: thrThread, ParentID: thrThread,
			Kind: session.MessagePlain, Author: *thrRef("nimbus"), Payload: json.RawMessage(`{"text":"reply"}`),
			Audience: thrAudience(session.AudienceReplyDefault, "atlas"),
			Seq:      base + 1, CreatedAt: thrAt(2)},
		{ID: thrThreadR2, SessionID: thrSessionID, ThreadID: thrThread, ParentID: thrThreadR1,
			Kind: session.MessagePlain, Author: *thrRef("atlas"), Payload: json.RawMessage(`{"text":"deeper"}`),
			Audience: thrAudience(session.AudienceReplyDefault, "nimbus"),
			Seq:      base + 2, CreatedAt: thrAt(3)},
	}
	for _, m := range msgs {
		require.NoError(t, store.PostMessage(ctx, m))
	}
}

// legacyTranscript is four §5.1 lines in the pre-persisted shape: the message
// records carry NO thread_id, exactly as the CR-FEAT-004 generation wrote them.
func legacyTranscript() string {
	return strings.Join([]string{
		`{"v":1,"type":"session.create","session_id":"` + thrLegacySID + `","seq":1,"ts":"2026-10-04T05:00:00Z","namespace":"acme","kind":"channel","title":"Legacy","created_by":{"agent":"atlas"}}`,
		`{"v":1,"type":"session.message","session_id":"` + thrLegacySID + `","seq":2,"ts":"2026-10-04T05:00:01Z","message_id":"` + thrLegacyRoot + `","message_kind":"plain","author":{"agent":"atlas"},"payload":{"text":"root"},"audience":{"rule":"session","targets":[{"kind":"agent","id":"nimbus"}]}}`,
		`{"v":1,"type":"session.thread.reply","session_id":"` + thrLegacySID + `","seq":3,"ts":"2026-10-04T05:00:02Z","message_id":"` + thrLegacyReply + `","parent_id":"` + thrLegacyRoot + `","message_kind":"plain","author":{"agent":"nimbus"},"payload":{"text":"reply"},"audience":{"rule":"reply-default","targets":[{"kind":"agent","id":"atlas"}]}}`,
		`{"v":1,"type":"session.thread.reply","session_id":"` + thrLegacySID + `","seq":4,"ts":"2026-10-04T05:00:03Z","message_id":"` + thrLegacyDeep + `","parent_id":"` + thrLegacyReply + `","message_kind":"plain","author":{"agent":"atlas"},"payload":{"text":"deeper"},"audience":{"rule":"reply-default","targets":[{"kind":"agent","id":"nimbus"}]}}`,
	}, "\n") + "\n"
}

// idsOf renders a message slice as its ids, so a chain comparison reads as the
// chain it is.
func idsOf(msgs []*session.Message) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m.ID)
	}
	return out
}

// newLegacyStore writes the legacy transcript to a fresh log root and returns
// the store that reads it.
func newLegacyStore(t *testing.T) *session.JSONLStore {
	t.Helper()
	root := t.TempDir()
	store, err := session.NewJSONLStore(root)
	require.NoError(t, err)
	require.NoError(t, thrWriteFile(filepath.Join(root, thrLegacySID+".jsonl"), legacyTranscript()))
	return store
}

// TestThreadID_PersistedOnStoredMessages is the storage half of the row: a
// message written through the log states its thread_id in the BYTES on disk,
// and the reduced State reports the same key with nothing to backfill.
func TestThreadID_PersistedOnStoredMessages(t *testing.T) {
	ctx := context.Background()
	store, err := session.NewJSONLStore(t.TempDir())
	require.NoError(t, err)
	thrCreateSession(t, ctx, store, thrSessionID)
	postThreadFixture(t, ctx, store, 2)

	// The bytes: every stored message line carries the field.
	raw, err := thrReadAll(filepath.Join(store.Root(), thrSessionID+".jsonl"))
	require.NoError(t, err)
	messageLines := 0
	for _, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		rec, err := session.ParseRecord([]byte(line))
		require.NoError(t, err)
		if rec.Type != session.RecordMessage && rec.Type != session.RecordThreadReply {
			continue
		}
		messageLines++
		require.NotEmpty(t, rec.ThreadID, "a stored message record states thread_id: %s", line)
		require.Contains(t, line, `"thread_id":"`+rec.ThreadID+`"`, "thread_id is persisted on the line: %s", line)
	}
	require.Equal(t, 3, messageLines, "one message line per posted message")

	// The reduced view: the same keys, and nothing derived (the transcript
	// already states every one of them).
	st, err := store.Load(ctx, thrSessionID)
	require.NoError(t, err)
	require.Empty(t, st.Findings, "a transcript that states every key has no finding")
	require.Equal(t, thrThread, st.Message(thrThread).ThreadID, "a root's thread_id is its own message id (§4.3)")
	require.Equal(t, thrThread, st.Message(thrThreadR1).ThreadID)
	require.Equal(t, thrThread, st.Message(thrThreadR2).ThreadID, "a reply keeps its thread (D11)")

	// The thread tree is one row for the thread, materialised from the record
	// alone (§5.2: a root thread has no parent and no anchor).
	th := st.Thread(thrThread)
	require.NotNil(t, th)
	require.Equal(t, thrThread, th.RootMessageID)
	require.Empty(t, th.ParentThreadID)
	require.Empty(t, th.AnchorMessageID)
}

// TestThreadID_ReplyChainReconstructableFromStorageAlone is the §4.3
// acceptance property: given only the stored transcript, a reader groups by
// thread_id, orders by seq and attaches each record to its parent_id — with no
// side table, no membership lookup and no client state.
func TestThreadID_ReplyChainReconstructableFromStorageAlone(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := session.NewJSONLStore(root)
	require.NoError(t, err)
	thrCreateSession(t, ctx, store, thrSessionID)
	postThreadFixture(t, ctx, store, 2)

	st, err := store.Load(ctx, thrSessionID)
	require.NoError(t, err)

	// Group by thread_id, order by seq.
	group := st.ThreadMessages(thrThread)
	require.Equal(t, []string{thrThread, thrThreadR1, thrThreadR2}, idsOf(group),
		"the thread is one group in seq order")

	// Attach each record to its parent_id: the reply chain of the leaf.
	chain, complete := st.ReplyChain(thrThreadR2)
	require.True(t, complete)
	require.Equal(t, []string{thrThread, thrThreadR1, thrThreadR2}, idsOf(chain))

	// D11 (§4.5): a reply STAYS IN THREAD. Every message in the chain is at
	// the SAME level, and a level is the thread's — never a parent_id hop.
	for _, m := range group {
		require.Equal(t, 0, st.ThreadDepth(m.ThreadID),
			"D11: replying never deepens the thread; depth is the thread tree's")
	}

	// The transport form is the same lines, so a fresh store reads the same
	// tree — the chain does not depend on the store that wrote it.
	path, err := store.Export(ctx, thrSessionID, filepath.Join(root, "bundle"))
	require.NoError(t, err)
	fresh, err := session.NewJSONLStore(filepath.Join(root, "fresh"))
	require.NoError(t, err)
	fromBundle, err := fresh.LoadBundle(path)
	require.NoError(t, err)
	chain2, complete2 := fromBundle.ReplyChain(thrThreadR2)
	require.True(t, complete2)
	require.Equal(t, idsOf(chain), idsOf(chain2))
}

// TestThreadID_LegacyRecordWithoutThreadIDStillReads is the backward-compatible
// half: a record written before thread_id was stored — the CR-FEAT-004 shape —
// is not refused. The reader derives the key from the transcript (§4.3) and
// REPORTS that it did, and the write path is unchanged: it still refuses to
// emit a thread-less message.
func TestThreadID_LegacyRecordWithoutThreadIDStillReads(t *testing.T) {
	ctx := context.Background()
	store := newLegacyStore(t)

	st, err := store.Load(ctx, thrLegacySID)
	require.NoError(t, err, "a record written before thread_id was stored must still READ")

	// Derived keys: a root's is its own id, a reply's is its chain's root.
	require.Equal(t, thrLegacyRoot, st.Message(thrLegacyRoot).ThreadID)
	require.Equal(t, thrLegacyRoot, st.Message(thrLegacyReply).ThreadID)
	require.Equal(t, thrLegacyRoot, st.Message(thrLegacyDeep).ThreadID)

	// The derivation is a FINDING, not a silent repair (§4.3).
	require.Len(t, st.Findings, 3)
	for _, f := range st.Findings {
		require.Equal(t, session.FindingDerivedThread, f.Kind)
		require.Equal(t, thrLegacyRoot, f.ThreadID)
	}

	// And the reconstruction still holds on the derived keys.
	chain, complete := st.ReplyChain(thrLegacyDeep)
	require.True(t, complete)
	require.Equal(t, []string{thrLegacyRoot, thrLegacyReply, thrLegacyDeep}, idsOf(chain))
	require.Len(t, st.ThreadMessages(thrLegacyRoot), 3)

	// Read tolerance is ONE-DIRECTIONAL. The write path still requires the
	// field: a reply that states no thread is refused (a reply cannot derive
	// it — the builder has no transcript; the caller must state it).
	reply := &session.Message{ID: "m-no-thread", SessionID: thrLegacySID, ParentID: thrLegacyRoot,
		Kind: session.MessagePlain, Author: session.AuthorRef{Agent: "nimbus"},
		Audience: session.Audience{Rule: session.AudienceReplyDefault}, Seq: 9, CreatedAt: thrAt(9)}
	_, err = reply.Record().MarshalLine()
	require.ErrorIs(t, err, session.ErrInvalidRecord, "the writer never produces a thread-less record")
}

// TestThreadID_BrokenThreadIsReportedNotReRooted is §4.3's second half: a
// record whose thread_id names no root, or whose parent_id names a message
// that is not in the transcript, is a BROKEN THREAD — reported as a finding,
// never silently re-rooted or dropped.
func TestThreadID_BrokenThreadIsReportedNotReRooted(t *testing.T) {
	const sid = "s-broken"
	sess := (&session.Session{ID: sid, CreatedBy: session.AuthorRef{Agent: "atlas"}}).CreateRecord(1, thrAt(0))
	rootRec := (&session.Message{ID: "m1", SessionID: sid, ThreadID: "m1", Kind: session.MessagePlain,
		Author: session.AuthorRef{Agent: "atlas"}, Audience: thrAudience(session.AudienceSession, "nimbus"),
		Seq: 2, CreatedAt: thrAt(1)}).Record()
	// thread_id names a thread no root defines.
	ghost := (&session.Message{ID: "m-ghost", SessionID: sid, ThreadID: "no-such-root", ParentID: "m1",
		Kind: session.MessagePlain, Author: session.AuthorRef{Agent: "nimbus"},
		Audience: thrAudience(session.AudienceReplyDefault, "atlas"), Seq: 3, CreatedAt: thrAt(2)}).Record()
	// parent_id names a message that is not in the transcript.
	orphan := (&session.Message{ID: "m-orphan", SessionID: sid, ThreadID: "m1", ParentID: "no-such-message",
		Kind: session.MessagePlain, Author: session.AuthorRef{Agent: "nimbus"},
		Audience: thrAudience(session.AudienceReplyDefault, "atlas"), Seq: 4, CreatedAt: thrAt(3)}).Record()
	// The same brokenness in the legacy shape: no thread_id AND no parent.
	legacyOrphan := &session.Record{
		V: session.RecordFormatVersion, Type: session.RecordThreadReply, SessionID: sid, Seq: 5, TS: thrAt(4),
		MessageID: "m-legacy-orphan", ParentID: "no-such-parent", MessageKind: session.MessagePlain,
		Author: thrRef("nimbus"), Audience: &session.Audience{Rule: session.AudienceReplyDefault},
	}

	st, err := session.Replay(sid, []*session.Record{sess, rootRec, ghost, orphan, legacyOrphan})
	require.NoError(t, err, "the read path tolerates the thread-less legacy shape")

	// Nothing is re-rooted: the keys the records state are left exactly as the
	// records state them, and the record with no key keeps none.
	require.Equal(t, "no-such-root", st.Message("m-ghost").ThreadID)
	require.Equal(t, "m1", st.Message("m-orphan").ThreadID)
	require.Empty(t, st.Message("m-legacy-orphan").ThreadID)

	findings := map[string]session.ThreadFinding{}
	for _, f := range st.Findings {
		require.Equal(t, session.FindingBrokenThread, f.Kind)
		findings[f.MessageID] = f
	}
	require.Len(t, findings, 3)
	require.Equal(t, "no-such-root", findings["m-ghost"].ThreadID)
	require.Equal(t, "no-such-message", findings["m-orphan"].ParentID)
	require.Equal(t, "no-such-parent", findings["m-legacy-orphan"].ParentID)

	// The one whole thread is still reconstructable, and a broken leaf does not
	// fabricate a chain for itself: the walk stops where the transcript does.
	chain, complete := st.ReplyChain("m-orphan")
	require.False(t, complete, "a chain that leaves the transcript is not complete")
	require.Equal(t, []string{"m-orphan"}, idsOf(chain),
		"a parent_id that names nothing stops the walk — it is not repaired into m1")
	whole, wholeComplete := st.ReplyChain("m1")
	require.True(t, wholeComplete)
	require.Equal(t, []string{"m1"}, idsOf(whole))
}

// TestThreadID_MigrationStory_WriteBackPersistsDerivedKeys states the storage
// change's migration story as a test: the JSONL log is append-only and is never
// rewritten, so the derived key reaches the log on the NEXT record for each
// message — the §3.2 write-back a fan-out already performs — and the transcript
// then states every key on its own.
func TestThreadID_MigrationStory_WriteBackPersistsDerivedKeys(t *testing.T) {
	ctx := context.Background()
	store := newLegacyStore(t)

	before, err := store.Load(ctx, thrLegacySID)
	require.NoError(t, err)
	require.NotEmpty(t, before.Findings, "the legacy transcript needs a derivation to begin with")

	// Re-project each message at its own seq: the §3.2 write-back shape, which
	// keep-LAST resolves (§5.1). Nothing is rewritten in place.
	for _, m := range before.Messages {
		require.NoError(t, store.PostMessage(ctx, m))
	}

	after, err := store.Load(ctx, thrLegacySID)
	require.NoError(t, err)
	require.Empty(t, after.Findings, "every key is now stated on the record")
	chain, complete := after.ReplyChain(thrLegacyDeep)
	require.True(t, complete)
	require.Equal(t, []string{thrLegacyRoot, thrLegacyReply, thrLegacyDeep}, idsOf(chain))

	// The appended lines DO carry the field — that is the migration.
	raw, err := thrReadAll(filepath.Join(store.Root(), thrLegacySID+".jsonl"))
	require.NoError(t, err)
	require.Equal(t, 3, strings.Count(string(raw), `"thread_id":"`+thrLegacyRoot+`"`),
		"one appended line per message carries the derived thread_id")
}
