package registry

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v5"
	"github.com/stretchr/testify/require"
)

// newMockStore creates a PostgresStore backed by a pgxmock pool.
// Tests must call mock.ExpectationsWereMet() at the end.
func newMockStore(t *testing.T) (*PostgresStore, pgxmock.PgxPoolIface) {
	t.Helper()
	mock, err := pgxmock.NewPool()
	require.NoError(t, err)
	s := &PostgresStore{pool: mock}
	t.Cleanup(s.Close)
	return s, mock
}

// testAgent returns a valid Agent for use in mock-backed unit tests.
func testAgent(t *testing.T) *Agent {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	return &Agent{
		ID:           "test-agent",
		PublicKey:    HexKey(pub),
		Capabilities: []string{"relay"},
		Status:       StatusOnline,
	}
}

// ---------------------------------------------------------------------------
// NewPostgresStore / NewPostgresStoreWithPoolConfig — input validation
// ---------------------------------------------------------------------------

func TestPostgresStoreUnit_NewPostgresStore_EmptyConnString(t *testing.T) {
	_, err := NewPostgresStore(context.Background(), "")
	require.True(t, errors.Is(err, ErrInvalidStoreInput),
		"expected ErrInvalidStoreInput, got %v", err)
}

func TestPostgresStoreUnit_NewPostgresStoreWithPoolConfig_EmptyConnString(t *testing.T) {
	_, err := NewPostgresStoreWithPoolConfig(context.Background(), "", DefaultPoolConfig())
	require.True(t, errors.Is(err, ErrInvalidStoreInput),
		"expected ErrInvalidStoreInput, got %v", err)
}

// ---------------------------------------------------------------------------
// Close
// ---------------------------------------------------------------------------

func TestPostgresStoreUnit_Close_NilPoolSafe(t *testing.T) {
	s := &PostgresStore{pool: nil}
	s.Close() // must not panic
}

// ---------------------------------------------------------------------------
// Register — input validation
// ---------------------------------------------------------------------------

func TestPostgresStoreUnit_Register_NilAgent(t *testing.T) {
	s, mock := newMockStore(t)
	err := s.Register(nil)
	require.True(t, errors.Is(err, ErrInvalidStoreInput))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPostgresStoreUnit_Register_BlankID(t *testing.T) {
	s, mock := newMockStore(t)
	ag := &Agent{ID: "", PublicKey: make(HexKey, ed25519.PublicKeySize)}
	err := s.Register(ag)
	require.True(t, errors.Is(err, ErrInvalidStoreInput))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPostgresStoreUnit_Register_InvalidPublicKeySize(t *testing.T) {
	s, mock := newMockStore(t)
	ag := &Agent{ID: "test", PublicKey: make(HexKey, 1)} // wrong size
	err := s.Register(ag)
	require.True(t, errors.Is(err, ErrInvalidStoreInput))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPostgresStoreUnit_Register_InvalidStatus(t *testing.T) {
	s, mock := newMockStore(t)
	ag := &Agent{
		ID:        "test",
		PublicKey: make(HexKey, ed25519.PublicKeySize),
		Status:    "bogus",
	}
	err := s.Register(ag)
	require.True(t, errors.Is(err, ErrInvalidStoreInput))
	require.NoError(t, mock.ExpectationsWereMet())
}

// ---------------------------------------------------------------------------
// Register — SQL execution paths
// ---------------------------------------------------------------------------

func TestPostgresStoreUnit_Register_Success(t *testing.T) {
	s, mock := newMockStore(t)
	ag := testAgent(t)

	mock.ExpectExec(`INSERT INTO agents`).
		WithArgs(ag.ID, []byte(ag.PublicKey), pgxmock.AnyArg(), string(ag.Status), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	err := s.Register(ag)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPostgresStoreUnit_Register_AgentExists(t *testing.T) {
	s, mock := newMockStore(t)
	ag := testAgent(t)

	mock.ExpectExec(`INSERT INTO agents`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(&pgconn.PgError{Code: "23505"})

	err := s.Register(ag)
	require.True(t, errors.Is(err, ErrAgentExists))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPostgresStoreUnit_Register_SQLError(t *testing.T) {
	s, mock := newMockStore(t)
	ag := testAgent(t)

	mock.ExpectExec(`INSERT INTO agents`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(errors.New("connection refused"))

	err := s.Register(ag)
	require.Error(t, err)
	require.False(t, errors.Is(err, ErrAgentExists))
	require.False(t, errors.Is(err, ErrInvalidStoreInput))
	require.NoError(t, mock.ExpectationsWereMet())
}

// ---------------------------------------------------------------------------
// Get
// ---------------------------------------------------------------------------

func TestPostgresStoreUnit_Get_Success(t *testing.T) {
	s, mock := newMockStore(t)
	ag := testAgent(t)
	now := time.Now().UTC()

	rows := pgxmock.NewRows([]string{"id", "public_key", "capabilities", "status", "registered_at", "last_seen"}).
		AddRow(ag.ID, []byte(ag.PublicKey), []byte(`["relay"]`), string(ag.Status), now, now)
	mock.ExpectQuery(`SELECT id, public_key, capabilities, status, registered_at, last_seen`).
		WithArgs(ag.ID).
		WillReturnRows(rows)

	got, err := s.Get(ag.ID)
	require.NoError(t, err)
	require.Equal(t, ag.ID, got.ID)
	require.Equal(t, ag.Status, got.Status)
	require.Equal(t, []string{"relay"}, got.Capabilities)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPostgresStoreUnit_Get_NotFound(t *testing.T) {
	s, mock := newMockStore(t)

	mock.ExpectQuery(`SELECT id, public_key, capabilities, status, registered_at, last_seen`).
		WithArgs("missing").
		WillReturnError(pgx.ErrNoRows)

	_, err := s.Get("missing")
	require.True(t, errors.Is(err, ErrAgentNotFound))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPostgresStoreUnit_Get_SQLError(t *testing.T) {
	s, mock := newMockStore(t)

	mock.ExpectQuery(`SELECT id, public_key, capabilities, status, registered_at, last_seen`).
		WithArgs("error-agent").
		WillReturnError(errors.New("connection closed"))

	_, err := s.Get("error-agent")
	require.Error(t, err)
	require.False(t, errors.Is(err, ErrAgentNotFound))
	require.NoError(t, mock.ExpectationsWereMet())
}

// ---------------------------------------------------------------------------
// List
// ---------------------------------------------------------------------------

func TestPostgresStoreUnit_List_Empty(t *testing.T) {
	s, mock := newMockStore(t)

	rows := pgxmock.NewRows([]string{"id", "public_key", "capabilities", "status", "registered_at", "last_seen"})
	mock.ExpectQuery(`SELECT id, public_key, capabilities, status, registered_at, last_seen FROM agents`).
		WillReturnRows(rows)

	agents := s.List()
	require.NotNil(t, agents)
	require.Empty(t, agents)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPostgresStoreUnit_List_Populated(t *testing.T) {
	s, mock := newMockStore(t)
	now := time.Now().UTC()
	// Two agents
	pub := make([]byte, ed25519.PublicKeySize)
	rows := pgxmock.NewRows([]string{"id", "public_key", "capabilities", "status", "registered_at", "last_seen"}).
		AddRow("a1", pub, []byte(`["relay"]`), "online", now, now).
		AddRow("a2", pub, []byte(`[]`), "offline", now, now)
	mock.ExpectQuery(`SELECT id, public_key, capabilities, status, registered_at, last_seen FROM agents`).
		WillReturnRows(rows)

	agents := s.List()
	require.Len(t, agents, 2)
	require.Equal(t, "a1", agents[0].ID)
	require.Equal(t, "a2", agents[1].ID)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPostgresStoreUnit_List_QueryError(t *testing.T) {
	s, mock := newMockStore(t)

	mock.ExpectQuery(`SELECT id, public_key, capabilities, status, registered_at, last_seen FROM agents`).
		WillReturnError(errors.New("connection closed"))

	agents := s.List()
	require.NotNil(t, agents)
	require.Empty(t, agents)
	require.NoError(t, mock.ExpectationsWereMet())
}

// ---------------------------------------------------------------------------
// Unregister
// ---------------------------------------------------------------------------

func TestPostgresStoreUnit_Unregister_Success(t *testing.T) {
	s, mock := newMockStore(t)

	mock.ExpectExec(`DELETE FROM agents`).
		WithArgs("test-agent").
		WillReturnResult(pgxmock.NewResult("DELETE", 1))

	err := s.Unregister("test-agent")
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPostgresStoreUnit_Unregister_NotFound(t *testing.T) {
	s, mock := newMockStore(t)

	mock.ExpectExec(`DELETE FROM agents`).
		WithArgs("missing").
		WillReturnResult(pgxmock.NewResult("DELETE", 0))

	err := s.Unregister("missing")
	require.True(t, errors.Is(err, ErrAgentNotFound))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPostgresStoreUnit_Unregister_SQLError(t *testing.T) {
	s, mock := newMockStore(t)

	mock.ExpectExec(`DELETE FROM agents`).
		WithArgs("error").
		WillReturnError(errors.New("connection closed"))

	err := s.Unregister("error")
	require.Error(t, err)
	require.False(t, errors.Is(err, ErrAgentNotFound))
	require.NoError(t, mock.ExpectationsWereMet())
}

// ---------------------------------------------------------------------------
// Deliver — input validation
// ---------------------------------------------------------------------------

func TestPostgresStoreUnit_Deliver_BlankAgentID(t *testing.T) {
	s, mock := newMockStore(t)
	err := s.Deliver("", &InboxEntry{Payload: []byte(`{}`)})
	require.True(t, errors.Is(err, ErrInvalidStoreInput))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPostgresStoreUnit_Deliver_NilEntry(t *testing.T) {
	s, mock := newMockStore(t)
	err := s.Deliver("agent", nil)
	require.True(t, errors.Is(err, ErrInvalidStoreInput))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPostgresStoreUnit_Deliver_InvalidJSON(t *testing.T) {
	s, mock := newMockStore(t)
	err := s.Deliver("agent", &InboxEntry{Payload: []byte("not-json")})
	require.True(t, errors.Is(err, ErrInvalidStoreInput))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPostgresStoreUnit_Deliver_ExpiryBeforeCreated(t *testing.T) {
	s, mock := newMockStore(t)
	now := time.Now().UTC()
	err := s.Deliver("agent", &InboxEntry{
		Payload:   []byte(`{}`),
		CreatedAt: now,
		ExpiresAt: now.Add(-time.Hour), // expires before creation
	})
	require.True(t, errors.Is(err, ErrInvalidStoreInput))
	require.NoError(t, mock.ExpectationsWereMet())
}

// ---------------------------------------------------------------------------
// Deliver — SQL execution paths
// ---------------------------------------------------------------------------

func TestPostgresStoreUnit_Deliver_Success(t *testing.T) {
	s, mock := newMockStore(t)
	entry := &InboxEntry{Payload: []byte(`{"msg":"hello"}`)}

	mock.ExpectExec(`INSERT INTO inbox_entries`).
		WithArgs(pgxmock.AnyArg(), "agent", entry.Payload, pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	err := s.Deliver("agent", entry)
	require.NoError(t, err)
	require.NotEmpty(t, entry.ID, "ID should be auto-generated when blank")
	require.False(t, entry.CreatedAt.IsZero())
	require.False(t, entry.ExpiresAt.IsZero())
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPostgresStoreUnit_Deliver_AgentNotFound(t *testing.T) {
	s, mock := newMockStore(t)

	mock.ExpectExec(`INSERT INTO inbox_entries`).
		WithArgs(pgxmock.AnyArg(), "missing", pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(&pgconn.PgError{Code: "23503"}) // FK violation

	err := s.Deliver("missing", &InboxEntry{Payload: []byte(`{}`)})
	require.True(t, errors.Is(err, ErrAgentNotFound))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPostgresStoreUnit_Deliver_DuplicateID(t *testing.T) {
	s, mock := newMockStore(t)

	mock.ExpectExec(`INSERT INTO inbox_entries`).
		WithArgs(pgxmock.AnyArg(), "agent", pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(&pgconn.PgError{Code: "23505"}) // duplicate PK

	err := s.Deliver("agent", &InboxEntry{ID: "dup", Payload: []byte(`{}`)})
	require.True(t, errors.Is(err, ErrInvalidStoreInput))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPostgresStoreUnit_Deliver_SQLError(t *testing.T) {
	s, mock := newMockStore(t)

	mock.ExpectExec(`INSERT INTO inbox_entries`).
		WithArgs(pgxmock.AnyArg(), "agent", pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(errors.New("disk full"))

	err := s.Deliver("agent", &InboxEntry{Payload: []byte(`{}`)})
	require.Error(t, err)
	require.False(t, errors.Is(err, ErrAgentNotFound))
	require.False(t, errors.Is(err, ErrInvalidStoreInput))
	require.NoError(t, mock.ExpectationsWereMet())
}

// ---------------------------------------------------------------------------
// Retrieve — input validation
// ---------------------------------------------------------------------------

func TestPostgresStoreUnit_Retrieve_BlankAgentID(t *testing.T) {
	s, mock := newMockStore(t)
	_, _, err := s.Retrieve("", time.Second, 1)
	require.True(t, errors.Is(err, ErrInvalidStoreInput))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPostgresStoreUnit_Retrieve_ZeroLeaseDuration(t *testing.T) {
	s, mock := newMockStore(t)
	_, _, err := s.Retrieve("agent", 0, 1)
	require.True(t, errors.Is(err, ErrInvalidStoreInput))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPostgresStoreUnit_Retrieve_NegativeLeaseDuration(t *testing.T) {
	s, mock := newMockStore(t)
	_, _, err := s.Retrieve("agent", -time.Second, 1)
	require.True(t, errors.Is(err, ErrInvalidStoreInput))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPostgresStoreUnit_Retrieve_ZeroMaxMessages(t *testing.T) {
	s, mock := newMockStore(t)
	_, _, err := s.Retrieve("agent", time.Second, 0)
	require.True(t, errors.Is(err, ErrInvalidStoreInput))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPostgresStoreUnit_Retrieve_NegativeMaxMessages(t *testing.T) {
	s, mock := newMockStore(t)
	_, _, err := s.Retrieve("agent", time.Second, -1)
	require.True(t, errors.Is(err, ErrInvalidStoreInput))
	require.NoError(t, mock.ExpectationsWereMet())
}

// ---------------------------------------------------------------------------
// Retrieve — SQL execution paths
// ---------------------------------------------------------------------------

func TestPostgresStoreUnit_Retrieve_Success(t *testing.T) {
	s, mock := newMockStore(t)
	now := time.Now().UTC()

	// Begin expects BEGIN
	mock.ExpectBegin()
	// Agent check — FOR KEY SHARE
	mock.ExpectQuery(`SELECT 1 FROM agents`).
		WithArgs("agent").
		WillReturnRows(pgxmock.NewRows([]string{"?"}).AddRow(1))
	// Claiming select — locks a disjoint FIFO batch.
	rows := pgxmock.NewRows([]string{"id", "agent_id", "payload", "created_at", "expires_at"}).
		AddRow("msg-1", "agent", []byte(`{}`), now, now.Add(time.Hour))
	mock.ExpectQuery(`FOR UPDATE SKIP LOCKED`).
		WithArgs("agent", pgxmock.AnyArg(), 10).
		WillReturnRows(rows)
	// The batch is stamped with a freshly minted lease.
	mock.ExpectExec(`UPDATE inbox_entries`).
		WithArgs("agent", pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), []string{"msg-1"}).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	// Commit
	mock.ExpectCommit()

	msgs, leaseID, err := s.Retrieve("agent", 30*time.Second, 10)
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	require.NotEmpty(t, leaseID)
	require.Equal(t, leaseID, msgs[0].LeaseID, "returned entries must carry the minted lease")
	require.Equal(t, 30*time.Second, msgs[0].LeaseDuration)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPostgresStoreUnit_Retrieve_AgentNotFound(t *testing.T) {
	s, mock := newMockStore(t)

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT 1 FROM agents`).
		WithArgs("missing").
		WillReturnError(pgx.ErrNoRows)
	mock.ExpectRollback()

	_, _, err := s.Retrieve("missing", time.Second, 1)
	require.True(t, errors.Is(err, ErrAgentNotFound))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPostgresStoreUnit_Retrieve_BeginError(t *testing.T) {
	s, mock := newMockStore(t)

	mock.ExpectBegin().WillReturnError(errors.New("pool exhausted"))

	_, _, err := s.Retrieve("agent", time.Second, 1)
	require.Error(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

// ---------------------------------------------------------------------------
// Ack — input validation
// ---------------------------------------------------------------------------

func TestPostgresStoreUnit_Ack_BlankAgentID(t *testing.T) {
	s, mock := newMockStore(t)
	err := s.Ack("", "lease", []string{"id"})
	require.True(t, errors.Is(err, ErrInvalidStoreInput))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPostgresStoreUnit_Ack_BlankLeaseID(t *testing.T) {
	s, mock := newMockStore(t)
	err := s.Ack("agent", "", []string{"id"})
	require.True(t, errors.Is(err, ErrInvalidStoreInput))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPostgresStoreUnit_Ack_DuplicateIDs(t *testing.T) {
	s, mock := newMockStore(t)
	err := s.Ack("agent", "lease", []string{"x", "x"})
	require.True(t, errors.Is(err, ErrInvalidStoreInput))
	require.NoError(t, mock.ExpectationsWereMet())
}

// ---------------------------------------------------------------------------
// Ack — SQL execution paths
// ---------------------------------------------------------------------------

func TestPostgresStoreUnit_Ack_Success(t *testing.T) {
	s, mock := newMockStore(t)

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT 1 FROM agents`).
		WithArgs("agent").
		WillReturnRows(pgxmock.NewRows([]string{"?"}).AddRow(1))

	// Classification lookup: the message exists under the supplied lease.
	mock.ExpectQuery(`SELECT id, COALESCE`).
		WithArgs("agent", []string{"msg-1"}).
		WillReturnRows(pgxmock.NewRows([]string{"id", "lease_id"}).AddRow("msg-1", "lease-1"))

	returningRows := pgxmock.NewRows([]string{"id"}).
		AddRow("msg-1")
	mock.ExpectQuery(`DELETE FROM inbox_entries`).
		WithArgs("agent", "lease-1", []string{"msg-1"}).
		WillReturnRows(returningRows)
	mock.ExpectCommit()

	err := s.Ack("agent", "lease-1", []string{"msg-1"})
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPostgresStoreUnit_Ack_AgentNotFound(t *testing.T) {
	s, mock := newMockStore(t)

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT 1 FROM agents`).
		WithArgs("missing").
		WillReturnError(pgx.ErrNoRows)
	mock.ExpectRollback()

	err := s.Ack("missing", "lease", []string{"id"})
	require.True(t, errors.Is(err, ErrAgentNotFound))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPostgresStoreUnit_Ack_LeaseConflict(t *testing.T) {
	s, mock := newMockStore(t)

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT 1 FROM agents`).
		WithArgs("agent").
		WillReturnRows(pgxmock.NewRows([]string{"?"}).AddRow(1))

	// The message exists, but under a different lease: no DELETE runs.
	mock.ExpectQuery(`SELECT id, COALESCE`).
		WithArgs("agent", []string{"msg-1"}).
		WillReturnRows(pgxmock.NewRows([]string{"id", "lease_id"}).AddRow("msg-1", "other-lease"))
	mock.ExpectRollback()

	err := s.Ack("agent", "lease", []string{"msg-1"})
	require.True(t, errors.Is(err, ErrLeaseConflict))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPostgresStoreUnit_Ack_EmptyIDsRejected(t *testing.T) {
	s, mock := newMockStore(t)

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT 1 FROM agents`).
		WithArgs("agent").
		WillReturnRows(pgxmock.NewRows([]string{"?"}).AddRow(1))
	mock.ExpectRollback()

	err := s.Ack("agent", "lease", nil)
	require.True(t, errors.Is(err, ErrInvalidStoreInput))
	require.NoError(t, mock.ExpectationsWereMet())
}

// ---------------------------------------------------------------------------
// Stats — input validation
// ---------------------------------------------------------------------------

func TestPostgresStoreUnit_Stats_BlankAgentID(t *testing.T) {
	s, mock := newMockStore(t)
	_, _, _, err := s.Stats("")
	require.True(t, errors.Is(err, ErrInvalidStoreInput))
	require.NoError(t, mock.ExpectationsWereMet())
}

// ---------------------------------------------------------------------------
// Stats — SQL execution paths
// ---------------------------------------------------------------------------

func TestPostgresStoreUnit_Stats_Success(t *testing.T) {
	s, mock := newMockStore(t)

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT 1 FROM agents`).
		WithArgs("agent").
		WillReturnRows(pgxmock.NewRows([]string{"?"}).AddRow(1))

	now := time.Now().UTC()
	past := now.Add(-time.Hour)
	aggRow := pgxmock.NewRows([]string{"queue_depth", "leased_count", "oldest_created_at"}).
		AddRow(int64(5), int64(2), &past)
	mock.ExpectQuery(`SELECT COUNT`).
		WithArgs("agent", pgxmock.AnyArg()).
		WillReturnRows(aggRow)
	mock.ExpectCommit()

	depth, leased, age, err := s.Stats("agent")
	require.NoError(t, err)
	require.Equal(t, 5, depth)
	require.Equal(t, 2, leased)
	require.Greater(t, age, time.Duration(0))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPostgresStoreUnit_Stats_EmptyQueue(t *testing.T) {
	s, mock := newMockStore(t)

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT 1 FROM agents`).
		WithArgs("agent").
		WillReturnRows(pgxmock.NewRows([]string{"?"}).AddRow(1))

	// NULL oldest_created_at → nil pointer
	aggRow := pgxmock.NewRows([]string{"queue_depth", "leased_count", "oldest_created_at"}).
		AddRow(int64(0), int64(0), nil)
	mock.ExpectQuery(`SELECT COUNT`).
		WithArgs("agent", pgxmock.AnyArg()).
		WillReturnRows(aggRow)
	mock.ExpectCommit()

	depth, leased, age, err := s.Stats("agent")
	require.NoError(t, err)
	require.Equal(t, 0, depth)
	require.Equal(t, 0, leased)
	require.Equal(t, time.Duration(0), age)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPostgresStoreUnit_Stats_AgentNotFound(t *testing.T) {
	s, mock := newMockStore(t)

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT 1 FROM agents`).
		WithArgs("missing").
		WillReturnError(pgx.ErrNoRows)
	mock.ExpectRollback()

	_, _, _, err := s.Stats("missing")
	require.True(t, errors.Is(err, ErrAgentNotFound))
	require.NoError(t, mock.ExpectationsWereMet())
}

// ---------------------------------------------------------------------------
// PurgeExpired — SQL execution paths
// ---------------------------------------------------------------------------

func TestPostgresStoreUnit_PurgeExpired_RemovesTTLExpired(t *testing.T) {
	s, mock := newMockStore(t)

	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM inbox_entries`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("DELETE", 3))
	mock.ExpectExec(`UPDATE inbox_entries`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("UPDATE", 2))
	mock.ExpectCommit()

	removed := s.PurgeExpired()
	require.Equal(t, 3, removed)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPostgresStoreUnit_PurgeExpired_BeginError(t *testing.T) {
	s, mock := newMockStore(t)

	mock.ExpectBegin().WillReturnError(errors.New("pool closed"))

	removed := s.PurgeExpired()
	require.Equal(t, 0, removed)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPostgresStoreUnit_PurgeExpired_DeleteError(t *testing.T) {
	s, mock := newMockStore(t)

	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM inbox_entries`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnError(errors.New("deadlock detected"))
	mock.ExpectRollback()

	removed := s.PurgeExpired()
	require.Equal(t, 0, removed)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPostgresStoreUnit_PurgeExpired_UpdateError(t *testing.T) {
	s, mock := newMockStore(t)

	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM inbox_entries`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("DELETE", 1))
	mock.ExpectExec(`UPDATE inbox_entries`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnError(errors.New("deadlock detected"))
	mock.ExpectRollback()

	removed := s.PurgeExpired()
	require.Equal(t, 0, removed)
	require.NoError(t, mock.ExpectationsWereMet())
}
