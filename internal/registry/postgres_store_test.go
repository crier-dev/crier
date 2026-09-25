//go:build integration

package registry

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/a2a"
	"github.com/crier-dev/crier/internal/guard"
	"github.com/crier-dev/crier/internal/webhook"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

// Shared between TestMain and every per-test helper. Populated once in TestMain
// and never mutated afterwards.
var (
	testConnString string
	pgContainer    *tcpostgres.PostgresContainer
)

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
		if ctr != nil {
			_ = ctr.Terminate(context.Background())
		}
		os.Exit(1)
	}
	pgContainer = ctr

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
	testConnString = fmt.Sprintf(
		"postgres://%s:%s@%s:%s/%s?sslmode=disable",
		pgUser, pgPass, host, port.Port(), pgDB,
	)

	code := m.Run()

	termCtx, termCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer termCancel()
	if err := ctr.Terminate(termCtx); err != nil {
		fmt.Fprintf(os.Stderr, "postgres terminate: %v\n", err)
	}

	os.Exit(code)
}

// --- Per-test store factory ---------------------------------------------------

// newTestStore drops every table created by the embedded migrations (plus the
// golang-migrate bookkeeping table) so each test starts from a clean schema,
// then calls NewPostgresStore — which itself runs RunMigrations on top.
func newTestStore(t *testing.T) *PostgresStore {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	cfg, err := pgxpool.ParseConfig(testConnString)
	require.NoError(t, err)
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	defer pool.Close()

	_, err = pool.Exec(ctx, `
DROP TABLE IF EXISTS dead_letters, inbox_entries, agents, schema_migrations CASCADE;`)
	require.NoError(t, err)

	store, err := NewPostgresStore(ctx, testConnString)
	require.NoError(t, err)
	t.Cleanup(store.Close)
	return store
}

// newTestAgent returns a fresh agent with a unique ID, valid 32-byte ed25519 key,
// and StatusOnline. It registers the agent in the store and returns it.
func newTestAgent(t *testing.T, store *PostgresStore, idPrefix string) *Agent {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	agent := &Agent{
		ID:           fmt.Sprintf("%s-%d", idPrefix, time.Now().UnixNano()),
		PublicKey:    HexKey(pub),
		Capabilities: []string{"relay", "mesh"},
		Status:       StatusOnline,
	}
	require.NoError(t, store.Register(agent))
	return agent
}

// --- NewPostgresStore / RunMigrations -----------------------------------------

func TestPostgresStore_NewPostgresStore_EmptyConnString(t *testing.T) {
	_, err := NewPostgresStore(context.Background(), "")
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrInvalidStoreInput),
		"expected ErrInvalidStoreInput, got %v", err)
}

func TestPostgresStore_NewPostgresStoreWithPoolConfig_EmptyConnString(t *testing.T) {
	_, err := NewPostgresStoreWithPoolConfig(context.Background(), "", DefaultPoolConfig())
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrInvalidStoreInput))
}

func TestPostgresStore_NewPostgresStoreWithPoolConfig_CustomConfig(t *testing.T) {
	// Verify PoolConfig values are accepted and applied — exercise the
	// pgxpool.Config setter branches that the default constructor skips.
	cfg := PoolConfig{
		MaxConns:        8,
		MinConns:        2,
		MaxConnLifetime: 15 * time.Minute,
		MaxConnIdleTime: 3 * time.Minute,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := NewPostgresStoreWithPoolConfig(ctx, testConnString, cfg)
	require.NoError(t, err)
	defer store.Close()
	require.NotNil(t, store)
}

func TestPostgresStore_NewPostgresStore_RunMigrations(t *testing.T) {
	// newTestStore already exercises NewPostgresStore → RunMigrations end to end.
	// This test makes the explicit coverage line and verifies the schema landed.
	store := newTestStore(t)
	require.NotNil(t, store)

	// Smoke: prove tables exist by inserting + selecting through the store.
	agent := newTestAgent(t, store, "mig")
	got, err := store.Get(agent.ID)
	require.NoError(t, err)
	require.Equal(t, agent.ID, got.ID)
}

func TestPostgresStore_RunMigrations_EmptyConnString(t *testing.T) {
	// Direct exercise of the standalone RunMigrations guard.
	err := RunMigrations(context.Background(), "")
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrInvalidStoreInput))
}

func TestPostgresStore_RunMigrations_BadConnString(t *testing.T) {
	// Cover the open/ping failure path of RunMigrations — unparseable URL.
	err := RunMigrations(context.Background(), "postgres://nope:nope@127.0.0.1:1/nope?sslmode=disable&connect_timeout=1")
	require.Error(t, err)
}

// --- Register -----------------------------------------------------------------

func TestPostgresStore_Register_Success(t *testing.T) {
	store := newTestStore(t)

	pub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	agent := &Agent{
		ID:           "agent-success",
		PublicKey:    HexKey(pub),
		Capabilities: []string{"relay"},
	}
	before := time.Now().UTC()
	require.NoError(t, store.Register(agent))

	require.Equal(t, StatusOnline, agent.Status, "blank status should default to online")
	require.False(t, agent.RegisteredAt.Before(before.Add(-time.Second)))
	require.Equal(t, []string{"relay"}, agent.Capabilities)

	got, err := store.Get("agent-success")
	require.NoError(t, err)
	require.Equal(t, agent.ID, got.ID)
	require.Equal(t, StatusOnline, got.Status)
	require.Equal(t, []string{"relay"}, got.Capabilities)
	require.Equal(t, []byte(pub), []byte(got.PublicKey))
}

func TestPostgresStore_Register_OfflineStatus(t *testing.T) {
	// Cover the non-default status branch (Register sets whatever valid
	// status the caller supplied; StatusOnline path is exercised by other tests).
	store := newTestStore(t)
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	agent := &Agent{
		ID:        "offline-agent",
		PublicKey: HexKey(pub),
		Status:    StatusOffline,
	}
	require.NoError(t, store.Register(agent))
	require.Equal(t, StatusOffline, agent.Status)

	got, err := store.Get("offline-agent")
	require.NoError(t, err)
	require.Equal(t, StatusOffline, got.Status)
}

func TestPostgresStore_Register_NilCapabilities(t *testing.T) {
	// Cover the json.Marshal branch on a nil Capabilities slice.
	store := newTestStore(t)
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	agent := &Agent{
		ID:        "no-caps",
		PublicKey: HexKey(pub),
		// Capabilities left nil.
	}
	require.NoError(t, store.Register(agent))
	require.NotNil(t, agent.Capabilities)
	require.Empty(t, agent.Capabilities)

	got, err := store.Get("no-caps")
	require.NoError(t, err)
	require.NotNil(t, got.Capabilities)
	require.Empty(t, got.Capabilities)
}

func TestPostgresStore_Register_Duplicate(t *testing.T) {
	store := newTestStore(t)
	existing := newTestAgent(t, store, "dup")

	// Second register with the same ID but a different key should fail with ErrAgentExists.
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	dup := &Agent{
		ID:        existing.ID,
		PublicKey: HexKey(pub),
	}
	err = store.Register(dup)
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrAgentExists), "expected ErrAgentExists, got %v", err)
}

func TestPostgresStore_Register_BlankID(t *testing.T) {
	store := newTestStore(t)
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	err = store.Register(&Agent{ID: "", PublicKey: HexKey(pub)})
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrInvalidStoreInput))
}

func TestPostgresStore_Register_NilAgent(t *testing.T) {
	store := newTestStore(t)
	err := store.Register(nil)
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrInvalidStoreInput))
}

func TestPostgresStore_Register_InvalidPublicKey(t *testing.T) {
	store := newTestStore(t)
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	short := HexKey(pub[:31]) // 31 bytes — wrong size (ed25519.PublicKeySize=32)
	err = store.Register(&Agent{ID: "bad-key", PublicKey: short})
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrInvalidStoreInput))
}

func TestPostgresStore_Register_InvalidStatus(t *testing.T) {
	store := newTestStore(t)
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	err = store.Register(&Agent{
		ID:        "bad-status",
		PublicKey: HexKey(pub),
		Status:    AgentStatus("banana"),
	})
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrInvalidStoreInput))
}

// --- Get ----------------------------------------------------------------------

func TestPostgresStore_Get_Success(t *testing.T) {
	store := newTestStore(t)
	agent := newTestAgent(t, store, "get")

	got, err := store.Get(agent.ID)
	require.NoError(t, err)
	require.Equal(t, agent.ID, got.ID)
	require.Equal(t, agent.Status, got.Status)
}

func TestPostgresStore_Get_NotFound(t *testing.T) {
	store := newTestStore(t)
	_, err := store.Get("does-not-exist")
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrAgentNotFound))
}

// --- List ---------------------------------------------------------------------

func TestPostgresStore_List_Empty(t *testing.T) {
	store := newTestStore(t)
	got := store.List()
	require.NotNil(t, got, "List must return non-nil on empty result")
	require.Empty(t, got)
}

func TestPostgresStore_List_Populated(t *testing.T) {
	store := newTestStore(t)
	a := newTestAgent(t, store, "list-a")
	b := newTestAgent(t, store, "list-b")
	c := newTestAgent(t, store, "list-c")

	agents := store.List()
	require.Len(t, agents, 3)
	seen := map[string]bool{}
	for _, ag := range agents {
		seen[ag.ID] = true
	}
	require.True(t, seen[a.ID])
	require.True(t, seen[b.ID])
	require.True(t, seen[c.ID])
}

// --- Unregister ---------------------------------------------------------------

func TestPostgresStore_Unregister_Success(t *testing.T) {
	store := newTestStore(t)
	agent := newTestAgent(t, store, "unreg")
	require.NoError(t, store.Deliver(agent.ID, &InboxEntry{Payload: json.RawMessage(`{"k":"v"}`)}))

	require.NoError(t, store.Unregister(agent.ID))

	_, err := store.Get(agent.ID)
	require.True(t, errors.Is(err, ErrAgentNotFound))
}

func TestPostgresStore_Unregister_NotFound(t *testing.T) {
	store := newTestStore(t)
	err := store.Unregister("ghost-agent")
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrAgentNotFound))
}

func TestPostgresStore_Unregister_CascadesInbox(t *testing.T) {
	store := newTestStore(t)
	agent := newTestAgent(t, store, "cascade")
	require.NoError(t, store.Deliver(agent.ID, &InboxEntry{Payload: json.RawMessage(`{}`)}))
	require.NoError(t, store.Unregister(agent.ID))

	// After cascade, Retrieve should report agent-not-found (FK target gone).
	_, _, err := store.Retrieve(agent.ID, time.Second, 5)
	require.True(t, errors.Is(err, ErrAgentNotFound))
}

// --- Deliver ------------------------------------------------------------------

func TestPostgresStore_Deliver_Success(t *testing.T) {
	store := newTestStore(t)
	agent := newTestAgent(t, store, "deliver")
	entry := &InboxEntry{Payload: json.RawMessage(`{"hello":"world"}`)}
	require.NoError(t, store.Deliver(agent.ID, entry))
	require.NotEmpty(t, entry.ID, "entry.ID should be auto-generated when blank")
	require.False(t, entry.CreatedAt.IsZero())
	require.False(t, entry.ExpiresAt.IsZero())
}

func TestPostgresStore_Deliver_BlankAgentID(t *testing.T) {
	// Cover the ErrInvalidStoreInput blank-agent-id guard on Deliver.
	store := newTestStore(t)
	err := store.Deliver("", &InboxEntry{Payload: json.RawMessage(`{}`)})
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrInvalidStoreInput))
}

func TestPostgresStore_Deliver_NilEntry(t *testing.T) {
	// Cover the ErrInvalidStoreInput nil-entry guard on Deliver.
	store := newTestStore(t)
	err := store.Deliver("any-agent", nil)
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrInvalidStoreInput))
}

func TestPostgresStore_Deliver_UnknownAgent(t *testing.T) {
	store := newTestStore(t)
	err := store.Deliver("nope", &InboxEntry{Payload: json.RawMessage(`{}`)})
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrAgentNotFound))
}

func TestPostgresStore_Deliver_InvalidJSON(t *testing.T) {
	store := newTestStore(t)
	agent := newTestAgent(t, store, "badjson")
	err := store.Deliver(agent.ID, &InboxEntry{Payload: []byte("not-json")})
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrInvalidStoreInput))
}

func TestPostgresStore_Deliver_DuplicateID(t *testing.T) {
	store := newTestStore(t)
	agent := newTestAgent(t, store, "dupmsg")
	entry := &InboxEntry{ID: "fixed-id", Payload: json.RawMessage(`{}`)}
	require.NoError(t, store.Deliver(agent.ID, entry))

	dup := &InboxEntry{ID: "fixed-id", Payload: json.RawMessage(`{}`)}
	err := store.Deliver(agent.ID, dup)
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrInvalidStoreInput))
}

// --- Retrieve -----------------------------------------------------------------

func TestPostgresStore_Retrieve_BlankAgentID(t *testing.T) {
	// Cover the ErrInvalidStoreInput blank-agent-id guard on Retrieve.
	store := newTestStore(t)
	_, _, err := store.Retrieve("", time.Second, 1)
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrInvalidStoreInput))
}

func TestPostgresStore_Retrieve_NonPositiveLease(t *testing.T) {
	// Cover the ErrInvalidStoreInput zero/negative lease duration guard.
	store := newTestStore(t)
	agent := newTestAgent(t, store, "zeroduration")
	_, _, err := store.Retrieve(agent.ID, 0, 1)
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrInvalidStoreInput))
}

func TestPostgresStore_Retrieve_NonPositiveMaxMessages(t *testing.T) {
	// Cover the ErrInvalidStoreInput zero/negative maxMessages guard.
	store := newTestStore(t)
	agent := newTestAgent(t, store, "zeromax")
	_, _, err := store.Retrieve(agent.ID, time.Second, 0)
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrInvalidStoreInput))
}

func TestPostgresStore_Retrieve_UnknownAgent(t *testing.T) {
	// Cover the agent existence check path inside the transaction.
	store := newTestStore(t)
	_, _, err := store.Retrieve("missing-agent", time.Second, 1)
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrAgentNotFound))
}

func TestPostgresStore_Retrieve_EmptyInbox(t *testing.T) {
	store := newTestStore(t)
	agent := newTestAgent(t, store, "empty")

	msgs, leaseID, err := store.Retrieve(agent.ID, 5*time.Second, 5)
	require.NoError(t, err)
	require.NotNil(t, msgs, "an empty retrieval must return a non-nil empty slice")
	require.Empty(t, msgs)
	// Nothing was claimed, so no lease is minted (DF-CRIER-32).
	require.Empty(t, leaseID)
}

func TestPostgresStore_Retrieve_AllLeasedMintsNoLease(t *testing.T) {
	store := newTestStore(t)
	agent := newTestAgent(t, store, "allleased")

	require.NoError(t, store.Deliver(agent.ID, &InboxEntry{Payload: json.RawMessage(`{}`)}))
	first, firstLease, err := store.Retrieve(agent.ID, 30*time.Second, 5)
	require.NoError(t, err)
	require.Len(t, first, 1)
	require.NotEmpty(t, firstLease)

	second, secondLease, err := store.Retrieve(agent.ID, 30*time.Second, 5)
	require.NoError(t, err)
	require.NotNil(t, second)
	require.Empty(t, second)
	require.Empty(t, secondLease, "a fully leased inbox must not mint a lease")

	// The first lease still owns its message.
	require.NoError(t, store.Ack(agent.ID, firstLease, []string{first[0].ID}))
}

func TestPostgresStore_Retrieve_FIFOOrder(t *testing.T) {
	store := newTestStore(t)
	agent := newTestAgent(t, store, "fifo")

	want := []string{}
	for i := 0; i < 5; i++ {
		e := &InboxEntry{Payload: json.RawMessage(fmt.Sprintf(`{"n":%d}`, i))}
		require.NoError(t, store.Deliver(agent.ID, e))
		want = append(want, e.ID)
	}

	msgs, _, err := store.Retrieve(agent.ID, 30*time.Second, 5)
	require.NoError(t, err)
	require.Len(t, msgs, 5)
	for i, m := range msgs {
		require.Equal(t, want[i], m.ID, "position %d should match FIFO order", i)
	}
}

func TestPostgresStore_Retrieve_MaxMessagesCapsBatch(t *testing.T) {
	// Verify the LIMIT clause on Retrieve: 7 entries, max=3 → exactly 3 returned.
	store := newTestStore(t)
	agent := newTestAgent(t, store, "capbatch")

	for i := 0; i < 7; i++ {
		require.NoError(t, store.Deliver(agent.ID, &InboxEntry{Payload: json.RawMessage(`{}`)}))
	}

	msgs, _, err := store.Retrieve(agent.ID, 30*time.Second, 3)
	require.NoError(t, err)
	require.Len(t, msgs, 3, "maxMessages should cap the batch size")
}

func TestPostgresStore_Retrieve_DisjointBatches(t *testing.T) {
	store := newTestStore(t)
	agent := newTestAgent(t, store, "disjoint")

	const total = 10
	for i := 0; i < total; i++ {
		require.NoError(t, store.Deliver(agent.ID, &InboxEntry{Payload: json.RawMessage(`{}`)}))
	}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		gotIDs  = map[string]int{}
		retErrs []error
	)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			msgs, _, err := store.Retrieve(agent.ID, 30*time.Second, 10)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				retErrs = append(retErrs, err)
				return
			}
			for _, m := range msgs {
				gotIDs[m.ID]++
			}
		}()
	}
	wg.Wait()
	require.Empty(t, retErrs)
	require.Len(t, gotIDs, total, "both retrievers combined should see every message exactly once")
	for id, n := range gotIDs {
		require.Equal(t, 1, n, "message %s appeared %d times, want 1", id, n)
	}
}

func TestPostgresStore_Retrieve_ExpiredLeaseReturnsToQueue(t *testing.T) {
	store := newTestStore(t)
	agent := newTestAgent(t, store, "lease")

	for i := 0; i < 3; i++ {
		require.NoError(t, store.Deliver(agent.ID, &InboxEntry{Payload: json.RawMessage(`{}`)}))
	}

	// Lease with a very short duration so we can wait it out cheaply.
	msgs1, _, err := store.Retrieve(agent.ID, 100*time.Millisecond, 5)
	require.NoError(t, err)
	require.Len(t, msgs1, 3)

	// Right after lease: queue still has them, leased=3.
	depth, leased, _, err := store.Stats(agent.ID)
	require.NoError(t, err)
	require.Equal(t, 3, depth)
	require.Equal(t, 3, leased)

	// After the lease expires and PurgeExpired runs, they should be retrievable again.
	time.Sleep(200 * time.Millisecond)
	store.PurgeExpired()

	msgs2, _, err := store.Retrieve(agent.ID, 30*time.Second, 5)
	require.NoError(t, err)
	require.Len(t, msgs2, 3, "expired lease should release messages back to the queue")
}

// --- Ack ----------------------------------------------------------------------

func TestPostgresStore_Ack_Success(t *testing.T) {
	store := newTestStore(t)
	agent := newTestAgent(t, store, "ack")

	ids := []string{}
	for i := 0; i < 3; i++ {
		e := &InboxEntry{Payload: json.RawMessage(`{}`)}
		require.NoError(t, store.Deliver(agent.ID, e))
		ids = append(ids, e.ID)
	}

	msgs, leaseID, err := store.Retrieve(agent.ID, 30*time.Second, 5)
	require.NoError(t, err)
	require.Len(t, msgs, 3)

	require.NoError(t, store.Ack(agent.ID, leaseID, ids))

	depth, leased, _, err := store.Stats(agent.ID)
	require.NoError(t, err)
	require.Equal(t, 0, depth, "Acked messages should be removed from queue depth")
	require.Equal(t, 0, leased)
}

func TestPostgresStore_Ack_WrongLease(t *testing.T) {
	store := newTestStore(t)
	agent := newTestAgent(t, store, "wronglease")

	require.NoError(t, store.Deliver(agent.ID, &InboxEntry{Payload: json.RawMessage(`{}`)}))
	msgs, _, err := store.Retrieve(agent.ID, 30*time.Second, 1)
	require.NoError(t, err)
	require.Len(t, msgs, 1)

	err = store.Ack(agent.ID, "not-the-real-lease", []string{msgs[0].ID})
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrLeaseConflict))
}

func TestPostgresStore_Ack_DuplicateIDs(t *testing.T) {
	store := newTestStore(t)
	agent := newTestAgent(t, store, "dupack")
	require.NoError(t, store.Deliver(agent.ID, &InboxEntry{Payload: json.RawMessage(`{}`)}))
	_, leaseID, err := store.Retrieve(agent.ID, 30*time.Second, 1)
	require.NoError(t, err)

	err = store.Ack(agent.ID, leaseID, []string{"x", "x"})
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrInvalidStoreInput))
}

func TestPostgresStore_Ack_EmptyMessageIDs(t *testing.T) {
	store := newTestStore(t)
	agent := newTestAgent(t, store, "noop")
	require.NoError(t, store.Deliver(agent.ID, &InboxEntry{Payload: json.RawMessage(`{}`)}))
	_, _, err := store.Retrieve(agent.ID, 30*time.Second, 1)
	require.NoError(t, err)

	// Empty messageIDs is a silent no-op (CR-GAP-014) — the store must reject it.
	err = store.Ack(agent.ID, "any-lease", nil)
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrInvalidStoreInput))

	depth, _, _, err := store.Stats(agent.ID)
	require.NoError(t, err)
	require.Equal(t, 1, depth, "rejected Ack should not delete anything")
}

func TestPostgresStore_Ack_BlankAgentID(t *testing.T) {
	// Cover the ErrInvalidStoreInput blank-agent-id guard on Ack.
	store := newTestStore(t)
	err := store.Ack("", "lease", []string{"id"})
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrInvalidStoreInput))
}

func TestPostgresStore_Ack_BlankLeaseID(t *testing.T) {
	// Cover the ErrInvalidStoreInput blank-lease-id guard on Ack.
	store := newTestStore(t)
	err := store.Ack("any-agent", "", []string{"id"})
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrInvalidStoreInput))
}

func TestPostgresStore_Ack_UnknownAgent(t *testing.T) {
	// Cover the agent existence check path inside the Ack transaction.
	store := newTestStore(t)
	err := store.Ack("missing-agent", "lease", []string{"id"})
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrAgentNotFound))
}

func TestPostgresStore_Ack_MissingID(t *testing.T) {
	// Ack with an ID the inbox does not hold must report ErrMessageNotFound
	// (distinct from a lease conflict) and delete nothing.
	store := newTestStore(t)
	agent := newTestAgent(t, store, "missingid")
	require.NoError(t, store.Deliver(agent.ID, &InboxEntry{Payload: json.RawMessage(`{}`)}))
	_, leaseID, err := store.Retrieve(agent.ID, 30*time.Second, 1)
	require.NoError(t, err)

	err = store.Ack(agent.ID, leaseID, []string{"definitely-not-a-real-id"})
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrMessageNotFound), "want ErrMessageNotFound, got %v", err)
	require.False(t, errors.Is(err, ErrLeaseConflict))

	depth, _, _, err := store.Stats(agent.ID)
	require.NoError(t, err)
	require.Equal(t, 1, depth, "a rejected Ack must not delete anything")
}

// --- Stats --------------------------------------------------------------------

func TestPostgresStore_Stats_BlankAgentID(t *testing.T) {
	// Cover the ErrInvalidStoreInput blank-agent-id guard on Stats.
	store := newTestStore(t)
	_, _, _, err := store.Stats("")
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrInvalidStoreInput))
}

func TestPostgresStore_Stats_EmptyQueue(t *testing.T) {
	store := newTestStore(t)
	agent := newTestAgent(t, store, "stats-empty")

	depth, leased, age, err := store.Stats(agent.ID)
	require.NoError(t, err)
	require.Equal(t, 0, depth)
	require.Equal(t, 0, leased)
	require.Equal(t, time.Duration(0), age)
}

func TestPostgresStore_Stats_QueuedAndLeased(t *testing.T) {
	store := newTestStore(t)
	agent := newTestAgent(t, store, "stats-mix")

	for i := 0; i < 4; i++ {
		require.NoError(t, store.Deliver(agent.ID, &InboxEntry{Payload: json.RawMessage(`{}`)}))
	}
	_, _, err := store.Retrieve(agent.ID, 30*time.Second, 2)
	require.NoError(t, err)

	// Allow a tick so MIN(created_at) gives a non-zero age.
	time.Sleep(10 * time.Millisecond)

	depth, leased, age, err := store.Stats(agent.ID)
	require.NoError(t, err)
	require.Equal(t, 4, depth, "all 4 messages still in queue (leased or not)")
	require.Equal(t, 2, leased)
	require.Greater(t, age, time.Duration(0), "oldest message age should be > 0")
}

func TestPostgresStore_Stats_AgentNotFound(t *testing.T) {
	store := newTestStore(t)
	_, _, _, err := store.Stats("missing-agent")
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrAgentNotFound))
}

// --- PurgeExpired -------------------------------------------------------------

func TestPostgresStore_PurgeExpired_TTLRemoval(t *testing.T) {
	store := newTestStore(t)
	agent := newTestAgent(t, store, "ttl")

	expired := &InboxEntry{
		Payload:   json.RawMessage(`{"dead":true}`),
		ExpiresAt: time.Now().UTC().Add(50 * time.Millisecond),
	}
	require.NoError(t, store.Deliver(agent.ID, expired))

	fresh := &InboxEntry{Payload: json.RawMessage(`{"alive":true}`)}
	require.NoError(t, store.Deliver(agent.ID, fresh))

	time.Sleep(100 * time.Millisecond)

	removed := store.PurgeExpired()
	require.Equal(t, 1, removed, "exactly one TTL-expired message should be deleted")

	depth, _, _, err := store.Stats(agent.ID)
	require.NoError(t, err)
	require.Equal(t, 1, depth, "the fresh message should remain")
}

func TestPostgresStore_PurgeExpired_StaleLeaseRelease(t *testing.T) {
	store := newTestStore(t)
	agent := newTestAgent(t, store, "stalelease")

	require.NoError(t, store.Deliver(agent.ID, &InboxEntry{Payload: json.RawMessage(`{}`)}))

	msgs, _, err := store.Retrieve(agent.ID, 100*time.Millisecond, 1)
	require.NoError(t, err)
	require.Len(t, msgs, 1)

	// Wait past the lease but not past the TTL (TTL default is 24h).
	time.Sleep(200 * time.Millisecond)

	// PurgeExpired should release the lease (zero TTL-expired deletes).
	removed := store.PurgeExpired()
	require.Equal(t, 0, removed, "no TTL-expired messages, only stale lease to release")

	// After release, the message should be retrievable again.
	msgs2, _, err := store.Retrieve(agent.ID, 30*time.Second, 1)
	require.NoError(t, err)
	require.Len(t, msgs2, 1, "released-lease message should be retrievable again")
}

// --- Close --------------------------------------------------------------------

func TestPostgresStore_Close_NilPoolSafe(t *testing.T) {
	// Defensive coverage for the Close() pool-nil guard; a PostgresStore with
	// a nil pool must not panic when Close is invoked.
	s := &PostgresStore{pool: nil}
	s.Close()
}

// --- Optional agent configs (DF-CRIER-151) -------------------------------------
//
// Before the fix the durable backend accepted a webhook/guard config with HTTP
// 200 and dropped it: the columns did not exist and neither Register nor Update
// wrote them. These tests prove persistence against real postgres — the only
// thing unit tests with a mock pool cannot prove.

func TestPostgresStore_Register_PersistsWebhookAndGuard(t *testing.T) {
	store := newTestStore(t)
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	agent := &Agent{
		ID:        "config-agent",
		PublicKey: HexKey(pub),
		Webhook:   &webhook.Config{URL: "http://127.0.0.1:9911/hook", DeliveryMode: "blocking"},
		Guard:     &guard.AgentGuardConfig{Policies: []guard.Policy{{ID: "default"}}},
	}
	require.NoError(t, store.Register(agent))

	got, err := store.Get("config-agent")
	require.NoError(t, err)
	require.NotNil(t, got.Webhook, "webhook config must be persisted by Register")
	require.Equal(t, *agent.Webhook, *got.Webhook)
	require.NotNil(t, got.Guard, "guard config must be persisted by Register")
	require.Equal(t, *agent.Guard, *got.Guard)

	// A completely fresh store (new pool, new process state) reads them back:
	// the configs live in the database, not in the registering process.
	fresh, err := NewPostgresStore(context.Background(), testConnString)
	require.NoError(t, err)
	defer fresh.Close()
	again, err := fresh.Get("config-agent")
	require.NoError(t, err)
	require.NotNil(t, again.Webhook)
	require.Equal(t, agent.Webhook.URL, again.Webhook.URL)
	require.Equal(t, "blocking", again.Webhook.DeliveryMode)
	require.NotNil(t, again.Guard)
}

func TestPostgresStore_Register_NilConfigsReadBackNil(t *testing.T) {
	store := newTestStore(t)
	agent := newTestAgent(t, store, "noconfig")

	got, err := store.Get(agent.ID)
	require.NoError(t, err)
	require.Nil(t, got.Webhook, "an agent registered without a webhook must read back nil")
	require.Nil(t, got.Guard, "an agent registered without a guard must read back nil")
	require.Nil(t, got.A2A, "an agent registered without an a2a block must read back nil")
}

// --- A2A opt-in block (INT-A2A-001) ------------------------------------------
//
// The block is the per-agent half of the A2A gate and a later row reads it off
// the row, so it must survive the durable backend the same way the webhook and
// guard configs do: registering on one process and reading from a fresh one.

func TestPostgresStore_Register_PersistsA2A(t *testing.T) {
	store := newTestStore(t)
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)

	agent := &Agent{
		ID:        "a2a-agent",
		PublicKey: HexKey(pub),
		A2A:       &a2a.Config{Enabled: true},
	}
	require.NoError(t, store.Register(agent))

	got, err := store.Get("a2a-agent")
	require.NoError(t, err)
	require.NotNil(t, got.A2A, "the a2a block must be persisted by Register")
	require.Equal(t, *agent.A2A, *got.A2A)

	// A completely fresh store (new pool, new process state) reads it back: the
	// opt-in lives in the database, not in the registering process.
	fresh, err := NewPostgresStore(context.Background(), testConnString)
	require.NoError(t, err)
	defer fresh.Close()
	again, err := fresh.Get("a2a-agent")
	require.NoError(t, err)
	require.NotNil(t, again.A2A)
	require.True(t, again.A2A.Enabled)

	// And the column really is SQL NULL for an agent that did not opt in —
	// "absent", not an empty object that would read as opted out.
	plain := newTestAgent(t, store, "a2a-none")
	var raw *string
	row := store.pool.QueryRow(context.Background(), `SELECT a2a::text FROM agents WHERE id = $1`, plain.ID)
	require.NoError(t, row.Scan(&raw))
	require.Nil(t, raw, "an agent registered without the block must store SQL NULL in agents.a2a")
}

func TestPostgresStore_Update_ClearsA2A(t *testing.T) {
	store := newTestStore(t)
	agent := newTestAgent(t, store, "a2a-clear")

	agent.A2A = &a2a.Config{Enabled: true}
	require.NoError(t, store.Update(agent))
	got, err := store.Get(agent.ID)
	require.NoError(t, err)
	require.NotNil(t, got.A2A)

	// Opting back out writes SQL NULL (the PATCH handler clears the field
	// before calling Update), so the agent reads back with no block at all.
	got.A2A = nil
	require.NoError(t, store.Update(got))

	cleared, err := store.Get(agent.ID)
	require.NoError(t, err)
	require.Nil(t, cleared.A2A, "an explicit nil a2a block must clear the stored opt-in")

	// ...and the OTHER optional configs are untouched by that write path.
	require.Nil(t, cleared.Webhook)
	require.Nil(t, cleared.Guard)
}

func TestPostgresStore_Update_PersistsChangedWebhook(t *testing.T) {
	store := newTestStore(t)
	agent := newTestAgent(t, store, "updatecfg")
	require.Nil(t, agent.Webhook)

	agent.Webhook = &webhook.Config{URL: "http://127.0.0.1:9911/hook", DeliveryMode: "blocking"}
	agent.Guard = &guard.AgentGuardConfig{Policies: []guard.Policy{{ID: "default"}}}
	require.NoError(t, store.Update(agent))

	got, err := store.Get(agent.ID)
	require.NoError(t, err)
	require.NotNil(t, got.Webhook, "PATCH must persist a new webhook on the postgres backend")
	require.Equal(t, "http://127.0.0.1:9911/hook", got.Webhook.URL)
	require.NotNil(t, got.Guard)

	// Change the webhook and persist again — the second write must land too.
	got.Webhook.DeliveryMode = "async"
	require.NoError(t, store.Update(got))

	again, err := store.Get(agent.ID)
	require.NoError(t, err)
	require.NotNil(t, again.Webhook)
	require.Equal(t, "async", again.Webhook.DeliveryMode, "changed webhook must be persisted")
}

func TestPostgresStore_Update_NilWebhookClears(t *testing.T) {
	store := newTestStore(t)
	agent := newTestAgent(t, store, "clearinput")

	agent.Webhook = &webhook.Config{URL: "http://127.0.0.1:9911/hook"}
	agent.Guard = &guard.AgentGuardConfig{Policies: []guard.Policy{{ID: "default"}}}
	require.NoError(t, store.Update(agent))

	got, err := store.Get(agent.ID)
	require.NoError(t, err)
	require.NotNil(t, got.Webhook)

	// Spec §7: webhook absent or null removes the webhook. The handler clears
	// the field before calling Update, so nil here must write SQL NULL.
	got.Webhook = nil
	got.Guard = nil
	require.NoError(t, store.Update(got))

	cleared, err := store.Get(agent.ID)
	require.NoError(t, err)
	require.Nil(t, cleared.Webhook, "an explicit nil webhook must clear the stored config")
	require.Nil(t, cleared.Guard)

	// ...and the columns really are SQL NULL, not a JSON null or empty object.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	db := openTestDB(ctx, t)
	defer db.Close()
	var (
		webhookNull bool
		guardNull   bool
	)
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT webhook IS NULL, guard IS NULL FROM agents WHERE id = $1`, agent.ID).
		Scan(&webhookNull, &guardNull))
	require.True(t, webhookNull, "cleared webhook column must be SQL NULL")
	require.True(t, guardNull, "cleared guard column must be SQL NULL")
}

// TestPostgresStore_Update_AdvancesLastSeenBothSides pins DF-CRIER-156
// against a real postgres (the unit test proves the statement's argument; this
// proves the row and the caller's object agree end-to-end). Pre-fix the UPDATE
// advanced the row but left the caller's agent at its registration value, so
// the PATCH 200 body — rendered from that object — disagreed with a following
// GET.
//
// Baseline note: the comparison uses the READ-BACK registration values, not the
// ones Register stamped into the caller's struct. Postgres truncates timestamps
// to microseconds, so a value that round-tripped through the database is the
// authoritative one; Register's own object still carries nanoseconds (that
// render-side difference is pre-existing and out of this ticket's scope).
func TestPostgresStore_Update_AdvancesLastSeenBothSides(t *testing.T) {
	store := newTestStore(t)
	agent := newTestAgent(t, store, "updseen")
	callerRegisteredAt := agent.RegisteredAt

	before, err := store.Get(agent.ID)
	require.NoError(t, err)
	preUpdate := before.LastSeen
	require.False(t, preUpdate.IsZero(), "Register must stamp last_seen")

	// Make the update instant distinguishable from registration's.
	time.Sleep(2 * time.Millisecond)

	agent.Capabilities = []string{"solver"}
	require.NoError(t, store.Update(agent))

	require.True(t, agent.LastSeen.After(preUpdate),
		"Update must advance the caller's last_seen (pre-update %v, got %v)", preUpdate, agent.LastSeen)
	require.True(t, agent.RegisteredAt.Equal(callerRegisteredAt),
		"Update must leave the caller's registered_at alone (%v), got %v",
		callerRegisteredAt, agent.RegisteredAt)
	require.Equal(t, StatusOnline, agent.Status)

	got, err := store.Get(agent.ID)
	require.NoError(t, err)
	require.True(t, got.LastSeen.After(preUpdate),
		"the stored row must advance (pre-update %v, stored %v)", preUpdate, got.LastSeen)
	require.True(t, got.LastSeen.Equal(agent.LastSeen),
		"stored last_seen %v != caller last_seen %v — PATCH response would diverge from the row",
		got.LastSeen, agent.LastSeen)
	require.True(t, got.RegisteredAt.Equal(before.RegisteredAt),
		"the stored row must keep the registration time (%v), got %v",
		before.RegisteredAt, got.RegisteredAt)
}

// TestPostgresStore_Get_StoredJSONNullReadsNil: a column holding the JSON value
// `null` (rather than SQL NULL) is the same absent config on the wire — no
// error, no empty object.
func TestPostgresStore_Get_StoredJSONNullReadsNil(t *testing.T) {
	store := newTestStore(t)
	agent := newTestAgent(t, store, "jsonnull")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	db := openTestDB(ctx, t)
	defer db.Close()
	_, err := db.ExecContext(ctx,
		`UPDATE agents SET webhook = 'null'::jsonb, guard = 'null'::jsonb WHERE id = $1`, agent.ID)
	require.NoError(t, err)

	got, err := store.Get(agent.ID)
	require.NoError(t, err)
	require.Nil(t, got.Webhook)
	require.Nil(t, got.Guard)
}

func TestPostgresStore_List_IncludesConfigs(t *testing.T) {
	store := newTestStore(t)

	withConfig := newTestAgent(t, store, "listcfg")
	withConfig.Webhook = &webhook.Config{URL: "http://127.0.0.1:9912/a"}
	require.NoError(t, store.Update(withConfig))

	plain := newTestAgent(t, store, "listplain")

	byID := map[string]*Agent{}
	for _, a := range store.List() {
		byID[a.ID] = a
	}
	require.NotNil(t, byID[withConfig.ID], "configured agent must appear in List")
	require.NotNil(t, byID[withConfig.ID].Webhook, "List must carry the stored webhook")
	require.Equal(t, "http://127.0.0.1:9912/a", byID[withConfig.ID].Webhook.URL)
	require.Nil(t, byID[plain.ID].Webhook, "List must leave an absent webhook nil")
}
