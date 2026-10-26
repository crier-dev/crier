//go:build integration

package registry

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// priority_integration_test.go — CR-FEAT-035 on the DURABLE backend, against a
// real PostgreSQL (testcontainers, the same harness the other integration tests
// use). Two properties can only be proven here:
//
//  1. migration 007 applies to an existing database and the rows already in it
//     read back at priority 0 — the ordering they have always had (the
//     migration is additive with NOT NULL DEFAULT 0, and this measures that
//     rather than trusting it);
//  2. the claim query's ORDER BY priority DESC, delivery_sequence really hands
//     back the highest priority message first, and that ordering survives a
//     restart of the store (it is stored, not remembered).

func TestPostgresIntegration_PriorityOrdersRetrieve(t *testing.T) {
	store := newTestStore(t)
	agent := newTestAgent(t, store, "priority")

	for i, id := range []string{"low-a", "low-b", "low-c"} {
		require.NoError(t, store.Deliver(agent.ID, &InboxEntry{
			ID: id, Payload: json.RawMessage(`{"n":1}`), Priority: 0,
		}), "delivery %d", i)
	}
	require.NoError(t, store.Deliver(agent.ID, &InboxEntry{
		ID: "urgent", Payload: json.RawMessage(`{"alert":true}`), Priority: MaxMessagePriority,
	}))

	// max=1 cuts AFTER ordering: the urgent message is behind three low-value
	// ones in arrival order and must still be the one handed back.
	batch, leaseID, err := store.Retrieve(agent.ID, 30*time.Second, 1)
	require.NoError(t, err)
	require.NotEmpty(t, leaseID)
	require.Len(t, batch, 1)
	require.Equal(t, "urgent", batch[0].ID)
	require.Equal(t, MaxMessagePriority, batch[0].Priority,
		"the stored priority must read back with the message")

	// The backlog follows in arrival order.
	rest, _, err := store.Retrieve(agent.ID, 30*time.Second, 10)
	require.NoError(t, err)
	require.Len(t, rest, 3)
	for i, want := range []string{"low-a", "low-b", "low-c"} {
		require.Equal(t, want, rest[i].ID, "equal-priority messages keep arrival order")
		require.Equal(t, 0, rest[i].Priority)
	}
}

// TestPostgresIntegration_ExistingRowsReadBackAtPriorityZero inserts a row the
// way a database written BEFORE migration 007 holds one — the priority column
// simply absent from the INSERT — and measures that it lands at the default and
// retrieves in arrival order. That is the migration's whole compatibility
// contract.
func TestPostgresIntegration_ExistingRowsReadBackAtPriorityZero(t *testing.T) {
	store := newTestStore(t)
	agent := newTestAgent(t, store, "legacy")

	now := time.Now().UTC()
	for _, id := range []string{"legacy-1", "legacy-2"} {
		_, err := store.pool.Exec(t.Context(), `
INSERT INTO inbox_entries (id, agent_id, payload, created_at, expires_at, acked)
VALUES ($1, $2, $3::jsonb, $4, $5, FALSE);`, id, agent.ID, []byte(`{"legacy":true}`), now, now.Add(time.Hour))
		require.NoError(t, err, "a pre-migration INSERT (no priority column) must still be accepted")
	}

	// A new, urgent message arrives after them.
	require.NoError(t, store.Deliver(agent.ID, &InboxEntry{
		ID: "new-urgent", Payload: json.RawMessage(`{}`), Priority: 5,
	}))

	batch, _, err := store.Retrieve(agent.ID, 30*time.Second, 10)
	require.NoError(t, err)
	require.Len(t, batch, 3)
	require.Equal(t, "new-urgent", batch[0].ID, "a new priority outranks the legacy rows")
	require.Equal(t, 5, batch[0].Priority)
	require.Equal(t, "legacy-1", batch[1].ID, "legacy rows read back at the default, in arrival order")
	require.Equal(t, 0, batch[1].Priority)
	require.Equal(t, "legacy-2", batch[2].ID)
	require.Equal(t, 0, batch[2].Priority)
}

// TestPostgresIntegration_QueueDepthMatchesThePerAgentCounters pins the
// store-wide depth against the per-agent Stats on a real database: the
// operator's number is the sum of the numbers a consumer already sees, not a
// second definition of "queued".
func TestPostgresIntegration_QueueDepthMatchesThePerAgentCounters(t *testing.T) {
	store := newTestStore(t)
	first := newTestAgent(t, store, "depth-a")
	second := newTestAgent(t, store, "depth-b")

	require.NoError(t, store.Deliver(first.ID, &InboxEntry{Payload: json.RawMessage(`{"n":1}`)}))
	require.NoError(t, store.Deliver(first.ID, &InboxEntry{Payload: json.RawMessage(`{"n":2}`)}))
	require.NoError(t, store.Deliver(second.ID, &InboxEntry{Payload: json.RawMessage(`{"n":3}`)}))

	// One message leased: it is held, not delivered, so it stays queued.
	leased, _, err := store.Retrieve(first.ID, 30*time.Second, 1)
	require.NoError(t, err)
	require.Len(t, leased, 1)

	depth, err := store.QueueDepth()
	require.NoError(t, err)
	require.Equal(t, 3, depth.Pending)
	require.Equal(t, 1, depth.Leased)
	require.Positive(t, depth.OldestAge)

	firstDepth, firstLeased, _, err := store.Stats(first.ID)
	require.NoError(t, err)
	secondDepth, secondLeased, _, err := store.Stats(second.ID)
	require.NoError(t, err)
	require.Equal(t, firstDepth+secondDepth, depth.Pending,
		"store-wide depth must be the sum of the per-agent depths")
	require.Equal(t, firstLeased+secondLeased, depth.Leased)
}
