package session

// PERF-004 acceptance battery for the widened SQLite read pool.
//
// The defect this pins: NewSQLiteStore used to pin SetMaxOpenConns(1) because the
// per-connection pragmas (foreign_keys, busy_timeout, synchronous) were applied
// ONCE at open — a second pooled connection would have come up with foreign-key
// enforcement off and no busy timeout, so the pool could not be widened without
// first arming every connection. The pragmas now ride the driver DSN
// (sqliteDSN), and these tests assert the consequences rather than the code:
//
//   - a file-backed store admits more than one reader, :memory: still does not
//     (each connection to an in-memory database is a DIFFERENT database);
//   - a fresh pooled connection ANSWERS pragma foreign_keys=1,
//     busy_timeout=5000, synchronous=1 — and the foreign key is actually
//     ENFORCED on it, not merely reported;
//   - the endpoint bodies are byte-identical between the narrow and wide arms
//     over the same seeded fixture, so the pool changed the queue and not the
//     answer;
//   - concurrent writes still succeed on the widened pool (they serialize inside
//     SQLite under WAL, bounded by busy_timeout).

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/stretchr/testify/require"
)

const (
	// poolFixtureAgent is the fixture's single local agent. It is NOT shared with
	// bench_test.go: a benchmark fixture change must not silently move this one.
	poolFixtureAgent = "atlas"
	// poolFixtureSessions sizes the equivalence fixture. Small on purpose — this
	// test proves SAME BYTES, not throughput.
	poolFixtureSessions = 3
	poolFixtureMessages = 3
)

// poolFixtureBase is a fixed clock so the seeded fixture is byte-reproducible.
var poolFixtureBase = time.Date(2026, 11, 19, 6, 0, 0, 0, time.UTC)

// ---------------------------------------------------------------------------
// Arm 1: the pool width
// ---------------------------------------------------------------------------

// TestSQLite_FileDatabaseWidensReadPool is acceptance criterion 1's first half:
// a FILE-backed database comes up with a bounded pool wider than one. The env
// seam is pinned to empty first so a deployment override in the test host's
// environment cannot make this assertion lie.
func TestSQLite_FileDatabaseWidensReadPool(t *testing.T) {
	t.Setenv(sqlitePoolEnv, "")
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "sessions.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	require.Greater(t, sqlitePoolConns, 1,
		"the widened pool must admit more than one reader, else this change fixed nothing")
	require.Equal(t, sqlitePoolConns, store.DB().Stats().MaxOpenConnections,
		"a file-backed store must open a bounded pool of %d connections", sqlitePoolConns)
}

// TestSQLite_MemoryDatabasePinnedToOneConnection is the other half: ":memory:"
// stays at ONE connection however wide the pool is configured, because a pooled
// in-memory database is a different (empty) database per connection.
func TestSQLite_MemoryDatabasePinnedToOneConnection(t *testing.T) {
	t.Setenv(sqlitePoolEnv, "8")
	store, err := NewSQLiteStore(sqliteInMemoryName)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	require.Equal(t, 1, store.DB().Stats().MaxOpenConnections,
		"an in-memory database must stay on one connection")
	// And it still works: a create through the pinned store is readable back.
	serveOneCreate(t, store)
}

// TestSQLite_PoolEnvOverride proves the seam AC 5 asks for: a named default with
// an override, where an unusable override is logged and IGNORED rather than
// applied as-is (a typo must not quietly re-create a one-connection store).
func TestSQLite_PoolEnvOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "override.sqlite")

	t.Setenv(sqlitePoolEnv, "3")
	store, err := NewSQLiteStore(path)
	require.NoError(t, err)
	require.Equal(t, 3, store.DB().Stats().MaxOpenConnections,
		"%s=3 must widen the pool to three connections", sqlitePoolEnv)
	require.NoError(t, store.Close())

	t.Setenv(sqlitePoolEnv, "not-a-number")
	store, err = NewSQLiteStore(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	require.Equal(t, sqlitePoolConns, store.DB().Stats().MaxOpenConnections,
		"an unparseable override must fall back to the default, not to one connection")
}

// ---------------------------------------------------------------------------
// Arm 2: the pragmas are armed on EVERY pooled connection
// ---------------------------------------------------------------------------

// TestSQLite_PoolPragmasArmedOnEveryConnection is the load-bearing one. It holds
// sqlitePoolConns connections open SIMULTANEOUSLY — so every one of them is a
// distinct physical connection database/sql had to open — and asks each what its
// per-connection settings actually are, then proves the foreign key is enforced
// on a connection that is not the first.
//
// Pre-fix this test cannot even acquire the second connection: the store pins
// one, so the acquire blocks until the context deadline.
func TestSQLite_PoolPragmasArmedOnEveryConnection(t *testing.T) {
	store, err := newSQLiteStorePooled(filepath.Join(t.TempDir(), "armed.sqlite"), sqlitePoolConns, sqliteOpenOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	held := make([]*sql.Conn, 0, sqlitePoolConns)
	for i := 0; i < sqlitePoolConns; i++ {
		conn, err := store.DB().Conn(ctx)
		require.NoError(t, err, "acquire pooled connection %d of %d", i+1, sqlitePoolConns)
		held = append(held, conn)
	}
	t.Cleanup(func() {
		for _, conn := range held {
			_ = conn.Close()
		}
	})
	require.Len(t, held, sqlitePoolConns)

	for i, conn := range held {
		var busyTimeout, foreignKeys, synchronous int
		require.NoError(t, conn.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&busyTimeout),
			"connection %d: read busy_timeout", i)
		require.NoError(t, conn.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&foreignKeys),
			"connection %d: read foreign_keys", i)
		require.NoError(t, conn.QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&synchronous),
			"connection %d: read synchronous", i)

		require.Equal(t, 5000, busyTimeout, "connection %d of %d is missing busy_timeout", i+1, sqlitePoolConns)
		require.Equal(t, 1, foreignKeys, "connection %d of %d is missing foreign_keys", i+1, sqlitePoolConns)
		require.Equal(t, 1, synchronous, "connection %d of %d is not NORMAL synchronous", i+1, sqlitePoolConns)
	}

	// The pragma must be in FORCE, not merely reported: a member row pointing at
	// a session that does not exist is refused by the LAST connection acquired.
	last := held[len(held)-1]
	_, err = last.ExecContext(ctx, `
		INSERT INTO chat_session_members
			(session_id, member_type, member_id, role, added_at)
		VALUES ('no-such-session', 'agent', 'atlas', 'owner', '2026-11-19T06:00:00Z')`)
	require.Error(t, err, "a no-such-session member must be refused by foreign_keys on a pooled connection")
	require.Contains(t, err.Error(), "FOREIGN KEY")
}

// TestSQLiteDSNCarriesPragmas pins the DSN shape both the store and the tests
// above depend on: one `_pragma` per setting, appended to a plain path with `?`
// and to an existing `file:` DSN with `&` (extended, never rewritten), and the
// statement spelling stays in lockstep with the DSN spelling.
func TestSQLiteDSNCarriesPragmas(t *testing.T) {
	values := func(dsn string) url.Values {
		t.Helper()
		i := strings.Index(dsn, "?")
		require.GreaterOrEqual(t, i, 0, "DSN %q carries no query", dsn)
		q, err := url.ParseQuery(dsn[i+1:])
		require.NoError(t, err)
		return q
	}

	plain := sqliteDSN("/tmp/sessions.sqlite")
	require.True(t, strings.HasPrefix(plain, "/tmp/sessions.sqlite?"),
		"a plain path takes the pragmas after '?': %q", plain)
	require.Equal(t,
		[]string{"busy_timeout(5000)", "foreign_keys(ON)", "synchronous(NORMAL)"},
		values(plain)["_pragma"])

	existing := sqliteDSN("file:/tmp/sessions.sqlite?cache=shared")
	require.True(t, strings.HasPrefix(existing, "file:/tmp/sessions.sqlite?cache=shared&_pragma="),
		"an existing file: DSN must be extended with '&': %q", existing)
	require.Equal(t, "shared", values(existing).Get("cache"), "the caller's own DSN parameter must survive")

	// Coherence (AC 1: "keep them coherent with the DSN pragmas, do not
	// double-arm in a way that conflicts"): the row-less statement form carries
	// the same values, which is what makes re-arming at open idempotent.
	require.Equal(t, []string{
		`PRAGMA busy_timeout = 5000`,
		`PRAGMA foreign_keys = ON`,
		`PRAGMA synchronous = NORMAL`,
	}, (sqliteDialect{}).InitStatements())
}

// ---------------------------------------------------------------------------
// Arm 3: same fixture, same bytes (AC 2)
// ---------------------------------------------------------------------------

// TestSQLite_PooledReadsAnswerSameBytes hashes the raw GET /sessions and
// GET /sessions/{id}/messages bodies for the single-connection and widened arms
// over the same seeded fixture and requires byte equality. It is the "the result
// is unchanged" half of the change: a pool that answers faster with a different
// body is a regression, not a fix.
func TestSQLite_PooledReadsAnswerSameBytes(t *testing.T) {
	narrow := seededPoolStore(t, "narrow", 1)
	wide := seededPoolStore(t, "wide", sqlitePoolConns)
	narrowSrv := servePoolStore(t, narrow)
	wideSrv := servePoolStore(t, wide)

	for _, path := range []string{
		"/sessions",
		"/sessions/pool-fix-000/messages",
	} {
		narrowBody := getPoolBody(t, narrowSrv.URL+path)
		wideBody := getPoolBody(t, wideSrv.URL+path)

		// A vacuous pass is the failure mode to rule out first: both arms
		// answering an empty/erroring body would compare equal.
		require.Contains(t, string(narrowBody), `"id":"pool-fix-000"`,
			"%s answered no fixture row on the narrow arm", path)
		require.Equal(t, len(narrowBody), len(wideBody), "%s body length differs", path)
		require.Equal(t, poolBodyHash(narrowBody), poolBodyHash(wideBody),
			"%s body differs between the pool1 and pool%d arms", path, sqlitePoolConns)
		require.True(t, bytes.Equal(narrowBody, wideBody), "%s body differs", path)
	}

	// The transcript arm must carry the message payload, not just the session.
	transcript := getPoolBody(t, wideSrv.URL+"/sessions/pool-fix-000/messages")
	require.Contains(t, string(transcript), "pool fixture payload",
		"the transcript fixture must actually render messages, else the equality is vacuous")
}

// ---------------------------------------------------------------------------
// Arm 4: the write path still works on a widened pool
// ---------------------------------------------------------------------------

// TestSQLite_PooledConcurrentWritesSucceed measures the worry AC 4 names instead
// of assuming it: widening a pool does not widen SQLite's single writer, so
// concurrent creates must wait on busy_timeout and still all commit. A store that
// answered SQLITE_BUSY here would be a serious regression traded for the read
// win.
func TestSQLite_PooledConcurrentWritesSucceed(t *testing.T) {
	store, err := newSQLiteStorePooled(filepath.Join(t.TempDir(), "writes.sqlite"), sqlitePoolConns, sqliteOpenOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	const writers, perWriter = 8, 5
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make([]error, writers)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				id := fmt.Sprintf("pool-write-%02d-%02d", w, i)
				sess := &Session{
					ID:        id,
					Kind:      KindChannel,
					Title:     "concurrent write",
					CreatedAt: poolFixtureBase.Add(time.Duration(w*perWriter+i) * time.Millisecond),
					CreatedBy: AuthorRef{Agent: poolFixtureAgent},
					State:     SessionOpen,
				}
				if err := store.CreateSession(ctx, sess, 1); err != nil {
					errs[w] = fmt.Errorf("writer %d create %s: %w", w, id, err)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	for w, err := range errs {
		require.NoError(t, err, "writer %d failed", w)
	}
	ids, err := store.Sessions(ctx)
	require.NoError(t, err)
	require.Len(t, ids, writers*perWriter, "every concurrent create must be durable")
	for _, id := range ids {
		st, err := store.Load(ctx, id)
		require.NoError(t, err, "load %s", id)
		require.Equal(t, id, st.Session.ID)
	}
}

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

// seededPoolStore opens a store with the requested pool width and seeds the
// fixture through the Repository the handler serves from.
func seededPoolStore(t *testing.T, name string, pool int) *SQLStore {
	t.Helper()
	store, err := newSQLiteStorePooled(filepath.Join(t.TempDir(), name+".sqlite"), pool, sqliteOpenOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	seedPoolFixture(t, store)
	return store
}

// seedPoolFixture writes poolFixtureSessions sessions, each with one agent member
// and poolFixtureMessages messages, at fixed timestamps so two independently
// seeded databases hold identical rows.
func seedPoolFixture(t *testing.T, store Repository) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < poolFixtureSessions; i++ {
		id := fmt.Sprintf("pool-fix-%03d", i)
		at := poolFixtureBase.Add(time.Duration(i) * time.Minute)
		require.NoError(t, store.CreateSession(ctx, &Session{
			ID:        id,
			Kind:      KindChannel,
			Title:     "pool fixture",
			CreatedAt: at,
			CreatedBy: AuthorRef{Agent: poolFixtureAgent},
			State:     SessionOpen,
		}, 1))
		require.NoError(t, store.AddMember(ctx, &Member{
			SessionID:  id,
			MemberType: MemberAgent,
			MemberID:   poolFixtureAgent,
			Role:       RoleOwner,
			AddedAt:    at,
			AddedBy:    poolFixtureAgent,
		}, 2, &ContextShare{Mode: ShareSummary}))
		for j := 0; j < poolFixtureMessages; j++ {
			mid := fmt.Sprintf("%s-m%02d", id, j)
			require.NoError(t, store.PostMessage(ctx, &Message{
				ID:        mid,
				ThreadID:  mid,
				SessionID: id,
				Kind:      MessagePlain,
				Author:    AuthorRef{Agent: poolFixtureAgent},
				Payload:   []byte(`{"text":"pool fixture payload"}`),
				Audience: Audience{
					Rule:    AudienceSession,
					Targets: []AudienceTarget{{Kind: TargetAgent, ID: poolFixtureAgent}},
				},
				Seq:       int64(3 + j),
				CreatedAt: at.Add(time.Duration(j) * time.Second),
			}))
		}
	}
}

// servePoolStore mounts the real route set over a store, so the bodies compared
// above are the shipped endpoints' bodies.
func servePoolStore(t *testing.T, store Repository) *httptest.Server {
	t.Helper()
	h := NewHTTPHandler(store, HTTPOptions{})
	r := mux.NewRouter()
	registerSessionRoutes(r, h)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

// getPoolBody fetches url and returns the raw response body, failing on a
// non-2xx status (an error body must never be mistaken for a compared body).
func getPoolBody(t *testing.T, url string) []byte {
	t.Helper()
	resp, err := http.Get(url)
	require.NoError(t, err, "GET %s", url)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err, "read %s", url)
	require.Equal(t, http.StatusOK, resp.StatusCode, "GET %s: body=%s", url, body)
	return body
}

// poolBodyHash renders the sha256 of a body for a readable failure message.
func poolBodyHash(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}
