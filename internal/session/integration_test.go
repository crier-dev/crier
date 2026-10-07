//go:build integration

// The integration battery runs against a REAL PostgreSQL, because the claims
// it checks are SQL-level claims an in-memory fake cannot make: that the §5.2
// tables exist under their fixed names, that keep-LAST is enforced by the
// upsert, and that the PostgreSQL projection and the JSONL log reduce to the
// SAME session State (CR-CHAT-002's acceptance).
//
// The build tag matches the repo's existing convention (internal/registry's
// Postgres tests): `make test` and the commit guard stay database-free and
// fast, and the battery runs with
//
//	go test -tags=integration -count=1 -timeout 5m ./internal/session
package session

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/crier-dev/crier/internal/registry"
)

var testDSN string

func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	const (
		pgUser = "test"
		pgPass = "test"
		pgDB   = "test"
	)
	ctr, err := tcpostgres.Run(ctx,
		"postgres:16-alpine",
		tcpostgres.WithDatabase(pgDB),
		tcpostgres.WithUsername(pgUser),
		tcpostgres.WithPassword(pgPass),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "postgres container start: %v\n", err)
		os.Exit(1)
	}
	host, err := ctr.Host(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "postgres host: %v\n", err)
		_ = ctr.Terminate(context.Background())
		os.Exit(1)
	}
	port, err := ctr.MappedPort(ctx, "5432/tcp")
	if err != nil {
		fmt.Fprintf(os.Stderr, "postgres mapped port: %v\n", err)
		_ = ctr.Terminate(context.Background())
		os.Exit(1)
	}
	testDSN = fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=disable", pgUser, pgPass, host, port.Port(), pgDB)

	code := m.Run()

	termCtx, termCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer termCancel()
	if err := ctr.Terminate(termCtx); err != nil {
		fmt.Fprintf(os.Stderr, "postgres terminate: %v\n", err)
	}
	os.Exit(code)
}

// newTestPostgresStore returns a session store over a schema wiped clean, so
// each test starts from nothing and cannot pass on a sibling's leftovers.
func newTestPostgresStore(t *testing.T) *PostgresStore {
	t.Helper()
	ctx := context.Background()
	store, err := NewPostgresStore(ctx, testDSN)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	// chat_threads/chat_transcript/... all cascade from chat_sessions.
	_, err = store.pool.Exec(ctx, `DROP TABLE IF EXISTS
		chat_deliveries, chat_transcript, chat_context_shares, chat_threads, chat_session_members, chat_sessions CASCADE`)
	require.NoError(t, err)
	require.NoError(t, store.ApplySchema(ctx))
	return store
}

// newTestRegistry returns the SHIPPED registry store over the same database,
// so the fan-out test exercises the real durable-inbox write rather than a
// session-local imitation of it.
func newTestRegistry(t *testing.T) *registry.PostgresStore {
	t.Helper()
	ctx := context.Background()

	// Wipe the registry's own schema first (the store runs migrations on
	// open), so each run starts from nothing.
	pool, err := pgxpool.New(ctx, testDSN)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `DROP TABLE IF EXISTS dead_letters, inbox_entries, agents, schema_migrations CASCADE`)
	require.NoError(t, err)
	pool.Close()

	store, err := registry.NewPostgresStore(ctx, testDSN)
	require.NoError(t, err)
	t.Cleanup(store.Close)
	return store
}

func registerAgent(t *testing.T, store *registry.PostgresStore, id string) {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	require.NoError(t, store.Register(&registry.Agent{
		ID:           id,
		PublicKey:    registry.HexKey(pub),
		Capabilities: []string{"chat"},
		Status:       registry.StatusOnline,
	}))
}

// ---------------------------------------------------------------------------
// The §5.2 schema is the fixed shape the spec names
// ---------------------------------------------------------------------------

func TestPostgres_SchemaShapeIsTheSpecShape(t *testing.T) {
	store := newTestPostgresStore(t)
	ctx := context.Background()

	// Table and column names are fixed by §5.2 "so the view and the client
	// cannot invent two schemas" — assert them by name.
	wantColumns := map[string][]string{
		"chat_sessions": {"id", "namespace", "kind", "title", "created_by", "created_at",
			"state", "closed_at", "retention_seconds", "group_id", "visibility"},
		"chat_session_members": {"session_id", "member_type", "member_id", "role",
			"added_at", "removed_at", "added_by", "removed_by"},
		"chat_transcript": {"session_id", "seq", "message_id", "thread_id", "parent_id",
			"message_kind", "author_type", "author_id", "principal_id", "payload", "guard",
			"audience", "created_at"},
		"chat_deliveries": {"session_id", "message_id", "target_agent_id", "inbox_entry_id",
			"outcome", "updated_at"},
		"chat_threads": {"thread_id", "session_id", "parent_thread_id", "root_message_id",
			"anchor_message_id", "created_by", "created_at"},
		"chat_context_shares": {"session_id", "member_type", "member_id", "mode",
			"boundary_message_id", "set_by", "set_at"},
	}
	for table, cols := range wantColumns {
		for _, col := range cols {
			var exists bool
			err := store.pool.QueryRow(ctx, `
				SELECT EXISTS (
					SELECT 1 FROM information_schema.columns
					WHERE table_name = $1 AND column_name = $2)`, table, col).Scan(&exists)
			require.NoError(t, err)
			require.True(t, exists, "%s.%s must exist (§5.2)", table, col)
		}
	}
}

func TestPostgres_TranscriptOrderingKeyIsSessionSeq(t *testing.T) {
	store := newTestPostgresStore(t)
	ctx := context.Background()
	for _, rec := range buildScenario() {
		require.NoError(t, store.Append(ctx, rec))
	}
	// (session_id, seq) is the ordering key, so the same seq can be re-appended
	// (the write-back case) but a DIFFERENT session_id is a separate row space.
	var n int
	require.NoError(t, store.pool.QueryRow(ctx,
		`SELECT count(*) FROM chat_transcript WHERE session_id = $1`, fixSessionID).Scan(&n))
	require.Equal(t, 4, n, "one transcript row per distinct message, not per line")
}

// ---------------------------------------------------------------------------
// CR-CHAT-002 acceptance: the same facts on both backends
// ---------------------------------------------------------------------------

func TestPostgres_RoundTripsIdenticalContentToTheLog(t *testing.T) {
	ctx := context.Background()
	store := newTestPostgresStore(t)
	recs := buildScenario()
	for _, rec := range recs {
		require.NoError(t, store.Append(ctx, rec))
	}

	fromPostgres, err := store.Load(ctx, fixSessionID)
	require.NoError(t, err)
	fromLog, err := Replay(fixSessionID, recs)
	require.NoError(t, err)

	// A session round-trips through Postgres AND through a JSONL bundle with
	// IDENTICAL content — the acceptance criterion, asserted as one equality.
	requireJSONEqual(t, fromLog, fromPostgres)

	// And the bundle half, on the same records.
	logStore, err := NewJSONLStore(t.TempDir())
	require.NoError(t, err)
	for _, rec := range recs {
		require.NoError(t, logStore.Append(ctx, rec))
	}
	bundlePath, err := logStore.Export(ctx, fixSessionID, t.TempDir())
	require.NoError(t, err)
	fresh, err := NewJSONLStore(filepath.Join(t.TempDir(), "fresh"))
	require.NoError(t, err)
	fromBundle, err := fresh.LoadBundle(bundlePath)
	require.NoError(t, err)
	requireJSONEqual(t, fromPostgres, fromBundle)

	require.NotNil(t, fromPostgres.Session.ClosedAt)
	require.Equal(t, at(13), *fromPostgres.Session.ClosedAt)
}

func TestPostgres_ReAppendIsIdempotentAndKeepLast(t *testing.T) {
	ctx := context.Background()
	store := newTestPostgresStore(t)
	recs := buildScenario()
	for _, rec := range recs {
		require.NoError(t, store.Append(ctx, rec))
	}
	// Replaying the whole log a second time changes nothing (every
	// (session_id, seq) is a byte-identical replay, §4.1).
	before, err := store.Load(ctx, fixSessionID)
	require.NoError(t, err)
	for _, rec := range recs {
		require.NoError(t, store.Append(ctx, rec))
	}
	after, err := store.Load(ctx, fixSessionID)
	require.NoError(t, err)
	requireJSONEqual(t, before, after)

	// keep-LAST: the LAST line for a seq wins, and the fan-out outcomes written
	// back at the same seq are what the view holds.
	task := after.Message(fixTaskMsg)
	require.Len(t, task.Outcomes, 1)
	require.Equal(t, OutcomeDelivered, task.Outcomes[0].Outcome)
}

func TestPostgres_UnknownSessionIsNotAnEmptySession(t *testing.T) {
	store := newTestPostgresStore(t)
	_, err := store.Load(context.Background(), "does-not-exist")
	require.ErrorIs(t, err, ErrSessionNotFound)
	_, err = store.Load(context.Background(), "")
	require.ErrorIs(t, err, ErrInvalidRecord)
}

// ---------------------------------------------------------------------------
// Fan-out through the SHIPPED durable inbox (§3.4, D1)
// ---------------------------------------------------------------------------

func TestPostgres_FanoutDeliversThroughTheShippedInboxPath(t *testing.T) {
	ctx := context.Background()
	reg := newTestRegistry(t)
	for _, id := range []string{"atlas", "nimbus", "kappa"} {
		registerAgent(t, reg, id)
	}

	msg := &Message{
		ID: "msg_pg_fanout", SessionID: fixSessionID, ThreadID: "msg_pg_fanout",
		Kind: MessagePlain, Author: *agentRef("atlas"),
		Payload: json.RawMessage(`{"text":"three inboxes"}`),
	}
	outcomes, err := Fanout(ctx, reg, msg, []string{"nimbus", "kappa", "atlas", "ghost"}, nil)
	require.NoError(t, err)
	require.Len(t, outcomes, 4)

	// All three registered members hold the SAME message in their own durable
	// inbox — reached through the shipped lease/ack path, not a session-local
	// queue.
	for _, target := range []string{"atlas", "nimbus", "kappa"} {
		entries, leaseID, err := reg.Retrieve(target, time.Minute, 10)
		require.NoError(t, err)
		require.Len(t, entries, 1, "%s has exactly one delivery", target)
		require.NotEmpty(t, leaseID)
		require.Equal(t, InboxEntryID("msg_pg_fanout", target), entries[0].ID)
		require.Equal(t, "atlas", entries[0].Sender)
		require.Equal(t, IdempotencyKey(fixSessionID, "msg_pg_fanout", "msg_pg_fanout", target),
			entries[0].IdempotencyKey)
	}

	// The unknown member is a NAMED refusal, not a vanished delivery.
	refused := map[string]DeliveryOutcome{}
	for _, o := range outcomes {
		refused[o.Target] = o
	}
	require.Equal(t, OutcomeRefused, refused["ghost"].Outcome)
	require.Contains(t, refused["ghost"].Reason, "not found")

	// A retried fan-out stores nothing new.
	before := inboxDepth(t, reg, "nimbus")
	_, err = Fanout(ctx, reg, msg, []string{"nimbus", "kappa", "atlas", "ghost"}, nil)
	require.NoError(t, err)
	require.Equal(t, before, inboxDepth(t, reg, "nimbus"))
}

func inboxDepth(t *testing.T, reg *registry.PostgresStore, id string) int {
	t.Helper()
	depth, _, _, err := reg.Stats(id)
	require.NoError(t, err)
	return depth
}

// ---------------------------------------------------------------------------
// The load-bearing rules, proven against the real view
// ---------------------------------------------------------------------------

func TestPostgres_DepthRuleAndBranchHoldInTheView(t *testing.T) {
	ctx := context.Background()
	store := newTestPostgresStore(t)
	for _, rec := range buildScenario() {
		require.NoError(t, store.Append(ctx, rec))
	}
	st, err := store.Load(ctx, fixSessionID)
	require.NoError(t, err)

	// A reply sets parent_id but not thread_id, and creates no thread.
	reply := st.Message(fixReplyMsg)
	require.Equal(t, fixRootMsg, reply.ParentID)
	require.Equal(t, fixRootMsg, reply.ThreadID)
	require.Nil(t, st.Thread(fixReplyMsg))
	require.Equal(t, 0, st.ThreadDepth(fixRootMsg))

	// The branch is what creates the level, with its parent_thread reference
	// and its anchor.
	child := st.Thread(fixBranchMsg)
	require.NotNil(t, child)
	require.Equal(t, fixRootMsg, child.ParentThreadID)
	require.Equal(t, fixRootMsg, child.AnchorMessageID)
	require.Equal(t, 1, st.ThreadDepth(fixBranchMsg))

	// The late-join context answer is queryable as a latest-state row.
	require.Len(t, st.ContextShares, 1)
	require.Equal(t, ShareFull, st.ContextShares[0].Mode)

	// Membership at T is a query over added_at/removed_at (§5.2).
	require.Len(t, st.MembersAt(at(9)), 4)
	require.Len(t, st.MembersAt(at(13)), 3)
}

func TestPostgres_RecordFromTheViewIsRereadable(t *testing.T) {
	ctx := context.Background()
	store := newTestPostgresStore(t)
	for _, rec := range buildScenario() {
		require.NoError(t, store.Append(ctx, rec))
	}
	// The view can re-derive the message records it justifies (§4.3: a
	// thread is reconstructable from the records alone), and re-appending one
	// is an idempotent no-op.
	recs, err := store.Records(ctx, fixSessionID)
	require.NoError(t, err)
	require.NotEmpty(t, recs)
	for _, rec := range recs {
		require.NoError(t, store.Append(ctx, rec))
	}
	st, err := store.Load(ctx, fixSessionID)
	require.NoError(t, err)
	require.Len(t, st.Messages, 4)
}

// TestPostgres_CRUDSurfaceWritesTheView drives the session CRUD surface
// against the real §5.2 tables: create → add member (with a late-join context
// answer) → post a root and a reply → close → reopen, then reads it all back.
func TestPostgres_CRUDSurfaceWritesTheView(t *testing.T) {
	ctx := context.Background()
	store := newTestPostgresStore(t)

	sess := &Session{ID: "sess-pg-crud", Namespace: "acme", Kind: KindDirect, Title: "1:1",
		CreatedAt: at(0), CreatedBy: AuthorRef{Principal: "prin_bane", AsAgent: "atlas"}}
	require.NoError(t, store.CreateSession(ctx, sess, 1))
	require.NoError(t, store.AddMember(ctx, &Member{
		SessionID: sess.ID, MemberType: MemberAgent, MemberID: "atlas", Role: RoleOwner,
		AddedAt: at(1), AddedBy: "atlas"}, 2, &ContextShare{Mode: ShareSummary}))

	root := &Message{ID: "m-pg-1", SessionID: sess.ID, ThreadID: "m-pg-1", Kind: MessagePlain,
		Author: AuthorRef{Agent: "atlas"}, Payload: json.RawMessage(`{"text":"hello"}`),
		Audience: Audience{Rule: AudienceSession, Targets: []AudienceTarget{{Kind: TargetAgent, ID: "atlas"}}},
		Seq:      3, CreatedAt: at(2)}
	require.NoError(t, store.PostMessage(ctx, root))

	reply := &Message{ID: "m-pg-2", SessionID: sess.ID, ThreadID: "m-pg-1", ParentID: "m-pg-1",
		Kind: MessagePlain, Author: AuthorRef{Agent: "atlas"}, Payload: json.RawMessage(`{"text":"reply"}`),
		Audience: Audience{Rule: AudienceReplyDefault, Targets: []AudienceTarget{{Kind: TargetAgent, ID: "atlas"}}},
		Seq:      4, CreatedAt: at(3)}
	require.NoError(t, store.PostMessage(ctx, reply))

	got, err := store.Session(ctx, sess.ID)
	require.NoError(t, err)
	require.Equal(t, KindDirect, got.Kind, "a DM is the same object with kind: direct")
	require.Equal(t, SessionOpen, got.State)

	members, err := store.Members(ctx, sess.ID)
	require.NoError(t, err)
	require.Len(t, members, 1)
	require.Equal(t, RoleOwner, members[0].Role)

	msgs, err := store.Messages(ctx, sess.ID)
	require.NoError(t, err)
	require.Len(t, msgs, 2)
	require.Equal(t, "m-pg-1", msgs[1].ThreadID, "the reply stayed in its thread")
	require.Equal(t, "m-pg-1", msgs[1].ParentID)

	shares, err := store.ContextShares(ctx, sess.ID)
	require.NoError(t, err)
	require.Len(t, shares, 1)
	require.Equal(t, ShareSummary, shares[0].Mode)

	threads, err := store.Threads(ctx, sess.ID)
	require.NoError(t, err)
	require.Len(t, threads, 1, "a reply creates no thread")
	require.Empty(t, threads[0].ParentThreadID)

	// Close then reopen: closing is not a delete, and reopen is an event.
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
// CR-CHAT-006 acceptance: ONE portable bundle, SQLite and PostgreSQL, equivalent
// ---------------------------------------------------------------------------

// TestDualBackend_BundleImportsIntoSQLiteAndPostgresEqually is the second
// CR-CHAT-006 acceptance: the SAME portable bundle imports into SQLite and into
// PostgreSQL with equivalent reads. The bundle is the transport form (the
// JSONL log, filtered and copied verbatim), so importing it into either view is
// a replay, and the two engines must agree on the State it produces.
func TestDualBackend_BundleImportsIntoSQLiteAndPostgresEqually(t *testing.T) {
	ctx := context.Background()
	recs := buildScenario()

	// The portable bundle, written from an ordinary JSONL log.
	logStore, err := NewJSONLStore(t.TempDir())
	require.NoError(t, err)
	for _, rec := range recs {
		require.NoError(t, logStore.Append(ctx, rec))
	}
	bundlePath, err := logStore.Export(ctx, fixSessionID, t.TempDir())
	require.NoError(t, err)

	// Into SQLite — no PostgreSQL service in this leg at all.
	sqliteStore, err := NewSQLiteStore(filepath.Join(t.TempDir(), "view.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqliteStore.Close() })
	applied, err := ImportBundle(ctx, sqliteStore, bundlePath)
	require.NoError(t, err)
	require.Equal(t, 13, applied)

	// Into PostgreSQL — the shipped pgxpool view, same records, same order.
	pgStore := newTestPostgresStore(t)
	applied, err = ImportBundle(ctx, pgStore, bundlePath)
	require.NoError(t, err)
	require.Equal(t, 13, applied)

	fromSQLite, err := sqliteStore.Load(ctx, fixSessionID)
	require.NoError(t, err)
	fromPostgres, err := pgStore.Load(ctx, fixSessionID)
	require.NoError(t, err)
	requireJSONEqual(t, fromPostgres, fromSQLite)

	// ... and both equal what the log alone reduces to.
	want, err := Replay(fixSessionID, recs)
	require.NoError(t, err)
	requireJSONEqual(t, want, fromSQLite)
	requireJSONEqual(t, want, fromPostgres)
}
