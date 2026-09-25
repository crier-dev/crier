package registry

// INT-A2A-001, durable half — the a2a block on the PostgreSQL backend.
//
// The block is a per-agent opt-in that a later row will READ off the row, so a
// backend that accepts it and drops it would be the DF-CRIER-151 failure shape
// again: a caller told 201 believing the agent had opted in, with nothing
// stored. These tests pin the three statements that carry it (INSERT, UPDATE,
// the Get/List SELECT) and the NULL round trip.

import (
	"errors"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v5"
	"github.com/stretchr/testify/require"

	"github.com/crier-dev/crier/internal/a2a"
)

const wantA2AJSON = `{"enabled":true}`

func testA2AConfig() *a2a.Config {
	return &a2a.Config{Enabled: true}
}

// TestPostgresStoreUnit_Register_PersistsA2A: the marshalled block must reach
// the INSERT args — the column set is pinned by the ExpectExec SQL regex, so a
// statement that loses the column fails here too.
func TestPostgresStoreUnit_Register_PersistsA2A(t *testing.T) {
	s, mock := newMockStore(t)
	ag := testAgent(t)
	ag.A2A = testA2AConfig()

	mock.ExpectExec(`INSERT INTO agents \( id, public_key, capabilities, status, registered_at, last_seen, webhook, guard, a2a, namespace \)`).
		WithArgs(ag.ID, []byte(ag.PublicKey), []byte(`["relay"]`), string(StatusOnline), pgxmock.AnyArg(), pgxmock.AnyArg(),
			([]byte)(nil), ([]byte)(nil), []byte(wantA2AJSON), nil).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	require.NoError(t, s.Register(ag))
	require.NotNil(t, ag.A2A, "Register must not consume or clear the caller's block")
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestPostgresStoreUnit_Register_NilA2AWritesNull: an agent registered without
// the block writes SQL NULL (a nil arg) — never an empty object, which would
// read back as "opted out" rather than "absent".
func TestPostgresStoreUnit_Register_NilA2AWritesNull(t *testing.T) {
	s, mock := newMockStore(t)
	ag := testAgent(t)

	mock.ExpectExec(`INSERT INTO agents`).
		WithArgs(ag.ID, []byte(ag.PublicKey), []byte(`["relay"]`), string(StatusOnline), pgxmock.AnyArg(), pgxmock.AnyArg(),
			([]byte)(nil), ([]byte)(nil), ([]byte)(nil), nil).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))

	require.NoError(t, s.Register(ag))
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestPostgresStoreUnit_Update_WritesAndClearsA2A: the UPDATE carries the block
// and an explicit clear writes SQL NULL into that column instead of skipping it.
func TestPostgresStoreUnit_Update_WritesAndClearsA2A(t *testing.T) {
	s, mock := newMockStore(t)
	ag := testAgent(t)
	ag.A2A = testA2AConfig()

	mock.ExpectExec(`UPDATE agents SET capabilities = \$2::jsonb, webhook = \$3::jsonb, guard = \$4::jsonb, a2a = \$5::jsonb, last_seen = \$6`).
		WithArgs(ag.ID, []byte(`["relay"]`), ([]byte)(nil), ([]byte)(nil), []byte(wantA2AJSON), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	require.NoError(t, s.Update(ag))

	// Opting back out: nil must be written as NULL, not dropped from the SET.
	ag.A2A = nil
	mock.ExpectExec(`UPDATE agents SET capabilities = \$2::jsonb, webhook = \$3::jsonb, guard = \$4::jsonb, a2a = \$5::jsonb, last_seen = \$6`).
		WithArgs(ag.ID, []byte(`["relay"]`), ([]byte)(nil), ([]byte)(nil), ([]byte)(nil), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	require.NoError(t, s.Update(ag))

	require.NoError(t, mock.ExpectationsWereMet())
}

// TestPostgresStoreUnit_Get_DecodesA2A: the read path decodes a stored block
// (Get) and reads a NULL column back as an absent one.
func TestPostgresStoreUnit_Get_DecodesA2A(t *testing.T) {
	now := time.Now().UTC()
	pub := make([]byte, 32)

	t.Run("stored block", func(t *testing.T) {
		s, mock := newMockStore(t)
		rows := pgxmock.NewRows(agentRowColumns()).
			AddRow("a1", pub, []byte(`[]`), "online", now, now, nil, nil, []byte(wantA2AJSON), "")
		mock.ExpectQuery(`SELECT id, public_key, capabilities, status, registered_at, last_seen, webhook, guard, a2a, COALESCE\(namespace, ''\) FROM agents`).
			WithArgs("a1").
			WillReturnRows(rows)

		got, err := s.Get("a1")
		require.NoError(t, err)
		require.NotNil(t, got.A2A, "a stored a2a block must survive the read path")
		require.True(t, got.A2A.Enabled)
		require.NoError(t, mock.ExpectationsWereMet())
	})

	t.Run("null column", func(t *testing.T) {
		s, mock := newMockStore(t)
		rows := pgxmock.NewRows(agentRowColumns()).
			AddRow("a1", pub, []byte(`[]`), "online", now, now, nil, nil, nil, "")
		mock.ExpectQuery(`SELECT id, public_key, capabilities, status, registered_at, last_seen, webhook, guard, a2a, COALESCE\(namespace, ''\) FROM agents`).
			WithArgs("a1").
			WillReturnRows(rows)

		got, err := s.Get("a1")
		require.NoError(t, err)
		require.Nil(t, got.A2A, "SQL NULL a2a must decode to a nil block")
		require.NoError(t, mock.ExpectationsWereMet())
	})
}

// TestPostgresStoreUnit_Get_InvalidA2AJSON: an unreadable stored document is an
// error, not an agent silently served without its opt-in.
func TestPostgresStoreUnit_Get_InvalidA2AJSON(t *testing.T) {
	s, mock := newMockStore(t)
	now := time.Now().UTC()
	pub := make([]byte, 32)

	rows := pgxmock.NewRows(agentRowColumns()).
		AddRow("a1", pub, []byte(`[]`), "online", now, now, nil, nil, []byte(`{"enabled":`), "")
	mock.ExpectQuery(`SELECT id, public_key, capabilities, status, registered_at, last_seen, webhook, guard, a2a, COALESCE\(namespace, ''\) FROM agents`).
		WithArgs("a1").
		WillReturnRows(rows)

	_, err := s.Get("a1")
	require.Error(t, err, "an unreadable stored a2a block must surface, not be ignored")
	require.False(t, errors.Is(err, ErrAgentNotFound))
	require.NoError(t, mock.ExpectationsWereMet())
}
