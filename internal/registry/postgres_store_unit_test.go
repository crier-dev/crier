package registry

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"
	"time"

	"github.com/crier-dev/crier/internal/guard"
	"github.com/crier-dev/crier/internal/webhook"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
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

// agentRowColumns is the column set Get/List scan, in order — it mirrors
// agentConfigColumns (DF-CRIER-151 adds webhook + guard, INT-A2A-001 adds a2a).
// Keep the two in sync.
func agentRowColumns() []string {
	return []string{"id", "public_key", "capabilities", "status", "registered_at", "last_seen", "webhook", "guard", "a2a"}
}

// Exact marshalled shapes, verified by running the marshaller (struct field
// order + omitempty decide these; the assertions below pin the wire shape).
const (
	wantWebhookJSON = `{"url":"http://hook.local/x"}`
	wantGuardJSON   = `{"policies":[{"id":"default","checks":{},"thresholds":{}}]}`
)

func testWebhookConfig() *webhook.Config {
	return &webhook.Config{URL: "http://hook.local/x"}
}

func testGuardConfig() *guard.AgentGuardConfig {
	return &guard.AgentGuardConfig{Policies: []guard.Policy{{ID: "default"}}}
}

// poisonConfig is a config whose marshalling always fails — it stands in for
// any config that cannot be persisted, which must surface as
// ErrInvalidStoreInput instead of being silently dropped.
type poisonConfig struct{}

func (poisonConfig) MarshalJSON() ([]byte, error) {
	return nil, errors.New("cannot marshal this config")
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

// TestPostgresStoreUnit_Register_KeylessAgentAllowedAndWritesNull pins the
// DF-CRIER-192 parity: an EMPTY key is a keyless agent, accepted by the
// store and persisted as SQL NULL (a nil arg) — an empty bytea would fail
// migration 004's length CHECK. Non-empty keys keep the 32-byte rule.
func TestPostgresStoreUnit_Register_KeylessAgentAllowedAndWritesNull(t *testing.T) {
	s, mock := newMockStore(t)
	ag := &Agent{ID: "keyless", PublicKey: HexKey(nil), Capabilities: []string{"relay"}}

	mock.ExpectExec(`INSERT INTO agents`).
		WithArgs(ag.ID, ([]byte)(nil), []byte(`["relay"]`), string(StatusOnline),
			pgxmock.AnyArg(), pgxmock.AnyArg(), ([]byte)(nil), ([]byte)(nil), ([]byte)(nil)).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	require.NoError(t, s.Register(ag))
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
// marshalling helpers — configuration failures must be loud
// ---------------------------------------------------------------------------

// TestPostgresStoreUnit_MarshalOptionalConfig_NilIsNull pins the SQL NULL
// representation: a nil config yields a nil slice, which pgx encodes as NULL
// (never an empty byte slice, which Postgres would reject as invalid JSON).
func TestPostgresStoreUnit_MarshalOptionalConfig_NilIsNull(t *testing.T) {
	raw, err := marshalOptionalConfig[*webhook.Config](nil)
	require.NoError(t, err)
	require.Nil(t, raw)
}

func TestPostgresStoreUnit_MarshalOptionalConfig_MarshalsConfig(t *testing.T) {
	raw, err := marshalOptionalConfig(testWebhookConfig())
	require.NoError(t, err)
	require.Equal(t, wantWebhookJSON, string(raw))
}

// TestPostgresStoreUnit_MarshalOptionalConfig_FailureIsInvalidInput is the
// anti-silent-drop guard: a config that cannot be marshalled must fail loudly
// rather than persist nothing behind a success status (DF-CRIER-151).
func TestPostgresStoreUnit_MarshalOptionalConfig_FailureIsInvalidInput(t *testing.T) {
	raw, err := marshalOptionalConfig(&poisonConfig{})
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrInvalidStoreInput), "want ErrInvalidStoreInput, got %v", err)
	require.Nil(t, raw)
}

// TestPostgresStoreUnit_UnmarshalOptionalConfig_NullFormsAreNil proves both
// NULL representations read back as an absent config: SQL NULL (nil src) and a
// stored JSON null. Neither may be an error or an empty object.
func TestPostgresStoreUnit_UnmarshalOptionalConfig_NullFormsAreNil(t *testing.T) {
	for _, raw := range [][]byte{nil, {}, []byte(""), []byte("null"), []byte(" null ")} {
		var cfg *webhook.Config
		require.NoError(t, unmarshalOptionalConfig(raw, &cfg), "raw=%q", raw)
		require.Nil(t, cfg, "raw=%q must decode to a nil config", raw)
	}
}

func TestPostgresStoreUnit_UnmarshalOptionalConfig_DecodesConfig(t *testing.T) {
	var cfg *webhook.Config
	require.NoError(t, unmarshalOptionalConfig([]byte(wantWebhookJSON), &cfg))
	require.NotNil(t, cfg)
	require.Equal(t, "http://hook.local/x", cfg.URL)
}

// ---------------------------------------------------------------------------
// Register — SQL execution paths
// ---------------------------------------------------------------------------

func TestPostgresStoreUnit_Register_Success(t *testing.T) {
	s, mock := newMockStore(t)
	ag := testAgent(t)

	mock.ExpectExec(`INSERT INTO agents`).
		WithArgs(ag.ID, []byte(ag.PublicKey), pgxmock.AnyArg(), string(ag.Status), pgxmock.AnyArg(), pgxmock.AnyArg(),
			([]byte)(nil), ([]byte)(nil), ([]byte)(nil)).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	err := s.Register(ag)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestPostgresStoreUnit_Register_PersistsWebhookAndGuard is the unit-level
// half of DF-CRIER-151: the marshalled configs must reach the INSERT args
// (pre-fix they were not in the statement at all, so the column set is pinned
// by the ExpectExec SQL regex as well).
func TestPostgresStoreUnit_Register_PersistsWebhookAndGuard(t *testing.T) {
	s, mock := newMockStore(t)
	ag := testAgent(t)
	ag.Webhook = testWebhookConfig()
	ag.Guard = testGuardConfig()

	mock.ExpectExec(`INSERT INTO agents \( id, public_key, capabilities, status, registered_at, last_seen, webhook, guard, a2a \)`).
		WithArgs(ag.ID, []byte(ag.PublicKey), []byte(`["relay"]`), string(StatusOnline), pgxmock.AnyArg(), pgxmock.AnyArg(),
			[]byte(wantWebhookJSON), []byte(wantGuardJSON), ([]byte)(nil)).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	require.NoError(t, s.Register(ag))

	// The caller's object is untouched by the marshalling.
	require.NotNil(t, ag.Webhook)
	require.NotNil(t, ag.Guard)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPostgresStoreUnit_Register_AgentExists(t *testing.T) {
	s, mock := newMockStore(t)
	ag := testAgent(t)

	mock.ExpectExec(`INSERT INTO agents`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(&pgconn.PgError{Code: "23505"})

	err := s.Register(ag)
	require.True(t, errors.Is(err, ErrAgentExists))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPostgresStoreUnit_Register_SQLError(t *testing.T) {
	s, mock := newMockStore(t)
	ag := testAgent(t)

	mock.ExpectExec(`INSERT INTO agents`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
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

	rows := pgxmock.NewRows(agentRowColumns()).
		AddRow(ag.ID, []byte(ag.PublicKey), []byte(`["relay"]`), string(ag.Status), now, now,
			[]byte(wantWebhookJSON), []byte(wantGuardJSON), nil)
	mock.ExpectQuery(`SELECT id, public_key, capabilities, status, registered_at, last_seen, webhook, guard, a2a FROM agents`).
		WithArgs(ag.ID).
		WillReturnRows(rows)

	got, err := s.Get(ag.ID)
	require.NoError(t, err)
	require.Equal(t, ag.ID, got.ID)
	require.Equal(t, ag.Status, got.Status)
	require.Equal(t, []string{"relay"}, got.Capabilities)
	require.NotNil(t, got.Webhook, "webhook config must survive the read path")
	require.Equal(t, "http://hook.local/x", got.Webhook.URL)
	require.NotNil(t, got.Guard, "guard config must survive the read path")
	require.Equal(t, []guard.Policy{{ID: "default"}}, got.Guard.Policies)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestPostgresStoreUnit_Get_NullConfigsAreNil covers the SQL NULL round trip:
// an agent stored without either config reads back with both pointers nil —
// not an error, not an empty object.
func TestPostgresStoreUnit_Get_NullConfigsAreNil(t *testing.T) {
	s, mock := newMockStore(t)
	now := time.Now().UTC()
	pub := make([]byte, ed25519.PublicKeySize)

	rows := pgxmock.NewRows(agentRowColumns()).
		AddRow("a1", pub, []byte(`[]`), "online", now, now, nil, nil, nil)
	mock.ExpectQuery(`SELECT id, public_key, capabilities, status, registered_at, last_seen, webhook, guard, a2a FROM agents`).
		WithArgs("a1").
		WillReturnRows(rows)

	got, err := s.Get("a1")
	require.NoError(t, err)
	require.Nil(t, got.Webhook, "SQL NULL webhook must decode to a nil pointer")
	require.Nil(t, got.Guard, "SQL NULL guard must decode to a nil pointer")
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestPostgresStoreUnit_Get_StoredJSONNullIsNil: a column holding the JSON
// value `null` (rather than SQL NULL) is the same absent config on the wire.
func TestPostgresStoreUnit_Get_StoredJSONNullIsNil(t *testing.T) {
	s, mock := newMockStore(t)
	now := time.Now().UTC()
	pub := make([]byte, ed25519.PublicKeySize)

	rows := pgxmock.NewRows(agentRowColumns()).
		AddRow("a1", pub, []byte(`[]`), "online", now, now, []byte("null"), []byte("null"), nil)
	mock.ExpectQuery(`SELECT id, public_key, capabilities, status, registered_at, last_seen, webhook, guard, a2a FROM agents`).
		WithArgs("a1").
		WillReturnRows(rows)

	got, err := s.Get("a1")
	require.NoError(t, err)
	require.Nil(t, got.Webhook)
	require.Nil(t, got.Guard)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPostgresStoreUnit_Get_InvalidConfigJSON(t *testing.T) {
	now := time.Now().UTC()
	pub := make([]byte, ed25519.PublicKeySize)

	t.Run("webhook", func(t *testing.T) {
		s, mock := newMockStore(t)
		rows := pgxmock.NewRows(agentRowColumns()).
			AddRow("a1", pub, []byte(`[]`), "online", now, now, []byte(`{"url":`), nil, nil)
		mock.ExpectQuery(`SELECT id, public_key, capabilities, status, registered_at, last_seen, webhook, guard, a2a FROM agents`).
			WithArgs("a1").
			WillReturnRows(rows)

		_, err := s.Get("a1")
		require.Error(t, err, "an unreadable stored config must surface, not be ignored")
		require.False(t, errors.Is(err, ErrAgentNotFound))
		require.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("guard", func(t *testing.T) {
		s, mock := newMockStore(t)
		rows := pgxmock.NewRows(agentRowColumns()).
			AddRow("a1", pub, []byte(`[]`), "online", now, now, nil, []byte(`[1,2]`), nil)
		mock.ExpectQuery(`SELECT id, public_key, capabilities, status, registered_at, last_seen, webhook, guard, a2a FROM agents`).
			WithArgs("a1").
			WillReturnRows(rows)

		_, err := s.Get("a1")
		require.Error(t, err)
		require.NoError(t, mock.ExpectationsWereMet())
	})
}

// TestPostgresStoreUnit_Get_KeylessAgentRoundTrips: a stored SQL NULL
// public_key (a keyless agent, DF-CRIER-192) reads back with an EMPTY key —
// not an error, matching the in-memory backend's representation.
func TestPostgresStoreUnit_Get_KeylessAgentRoundTrips(t *testing.T) {
	s, mock := newMockStore(t)
	now := time.Now().UTC()

	rows := pgxmock.NewRows(agentRowColumns()).
		AddRow("keyless", nil, []byte(`[]`), "online", now, now, nil, nil, nil)
	mock.ExpectQuery(`SELECT id, public_key, capabilities, status, registered_at, last_seen, webhook, guard, a2a FROM agents`).
		WithArgs("keyless").
		WillReturnRows(rows)

	got, err := s.Get("keyless")
	require.NoError(t, err)
	require.Equal(t, 0, len(got.PublicKey), "SQL NULL public_key must decode to an empty key")
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestPostgresStoreUnit_List_KeylessAgentKept: a NULL public_key row stays
// in the listing (pre-fix any non-32-byte key dropped the WHOLE list).
func TestPostgresStoreUnit_List_KeylessAgentKept(t *testing.T) {
	s, mock := newMockStore(t)
	now := time.Now().UTC()
	pub := make([]byte, ed25519.PublicKeySize)

	rows := pgxmock.NewRows(agentRowColumns()).
		AddRow("keyless", nil, []byte(`[]`), "online", now, now, nil, nil, nil).
		AddRow("keyed", pub, []byte(`[]`), "online", now, now, nil, nil, nil)
	mock.ExpectQuery(`SELECT id, public_key, capabilities, status, registered_at, last_seen, webhook, guard, a2a FROM agents`).
		WillReturnRows(rows)

	agents := s.List()
	require.Len(t, agents, 2)
	require.Equal(t, 0, len(agents[0].PublicKey))
	require.Equal(t, ed25519.PublicKeySize, len(agents[1].PublicKey))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPostgresStoreUnit_Get_NotFound(t *testing.T) {
	s, mock := newMockStore(t)

	mock.ExpectQuery(`SELECT id, public_key, capabilities, status, registered_at, last_seen, webhook, guard, a2a FROM agents`).
		WithArgs("missing").
		WillReturnError(pgx.ErrNoRows)

	_, err := s.Get("missing")
	require.True(t, errors.Is(err, ErrAgentNotFound))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPostgresStoreUnit_Get_SQLError(t *testing.T) {
	s, mock := newMockStore(t)

	mock.ExpectQuery(`SELECT id, public_key, capabilities, status, registered_at, last_seen, webhook, guard, a2a FROM agents`).
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

	rows := pgxmock.NewRows(agentRowColumns())
	mock.ExpectQuery(`SELECT id, public_key, capabilities, status, registered_at, last_seen, webhook, guard, a2a FROM agents`).
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
	rows := pgxmock.NewRows(agentRowColumns()).
		AddRow("a1", pub, []byte(`["relay"]`), "online", now, now, []byte(wantWebhookJSON), nil, nil).
		AddRow("a2", pub, []byte(`[]`), "offline", now, now, nil, []byte(wantGuardJSON), nil)
	mock.ExpectQuery(`SELECT id, public_key, capabilities, status, registered_at, last_seen, webhook, guard, a2a FROM agents`).
		WillReturnRows(rows)

	agents := s.List()
	require.Len(t, agents, 2)
	require.Equal(t, "a1", agents[0].ID)
	require.Equal(t, "a2", agents[1].ID)
	// Configs are decoded per row, and a NULL stays nil.
	require.NotNil(t, agents[0].Webhook)
	require.Equal(t, "http://hook.local/x", agents[0].Webhook.URL)
	require.Nil(t, agents[0].Guard)
	require.Nil(t, agents[1].Webhook)
	require.NotNil(t, agents[1].Guard)
	require.Equal(t, []guard.Policy{{ID: "default"}}, agents[1].Guard.Policies)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPostgresStoreUnit_List_QueryError(t *testing.T) {
	s, mock := newMockStore(t)

	mock.ExpectQuery(`SELECT id, public_key, capabilities, status, registered_at, last_seen, webhook, guard, a2a FROM agents`).
		WillReturnError(errors.New("connection closed"))

	agents := s.List()
	require.NotNil(t, agents)
	require.Empty(t, agents)
	require.NoError(t, mock.ExpectationsWereMet())
}

// ---------------------------------------------------------------------------
// Update — the PATCH /agents/{id} path (DF-CRIER-151)
// ---------------------------------------------------------------------------

func TestPostgresStoreUnit_Update_NilAgent(t *testing.T) {
	s, mock := newMockStore(t)
	err := s.Update(nil)
	require.True(t, errors.Is(err, ErrInvalidStoreInput))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPostgresStoreUnit_Update_BlankID(t *testing.T) {
	s, mock := newMockStore(t)
	err := s.Update(&Agent{ID: ""})
	require.True(t, errors.Is(err, ErrInvalidStoreInput))
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestPostgresStoreUnit_Update_PersistsWebhookAndGuard pins the UPDATE column
// set and args — the pre-fix statement wrote capabilities only.
func TestPostgresStoreUnit_Update_PersistsWebhookAndGuard(t *testing.T) {
	s, mock := newMockStore(t)
	ag := testAgent(t)
	ag.Webhook = testWebhookConfig()
	ag.Guard = testGuardConfig()

	mock.ExpectExec(`UPDATE agents SET capabilities = \$2::jsonb, webhook = \$3::jsonb, guard = \$4::jsonb, a2a = \$5::jsonb, last_seen = \$6`).
		WithArgs(ag.ID, []byte(`["relay"]`), []byte(wantWebhookJSON), []byte(wantGuardJSON), ([]byte)(nil), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	require.NoError(t, s.Update(ag))
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestPostgresStoreUnit_Update_NilWebhookClearsColumn: spec §7 — webhook
// absent/null removes the webhook, which is an explicit SQL NULL write, not a
// skipped column.
func TestPostgresStoreUnit_Update_NilWebhookClearsColumn(t *testing.T) {
	s, mock := newMockStore(t)
	ag := testAgent(t)
	ag.Webhook = nil
	ag.Guard = nil

	mock.ExpectExec(`UPDATE agents SET capabilities = \$2::jsonb, webhook = \$3::jsonb, guard = \$4::jsonb, a2a = \$5::jsonb, last_seen = \$6`).
		WithArgs(ag.ID, []byte(`["relay"]`), ([]byte)(nil), ([]byte)(nil), ([]byte)(nil), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	require.NoError(t, s.Update(ag))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPostgresStoreUnit_Update_NotFound(t *testing.T) {
	s, mock := newMockStore(t)
	ag := testAgent(t)

	mock.ExpectExec(`UPDATE agents`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))

	err := s.Update(ag)
	require.True(t, errors.Is(err, ErrAgentNotFound))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPostgresStoreUnit_Update_NilCapabilitiesMarshalAsEmptyArray(t *testing.T) {
	// A nil capability list is stored as [] rather than null, matching Register
	// (the column is NOT NULL DEFAULT '[]' with a jsonb_typeof = 'array' CHECK).
	s, mock := newMockStore(t)
	ag := testAgent(t)
	ag.Capabilities = nil

	mock.ExpectExec(`UPDATE agents`).
		WithArgs(ag.ID, []byte(`[]`), ([]byte)(nil), ([]byte)(nil), ([]byte)(nil), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	require.NoError(t, s.Update(ag))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPostgresStoreUnit_Update_SQLError(t *testing.T) {
	s, mock := newMockStore(t)
	ag := testAgent(t)

	mock.ExpectExec(`UPDATE agents`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(errors.New("connection closed"))

	err := s.Update(ag)
	require.Error(t, err)
	require.False(t, errors.Is(err, ErrAgentNotFound))
	require.False(t, errors.Is(err, ErrInvalidStoreInput))
	require.NoError(t, mock.ExpectationsWereMet())
}

// capturedTimeArg is a pgxmock Argument matcher that records the value handed
// to the driver, so a unit test can prove the statement and the caller's
// object carry the SAME instant (the pgxmock equivalent of inspecting $5).
type capturedTimeArg struct {
	got   any
	calls int
}

func (c *capturedTimeArg) Match(v any) bool {
	c.got = v
	c.calls++
	return true
}

// asTime normalises the recorded driver value. pgx hands the time.Time
// straight through; a pgtype.Timestamptz conversion is accepted too so the
// assertion stays about the instant, not about the Go type pgx chose.
func (c *capturedTimeArg) asTime(t *testing.T) time.Time {
	t.Helper()
	if c.calls == 0 {
		t.Fatal("the last_seen argument was never handed to the driver")
	}
	switch v := c.got.(type) {
	case time.Time:
		return v
	case pgtype.Timestamptz:
		if !v.Valid {
			t.Fatalf("last_seen argument = %+v, want a valid timestamp", v)
		}
		return v.Time
	default:
		t.Fatalf("last_seen argument type = %T (%v), want time.Time", c.got, c.got)
		return time.Time{}
	}
}

// TestPostgresStoreUnit_Update_AdvancesCallerLastSeen pins DF-CRIER-156 on
// the postgres backend: the UPDATE already wrote time.Now() into last_seen,
// but the value was never assigned back to the caller's agent — and
// HandleUpdateAgent renders exactly that object, so the PATCH 200 body
// reported the PRE-write last_seen while the stored row had the new one. The
// timestamp must be generated once and visible in both places.
func TestPostgresStoreUnit_Update_AdvancesCallerLastSeen(t *testing.T) {
	s, mock := newMockStore(t)
	ag := testAgent(t)
	ag.RegisteredAt = time.Now().UTC().Add(-time.Hour)
	ag.LastSeen = time.Now().UTC().Add(-time.Hour)
	registeredAt := ag.RegisteredAt
	preUpdate := ag.LastSeen
	cap := &capturedTimeArg{}

	mock.ExpectExec(`UPDATE agents SET capabilities = \$2::jsonb, webhook = \$3::jsonb, guard = \$4::jsonb, a2a = \$5::jsonb, last_seen = \$6`).
		WithArgs(ag.ID, []byte(`["relay"]`), ([]byte)(nil), ([]byte)(nil), ([]byte)(nil), cap).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	require.NoError(t, s.Update(ag))
	require.NoError(t, mock.ExpectationsWereMet())

	require.True(t, ag.LastSeen.After(preUpdate),
		"Update must advance the caller's last_seen (pre-update %v, got %v)", preUpdate, ag.LastSeen)
	require.True(t, cap.asTime(t).Equal(ag.LastSeen),
		"driver last_seen %v != caller last_seen %v — the response would diverge from the row",
		cap.asTime(t), ag.LastSeen)
	require.True(t, ag.RegisteredAt.Equal(registeredAt),
		"Update must preserve registered_at (%v), got %v", registeredAt, ag.RegisteredAt)
	require.Equal(t, StatusOnline, ag.Status)
}

// ---------------------------------------------------------------------------
// Touch — the mesh heartbeat path (CR-FEAT-024)
// ---------------------------------------------------------------------------

// TestPostgresStoreUnit_Touch_AdvancesLastSeenOnly pins the statement: the
// heartbeat path writes ONE column through GREATEST, so a heartbeat can never
// reshape a row it is only supposed to date, and can never move last_seen
// backwards (an out-of-order or replayed frame). RED on a bare
// `SET last_seen = $2`: the monotonic guard is gone.
func TestPostgresStoreUnit_Touch_AdvancesLastSeenOnly(t *testing.T) {
	s, mock := newMockStore(t)
	heartbeat := time.Now().UTC().Truncate(time.Microsecond)
	cap := &capturedTimeArg{}

	mock.ExpectExec(`UPDATE agents SET last_seen = GREATEST\(last_seen, \$2\) WHERE id = \$1`).
		WithArgs("test-agent", cap).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))

	require.NoError(t, s.Touch("test-agent", heartbeat))
	require.NoError(t, mock.ExpectationsWereMet())
	// The instant handed to the driver is the heartbeat's, microsecond-truncated
	// to match what the column will hold (the same rule Update applies).
	require.True(t, cap.asTime(t).Equal(heartbeat),
		"driver last_seen %v != heartbeat %v", cap.asTime(t), heartbeat)
}

// TestPostgresStoreUnit_Touch_NotFound: no such row is ErrAgentNotFound — the
// ordinary case for a peer that connected without a registry row, which the
// mesh sink logs at debug rather than as a failure.
func TestPostgresStoreUnit_Touch_NotFound(t *testing.T) {
	s, mock := newMockStore(t)

	mock.ExpectExec(`UPDATE agents`).
		WithArgs("missing", pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))

	err := s.Touch("missing", time.Now())
	require.True(t, errors.Is(err, ErrAgentNotFound))
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestPostgresStoreUnit_Touch_BlankID rejects a blank id without reaching the
// pool: a statement built around an empty id could only match nothing.
func TestPostgresStoreUnit_Touch_BlankID(t *testing.T) {
	s, mock := newMockStore(t)
	err := s.Touch("", time.Now())
	require.True(t, errors.Is(err, ErrInvalidStoreInput))
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestPostgresStoreUnit_Touch_SQLError: a store failure is returned (the sink
// warns on it) and is NOT laundered into agent-not-found — a registry that
// cannot record liveness must not look like a registry that has no such agent.
func TestPostgresStoreUnit_Touch_SQLError(t *testing.T) {
	s, mock := newMockStore(t)

	mock.ExpectExec(`UPDATE agents`).
		WithArgs("test-agent", pgxmock.AnyArg()).
		WillReturnError(errors.New("connection closed"))

	err := s.Touch("test-agent", time.Now())
	require.Error(t, err)
	require.False(t, errors.Is(err, ErrAgentNotFound))
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
	entry := &InboxEntry{Payload: []byte(`{"msg":"hello"}`), Sender: "foreman", IdempotencyKey: "k-1"}

	// Eight arguments since CR-FEAT-035: the INSERT names the priority column
	// too, because a retrieval priority that only lived in memory would read
	// back in arrival order on the durable backend. sender + idempotency_key
	// ride with the message since CR-FEAT-025 (the receipt address and the
	// dead-letter provenance).
	mock.ExpectExec(`INSERT INTO inbox_entries`).
		WithArgs(pgxmock.AnyArg(), "agent", entry.Payload, "foreman", "k-1",
			0, pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	err := s.Deliver("agent", entry)
	require.NoError(t, err)
	require.NotEmpty(t, entry.ID, "ID should be auto-generated when blank")
	require.False(t, entry.CreatedAt.IsZero())
	require.False(t, entry.ExpiresAt.IsZero())
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPostgresStoreUnit_Deliver_AbsentProvenanceIsSQLNull(t *testing.T) {
	s, mock := newMockStore(t)

	// A delivery that names no sender and carries no key stores NULL for both
	// (CR-FEAT-025) — never an empty string, so "not recorded" has exactly one
	// representation in the database.
	mock.ExpectExec(`INSERT INTO inbox_entries`).
		WithArgs(pgxmock.AnyArg(), "agent", pgxmock.AnyArg(), nil, nil,
			0, pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	err := s.Deliver("agent", &InboxEntry{Payload: []byte(`{}`)})
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPostgresStoreUnit_Deliver_AgentNotFound(t *testing.T) {
	s, mock := newMockStore(t)

	mock.ExpectExec(`INSERT INTO inbox_entries`).
		WithArgs(pgxmock.AnyArg(), "missing", pgxmock.AnyArg(), pgxmock.AnyArg(),
								pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(&pgconn.PgError{Code: "23503"}) // FK violation

	err := s.Deliver("missing", &InboxEntry{Payload: []byte(`{}`)})
	require.True(t, errors.Is(err, ErrAgentNotFound))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPostgresStoreUnit_Deliver_DuplicateID(t *testing.T) {
	s, mock := newMockStore(t)

	mock.ExpectExec(`INSERT INTO inbox_entries`).
		WithArgs(pgxmock.AnyArg(), "agent", pgxmock.AnyArg(), pgxmock.AnyArg(),
								pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(&pgconn.PgError{Code: "23505"}) // duplicate PK

	err := s.Deliver("agent", &InboxEntry{ID: "dup", Payload: []byte(`{}`)})
	require.True(t, errors.Is(err, ErrInvalidStoreInput))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPostgresStoreUnit_Deliver_SQLError(t *testing.T) {
	s, mock := newMockStore(t)

	mock.ExpectExec(`INSERT INTO inbox_entries`).
		WithArgs(pgxmock.AnyArg(), "agent", pgxmock.AnyArg(), pgxmock.AnyArg(),
			pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
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
	// Claiming select — locks a disjoint priority batch. Since CR-FEAT-025 the
	// row also carries the delivery's sender and idempotency key (read back
	// through COALESCE so a NULL provenance arrives as ""), and since
	// CR-FEAT-035 its priority (the first ORDER BY key).
	rows := pgxmock.NewRows([]string{"id", "agent_id", "payload", "sender", "idempotency_key", "priority", "created_at", "expires_at"}).
		AddRow("msg-1", "agent", []byte(`{}`), "foreman", "", 0, now, now.Add(time.Hour))
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
	require.Equal(t, "foreman", msgs[0].Sender, "the stored sender travels with the retrieved message")
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
	now := time.Now().UTC()

	// The sweep reads the removed rows back from the DELETE itself
	// (CR-FEAT-025), so the report describes what was actually deleted.
	expired := now.Add(-time.Hour)
	mock.ExpectBegin()
	rows := pgxmock.NewRows([]string{"id", "agent_id", "payload", "sender", "idempotency_key", "created_at", "expires_at"}).
		AddRow("m1", "agent", []byte(`{"n":1}`), "foreman", "k-1", now.Add(-2*time.Hour), expired).
		AddRow("m2", "agent", []byte(`{"n":2}`), "", "", now.Add(-2*time.Hour), expired).
		AddRow("m3", "other", []byte(`{"n":3}`), "", "", now.Add(-2*time.Hour), expired)
	mock.ExpectQuery(`DELETE FROM inbox_entries`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(rows)
	mock.ExpectExec(`UPDATE inbox_entries`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("UPDATE", 2))
	// Retention housekeeping runs in the same transaction.
	mock.ExpectExec(`DELETE FROM dead_letters`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("DELETE", 0))
	mock.ExpectCommit()

	reported := make(map[string]string)
	removed := s.PurgeExpiredReport(func(agentID string, entry *InboxEntry) {
		reported[entry.ID] = entry.Sender
	})
	require.Equal(t, 3, removed)
	require.Equal(t, map[string]string{"m1": "foreman", "m2": "", "m3": ""}, reported)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPostgresStoreUnit_PurgeExpired_ReportsAfterCommit(t *testing.T) {
	s, mock := newMockStore(t)
	now := time.Now().UTC()

	mock.ExpectBegin()
	rows := pgxmock.NewRows([]string{"id", "agent_id", "payload", "sender", "idempotency_key", "created_at", "expires_at"}).
		AddRow("m1", "agent", []byte(`{}`), "foreman", "", now.Add(-time.Hour), now.Add(-time.Minute))
	mock.ExpectQuery(`DELETE FROM inbox_entries`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(rows)
	mock.ExpectExec(`UPDATE inbox_entries`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("UPDATE", 0))
	mock.ExpectExec(`DELETE FROM dead_letters`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("DELETE", 0))
	mock.ExpectCommit()

	// The report runs after the commit: with a commit failure (the previous
	// test class) nothing is reported, so a caller can never dead-letter a
	// message that is still in the inbox.
	var reported []string
	removed := s.PurgeExpiredReport(func(agentID string, entry *InboxEntry) {
		reported = append(reported, entry.ID)
	})
	require.Equal(t, 1, removed)
	require.Equal(t, []string{"m1"}, reported)
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
	mock.ExpectQuery(`DELETE FROM inbox_entries`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnError(errors.New("deadlock detected"))
	mock.ExpectRollback()

	var reported int
	removed := s.PurgeExpiredReport(func(agentID string, entry *InboxEntry) { reported++ })
	require.Equal(t, 0, removed)
	require.Zero(t, reported, "a failed delete reports nothing")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPostgresStoreUnit_PurgeExpired_UpdateError(t *testing.T) {
	s, mock := newMockStore(t)
	now := time.Now().UTC()

	mock.ExpectBegin()
	rows := pgxmock.NewRows([]string{"id", "agent_id", "payload", "sender", "idempotency_key", "created_at", "expires_at"}).
		AddRow("m1", "agent", []byte(`{}`), "", "", now.Add(-time.Hour), now.Add(-time.Minute))
	mock.ExpectQuery(`DELETE FROM inbox_entries`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnRows(rows)
	mock.ExpectExec(`UPDATE inbox_entries`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnError(errors.New("deadlock detected"))
	mock.ExpectRollback()

	var reported int
	removed := s.PurgeExpiredReport(func(agentID string, entry *InboxEntry) { reported++ })
	require.Equal(t, 0, removed)
	require.Zero(t, reported, "a rolled-back sweep reports nothing")
	require.NoError(t, mock.ExpectationsWereMet())
}
