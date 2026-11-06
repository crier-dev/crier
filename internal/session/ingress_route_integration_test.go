//go:build integration

package session

// ingress_route_integration_test.go — the PostgreSQL arm of the CRIER-INGRESS
// §3 receipt: the same row the SQLite battery asserts, written by the SAME
// store the server selects for CR_SESSION_BACKEND=postgres (the pgxpool-backed
// PostgresStore) and by the shared SQLStore's PostgreSQL adapter.
//
// It is behind the repo's own integration gate (internal/session's existing
// //go:build integration + testcontainers TestMain):
//
//	go test -tags=integration -count=1 -timeout 5m ./internal/session

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// resetIngressRouteTable drops the route table so each arm starts from nothing
// and cannot pass on a sibling test's leftovers. Applying the schema (on store
// open) is what recreates it — which is also the additive-schema proof on this
// engine: no migration step, CREATE TABLE IF NOT EXISTS reaches the database.
func resetIngressRouteTable(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testDSN)
	require.NoError(t, err)
	defer pool.Close()
	_, err = pool.Exec(ctx, `DROP TABLE IF EXISTS chat_ingress_routes`)
	require.NoError(t, err)
}

// TestPostgres_IngressRouteRowIsProjected asserts the ROW, not that the call
// returned nil: kind, the resolved agent, the capability, the entry id, the
// author split and the tags, read back from the database.
func TestPostgres_IngressRouteRowIsProjected(t *testing.T) {
	ctx := context.Background()
	resetIngressRouteTable(t)

	store, err := NewPostgresStore(ctx, testDSN)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	rec := fixIngressRoute()
	require.NoError(t, store.RecordIngressRoute(ctx, rec))

	var (
		kind, routeID, agentID, capability, messageID string
		authorType, authorID, principalID             string
		tags                                          *string
		createdAt                                     time.Time
	)
	require.NoError(t, store.pool.QueryRow(ctx, `
		SELECT kind, route_id, agent_id, capability, message_id,
		       author_type, author_id, principal_id, tags, created_at
		FROM chat_ingress_routes WHERE route_id = $1`, rec.ID).
		Scan(&kind, &routeID, &agentID, &capability, &messageID,
			&authorType, &authorID, &principalID, &tags, &createdAt))

	require.Equal(t, "ingress.route", kind)
	require.Equal(t, rec.ID, routeID)
	require.Equal(t, "alpha", agentID)
	require.Equal(t, "build", capability)
	require.Equal(t, "e_9a2b", messageID)
	require.Equal(t, "agent", authorType)
	require.Equal(t, "ci-bot", authorID)
	require.Equal(t, "human-1", principalID)
	require.NotNil(t, tags)
	require.JSONEq(t, `["build","triage"]`, *tags)
	require.True(t, rec.CreatedAt.Equal(createdAt), "created_at = %s, want %s", createdAt, rec.CreatedAt)

	// The receipt is not a transcript row: mode=inbox never touches a session.
	var transcript int
	require.NoError(t, store.pool.QueryRow(ctx,
		`SELECT count(*) FROM chat_transcript WHERE message_id = $1`, rec.MessageID).Scan(&transcript))
	require.Zero(t, transcript)
}

// TestPostgres_IngressRouteRowIsProjectedByTheSharedStore drives the shared
// SQLStore's PostgreSQL adapter — the second engine of the "one repository"
// claim — through the SAME assertion, so the row is proven identical on both
// PostgreSQL code paths in this package.
func TestPostgres_IngressRouteRowIsProjectedByTheSharedStore(t *testing.T) {
	ctx := context.Background()
	resetIngressRouteTable(t)

	store, err := NewPostgresSQLStore(ctx, testDSN)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	rec := fixIngressRoute()
	rec.ID = "r_shared"
	rec.MessageID = "e_shared"
	rec.Tags = nil
	require.NoError(t, store.RecordIngressRoute(ctx, rec))

	var (
		kind, agentID, messageID, createdAt string
		tags                                *string
	)
	require.NoError(t, store.DB().QueryRowContext(ctx, `
		SELECT kind, agent_id, message_id, tags, created_at
		FROM chat_ingress_routes WHERE route_id = $1`, rec.ID).
		Scan(&kind, &agentID, &messageID, &tags, &createdAt))
	require.Equal(t, "ingress.route", kind)
	require.Equal(t, "alpha", agentID)
	require.Equal(t, "e_shared", messageID)
	require.Nil(t, tags, "an untagged push stores NULL")
	require.Equal(t, encTime(rec.CreatedAt), createdAt)

	// A duplicate route id must be REPORTED on this engine too, never dropped.
	dup := rec
	dup.MessageID = "e_other"
	require.Error(t, store.RecordIngressRoute(ctx, dup))
	var n int
	require.NoError(t, store.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM chat_ingress_routes`).Scan(&n))
	require.Equal(t, 1, n)
}
