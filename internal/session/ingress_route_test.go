package session

// ingress_route_test.go — the SQLite arm of the CRIER-INGRESS §3 receipt:
// an `ingress.route` row in the session store's own view, asserted by COLUMN
// value rather than by "the call returned nil", plus the refusals that keep a
// receipt from being a row that cannot say where the push landed.
//
// The PostgreSQL arm of the same assertion lives behind the repo's integration
// gate (ingress_route_integration_test.go), which is how this package's other
// live-database claims are made.

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fixIngressRoute is one complete route record: everything the row must carry.
func fixIngressRoute() IngressRoute {
	return IngressRoute{
		ID:         "r_4f1c",
		AgentID:    "alpha",
		Capability: "build",
		MessageID:  "e_9a2b",
		Author:     AuthorRef{Agent: "ci-bot", Principal: "human-1"},
		Tags:       []string{"build", "triage"},
		CreatedAt:  time.Date(2026, 11, 6, 12, 30, 0, 0, time.UTC),
	}
}

func TestSQLite_IngressRouteRowIsProjected(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := newTestSQLiteStore(t)
	rec := fixIngressRoute()

	require.NoError(t, store.RecordIngressRoute(ctx, rec))

	// Read the row back from storage, not from the value we passed in: the
	// spec's claim is about the VIEW, so every assertion below is a column.
	var (
		kind, routeID, agentID, capability, messageID string
		authorType, authorID, principalID, createdAt  string
		tags                                          *string
	)
	require.NoError(t, store.DB().QueryRowContext(ctx, `
		SELECT kind, route_id, agent_id, capability, message_id,
		       author_type, author_id, principal_id, tags, created_at
		FROM chat_ingress_routes WHERE route_id = ?`, rec.ID).
		Scan(&kind, &routeID, &agentID, &capability, &messageID,
			&authorType, &authorID, &principalID, &tags, &createdAt))

	require.Equal(t, "ingress.route", kind, "the row must name its kind exactly as §3 spells it")
	require.Equal(t, rec.ID, routeID)
	require.Equal(t, "alpha", agentID, "the row must name the RESOLVED recipient")
	require.Equal(t, "build", capability)
	require.Equal(t, "e_9a2b", messageID, "the row must carry the inbox entry id an ack later names")
	require.Equal(t, "agent", authorType)
	require.Equal(t, "ci-bot", authorID)
	require.Equal(t, "human-1", principalID)
	require.NotNil(t, tags, "tags must be stored, not dropped")
	require.JSONEq(t, `["build","triage"]`, *tags)
	require.Equal(t, encTime(rec.CreatedAt), createdAt)

	// mode=inbox never touches a session, so the receipt is NOT a transcript
	// row and must not appear as one.
	var transcript int
	require.NoError(t, store.DB().QueryRowContext(ctx, `SELECT count(*) FROM chat_transcript`).Scan(&transcript))
	require.Zero(t, transcript, "an ingress route must not be projected into chat_transcript")
}

func TestSQLite_IngressRouteUntaggedRowStoresNull(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := newTestSQLiteStore(t)
	rec := fixIngressRoute()
	rec.ID = "r_untagged"
	rec.Capability = ""
	rec.Tags = nil
	rec.Author = AuthorRef{Agent: "alpha"}

	require.NoError(t, store.RecordIngressRoute(ctx, rec))

	var (
		capability string
		tags       *string
		principal  string
	)
	require.NoError(t, store.DB().QueryRowContext(ctx, `
		SELECT capability, tags, principal_id FROM chat_ingress_routes WHERE route_id = ?`, rec.ID).
		Scan(&capability, &tags, &principal))
	require.Empty(t, capability, "a direct push has no capability, and the row says so with an empty value")
	require.Nil(t, tags, "an untagged push stores SQL NULL — never an invented empty array")
	require.Empty(t, principal)
}

func TestSQLite_IngressRouteRefusesIncompleteRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := newTestSQLiteStore(t)

	for name, mutate := range map[string]func(*IngressRoute){
		"no id":         func(r *IngressRoute) { r.ID = "" },
		"no agent":      func(r *IngressRoute) { r.AgentID = "" },
		"no message id": func(r *IngressRoute) { r.MessageID = "" },
		"no created_at": func(r *IngressRoute) { r.CreatedAt = time.Time{} },
	} {
		t.Run(name, func(t *testing.T) {
			rec := fixIngressRoute()
			mutate(&rec)
			err := store.RecordIngressRoute(ctx, rec)
			require.Error(t, err)
			require.True(t, errors.Is(err, ErrInvalidRecord), "want ErrInvalidRecord, got %v", err)
		})
	}

	var n int
	require.NoError(t, store.DB().QueryRowContext(ctx, `SELECT count(*) FROM chat_ingress_routes`).Scan(&n))
	require.Zero(t, n, "a refused row must leave nothing behind")
}

func TestSQLite_IngressRouteDuplicateIDIsReported(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := newTestSQLiteStore(t)
	rec := fixIngressRoute()
	require.NoError(t, store.RecordIngressRoute(ctx, rec))

	// Two receipts claiming one id is a bug, not a replay: the handler mints a
	// route id per push and never re-records an idempotent retry, so the second
	// write must be REPORTED rather than silently dropped.
	dup := rec
	dup.MessageID = "e_other"
	err := store.RecordIngressRoute(ctx, dup)
	require.Error(t, err)

	var (
		n       int
		storedM string
	)
	require.NoError(t, store.DB().QueryRowContext(ctx, `SELECT count(*) FROM chat_ingress_routes`).Scan(&n))
	require.Equal(t, 1, n)
	require.NoError(t, store.DB().QueryRowContext(ctx,
		`SELECT message_id FROM chat_ingress_routes WHERE route_id = ?`, rec.ID).Scan(&storedM))
	require.Equal(t, "e_9a2b", storedM, "the first receipt survives intact")
}

// TestSQLite_IngressRouteTableReachesAnExistingDatabase is the additive-schema
// arm: a database created before this table existed must get it on the next
// open, with no migration step (the same CREATE TABLE IF NOT EXISTS contract
// chat_seq, chat_tasks and the group tables already rely on).
func TestSQLite_IngressRouteTableReachesAnExistingDatabase(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sessions.sqlite")

	store, err := NewSQLiteStore(path)
	require.NoError(t, err)
	// Simulate the pre-existing deployment: every other table is there, this one
	// is not.
	_, err = store.DB().ExecContext(ctx, `DROP TABLE chat_ingress_routes`)
	require.NoError(t, err)
	require.NoError(t, store.Close())

	reopened, err := NewSQLiteStore(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = reopened.Close() })

	rec := fixIngressRoute()
	require.NoError(t, reopened.RecordIngressRoute(ctx, rec))
	var kind string
	require.NoError(t, reopened.DB().QueryRowContext(ctx,
		`SELECT kind FROM chat_ingress_routes WHERE route_id = ?`, rec.ID).Scan(&kind))
	require.Equal(t, "ingress.route", kind)
}
