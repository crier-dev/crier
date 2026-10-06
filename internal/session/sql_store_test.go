package session

// CR-CHAT-006 acceptance battery for the shared SQL session store: the SQLite
// adapter proven against the SAME facts the JSONL log and the PostgreSQL view
// are measured against. Every test here runs with NO PostgreSQL service, which
// is the point of the SQLite backend.

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// newTestSQLiteStore returns the shared SQL store over a fresh SQLite database
// file, so each test starts from nothing and the "restart" tests can reopen a
// real path.
func newTestSQLiteStore(t *testing.T) *SQLStore {
	t.Helper()
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "sessions.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func appendRecords(t *testing.T, store Store, recs []*Record) {
	t.Helper()
	ctx := context.Background()
	for _, rec := range recs {
		require.NoError(t, store.Append(ctx, rec))
	}
}

// ---------------------------------------------------------------------------
// The adapter is the shared repository, and the view is the spec's shape
// ---------------------------------------------------------------------------

func TestSQLite_BackendNameAndSchemaShape(t *testing.T) {
	store := newTestSQLiteStore(t)
	require.Equal(t, "sqlite", store.Backend())

	ctx := context.Background()
	// Table and column names are fixed by the §5.2 view "so the view and the
	// client cannot invent two schemas" — assert them by name, on SQLite.
	for _, table := range []string{
		"chat_sessions", "chat_session_members", "chat_transcript",
		"chat_deliveries", "chat_threads", "chat_context_shares",
	} {
		var n int
		require.NoError(t, store.DB().QueryRowContext(ctx,
			`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table).Scan(&n))
		require.Equal(t, 1, n, "table %s must exist (§5.2)", table)
	}

	wantColumns := []string{
		"session_id", "seq", "message_id", "thread_id", "parent_id", "message_kind",
		"author_type", "author_id", "principal_id", "payload", "guard", "audience", "created_at",
	}
	rows, err := store.DB().QueryContext(ctx, `PRAGMA table_info(chat_transcript)`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	got := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt any
		require.NoError(t, rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk))
		got[name] = true
	}
	require.NoError(t, rows.Err())
	for _, col := range wantColumns {
		require.True(t, got[col], "chat_transcript.%s must exist (§5.2)", col)
	}
}

// ---------------------------------------------------------------------------
// CR-CHAT-006 acceptance: the same facts on every backend
// ---------------------------------------------------------------------------

func TestSQLite_ParityWithTheLogAndTheBundle(t *testing.T) {
	ctx := context.Background()
	store := newTestSQLiteStore(t)
	recs := buildScenario()
	appendRecords(t, store, recs)

	fromSQL, err := store.Load(ctx, fixSessionID)
	require.NoError(t, err)
	fromLog, err := Replay(fixSessionID, recs)
	require.NoError(t, err)

	// The SQLite view reduces to the SAME State the ordered log does — the
	// one-value acceptance, on a backend that needs no PostgreSQL.
	requireJSONEqual(t, fromLog, fromSQL)

	// ... and the portable bundle (the transport form) imports into a FRESH
	// SQLite view with the same result, which is the "same bundle, equivalent
	// reads" acceptance.
	logStore, err := NewJSONLStore(t.TempDir())
	require.NoError(t, err)
	appendRecords(t, logStore, recs)
	bundlePath, err := logStore.Export(ctx, fixSessionID, t.TempDir())
	require.NoError(t, err)

	fresh := newTestSQLiteStore(t)
	applied, err := ImportBundle(ctx, fresh, bundlePath)
	require.NoError(t, err)
	require.Equal(t, 13, applied, "keep-LAST collapses the write-back duplicate")
	fromBundle, err := fresh.Load(ctx, fixSessionID)
	require.NoError(t, err)
	requireJSONEqual(t, fromLog, fromBundle)

	require.NotNil(t, fromBundle.Session.ClosedAt)
	require.Equal(t, at(13), *fromBundle.Session.ClosedAt)
}

func TestSQLite_DuplicateReplayIsIdempotentAndKeepLast(t *testing.T) {
	ctx := context.Background()
	store := newTestSQLiteStore(t)
	recs := buildScenario()
	appendRecords(t, store, recs)
	before, err := store.Load(ctx, fixSessionID)
	require.NoError(t, err)

	// Replaying the whole log a second time is a byte-identical replay at
	// every (session_id, seq) — a no-op (§4.1).
	appendRecords(t, store, recs)
	after, err := store.Load(ctx, fixSessionID)
	require.NoError(t, err)
	requireJSONEqual(t, before, after)

	// keep-LAST: the fan-out outcomes written back at the same seq win.
	task := after.Message(fixTaskMsg)
	require.Len(t, task.Outcomes, 1)
	require.Equal(t, OutcomeDelivered, task.Outcomes[0].Outcome)

	// The view can re-derive the message records it justifies and re-append
	// them — again a no-op, so a re-projection can never duplicate a message.
	rerecs, err := store.Records(ctx, fixSessionID)
	require.NoError(t, err)
	require.NotEmpty(t, rerecs)
	appendRecords(t, store, rerecs)
	st, err := store.Load(ctx, fixSessionID)
	require.NoError(t, err)
	require.Len(t, st.Messages, 4)
}

func TestSQLite_RestartRecoversIdenticalState(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "restart.sqlite")

	store, err := NewSQLiteStore(path)
	require.NoError(t, err)
	appendRecords(t, store, buildScenario())
	before, err := store.Load(ctx, fixSessionID)
	require.NoError(t, err)
	require.NoError(t, store.Close())

	// A fresh process over the same file: the durable view, not a cache.
	reopened, err := NewSQLiteStore(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = reopened.Close() })
	after, err := reopened.Load(ctx, fixSessionID)
	require.NoError(t, err)
	requireJSONEqual(t, before, after)

	// The schema is applied idempotently on the re-open.
	require.Equal(t, SessionClosed, after.Session.State)
	require.Len(t, after.Threads, 3)
	require.Len(t, after.Messages, 4)
}

// TestSQLite_CleanInstallLifecycleSurvivesRestart is criterion 1: a deployment
// with ONLY a JSONL log and a SQLite file creates a session, appends and
// retrieves messages, branches a thread, restarts, and recovers identical
// state — with no PostgreSQL URL anywhere.
func TestSQLite_CleanInstallLifecycleSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	logRoot := filepath.Join(root, "log")
	dbPath := filepath.Join(root, "view.sqlite")

	log, err := NewJSONLStore(logRoot)
	require.NoError(t, err)
	view, err := OpenStore(ctx, BackendSQLite, StoreOptions{SQLitePath: dbPath})
	require.NoError(t, err, "the SQLite backend needs no database URL")
	require.Equal(t, BackendSQLite, view.(*SQLStore).Backend())

	// The log is the allocator (§3.1) and the transport form; the view is
	// projected from it, one direction at a time (§2.2).
	write := func(makeRec func(seq int64) *Record) {
		t.Helper()
		seq, err := log.NextSeq(ctx, "sess-clean-install")
		require.NoError(t, err)
		rec := makeRec(seq)
		require.NoError(t, log.Append(ctx, rec))
		_, err = ProjectLog(ctx, view, []*Record{rec})
		require.NoError(t, err)
	}

	write(func(seq int64) *Record {
		return (&Session{ID: "sess-clean-install", Kind: KindChannel, Title: "Clean install",
			CreatedAt: at(0), CreatedBy: AuthorRef{Agent: "atlas"}}).CreateRecord(seq, at(0))
	})
	write(func(seq int64) *Record {
		return (&Member{SessionID: "sess-clean-install", MemberType: MemberAgent, MemberID: "atlas",
			Role: RoleOwner, AddedBy: "atlas"}).AddRecord(seq, at(1), nil)
	})
	write(func(seq int64) *Record {
		return (&Message{ID: "m1", SessionID: "sess-clean-install", ThreadID: "m1", Kind: MessagePlain,
			Author: AuthorRef{Agent: "atlas"}, Payload: json.RawMessage(`{"text":"hello"}`),
			Audience: Audience{Rule: AudienceSession, Targets: []AudienceTarget{{Kind: TargetAgent, ID: "atlas"}}},
			Seq:      seq, CreatedAt: at(2)}).Record()
	})
	write(func(seq int64) *Record {
		return (&Message{ID: "m2", SessionID: "sess-clean-install", ThreadID: "m1", ParentID: "m1",
			Kind: MessagePlain, Author: AuthorRef{Agent: "atlas"},
			Payload:  json.RawMessage(`{"text":"reply"}`),
			Audience: Audience{Rule: AudienceReplyDefault, Targets: []AudienceTarget{{Kind: TargetAgent, ID: "atlas"}}},
			Seq:      seq, CreatedAt: at(3)}).Record()
	})
	// A deliberate branch — the only record that creates a thread (§4.5).
	write(func(seq int64) *Record {
		return (&Thread{ID: "thr1", SessionID: "sess-clean-install", ParentThreadID: "m1",
			RootMessageID: "thr1", AnchorMessageID: "m1", CreatedBy: AuthorRef{Agent: "atlas"},
			CreatedAt: at(4)}).BranchRecord(seq, at(4), "split")
	})

	got, err := view.Load(ctx, "sess-clean-install")
	require.NoError(t, err)
	require.Len(t, got.Messages, 2)
	require.Equal(t, "m1", got.Messages[1].ThreadID, "a reply stays in its thread")
	require.Equal(t, 1, got.ThreadDepth("thr1"), "the branch created the level")
	require.NotNil(t, got.Thread("thr1"))

	// Reconciliation: the log and the view are in agreement, and a mismatch
	// would be REPORTED rather than healed.
	logRecs, err := log.Records(ctx, "sess-clean-install")
	require.NoError(t, err)
	reconciled, err := Reconcile(ctx, view, "sess-clean-install", logRecs)
	require.NoError(t, err)
	requireJSONEqual(t, got, reconciled)

	// Restart: close both, reopen the same paths, re-project the log.
	require.NoError(t, view.Close())
	require.NoError(t, log.Close())

	log2, err := NewJSONLStore(logRoot)
	require.NoError(t, err)
	t.Cleanup(func() { _ = log2.Close() })
	view2, err := OpenStore(ctx, BackendSQLite, StoreOptions{SQLitePath: dbPath})
	require.NoError(t, err)
	t.Cleanup(func() { _ = view2.Close() })

	logRecs2, err := log2.Records(ctx, "sess-clean-install")
	require.NoError(t, err)
	_, err = ProjectLog(ctx, view2, logRecs2)
	require.NoError(t, err)
	after, err := view2.Load(ctx, "sess-clean-install")
	require.NoError(t, err)
	requireJSONEqual(t, got, after)
}

// TestSQLite_ReconcileReportsDivergenceReadOnly proves the reconciler is
// read-only: a view that is AHEAD of the log is reported, not rolled back.
func TestSQLite_ReconcileReportsDivergenceReadOnly(t *testing.T) {
	ctx := context.Background()
	store := newTestSQLiteStore(t)
	recs := buildScenario()
	appendRecords(t, store, recs)

	_, err := Reconcile(ctx, store, fixSessionID, recs)
	require.NoError(t, err, "in sync")

	// A log that stops before the close: the view (which has it) diverges.
	stale := recs[:12]
	_, err = Reconcile(ctx, store, fixSessionID, stale)
	require.Error(t, err)
	require.Contains(t, err.Error(), "diverge")
	require.Contains(t, err.Error(), "sha256=")

	// Nothing was healed: the view still carries the close it already had.
	after, err := store.Load(ctx, fixSessionID)
	require.NoError(t, err)
	require.Equal(t, SessionClosed, after.Session.State)
	require.NotNil(t, after.Session.ClosedAt)
}

func TestSQLite_UnknownSessionIsNotAnEmptySession(t *testing.T) {
	store := newTestSQLiteStore(t)
	_, err := store.Load(context.Background(), "does-not-exist")
	require.ErrorIs(t, err, ErrSessionNotFound)
	_, err = store.Load(context.Background(), "")
	require.ErrorIs(t, err, ErrInvalidRecord)
}

// TestSQLite_CRUDSurfaceWritesTheView drives the session CRUD surface against
// the SQLite tables: create → add member (with a late-join context answer) →
// post a root and a reply → read it all back → close → reopen.
func TestSQLite_CRUDSurfaceWritesTheView(t *testing.T) {
	ctx := context.Background()
	store := newTestSQLiteStore(t)

	sess := &Session{ID: "sess-sqlite-crud", Namespace: "acme", Kind: KindDirect, Title: "1:1",
		CreatedAt: at(0), CreatedBy: AuthorRef{Principal: "prin_bane", AsAgent: "atlas"}}
	require.NoError(t, store.CreateSession(ctx, sess, 1))
	require.NoError(t, store.AddMember(ctx, &Member{
		SessionID: sess.ID, MemberType: MemberAgent, MemberID: "atlas", Role: RoleOwner,
		AddedAt: at(1), AddedBy: "atlas"}, 2, &ContextShare{Mode: ShareSummary}))

	root := &Message{ID: "m-1", SessionID: sess.ID, ThreadID: "m-1", Kind: MessagePlain,
		Author: AuthorRef{Agent: "atlas"}, Payload: json.RawMessage(`{"text":"hello"}`),
		Audience: Audience{Rule: AudienceSession, Targets: []AudienceTarget{{Kind: TargetAgent, ID: "atlas"}}},
		Seq:      3, CreatedAt: at(2)}
	require.NoError(t, store.PostMessage(ctx, root))

	reply := &Message{ID: "m-2", SessionID: sess.ID, ThreadID: "m-1", ParentID: "m-1",
		Kind: MessagePlain, Author: AuthorRef{Agent: "atlas"}, Payload: json.RawMessage(`{"text":"reply"}`),
		Audience: Audience{Rule: AudienceReplyDefault, Targets: []AudienceTarget{{Kind: TargetAgent, ID: "atlas"}}},
		Seq:      4, CreatedAt: at(3)}
	require.NoError(t, store.PostMessage(ctx, reply))

	got, err := store.Session(ctx, sess.ID)
	require.NoError(t, err)
	require.Equal(t, KindDirect, got.Kind)
	require.Equal(t, SessionOpen, got.State)
	require.Equal(t, AuthorRef{Principal: "prin_bane", AsAgent: "atlas"}, got.CreatedBy)

	members, err := store.Members(ctx, sess.ID)
	require.NoError(t, err)
	require.Len(t, members, 1)
	require.Equal(t, RoleOwner, members[0].Role)

	msgs, err := store.Messages(ctx, sess.ID)
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	require.Equal(t, "m-1", msgs[1].ThreadID, "the reply stayed in its thread")
	require.Equal(t, "m-1", msgs[1].ParentID)

	shares, err := store.ContextShares(ctx, sess.ID)
	require.NoError(t, err)
	require.Len(t, shares, 1)
	require.Equal(t, ShareSummary, shares[0].Mode)

	threads, err := store.Threads(ctx, sess.ID)
	require.NoError(t, err)
	require.Len(t, threads, 1, "a reply creates no thread")
	require.Empty(t, threads[0].ParentThreadID)

	require.NoError(t, store.CloseSession(ctx, sess.ID, 5, at(4), AuthorRef{Agent: "atlas"}, "done"))
	closed, err := store.Session(ctx, sess.ID)
	require.NoError(t, err)
	require.Equal(t, SessionClosed, closed.State)
	require.NotNil(t, closed.ClosedAt)

	require.NoError(t, store.ReopenSession(ctx, sess.ID, 6, at(5), AuthorRef{Agent: "atlas"}))
	reopened, err := store.Session(ctx, sess.ID)
	require.NoError(t, err)
	require.Equal(t, SessionOpen, reopened.State)
	require.Nil(t, reopened.ClosedAt)

	// The transcript survived the lifecycle change.
	msgs, err = store.Messages(ctx, sess.ID)
	require.NoError(t, err)
	require.Len(t, msgs, 2)
}

// ---------------------------------------------------------------------------
// Backend selection (CR_SESSION_BACKEND)
// ---------------------------------------------------------------------------

func TestOpenStore_BackendSelection(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	sqlStore, err := OpenStore(ctx, BackendSQLite, StoreOptions{SQLitePath: filepath.Join(dir, "s.sqlite")})
	require.NoError(t, err)
	require.Equal(t, BackendSQLite, sqlStore.(*SQLStore).Backend())
	require.NoError(t, sqlStore.Close())

	logStore, err := OpenStore(ctx, BackendJSONL, StoreOptions{LogRoot: filepath.Join(dir, "log")})
	require.NoError(t, err)
	_, ok := logStore.(*JSONLStore)
	require.True(t, ok, "the jsonl backend is the ordered append log")
	require.NoError(t, logStore.Close())

	// postgres needs a URL; no service is required by the SQLite path, which
	// is the whole point.
	_, err = OpenStore(ctx, BackendPostgres, StoreOptions{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "database URL")

	// "auto" is a config-layer policy, refused here so the engine is explicit.
	_, err = OpenStore(ctx, BackendAuto, StoreOptions{SQLitePath: filepath.Join(dir, "x.sqlite")})
	require.Error(t, err)
	require.Contains(t, err.Error(), "empty")

	_, err = OpenStore(ctx, "mysql", StoreOptions{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown backend")

	_, err = OpenStore(ctx, BackendSQLite, StoreOptions{})
	require.ErrorIs(t, err, ErrInvalidRecord)
}

// TestSQLite_ConcurrentAppendsAreSerialised exercises the single-writer
// posture: two goroutines appending at distinct seqs both land, with no
// SQLITE_BUSY surfacing as a failed append.
func TestSQLite_ConcurrentAppendsAreSerialised(t *testing.T) {
	ctx := context.Background()
	store := newTestSQLiteStore(t)

	sess := &Session{ID: "sess-conc", Kind: KindChannel, CreatedAt: at(0), CreatedBy: AuthorRef{Agent: "atlas"}}
	require.NoError(t, store.CreateSession(ctx, sess, 1))

	const n = 12
	errc := make(chan error, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			m := &Message{ID: "cm-" + string(rune('a'+i)), SessionID: "sess-conc",
				Kind: MessagePlain, Author: AuthorRef{Agent: "atlas"},
				Payload: json.RawMessage(`{"text":"c"}`),
				Audience: Audience{Rule: AudienceSession,
					Targets: []AudienceTarget{{Kind: TargetAgent, ID: "atlas"}}},
				Seq: int64(2 + i), CreatedAt: at(1 + i)}
			m.ThreadID = m.ID
			errc <- store.PostMessage(ctx, m)
		}(i)
	}
	for i := 0; i < n; i++ {
		require.NoError(t, <-errc)
	}
	msgs, err := store.Messages(ctx, "sess-conc")
	require.NoError(t, err)
	require.Len(t, msgs, n)
}
